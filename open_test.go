package filesystem_fat32

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/safeio"
)

// probeOpener asserts the capability is reachable the way a caller reaches it —
// through the filesystem.Filesystem interface Open returns, not the concrete
// type — and hands back the Opener.
func probeOpener(t *testing.T, fsIfc filesystem.Filesystem) filesystem.Opener {
	t.Helper()
	o, ok := fsIfc.(filesystem.Opener)
	if !ok {
		t.Fatal("fat32 does not satisfy filesystem.Opener")
	}
	return o
}

// checkAgainstReadFile is the verification that matters: for a real file on a
// real image, every ReadAt must return EXACTLY the corresponding slice of what
// ReadFile returns. It sweeps offsets deliberately chosen to straddle cluster
// boundaries, because an off-by-one-cluster in the offset→cluster mapping is
// invisible on a single-cluster file and corrupts everything on a longer one.
func checkAgainstReadFile(t *testing.T, fsIfc filesystem.Filesystem, path string, clusterSize int64) {
	t.Helper()
	want, err := fsIfc.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	f, err := probeOpener(t, fsIfc).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	defer f.Close()

	if got := f.Size(); got != int64(len(want)) {
		t.Fatalf("%s: Size() = %d, want %d (len of ReadFile)", path, got, len(want))
	}
	size := int64(len(want))

	// Offsets: the start, both sides of and exactly on every cluster
	// boundary, a few interior points, and the very end.
	offsets := map[int64]bool{0: true, size: true}
	if size > 0 {
		offsets[size-1] = true
	}
	for b := clusterSize; b < size+clusterSize; b += clusterSize {
		for _, o := range []int64{b - 1, b, b + 1} {
			if o >= 0 && o <= size {
				offsets[o] = true
			}
		}
	}
	// Lengths: sub-cluster, exactly a cluster, straddling several.
	lengths := []int{1, 7, int(clusterSize) - 1, int(clusterSize), int(clusterSize) + 1, 3*int(clusterSize) + 5}

	for off := range offsets {
		for _, l := range lengths {
			if l <= 0 {
				continue
			}
			p := make([]byte, l)
			n, err := f.ReadAt(p, off)

			// Expected bytes and expected io.ReaderAt outcome.
			end := off + int64(l)
			short := end > size
			wantN := l
			if short {
				wantN = int(size - off)
				if wantN < 0 {
					wantN = 0
				}
			}
			if n != wantN {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) n = %d, want %d", path, l, off, n, wantN)
			}
			if short {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want io.EOF (short read)", path, l, off, err)
				}
			} else if err != nil {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want nil (full read)", path, l, off, err)
			}
			if !bytes.Equal(p[:n], want[off:off+int64(n)]) {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) bytes differ from ReadFile[%d:%d]", path, l, off, off, off+int64(n))
			}
		}
	}

	// io.SectionReader is the consumer the io.ReaderAt contract protects;
	// running the whole file through it must reproduce ReadFile exactly.
	got, err := io.ReadAll(io.NewSectionReader(f, 0, size))
	if err != nil {
		t.Fatalf("%s: ReadAll(SectionReader): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: SectionReader round-trip differs from ReadFile", path)
	}
}

