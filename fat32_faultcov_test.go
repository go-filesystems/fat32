package filesystem_fat32

import (
	"errors"
	"testing"
	"time"
)

// errInjected is the sentinel returned by the fault-injecting diskRW used to
// drive the otherwise-unreachable I/O error branches in Truncate / shrinkChain
// / growChain / writeLabelAt.
var errInjected = errors.New("fat32: injected disk error")

// faultRW wraps a real diskRW and fails I/O selectively. Faults can be armed
// either by 1-based call index (failReadAt/failWriteAt) or by half-open byte
// region [regLo, regHi) on a given operation — the region form is deterministic
// regardless of how many unrelated calls precede the targeted FAT/data access.
type faultRW struct {
	inner       diskRW
	reads       int
	writes      int
	failReadAt  int // 0 = disabled
	failWriteAt int // 0 = disabled

	failReadLo, failReadHi   int64 // fail ReadAt whose off ∈ [lo,hi)
	failWriteLo, failWriteHi int64 // fail WriteAt whose off ∈ [lo,hi)
	readRegionSkip           int   // let the first N in-region reads succeed
	writeRegionSkip          int   // let the first N in-region writes succeed
	readRegionHits           int
	writeRegionHits          int
}

func (f *faultRW) ReadAt(p []byte, off int64) (int, error) {
	f.reads++
	if f.failReadAt != 0 && f.reads == f.failReadAt {
		return 0, errInjected
	}
	if f.failReadHi > f.failReadLo && off >= f.failReadLo && off < f.failReadHi {
		f.readRegionHits++
		if f.readRegionHits > f.readRegionSkip {
			return 0, errInjected
		}
	}
	return f.inner.ReadAt(p, off)
}

func (f *faultRW) WriteAt(p []byte, off int64) (int, error) {
	f.writes++
	if f.failWriteAt != 0 && f.writes == f.failWriteAt {
		return 0, errInjected
	}
	if f.failWriteHi > f.failWriteLo && off >= f.failWriteLo && off < f.failWriteHi {
		f.writeRegionHits++
		if f.writeRegionHits > f.writeRegionSkip {
			return 0, errInjected
		}
	}
	return f.inner.WriteAt(p, off)
}

func (f *faultRW) Close() error { return f.inner.Close() }

// failFATReads arms a fault on every ReadAt that targets the (first) FAT region.
func (f *faultRW) failFATReads(fs *fat32FS) {
	base := fs.info.FATOffset(fs.partOffset)
	f.failReadLo = base
	f.failReadHi = base + int64(fs.info.FATSize)*int64(fs.info.BytesPerSector)
}

// failFATWrites arms a fault on every WriteAt that targets the (first) FAT region.
func (f *faultRW) failFATWrites(fs *fat32FS) {
	base := fs.info.FATOffset(fs.partOffset)
	f.failWriteLo = base
	f.failWriteHi = base + int64(fs.info.FATSize)*int64(fs.info.BytesPerSector)
}

// failDataWrites arms a fault on every WriteAt that targets the data region.
func (f *faultRW) failDataWrites(fs *fat32FS) {
	f.failWriteLo = fs.info.DataOffset(fs.partOffset)
	f.failWriteHi = 1 << 62
}

// wrapFault swaps the filesystem's backing diskRW for a faultRW and returns it
// so the caller can arm fault points after the file has been populated.
func wrapFault(fs *fat32FS) *faultRW {
	w := &faultRW{inner: fs.f}
	fs.f = w
	return w
}

