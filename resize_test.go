package filesystem_fat32

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// Test sizes for Grow / Shrink. The package's Format builds 4 KiB clusters
// regardless of volume size, so a "small" 4 MiB image already has 7 KiB of
// FAT slack — plenty to grow into. We exercise both directions with concrete
// power-of-two multiples of the cluster size.
const (
	resizeMinSize     = int64(4 * 1024 * 1024)   // 4 MiB — same as fat32TestSize
	resizeBaseSize    = int64(16 * 1024 * 1024)  // 16 MiB starting volume
	resizeBigSize     = int64(32 * 1024 * 1024)  // 32 MiB grown volume
	resizeClusterByte = fmtBytesPerSector * fmtSectorsPerCluster
)

// openResizeFS formats a fresh test image at sizeBytes and returns the
// concrete *fat32FS for direct access to Grow / Shrink / Resize.
func openResizeFS(t *testing.T, sizeBytes int64) (string, *fat32FS) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resize.img")
	fsi, err := Format(path, sizeBytes, FormatConfig{Label: "RESIZE"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	return path, fsi.(*fat32FS)
}

// fileSize returns the on-disk byte length of path.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return fi.Size()
}

// readTotalSectors decodes BPB_TotSec32 from the boot sector at path.
func readTotalSectors(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var buf [4]byte
	if _, err := f.ReadAt(buf[:], 32); err != nil {
		t.Fatalf("read TotSec32: %v", err)
	}
	return binary.LittleEndian.Uint32(buf[:])
}

// readBackupTotalSectors reads BPB_TotSec32 from the backup boot sector,
// assuming the canonical layout (sector 6).
func readBackupTotalSectors(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var buf [4]byte
	if _, err := f.ReadAt(buf[:], int64(fmtBackupBootSector)*fmtBytesPerSector+32); err != nil {
		t.Fatalf("read backup TotSec32: %v", err)
	}
	return binary.LittleEndian.Uint32(buf[:])
}

// readFSInfoFreeCount decodes the free-cluster hint at FSInfo offset 488.
func readFSInfoFreeCount(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var buf [4]byte
	off := int64(fmtFSInfoSector)*fmtBytesPerSector + 488
	if _, err := f.ReadAt(buf[:], off); err != nil {
		t.Fatalf("read FSInfo free count: %v", err)
	}
	return binary.LittleEndian.Uint32(buf[:])
}

// ─── Grow happy path ──────────────────────────────────────────────────────

func TestResize_GrowExtendsImage(t *testing.T) {
	path, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()

	// Write a small file so we have something to verify after the grow.
	const payload = "before grow"
	if err := fs.WriteFile("/before.txt", []byte(payload), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := fs.Grow(resizeBigSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if got := fileSize(t, path); got != resizeBigSize {
		t.Errorf("file size = %d, want %d", got, resizeBigSize)
	}
	wantSectors := uint32(resizeBigSize / fmtBytesPerSector)
	if got := readTotalSectors(t, path); got != wantSectors {
		t.Errorf("primary TotSec32 = %d, want %d", got, wantSectors)
	}
	if got := readBackupTotalSectors(t, path); got != wantSectors {
		t.Errorf("backup TotSec32 = %d, want %d", got, wantSectors)
	}
	if fs.info.TotalSectors != wantSectors {
		t.Errorf("in-memory Info.TotalSectors = %d, want %d", fs.info.TotalSectors, wantSectors)
	}

	// Verify the prior file is still readable through the in-memory FS.
	got, err := fs.ReadFile("/before.txt")
	if err != nil {
		t.Fatalf("ReadFile after Grow: %v", err)
	}
	if string(got) != payload {
		t.Errorf("ReadFile after Grow = %q, want %q", got, payload)
	}

	// And that re-opening produces a consistent view (BPB matches on disk).
	fs.Close()
	fsi, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open after Grow: %v", err)
	}
	defer fsi.Close()
	if got := fsi.(*fat32FS).info.TotalSectors; got != wantSectors {
		t.Errorf("re-opened Info.TotalSectors = %d, want %d", got, wantSectors)
	}
}

// ─── Shrink happy path ────────────────────────────────────────────────────

func TestResize_ShrinkTrimsImage(t *testing.T) {
	path, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()

	// Write small file in the very first data clusters so Shrink doesn't
	// hit a high cluster number.
	if err := fs.WriteFile("/hello.txt", []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 4 MiB stays well above the per-test floor.
	if err := fs.Shrink(resizeMinSize); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if got := fileSize(t, path); got != resizeMinSize {
		t.Errorf("file size = %d, want %d", got, resizeMinSize)
	}
	wantSectors := uint32(resizeMinSize / fmtBytesPerSector)
	if got := readTotalSectors(t, path); got != wantSectors {
		t.Errorf("primary TotSec32 = %d, want %d", got, wantSectors)
	}
	if got := readBackupTotalSectors(t, path); got != wantSectors {
		t.Errorf("backup TotSec32 = %d, want %d", got, wantSectors)
	}

	got, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile after Shrink: %v", err)
	}
	if string(got) != "hi" {
		t.Errorf("ReadFile after Shrink = %q, want %q", got, "hi")
	}
}

// ─── Resize dispatcher ────────────────────────────────────────────────────

func TestResize_Dispatcher(t *testing.T) {
	t.Run("equal size is no-op", func(t *testing.T) {
		_, fs := openResizeFS(t, resizeBaseSize)
		defer fs.Close()
		if err := fs.Resize(resizeBaseSize); err != nil {
			t.Errorf("Resize(same): %v", err)
		}
	})
	t.Run("grow via Resize", func(t *testing.T) {
		path, fs := openResizeFS(t, resizeBaseSize)
		defer fs.Close()
		if err := fs.Resize(resizeBigSize); err != nil {
			t.Fatalf("Resize(bigger): %v", err)
		}
		if got := fileSize(t, path); got != resizeBigSize {
			t.Errorf("file size = %d, want %d", got, resizeBigSize)
		}
	})
	t.Run("shrink via Resize", func(t *testing.T) {
		path, fs := openResizeFS(t, resizeBaseSize)
		defer fs.Close()
		if err := fs.Resize(resizeMinSize); err != nil {
			t.Fatalf("Resize(smaller): %v", err)
		}
		if got := fileSize(t, path); got != resizeMinSize {
			t.Errorf("file size = %d, want %d", got, resizeMinSize)
		}
	})
}

// ─── Shrink refuses to drop allocated clusters ────────────────────────────

// TestResize_ShrinkRefusesUsedClusters fills the volume far enough that the
// highest-used cluster sits above the new size's cluster count and asserts
// Shrink returns an error rather than silently trashing data.
func TestResize_ShrinkRefusesUsedClusters(t *testing.T) {
	// Sized so Shrink to resizeMinSize is rejected: we need at least one
	// allocated cluster past (resizeMinSize / cluster) - reservedSectorsEquivalent.
	// 64 MiB is plenty; we'll fill the tail.
	path, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()

	// Allocate a single cluster at a high cluster number by manually
	// flipping a FAT entry past the shrink ceiling. This is way faster
	// than writing tens of MiB of payload.
	newClusterCount := computeClusterCount(fs.info, resizeMinSize)
	highCluster := newClusterCount + 4 // safely past the new ceiling
	if err := fs.setFATEntry(highCluster, 0x0FFFFFFF); err != nil {
		t.Fatalf("setFATEntry(high cluster): %v", err)
	}

	err := fs.Shrink(resizeMinSize)
	if err == nil {
		t.Fatalf("Shrink succeeded with allocated cluster %d past the new ceiling %d",
			highCluster, newClusterCount+2)
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Errorf("Shrink error %q does not mention 'in use'", err)
	}
	// Image size must not have shrunk.
	if got := fileSize(t, path); got != resizeBaseSize {
		t.Errorf("file size after refused Shrink = %d, want %d", got, resizeBaseSize)
	}
}

// computeClusterCount is a test-side mirror of the data-sector arithmetic
// in Grow / Shrink.
func computeClusterCount(info Info, sizeBytes int64) uint32 {
	totalSectors := sizeBytes / int64(info.BytesPerSector)
	dataSectors := totalSectors -
		int64(info.ReservedSectors) -
		int64(info.FATCount)*int64(info.FATSize)
	return uint32(dataSectors / int64(info.SectorsPerCluster))
}

// ─── Validation errors ────────────────────────────────────────────────────

func TestResize_GrowRejectsBadInputs(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()

	tests := []struct {
		name string
		size int64
		want string
	}{
		{"shrink direction", resizeBaseSize - resizeClusterByte, "<= current size"},
		{"equal direction", resizeBaseSize, "<= current size"},
		{"misaligned", resizeBigSize + 1, "not a multiple of cluster size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := fs.Grow(tc.size)
			if err == nil {
				t.Fatalf("Grow(%d) = nil, want error", tc.size)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Grow error %q missing %q", err, tc.want)
			}
		})
	}
}