// TestOpenFileOnMkfsImage is the real-image proof. The fixture is a 32 MiB
// FAT32 volume produced by mtools `mformat` (see testdata/mkfs/EXPECTED.txt) —
// not by this package — so the geometry and the chains are somebody else's.
// The two files it ships are short, so the test then writes a MULTI-CLUSTER,
// deliberately FRAGMENTED file into that same real volume and checks that too:
// fragmentation is what makes the offset→cluster mapping non-trivial, and a
// contiguous file would hide a wrong mapping behind correct arithmetic.
func TestOpenFileOnMkfsImage(t *testing.T) {
	path := extractFixture(t)
	fsIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()

	clusterSize := int64(fsIfc.(*fat32FS).info.ClusterSize())
	if clusterSize <= 0 {
		t.Fatalf("cluster size = %d", clusterSize)
	}
	t.Logf("mtools fixture cluster size = %d bytes", clusterSize)

	for _, name := range []string{"/" + fixtureHelloName, "/" + fixtureNotesName} {
		checkAgainstReadFile(t, fsIfc, name, clusterSize)
	}

	// Force a fragmented chain: fill A, then B, free A, then write C larger
	// than A's hole. C takes A's freed clusters first and then continues past
	// B, so its chain jumps backwards-then-forwards across B's clusters.
	fill := func(n int, seed byte) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*7) ^ seed ^ byte(i>>8)
		}
		return b
	}
	if err := fsIfc.WriteFile("/A.BIN", fill(int(clusterSize)*20, 0xA1), 0o644); err != nil {
		t.Fatalf("WriteFile A: %v", err)
	}
	if err := fsIfc.WriteFile("/B.BIN", fill(int(clusterSize)*8, 0xB2), 0o644); err != nil {
		t.Fatalf("WriteFile B: %v", err)
	}
	if err := fsIfc.DeleteFile("/A.BIN"); err != nil {
		t.Fatalf("DeleteFile A: %v", err)
	}
	// Not a multiple of the cluster size, so the tail cluster is partial —
	// the case where a read past Size() inside a valid cluster must still
	// report io.EOF rather than hand back slack bytes.
	big := fill(int(clusterSize)*50+123, 0xC3)
	if err := fsIfc.WriteFile("/C.BIN", big, 0o644); err != nil {
		t.Fatalf("WriteFile C: %v", err)
	}

	// Prove the chain really is fragmented, otherwise the test below is weaker
	// than it claims to be.
	fs := fsIfc.(*fat32FS)
	entry, _, err := fs.resolvePath("/C.BIN")
	if err != nil {
		t.Fatalf("resolvePath C: %v", err)
	}
	clusters, _, err := fs.chainClusters(entry.cluster, uint64(entry.size))
	if err != nil {
		t.Fatalf("chainClusters C: %v", err)
	}
	discontinuities := 0
	for i := 1; i < len(clusters); i++ {
		if clusters[i] != clusters[i-1]+1 {
			discontinuities++
		}
	}
	if discontinuities == 0 {
		t.Fatalf("/C.BIN chain is contiguous (%d clusters) — the fragmentation setup did not take", len(clusters))
	}
	t.Logf("/C.BIN: %d clusters, %d discontinuities", len(clusters), discontinuities)

	checkAgainstReadFile(t, fsIfc, "/C.BIN", clusterSize)
	checkAgainstReadFile(t, fsIfc, "/B.BIN", clusterSize)
}

// TestOpenFileOnFormattedImage repeats the proof on a volume this package
// formats itself, whose clusters are 4 KiB rather than the fixture's, so the
// boundary arithmetic is exercised at a second geometry.
func TestOpenFileOnFormattedImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opener.img")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{Label: "OPENER"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()

	clusterSize := int64(fsIfc.(*fat32FS).info.ClusterSize())
	data := make([]byte, clusterSize*5+17)
	for i := range data {
		data[i] = byte(i%251) ^ 0x5A
	}
	if err := fsIfc.MkDir("/dir", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fsIfc.WriteFile("/dir/data.bin", data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	checkAgainstReadFile(t, fsIfc, "/dir/data.bin", clusterSize)

	// A file whose length is an exact multiple of the cluster size: the last
	// cluster is full, so the EOF must come from the chain ending rather than
	// from the size clamp inside the final cluster.
	exact := data[:clusterSize*2]
	if err := fsIfc.WriteFile("/exact.bin", exact, 0o644); err != nil {
		t.Fatalf("WriteFile exact: %v", err)
	}
	checkAgainstReadFile(t, fsIfc, "/exact.bin", clusterSize)
}

// TestOpenFileEOFSemantics pins the io.ReaderAt end-of-file rules that generic
// consumers depend on. A short read with a nil error is the failure mode that
// breaks io.SectionReader silently, so it is asserted explicitly here.
func TestOpenFileEOFSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eof.img")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()
	if err := fsIfc.WriteFile("/x.bin", []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := probeOpener(t, fsIfc).OpenFile("/x.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	if f.Size() != 10 {
		t.Fatalf("Size() = %d, want 10", f.Size())
	}

	p := make([]byte, 4)
	if n, err := f.ReadAt(p, 2); n != 4 || err != nil || string(p) != "2345" {
		t.Fatalf("ReadAt(4,2) = %d, %v, %q", n, err, p)
	}
	// Straddling the end: bytes delivered AND io.EOF.
	if n, err := f.ReadAt(p, 8); n != 2 || !errors.Is(err, io.EOF) || string(p[:n]) != "89" {
		t.Fatalf("ReadAt(4,8) = %d, %v, %q; want 2, io.EOF, \"89\"", n, err, p[:n])
	}
	// Exactly at and past Size(): 0, io.EOF.
	if n, err := f.ReadAt(p, 10); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt at Size() = %d, %v; want 0, io.EOF", n, err)
	}
	if n, err := f.ReadAt(p, 1<<40); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt far past Size() = %d, %v; want 0, io.EOF", n, err)
	}
	// A zero-length read inside the file is a full read: 0, nil.
	if n, err := f.ReadAt(nil, 3); n != 0 || err != nil {
		t.Fatalf("ReadAt(empty, 3) = %d, %v; want 0, nil", n, err)
	}
	// Negative offset is an error, never a panic or an out-of-range index.
	if n, err := f.ReadAt(p, -1); n != 0 || err == nil {
		t.Fatalf("ReadAt(-1) = %d, %v; want an error", n, err)
	}
}