// makeFileWithClusters formats a fresh image, writes a file occupying several
// clusters, and returns the concrete fs plus the file path inside it.
func makeFileWithClusters(t *testing.T, nClusters int) (*fat32FS, string) {
	t.Helper()
	_, fs := openResizeFS(t, resizeBaseSize)
	data := make([]byte, int(fs.info.ClusterSize())*nClusters)
	for i := range data {
		data[i] = byte(i)
	}
	if err := fs.WriteFile("/big.bin", data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return fs, "/big.bin"
}

// --- Truncate validation branches (fat32.go:488/492/495/499/514) ----------

func TestTruncateNegativeSize(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	if err := fs.Truncate("/x", -1); err == nil {
		t.Fatal("Truncate with negative size: want error, got nil")
	}
}

func TestTruncateRootPath(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	// getParentDir("/") yields an empty name → "not a regular file".
	if err := fs.Truncate("/", 0); err == nil {
		t.Fatal("Truncate of root: want error, got nil")
	}
}

func TestTruncateMissingParent(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	if err := fs.Truncate("/nope/file.txt", 0); err == nil {
		t.Fatal("Truncate under missing parent: want error, got nil")
	}
}

func TestTruncateMissingFile(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	if err := fs.Truncate("/ghost.txt", 0); err == nil {
		t.Fatal("Truncate of missing file: want error, got nil")
	}
}

func TestTruncateDirectory(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	if err := fs.MkDir("/sub", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.Truncate("/sub", 0); err == nil {
		t.Fatal("Truncate of directory: want error, got nil")
	}
}

func TestTruncateNoChange(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	if err := fs.WriteFile("/same.txt", []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// newSize == oldSize → early return nil (fat32.go:514).
	if err := fs.Truncate("/same.txt", 5); err != nil {
		t.Fatalf("Truncate no-op: %v", err)
	}
}

func TestTruncateReadDirError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 2)
	w := wrapFault(fs)
	// readDirBuf is the first ReadAt issued by Truncate after getParentDir.
	w.failReadAt = w.reads + 1
	if err := fs.Truncate(path, 0); err == nil {
		t.Fatal("Truncate with readDirBuf error: want error, got nil")
	}
}

// fileFirstCluster resolves path and returns its starting cluster, asserting
// the file occupies a real chain (cluster >= 2).
func fileFirstCluster(t *testing.T, fs *fat32FS, path string) uint32 {
	t.Helper()
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		t.Fatalf("resolvePath %q: %v", path, err)
	}
	if entry.cluster < 2 {
		t.Fatalf("file %q has no chain (cluster=%d)", path, entry.cluster)
	}
	return entry.cluster
}

// --- shrinkChain error paths, exercised directly ---------------------------
// Calling shrinkChain directly isolates its FAT/data I/O so faults land on the
// intended access (path resolution no longer perturbs the call counters).

func TestShrinkChainEmpty(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	// firstCluster < 2 → early return nil (fat32.go:548).
	if err := fs.shrinkChain(0, 3, 100); err != nil {
		t.Fatalf("shrinkChain(0,...): %v", err)
	}
}

func TestShrinkChainWalkReadError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 4)
	first := fileFirstCluster(t, fs, path)
	w := wrapFault(fs)
	w.failFATReads(fs) // first FAT read is the walk read (fat32.go:562).
	if err := fs.shrinkChain(first, 3, int64(fs.info.ClusterSize())*3); err == nil {
		t.Fatal("shrinkChain walk read error: want error, got nil")
	}
}

func TestShrinkChainShortChain(t *testing.T) {
	fs, path := makeFileWithClusters(t, 2)
	first := fileFirstCluster(t, fs, path)
	// Ask to keep more clusters than the chain actually has: the walk meets the
	// EOC sentinel and breaks (fat32.go:566), then 573 returns nil.
	if err := fs.shrinkChain(first, 8, int64(fs.info.ClusterSize())*8); err != nil {
		t.Fatalf("shrinkChain short chain: %v", err)
	}
}

func TestShrinkChainTailReadError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 3)
	first := fileFirstCluster(t, fs, path)
	w := wrapFault(fs)
	// keepClusters == 1 → no walk; the first FAT read is the tail read (579).
	w.failFATReads(fs)
	if err := fs.shrinkChain(first, 1, int64(fs.info.ClusterSize())); err == nil {
		t.Fatal("shrinkChain tail read error: want error, got nil")
	}
}

func TestShrinkChainSetFATError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 3)
	first := fileFirstCluster(t, fs, path)
	w := wrapFault(fs)
	// keepClusters == 1: tail read (579) succeeds, then setFATEntry (583)
	// writes the FAT → fail it.
	w.failFATWrites(fs)
	if err := fs.shrinkChain(first, 1, int64(fs.info.ClusterSize())); err == nil {
		t.Fatal("shrinkChain setFATEntry error: want error, got nil")
	}
}