func TestResize_ShrinkRejectsBadInputs(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()

	tests := []struct {
		name string
		size int64
		want string
	}{
		{"grow direction", resizeBaseSize + resizeClusterByte, ">= current size"},
		{"equal direction", resizeBaseSize, ">= current size"},
		{"misaligned", resizeMinSize + 1, "not a multiple of cluster size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := fs.Shrink(tc.size)
			if err == nil {
				t.Fatalf("Shrink(%d) = nil, want error", tc.size)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Shrink error %q missing %q", err, tc.want)
			}
		})
	}
}

func TestResize_GrowRejectsTooFewDataClusters(t *testing.T) {
	// Force Grow into a pathological corner where the requested grow size
	// barely covers the reserved + FAT region, leaving fewer than the
	// floor's worth of data clusters. Mutate Info so the FAT looks huge
	// relative to the volume — pickFATSize will then fail or return a
	// cluster count below resizeMinDataClusters.
	_, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()

	// Pretend the FAT chews up almost every data sector. Grow's data-sector
	// calculation then yields 0 clusters at the requested target.
	fs.info.FATSize = (fs.info.TotalSectors - uint32(fs.info.ReservedSectors)) / uint32(fs.info.FATCount)
	if err := fs.Grow(resizeBaseSize + resizeClusterByte); err == nil {
		t.Fatalf("Grow with hyper-large FAT = nil, want error")
	}
}

// ─── Resizer interface satisfaction ───────────────────────────────────────

func TestResize_SatisfiesResizerInterface(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()
	var r filesystem.Resizer = fs
	if err := r.Resize(resizeBaseSize); err != nil {
		t.Fatalf("Resize via interface: %v", err)
	}
}

// ─── Partitioned-image refusal ────────────────────────────────────────────

