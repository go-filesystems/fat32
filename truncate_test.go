package filesystem_fat32

import (
	"bytes"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// TestTruncaterInterface confirms the driver advertises the optional
// in-place truncation interface.
func TestTruncaterInterface(t *testing.T) {
	var fsIfc filesystem.Filesystem = (*fat32FS)(nil)
	if _, ok := fsIfc.(filesystem.Truncater); !ok {
		t.Fatal("fat32FS does not satisfy filesystem.Truncater")
	}
}

// fileSize reads the 8.3 FileSize field recorded in the root directory entry
// for a top-level file, so tests can assert the on-disk size independently of
// the byte count returned by ReadFile.
func entryFileSize(t *testing.T, fs *fat32FS, name string) uint32 {
	t.Helper()
	st, err := fs.Stat("/" + name)
	if err != nil {
		t.Fatalf("Stat %s: %v", name, err)
	}
	return uint32(st.Size())
}

// TestTruncateShrinkGrow exercises shrink, grow, and persistence across a
// close/re-open cycle on a real Format'd image with a multi-cluster file.
func TestTruncateShrinkGrow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncate.fat32")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{Label: "TRUNC"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsIfc.(*fat32FS)
	tr := fsIfc.(filesystem.Truncater)
	clusterSize := int64(fs.info.ClusterSize()) // 4 KiB

	// Multi-cluster file: 3 clusters + a partial 4th (≈ 13 KiB).
	const orig = 13000
	data := make([]byte, orig)
	for i := range data {
		data[i] = byte('A' + i%26)
	}
	if err := fsIfc.WriteFile("/data.bin", data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entry, _, err := fs.resolvePath("/data.bin")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	firstCluster := entry.cluster
	origChain := chainClusters(t, fs, firstCluster)
	wantClusters := int((orig + clusterSize - 1) / clusterSize)
	if len(origChain) != wantClusters {
		t.Fatalf("orig chain length = %d, want %d", len(origChain), wantClusters)
	}
	freeAfterWrite, err := countFreeClusters(fs)
	if err != nil {
		t.Fatalf("countFreeClusters: %v", err)
	}

	// ── Shrink to 5000 bytes (2 clusters) ─────────────────────────────────────
	const shrunk = 5000
	if err := tr.Truncate("/data.bin", shrunk); err != nil {
		t.Fatalf("Truncate shrink: %v", err)
	}
	if got := entryFileSize(t, fs, "data.bin"); got != shrunk {
		t.Fatalf("FileSize after shrink = %d, want %d", got, shrunk)
	}
	rd, err := fsIfc.ReadFile("/data.bin")
	if err != nil {
		t.Fatalf("ReadFile after shrink: %v", err)
	}
	if len(rd) != shrunk {
		t.Fatalf("ReadFile len after shrink = %d, want %d", len(rd), shrunk)
	}
	if !bytes.Equal(rd, data[:shrunk]) {
		t.Fatal("ReadFile content after shrink does not match original prefix")
	}
	shrinkChainLen := len(chainClusters(t, fs, entry.cluster))
	wantShrinkClusters := int((shrunk + clusterSize - 1) / clusterSize)
	if shrinkChainLen != wantShrinkClusters {
		t.Fatalf("chain length after shrink = %d, want %d", shrinkChainLen, wantShrinkClusters)
	}
	freeAfterShrink, err := countFreeClusters(fs)
	if err != nil {
		t.Fatalf("countFreeClusters: %v", err)
	}
	if freed := freeAfterShrink - freeAfterWrite; freed != wantClusters-wantShrinkClusters {
		t.Fatalf("freed clusters = %d, want %d", freed, wantClusters-wantShrinkClusters)
	}

	// ── Grow back to 10000 bytes (3 clusters) ─────────────────────────────────
	const grown = 10000
	if err := tr.Truncate("/data.bin", grown); err != nil {
		t.Fatalf("Truncate grow: %v", err)
	}
	if got := entryFileSize(t, fs, "data.bin"); got != grown {
		t.Fatalf("FileSize after grow = %d, want %d", got, grown)
	}
	rd, err = fsIfc.ReadFile("/data.bin")
	if err != nil {
		t.Fatalf("ReadFile after grow: %v", err)
	}
	if len(rd) != grown {
		t.Fatalf("ReadFile len after grow = %d, want %d", len(rd), grown)
	}
	// The first shrunk bytes are unchanged; the tail is zero-filled.
	if !bytes.Equal(rd[:shrunk], data[:shrunk]) {
		t.Fatal("grown file does not preserve retained prefix")
	}
	for i := shrunk; i < grown; i++ {
		if rd[i] != 0 {
			t.Fatalf("grown tail byte %d = %d, want 0", i, rd[i])
		}
	}

	// ── Persist across close/re-open ──────────────────────────────────────────
	if err := fsIfc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer reIfc.Close()
	re := reIfc.(*fat32FS)
	if got := entryFileSize(t, re, "data.bin"); got != grown {
		t.Fatalf("FileSize after reopen = %d, want %d", got, grown)
	}
	rd, err = reIfc.ReadFile("/data.bin")
	if err != nil {
		t.Fatalf("ReadFile after reopen: %v", err)
	}
	if len(rd) != grown || !bytes.Equal(rd[:shrunk], data[:shrunk]) {
		t.Fatal("reopened file content mismatch")
	}
	for i := shrunk; i < grown; i++ {
		if rd[i] != 0 {
			t.Fatalf("reopened tail byte %d = %d, want 0", i, rd[i])
		}
	}
}

// TestTruncateToZeroAndFromZero covers the first-cluster transitions: shrinking
// to zero releases the whole chain and clears the first-cluster fields, and a
// subsequent grow from zero allocates a fresh head cluster.
func TestTruncateToZeroAndFromZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trunc-zero.fat32")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{Label: "TZERO"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsIfc.(*fat32FS)
	tr := fsIfc.(filesystem.Truncater)
	defer fsIfc.Close()

	data := bytes.Repeat([]byte("xyz"), 4000) // 12000 bytes, multi-cluster
	if err := fsIfc.WriteFile("/z.bin", data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	freeBefore, err := countFreeClusters(fs)
	if err != nil {
		t.Fatalf("countFreeClusters: %v", err)
	}
	usedClusters := len(chainClusters(t, fs, mustCluster(t, fs, "/z.bin")))

	// Shrink to zero.
	if err := tr.Truncate("/z.bin", 0); err != nil {
		t.Fatalf("Truncate to zero: %v", err)
	}
	if got := entryFileSize(t, fs, "z.bin"); got != 0 {
		t.Fatalf("FileSize after truncate-to-zero = %d, want 0", got)
	}
	if c := mustCluster(t, fs, "/z.bin"); c != 0 {
		t.Fatalf("first cluster after truncate-to-zero = %d, want 0", c)
	}
	rd, err := fsIfc.ReadFile("/z.bin")
	if err != nil {
		t.Fatalf("ReadFile empty: %v", err)
	}
	if len(rd) != 0 {
		t.Fatalf("ReadFile len after truncate-to-zero = %d, want 0", len(rd))
	}
	freeAfter, err := countFreeClusters(fs)
	if err != nil {
		t.Fatalf("countFreeClusters: %v", err)
	}
	if freed := freeAfter - freeBefore; freed != usedClusters {
		t.Fatalf("freed clusters = %d, want %d", freed, usedClusters)
	}

	// Grow from zero.
	const grown = 6000
	if err := tr.Truncate("/z.bin", grown); err != nil {
		t.Fatalf("Truncate from zero: %v", err)
	}
	if got := entryFileSize(t, fs, "z.bin"); got != grown {
		t.Fatalf("FileSize after grow-from-zero = %d, want %d", got, grown)
	}
	if c := mustCluster(t, fs, "/z.bin"); c < 2 {
		t.Fatalf("first cluster after grow-from-zero = %d, want >= 2", c)
	}
	rd, err = fsIfc.ReadFile("/z.bin")
	if err != nil {
		t.Fatalf("ReadFile after grow-from-zero: %v", err)
	}
	if len(rd) != grown {
		t.Fatalf("ReadFile len after grow-from-zero = %d, want %d", len(rd), grown)
	}
	for i, b := range rd {
		if b != 0 {
			t.Fatalf("grow-from-zero byte %d = %d, want 0", i, b)
		}
	}
}

// TestTruncateRejectsDirectory confirms directories cannot be truncated.
func TestTruncateRejectsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trunc-dir.fat32")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{Label: "TDIR"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()
	if err := fsIfc.MkDir("/sub", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fsIfc.(filesystem.Truncater).Truncate("/sub", 0); err == nil {
		t.Fatal("Truncate on a directory unexpectedly succeeded")
	}
	if err := fsIfc.(filesystem.Truncater).Truncate("/missing.bin", 0); err == nil {
		t.Fatal("Truncate on a missing file unexpectedly succeeded")
	}
}

func mustCluster(t *testing.T, fs *fat32FS, path string) uint32 {
	t.Helper()
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		t.Fatalf("resolvePath %s: %v", path, err)
	}
	return entry.cluster
}