func TestShrinkChainFreeChainError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 3)
	first := fileFirstCluster(t, fs, path)
	w := wrapFault(fs)
	// keepClusters == 1: tail read (579) and the terminating setFATEntry (583)
	// succeed (skip one FAT write), then freeChain's FAT write fails (587).
	w.failFATWrites(fs)
	w.writeRegionSkip = 1
	if err := fs.shrinkChain(first, 1, int64(fs.info.ClusterSize())); err == nil {
		t.Fatal("shrinkChain freeChain error: want error, got nil")
	}
}

func TestShrinkChainSlackWriteError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 3)
	first := fileFirstCluster(t, fs, path)
	w := wrapFault(fs)
	// Non-aligned newSize → zero-fill slack in last retained cluster (599).
	w.failDataWrites(fs)
	if err := fs.shrinkChain(first, 1, int64(fs.info.ClusterSize())-7); err == nil {
		t.Fatal("shrinkChain slack write error: want error, got nil")
	}
}

func TestTruncateShrinkToZero(t *testing.T) {
	fs, path := makeFileWithClusters(t, 3)
	// keepClusters == 0 → freeChain path (fat32.go:551) and firstCluster reset.
	if err := fs.Truncate(path, 0); err != nil {
		t.Fatalf("Truncate to zero: %v", err)
	}
	got, err := fs.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after truncate-to-zero len = %d, want 0", len(got))
	}
}

// --- growChain error paths, exercised directly -----------------------------

func TestGrowChainSlackWriteError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 1)
	first := fileFirstCluster(t, fs, path)
	w := wrapFault(fs)
	// oldSize non-aligned → growChain zero-fills the old last cluster's slack
	// (data WriteAt, fat32.go:637) before allocating.
	w.failDataWrites(fs)
	cs := int64(fs.info.ClusterSize())
	if _, err := fs.growChain(first, 1, 3, cs-13); err == nil {
		t.Fatal("growChain slack write error: want error, got nil")
	}
}

func TestGrowChainSlackWalkReadError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 3)
	first := fileFirstCluster(t, fs, path)
	w := wrapFault(fs)
	// oldClusters > 1 + non-aligned oldSize → growChain walks the chain to find
	// the last cluster for the slack fill (FAT read, fat32.go:624).
	w.failFATReads(fs)
	cs := int64(fs.info.ClusterSize())
	if _, err := fs.growChain(first, 3, 6, cs*2+5); err == nil {
		t.Fatal("growChain slack walk read error: want error, got nil")
	}
}

func TestGrowChainNoGrowth(t *testing.T) {
	fs, path := makeFileWithClusters(t, 2)
	first := fileFirstCluster(t, fs, path)
	cs := int64(fs.info.ClusterSize())
	// newClusters == oldClusters → need <= 0 early return (fat32.go:645).
	got, err := fs.growChain(first, 2, 2, cs*2)
	if err != nil {
		t.Fatalf("growChain no-growth: %v", err)
	}
	if got != first {
		t.Fatalf("growChain no-growth returned %d, want %d", got, first)
	}
}

func TestGrowChainAllocClusterError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 1)
	first := fileFirstCluster(t, fs, path)
	w := wrapFault(fs)
	// allocCluster scans the FAT for a free entry (FAT read) → fail it so the
	// allocation error + rollback path runs (fat32.go:659-661).
	w.failFATReads(fs)
	cs := int64(fs.info.ClusterSize())
	if _, err := fs.growChain(first, 1, 4, cs); err == nil {
		t.Fatal("growChain allocCluster error: want error, got nil")
	}
}

func TestGrowChainZeroClusterWriteError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 1)
	first := fileFirstCluster(t, fs, path)
	w := wrapFault(fs)
	// Aligned grow: each new cluster is zero-filled (data WriteAt, 668).
	w.failDataWrites(fs)
	cs := int64(fs.info.ClusterSize())
	if _, err := fs.growChain(first, 1, 4, cs); err == nil {
		t.Fatal("growChain zero-cluster write error: want error, got nil")
	}
}