// TestResize_RefusesPartitionedImage builds a tiny MBR-partitioned image
// whose only partition is a freshly-formatted FAT32 volume, then asserts
// Grow / Shrink both refuse: the on-disk size we'd manipulate is the whole
// disk image, but we don't update partition tables.
func TestResize_RefusesPartitionedImage(t *testing.T) {
	tmp := t.TempDir()
	imgPath := filepath.Join(tmp, "part.img")

	// Format a standalone FAT32 image first.
	fsPath := filepath.Join(tmp, "fs.img")
	if _, err := Format(fsPath, resizeBaseSize, FormatConfig{}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	fsBytes, err := os.ReadFile(fsPath)
	if err != nil {
		t.Fatalf("read fs image: %v", err)
	}

	// Assemble: 2048 sectors of MBR + partition + fsBytes.
	const startLBA = 2048
	disk := make([]byte, int64(startLBA)*sectorSize+resizeBaseSize)
	writeMBRPartition(disk, 0, startLBA)
	copy(disk[startLBA*sectorSize:], fsBytes)
	if err := os.WriteFile(imgPath, disk, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fsi, err := Open(imgPath, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsi.Close()
	pfs := fsi.(*fat32FS)
	if pfs.partOffset == 0 {
		t.Fatalf("partOffset = 0, want non-zero (test scaffolding bug)")
	}

	if err := pfs.Grow(resizeBaseSize + resizeClusterByte); err == nil {
		t.Errorf("Grow on partitioned image = nil, want error")
	}
	if err := pfs.Shrink(resizeMinSize); err == nil {
		t.Errorf("Shrink on partitioned image = nil, want error")
	}
}

// ─── refreshFSInfo updates the free-count hint ────────────────────────────

func TestResize_FSInfoFreeCountTracks(t *testing.T) {
	path, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()

	// Force the FSInfo counter to something obviously stale before we Grow.
	stale := make([]byte, fmtBytesPerSector)
	if _, err := fs.f.ReadAt(stale, int64(fmtFSInfoSector)*fmtBytesPerSector); err != nil {
		t.Fatalf("ReadAt FSInfo: %v", err)
	}
	binary.LittleEndian.PutUint32(stale[488:492], 0)
	if _, err := fs.f.WriteAt(stale, int64(fmtFSInfoSector)*fmtBytesPerSector); err != nil {
		t.Fatalf("WriteAt FSInfo: %v", err)
	}
	if got := readFSInfoFreeCount(t, path); got != 0 {
		t.Fatalf("pre-Grow free count = %d, want 0 (test scaffolding bug)", got)
	}

	if err := fs.Grow(resizeBigSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	got := readFSInfoFreeCount(t, path)
	if got == 0 || got == 0xFFFFFFFF {
		t.Errorf("post-Grow FSInfo free count = %d, want a real count", got)
	}
	// Sanity bound: free clusters must equal what we get by walking the
	// FAT entries for the data region (the official cluster range, ignoring
	// the FAT-region tail slack that the legacy countFreeClusters helper
	// over-counts).
	want := walkFreeDataClusters(t, fs)
	if got != want {
		t.Errorf("FSInfo free count = %d, want %d (walked FAT data range)", got, want)
	}
}

// walkFreeDataClusters counts free FAT entries within the legal data-region
// cluster range (cluster numbers 2..dataClusterCount+1). This is the
// reference value the FSInfo free-cluster hint should mirror.
func walkFreeDataClusters(t *testing.T, fs *fat32FS) uint32 {
	t.Helper()
	dataSectors := int64(fs.info.TotalSectors) -
		int64(fs.info.ReservedSectors) -
		int64(fs.info.FATCount)*int64(fs.info.FATSize)
	clusterCount := uint32(dataSectors / int64(fs.info.SectorsPerCluster))
	fatBase := fs.info.FATOffset(fs.partOffset)
	var buf [4]byte
	var free uint32
	for c := uint32(2); c < clusterCount+2; c++ {
		if _, err := fs.f.ReadAt(buf[:], fatBase+int64(c)*4); err != nil {
			t.Fatalf("ReadAt FAT entry %d: %v", c, err)
		}
		if binary.LittleEndian.Uint32(buf[:])&0x0FFFFFFF == 0 {
			free++
		}
	}
	return free
}

// ─── Truncate failure propagation ─────────────────────────────────────────

// truncFailDisk wraps a real *os.File so we keep working ReadAt / WriteAt,
// but every Truncate call fails. Used to verify Grow / Shrink report the
// underlying error rather than mask it.
type truncFailDisk struct{ inner *os.File }

func (d *truncFailDisk) ReadAt(p []byte, off int64) (int, error) {
	return d.inner.ReadAt(p, off)
}
func (d *truncFailDisk) WriteAt(p []byte, off int64) (int, error) {
	return d.inner.WriteAt(p, off)
}
func (d *truncFailDisk) Close() error          { return d.inner.Close() }
func (d *truncFailDisk) Truncate(int64) error  { return errors.New("truncate boom") }
func (d *truncFailDisk) Stat() (os.FileInfo, error) {
	return d.inner.Stat()
}

func TestResize_TruncateErrorPropagates(t *testing.T) {
	path, fsi := openResizeFS(t, resizeBaseSize)
	fsi.Close()

	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	info, err := readInfo(f, 0)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	fs := &fat32FS{f: &truncFailDisk{inner: f}, partOffset: 0, info: info}
	defer fs.Close()

	if err := fs.Grow(resizeBigSize); err == nil || !strings.Contains(err.Error(), "truncate") {
		t.Errorf("Grow Truncate error = %v, want truncate-wrap", err)
	}
	if err := fs.Shrink(resizeMinSize); err == nil || !strings.Contains(err.Error(), "truncate") {
		t.Errorf("Shrink Truncate error = %v, want truncate-wrap", err)
	}
}

// ─── Cross-compat: Grow → fsck.vfat / fsck_msdos ──────────────────────────

// TestResizeThenFsckVfat formats a fresh image, writes a couple of small
// files, grows it, then shrinks it back, and runs the canonical FAT fsck
// tool against the result. Skips when neither dosfstools `fsck.vfat` nor
// macOS `fsck_msdos` is on PATH.
func TestResizeThenFsckVfat(t *testing.T) {
	fsck, ok := lookupFsck()
	if !ok {
		t.Skip("canonical FAT fsck not found (need dosfstools `fsck.vfat` or macOS `fsck_msdos`)")
	}
	path := filepath.Join(t.TempDir(), "resize-fsck.fat32")

	fsi, err := Format(path, resizeBaseSize, FormatConfig{Label: "RESIZEFS"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsi.(*fat32FS)

	if err := fs.WriteFile("/A.TXT", []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("WriteFile A.TXT: %v", err)
	}
	if err := fs.WriteFile("/B.TXT", []byte("bravo\n"), 0o644); err != nil {
		t.Fatalf("WriteFile B.TXT: %v", err)
	}

	if err := fs.Grow(resizeBigSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if err := fs.Shrink(resizeMinSize); err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cmd := exec.Command(fsck, "-n", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	combined := stdout.String() + stderr.String()
	t.Logf("%s -n %s exit=%v\n%s", filepath.Base(fsck), path, runErr, combined)
	if runErr != nil {
		t.Fatalf("%s -n exited non-zero (%v); output above", filepath.Base(fsck), runErr)
	}
	lc := strings.ToLower(combined)
	if strings.Contains(lc, "filesystem is dirty") ||
		strings.Contains(lc, "had errors") ||
		strings.Contains(lc, "no fixes were made, but errors were found") {
		t.Fatalf("%s reported damage on resized image:\n%s", filepath.Base(fsck), combined)
	}

	// Verify the files survived through Format → write → Grow → Shrink.
	fsi2, err := Open(path, -1)
	if err != nil {
		t.Fatalf("re-Open after fsck: %v", err)
	}
	defer fsi2.Close()
	for _, want := range []struct {
		name, body string
	}{
		{"/A.TXT", "alpha\n"},
		{"/B.TXT", "bravo\n"},
	} {
		got, err := fsi2.ReadFile(want.name)
		if err != nil {
			t.Errorf("ReadFile %s: %v", want.name, err)
			continue
		}
		if string(got) != want.body {
			t.Errorf("ReadFile %s = %q, want %q", want.name, got, want.body)
		}
	}
}

// ─── Stress: repeated Grow/Shrink cycles ──────────────────────────────────

// TestResizeStressCycle runs N rounds of Grow → Shrink and verifies the
// volume remains intact (files readable, fsck-clean when fsck is available).
// Each cycle picks a different intermediate big size so we exercise a range
// of cluster-count deltas.
func TestResizeStressCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("resize stress cycle skipped in short mode")
	}
	path, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()

	const payload = "stress payload"
	if err := fs.WriteFile("/marker.txt", []byte(payload), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cycles := 8
	for i := 0; i < cycles; i++ {
		// Grow to a varying size strictly above the base.
		big := resizeBaseSize + int64(i+1)*int64(4*1024*1024)
		if err := fs.Grow(big); err != nil {
			t.Fatalf("cycle %d Grow(%d): %v", i, big, err)
		}
		// Shrink back to a random pick between min and base. We must stay
		// above the FAT32 floor and above the highest-used cluster — using
		// resizeMinSize keeps that safe because /marker.txt lives in the
		// first data cluster.
		if err := fs.Shrink(resizeMinSize); err != nil {
			t.Fatalf("cycle %d Shrink(%d): %v", i, resizeMinSize, err)
		}

		// /marker.txt must still be readable.
		got, err := fs.ReadFile("/marker.txt")
		if err != nil {
			t.Fatalf("cycle %d ReadFile after Shrink: %v", i, err)
		}
		if string(got) != payload {
			t.Fatalf("cycle %d ReadFile = %q, want %q", i, got, payload)
		}
	}

	// Optional fsck pass at the end if the tool is available.
	if fsck, ok := lookupFsck(); ok {
		// Drop our handle before fsck reads the image (Linux is OK with
		// shared handles; macOS is fussier).
		fs.Close()
		cmd := exec.Command(fsck, "-n", path)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s -n exited non-zero after stress cycle (%v)\n%s",
				filepath.Base(fsck), err, stdout.String()+stderr.String())
		}
		// Re-open so the test cleanup can Close() without panicking.
		reopened, err := Open(path, -1)
		if err != nil {
			t.Fatalf("re-Open after fsck: %v", err)
		}
		// Replace the closed handle so the deferred Close still works.
		_ = fs
		fs = reopened.(*fat32FS)
	}
	// Silence "declared but not used" if fsck branch is skipped.
	_ = fmt.Sprintf
}

// ─── Error-path coverage ──────────────────────────────────────────────────

// resizeFaultDisk wraps a real *os.File and fails ReadAt or WriteAt when a
// caller-supplied predicate trips. It implements truncater so Grow / Shrink
// will keep their non-Truncate paths in play.
type resizeFaultDisk struct {
	inner    *os.File
	readErr  func(off int64) error
	writeErr func(off int64) error
}

func (d *resizeFaultDisk) ReadAt(p []byte, off int64) (int, error) {
	if d.readErr != nil {
		if err := d.readErr(off); err != nil {
			return 0, err
		}
	}
	return d.inner.ReadAt(p, off)
}
func (d *resizeFaultDisk) WriteAt(p []byte, off int64) (int, error) {
	if d.writeErr != nil {
		if err := d.writeErr(off); err != nil {
			return 0, err
		}
	}
	return d.inner.WriteAt(p, off)
}
func (d *resizeFaultDisk) Close() error                  { return d.inner.Close() }
func (d *resizeFaultDisk) Truncate(n int64) error        { return d.inner.Truncate(n) }
func (d *resizeFaultDisk) Stat() (os.FileInfo, error)    { return d.inner.Stat() }

// openFaultFS formats a fresh image and reopens it through a resizeFaultDisk
// so the test can inject failures at a chosen offset.
func openFaultFS(t *testing.T, sizeBytes int64) (string, *resizeFaultDisk, *fat32FS) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fault.img")
	fsi, err := Format(path, sizeBytes, FormatConfig{Label: "FAULT"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fsi.Close()
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	info, err := readInfo(f, 0)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	disk := &resizeFaultDisk{inner: f}
	return path, disk, &fat32FS{f: disk, partOffset: 0, info: info}
}

// TestResize_GrowShiftReadErr forces the data-region shift to fail on its
// first read, then asserts Grow surfaces a wrapped "shift data region"
// error so callers can distinguish it from a boot-sector failure.
func TestResize_GrowShiftReadErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	// The shift reads from oldDataBase + i*clusterSize. Any read past
	// the boot region but inside the old data region is a shift read.
	dataBase := fs.info.DataOffset(fs.partOffset)
	disk.readErr = func(off int64) error {
		if off >= dataBase {
			return errors.New("shift read boom")
		}
		return nil
	}
	err := fs.Grow(resizeBigSize)
	if err == nil || !strings.Contains(err.Error(), "shift data region") {
		t.Fatalf("Grow shift-read error = %v, want shift-data-region wrap", err)
	}
}

// TestResize_GrowShiftWriteErr forces a write inside the new data region to
// fail and verifies Grow's shift path reports it cleanly.
func TestResize_GrowShiftWriteErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	bytesPerSector := int64(fs.info.BytesPerSector)
	// The shift writes start at newDataBase, which depends on newFATSize.
	// Picking a threshold past the old end of file but before any
	// FAT-copy write guarantees we trip during shiftDataRegion.
	oldEnd := int64(fs.info.TotalSectors) * bytesPerSector
	disk.writeErr = func(off int64) error {
		if off > oldEnd {
			return errors.New("shift write boom")
		}
		return nil
	}
	err := fs.Grow(resizeBigSize)
	if err == nil || !strings.Contains(err.Error(), "shift data region") {
		t.Fatalf("Grow shift-write error = %v, want shift-data-region wrap", err)
	}
}

// TestResize_RelocateFATCopiesDirect exercises relocateFATCopies in
// isolation by calling it with a hand-built fault disk: we fault one of
// the FAT1-mirror writes and confirm the wrap.
func TestResize_RelocateFATCopiesDirect(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	bytesPerSector := int64(fs.info.BytesPerSector)
	fatBase := fs.info.FATOffset(fs.partOffset)
	oldFATSize := int64(fs.info.FATSize)
	newFATSize := oldFATSize + 4
	newFATBytes := newFATSize * bytesPerSector
	// FAT1 mirror lives at fatBase + newFATBytes — fail any write at that
	// exact offset.
	target := fatBase + newFATBytes
	disk.writeErr = func(off int64) error {
		if off == target {
			return errors.New("fat1 mirror boom")
		}
		return nil
	}
	err := fs.relocateFATCopies(oldFATSize, newFATSize)
	if err == nil {
		t.Fatalf("relocateFATCopies = nil, want error")
	}
	if !strings.Contains(err.Error(), "write FAT") {
		t.Fatalf("relocateFATCopies error = %v, want write-FAT wrap", err)
	}
}

// TestResize_GrowWriteFATSizeErr fails the BPB rewrite that updates
// BPB_FATSz32 and confirms Grow reports a "write FAT size" wrap.
func TestResize_GrowWriteFATSizeErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	// Allow the data-shift writes and FAT-copy writes to succeed; only fail
	// the small per-sector write at offset 0 (primary BPB).
	disk.writeErr = func(off int64) error {
		if off == 0 {
			return errors.New("bpb write boom")
		}
		return nil
	}
	err := fs.Grow(resizeBigSize)
	if err == nil {
		t.Fatalf("Grow = nil, want error")
	}
	if !strings.Contains(err.Error(), "write FAT size") &&
		!strings.Contains(err.Error(), "write total sectors") &&
		!strings.Contains(err.Error(), "boom") {
		t.Fatalf("Grow BPB-write error = %v, want BPB wrap", err)
	}
}

// TestResize_GrowFSInfoReadErr fails the FSInfo refresh on its first read
// and confirms Grow surfaces the wrap.
func TestResize_GrowFSInfoReadErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	bytesPerSector := int64(fs.info.BytesPerSector)
	fsInfoOff := int64(fs.info.FSInfoSector) * bytesPerSector
	disk.readErr = func(off int64) error {
		if off == fsInfoOff {
			return errors.New("fsinfo read boom")
		}
		return nil
	}
	err := fs.Grow(resizeBigSize)
	if err == nil || !strings.Contains(err.Error(), "refresh FSInfo") {
		t.Fatalf("Grow FSInfo-read error = %v, want refresh-FSInfo wrap", err)
	}
}

// TestResize_ShrinkReadEntryErr makes the FAT entry walk fail mid-Shrink.
func TestResize_ShrinkReadEntryErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	fatBase := fs.info.FATOffset(fs.partOffset)
	dataBase := fs.info.DataOffset(fs.partOffset)
	// The shrink walk starts at offset `fatBase + (newClusterCount+2)*4`
	// and runs to the end of the FAT region. Fail any 4-byte read past
	// the FAT header but inside the FAT region.
	disk.readErr = func(off int64) error {
		if off > fatBase+12 && off < dataBase {
			return errors.New("fat entry boom")
		}
		return nil
	}
	err := fs.Shrink(resizeMinSize)
	if err == nil || !strings.Contains(err.Error(), "read FAT entry") {
		t.Fatalf("Shrink read-entry error = %v, want read-FAT-entry wrap", err)
	}
}

// TestResize_RefreshFSInfoUnsignedSector verifies refreshFSInfo bails out
// silently (returns nil) when the FSInfo sector signature doesn't match.
// This protects callers that point us at filesystems whose FSInfoSector
// field references a hand-crafted or corrupt sector — we'd rather skip
// the rewrite than corrupt data.
func TestResize_RefreshFSInfoUnsignedSector(t *testing.T) {
	_, _, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	// Wipe the FSInfo signatures so refreshFSInfo treats the sector as
	// non-canonical and bails.
	off := int64(fs.info.FSInfoSector) * int64(fs.info.BytesPerSector)
	zero := make([]byte, fs.info.BytesPerSector)
	if _, err := fs.f.WriteAt(zero, off); err != nil {
		t.Fatalf("WriteAt FSInfo zero: %v", err)
	}
	// Now refreshFSInfo must be a no-op: no error, no signature damage.
	if err := fs.refreshFSInfo(); err != nil {
		t.Errorf("refreshFSInfo with bad signature = %v, want nil", err)
	}
}

// TestResize_RefreshFSInfoZeroSector exercises the FSInfoSector==0 short-
// circuit.
func TestResize_RefreshFSInfoZeroSector(t *testing.T) {
	_, _, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	fs.info.FSInfoSector = 0
	if err := fs.refreshFSInfo(); err != nil {
		t.Errorf("refreshFSInfo with FSInfoSector=0 = %v, want nil", err)
	}
}

// TestResize_WriteTotalSectorsBadSignature covers the boot-signature guard
// in writeTotalSectors: if the sector isn't actually a BPB, we refuse to
// scribble on it.
func TestResize_WriteTotalSectorsBadSignature(t *testing.T) {
	_, _, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	// Corrupt the primary boot signature.
	bad := make([]byte, fs.info.BytesPerSector)
	if _, err := fs.f.ReadAt(bad, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	bad[510] = 0
	if _, err := fs.f.WriteAt(bad, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := fs.writeTotalSectors(fs.info.TotalSectors); err == nil ||
		!strings.Contains(err.Error(), "boot signature missing") {
		t.Errorf("writeTotalSectors with bad signature = %v, want boot-signature error", err)
	}
}

// TestResize_WriteFATSizeBadSignature mirrors the above for writeFATSize.
func TestResize_WriteFATSizeBadSignature(t *testing.T) {
	_, _, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	bad := make([]byte, fs.info.BytesPerSector)
	if _, err := fs.f.ReadAt(bad, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	bad[511] = 0
	if _, err := fs.f.WriteAt(bad, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := fs.writeFATSize(fs.info.FATSize); err == nil ||
		!strings.Contains(err.Error(), "boot signature missing") {
		t.Errorf("writeFATSize with bad signature = %v, want boot-signature error", err)
	}
}

// TestResize_CurrentSizeFallback exercises currentSize's fallback path:
// when the backing store can't be Stat'd, the function relies on the
// TotalSectors field instead. This branch is hit by the mockDisk shim that
// other tests use, but we hit it explicitly here for coverage.
func TestResize_CurrentSizeFallback(t *testing.T) {
	// newMockFS lives in fat32_test.go and produces a fat32FS backed by an
	// in-memory mockDisk. The mockDisk has no Stat method.
	boot := defaultFAT32BootSector()
	info, err := readInfo(bytes.NewReader(boot), 0)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	fs := newMockFS(boot, info, 0)
	got, err := fs.currentSize()
	if err != nil {
		t.Fatalf("currentSize: %v", err)
	}
	want := int64(info.TotalSectors) * int64(info.BytesPerSector)
	if got != want {
		t.Errorf("currentSize fallback = %d, want %d", got, want)
	}
}

// TestResize_GrowSectorOverflow exercises the "exceeds 32-bit sector count"
// guard. We can't easily build a 2 TiB file, so instead we mutate the
// requested size to be just above the uint32 sector ceiling and let the
// pre-Truncate validations catch it.
func TestResize_GrowSectorOverflow(t *testing.T) {
	_, _, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	// (uint32 max + 1) * BPS — overflows the uint32 sector field.
	bigSize := (int64(^uint32(0)) + 2) * int64(fs.info.BytesPerSector)
	// Round to cluster boundary so the misaligned guard doesn't preempt.
	cs := int64(fs.info.ClusterSize())
	if bigSize%cs != 0 {
		bigSize += cs - bigSize%cs
	}
	err := fs.Grow(bigSize)
	if err == nil || !strings.Contains(err.Error(), "exceeds 32-bit sector count") {
		t.Errorf("Grow overflow = %v, want exceeds-32-bit-sector-count", err)
	}
}

// TestResize_PickFATSizeRejectsImpossible exercises pickFATSize's "no data
// sectors" and "did not converge" guards.
func TestResize_PickFATSizeRejectsImpossible(t *testing.T) {
	// totalSectors == reservedSectors: no room for FAT or data → error.
	if _, _, err := pickFATSize(32, 32, 2, 8, 512, 1); err == nil {
		t.Errorf("pickFATSize(no data) = nil, want error")
	}
	// Reserve so much for FAT that no clusters remain.
	if _, _, err := pickFATSize(100, 32, 2, 8, 512, 100); err == nil {
		t.Errorf("pickFATSize(no clusters) = nil, want error")
	}
}

// TestResize_NoTruncaterFails uses the in-memory mockDisk (which lacks
// Truncate) to confirm Grow / Shrink decline cleanly when the backing
// store can't be resized in place.
func TestResize_NoTruncaterFails(t *testing.T) {
	// Build a 1 MiB image in memory with a default BPB so Open could parse
	// it, then wrap it in mockDisk (no Truncate).
	image := make([]byte, 1024*1024)
	copy(image, defaultFAT32BootSector())
	info, err := readInfo(bytes.NewReader(image), 0)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	fs := newMockFS(image, info, 0)
	// currentSize falls back to info.TotalSectors*BPS = 32 MiB. Pick a
	// grow target above that.
	bigSize := int64(info.TotalSectors)*int64(info.BytesPerSector) + int64(info.ClusterSize())
	if err := fs.Grow(bigSize); err == nil ||
		!strings.Contains(err.Error(), "does not support Truncate") {
		t.Errorf("Grow with no Truncate = %v, want truncater-missing error", err)
	}
	smallSize := int64(info.TotalSectors)*int64(info.BytesPerSector) - int64(info.ClusterSize())
	if err := fs.Shrink(smallSize); err == nil ||
		!strings.Contains(err.Error(), "does not support Truncate") {
		t.Errorf("Shrink with no Truncate = %v, want truncater-missing error", err)
	}
}

// TestResize_ResizeCurrentSizeError exercises Resize's currentSize error
// path. We do this by injecting an os.File whose Stat method always errors.
type statFailDisk struct{ *os.File }

func (d *statFailDisk) Stat() (os.FileInfo, error) {
	return nil, errors.New("stat boom")
}

func TestResize_ResizeCurrentSizeError(t *testing.T) {
	path, fsi := openResizeFS(t, resizeBaseSize)
	fsi.Close()
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	info, err := readInfo(f, 0)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	fs := &fat32FS{f: &statFailDisk{File: f}, partOffset: 0, info: info}
	defer fs.Close()
	if err := fs.Resize(resizeBigSize); err == nil ||
		!strings.Contains(err.Error(), "stat") {
		t.Errorf("Resize stat-fail = %v, want stat-error wrap", err)
	}
	if err := fs.Grow(resizeBigSize); err == nil ||
		!strings.Contains(err.Error(), "stat") {
		t.Errorf("Grow stat-fail = %v, want stat-error wrap", err)
	}
	if err := fs.Shrink(resizeMinSize); err == nil ||
		!strings.Contains(err.Error(), "stat") {
		t.Errorf("Shrink stat-fail = %v, want stat-error wrap", err)
	}
}

// TestResize_GrowFSInfoSignatureSkipped exercises Grow's refreshFSInfo
// no-op path: when the FSInfo signature doesn't match, refreshFSInfo
// returns nil, so Grow still succeeds end-to-end.
func TestResize_GrowFSInfoSignatureSkipped(t *testing.T) {
	_, _, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	// Stomp on the FSInfo signature bytes so refreshFSInfo bails silently.
	off := int64(fs.info.FSInfoSector) * int64(fs.info.BytesPerSector)
	buf := make([]byte, fs.info.BytesPerSector)
	if _, err := fs.f.ReadAt(buf, off); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	buf[0] = 0x00 // corrupt lead signature
	if _, err := fs.f.WriteAt(buf, off); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := fs.Grow(resizeBigSize); err != nil {
		t.Errorf("Grow with FSInfo-skipped = %v, want nil", err)
	}
}

// TestResize_GrowMisalignedSize ensures the cluster-alignment check fires
// even when newSize exceeds the current size and the partition guard.
func TestResize_GrowMisalignedSize(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()
	// newSize > cur, but +1 byte ruins cluster alignment.
	bad := resizeBigSize + 1
	if err := fs.Grow(bad); err == nil ||
		!strings.Contains(err.Error(), "not a multiple") {
		t.Errorf("Grow misaligned = %v, want alignment error", err)
	}
}

// TestResize_PickFATSizeStableAtMinFAT verifies pickFATSize accepts an
// already-large enough FAT and skips the iteration.
func TestResize_PickFATSizeStableAtMinFAT(t *testing.T) {
	// 16 MiB volume worth: totalSectors=32768, reserved=32, FATCount=2,
	// SPC=8. With minFATSize=64 (already huge), the FAT covers more than
	// enough clusters, so pickFATSize returns immediately.
	fatSize, clusters, err := pickFATSize(32768, 32, 2, 8, 512, 64)
	if err != nil {
		t.Fatalf("pickFATSize: %v", err)
	}
	if fatSize != 64 {
		t.Errorf("fatSize = %d, want 64 (locked at minimum)", fatSize)
	}
	if clusters == 0 {
		t.Errorf("clusters = 0")
	}
}

// TestResize_PickFATSizeZeroMinFAT exercises the minFATSize<1 branch.
func TestResize_PickFATSizeZeroMinFAT(t *testing.T) {
	fatSize, _, err := pickFATSize(32768, 32, 2, 8, 512, 0)
	if err != nil {
		t.Fatalf("pickFATSize: %v", err)
	}
	if fatSize < 1 {
		t.Errorf("fatSize = %d, want >= 1 (clamped)", fatSize)
	}
}

// TestResize_ShiftDataRegionNoop exercises shiftDataRegion's no-op
// short-circuits.
func TestResize_ShiftDataRegionNoop(t *testing.T) {
	_, _, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	// Same base → no-op.
	if err := fs.shiftDataRegion(100, 100, 5, 4096); err != nil {
		t.Errorf("shiftDataRegion(same base) = %v, want nil", err)
	}
	// Zero clusters → no-op.
	if err := fs.shiftDataRegion(100, 200, 0, 4096); err != nil {
		t.Errorf("shiftDataRegion(0 clusters) = %v, want nil", err)
	}
}

// TestResize_RelocateFATCopiesReadErr fails the canonical FAT0 read in
// relocateFATCopies and asserts the wrap.
func TestResize_RelocateFATCopiesReadErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	fatBase := fs.info.FATOffset(fs.partOffset)
	disk.readErr = func(off int64) error {
		// Fail any read large enough to be the FAT0 bulk-read.
		if off == fatBase {
			return errors.New("fat0 bulk read boom")
		}
		return nil
	}
	if err := fs.relocateFATCopies(int64(fs.info.FATSize), int64(fs.info.FATSize)+4); err == nil ||
		!strings.Contains(err.Error(), "read FAT0") {
		t.Errorf("relocateFATCopies read-FAT0 = %v, want read-FAT0 wrap", err)
	}
}

// TestResize_RelocateFATTailWriteErr exercises the "zero FAT tail" write
// path in relocateFATCopies (i.e., the bytes past the relocated FAT1).
func TestResize_RelocateFATTailWriteErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	bytesPerSector := int64(fs.info.BytesPerSector)
	fatBase := fs.info.FATOffset(fs.partOffset)
	oldFATSize := int64(fs.info.FATSize)
	newFATSize := oldFATSize + 4
	newFATBytes := newFATSize * bytesPerSector
	oldFATBytes := oldFATSize * bytesPerSector
	// The "zero FAT1 tail" target is at offset fatBase + newFATBytes + oldFATBytes.
	target := fatBase + newFATBytes + oldFATBytes
	disk.writeErr = func(off int64) error {
		if off == target {
			return errors.New("zero tail boom")
		}
		return nil
	}
	if err := fs.relocateFATCopies(oldFATSize, newFATSize); err == nil ||
		!strings.Contains(err.Error(), "zero FAT") {
		t.Errorf("relocateFATCopies tail-write = %v, want zero-FAT-tail wrap", err)
	}
}

// TestResize_RelocateFAT0TailWriteErr exercises the FAT0 tail zeroing
// write in relocateFATCopies.
func TestResize_RelocateFAT0TailWriteErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	bytesPerSector := int64(fs.info.BytesPerSector)
	fatBase := fs.info.FATOffset(fs.partOffset)
	oldFATSize := int64(fs.info.FATSize)
	newFATSize := oldFATSize + 4
	oldFATBytes := oldFATSize * bytesPerSector
	// FAT0 tail zeroing target: fatBase + oldFATBytes.
	target := fatBase + oldFATBytes
	disk.writeErr = func(off int64) error {
		if off == target {
			return errors.New("fat0 tail boom")
		}
		return nil
	}
	if err := fs.relocateFATCopies(oldFATSize, newFATSize); err == nil ||
		!strings.Contains(err.Error(), "zero FAT0 tail") {
		t.Errorf("relocateFATCopies FAT0 tail = %v, want zero-FAT0-tail wrap", err)
	}
}

// TestResize_WriteFATSizeReadErr fails the per-sector read inside
// writeFATSize.
func TestResize_WriteFATSizeReadErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	disk.readErr = func(off int64) error {
		if off == 0 {
			return errors.New("boot read boom")
		}
		return nil
	}
	if err := fs.writeFATSize(fs.info.FATSize); err == nil ||
		!strings.Contains(err.Error(), "read boot sector") {
		t.Errorf("writeFATSize read-fail = %v, want read-boot-sector wrap", err)
	}
}

// TestResize_WriteTotalSectorsReadErr does the same for writeTotalSectors.
func TestResize_WriteTotalSectorsReadErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	disk.readErr = func(off int64) error {
		if off == 0 {
			return errors.New("boot read boom")
		}
		return nil
	}
	if err := fs.writeTotalSectors(fs.info.TotalSectors); err == nil ||
		!strings.Contains(err.Error(), "read boot sector") {
		t.Errorf("writeTotalSectors read-fail = %v, want read-boot-sector wrap", err)
	}
}

