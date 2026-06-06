package filesystem_fat32

// Cross-compatibility tests against canonical FAT32 tooling:
//
//   * read-side  — open a FAT32 image produced by mtools `mformat -F`
//                  (equivalent on-disk layout to dosfstools `mkfs.vfat -F 32`
//                  and macOS `newfs_msdos -F 32`) and verify our parser
//                  agrees about labels, sizes, and file contents.
//
//   * write-side — format + populate a fresh image with this package, then
//                  shell out to `fsck.vfat -n` (dosfstools, Linux) or
//                  `fsck_msdos -n` (macOS native) and require a clean exit.
//                  A second test routes the same image through mtools
//                  `mdir` and asserts our file names appear in the listing.
//
// Canonical tools are LICENSE-incompatible with this package (dosfstools and
// mtools are GPL; this package is BSD-3-Clause), so we shell out via
// `os/exec` only — no linking, no CGO. Every external-tool gate uses
// `t.Skip` with a message naming the missing package, so `go test ./...`
// stays green on minimal CI runners.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Test 1 — read a canonical-tool-produced image (testdata/mkfs/image.fat32.gz)
// ---------------------------------------------------------------------------

const (
	fixtureRel        = "testdata/mkfs/image.fat32.gz"
	fixtureLabel      = "TESTFAT32"
	fixtureTotalBytes = int64(33554432) // 32 MiB — FAT32 minimum cluster count
	fixtureSectors    = uint32(65536)

	fixtureHelloName   = "HELLO.TXT"
	fixtureHelloSize   = 28
	fixtureHelloSHA256 = "0fb70af07b4a82c32af7bf602d3e9bb533ba3b3b9c55aec030a3880e93d8f319"
	fixtureNotesName   = "NOTES.TXT"
	fixtureNotesSize   = 88
	fixtureNotesSHA256 = "9a616954bcb237e505134e9cfa512e90983b22c5b2dc651191b35042b6dffb1c"
)

// extractFixture decompresses testdata/mkfs/image.fat32.gz into a fresh
// temp file and returns its path. Skips the test if the fixture is missing
// (e.g. someone pruned testdata in a shallow checkout).
func extractFixture(t *testing.T) string {
	t.Helper()
	gzPath, err := filepath.Abs(fixtureRel)
	if err != nil {
		t.Fatalf("abs(%s): %v", fixtureRel, err)
	}
	gzFile, err := os.Open(gzPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skipf("canonical-tool fixture missing at %s — re-build with mtools/mformat per testdata/mkfs/EXPECTED.txt", gzPath)
		}
		t.Fatalf("open fixture: %v", err)
	}
	defer gzFile.Close()
	gz, err := gzip.NewReader(gzFile)
	if err != nil {
		t.Fatalf("gzip open: %v", err)
	}
	defer gz.Close()

	imgPath := filepath.Join(t.TempDir(), "image.fat32")
	out, err := os.OpenFile(imgPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create temp image: %v", err)
	}
	if _, err := io.Copy(out, gz); err != nil {
		out.Close()
		t.Fatalf("decompress fixture: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close temp image: %v", err)
	}
	return imgPath
}