func TestGrowChainChainWriteError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 1)
	first := fileFirstCluster(t, fs, path)
	cs := int64(fs.info.ClusterSize())

	// Dry-run an identical grow to count FAT writes. The trailing FAT writes
	// are the run-linking setFATEntry calls (fat32.go:674-678 / 700); failing
	// the first of those exercises the link-error + rollback branch (675).
	dryFS, dryPath := makeFileWithClusters(t, 1)
	dryFirst := fileFirstCluster(t, dryFS, dryPath)
	dw := wrapFault(dryFS)
	dw.failWriteLo = dryFS.info.FATOffset(dryFS.partOffset)
	dw.failWriteHi = dw.failWriteLo + int64(dryFS.info.FATSize)*int64(dryFS.info.BytesPerSector)
	dw.writeRegionSkip = 1 << 30 // count only
	if _, err := dryFS.growChain(dryFirst, 1, 4, cs); err != nil {
		t.Fatalf("dry-run growChain: %v", err)
	}
	// failFATWrites matches only FAT0, so each setFATEntry is exactly one
	// in-region write regardless of FATCount. Order: need EOC marks, (need-1)
	// internal links, then 1 tail link — the last in-region write. Skip up to it.
	skip := dw.writeRegionHits - 1
	if skip < 0 {
		skip = 0
	}

	w := wrapFault(fs)
	w.failFATWrites(fs)
	w.writeRegionSkip = skip
	if _, err := fs.growChain(first, 1, 4, cs); err == nil {
		t.Fatal("growChain run-link write error: want error, got nil")
	}
}

func TestGrowChainInternalLinkWriteError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 1)
	first := fileFirstCluster(t, fs, path)
	cs := int64(fs.info.ClusterSize())

	// Dry-run to count FAT writes, then fail the FIRST run-internal link write
	// (fat32.go:675) — the link loop runs before the tail-link write (700).
	dryFS, dryPath := makeFileWithClusters(t, 1)
	dryFirst := fileFirstCluster(t, dryFS, dryPath)
	dw := wrapFault(dryFS)
	dw.failWriteLo = dryFS.info.FATOffset(dryFS.partOffset)
	dw.failWriteHi = dw.failWriteLo + int64(dryFS.info.FATSize)*int64(dryFS.info.BytesPerSector)
	dw.writeRegionSkip = 1 << 30
	if _, err := dryFS.growChain(dryFirst, 1, 4, cs); err != nil {
		t.Fatalf("dry-run growChain: %v", err)
	}
	// In-region writes (FAT0 only): need EOC marks, then (need-1) internal links,
	// then 1 tail link. With need=3 there are 6 writes total; the first internal
	// link is write #4. Skip the EOC marks (= total - need = the link writes →
	// skip total-3 so write #4 is the first to fail).
	skip := dw.writeRegionHits - 3 // need=3 link writes (2 internal + 1 tail)
	if skip < 0 {
		skip = 0
	}

	w := wrapFault(fs)
	w.failFATWrites(fs)
	w.writeRegionSkip = skip
	if _, err := fs.growChain(first, 1, 4, cs); err == nil {
		t.Fatal("growChain internal-link write error: want error, got nil")
	}
}

func TestGrowChainEOCMarkWriteError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 1)
	first := fileFirstCluster(t, fs, path)
	cs := int64(fs.info.ClusterSize())
	w := wrapFault(fs)
	// Fail the first FAT write: that is the EOC mark on the first freshly
	// allocated cluster (fat32.go:663), which rolls back the allocation.
	w.failFATWrites(fs)
	if _, err := fs.growChain(first, 1, 4, cs); err == nil {
		t.Fatal("growChain EOC-mark write error: want error, got nil")
	}
}

// --- Truncate-level error propagation (fat32.go:524/532) ------------------

func TestTruncateShrinkErrorPropagates(t *testing.T) {
	fs, path := makeFileWithClusters(t, 4)
	w := wrapFault(fs)
	// Only data WRITES fail; path resolution / readDirBuf use reads and the FAT,
	// so they succeed and the error surfaces from shrinkChain's slack zero-fill
	// (fat32.go:524 "return err").
	w.failDataWrites(fs)
	newSize := int64(fs.info.ClusterSize())*2 + 9 // non-aligned → slack fill
	if err := fs.Truncate(path, newSize); err == nil {
		t.Fatal("Truncate shrink error propagation: want error, got nil")
	}
}