// TestResize_ShrinkBelowDataClusterFloor covers the floor check in Shrink:
// when the target volume yields fewer data clusters than the resize floor,
// Shrink refuses with an explanatory error. We carefully size the mutated
// FAT and target so dataSectors is positive but the cluster count is <16.
func TestResize_ShrinkBelowDataClusterFloor(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()
	// Need: dataSectors > 0 but < 16 * sectorsPerCluster.
	// dataSectors = totalSectors - reserved - FATCount*FATSize.
	// Pick target totalSectors = reserved + 2*FATSize + 32 (4 clusters worth).
	bpsCluster := int64(fs.info.ClusterSize())
	target := int64(fs.info.ReservedSectors)*int64(fs.info.BytesPerSector) +
		2*int64(fs.info.FATSize)*int64(fs.info.BytesPerSector) +
		4*bpsCluster // 4 data clusters → below floor of 16
	if target%bpsCluster != 0 {
		target -= target % bpsCluster
	}
	if err := fs.Shrink(target); err == nil ||
		!strings.Contains(err.Error(), "below floor") {
		t.Errorf("Shrink below-floor = %v, want below-floor error", err)
	}
}

// TestResize_RefreshFSInfoReadEntryErr fails a FAT entry read inside
// refreshFSInfo and confirms the wrap.
func TestResize_RefreshFSInfoReadEntryErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	fatBase := fs.info.FATOffset(fs.partOffset)
	dataBase := fs.info.DataOffset(fs.partOffset)
	disk.readErr = func(off int64) error {
		// Trip any FAT entry read inside the data range of refreshFSInfo.
		if off > fatBase+12 && off < dataBase {
			return errors.New("fat entry read boom")
		}
		return nil
	}
	if err := fs.refreshFSInfo(); err == nil ||
		!strings.Contains(err.Error(), "read FAT entry") {
		t.Errorf("refreshFSInfo entry-read = %v, want read-FAT-entry wrap", err)
	}
}

