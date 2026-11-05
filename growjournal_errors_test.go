package filesystem_fat32

import (
	"errors"
	"testing"
)

// syncFaulty fails its n-th Sync (n counted from 0), once.
type syncFaulty struct {
	*resizeFaulty
	left int
}

var errSyncFault = errors.New("injected sync fault")

func (s *syncFaulty) Sync() error {
	if s.left == 0 {
		s.left--
		return errSyncFault
	}
	s.left--
	return nil
}

// Every Sync of a grow can fail: the failure is reported, and the next Open
// finishes the grow.
func TestGrowSyncFailureAtEverySync(t *testing.T) { growSyncFailures(t) }

// The same with a Sync barrier around every chunk, whatever its size.
func TestGrowSyncFailureAtEverySyncWithBarriers(t *testing.T) {
	old := growBarrierMin
	growBarrierMin = 0
	t.Cleanup(func() { growBarrierMin = old })
	growSyncFailures(t)
}

func growSyncFailures(t *testing.T) {
	for _, typ := range []int{16, 32} {
		failed := 0
		for n := 0; n < 100; n++ {
			var path string
			var fs *fat32FS
			var target int64
			var verify func()
			if typ == 32 {
				v := newGrow32Volume(t)
				path = v.copyTo(t)
				ff, f := openCounting(t, path, -1)
				f.f = &syncFaulty{ff, n}
				fs, target = f, resizeBaseSize/2
				verify = func() { v.check(t, path, func(uint32) bool { return true }) }
			} else {
				p, ff, f := faultyVolume(t, 16)
				path, fs = p, f
				fs.f = &syncFaulty{ff, n}
				target = sizeForCount(f, growCounts[16])
				verify = func() {
					re, err := Open(path, -1)
					if err != nil {
						t.Fatal(err)
					}
					checkVolume(t, re.(*fat32FS))
					re.Close()
				}
			}
			err := fs.Grow(target)
			if err == nil {
				break
			}
			failed++
			if !errors.Is(err, errSyncFault) {
				t.Fatalf("FAT%d sync %d: %v", typ, n, err)
			}
			verify()
		}
		if failed < 4 {
			t.Fatalf("FAT%d: only %d syncs failed", typ, failed)
		}
	}
}

// Every read of a FAT32 grow is reported when it fails.
func TestGrowFAT32FailureAtEveryRead(t *testing.T) {
	v := newGrow32Volume(t)
	_, cfs := openCounting(t, v.copyTo(t), -1)
	cf := cfs.f.(*resizeFaulty)
	cf.readsLeft = 1 << 30
	if err := cfs.Grow(resizeBaseSize / 2); err != nil {
		t.Fatal(err)
	}
	total := 1<<30 - cf.readsLeft
	stride := total/120 + 1
	for k := 0; k < total; k++ {
		if k > 10 && k < total-10 && k%stride != 0 {
			continue
		}
		path := v.copyTo(t)
		ff, fs := openCounting(t, path, -1)
		ff.readsLeft = k
		if err := fs.Grow(resizeBaseSize / 2); !errors.Is(err, errResizeFault) {
			t.Fatalf("failing at read %d of %d: %v", k, total, err)
		}
		ff.File.Close()
		v.check(t, path, func(uint32) bool { return true })
	}
}