func TestTruncateGrowErrorPropagates(t *testing.T) {
	fs, path := makeFileWithClusters(t, 1)
	w := wrapFault(fs)
	// growChain zero-fills new clusters via data writes; failing them surfaces
	// as Truncate's "return 0, err" → Truncate's growChain error (fat32.go:532).
	w.failDataWrites(fs)
	newSize := int64(fs.info.ClusterSize()) * 4
	if err := fs.Truncate(path, newSize); err == nil {
		t.Fatal("Truncate grow error propagation: want error, got nil")
	}
}

func TestGrowChainSlackShortChain(t *testing.T) {
	fs, path := makeFileWithClusters(t, 2)
	first := fileFirstCluster(t, fs, path)
	cs := int64(fs.info.ClusterSize())
	// Claim more oldClusters than the chain actually has and a non-aligned
	// oldSize: the slack walk meets the EOC sentinel early, sets last=0 and
	// breaks (fat32.go:628), skipping the slack zero-fill.
	if _, err := fs.growChain(first, 5, 7, cs*4+9); err != nil {
		t.Fatalf("growChain slack short chain: %v", err)
	}
}

func TestGrowChainTailWalkReadError(t *testing.T) {
	fs, path := makeFileWithClusters(t, 3)
	first := fileFirstCluster(t, fs, path)
	cs := int64(fs.info.ClusterSize())

	// First do a no-fault dry run on an identical, independent fs to learn how
	// many FAT reads precede the post-allocation tail walk. growChain's only
	// trailing read loop is the tail walk (fat32.go:688-699); counting total
	// in-region reads of a successful grow gives us the index just before it.
	dryFS, dryPath := makeFileWithClusters(t, 3)
	dryFirst := fileFirstCluster(t, dryFS, dryPath)
	dw := wrapFault(dryFS)
	dw.failReadLo = dryFS.info.FATOffset(dryFS.partOffset)
	dw.failReadHi = dw.failReadLo + int64(dryFS.info.FATSize)*int64(dryFS.info.BytesPerSector)
	dw.readRegionSkip = 1 << 30 // count but never fail
	if _, err := dryFS.growChain(dryFirst, 3, 6, cs*3); err != nil {
		t.Fatalf("dry-run growChain: %v", err)
	}
	// The tail walk issues >=1 read; failing the very first read after the
	// allocation/zero-fill/EOC phase lands inside it. allocCluster + the EOC
	// setFATEntry reads dominate; the walk reads are the final ones. Skipping
	// (totalReads - chainLen) leaves the walk reads to fail.
	walkReads := int64(3) // chain length first..first+2
	skip := dw.readRegionHits - int(walkReads)
	if skip < 0 {
		skip = 0
	}

	w := wrapFault(fs)
	w.failFATReads(fs)
	w.readRegionSkip = skip
	if _, err := fs.growChain(first, 3, 6, cs*3); err == nil {
		t.Fatal("growChain tail walk read error: want error, got nil")
	}
}

func TestTruncateGrowFromEmpty(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	if err := fs.WriteFile("/empty.bin", nil, 0o644); err != nil {
		t.Fatalf("WriteFile empty: %v", err)
	}
	// firstCluster == 0 path: growChain allocates a fresh head cluster.
	newSize := int64(fs.info.ClusterSize()) * 2
	if err := fs.Truncate("/empty.bin", newSize); err != nil {
		t.Fatalf("Truncate grow-from-empty: %v", err)
	}
	got, err := fs.ReadFile("/empty.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if int64(len(got)) != newSize {
		t.Fatalf("grown size = %d, want %d", len(got), newSize)
	}
}

// --- writeLabelAt error branches (label.go:72/75/80/85) -------------------

func TestSetLabelReadError(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	w := wrapFault(fs)
	w.failReadAt = 1 // first ReadAt is writeLabelAt's boot-sector read.
	if err := fs.SetLabel("NEW"); err == nil {
		t.Fatal("SetLabel with boot-sector read error: want error, got nil")
	}
}

func TestSetLabelWriteError(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	w := wrapFault(fs)
	w.failWriteAt = 1 // first WriteAt is the primary boot-sector rewrite.
	if err := fs.SetLabel("NEW"); err == nil {
		t.Fatal("SetLabel with boot-sector write error: want error, got nil")
	}
}