// TestResize_RefreshFSInfoWriteErr fails the final FSInfo write and asserts
// the wrap.
func TestResize_RefreshFSInfoWriteErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	fsInfoOff := int64(fs.info.FSInfoSector) * int64(fs.info.BytesPerSector)
	disk.writeErr = func(off int64) error {
		if off == fsInfoOff {
			return errors.New("fsinfo write boom")
		}
		return nil
	}
	if err := fs.refreshFSInfo(); err == nil ||
		!strings.Contains(err.Error(), "write FSInfo") {
		t.Errorf("refreshFSInfo write = %v, want write-FSInfo wrap", err)
	}
}

// TestResize_ShrinkLeavesNoDataSectors covers Shrink's "leaves no data
// sectors" guard by mutating Info so the FAT region eats every sector.
func TestResize_ShrinkLeavesNoDataSectors(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()
	// Pretend the FAT region is huge — the resulting dataSectors goes
	// negative and Shrink hits its "leaves no data sectors" guard.
	fs.info.FATSize = fs.info.TotalSectors
	if err := fs.Shrink(resizeBaseSize - resizeClusterByte); err == nil ||
		!strings.Contains(err.Error(), "leaves no data sectors") {
		t.Errorf("Shrink no-data = %v, want leaves-no-data error", err)
	}
}

