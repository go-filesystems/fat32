package filesystem_fat32

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// FAT32: a grow that must grow its FAT, failed at (a sample of) every write, then
// reopened. The acceptance rule of the journaled move: the volume opens, is
// fsck-clean, and every file's SHA-256 is all old or all new (files never change
// in a grow, so: every hash intact), and the geometry is the old or the new one.

type grow32Volume struct {
	path   string
	hashes map[string][32]byte
}

func newGrow32Volume(t *testing.T) grow32Volume {
	t.Helper()
	path := filepath.Join(t.TempDir(), "g32.img")
	fsi, err := Format(path, resizeBaseSize/4, FormatConfig{Label: "GROW32"})
	if err != nil {
		t.Fatal(err)
	}
	v := grow32Volume{path: path, hashes: map[string][32]byte{}}
	for i, size := range []int{10, 5000, 300000, 1 << 20} {
		name := "/F" + string(rune('A'+i)) + ".BIN"
		data := pat(size, byte(i+1))
		if err := fsi.WriteFile(name, data, 0o644); err != nil {
			t.Fatal(err)
		}
		v.hashes[name] = sha256.Sum256(data)
	}
	if err := fsi.Close(); err != nil {
		t.Fatal(err)
	}
	return v
}

func (v grow32Volume) copyTo(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(v.path)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "run.img")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (v grow32Volume) check(t *testing.T, path string, wantTotal func(uint32) bool) {
	t.Helper()
	re, err := Open(path, -1)
	if err != nil {
		t.Fatalf("image does not open: %v", err)
	}
	defer re.Close()
	if !wantTotal(re.(*fat32FS).info.TotalSectors) {
		t.Fatalf("total sectors %d is neither the old nor the new geometry", re.(*fat32FS).info.TotalSectors)
	}
	for name, want := range v.hashes {
		got, err := re.ReadFile(name)
		if err != nil || sha256.Sum256(got) != want {
			t.Fatalf("%s: err=%v, hash differs", name, err)
		}
	}
}