// TestOpenFileEmptyFile covers the start == 0 chain: an empty file has no
// clusters at all, so Size is 0 and any read is immediately io.EOF.
func TestOpenFileEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.img")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()
	if err := fsIfc.WriteFile("/empty.bin", nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := probeOpener(t, fsIfc).OpenFile("/empty.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	if f.Size() != 0 {
		t.Fatalf("Size() = %d, want 0", f.Size())
	}
	if n, err := f.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt on empty = %d, %v; want 0, io.EOF", n, err)
	}
}

// TestOpenFileClose checks Close is idempotent and that a read after it fails
// loudly instead of quietly serving stale cluster numbers.
func TestOpenFileClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "close.img")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()
	if err := fsIfc.WriteFile("/x.bin", []byte("abc"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := probeOpener(t, fsIfc).OpenFile("/x.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n, err := f.ReadAt(make([]byte, 1), 0); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("ReadAt after Close = %d, %v; want 0, os.ErrClosed", n, err)
	}
}

// TestOpenFileRejects covers every refusal path: the root, a directory, and a
// path that does not resolve. Each must fail the same way ReadFile does.
func TestOpenFileRejects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reject.img")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()
	if err := fsIfc.MkDir("/sub", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	o := probeOpener(t, fsIfc)
	for _, tc := range []struct{ name, path string }{
		{"root", "/"},
		{"directory", "/sub"},
		{"missing", "/nope.bin"},
		{"relative", "nope.bin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if f, err := o.OpenFile(tc.path); err == nil {
				f.Close()
				t.Fatalf("OpenFile(%q) succeeded, want an error", tc.path)
			}
		})
	}
}

// TestOpenFileConcurrentReads exercises the concurrency guarantee io.ReaderAt
// makes and a mount depends on: many ReadAt calls in flight on one File, each
// independent of the others. Run under -race this is the only thing that would
// catch shared mutable state on the read path.
func TestOpenFileConcurrentReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conc.img")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()
	clusterSize := int64(fsIfc.(*fat32FS).info.ClusterSize())
	data := make([]byte, clusterSize*6+9)
	for i := range data {
		data[i] = byte(i * 31)
	}
	if err := fsIfc.WriteFile("/c.bin", data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := probeOpener(t, fsIfc).OpenFile("/c.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * (clusterSize / 3)
			if off > int64(len(data)) {
				off = int64(len(data))
			}
			l := int(clusterSize) + i
			p := make([]byte, l)
			n, err := f.ReadAt(p, off)
			if err != nil && !errors.Is(err, io.EOF) {
				errCh <- fmt.Errorf("goroutine %d: ReadAt: %w", i, err)
				return
			}
			if !bytes.Equal(p[:n], data[off:off+int64(n)]) {
				errCh <- fmt.Errorf("goroutine %d: bytes differ at off=%d", i, off)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// --- hardened-image paths -------------------------------------------------
//
// OpenFile walks the FAT with the same safeio guards ReadFile uses. These
// tests prove the guards are actually wired in on the new path rather than
// only on the old one — a forged image must not hang or over-allocate here
// either.

func TestOpenFileCyclicChain(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "CYCLE   BIN", 0x20, 3, 1<<20)
		root[32] = 0x00
	}, map[uint32]uint32{3: 4, 4: 3}, nil)
	fsIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	if _, err := probeOpener(t, fsIfc).OpenFile("/cycle.bin"); !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("OpenFile(cyclic) err = %v, want ErrCycle", err)
	}
}

func TestOpenFileLoopGuard(t *testing.T) {
	withTinyChainGuard(t, 2)
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "LONG    BIN", 0x20, 3, 1<<20)
		root[32] = 0x00
	}, map[uint32]uint32{3: 4, 4: 5, 5: 0x0FFFFFFF}, nil)
	fsIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	if _, err := probeOpener(t, fsIfc).OpenFile("/long.bin"); !errors.Is(err, safeio.ErrLoopLimit) {
		t.Fatalf("OpenFile(overlong) err = %v, want ErrLoopLimit", err)
	}
}

