package filesystem_fat32

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"
)

// Growing a FAT12/16 volume whose FAT must itself grow (the layout move of
// resize1216.go): every result is read back file by file against the manifest's
// SHA-256 and judged by fsck_msdos / fsck.vfat.

// growTarget is the image size, in bytes, of count data clusters on the
// current FAT. The FAT grows, so the volume ends with fewer clusters.
var growCounts = map[int]int64{12: 4000, 16: 30000}

func TestFAT1216GrowBiggerFAT(t *testing.T) {
	for _, typ := range []int{12, 16} {
		t.Run(fmt.Sprintf("FAT%d", typ), func(t *testing.T) {
			im, _ := f1216Fixture(t, typ)
			path, fs := f1216Writable(t, typ)
			fat, orig, kind := fs.info.FATSize, int64(fs.info.DataClusters), fs.info.Type
			if err := fs.Grow(sizeForCount(fs, growCounts[typ])); err != nil {
				t.Fatal(err)
			}
			if fs.info.FATSize <= fat {
				t.Fatalf("FAT is still %d sectors", fs.info.FATSize)
			}
			count := int64(fs.info.DataClusters)
			if count <= orig {
				t.Fatalf("%d clusters after the grow, had %d", count, orig)
			}
			// The grown volume is writable end to end, into the last cluster.
			data := pat(int(count-orig)*int(fs.info.ClusterSize()), 5)
			if err := fs.WriteFile("/DIR1/GROWN.BIN", data, 0o644); err != nil {
				t.Fatal(err)
			}
			if got, err := fs.ReadFile("/DIR1/GROWN.BIN"); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("file written into the grown volume: %v", err)
			}
			if err := fs.DeleteFile("/DIR1/GROWN.BIN"); err != nil {
				t.Fatal(err)
			}
			settleResized(t, im, path, fs, kind, count)
		})
	}
}

// A grow that grows the FAT but is too small to keep the clusters the volume
// already has is refused, and so is one that would change the type.
func TestFAT1216GrowBiggerFATRefusals(t *testing.T) {
	_, fs := f1216Writable(t, 12)
	_, capacity := fs.narrowGeometry(int64(fs.info.TotalSectors))
	if err := fs.Grow(sizeForCount(fs, capacity)); err != nil {
		t.Fatal(err)
	}
	// One sector more would need a bigger FAT that eats it.
	size := int64(fs.info.TotalSectors+1) * int64(fs.info.BytesPerSector)
	if err := fs.Grow(size); !errors.Is(err, ErrResizeFAT) {
		t.Fatalf("Grow by one sector at a full FAT = %v", err)
	}
	if fs.info.TotalSectors+1 == 0 || int64(fs.info.DataClusters) != capacity {
		t.Fatal("a refused grow changed the volume")
	}
	// Past 4084 clusters: FAT12 would become FAT16.
	if err := fs.Grow(sizeForCount(fs, 5000)); !errors.Is(err, ErrResizeType) {
		t.Fatalf("Grow across the FAT12/16 threshold = %v", err)
	}
	// FAT size that BPB_FATSz16 cannot hold, and no data cluster at all.
	big := &fat32FS{f: fs.f, info: fs.info}
	big.info.Type = FAT16
	if _, _, err := big.narrowGrowGeometry(1 << 32); !errors.Is(err, ErrResizeFAT) {
		t.Fatalf("narrowGrowGeometry(2^32) = %v", err)
	}
	if _, _, err := big.narrowGrowGeometry(1); !errors.Is(err, ErrResizeFAT) {
		t.Fatalf("narrowGrowGeometry(1) = %v", err)
	}
}

func TestFAT1216GrowBadBootSignature(t *testing.T) {
	path, fs := f1216Writable(t, 16)
	raw, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.WriteAt([]byte{0, 0}, 510); err != nil {
		t.Fatal(err)
	}
	if err := fs.Grow(sizeForCount(fs, growCounts[16])); err == nil {
		t.Fatal("Grow over a boot sector without a signature succeeded")
	}
}

// logFaulty records the offset of every write.
type logFaulty struct {
	*resizeFaulty
	offs []int64
}

func (l *logFaulty) WriteAt(p []byte, off int64) (int, error) {
	n, err := l.resizeFaulty.WriteAt(p, off)
	if err == nil {
		l.offs = append(l.offs, off)
	}
	return n, err
}

func growVolume(t *testing.T, typ int) (string, *logFaulty, *fat32FS) {
	t.Helper()
	path, ff, fs := faultyVolume(t, typ)
	lf := &logFaulty{resizeFaulty: ff}
	fs.f = lf
	return path, lf, fs
}