// The helpers of the journal, at their edges.
func TestGrowJournalEdges(t *testing.T) {
	path, fs := f1216Writable(t, 16)
	_ = path
	// A BPB that leaves no data sector has no data clusters.
	f32 := &fat32FS{info: Info{Type: FAT32, BytesPerSector: 512, SectorsPerCluster: 1, ReservedSectors: 32, FATCount: 2, FATSize: 100, TotalSectors: 10}}
	if n := f32.dataClusterCount(); n != 0 {
		t.Fatalf("dataClusterCount = %d", n)
	}
	// A plain Sync-less backing store has nothing to flush.
	if err := fs.syncBacking(); err != nil {
		t.Fatal(err)
	}
	// A committed record whose clearing fails is reported.
	_, ff, rfs := faultyVolume(t, 16)
	size := int64(rfs.info.TotalSectors) * int64(rfs.info.BytesPerSector)
	bps := int64(rfs.info.BytesPerSector)
	done := growJournal{oldFAT: 1, newFAT: rfs.info.FATSize, oldTotal: 1, newTotal: rfs.info.TotalSectors}
	if _, err := ff.File.WriteAt(done.encode(int(bps)), size-bps); err != nil {
		t.Fatal(err)
	}
	ff.writesLeft = 0
	if err := rfs.recoverGrow(size); !errors.Is(err, errResizeFault) {
		t.Fatalf("recoverGrow with the clear failing: %v", err)
	}
	// openRecover: a failing recovery and a failing re-read are both reported.
	if err := rfs.openRecover(size, nil); !errors.Is(err, errResizeFault) {
		t.Fatalf("openRecover: %v", err)
	}
	ff.writesLeft = -1
	_, base, bfs := growVolume(t, 16)
	if err := bfs.Grow(sizeForCount(bfs, growCounts[16])); err != nil {
		t.Fatal(err)
	}
	stop := journalWriteIndex(t, base, bfs) + 2
	_, lf, gfs := growVolume(t, 16)
	lf.writesLeft = stop
	if err := gfs.Grow(sizeForCount(gfs, growCounts[16])); err == nil {
		t.Fatal("not interrupted")
	}
	lf.writesLeft = -1
	rerr := errors.New("reread boom")
	fresh := &fat32FS{f: lf, info: gfs.info}
	fresh.info.FATSize, fresh.info.TotalSectors = bfs.info.FATSize, bfs.info.TotalSectors
	fresh.info = infoBefore(t, lf)
	st, _ := lf.File.Stat()
	if err := fresh.openRecover(st.Size(), func() (Info, error) { return Info{}, rerr }); !errors.Is(err, rerr) {
		t.Fatalf("openRecover re-read: %v", err)
	}
}

func infoBefore(t *testing.T, f *logFaulty) Info {
	t.Helper()
	info, err := readInfo(f.File, 0)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// A FAT32 grow that does not need a bigger FAT only rewrites the geometry: no
// journal, one write per boot sector. Every write and read of it can fail, and
// the volume is then the old or the new one, whole.
func TestGrowFAT32SameFATFailureAtEveryIO(t *testing.T) {
	v := newGrow32Volume(t)
	_, cfs := openCounting(t, v.copyTo(t), 1<<30)
	cf := cfs.f.(*resizeFaulty)
	cf.readsLeft = 1 << 30
	oldFAT, oldTotal := cfs.info.FATSize, cfs.info.TotalSectors
	target := int64(oldTotal+8) * int64(cfs.info.BytesPerSector)
	if err := cfs.Grow(target); err != nil {
		t.Fatal(err)
	}
	if cfs.info.FATSize != oldFAT {
		t.Fatalf("the FAT grew to %d from %d: not the case under test", cfs.info.FATSize, oldFAT)
	}
	writes, reads := 1<<30-cf.writesLeft, 1<<30-cf.readsLeft
	isOldOrNew := func(n uint32) bool { return n == oldTotal || n == cfs.info.TotalSectors }
	for k := 0; k <= writes+reads; k++ {
		path := v.copyTo(t)
		ff, fs := openCounting(t, path, -1)
		if k < writes {
			ff.writesLeft = k
		} else {
			ff.readsLeft = k - writes
		}
		err := fs.Grow(target)
		if (err == nil) != (k == writes+reads) {
			t.Fatalf("failing at io %d of %d: %v", k, writes+reads, err)
		}
		ff.File.Close()
		v.check(t, path, isOldOrNew)
		fsckClean(t, path)
	}
}

// journaledGrow refuses a grow whose moved layout would reach the journal.
func TestJournaledGrowRefusesTooSmall(t *testing.T) {
	_, fs := f1216Writable(t, 16)
	size := int64(fs.info.TotalSectors) * int64(fs.info.BytesPerSector)
	if err := fs.journaledGrow(int64(fs.info.FATSize)+1, int64(fs.info.TotalSectors)+1, size+int64(fs.info.BytesPerSector)); !errors.Is(err, ErrGrowTooSmall) {
		t.Fatalf("journaledGrow = %v", err)
	}
}
