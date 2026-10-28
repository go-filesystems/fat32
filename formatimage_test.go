package filesystem_fat32

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// memImage is an in-memory image: what a WebAssembly page has instead of a
// host file.
type memImage struct {
	b      []byte
	writes int
	failAt int // fail the failAt-th WriteAt (1-based); 0 never fails
}

func (m *memImage) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memImage) WriteAt(p []byte, off int64) (int, error) {
	m.writes++
	if m.writes == m.failAt {
		return 0, errFmtBoom
	}
	if off < 0 || off+int64(len(p)) > int64(len(m.b)) {
		return 0, errors.New("memImage: write out of range")
	}
	return copy(m.b[off:], p), nil
}

// TestFormatImage_RoundTrip formats a buffer pre-filled with garbage -- a
// reused buffer -- and checks the volume is empty, writable and readable.
func TestFormatImage_RoundTrip(t *testing.T) {
	img := &memImage{b: bytes.Repeat([]byte{0xA5}, fat32TestSize)}
	fs, err := FormatImage(img, fat32TestSize, FormatConfig{Label: "PLAYGROUND", VolumeID: 0xCAFE})
	if err != nil {
		t.Fatalf("FormatImage: %v", err)
	}
	defer fs.Close()
	ents, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("fresh volume over garbage lists %d entries, want 0", len(ents))
	}
	if err := fs.MkDir("/docs", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	want := []byte("hello from memory\n")
	if err := fs.WriteFile("/docs/hello.txt", want, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := fs.ReadFile("/docs/hello.txt")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("ReadFile = %q, %v; want %q", got, err, want)
	}

	// The same bytes, reopened from scratch, are the same volume.
	again, err := OpenReader(bytes.NewReader(img.b), int64(len(img.b)))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer again.Close()
	if got, err := again.ReadFile("/docs/hello.txt"); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("reopened ReadFile = %q, %v", got, err)
	}
	if l, ok := again.(interface{ Label() string }); ok && strings.TrimSpace(l.Label()) != "PLAYGROUND" {
		t.Fatalf("label = %q", l.Label())
	}
}

// TestFormatImage_SameBytesAsFormat: over a zeroed buffer FormatImage writes
// exactly what Format writes to a file, so the two entry points cannot drift.
func TestFormatImage_SameBytesAsFormat(t *testing.T) {
	cfg := FormatConfig{Label: "SAME", VolumeID: 0x1234}
	path := filepath.Join(t.TempDir(), "f.img")
	fs, err := Format(path, fat32TestSize, cfg)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	fromFile, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	img := &memImage{b: make([]byte, fat32TestSize)}
	mfs, err := FormatImage(img, fat32TestSize, cfg)
	if err != nil {
		t.Fatalf("FormatImage: %v", err)
	}
	mfs.Close()
	if !bytes.Equal(fromFile, img.b) {
		t.Fatal("FormatImage and Format wrote different bytes")
	}
}

// TestFormatImage_Mdir has mtools read an image formatted in memory and
// populated through the driver: another implementation's opinion.
func TestFormatImage_Mdir(t *testing.T) {
	mdir, err := exec.LookPath("mdir")
	if err != nil {
		if os.Getenv("FAT32_REQUIRE_MTOOLS") != "" {
			t.Fatalf("FAT32_REQUIRE_MTOOLS is set but mtools is not installed: %v", err)
		}
		t.Skip("mtools `mdir` not found in PATH (install mtools)")
	}
	img := &memImage{b: bytes.Repeat([]byte{0x5A}, fat32TestSize)}
	fs, err := FormatImage(img, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("FormatImage: %v", err)
	}
	if err := fs.WriteFile("/README.TXT", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fs.Close()
	path := filepath.Join(t.TempDir(), "mem.img")
	if err := os.WriteFile(path, img.b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(mdir, "-i", path, "-/", "-b", "::")
	cmd.Env = append(os.Environ(), "MTOOLS_SKIP_CHECK=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mdir: %v\n%s", err, out)
	}
	if !strings.Contains(strings.ToUpper(string(out)), "README.TXT") {
		t.Fatalf("mdir does not list README.TXT:\n%s", out)
	}
}

func TestFormatImage_Refusals(t *testing.T) {
	if _, err := FormatImage(nil, fat32TestSize, FormatConfig{}); err == nil {
		t.Fatal("nil image accepted")
	}
	img := &memImage{b: make([]byte, 8192)}
	for _, size := range []int64{4095, 4096, 8192} {
		if _, err := FormatImage(img, size, FormatConfig{}); err == nil {
			t.Fatalf("size %d accepted", size)
		}
	}
	if img.writes != 0 {
		t.Fatalf("a refused size wrote %d times", img.writes)
	}
	// Larger than the 32-bit sector count can describe.
	if _, err := formatLayout(int64(0x100000000) * fmtBytesPerSector); err == nil {
		t.Fatal("a size beyond 2^32 sectors was accepted")
	}
}

// TestFormatImage_WriteFailures fails each write in turn: the zeroing, the
// boot sector, its backup, FSInfo, both FATs and the label entry.
func TestFormatImage_WriteFailures(t *testing.T) {
	// Count the writes of a clean run first, so the test follows the code.
	probe := &memImage{b: make([]byte, fat32TestSize)}
	if _, err := FormatImage(probe, fat32TestSize, FormatConfig{Label: "L"}); err != nil {
		t.Fatal(err)
	}
	if probe.writes < 7 {
		t.Fatalf("only %d writes in a clean run", probe.writes)
	}
	for i := 1; i <= probe.writes; i++ {
		img := &memImage{b: make([]byte, fat32TestSize), failAt: i}
		if _, err := FormatImage(img, fat32TestSize, FormatConfig{Label: "L"}); !errors.Is(err, errFmtBoom) {
			t.Fatalf("write %d failing: got %v, want errFmtBoom", i, err)
		}
	}
}

func TestZeroRange_Chunks(t *testing.T) {
	img := &memImage{b: bytes.Repeat([]byte{1}, 3*fmtZeroChunk+5)}
	if err := zeroRange(img, 3, int64(len(img.b))-3); err != nil {
		t.Fatal(err)
	}
	if img.writes != 4 {
		t.Fatalf("writes = %d, want 4 chunks", img.writes)
	}
	if !bytes.Equal(img.b[:3], []byte{1, 1, 1}) || bytes.ContainsRune(img.b[3:], 1) {
		t.Fatal("zeroRange touched the wrong bytes")
	}
}