func openCounting(t *testing.T, path string, writesLeft int) (*resizeFaulty, *fat32FS) {
	t.Helper()
	raw, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	info, err := readInfo(raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	ff := &resizeFaulty{File: raw, readsLeft: -1, writesLeft: writesLeft}
	return ff, &fat32FS{f: ff, info: info}
}

func TestGrowFAT32FailureAtEveryWrite(t *testing.T) {
	v := newGrow32Volume(t)
	target := resizeBaseSize / 2
	// Count the writes of an uninterrupted grow.
	cp := v.copyTo(t)
	cf, cfs := openCounting(t, cp, 1<<30)
	oldTotal := cfs.info.TotalSectors
	if err := cfs.Grow(target); err != nil {
		t.Fatal(err)
	}
	newTotal := cfs.info.TotalSectors
	total := 1<<30 - cf.writesLeft
	isOldOrNew := func(n uint32) bool { return n == oldTotal || n == newTotal }
	stride := total/150 + 1
	seen := 0
	for k := 0; k <= total; k++ {
		if k > 12 && k < total-12 && k%stride != 0 {
			continue
		}
		seen++
		path := v.copyTo(t)
		ff, fs := openCounting(t, path, k)
		err := fs.Grow(target)
		if (err == nil) != (k == total) {
			t.Fatalf("failing at write %d of %d: %v", k, total, err)
		}
		if err != nil && !errors.Is(err, errResizeFault) {
			t.Fatalf("failing at write %d: %v", k, err)
		}
		ff.File.Close()
		v.check(t, path, isOldOrNew)
		fsckClean(t, path)
		raw, _ := os.ReadFile(path)
		if _, ok := decodeGrowJournal(raw[len(raw)-512:]); ok {
			t.Fatalf("failing at write %d: the journal outlived the recovery", k)
		}
	}
	t.Logf("%d of %d writes failed in turn", seen, total)
}

// A refused grow (too small to journal) writes nothing.
func TestGrowRefusedWhenTooSmallToJournal(t *testing.T) {
	_, fs := f1216Writable(t, 16)
	_, capacity := fs.narrowGeometry(int64(fs.info.TotalSectors))
	// One cluster past what the FAT holds needs a bigger FAT that eats the gain.
	size := sizeForCount(fs, capacity+1)
	plan := fs.info
	_ = plan
	if _, err := fs.planMove(int64(fs.info.FATSize)+1, int64(fs.info.TotalSectors)+1, size); !errors.Is(err, ErrGrowTooSmall) {
		t.Fatalf("planMove = %v, want ErrGrowTooSmall", err)
	}
}

// A volume whose last sector holds a record for another geometry, or a damaged
// one, or nothing, is left alone; a read-only reader over an interrupted volume
// is refused rather than served half-moved.
func TestRecoverGrowIgnoresForeignRecords(t *testing.T) {
	path, fs := f1216Writable(t, 16)
	size := int64(fs.info.TotalSectors) * int64(fs.info.BytesPerSector)
	bps := int64(fs.info.BytesPerSector)
	if err := os.Truncate(path, size+bps); err != nil {
		t.Fatal(err)
	}
	raw, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	stale := growJournal{oldFAT: fs.info.FATSize + 9, newFAT: fs.info.FATSize + 10, oldTotal: 1, newTotal: 2}
	if _, err := raw.WriteAt(stale.encode(int(bps)), size); err != nil {
		t.Fatal(err)
	}
	f := &fat32FS{f: raw, info: fs.info}
	if err := f.recoverGrow(size + bps); err != nil {
		t.Fatalf("stale record: %v", err)
	}
	damaged := stale.encode(int(bps))
	damaged[41] ^= 1
	if _, ok := decodeGrowJournal(damaged); ok {
		t.Fatal("a record with a bad checksum was accepted")
	}
	if _, ok := decodeGrowJournal(damaged[:10]); ok {
		t.Fatal("a short record was accepted")
	}
	// An unreadable last sector is an error, not "no record".
	ff := &fat32FS{f: &resizeFaulty{File: raw, readsLeft: 0, writesLeft: -1}, info: fs.info}
	if err := ff.recoverGrow(size + bps); !errors.Is(err, errResizeFault) {
		t.Fatalf("unreadable journal sector: %v", err)
	}
	// A pending record that does not fit the image is refused.
	pending := growJournal{oldFAT: fs.info.FATSize, newFAT: fs.info.FATSize + 1, oldTotal: fs.info.TotalSectors,
		newTotal: fs.info.TotalSectors + 1, remaining: 1 << 40, region: 1 << 40}
	if _, err := raw.WriteAt(pending.encode(int(bps)), size); err != nil {
		t.Fatal(err)
	}
	if err := f.recoverGrow(size + bps); err == nil {
		t.Fatal("a journal that does not fit the image was replayed")
	}
	// A committed record (BPB already the new one) is cleared.
	done := growJournal{oldFAT: 1, newFAT: fs.info.FATSize, oldTotal: 1, newTotal: fs.info.TotalSectors}
	if _, err := raw.WriteAt(done.encode(int(bps)), size); err != nil {
		t.Fatal(err)
	}
	if err := f.recoverGrow(size + bps); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, bps)
	if _, err := raw.ReadAt(b, size); err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeGrowJournal(b); ok {
		t.Fatal("a committed record was not cleared")
	}
}

// OpenReader over an interrupted volume it cannot write: an error.
func TestOpenReaderInterruptedGrowReadOnly(t *testing.T) {
	_, base, bfs := growVolume(t, 16)
	if err := bfs.Grow(sizeForCount(bfs, growCounts[16])); err != nil {
		t.Fatal(err)
	}
	stop := journalWriteIndex(t, base, bfs) + 2
	path, lf, fs := growVolume(t, 16)
	lf.writesLeft = stop
	if err := fs.Grow(sizeForCount(fs, growCounts[16])); err == nil {
		t.Fatal("not interrupted")
	}
	lf.File.Close()
	img, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReader(readOnlyAt{&rwBuf{img}}, int64(len(img))); !errors.Is(err, ErrReadOnlyReader) {
		t.Fatalf("OpenReader over an interrupted volume, read-only: %v", err)
	}
	// Over a writable buffer it is finished, and the volume serves.
	fs2, err := OpenReader(&rwBuf{img}, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if int64(fs2.(*fat32FS).info.DataClusters) != int64(bfs.info.DataClusters) {
		t.Fatal("the recovered volume is not the grown one")
	}
}

type readOnlyAt struct{ r *rwBuf }

func (a readOnlyAt) ReadAt(p []byte, off int64) (int, error) { return a.r.ReadAt(p, off) }
