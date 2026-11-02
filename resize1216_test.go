package filesystem_fat32

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Resizing FAT12 and FAT16. Every scenario runs on the newfs_msdos fixtures,
// is read back file by file against the manifest's SHA-256, and is judged by
// fsck_msdos -n / fsck.vfat -n when present (see settle).

// sizeForCount is the image size, in bytes, of a volume with count data
// clusters and the FAT of fs.
func sizeForCount(fs *fat32FS, count int64) int64 {
	dataStart := int64(fs.info.ReservedSectors) + int64(fs.info.FATCount)*int64(fs.info.FATSize) + int64(fs.info.RootDirSectors())
	return (dataStart + count*int64(fs.info.SectorsPerCluster)) * int64(fs.info.BytesPerSector)
}

// highestUsed is the highest cluster number the FAT says is in use.
func highestUsed(t testing.TB, fs *fat32FS) uint32 {
	t.Helper()
	hi := uint32(0)
	for c := uint32(2); c < fs.lastCluster(); c++ {
		v, err := fs.fatEntry(c)
		if err != nil {
			t.Fatal(err)
		}
		if v != 0 {
			hi = c
		}
	}
	return hi
}

// filesIntact reads every file of the manifest back from the open volume.
func filesIntact(t testing.TB, im f1216Image, fs *fat32FS) {
	t.Helper()
	for _, f := range im.Files {
		data, err := fs.ReadFile(f.Path)
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		if len(data) != f.Size || f1216Sum(data) != f.SHA256 {
			t.Fatalf("%s changed by the resize", f.Path)
		}
	}
}

