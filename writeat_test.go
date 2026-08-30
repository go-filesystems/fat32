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
)

// probeWritable asserts the capability is reachable the way a caller reaches
// it — through the filesystem.File that OpenFile returns, not the concrete
// type — and hands back the WritableFile.
func probeWritable(t *testing.T, f filesystem.File) filesystem.WritableFile {
	t.Helper()
	w, ok := f.(filesystem.WritableFile)
	if !ok {
		t.Fatal("fat32's File does not satisfy filesystem.WritableFile")
	}
	return w
}

// pattern builds deterministic, position-dependent bytes. A constant fill
// would hide an off-by-one-cluster: every wrong byte would happen to be right.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) ^ seed ^ byte(i>>8)
	}
	return b
}

// readModifyWrite is the slow path this whole capability exists to replace:
// read the entire file, splice, write the entire file back. It is the ORACLE
// for every WriteAt below — the two must agree byte for byte, because a caller
// that falls back to it when a driver has no WritableFile must get the same
// filesystem either way.
func readModifyWrite(t *testing.T, fsIfc filesystem.Filesystem, path string, p []byte, off int64) {
	t.Helper()
	cur, err := fsIfc.ReadFile(path)
	if err != nil {
		t.Fatalf("oracle ReadFile(%s): %v", path, err)
	}
	if end := off + int64(len(p)); end > int64(len(cur)) {
		grown := make([]byte, end)
		copy(grown, cur)
		cur = grown
	}
	copy(cur[off:], p)
	if err := fsIfc.WriteFile(path, cur, 0o644); err != nil {
		t.Fatalf("oracle WriteFile(%s): %v", path, err)
	}
}