// TestResize_GrowLeavesNoDataSectors mirrors the above for Grow via
// pickFATSize, when the requested grow is so close to the reserved area
// that pickFATSize can't find any data clusters.
func TestResize_GrowLeavesNoDataSectors(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()
	// Force pickFATSize to fail by claiming a giant existing FATSize.
	// We bump the recorded FATSize so pickFATSize starts from a value
	// large enough to leave no data sectors for ANY cluster count.
	fs.info.FATSize = uint32(resizeBigSize / int64(fs.info.BytesPerSector))
	if err := fs.Grow(resizeBigSize); err == nil ||
		!strings.Contains(err.Error(), "fat32: Grow:") {
		t.Errorf("Grow pickFATSize-fail = %v, want pickFATSize wrap", err)
	}
}

// TestResize_ShrinkWriteZeroEntryErr exercises the "zero FAT entry" write
// failure path inside Shrink.
func TestResize_ShrinkWriteZeroEntryErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	fatBase := fs.info.FATOffset(fs.partOffset)
	dataBase := fs.info.DataOffset(fs.partOffset)
	// Fail any 4-byte write inside the FAT region past the canonical
	// header (offsets fatBase+12 .. dataBase).
	disk.writeErr = func(off int64) error {
		if off > fatBase+12 && off < dataBase {
			return errors.New("zero entry boom")
		}
		return nil
	}
	if err := fs.Shrink(resizeMinSize); err == nil ||
		!strings.Contains(err.Error(), "zero FAT") {
		t.Errorf("Shrink zero-entry error = %v, want zero-FAT wrap", err)
	}
}