// settleResized judges a resized volume: structural check, files, size on disk
// equal to the BPB, FAT copies equal, fsck clean. It returns the reopened fs.
func settleResized(t *testing.T, im f1216Image, path string, fs *fat32FS, wantType FATType, wantClusters int64) *fat32FS {
	t.Helper()
	if fs.info.Type != wantType || int64(fs.info.DataClusters) != wantClusters {
		t.Fatalf("after the resize: %v with %d clusters, want %v with %d", fs.info.Type, fs.info.DataClusters, wantType, wantClusters)
	}
	filesIntact(t, im, fs)
	settle(t, path, fs)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	re, err := Open(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	r := re.(*fat32FS)
	t.Cleanup(func() { r.Close() })
	if st.Size() < int64(r.info.TotalSectors)*int64(r.info.BytesPerSector) {
		t.Fatalf("file of %d bytes is shorter than the %d sectors the BPB claims", st.Size(), r.info.TotalSectors)
	}
	if r.info.Type != wantType || int64(r.info.DataClusters) != wantClusters {
		t.Fatalf("reopened as %v with %d clusters, want %v with %d", r.info.Type, r.info.DataClusters, wantType, wantClusters)
	}
	filesIntact(t, im, r)
	return r
}

// Shrink then grow back, on both types, with a write in between that lands in
// the clusters the grow exposes. The grow stops exactly at what the FAT holds,
// and one cluster more is refused with ErrResizeFATFull.
func TestFAT1216ShrinkGrow(t *testing.T) {
	for _, typ := range []int{12, 16} {
		t.Run(fmt.Sprintf("FAT%d", typ), func(t *testing.T) {
			im, _ := f1216Fixture(t, typ)
			path, fs := f1216Writable(t, typ)
			orig := int64(fs.info.DataClusters)
			origSize := sizeForCount(fs, orig)
			hi := int64(highestUsed(t, fs))
			shrunk := hi - 1 + 9 // 9 free clusters after the last one in use
			if typ == 16 {
				shrunk = 5000 // stays above 4084
			}
			fat := fs.info.Type

			// Shrink. Size on disk and in the BPB follow; the FAT keeps its size.
			fatSize := fs.info.FATSize
			if err := fs.Shrink(sizeForCount(fs, shrunk)); err != nil {
				t.Fatal(err)
			}
			if fs.info.FATSize != fatSize {
				t.Fatal("Shrink changed the FAT size")
			}
			fs = settleResized(t, im, path, fs, fat, shrunk)
			// Grow back to the original size (the FAT has room: it held it).
			if err := fs.Resize(origSize); err != nil {
				t.Fatal(err)
			}
			// The grown volume is writable end to end, into the new clusters.
			data := pat(int(orig-shrunk-2)*int(fs.info.ClusterSize()), 3)
			if err := fs.WriteFile("/DIR1/GROWN.BIN", data, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := fs.ReadFile("/DIR1/GROWN.BIN")
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("file written into the grown volume: %v", err)
			}
			if err := fs.DeleteFile("/DIR1/GROWN.BIN"); err != nil {
				t.Fatal(err)
			}
			fs = settleResized(t, im, path, fs, fat, orig)
			if st, _ := os.Stat(path); st.Size() != origSize {
				t.Fatalf("size on disk %d, want %d", st.Size(), origSize)
			}

			// Full FAT: the next cluster would need a bigger FAT.
			_, capacity := fs.narrowGeometry(int64(fs.info.TotalSectors))
			if err := fs.Grow(sizeForCount(fs, capacity)); err != nil {
				t.Fatal(err)
			}
			fs = settleResized(t, im, path, fs, fat, capacity)
			if err := fs.Grow(sizeForCount(fs, capacity+1)); !errors.Is(err, ErrResizeFATFull) || !errors.Is(err, ErrResizeFAT) {
				t.Fatalf("Grow past the FAT = %v, want ErrResizeFATFull", err)
			}
		})
	}
}

// A grow exposes only free clusters, whatever junk another tool left in the
// FAT slack: odd and even first new cluster on FAT12, and FAT16.
func TestFAT1216GrowClearsFATSlack(t *testing.T) {
	for _, typ := range []int{12, 16} {
		for _, skip := range []int64{0, 1} { // shifts the parity of the first new cluster
			t.Run(fmt.Sprintf("FAT%d+%d", typ, skip), func(t *testing.T) {
				im, _ := f1216Fixture(t, typ)
				path, fs := f1216Writable(t, typ)
				orig := int64(fs.info.DataClusters)
				fatBytes := int64(fs.info.FATSize) * int64(fs.info.BytesPerSector)
				if err := fs.Shrink(sizeForCount(fs, orig-skip-4)); err != nil {
					t.Fatal(err)
				}
				// Junk over the whole slack of every copy, then repair the nibble
				// FAT12 shares with the last live entry (that one is ours).
				for i := int64(0); i < int64(fs.info.FATCount); i++ {
					start := int64(fs.lastCluster())
					if typ == 16 {
						start *= 2
					} else {
						start += start / 2
					}
					junk := bytes.Repeat([]byte{0xAA}, int(fatBytes-start))
					if typ == 12 && fs.lastCluster()&1 == 1 {
						// keep the live entry's nibble: rewrite from the next byte
						start++
						junk = junk[1:]
					}
					if _, err := fs.f.WriteAt(junk, fs.info.FATOffset(0)+i*fatBytes+start); err != nil {
						t.Fatal(err)
					}
				}
				if err := fs.Grow(sizeForCount(fs, orig)); err != nil {
					t.Fatal(err)
				}
				for c := uint32(2); c < fs.lastCluster(); c++ {
					v, _ := fs.fatEntry(c)
					if c >= uint32(orig-skip-4)+2 && v != 0 {
						t.Fatalf("cluster %d is not free after the grow: 0x%x", c, v)
					}
				}
				settleResized(t, im, path, fs, fs.info.Type, orig)
			})
		}
	}
}

// The type never changes: both types name themselves, and the image is left
// untouched.
func TestFAT1216ResizeRefusesTypeChange(t *testing.T) {
	cases := []struct {
		typ   int
		grow  bool
		count int64
		want  string
	}{
		{12, true, 4085, "growing to 4085 clusters would make this FAT12 volume FAT16; copy it into a new image of that size with CopyResized"},
		{16, false, 4084, "shrinking to 4084 clusters would make this FAT16 volume FAT12; copy it into a new image of that size with CopyResized"},
		{16, true, 65525, "growing to 65525 clusters would make this FAT16 volume FAT32; copy it into a new image of that size with CopyResized"},
	}
	for _, c := range cases {
		path, fs := f1216Writable(t, c.typ)
		before, _ := os.ReadFile(path)
		size := sizeForCount(fs, c.count)
		var err error
		if c.grow {
			err = fs.Grow(size)
		} else {
			err = fs.Shrink(size)
		}
		if !errors.Is(err, ErrResizeType) || !errors.Is(err, ErrResizeFAT) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("FAT%d to %d clusters: %v\nwant it to say %q", c.typ, c.count, err, c.want)
		}
		if err2 := fs.Resize(size); !errors.Is(err2, ErrResizeType) {
			t.Errorf("FAT%d Resize to %d clusters: %v", c.typ, c.count, err2)
		}
		fs.Close()
		if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
			t.Errorf("FAT%d: a refused resize changed the image", c.typ)
		}
	}
}