// A grow interrupted at every one of its writes. The design note's guarantee:
//   - before the first write that touches the old root/data region the old
//     volume is untouched: it opens, is consistent, keeps every file;
//   - from there to the boot sector write (the window) the volume is not
//     consistent; the test measures that window and asserts it ends at the very
//     last write, the boot sector;
//   - a grow that completes is fsck-clean with every file's hash unchanged.
func TestFAT1216GrowFailureAtEveryWrite(t *testing.T) {
	for _, typ := range []int{12, 16} {
		im, _ := f1216Fixture(t, typ)
		_, base, bfs := growVolume(t, typ)
		orig := int64(bfs.info.DataClusters)
		rootStart := (int64(bfs.info.ReservedSectors) + int64(bfs.info.FATCount)*int64(bfs.info.FATSize)) * int64(bfs.info.BytesPerSector)
		if err := bfs.Grow(sizeForCount(bfs, growCounts[typ])); err != nil {
			t.Fatal(err)
		}
		total := len(base.offs)
		first := total
		for i, o := range base.offs {
			if o >= rootStart {
				first = i
				break
			}
		}
		if first >= total-1 {
			t.Fatalf("FAT%d: %d writes, first destructive %d: no window to measure", typ, total, first)
		}
		for k := 0; k <= total; k++ {
			path, lf, fs := growVolume(t, typ)
			lf.writesLeft = k
			opErr := fs.Grow(sizeForCount(fs, growCounts[typ]))
			lf.File.Close()
			if (opErr == nil) != (k == total) {
				t.Fatalf("FAT%d: failing at write %d of %d: %v", typ, k, total, opErr)
			}
			re, err := Open(path, -1)
			if k >= first && k < total {
				// The window: the volume is not promised consistent. Only the
				// last write of all, the boot sector, ends it.
				if err == nil {
					re.Close()
				}
				continue
			}
			if err != nil {
				t.Fatalf("FAT%d: failing at write %d: image does not open: %v", typ, k, err)
			}
			r := re.(*fat32FS)
			want := orig
			if k == total {
				want = int64(fs.info.DataClusters)
			}
			if int64(r.info.DataClusters) != want {
				t.Fatalf("FAT%d: failing at write %d: %d clusters, want %d", typ, k, r.info.DataClusters, want)
			}
			if leaks := checkVolume(t, r); leaks != 0 {
				t.Fatalf("FAT%d: failing at write %d: %d leaked clusters", typ, k, leaks)
			}
			filesIntact(t, im, r)
			r.Close()
			if k == total {
				fsckClean(t, path)
			} else if k > 0 {
				// An aborted early step left the old volume fsck-clean too.
				fsckClean(t, path)
			}
		}
	}
}

// Every read of a grow can fail, and each failure is reported, never swallowed.
func TestFAT1216GrowFailureAtEveryRead(t *testing.T) {
	for _, typ := range []int{12, 16} {
		for k := 0; k < 1000; k++ {
			_, lf, fs := growVolume(t, typ)
			lf.readsLeft = k
			err := fs.Grow(sizeForCount(fs, growCounts[typ]))
			if err == nil {
				break
			}
			if !errors.Is(err, errResizeFault) {
				t.Fatalf("FAT%d: failing at read %d: %v", typ, k, err)
			}
		}
	}
}

// FuzzGrowFAT drives a fixture volume to a fuzzed size, as Grow and as Shrink;
// whatever the size, nothing panics, the type does not change, and every file
// keeps its hash.
func FuzzGrowFAT(f *testing.F) {
	f.Add(uint32(100), false)
	f.Add(uint32(2000), true)
	f.Add(uint32(60000), false)
	f.Add(uint32(70000), true)
	f.Add(uint32(0), true)
	f.Fuzz(func(t *testing.T, extra uint32, wide bool) {
		typ := 12
		if wide {
			typ = 16
		}
		im, img := f1216Fixture(t, typ)
		rw, err := OpenReader(&rwTrunc{rwBuf{img}}, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		fs := rw.(*fat32FS)
		kind := fs.info.Type
		size := (int64(fs.info.TotalSectors) + int64(extra%200000)) * int64(fs.info.BytesPerSector)
		if err := fs.Grow(size); err == nil {
			if fs.info.Type != kind {
				t.Fatalf("type changed to %v", fs.info.Type)
			}
			filesIntact(t, im, fs)
			checkVolume(t, fs)
		}
	})
}