// TestResize_ShrinkTotalSectorsErr fails the BPB rewrite in the Shrink path
// to verify the wrap.
func TestResize_ShrinkTotalSectorsErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	disk.writeErr = func(off int64) error {
		if off == 0 {
			return errors.New("bpb boom")
		}
		return nil
	}
	if err := fs.Shrink(resizeMinSize); err == nil ||
		!strings.Contains(err.Error(), "write total sectors") {
		t.Errorf("Shrink BPB error = %v, want write-total-sectors wrap", err)
	}
}

// TestResize_ShrinkFSInfoReadErr fails the FSInfo refresh in the Shrink
// path to verify the wrap.
func TestResize_ShrinkFSInfoReadErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	fsInfoOff := int64(fs.info.FSInfoSector) * int64(fs.info.BytesPerSector)
	disk.readErr = func(off int64) error {
		if off == fsInfoOff {
			return errors.New("fsinfo read boom")
		}
		return nil
	}
	if err := fs.Shrink(resizeMinSize); err == nil ||
		!strings.Contains(err.Error(), "refresh FSInfo") {
		t.Errorf("Shrink FSInfo error = %v, want refresh-FSInfo wrap", err)
	}
}

// TestResize_GrowRelocateFATError fails the FAT1 mirror write that happens
// AFTER shiftDataRegion, so Grow's "rebuild FAT copies" wrap fires.
func TestResize_GrowRelocateFATError(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	bytesPerSector := int64(fs.info.BytesPerSector)
	// Compute the new FAT1 mirror offset for a 16→32 grow.
	newFATSize, _, err := pickFATSize(
		resizeBigSize/bytesPerSector,
		int64(fs.info.ReservedSectors),
		int64(fs.info.FATCount),
		int64(fs.info.SectorsPerCluster),
		bytesPerSector,
		int64(fs.info.FATSize),
	)
	if err != nil {
		t.Fatalf("pickFATSize: %v", err)
	}
	fatBase := fs.info.FATOffset(fs.partOffset)
	target := fatBase + newFATSize*bytesPerSector
	disk.writeErr = func(off int64) error {
		if off == target {
			return errors.New("fat1 mirror boom")
		}
		return nil
	}
	if err := fs.Grow(resizeBigSize); err == nil ||
		!strings.Contains(err.Error(), "rebuild FAT copies") {
		t.Errorf("Grow relocate-FAT error = %v, want rebuild-FAT-copies wrap", err)
	}
}