// Hostile sizes are refused, each with its own reason, and change nothing.
func TestFAT1216ResizeHostileSizes(t *testing.T) {
	for _, typ := range []int{12, 16} {
		path, fs := f1216Writable(t, typ)
		before, _ := os.ReadFile(path)
		cur := int64(len(before))
		cs := int64(fs.info.ClusterSize())
		huge := (int64(math.MaxInt64) / cs) * cs                   // a whole number of sectors
		overflow := int64(1) << 32 * int64(fs.info.BytesPerSector) // 2^32 sectors: does not fit BPB_TotSec32
		cases := []struct {
			name string
			run  func() error
			want string
		}{
			{"Grow(0)", func() error { return fs.Grow(0) }, "<= current size"},
			{"Grow(-1)", func() error { return fs.Grow(-1) }, "<= current size"},
			{"Grow(same)", func() error { return fs.Grow(cur) }, "<= current size"},
			{"Grow(+1 byte)", func() error { return fs.Grow(cur + 1) }, "not a multiple of sector size"},
			{"Grow(+511 bytes)", func() error { return fs.Grow(cur + 511) }, "not a multiple of sector size"},
			{"Grow(2^32 sectors)", func() error { return fs.Grow(overflow) }, "exceeds 32-bit sector count"},
			{"Grow(MaxInt64)", func() error { return fs.Grow(math.MaxInt64) }, "not a multiple of sector size"},
			{"Grow(MaxInt64 rounded)", func() error { return fs.Grow(huge) }, "exceeds 32-bit sector count"},
			{"Shrink(0)", func() error { return fs.Shrink(0) }, "not positive"},
			{"Shrink(-512)", func() error { return fs.Shrink(-512) }, "not positive"},
			{"Shrink(MinInt64)", func() error { return fs.Shrink(math.MinInt64) }, "not positive"},
			{"Shrink(same)", func() error { return fs.Shrink(cur) }, ">= current size"},
			{"Shrink(-1 byte)", func() error { return fs.Shrink(cur - 1) }, "not a multiple of sector size"},
			{"Shrink(one cluster)", func() error { return fs.Shrink(cs) }, "yields 0 data clusters"},
			{"Shrink(metadata only)", func() error { return fs.Shrink(sizeForCount(fs, 0) - cs) }, "yields 0 data clusters"},
			{"Shrink(15 clusters)", func() error { return fs.Shrink(sizeForCount(fs, 15)) }, "below floor"},
			{"Shrink(in use)", func() error { return fs.Shrink(sizeForCount(fs, 20)) }, "is in use"},
			{"Resize(0)", func() error { return fs.Resize(0) }, "not positive"},
			{"Resize(-5)", func() error { return fs.Resize(-5) }, "not positive"},
		}
		for _, c := range cases {
			if typ == 16 && (c.name == "Shrink(15 clusters)" || c.name == "Shrink(in use)") {
				continue // FAT16 would first change type: covered by the type test
			}
			err := c.run()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("FAT%d %s = %v, want %q", typ, c.name, err, c.want)
			}
		}
		fs.Close()
		if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
			t.Errorf("FAT%d: a refused resize changed the image", typ)
		}
	}
}

// A shrink on a FAT16 volume that still holds data past the cut is refused
// because the cluster is in use, not because of the type.
func TestFAT1216ShrinkRefusesUsedCluster(t *testing.T) {
	path, fs := f1216Writable(t, 16)
	before, _ := os.ReadFile(path)
	hi := int64(highestUsed(t, fs))
	if hi-2 < 4085 {
		// The fixture is packed low: put a file's last cluster high up.
		data := pat(5000*int(fs.info.ClusterSize()), 5)
		if err := fs.WriteFile("/HIGH.BIN", data, 0o644); err != nil {
			t.Fatal(err)
		}
		hi = int64(highestUsed(t, fs))
		before, _ = os.ReadFile(path)
	}
	err := fs.Shrink(sizeForCount(fs, hi-2)) // cuts the last used cluster
	if err == nil || !strings.Contains(err.Error(), "is in use") {
		t.Fatalf("Shrink over a used cluster = %v", err)
	}
	fs.Close()
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("a refused shrink changed the image")
	}
}