func TestSetLabelBackupWriteError(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	if fs.info.BackupBootSector == 0 {
		t.Skip("image has no backup boot sector")
	}
	w := wrapFault(fs)
	// Primary write succeeds (write #1); backup write (#2) fails, hitting the
	// "backup boot sector" wrapped-error branch in SetLabel (label.go:57).
	w.failWriteAt = 2
	if err := fs.SetLabel("NEW"); err == nil {
		t.Fatal("SetLabel with backup write error: want error, got nil")
	}
}

func TestSetLabelBadSignature(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	// Corrupt the boot signature so writeLabelAt's signature check fails.
	if _, err := fs.f.WriteAt([]byte{0x00, 0x00}, fs.partOffset+int64(bsOffBootSignature)); err != nil {
		t.Fatalf("corrupt signature: %v", err)
	}
	if err := fs.SetLabel("NEW"); err == nil {
		t.Fatal("SetLabel with bad signature: want error, got nil")
	}
}

func TestSetLabelBytesPerSectorMismatch(t *testing.T) {
	_, fs := openResizeFS(t, resizeBaseSize)
	// Corrupt BPB_BytsPerSec so the post-read sanity check trips (label.go:80).
	if _, err := fs.f.WriteAt([]byte{0xFF, 0xFF}, fs.partOffset+int64(bsOffBytesPerSect)); err != nil {
		t.Fatalf("corrupt bytes-per-sector: %v", err)
	}
	if err := fs.SetLabel("NEW"); err == nil {
		t.Fatal("SetLabel with bytes-per-sector mismatch: want error, got nil")
	}
}

// --- pickFATSize edge cases (resize.go:277/287/295) -----------------------

func TestPickFATSizeNoDataSectors(t *testing.T) {
	// reservedSectors >= totalSectors → no data sectors.
	if _, _, err := pickFATSize(10, 10, 2, 8, 512, 1); err == nil {
		t.Fatal("pickFATSize with no data sectors: want error, got nil")
	}
}

func TestPickFATSizeNoDataClusters(t *testing.T) {
	// A huge FAT floor consumes every non-reserved sector → no data clusters.
	if _, _, err := pickFATSize(100, 10, 2, 8, 512, 1000); err == nil {
		t.Fatal("pickFATSize with no data clusters: want error, got nil")
	}
}

func TestPickFATSizeConverges(t *testing.T) {
	// Sanity: a normal geometry converges and returns positive values.
	fatSize, clusters, err := pickFATSize(40960, 32, 2, 8, 512, 1)
	if err != nil {
		t.Fatalf("pickFATSize: %v", err)
	}
	if fatSize <= 0 || clusters == 0 {
		t.Fatalf("pickFATSize returned fatSize=%d clusters=%d", fatSize, clusters)
	}
}

// --- makeShortAlias long-extension + long collision suffix ----------------

func TestMakeShortAliasLongExtension(t *testing.T) {
	taken := map[[11]byte]bool{}
	// Extension longer than 3 chars must be truncated to 3 (fat32.go:1100).
	alias := makeShortAlias("archive.tarball", taken)
	ext := string(alias[8:11])
	if ext != "TAR" {
		t.Fatalf("alias extension = %q, want %q", ext, "TAR")
	}
}

func TestMakeShortAliasManyCollisions(t *testing.T) {
	taken := map[[11]byte]bool{}
	// Force a large collision run so the numeric suffix grows past one digit,
	// shrinking the stem and (eventually) clamping stemLen to >=1 (fat32.go:1108).
	base := "AAAAAAAA.TXT"
	for i := 0; i < 1500; i++ {
		alias := makeShortAlias(base, taken)
		if taken[alias] {
			t.Fatalf("makeShortAlias returned a duplicate alias at iteration %d", i)
		}
		taken[alias] = true
	}
}

// --- encodeFATDateTime pre-1980 epoch clamp (fat32.go:31) -----------------

func TestEncodeFATDateTimePre1980(t *testing.T) {
	date, tod, tenth := encodeFATDateTime(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC))
	if date != fatEpochDate || tod != 0 || tenth != 0 {
		t.Fatalf("pre-1980 encode = (%d,%d,%d), want (%d,0,0)", date, tod, tenth, fatEpochDate)
	}
}