// TestResize_GrowWriteTotalSectorsErr fails the BPB total-sector update
// that happens after the FAT shuffle, so Grow's "write total sectors"
// wrap fires from the path past the FAT-extension block.
func TestResize_GrowWriteTotalSectorsErr(t *testing.T) {
	_, disk, fs := openFaultFS(t, resizeBaseSize)
	defer fs.Close()
	// Count writes at offset 0 (primary BPB). The first BPB write is the
	// writeFATSize call (which writes at offset 0); the second is
	// writeTotalSectors. Fail only the second.
	bpbCalls := 0
	disk.writeErr = func(off int64) error {
		if off == 0 {
			bpbCalls++
			if bpbCalls >= 2 {
				return errors.New("totsec write boom")
			}
		}
		return nil
	}
	if err := fs.Grow(resizeBigSize); err == nil ||
		!strings.Contains(err.Error(), "write total sectors") {
		t.Errorf("Grow write-total-sectors = %v, want write-total-sectors wrap", err)
	}
}

// TestResize_BackupBootSectorMirrored confirms that when BackupBootSector
// is set (the default Format layout), both BPB copies receive the new
// TotalSectors / FATSize values. The existing happy-path test already
// covers TotalSectors; this one nails down FATSize on the backup too.
func TestResize_BackupBootSectorMirrored(t *testing.T) {
	path, fs := openResizeFS(t, resizeBaseSize)
	defer fs.Close()
	if err := fs.Grow(resizeBigSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	primaryFAT := func(off int64) uint32 {
		var buf [4]byte
		if _, err := fs.f.ReadAt(buf[:], off); err != nil {
			t.Fatalf("ReadAt FATSize at %d: %v", off, err)
		}
		return binary.LittleEndian.Uint32(buf[:])
	}
	backupOff := int64(fs.info.BackupBootSector)*int64(fs.info.BytesPerSector) + 36
	if got := primaryFAT(36); got != fs.info.FATSize {
		t.Errorf("primary FATSize = %d, want %d", got, fs.info.FATSize)
	}
	if got := primaryFAT(backupOff); got != fs.info.FATSize {
		t.Errorf("backup FATSize = %d, want %d", got, fs.info.FATSize)
	}
	_ = path
}