// noTrunc is a writable image that cannot be truncated.
type noTrunc struct{ b []byte }

func (r *noTrunc) ReadAt(p []byte, off int64) (int, error) { return (&rwBuf{r.b}).ReadAt(p, off) }
func (r *noTrunc) WriteAt(p []byte, off int64) (int, error) {
	return (&rwBuf{r.b}).WriteAt(p, off)
}

func TestFAT1216ResizeNeedsTruncate(t *testing.T) {
	for _, typ := range []int{12, 16} {
		_, img := f1216Fixture(t, typ)
		fs0, err := OpenReader(&noTrunc{img}, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		fs := fs0.(*fat32FS)
		before := append([]byte(nil), img...)
		shrunk := int64(fs.info.DataClusters) - 100
		if typ == 12 {
			shrunk = 1000
		}
		if err := fs.Shrink(sizeForCount(fs, shrunk)); err == nil || !strings.Contains(err.Error(), "does not support Truncate") {
			t.Errorf("FAT%d Shrink without Truncate = %v", typ, err)
		}
		// The image is not truncated, so cur = TotalSectors; a grow into the slack.
		fs.info.TotalSectors -= 4
		fs.info.DataClusters -= 4
		if err := fs.Grow(sizeForCount(fs, int64(fs.info.DataClusters)+4)); err == nil || !strings.Contains(err.Error(), "does not support Truncate") {
			t.Errorf("FAT%d Grow without Truncate = %v", typ, err)
		}
		if !bytes.Equal(before, img) {
			t.Errorf("FAT%d: a refused resize changed the image", typ)
		}
	}
}

// An image longer than its volume has slack after the last sector: a shrink
// that only cuts into the slack is not a shrink of the volume, and is refused
// rather than treated as a grow.
func TestFAT1216ShrinkOnlyCutsSlack(t *testing.T) {
	path, fs := f1216Writable(t, 12)
	size := int64(fs.info.TotalSectors) * int64(fs.info.BytesPerSector)
	if err := os.Truncate(path, size+8192); err != nil {
		t.Fatal(err)
	}
	if err := fs.Shrink(size + 4096); err == nil || !strings.Contains(err.Error(), "does not cut into") {
		t.Fatalf("Shrink into the slack = %v", err)
	}
}

// A volume inside a partition is not resized: the partition table is not ours.
func TestFAT1216ResizeRefusesPartition(t *testing.T) {
	_, fs := f1216Writable(t, 12)
	fs.partOffset = 512
	if err := fs.Grow(1 << 30); err == nil || !strings.Contains(err.Error(), "inside a partition") {
		t.Fatalf("Grow in a partition = %v", err)
	}
	if err := fs.Shrink(1 << 10); err == nil || !strings.Contains(err.Error(), "inside a partition") {
		t.Fatalf("Shrink in a partition = %v", err)
	}
}

// resizeFaulty fails reads, writes, Truncate or Stat on demand.
type resizeFaulty struct {
	*os.File
	readsLeft, writesLeft int // fail once exhausted; -1 never
	truncErr, statErr     error
}

var errResizeFault = errors.New("injected resize fault")

func (f *resizeFaulty) ReadAt(p []byte, off int64) (int, error) {
	if f.readsLeft == 0 {
		return 0, errResizeFault
	}
	if f.readsLeft > 0 {
		f.readsLeft--
	}
	return f.File.ReadAt(p, off)
}

func (f *resizeFaulty) WriteAt(p []byte, off int64) (int, error) {
	if f.writesLeft == 0 {
		return 0, errResizeFault
	}
	if f.writesLeft > 0 {
		f.writesLeft--
	}
	return f.File.WriteAt(p, off)
}

func (f *resizeFaulty) Truncate(n int64) error {
	if f.truncErr != nil {
		return f.truncErr
	}
	return f.File.Truncate(n)
}

func (f *resizeFaulty) Stat() (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return f.File.Stat()
}

func faultyVolume(t *testing.T, typ int) (string, *resizeFaulty, *fat32FS) {
	t.Helper()
	_, img := f1216Fixture(t, typ)
	path := filepath.Join(t.TempDir(), "f.img")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	info, err := readInfo(raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	ff := &resizeFaulty{File: raw, readsLeft: -1, writesLeft: -1}
	return path, ff, &fat32FS{f: ff, info: info}
}

// Every I/O error of a resize is reported, and a failed step never leaves a
// volume that does not open.
func TestFAT1216ResizeIOErrors(t *testing.T) {
	for _, typ := range []int{12, 16} {
		shrinkTo := func(fs *fat32FS) int64 { return sizeForCount(fs, int64(fs.info.DataClusters)-12) }
		growTo := func(fs *fat32FS) int64 { return sizeForCount(fs, int64(fs.info.DataClusters)+4) }
		type tc struct {
			name  string
			setup func(*resizeFaulty)
			run   func(*fat32FS) error
		}
		cases := []tc{
			{"Stat/Grow", func(f *resizeFaulty) { f.statErr = errResizeFault }, func(fs *fat32FS) error { return fs.Grow(growTo(fs)) }},
			{"Stat/Shrink", func(f *resizeFaulty) { f.statErr = errResizeFault }, func(fs *fat32FS) error { return fs.Shrink(shrinkTo(fs)) }},
			{"Stat/Resize", func(f *resizeFaulty) { f.statErr = errResizeFault }, func(fs *fat32FS) error { return fs.Resize(shrinkTo(fs)) }},
			{"Truncate/Shrink", func(f *resizeFaulty) { f.truncErr = errResizeFault }, func(fs *fat32FS) error { return fs.Shrink(shrinkTo(fs)) }},
			{"WriteBPB/Shrink", func(f *resizeFaulty) { f.writesLeft = 0 }, func(fs *fat32FS) error { return fs.Shrink(shrinkTo(fs)) }},
			// The scan reads 12 entries and passes; the boot sector read fails.
			{"ReadBPB/Shrink", func(f *resizeFaulty) { f.readsLeft = 12 }, func(fs *fat32FS) error { return fs.Shrink(shrinkTo(fs)) }},
			{"ScanFAT/Shrink", func(f *resizeFaulty) { f.readsLeft = 0 }, func(fs *fat32FS) error { return fs.Shrink(shrinkTo(fs)) }},
			{"Truncate/Grow", func(f *resizeFaulty) { f.truncErr = errResizeFault }, func(fs *fat32FS) error { return fs.Grow(growTo(fs)) }},
			{"ClearTail/Grow", func(f *resizeFaulty) { f.writesLeft = 0 }, func(fs *fat32FS) error { return fs.Grow(growTo(fs)) }},
			// The tail clear writes once per FAT copy (2); the boot sector write fails.
			{"WriteBPB/Grow", func(f *resizeFaulty) { f.writesLeft = 2 }, func(fs *fat32FS) error { return fs.Grow(growTo(fs)) }},
		}
		for _, c := range cases {
			path, ff, fs := faultyVolume(t, typ)
			// Grow needs slack: shrink the volume honestly first, then arm the fault.
			if strings.HasSuffix(c.name, "/Grow") {
				if err := fs.Shrink(sizeForCount(fs, int64(fs.info.DataClusters)-12)); err != nil {
					t.Fatal(err)
				}
			}
			c.setup(ff)
			err := c.run(fs)
			if !errors.Is(err, errResizeFault) {
				t.Errorf("FAT%d %s = %v, want the injected fault", typ, c.name, err)
			}
			ff.File.Close()
			re, err := Open(path, -1)
			if err != nil {
				t.Fatalf("FAT%d %s left an image that does not open: %v", typ, c.name, err)
			}
			checkVolume(t, re.(*fat32FS))
			re.Close()
		}
	}
}

// FAT12 only: the tail clear reads one byte per copy when the first new
// cluster is odd; a failing read there is reported.
func TestFAT12GrowReadsSharedByte(t *testing.T) {
	_, ff, fs := faultyVolume(t, 12)
	if err := fs.Shrink(sizeForCount(fs, int64(fs.info.DataClusters)-8)); err != nil {
		t.Fatal(err)
	}
	if fs.lastCluster()&1 != 1 {
		t.Fatalf("test needs an odd first new cluster, got %d", fs.lastCluster())
	}
	ff.readsLeft = 0
	if err := fs.Grow(sizeForCount(fs, int64(fs.info.DataClusters)+8)); !errors.Is(err, errResizeFault) {
		t.Fatalf("Grow with the shared byte unreadable = %v", err)
	}
}

// A shrink that fails at any of its writes, and a grow likewise, leaves a
// volume that opens at the old or the new size with every file intact: the
// crash-safety ordering of the write path, extended to resize.
func TestFAT1216ResizeFailureAtEveryWrite(t *testing.T) {
	for _, typ := range []int{12, 16} {
		for _, op := range []string{"Shrink", "Grow"} {
			im, _ := f1216Fixture(t, typ)
			for k := 0; k < 40; k++ {
				path, ff, fs := faultyVolume(t, typ)
				orig := int64(fs.info.DataClusters)
				cut := int64(12)
				if op == "Grow" {
					// start from a shrunk volume so the FAT has room
					if err := fs.Shrink(sizeForCount(fs, orig-cut)); err != nil {
						t.Fatal(err)
					}
				}
				ff.writesLeft = k
				var opErr error
				if op == "Shrink" {
					opErr = fs.Shrink(sizeForCount(fs, orig-cut))
				} else {
					opErr = fs.Grow(sizeForCount(fs, orig))
				}
				ff.File.Close()
				re, err := Open(path, -1)
				if err != nil {
					t.Fatalf("FAT%d %s failing after %d writes: image does not open: %v", typ, op, k, err)
				}
				r := re.(*fat32FS)
				if n := int64(r.info.DataClusters); n != orig && n != orig-cut {
					t.Fatalf("FAT%d %s after %d writes: %d clusters, neither %d nor %d", typ, op, k, n, orig, orig-cut)
				}
				if leaks := checkVolume(t, r); leaks != 0 {
					t.Fatalf("FAT%d %s after %d writes: %d leaked clusters", typ, op, k, leaks)
				}
				filesIntact(t, im, r)
				r.Close()
				if opErr == nil {
					fsckClean(t, path)
					break
				}
			}
		}
	}
}

// A FAT16 volume that crosses 65535 sectors switches BPB_TotSec16 to
// BPB_TotSec32 and back, and fsck-visible readers (Open) follow.
func TestFAT16TotalSectorsFields(t *testing.T) {
	const rsvd, fatSz, spc = 1, 256, 1
	path := filepath.Join(t.TempDir(), "big.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	boot := f1216Boot(60000, spc, fatSz, 512, rsvd, 2)
	if _, err := f.WriteAt(boot, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(60000 * 512); err != nil {
		t.Fatal(err)
	}
	f.Close()
	fs0, err := Open(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	fs := fs0.(*fat32FS)
	defer fs.Close()
	fields := func() (uint16, uint32) {
		b, _ := os.ReadFile(path)
		return binary.LittleEndian.Uint16(b[19:]), binary.LittleEndian.Uint32(b[32:])
	}
	if err := fs.Grow(66000 * 512); err != nil {
		t.Fatal(err)
	}
	if t16, t32 := fields(); t16 != 0 || t32 != 66000 {
		t.Fatalf("TotSec16=%d TotSec32=%d after growing past 16 bits", t16, t32)
	}
	if fs.info.Type != FAT16 || fs.info.TotalSectors != 66000 {
		t.Fatalf("info after grow: %v %d", fs.info.Type, fs.info.TotalSectors)
	}
	if err := fs.Shrink(61000 * 512); err != nil {
		t.Fatal(err)
	}
	if t16, t32 := fields(); t16 != 61000 || t32 != 0 {
		t.Fatalf("TotSec16=%d TotSec32=%d after shrinking back under 16 bits", t16, t32)
	}
	re, err := Open(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if r := re.(*fat32FS); r.info.TotalSectors != 61000 || r.info.Type != FAT16 {
		t.Fatalf("reopened: %v %d", r.info.Type, r.info.TotalSectors)
	}
}