func TestReadMkfsImage(t *testing.T) {
	imgPath := extractFixture(t)

	// Sanity-check the on-disk size matches the recorded property.
	st, err := os.Stat(imgPath)
	if err != nil {
		t.Fatalf("stat image: %v", err)
	}
	if st.Size() != fixtureTotalBytes {
		t.Fatalf("fixture size = %d, want %d", st.Size(), fixtureTotalBytes)
	}

	fs := openTestFS(t, imgPath)

	info := fs.Info()
	if got := uint32(info.TotalSectors); got != fixtureSectors {
		t.Errorf("Info.TotalSectors = %d, want %d", got, fixtureSectors)
	}
	if info.BytesPerSector != 512 {
		t.Errorf("Info.BytesPerSector = %d, want 512", info.BytesPerSector)
	}
	if info.TypeLabel != "FAT32" {
		t.Errorf("Info.TypeLabel = %q, want %q", info.TypeLabel, "FAT32")
	}
	if got := strings.TrimRight(info.VolumeLabel, " \x00"); got != fixtureLabel {
		t.Errorf("Info.VolumeLabel = %q, want %q", got, fixtureLabel)
	}
	if got := fs.Label(); strings.TrimRight(got, " \x00") != fixtureLabel {
		t.Errorf("Label() = %q, want %q", got, fixtureLabel)
	}

	// Root directory listing must contain both fixture files. mformat lays
	// down a volume-label entry as well; we tolerate that and only assert
	// our two regular files are present.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		have[strings.ToUpper(e.Name())] = true
	}
	for _, want := range []string{fixtureHelloName, fixtureNotesName} {
		if !have[want] {
			t.Errorf("ListDir(/): missing %q (got %v)", want, have)
		}
	}

	for _, f := range []struct {
		name   string
		size   int
		sha256 string
	}{
		{fixtureHelloName, fixtureHelloSize, fixtureHelloSHA256},
		{fixtureNotesName, fixtureNotesSize, fixtureNotesSHA256},
	} {
		data, err := fs.ReadFile("/" + f.name)
		if err != nil {
			t.Fatalf("ReadFile(/%s): %v", f.name, err)
		}
		if len(data) != f.size {
			t.Errorf("ReadFile(/%s) length = %d, want %d", f.name, len(data), f.size)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != f.sha256 {
			t.Errorf("ReadFile(/%s) sha256 = %s, want %s", f.name, got, f.sha256)
		}
	}

	// Bonus: re-stat one entry through the Filesystem API.
	stat, err := fs.Stat("/" + fixtureHelloName)
	if err != nil {
		t.Fatalf("Stat(/%s): %v", fixtureHelloName, err)
	}
	if stat.Size() != uint64(fixtureHelloSize) {
		t.Errorf("Stat(/%s).Size = %d, want %d", fixtureHelloName, stat.Size(), fixtureHelloSize)
	}
}

// ---------------------------------------------------------------------------
// Test 2 — write a fresh image, validate with fsck.vfat / fsck_msdos.
// ---------------------------------------------------------------------------

// lookupFsck returns the first available canonical FAT fsck tool.
// On Linux, dosfstools ships `fsck.vfat`; on macOS, `fsck_msdos` is in /sbin.
// Returns ("", false) when neither is found — the caller should t.Skip.
func lookupFsck() (string, bool) {
	for _, name := range []string{"fsck.vfat", "fsck_msdos"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, true
		}
		// macOS may not have /sbin on PATH for `go test`. Try the usual spot.
		if name == "fsck_msdos" {
			const sbin = "/sbin/fsck_msdos"
			if _, err := os.Stat(sbin); err == nil {
				return sbin, true
			}
		}
	}
	return "", false
}