// checkReadPathsAgree reads the file back BOTH ways — ReadAt on a freshly
// opened File, and ReadFile — and requires them to be identical. The two use
// different code (a cluster walk with a materialising buffer, versus offset
// arithmetic over a resolved chain), so a write that updated one view and not
// the other is caught here and nowhere else.
func checkReadPathsAgree(t *testing.T, fsIfc filesystem.Filesystem, path string, want []byte) {
	t.Helper()
	viaReadFile, err := fsIfc.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if !bytes.Equal(viaReadFile, want) {
		t.Fatalf("%s: ReadFile gave %d bytes, want %d; first difference at %d",
			path, len(viaReadFile), len(want), firstDiff(viaReadFile, want))
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	defer f.Close()
	if got := f.Size(); got != int64(len(want)) {
		t.Fatalf("%s: Size() = %d, want %d", path, got, len(want))
	}
	viaReadAt := make([]byte, len(want))
	if len(want) > 0 {
		n, err := f.ReadAt(viaReadAt, 0)
		if n != len(want) || (err != nil && !errors.Is(err, io.EOF)) {
			t.Fatalf("%s: ReadAt(all) = %d, %v", path, n, err)
		}
	}
	if !bytes.Equal(viaReadAt, want) {
		t.Fatalf("%s: ReadAt disagrees with the expected content at byte %d",
			path, firstDiff(viaReadAt, want))
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// writeCase is one positional write to prove equivalent to read-modify-write.
type writeCase struct {
	name string
	off  func(clusterSize, size int64) int64
	n    func(clusterSize, size int64) int
}

// writeCases covers, on every geometry: writes wholly inside one cluster, a
// write straddling a cluster boundary, a write that extends the file, and a
// write landing in a HOLE past the end — the three cases where a positional
// write can differ from a whole-file rewrite, plus the one where it must not.
var writeCases = []writeCase{
	{"start", func(cs, sz int64) int64 { return 0 }, func(cs, sz int64) int { return 17 }},
	{"interior-within-cluster", func(cs, sz int64) int64 { return cs + 5 }, func(cs, sz int64) int { return int(cs) - 10 }},
	{"straddles-one-boundary", func(cs, sz int64) int64 { return cs - 3 }, func(cs, sz int64) int { return 9 }},
	{"straddles-many-boundaries", func(cs, sz int64) int64 { return cs/2 + 1 }, func(cs, sz int64) int { return int(3*cs) + 7 }},
	{"exactly-on-boundary", func(cs, sz int64) int64 { return 2 * cs }, func(cs, sz int64) int { return int(cs) }},
	{"last-byte", func(cs, sz int64) int64 { return sz - 1 }, func(cs, sz int64) int { return 1 }},
	{"extends-within-last-cluster", func(cs, sz int64) int64 { return sz }, func(cs, sz int64) int { return 3 }},
	{"extends-past-last-cluster", func(cs, sz int64) int64 { return sz - 2 }, func(cs, sz int64) int { return int(2*cs) + 11 }},
	{"hole-inside-last-cluster", func(cs, sz int64) int64 { return sz + 4 }, func(cs, sz int64) int { return 6 }},
	{"hole-spanning-whole-clusters", func(cs, sz int64) int64 { return sz + 3*cs + 9 }, func(cs, sz int64) int { return int(cs) + 1 }},
	{"whole-file-overwrite", func(cs, sz int64) int64 { return 0 }, func(cs, sz int64) int { return int(sz) }},
}

// runWriteCases is THE verification the capability has to survive: on a real
// image, for every offset shape above, WriteAt must produce exactly the same
// file as ReadFile + splice + WriteFile, and the result must read back the
// same through ReadAt and through ReadFile.
//
// Each case gets its own pair of freshly built images, so a case cannot be
// masked by the state a previous one left behind.
func runWriteCases(t *testing.T, mk func(t *testing.T) filesystem.Filesystem) {
	t.Helper()
	for _, tc := range writeCases {
		t.Run(tc.name, func(t *testing.T) {
			mine := mk(t)
			oracle := mk(t)
			clusterSize := int64(mine.(*fat32FS).info.ClusterSize())

			const path = "/DATA.BIN"
			initial := pattern(int(clusterSize)*6+37, 0x5A)
			for _, fsIfc := range []filesystem.Filesystem{mine, oracle} {
				if err := fsIfc.WriteFile(path, initial, 0o644); err != nil {
					t.Fatalf("seed WriteFile: %v", err)
				}
			}
			size := int64(len(initial))
			off := tc.off(clusterSize, size)
			data := pattern(tc.n(clusterSize, size), 0xC7)

			// The path under test.
			f, err := mine.(filesystem.Opener).OpenFile(path)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			w := probeWritable(t, f)
			n, err := w.WriteAt(data, off)
			if n != len(data) || err != nil {
				t.Fatalf("WriteAt(len=%d, off=%d) = %d, %v — io.WriterAt requires all of p or an error",
					len(data), off, n, err)
			}
			wantSize := max(size, off+int64(len(data)))
			if got := w.Size(); got != wantSize {
				t.Fatalf("Size() = %d after WriteAt, want %d — a WritableFile's Size must follow its own writes",
					got, wantSize)
			}
			if err := w.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// The oracle.
			readModifyWrite(t, oracle, path, data, off)
			want, err := oracle.ReadFile(path)
			if err != nil {
				t.Fatalf("oracle ReadFile: %v", err)
			}
			if int64(len(want)) != wantSize {
				t.Fatalf("oracle produced %d bytes, want %d", len(want), wantSize)
			}

			// They must agree, and both read paths must agree with them.
			checkReadPathsAgree(t, mine, path, want)

			// A Stat through the Filesystem must see the new length too: FAT
			// keeps it in the directory entry, so a WriteAt that forgot to
			// rewrite the entry would pass every check above and fail here.
			st, err := mine.Stat(path)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if int64(st.Size()) != wantSize {
				t.Fatalf("Stat().Size() = %d, want %d", st.Size(), wantSize)
			}
		})
	}
}

// TestWriteAtOnMtoolsImage runs the whole sweep on the image mtools' mformat
// produced — a geometry this package did not choose — so the offset arithmetic
// is proven against a foreign layout rather than against our own assumptions.
func TestWriteAtOnMtoolsImage(t *testing.T) {
	runWriteCases(t, func(t *testing.T) filesystem.Filesystem {
		fsIfc, err := Open(extractFixture(t), -1)
		if err != nil {
			t.Fatalf("Open fixture: %v", err)
		}
		t.Cleanup(func() { _ = fsIfc.Close() })
		return fsIfc
	})
}

// TestWriteAtOnFormattedImage repeats the sweep at a second geometry — 4 KiB
// clusters from this package's own Format — because a boundary bug can hide at
// one cluster size and not another.
func TestWriteAtOnFormattedImage(t *testing.T) {
	runWriteCases(t, func(t *testing.T) filesystem.Filesystem {
		path := filepath.Join(t.TempDir(), "writeat.img")
		fsIfc, err := Format(path, 16*1024*1024, FormatConfig{Label: "WRITEAT"})
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		t.Cleanup(func() { _ = fsIfc.Close() })
		return fsIfc
	})
}

// TestWriteAtSequentialMatchesWholeFile is the shape the NFS server produces
// and the reason this capability exists: a file written from offset zero in
// fixed-size blocks. Before WriteAt each block cost a full read-modify-write,
// so the total was quadratic; here it must still land byte-for-byte identical
// to a single whole-file write of the same bytes.
func TestWriteAtSequentialMatchesWholeFile(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string) filesystem.Filesystem {
		fsIfc, err := Format(filepath.Join(dir, name), 16*1024*1024, FormatConfig{})
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		t.Cleanup(func() { _ = fsIfc.Close() })
		return fsIfc
	}
	const path = "/SEQ.BIN"
	const block = 32 * 1024
	whole := pattern(block*13+555, 0x3C)

	incremental := mk("incremental.img")
	if err := incremental.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	f, err := incremental.(filesystem.Opener).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	for off := 0; off < len(whole); off += block {
		end := min(off+block, len(whole))
		if n, err := w.WriteAt(whole[off:end], int64(off)); n != end-off || err != nil {
			t.Fatalf("WriteAt(off=%d) = %d, %v", off, n, err)
		}
		// The client's next GETATTR must already see the file grow, which is
		// what makes an appending stream over NFS behave.
		if got := w.Size(); got != int64(end) {
			t.Fatalf("Size() = %d after writing through %d, want %d", got, end, end)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkReadPathsAgree(t, incremental, path, whole)

	atOnce := mk("atonce.img")
	if err := atOnce.WriteFile(path, whole, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want, err := atOnce.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got, err := incremental.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("block-by-block WriteAt differs from a single WriteFile at byte %d", firstDiff(got, want))
	}
}

// TestWriteAtIntoFragmentedChain proves the offset→cluster mapping on a chain
// that is NOT contiguous. A contiguous layout would let a wrong mapping (say,
// "first cluster plus index") pass every other test in this file.
func TestWriteAtIntoFragmentedChain(t *testing.T) {
	fsIfc, err := Open(extractFixture(t), -1)
	if err != nil {
		t.Fatalf("Open fixture: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*fat32FS)
	cs := int(fs.info.ClusterSize())

	// Fill A, then B, free A, then write C larger than A's hole: C takes A's
	// freed clusters and continues past B, so its chain jumps around B's.
	if err := fsIfc.WriteFile("/A.BIN", pattern(cs*20, 0xA1), 0o644); err != nil {
		t.Fatalf("WriteFile A: %v", err)
	}
	if err := fsIfc.WriteFile("/B.BIN", pattern(cs*8, 0xB2), 0o644); err != nil {
		t.Fatalf("WriteFile B: %v", err)
	}
	if err := fsIfc.DeleteFile("/A.BIN"); err != nil {
		t.Fatalf("DeleteFile A: %v", err)
	}
	want := pattern(cs*50+123, 0xC3)
	if err := fsIfc.WriteFile("/C.BIN", want, 0o644); err != nil {
		t.Fatalf("WriteFile C: %v", err)
	}

	entry, _, err := fs.resolvePath("/C.BIN")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	clusters, _, err := fs.chainClusters(entry.cluster, uint64(entry.size))
	if err != nil {
		t.Fatalf("chainClusters: %v", err)
	}
	disc := 0
	for i := 1; i < len(clusters); i++ {
		if clusters[i] != clusters[i-1]+1 {
			disc++
		}
	}
	if disc == 0 {
		t.Fatalf("/C.BIN chain is contiguous (%d clusters) — the fragmentation setup did not take", len(clusters))
	}
	t.Logf("/C.BIN: %d clusters, %d discontinuities", len(clusters), disc)

	f, err := fsIfc.(filesystem.Opener).OpenFile("/C.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	// Write across every discontinuity in the chain: at each jump, a span
	// straddling the boundary between the two non-adjacent clusters.
	for i := 1; i < len(clusters); i++ {
		if clusters[i] == clusters[i-1]+1 {
			continue
		}
		off := int64(i*cs) - 5
		p := pattern(11, byte(i))
		if n, err := w.WriteAt(p, off); n != len(p) || err != nil {
			t.Fatalf("WriteAt across discontinuity at cluster index %d: %d, %v", i, n, err)
		}
		copy(want[off:], p)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkReadPathsAgree(t, fsIfc, "/C.BIN", want)
}

// TestWriteAtHoleReadsAsZeros pins the rule a caller cannot check any other
// way: bytes never written, between the old end of file and a write past it,
// must read back as zeros through BOTH read paths — not as whatever the
// clusters held when some earlier, deleted file owned them.
func TestWriteAtHoleReadsAsZeros(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hole.img")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()
	cs := int(fsIfc.(*fat32FS).info.ClusterSize())

	// Dirty the data region first: write a file of 0xFF, delete it, so the
	// clusters a later grow allocates are certainly not already zero.
	dirty := bytes.Repeat([]byte{0xFF}, cs*12)
	if err := fsIfc.WriteFile("/DIRTY.BIN", dirty, 0o644); err != nil {
		t.Fatalf("WriteFile dirty: %v", err)
	}
	if err := fsIfc.DeleteFile("/DIRTY.BIN"); err != nil {
		t.Fatalf("DeleteFile dirty: %v", err)
	}

	head := pattern(10, 0x11)
	if err := fsIfc.WriteFile("/SPARSE.BIN", head, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile("/SPARSE.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	tail := pattern(7, 0x22)
	holeAt := int64(cs*5 + 3)
	if _, err := w.WriteAt(tail, holeAt); err != nil {
		t.Fatalf("WriteAt past end: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := make([]byte, int(holeAt)+len(tail))
	copy(want, head)
	copy(want[holeAt:], tail)
	checkReadPathsAgree(t, fsIfc, "/SPARSE.BIN", want)
}

// TestTruncateFile exercises the file-scoped Truncate in both directions and
// checks it against the path-scoped one on an identical image: the two must
// leave the same file, since a caller may reach either.
func TestTruncateFile(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string) filesystem.Filesystem {
		fsIfc, err := Format(filepath.Join(dir, name), 16*1024*1024, FormatConfig{})
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		t.Cleanup(func() { _ = fsIfc.Close() })
		return fsIfc
	}
	const path = "/T.BIN"

	for _, size := range []int64{0, 1, 4095, 4096, 4097, 20000, 40000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			mine, oracle := mk(fmt.Sprintf("m%d.img", size)), mk(fmt.Sprintf("o%d.img", size))
			initial := pattern(9000, 0x77)
			for _, fsIfc := range []filesystem.Filesystem{mine, oracle} {
				if err := fsIfc.WriteFile(path, initial, 0o644); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			f, err := mine.(filesystem.Opener).OpenFile(path)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			w := probeWritable(t, f)
			if err := w.Truncate(size); err != nil {
				t.Fatalf("Truncate(%d): %v", size, err)
			}
			if got := w.Size(); got != size {
				t.Fatalf("Size() = %d after Truncate(%d)", got, size)
			}
			// Truncating to the size it already has must be a no-op that
			// still succeeds — the early return.
			if err := w.Truncate(size); err != nil {
				t.Fatalf("Truncate(%d) second time: %v", size, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			if err := oracle.(filesystem.Truncater).Truncate(path, size); err != nil {
				t.Fatalf("path-scoped Truncate(%d): %v", size, err)
			}
			want, err := oracle.ReadFile(path)
			if err != nil {
				t.Fatalf("oracle ReadFile: %v", err)
			}
			checkReadPathsAgree(t, mine, path, want)
		})
	}
}

// TestTruncateThenWriteReadsZeros: growing by Truncate must zero-fill, and the
// grown region must still be zeros after a later write elsewhere.
func TestTruncateGrowZeroFills(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tg.img")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()

	// Dirty the region a grow will claim.
	if err := fsIfc.WriteFile("/D.BIN", bytes.Repeat([]byte{0xEE}, 40000), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fsIfc.DeleteFile("/D.BIN"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	head := pattern(100, 0x01)
	if err := fsIfc.WriteFile("/G.BIN", head, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile("/G.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	if err := w.Truncate(30000); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := make([]byte, 30000)
	copy(want, head)
	checkReadPathsAgree(t, fsIfc, "/G.BIN", want)
}

// TestWriteAtConcurrentDisjointRanges: io.WriterAt permits parallel writes to
// non-overlapping ranges, and a mount issues exactly that. Under -race this
// also proves the File's own state is not torn by a concurrent extend.
func TestWriteAtConcurrentDisjointRanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conc.img")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()

	const n, chunk = 32, 4096
	want := pattern(n*chunk, 0x9E)
	if err := fsIfc.WriteFile("/C.BIN", make([]byte, n*chunk), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile("/C.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			off := int64(i * chunk)
			_, errs[i] = w.WriteAt(want[off:off+chunk], off)
		}()
	}
	// Reads run alongside: io.ReaderAt calls stay parallel-safe throughout.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.ReadAt(make([]byte, 512), 0)
			_ = w.Size()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent WriteAt %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkReadPathsAgree(t, fsIfc, "/C.BIN", want)
}

// --- contract and error branches -----------------------------------------

// openWritable formats a small image, creates a file, and returns it opened.
func openWritable(t *testing.T, initial []byte) (*fat32FS, filesystem.WritableFile) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "w.img")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	t.Cleanup(func() { _ = fsIfc.Close() })
	if err := fsIfc.WriteFile("/W.BIN", initial, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile("/W.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return fsIfc.(*fat32FS), probeWritable(t, f)
}

func TestWriteAtContractEdges(t *testing.T) {
	_, w := openWritable(t, pattern(100, 3))

	// An empty write is a no-op that succeeds: io.WriterAt says nothing else,
	// and refusing it would break callers that pass a zero-length buffer.
	if n, err := w.WriteAt(nil, 0); n != 0 || err != nil {
		t.Fatalf("WriteAt(empty) = %d, %v, want 0, nil", n, err)
	}
	// A negative offset is an error, never a panic and never a write.
	if n, err := w.WriteAt([]byte("x"), -1); n != 0 || err == nil {
		t.Fatalf("WriteAt(-1) = %d, %v, want an error", n, err)
	}
	// FAT32 records a file's length in 32 bits: a write past 4 GiB − 1 cannot
	// be recorded, so it is refused rather than wrapping to a small size.
	if _, err := w.WriteAt([]byte("x"), maxFAT32FileSize); err == nil {
		t.Fatal("WriteAt at the 4 GiB limit returned nil, want an error")
	}
	// ...including when the addition itself would overflow int64.
	if _, err := w.WriteAt(make([]byte, 8), 1<<62); err == nil {
		t.Fatal("WriteAt with an overflowing end returned nil, want an error")
	}
	if err := w.Truncate(-1); err == nil {
		t.Fatal("Truncate(-1) returned nil, want an error")
	}
	if err := w.Truncate(maxFAT32FileSize + 1); err == nil {
		t.Fatal("Truncate past the FAT32 limit returned nil, want an error")
	}
}

func TestWriteAfterCloseIsRefused(t *testing.T) {
	_, w := openWritable(t, pattern(100, 4))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent, as the read path documents.
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := w.WriteAt([]byte("x"), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("WriteAt after Close = %v, want os.ErrClosed", err)
	}
	if err := w.Truncate(0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Truncate after Close = %v, want os.ErrClosed", err)
	}
	if err := w.Sync(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Sync after Close = %v, want os.ErrClosed", err)
	}
}

// syncRecorder is a diskRW that HAS a Sync, so the forwarding branch is
// exercised in both outcomes.
type syncRecorder struct {
	diskRW
	calls int
	err   error
}

func (s *syncRecorder) Sync() error { s.calls++; return s.err }

func TestSyncForwardsToTheBackingHandle(t *testing.T) {
	// The normal case: Open uses os.OpenFile, whose *os.File has Sync, so the
	// real image path really does reach fsync(2).
	fs, w := openWritable(t, pattern(10, 5))
	if _, ok := fs.f.(interface{ Sync() error }); !ok {
		t.Fatal("a file-backed image's diskRW has no Sync — the FILE_SYNC promise would be empty")
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	rec := &syncRecorder{diskRW: fs.f}
	fs.f = rec
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("Sync forwarded %d times, want 1", rec.calls)
	}
	// A failing fsync must be reported, not swallowed: a server that answered
	// FILE_SYNC on it would be lying about durability.
	rec.err = errInjected
	if err := w.Sync(); !errors.Is(err, errInjected) {
		t.Fatalf("Sync = %v, want the backing error", err)
	}
}

// noSyncRW models a backing store with no Sync at all — a memory image, or a
// caller's own io.WriterAt. There is nothing to flush, so Sync must succeed
// rather than invent a failure.
type noSyncRW struct {
	r io.ReaderAt
	w io.WriterAt
}

func (n noSyncRW) ReadAt(p []byte, off int64) (int, error)  { return n.r.ReadAt(p, off) }
func (n noSyncRW) WriteAt(p []byte, off int64) (int, error) { return n.w.WriteAt(p, off) }
func (n noSyncRW) Close() error                             { return nil }

func TestSyncWithoutABackingSync(t *testing.T) {
	fs, w := openWritable(t, pattern(10, 6))
	inner := fs.f
	fs.f = noSyncRW{r: inner, w: inner}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync with no backing Sync = %v, want nil", err)
	}
}

func TestWriteAtIOErrors(t *testing.T) {
	// A data-region write failure is reported with the bytes that did land.
	fs, w := openWritable(t, pattern(4096*3, 7))
	fault := wrapFault(fs)
	fault.failDataWrites(fs)
	if _, err := w.WriteAt(pattern(16, 8), 0); !errors.Is(err, errInjected) {
		t.Fatalf("WriteAt with a failing data write = %v, want the injected error", err)
	}

	// A FAT read failure during the allocation scan for a growing write.
	fs2, w2 := openWritable(t, pattern(100, 9))
	f2 := wrapFault(fs2)
	f2.failFATReads(fs2)
	if _, err := w2.WriteAt(pattern(8, 10), 40000); !errors.Is(err, errInjected) {
		t.Fatalf("WriteAt with a failing FAT read = %v, want the injected error", err)
	}

	// A FAT write failure while claiming the run: the clusters must be given
	// back, so the rollback runs.
	fs3, w3 := openWritable(t, pattern(100, 11))
	f3 := wrapFault(fs3)
	f3.failFATWrites(fs3)
	if _, err := w3.WriteAt(pattern(8, 12), 40000); !errors.Is(err, errInjected) {
		t.Fatalf("WriteAt with a failing FAT write = %v, want the injected error", err)
	}

	// A data write failure while zero-filling a freshly allocated cluster.
	fs4, w4 := openWritable(t, pattern(100, 13))
	f4 := wrapFault(fs4)
	f4.failWriteLo = fs4.info.DataOffset(fs4.partOffset)
	f4.failWriteHi = 1 << 62
	f4.writeRegionSkip = 0
	if _, err := w4.WriteAt(pattern(8, 14), 40000); !errors.Is(err, errInjected) {
		t.Fatalf("WriteAt with a failing cluster zero-fill = %v, want the injected error", err)
	}
}

func TestTruncateIOErrors(t *testing.T) {
	// Shrink with the FAT unwritable: terminating the retained chain fails.
	fs, w := openWritable(t, pattern(4096*6, 15))
	fault := wrapFault(fs)
	fault.failFATWrites(fs)
	if err := w.Truncate(4096); !errors.Is(err, errInjected) {
		t.Fatalf("Truncate(shrink) with a failing FAT write = %v", err)
	}

	// Shrink to a size whose last cluster has slack, with the data region
	// unwritable: zeroing the slack fails.
	fs2, w2 := openWritable(t, pattern(4096*6, 16))
	f2 := wrapFault(fs2)
	f2.failDataWrites(fs2)
	if err := w2.Truncate(4096 + 7); !errors.Is(err, errInjected) {
		t.Fatalf("Truncate(shrink, slack) with a failing data write = %v", err)
	}

	// Grow within the last cluster, with the data region unwritable: zeroing
	// the OLD cluster's slack fails.
	fs3, w3 := openWritable(t, pattern(100, 17))
	f3 := wrapFault(fs3)
	f3.failDataWrites(fs3)
	if err := w3.Truncate(200); !errors.Is(err, errInjected) {
		t.Fatalf("Truncate(grow in place) with a failing data write = %v", err)
	}

	// Freeing the dropped clusters fails after the retained chain has been
	// terminated: the first FAT write succeeds, the second does not.
	fs4, w4 := openWritable(t, pattern(4096*6, 18))
	f4 := wrapFault(fs4)
	f4.failFATWrites(fs4)
	f4.writeRegionSkip = 1
	if err := w4.Truncate(4096); !errors.Is(err, errInjected) {
		t.Fatalf("Truncate(shrink) with the free pass failing = %v", err)
	}
}

// TestShrinkToEmptyAndBackFromEmpty covers the two chain-boundary cases: a
// file with no first cluster at all, and one grown from that state.
func TestShrinkToEmptyAndBackFromEmpty(t *testing.T) {
	fsRaw, w := openWritable(t, pattern(9000, 19))
	if err := w.Truncate(0); err != nil {
		t.Fatalf("Truncate(0): %v", err)
	}
	if w.Size() != 0 {
		t.Fatalf("Size() = %d after Truncate(0)", w.Size())
	}
	// FAT spells "empty" as first-cluster zero; a file left pointing at a
	// freed cluster would be corruption a later ReadFile would surface.
	entry, _, err := fsRaw.resolvePath("/W.BIN")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if entry.cluster != 0 {
		t.Fatalf("emptied file still points at cluster %d, want 0", entry.cluster)
	}
	// ...and growing from empty must give it a head again.
	back := pattern(5000, 20)
	if _, err := w.WriteAt(back, 0); err != nil {
		t.Fatalf("WriteAt from empty: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkReadPathsAgree(t, fsRaw, "/W.BIN", back)
}

// TestWriteAtWhenTheDirectoryEntryIsGone covers the failure a positional write
// cannot avoid: FAT keeps the length in the directory, so if the entry has
// been removed underneath the File, the size cannot be recorded.
func TestWriteAtWhenTheDirectoryEntryIsGone(t *testing.T) {
	fs, w := openWritable(t, pattern(100, 21))
	if err := fs.DeleteFile("/W.BIN"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := w.WriteAt(pattern(8, 22), 40000); err == nil {
		t.Fatal("WriteAt after the entry was deleted returned nil, want an error")
	}
}

// TestSetDirEntryExtentRejectsARootPath: "/" has no directory entry of its
// own, so the helper must refuse it rather than patch something else.
func TestSetDirEntryExtentRejectsARootPath(t *testing.T) {
	fs, _ := openWritable(t, pattern(10, 23))
	if err := fs.setDirEntryExtent("/", 2, 10); err == nil {
		t.Fatal("setDirEntryExtent(\"/\") returned nil, want an error")
	}
	if err := fs.setDirEntryExtent("no-leading-slash", 2, 10); err == nil {
		t.Fatal("setDirEntryExtent on a relative path returned nil, want an error")
	}
	if err := fs.setDirEntryExtent("/NOPE.BIN", 2, 10); err == nil {
		t.Fatal("setDirEntryExtent on a missing entry returned nil, want an error")
	}
	// A directory read failure on the way to the entry.
	fault := wrapFault(fs)
	fault.failReadLo = 0
	fault.failReadHi = 1 << 62
	if err := fs.setDirEntryExtent("/W.BIN", 2, 10); !errors.Is(err, errInjected) {
		t.Fatalf("setDirEntryExtent with unreadable directory = %v", err)
	}
}

func TestAllocClusterRunEdges(t *testing.T) {
	fs, _ := openWritable(t, pattern(10, 24))
	// Asking for nothing is not an error; it is the loop bound at zero.
	run, err := fs.allocClusterRun(0)
	if run != nil || err != nil {
		t.Fatalf("allocClusterRun(0) = %v, %v, want nil, nil", run, err)
	}
	// A run larger than the whole volume can supply must fail, not return
	// short: a short run would be linked into a file that then claims bytes
	// no cluster holds.
	if _, err := fs.allocClusterRun(1 << 30); err == nil {
		t.Fatal("allocClusterRun beyond the volume returned nil, want an error")
	}
	// A run spanning more than one page of FAT, to cross the read boundary.
	big, err := fs.allocClusterRun(fatEntriesPerScanRead + 5)
	if err != nil {
		t.Fatalf("allocClusterRun across a page boundary: %v", err)
	}
	if len(big) != fatEntriesPerScanRead+5 {
		t.Fatalf("allocClusterRun returned %d clusters", len(big))
	}
	for i := 1; i < len(big); i++ {
		if big[i] <= big[i-1] {
			t.Fatalf("allocClusterRun returned an out-of-order run at %d", i)
		}
	}
}

// TestWriteAtOnAFileWithNoFreeSpace: a grow that cannot be satisfied must fail
// the write outright rather than write part of it.
func TestWriteAtNoFreeClusters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "full.img")
	// The smallest image Format accepts, so filling it is cheap.
	fsIfc, err := Format(path, 2*1024*1024, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*fat32FS)
	if err := fsIfc.WriteFile("/S.BIN", pattern(100, 25), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fsIfc.(filesystem.Opener).OpenFile("/S.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w := probeWritable(t, f)
	// Claim every remaining cluster, in the largest runs the volume will
	// give, until none is left. Only then does a grow have nowhere to go.
	claimed := 0
	for n := int(fs.info.TotalSectors/uint32(fs.info.SectorsPerCluster)) + 2; n > 0; n /= 2 {
		for {
			run, err := fs.allocClusterRun(n)
			if err != nil {
				break
			}
			for _, c := range run {
				if err := fs.setFATEntry(c, 0x0FFFFFFF); err != nil {
					t.Fatalf("setFATEntry: %v", err)
				}
			}
			claimed += len(run)
		}
	}
	if claimed == 0 {
		t.Fatal("claimed no clusters — the volume was already full, so the test proves nothing")
	}
	t.Logf("claimed %d clusters to fill the volume", claimed)
	if _, err := w.WriteAt(pattern(8, 26), 1_500_000); err == nil {
		t.Fatal("WriteAt on a full volume returned nil, want an error")
	}
}

// TestGrowLinkFailures drives the two failure points that only exist once the
// new clusters have been claimed and zeroed: linking the run to itself, and
// linking it onto the file's existing tail. Both must roll back, so a failed
// grow does not leave the file pointing at a half-linked chain.
func TestGrowLinkFailures(t *testing.T) {
	// The run is claimed with one FAT write per cluster, then linked with
	// one per internal edge, then one for the tail: letting exactly the
	// claims through puts the fault on the first internal link.
	t.Run("internal-link", func(t *testing.T) {
		fs, w := openWritable(t, pattern(100, 27))
		cs := int64(fs.info.ClusterSize())
		fault := wrapFault(fs)
		fault.failFATWrites(fs)
		fault.writeRegionSkip = 3 // three claims succeed
		if err := w.Truncate(100 + 3*cs); !errors.Is(err, errInjected) {
			t.Fatalf("grow with a failing internal link = %v, want the injected error", err)
		}
	})
	// A one-cluster run has no internal edges, so the second FAT write is
	// the link onto the existing tail.
	t.Run("tail-link", func(t *testing.T) {
		fs, w := openWritable(t, pattern(100, 28))
		cs := int64(fs.info.ClusterSize())
		fault := wrapFault(fs)
		fault.failFATWrites(fs)
		fault.writeRegionSkip = 1 // the single claim succeeds
		if err := w.Truncate(cs + 1); !errors.Is(err, errInjected) {
			t.Fatalf("grow with a failing tail link = %v, want the injected error", err)
		}
	})
}