// TestOpenFileHugeFileSizeClamped covers the clamp: a directory entry claiming
// ~4 GB on a two-cluster chain must yield a File whose Size is what the chain
// really addresses — the same length ReadFile returns — not the forged number.
func TestOpenFileHugeFileSizeClamped(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "HUGE    BIN", 0x20, 3, 0xFFFFFFFF)
		root[32] = 0x00
	}, map[uint32]uint32{3: 4, 4: 0x0FFFFFFF}, nil)
	fsIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	want, err := fsIfc.ReadFile("/huge.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	f, err := probeOpener(t, fsIfc).OpenFile("/huge.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	if f.Size() != int64(len(want)) {
		t.Fatalf("Size() = %d, want %d (len of ReadFile)", f.Size(), len(want))
	}
	got := make([]byte, len(want))
	if n, err := f.ReadAt(got, 0); n != len(want) || err != nil {
		t.Fatalf("ReadAt(all) = %d, %v; want %d, nil", n, err, len(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatal("ReadAt bytes differ from ReadFile")
	}
}

// TestOpenFileClusterBelowTwo covers the `cluster < 2` termination: a chain
// pointing into the reserved FAT entries stops the walk, so the file reads as
// the clusters gathered so far.
func TestOpenFileClusterBelowTwo(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "STOP    BIN", 0x20, 3, 1<<20)
		root[32] = 0x00
	}, map[uint32]uint32{3: 1}, nil)
	fsIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fsIfc.Close()
	want, err := fsIfc.ReadFile("/stop.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	f, err := probeOpener(t, fsIfc).OpenFile("/stop.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	if f.Size() != int64(len(want)) {
		t.Fatalf("Size() = %d, want %d", f.Size(), len(want))
	}
}