// writeFreshImage formats a minimal FAT32 image at path and adds two files.
// Returns the file names (basename) written into the image root.
func writeFreshImage(t *testing.T, path string) []string {
	t.Helper()
	fs, err := Format(path, fat32TestSize, FormatConfig{Label: "GOFAT32"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.WriteFile("/A.TXT", []byte("hello from go-fat32\n"), 0o644); err != nil {
		t.Fatalf("WriteFile A.TXT: %v", err)
	}
	if err := fs.WriteFile("/B.TXT", []byte("canonical-tool cross-compat\n"), 0o644); err != nil {
		t.Fatalf("WriteFile B.TXT: %v", err)
	}
	return []string{"A.TXT", "B.TXT"}
}

func TestWriteThenFsckVfat(t *testing.T) {
	fsck, ok := lookupFsck()
	if !ok {
		t.Skip("canonical FAT fsck not found (need dosfstools `fsck.vfat` or macOS `fsck_msdos`)")
	}
	path := filepath.Join(t.TempDir(), "out.fat32")
	_ = writeFreshImage(t, path)

	// -n = answer "no" to every fix prompt; the tool must exit 0 with no
	// changes required for our image to be considered clean.
	cmd := exec.Command(fsck, "-n", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	combined := stdout.String() + stderr.String()
	t.Logf("%s -n %s exit=%v\n%s", filepath.Base(fsck), path, err, combined)
	if err != nil {
		t.Fatalf("%s -n exited non-zero (%v); output above", filepath.Base(fsck), err)
	}
	// Defensive: even if exit was 0, refuse output that announces real
	// damage. dosfstools and fsck_msdos both print "errors" only when the
	// volume is broken — warnings (FSInfo free-count unset) are fine.
	lc := strings.ToLower(combined)
	if strings.Contains(lc, "filesystem is dirty") ||
		strings.Contains(lc, "had errors") ||
		strings.Contains(lc, "no fixes were made, but errors were found") {
		t.Fatalf("%s reported damage:\n%s", filepath.Base(fsck), combined)
	}
}

// TestWriteThenFsckVfatMultiClusterRoot writes enough files into the root
// directory to force the on-disk root chain past one cluster, then runs the
// canonical fsck tool against the resulting image. This guards against the
// historical bug where writeDirBuf truncated the chain at cluster 1 — a
// fresh fsck run on a multi-cluster root is the strongest end-to-end check.
func TestWriteThenFsckVfatMultiClusterRoot(t *testing.T) {
	fsck, ok := lookupFsck()
	if !ok {
		t.Skip("canonical FAT fsck not found (need dosfstools `fsck.vfat` or macOS `fsck_msdos`)")
	}
	path := filepath.Join(t.TempDir(), "out.fat32")
	// 16 MiB has plenty of room for ~256 LFN root entries plus their data.
	fs, err := Format(path, 16*1024*1024, FormatConfig{Label: "MULTICLUS"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	// 256 long-named files: 1 LFN + 1 short entry each ⇒ 512 slots = 16 KiB =
	// 4 clusters of directory; well past the prior 1-cluster ceiling.
	for i := 0; i < 256; i++ {
		name := fmt.Sprintf("/multi-cluster-root-%04d.dat", i)
		if err := fs.WriteFile(name, []byte("payload"), 0o644); err != nil {
			fs.Close()
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cmd := exec.Command(fsck, "-n", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	combined := stdout.String() + stderr.String()
	t.Logf("%s -n %s exit=%v\n%s", filepath.Base(fsck), path, err, combined)
	if err != nil {
		t.Fatalf("%s -n exited non-zero (%v); output above", filepath.Base(fsck), err)
	}
	lc := strings.ToLower(combined)
	if strings.Contains(lc, "filesystem is dirty") ||
		strings.Contains(lc, "had errors") ||
		strings.Contains(lc, "no fixes were made, but errors were found") {
		t.Fatalf("%s reported damage on multi-cluster-root image:\n%s", filepath.Base(fsck), combined)
	}
}

// ---------------------------------------------------------------------------
// Test 3 — write a fresh image, list it with mtools `mdir`.
// ---------------------------------------------------------------------------

func TestWriteThenMtoolsMdir(t *testing.T) {
	mdir, err := exec.LookPath("mdir")
	if err != nil {
		t.Skip("mtools `mdir` not found in PATH (install mtools)")
	}

	tmp := t.TempDir()
	imgPath := filepath.Join(tmp, "out.fat32")
	names := writeFreshImage(t, imgPath)

	// mdir respects MTOOLSRC for drive aliasing, but it also accepts
	// `-i <image>` to address an unaliased image file directly. We use
	// `::` to mean "root of the image addressed by -i". Disable the
	// signature/size check that mtools performs by default — our 4 MiB
	// image is well below the typical FAT32 lower bound but is still
	// internally consistent.
	cmd := exec.Command(mdir, "-i", imgPath, "-/", "-b", "::")
	cmd.Env = append(os.Environ(), "MTOOLS_SKIP_CHECK=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("mdir failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	listing := stdout.String()
	t.Logf("mdir -i %s -/ -b ::\n%s", imgPath, listing)
	for _, want := range names {
		// `mdir -b` prints one absolute path per line ("::/A.TXT"). Match
		// the basename case-insensitively to stay tolerant of mtools
		// quirks across versions/platforms.
		if !strings.Contains(strings.ToUpper(listing), strings.ToUpper(want)) {
			t.Errorf("mdir listing missing %q\nfull output:\n%s", want, listing)
		}
	}
}