// mockImageFS loads the image at path into a mockDisk so a read failure can be
// injected at an exact byte offset — the only way to cover the I/O error
// branches of chainClusters and ReadAt deterministically.
func mockImageFS(t *testing.T, path string, readErr func(int64) error) *fat32FS {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	info, err := readInfo(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	return newMockFSWithErrors(data, info, 0, readErr, nil)
}

// TestOpenFileFATReadError covers the FAT-entry read failure inside
// chainClusters: OpenFile must surface it rather than return a truncated File.
func TestOpenFileFATReadError(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "ERR     BIN", 0x20, 3, 4096)
		root[32] = 0x00
	}, map[uint32]uint32{3: 4, 4: 0x0FFFFFFF}, nil)
	fs := mockImageFS(t, path, nil)
	fatBase := fs.info.FATOffset(0) + 3*4
	fs.f.(*mockDisk).readErr = func(off int64) error {
		if off == fatBase {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	if _, err := fs.OpenFile("/err.bin"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("OpenFile with failing FAT read = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestOpenFileDataReadError covers the data read failure inside ReadAt: the
// error must come back with however many bytes were already delivered, never
// as a silent short read.
func TestOpenFileDataReadError(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "ERR     BIN", 0x20, 3, 8192)
		root[32] = 0x00
	}, map[uint32]uint32{3: 4, 4: 0x0FFFFFFF}, nil)
	fs := mockImageFS(t, path, nil)
	clusterSize := int64(fs.info.ClusterSize())
	// Fail on the second cluster of the chain, so the first one succeeds and
	// ReadAt returns a partial n together with the error.
	bad := fs.info.DataOffset(0) + int64(4-2)*clusterSize
	fs.f.(*mockDisk).readErr = func(off int64) error {
		if off == bad {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	f, err := fs.OpenFile("/err.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	p := make([]byte, 2*clusterSize)
	n, err := f.ReadAt(p, 0)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadAt err = %v, want io.ErrUnexpectedEOF", err)
	}
	if int64(n) != clusterSize {
		t.Fatalf("ReadAt n = %d, want %d (first cluster delivered)", n, clusterSize)
	}
}

// TestChainClustersEmpty covers the start == 0 short-circuit directly.
func TestChainClustersEmpty(t *testing.T) {
	path := fatTestImage(t, nil, nil, nil)
	fs := openTestFS(t, path)
	clusters, size, err := fs.chainClusters(0, 1234)
	if err != nil || clusters != nil || size != 0 {
		t.Fatalf("chainClusters(0) = %v, %d, %v; want nil, 0, nil", clusters, size, err)
	}
}

// TestChainClustersMatchesReadClusterChain is the invariant that keeps the two
// walks honest: for any chain, the clusters chainClusters collects must cover
// exactly as many bytes as readClusterChain returns. If one walk ever gains a
// termination condition the other lacks, ReadAt and ReadFile would disagree.
func TestChainClustersMatchesReadClusterChain(t *testing.T) {
	cases := []struct {
		name string
		fat  map[uint32]uint32
		size uint32
	}{
		{"single cluster", map[uint32]uint32{3: 0x0FFFFFFF}, 100},
		{"two clusters", map[uint32]uint32{3: 4, 4: 0x0FFFFFFF}, 6000},
		{"chain longer than size", map[uint32]uint32{3: 4, 4: 5, 5: 0x0FFFFFFF}, 10},
		{"size longer than chain", map[uint32]uint32{3: 0x0FFFFFFF}, 1 << 20},
		{"bad next cluster", map[uint32]uint32{3: 0x0FFFFFF7}, 1 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := fatTestImage(t, func(root []byte) {
				writeFAT32ShortEntrySized(root[0:32], "X       BIN", 0x20, 3, tc.size)
				root[32] = 0x00
			}, tc.fat, nil)
			fs := openTestFS(t, path)
			want, err := fs.readClusterChain(3, uint64(tc.size))
			if err != nil {
				t.Fatalf("readClusterChain: %v", err)
			}
			_, size, err := fs.chainClusters(3, uint64(tc.size))
			if err != nil {
				t.Fatalf("chainClusters: %v", err)
			}
			if size != int64(len(want)) {
				t.Fatalf("chainClusters size = %d, readClusterChain len = %d", size, len(want))
			}
		})
	}
}

// TestOpenFileFragmentedChainSpotCheck reads a fragmented file cluster by
// cluster and compares each cluster against the raw image bytes at the disk
// offset the FAT says it lives at. This is the check that catches an
// off-by-one in the offset→cluster index mapping directly, without going
// through ReadFile at all.
func TestOpenFileFragmentedChainSpotCheck(t *testing.T) {
	imgPath := filepath.Join(t.TempDir(), "frag.img")
	fsIfc, err := Format(imgPath, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsIfc.(*fat32FS)
	clusterSize := int64(fs.info.ClusterSize())
	fill := func(n int, seed byte) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*13) ^ seed
		}
		return b
	}
	if err := fsIfc.WriteFile("/pad.bin", fill(int(clusterSize)*6, 0x11), 0o644); err != nil {
		t.Fatalf("WriteFile pad: %v", err)
	}
	if err := fsIfc.WriteFile("/keep.bin", fill(int(clusterSize)*3, 0x22), 0o644); err != nil {
		t.Fatalf("WriteFile keep: %v", err)
	}
	if err := fsIfc.DeleteFile("/pad.bin"); err != nil {
		t.Fatalf("DeleteFile pad: %v", err)
	}
	body := fill(int(clusterSize)*12+5, 0x33)
	if err := fsIfc.WriteFile("/frag.bin", body, 0o644); err != nil {
		t.Fatalf("WriteFile frag: %v", err)
	}
	entry, _, err := fs.resolvePath("/frag.bin")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	clusters, size, err := fs.chainClusters(entry.cluster, uint64(entry.size))
	if err != nil {
		t.Fatalf("chainClusters: %v", err)
	}
	if err := fsIfc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}

	fsIfc2, err := Open(imgPath, -1)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer fsIfc2.Close()
	f, err := probeOpener(t, fsIfc2).OpenFile("/frag.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	dataBase := fs.info.DataOffset(0)
	for i, cluster := range clusters {
		off := int64(i) * clusterSize
		want := clusterSize
		if rem := size - off; want > rem {
			want = rem
		}
		diskOff := dataBase + int64(cluster-2)*clusterSize
		got := make([]byte, want)
		n, err := f.ReadAt(got, off)
		if int64(n) != want || (err != nil && !errors.Is(err, io.EOF)) {
			t.Fatalf("cluster %d: ReadAt = %d, %v", i, n, err)
		}
		if !bytes.Equal(got, raw[diskOff:diskOff+want]) {
			t.Fatalf("cluster index %d (cluster %d): ReadAt bytes != image bytes at offset %d", i, cluster, diskOff)
		}
	}
	// And the whole thing matches what was written in the first place.
	all := make([]byte, size)
	if n, err := f.ReadAt(all, 0); int64(n) != size || err != nil {
		t.Fatalf("ReadAt(all) = %d, %v", n, err)
	}
	if !bytes.Equal(all, body) {
		t.Fatal("ReadAt(all) differs from the bytes written")
	}
}
