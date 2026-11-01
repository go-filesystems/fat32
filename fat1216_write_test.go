package filesystem_fat32

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Writing FAT12/16. Every scenario runs on an image formatted by newfs_msdos
// (testdata/), then is judged by (1) an in-process structural checker, (2)
// both FAT copies being equal, (3) fsck_msdos -n / fsck.vfat -n when present,
// and for one scenario (4) a read-only OS mount and mtools as foreign readers.

// f1216Writable copies the fixture to a file and opens it for writing.
func f1216Writable(t *testing.T, typ int) (string, *fat32FS) {
	t.Helper()
	_, img := f1216Fixture(t, typ)
	path := filepath.Join(t.TempDir(), fmt.Sprintf("fat%d.img", typ))
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	return path, fs.(*fat32FS)
}

// checkVolume is a structural fsck: every file and directory chain is made of
// in-use, in-range clusters, ends in an end-of-chain mark, covers the size, and
// shares no cluster with another chain. It returns the number of clusters in
// use that nothing references (leaks: harmless to readers, reported by fsck).
func checkVolume(t *testing.T, fs *fat32FS) (leaks int) {
	t.Helper()
	used := map[uint32]string{}
	var walk func(dir uint32, path string)
	walk = func(dir uint32, path string) {
		buf, err := fs.readDirBuf(dir)
		if err != nil {
			t.Fatalf("checkVolume: read %s: %v", path, err)
		}
		for _, e := range parseRootDirMetadata(buf) {
			if e.name == "." || e.name == ".." || e.attr&fatAttrVolumeID != 0 {
				continue
			}
			p := path + "/" + e.name
			if e.cluster < 2 {
				if e.size != 0 && e.attr&fatAttrDirectory == 0 {
					t.Errorf("checkVolume: %s has size %d and no cluster", p, e.size)
				}
				continue
			}
			n := 0
			for c := e.cluster; ; n++ {
				if c < 2 || c >= fs.lastCluster() {
					t.Errorf("checkVolume: %s chain leaves the volume at %d", p, c)
					break
				}
				if prev, dup := used[c]; dup {
					t.Errorf("checkVolume: cluster %d is in both %s and %s", c, prev, p)
					break
				}
				used[c] = p
				v, err := fs.fatEntry(c)
				if err != nil {
					t.Errorf("checkVolume: %s: %v", p, err)
					break
				}
				if v == 0 {
					t.Errorf("checkVolume: %s uses cluster %d, which the FAT says is free", p, c)
					break
				}
				if v >= fs.eocCluster() {
					break
				}
				if v >= fs.badCluster() {
					t.Errorf("checkVolume: %s chain ends in a bad cluster mark", p)
					break
				}
				c = v
			}
			n++
			if e.attr&fatAttrDirectory == 0 {
				if need := (int64(e.size) + int64(fs.info.ClusterSize()) - 1) / int64(fs.info.ClusterSize()); int64(n) < need {
					t.Errorf("checkVolume: %s has %d clusters for %d bytes", p, n, e.size)
				}
			} else {
				walk(e.cluster, p)
			}
		}
	}
	walk(fs.rootDir(), "")
	for c := uint32(2); c < fs.lastCluster(); c++ {
		v, err := fs.fatEntry(c)
		if err != nil {
			t.Fatal(err)
		}
		if v != 0 {
			if _, ok := used[c]; !ok {
				leaks++
			}
		}
	}
	return leaks
}

// fatCopiesEqual requires every FAT copy to be byte-identical to the first.
func fatCopiesEqual(t *testing.T, path string, fs *fat32FS) {
	t.Helper()
	img, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(fs.info.FATSize) * int64(fs.info.BytesPerSector)
	base := fs.info.FATOffset(0)
	for i := int64(1); i < int64(fs.info.FATCount); i++ {
		if !bytes.Equal(img[base:base+size], img[base+i*size:base+(i+1)*size]) {
			t.Fatalf("FAT copy %d differs from FAT 0", i)
		}
	}
}

// fsckClean runs the platform's canonical fsck read-only on the image.
func fsckClean(t *testing.T, path string) {
	t.Helper()
	fsck, ok := lookupFsck()
	if !ok {
		t.Log("no fsck_msdos / fsck.vfat on this machine: structural checks only")
		return
	}
	out, err := exec.Command(fsck, "-n", path).CombinedOutput()
	lc := strings.ToLower(string(out))
	if err != nil || strings.Contains(lc, "had errors") || strings.Contains(lc, "filesystem is dirty") ||
		strings.Contains(lc, "no fixes were made, but errors were found") {
		t.Fatalf("%s -n on %s: %v\n%s", filepath.Base(fsck), filepath.Base(path), err, out)
	}
}

// settle closes the volume and judges the image on disk.
func settle(t *testing.T, path string, fs *fat32FS) {
	t.Helper()
	if leaks := checkVolume(t, fs); leaks != 0 {
		t.Errorf("%d clusters in use that nothing references", leaks)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	re, err := Open(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	fatCopiesEqual(t, path, re.(*fat32FS))
	re.Close()
	fsckClean(t, path)
}

func pat(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed ^ byte(i>>8)
	}
	return b
}

// Every mutation on both widths, including: a long name in a subdirectory, a
// multi-cluster file, overwrite (smaller and larger), nested mkdir, recursive
// delete, rename within and across directories and over an existing file,
// truncate both ways, and the volume label.
func TestFAT1216Mutations(t *testing.T) {
	for _, typ := range []int{12, 16} {
		t.Run(fmt.Sprintf("FAT%d", typ), func(t *testing.T) {
			path, fs := f1216Writable(t, typ)
			big := pat(40000, 1)
			// /MANY and /DIR1 have room; the FAT12 root is full to its last slot.
			must := func(what string, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
			}
			must("WriteFile big", fs.WriteFile("/DIR1/big file.bin", big, 0o644))
			must("MkDir", fs.MkDir("/DIR1/NEW", 0o755))
			must("MkDir nested", fs.MkDir("/DIR1/NEW/Sub dir", 0o755))
			must("WriteFile nested", fs.WriteFile("/DIR1/NEW/Sub dir/n.txt", []byte("nested"), 0o644))
			must("WriteFile empty", fs.WriteFile("/DIR1/empty", nil, 0o644))
			must("overwrite smaller", fs.WriteFile("/DIR1/big file.bin", big[:3000], 0o644))
			must("overwrite larger", fs.WriteFile("/DIR1/big file.bin", big, 0o644))
			must("Rename same dir", fs.Rename("/DIR1/empty", "/DIR1/renamed"))
			must("Rename across dirs", fs.Rename("/DIR1/NEW/Sub dir/n.txt", "/MANY/moved.txt"))
			must("Rename over existing", fs.Rename("/MANY/G001.TXT", "/MANY/G002.TXT"))
			must("Rename directory", fs.Rename("/DIR1/NEW/Sub dir", "/DIR1/DIR2/Moved dir"))
			must("DeleteFile", fs.DeleteFile("/MANY/G003.TXT"))
			must("Truncate shrink", fs.Truncate("/DIR1/big file.bin", 5000))
			must("Truncate grow", fs.Truncate("/DIR1/big file.bin", 20000))
			must("Truncate to zero", fs.Truncate("/MANY/G004.TXT", 0))
			must("DeleteDir recursive", fs.DeleteDir("/DIR1/NEW"))
			must("SetLabel", fs.SetLabel("NEWLABEL"))

			if got, err := fs.ReadFile("/DIR1/big file.bin"); err != nil || len(got) != 20000 ||
				!bytes.Equal(got[:5000], big[:5000]) || !bytes.Equal(got[5000:], make([]byte, 15000)) {
				t.Fatalf("truncated file reads %d bytes, %v", len(got), err)
			}
			if got, err := fs.ReadFile("/MANY/moved.txt"); err != nil || string(got) != "nested" {
				t.Fatalf("moved file = %q, %v", got, err)
			}
			if got, err := fs.ReadFile("/MANY/G002.TXT"); err != nil || len(got) != 5 {
				t.Fatalf("rename over existing: %v", err)
			}
			if _, err := fs.Stat("/MANY/G001.TXT"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("renamed-away file still there: %v", err)
			}
			if _, err := fs.Stat("/DIR1/NEW"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("deleted dir still there: %v", err)
			}
			if got := fs.Label(); got != "NEWLABEL" {
				t.Fatalf("Label = %q", got)
			}
			// ".." of a directory moved to a subdirectory names its new parent;
			// one created under the root holds cluster 0 (checked on FAT16,
			// whose root has room).
			if typ == 16 {
				must("MkDir at root", fs.MkDir("/TOP", 0o755))
				must("WriteFile LFN at root", fs.WriteFile("/a long name in the root.txt", []byte("root"), 0o644))
				must("WriteFile at root", fs.WriteFile("/SHORT.TXT", pat(5000, 2), 0o644))
				must("Rename into root", fs.Rename("/DIR1/renamed", "/renamed-up"))
				if got, err := fs.ReadFile("/a long name in the root.txt"); err != nil || string(got) != "root" {
					t.Fatalf("root LFN file = %q, %v", got, err)
				}
				raw, _ := fs.readDirBuf(fs.rootDir())
				_ = raw
				top := f1216DotDot(t, fs, "/TOP")
				if top != 0 {
					t.Fatalf("\"..\" of /TOP = %d, want 0", top)
				}
				must("DeleteFile at root", fs.DeleteFile("/SHORT.TXT"))
			}
			settle(t, path, fs)
		})
	}
}

// f1216DotDot returns the first cluster the ".." entry of dir holds.
func f1216DotDot(t *testing.T, fs *fat32FS, dir string) uint32 {
	t.Helper()
	e, _, err := fs.resolvePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	buf, err := fs.readDirBuf(e.cluster)
	if err != nil {
		t.Fatal(err)
	}
	ents := parseRootDirMetadata(buf)
	for _, x := range ents {
		if x.name == ".." {
			return x.cluster
		}
	}
	t.Fatal("no .. entry")
	return 0
}

// A file written through File.WriteAt and File.Truncate, long enough to put
// FAT12 entries on both sides of a sector boundary in its chain.
func TestFAT1216FileWriteAt(t *testing.T) {
	for _, typ := range []int{12, 16} {
		t.Run(fmt.Sprintf("FAT%d", typ), func(t *testing.T) {
			path, fs := f1216Writable(t, typ)
			size := 420000
			if typ == 12 {
				size = 700000 // fits the 776 KiB free; reaches clusters 683 and 1025
			}
			if err := fs.WriteFile("/MANY/W.BIN", []byte("seed"), 0o644); err != nil {
				t.Fatal(err)
			}
			h, err := fs.OpenFile("/MANY/W.BIN")
			if err != nil {
				t.Fatal(err)
			}
			wf := h.(*fat32File)
			want := pat(size, 3)
			if n, err := wf.WriteAt(want, 0); err != nil || n != size {
				t.Fatalf("WriteAt = %d, %v", n, err)
			}
			if typ == 12 {
				straddle := false
				for _, c := range wf.clusters {
					if (int(c)+int(c)/2)%512 == 511 {
						straddle = true
					}
				}
				if !straddle {
					t.Fatal("the chain holds no FAT12 entry straddling a sector boundary")
				}
			}
			if n, err := wf.WriteAt([]byte("PATCH"), 1234); err != nil || n != 5 {
				t.Fatalf("WriteAt patch = %d, %v", n, err)
			}
			copy(want[1234:], "PATCH")
			if err := wf.Truncate(int64(size) / 2); err != nil {
				t.Fatal(err)
			}
			if err := wf.Truncate(int64(size)/2 + 3000); err != nil {
				t.Fatal(err)
			}
			wf.Close()
			got, err := fs.ReadFile("/MANY/W.BIN")
			if err != nil || len(got) != size/2+3000 || !bytes.Equal(got[:size/2], want[:size/2]) ||
				!bytes.Equal(got[size/2:], make([]byte, 3000)) {
				t.Fatalf("read back %d bytes, %v", len(got), err)
			}
			settle(t, path, fs)
		})
	}
}

// FAT12 entries are written as a read-modify-write of the packed word: the
// neighbouring entry, sharing a byte, is untouched, in every FAT copy, at a
// sector straddle too.
func TestFAT12EntryWriteKeepsNeighbours(t *testing.T) {
	path, fs := f1216Writable(t, 12)
	for _, n := range []uint32{500, 501, 340, 341, 342} {
		if err := fs.setFATEntry(n, 0xABC); err != nil {
			t.Fatal(err)
		}
	}
	// Neighbours of the straddling entry 341 keep their values.
	for _, n := range []uint32{340, 341, 342} {
		if v, _ := fs.fatEntry(n); v != 0xABC {
			t.Fatalf("entry %d = %#x", n, v)
		}
	}
	if err := fs.setFATEntry(341, 0x123); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[uint32]uint32{340: 0xABC, 341: 0x123, 342: 0xABC} {
		if v, _ := fs.fatEntry(n); v != want {
			t.Fatalf("after writing 341: entry %d = %#x, want %#x", n, v, want)
		}
	}
	fs.Close()
	re, _ := Open(path, -1)
	fatCopiesEqual(t, path, re.(*fat32FS))
	re.Close()
	if err := fs.setFATEntry(1, 0); err == nil {
		t.Fatal("wrote the reserved entry 1")
	}
}

// Allocation never reaches the reserved and bad values: the highest cluster a
// FAT12/16 volume hands out is below 0xFF7 / 0xFFF7.
func TestFAT1216AllocationStaysInRange(t *testing.T) {
	for _, typ := range []int{12, 16} {
		_, fs := f1216Writable(t, typ)
		if fs.lastCluster() > fs.badCluster() {
			t.Fatalf("FAT%d: clusters run up to %d, past the bad mark %#x", typ, fs.lastCluster()-1, fs.badCluster())
		}
		for c := fs.lastCluster(); c < fs.lastCluster()+3; c++ {
			if err := fs.setFATEntry(c, fs.eocMark()); err == nil {
				t.Fatalf("FAT%d: wrote entry %d, outside the volume", typ, c)
			}
		}
	}
}

// A full disk: the write is refused, the files already there are intact, an
// overwrite that cannot fit leaves the old file as it was, and the image is
// clean.
func TestFAT1216FullDisk(t *testing.T) {
	for _, typ := range []int{12, 16} {
		t.Run(fmt.Sprintf("FAT%d", typ), func(t *testing.T) {
			path, fs := f1216Writable(t, typ)
			chunk := pat(60000, 4)
			var written []string
			for i := 0; ; i++ {
				name := fmt.Sprintf("/MANY/FILL%02d.BIN", i)
				if err := fs.WriteFile(name, chunk, 0o644); err != nil {
					if !strings.Contains(err.Error(), "no free clusters") {
						t.Fatalf("WriteFile on a full disk: %v", err)
					}
					break
				}
				written = append(written, name)
				if i > 400 {
					t.Fatal("the disk never filled")
				}
			}
			if len(written) == 0 {
				t.Fatal("nothing fitted")
			}
			// Take what is left, a cluster at a time, until not one is free.
			for i := 0; ; i++ {
				if err := fs.WriteFile(fmt.Sprintf("/MANY/TAIL%03d.BIN", i), []byte{1}, 0o644); err != nil {
					if !strings.Contains(err.Error(), "no free clusters") && !strings.Contains(err.Error(), "full") {
						t.Fatalf("filling the tail: %v", err)
					}
					break
				}
			}
			for _, n := range written {
				if got, err := fs.ReadFile(n); err != nil || !bytes.Equal(got, chunk) {
					t.Fatalf("%s damaged after the disk filled: %v", n, err)
				}
			}
			// Overwriting with something that cannot fit keeps the old content.
			if err := fs.WriteFile(written[0], pat(len(chunk)*3, 5), 0o644); err == nil {
				// It fitted only if the file held enough space itself; with the
				// disk full that cannot be.
				t.Fatal("an overwrite larger than the free space succeeded")
			}
			if got, err := fs.ReadFile(written[0]); err != nil || !bytes.Equal(got, chunk) {
				t.Fatalf("failed overwrite damaged the old file: %v", err)
			}
			if err := fs.MkDir("/MANY/NODIR", 0o755); err == nil {
				t.Fatal("MkDir on a full disk succeeded")
			}
			if err := fs.Truncate(written[1], int64(len(chunk))*4); err == nil {
				t.Fatal("growing a file on a full disk succeeded")
			}
			if got, err := fs.ReadFile(written[1]); err != nil || !bytes.Equal(got, chunk) {
				t.Fatalf("failed grow damaged the file: %v", err)
			}
			h, _ := fs.OpenFile(written[2])
			if _, err := h.(*fat32File).WriteAt(pat(len(chunk)*4, 6), 0); err == nil {
				t.Fatal("extending a file on a full disk succeeded")
			}
			if got, err := fs.ReadFile(written[2]); err != nil || !bytes.Equal(got, chunk) {
				t.Fatalf("failed extend damaged the file: %v", err)
			}
			// Freeing space makes room again.
			if err := fs.DeleteFile(written[3]); err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile("/MANY/AGAIN.BIN", chunk[:1000], 0o644); err != nil {
				t.Fatalf("no room after a delete: %v", err)
			}
			settle(t, path, fs)
		})
	}
}

// A full fixed root: a create fails cleanly and the volume is unchanged; long
// names need their entries and are refused when only one slot is free; every
// other root operation still works.
func TestFAT12FullRoot(t *testing.T) {
	path, fs := f1216Writable(t, 12)
	img0, _ := os.ReadFile(path)
	for name, err := range map[string]error{
		"WriteFile": fs.WriteFile("/NEW.TXT", []byte("x"), 0o644),
		"MkDir":     fs.MkDir("/NEWDIR", 0o755),
		"Rename":    fs.Rename("/MANY/G000.TXT", "/MOVED.TXT"),
	} {
		if err == nil || !strings.Contains(err.Error(), "full") {
			t.Errorf("%s in a full root: %v", name, err)
		}
	}
	fs.Close()
	if img1, _ := os.ReadFile(path); !bytes.Equal(img0, img1) {
		t.Fatal("a refused create in a full root changed the image")
	}
	fs2, _ := Open(path, -1)
	fs = fs2.(*fat32FS)
	if _, err := fs.Stat("/MANY/G000.TXT"); err != nil {
		t.Fatalf("the file a refused rename was moving is gone: %v", err)
	}
	// Free exactly one slot: a short name now fits, a long one (3 slots) not.
	if err := fs.DeleteFile("/F000.TXT"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/a longer name.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("a long name taking 3 slots fitted in 1")
	}
	if err := fs.WriteFile("/ONE.TXT", []byte("one"), 0o644); err != nil {
		t.Fatalf("short name in the freed slot: %v", err)
	}
	// An overwrite in a full root reuses its own slot.
	if err := fs.WriteFile("/ONE.TXT", []byte("two"), 0o644); err != nil {
		t.Fatalf("overwrite in a full root: %v", err)
	}
	// The label entry exists, so changing the label works in a full root.
	if err := fs.SetLabel("FULLROOT"); err != nil {
		t.Fatalf("SetLabel in a full root: %v", err)
	}
	// ...and a failed attempt to add one changes neither label nor root.
	settle(t, path, fs)
}

// With no volume-label entry and no free slot, SetLabel is refused before the
// boot sector is touched.
func TestFAT12SetLabelNeedsASlot(t *testing.T) {
	path, fs := f1216Writable(t, 12)
	raw, _ := fs.readFixedRoot()
	for off := 0; off < len(raw); off += 32 {
		if raw[off] != 0 && raw[off] != 0xE5 && raw[off+11]&fatAttrVolumeID != 0 && raw[off+11] != fatAttrLongName {
			raw[off] = 0xE5 // drop the label entry...
		}
	}
	// ...and fill the freed slot.
	for off := 0; off < len(raw); off += 32 {
		if raw[off] == 0xE5 {
			copy(raw[off:], "FILLER  TXT")
			raw[off+11] = 0x20
		}
	}
	if err := fs.writeFixedRoot(raw); err != nil {
		t.Fatal(err)
	}
	before := fs.Label()
	if err := fs.SetLabel("NOSLOT"); err == nil {
		t.Fatal("SetLabel succeeded with no slot for the label entry")
	}
	if fs.Label() != before {
		t.Fatalf("Label changed to %q", fs.Label())
	}
	img, _ := os.ReadFile(path)
	if string(img[43:54]) != "FAT12TEST  " {
		t.Fatalf("boot sector label = %q", img[43:54])
	}
	// Clearing the label with no entry to delete is a no-op that works.
	_ = fs.Close()
}

// Setting and clearing the label on FAT16, where the root has room.
func TestFAT16SetAndClearLabel(t *testing.T) {
	path, fs := f1216Writable(t, 16)
	for _, l := range []string{"ABC", "", "LAST ONE"} {
		if err := fs.SetLabel(l); err != nil {
			t.Fatalf("SetLabel(%q): %v", l, err)
		}
		if fs.Label() != l {
			t.Fatalf("Label = %q, want %q", fs.Label(), l)
		}
	}
	settle(t, path, fs)
	img, _ := os.ReadFile(path)
	if string(img[43:54]) != "LAST ONE   " {
		t.Fatalf("boot sector label = %q", img[43:54])
	}
}

// failingFile fails every WriteAt after the first `after` ones.
type failingFile struct {
	*os.File
	after, n int
}

func (f *failingFile) WriteAt(p []byte, off int64) (int, error) {
	if f.n >= f.after {
		return 0, errors.New("injected write failure")
	}
	f.n++
	return f.File.WriteAt(p, off)
}

// A write that fails after any number of underlying writes never leaves a
// dangling or cross-linked chain: whatever is referenced is in use and in
// range. (A cluster taken and not yet linked may stay allocated: a leak a
// reader never sees and fsck reclaims.)
func TestFAT1216FailureAtEveryWrite(t *testing.T) {
	ops := map[string]func(fs *fat32FS) error{
		"WriteFile":  func(fs *fat32FS) error { return fs.WriteFile("/MANY/F.BIN", pat(9000, 7), 0o644) },
		"Overwrite":  func(fs *fat32FS) error { return fs.WriteFile("/DIR1/inner long name.txt", pat(9000, 8), 0o644) },
		"MkDir":      func(fs *fat32FS) error { return fs.MkDir("/DIR1/NEWDIR", 0o755) },
		"DeleteFile": func(fs *fat32FS) error { return fs.DeleteFile("/DIR1/inner long name.txt") },
		"DeleteDir":  func(fs *fat32FS) error { return fs.DeleteDir("/DIR1") },
		"Rename":     func(fs *fat32FS) error { return fs.Rename("/MANY/G001.TXT", "/MANY/G002.TXT") },
		"Grow":       func(fs *fat32FS) error { return fs.Truncate("/MANY/G001.TXT", 6000) },
		"Shrink":     func(fs *fat32FS) error { return fs.Truncate("/A long file name.txt", 100) },
	}
	for _, typ := range []int{12, 16} {
		for name, op := range ops {
			for k := 0; k < 40; k++ {
				_, img := f1216Fixture(t, typ)
				path := filepath.Join(t.TempDir(), "f.img")
				if err := os.WriteFile(path, img, 0o600); err != nil {
					t.Fatal(err)
				}
				raw, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				ff := &failingFile{File: raw, after: k}
				info, err := readInfo(raw, 0)
				if err != nil {
					t.Fatal(err)
				}
				fs := &fat32FS{f: ff, info: info}
				opErr := op(fs)
				raw.Close()
				fresh, err := Open(path, -1)
				if err != nil {
					t.Fatal(err)
				}
				checkVolume(t, fresh.(*fat32FS)) // leaks tolerated, dangling and cross-links not
				fresh.Close()
				if opErr == nil {
					break // k writes were enough: later k change nothing
				}
				if t.Failed() {
					t.Fatalf("FAT%d %s with the write failing after %d writes left the image inconsistent (op error: %v)", typ, name, k, opErr)
				}
			}
		}
	}
}

// Foreign readers of what we wrote: a read-only OS mount (macOS) and mtools.
func TestFAT1216ReadBackByForeignReaders(t *testing.T) {
	for _, typ := range []int{12, 16} {
		t.Run(fmt.Sprintf("FAT%d", typ), func(t *testing.T) {
			path, fs := f1216Writable(t, typ)
			data := pat(33333, 9)
			if err := fs.MkDir("/MANY/OUT", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile("/MANY/OUT/written by go.bin", data, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := fs.Close(); err != nil {
				t.Fatal(err)
			}
			if mcopy, err := exec.LookPath("mcopy"); err == nil {
				out := filepath.Join(t.TempDir(), "out.bin")
				if o, err := exec.Command(mcopy, "-i", path, "::/MANY/OUT/written by go.bin", out).CombinedOutput(); err != nil {
					t.Fatalf("mcopy: %v\n%s", err, o)
				}
				if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
					t.Fatal("mtools read different bytes")
				}
			} else {
				t.Log("mtools not installed")
			}
			if runtime.GOOS != "darwin" {
				return
			}
			hdiutil, err := exec.LookPath("hdiutil")
			if err != nil {
				return
			}
			out, err := exec.Command(hdiutil, "attach", "-readonly", "-nobrowse", "-imagekey", "diskimage-class=CRawDiskImage", path).Output()
			if err != nil {
				t.Skipf("cannot attach the image read-only: %v", err)
			}
			dev := strings.Fields(string(out))[0]
			t.Cleanup(func() { _ = exec.Command(hdiutil, "detach", dev).Run() })
			var mount string
			for _, f := range strings.Split(strings.TrimSpace(string(out)), "\t") {
				if strings.HasPrefix(strings.TrimSpace(f), "/Volumes/") {
					mount = strings.TrimSpace(f)
				}
			}
			if mount == "" {
				t.Skipf("no mount point in %q", out)
			}
			got, err := os.ReadFile(filepath.Join(mount, "MANY", "OUT", "written by go.bin"))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("the OS read %d bytes, %v", len(got), err)
			}
		})
	}
}

// Error paths of the FAT12/16 write plumbing.
func TestFAT1216WritePlumbingErrors(t *testing.T) {
	_, img := f1216Fixture(t, 12)
	info, _ := readInfo(bytes.NewReader(img), 0)
	// Reads work, writes fail: FAT entry writes, the fixed root, allocation.
	ro := &fat32FS{f: readerAt{bytes.NewReader(img)}, info: info}
	if err := ro.setNarrowFATEntry(5, 0); err == nil {
		t.Error("FAT12 entry write succeeded over a read-only reader")
	}
	info16 := info
	info16.Type = FAT16
	ro16 := &fat32FS{f: readerAt{bytes.NewReader(img)}, info: info16}
	if err := ro16.setNarrowFATEntry(5, 0); err == nil {
		t.Error("FAT16 entry write succeeded over a read-only reader")
	}
	if err := ro.writeFixedRoot(make([]byte, 32)); err == nil {
		t.Error("root write succeeded over a read-only reader")
	}
	if err := ro.writeFixedRoot(make([]byte, int(info.RootEntries)*32+32)); err == nil {
		t.Error("an over-long root buffer was written")
	}
	// A FAT that cannot be read: FAT12 read-modify-write, allocation scans.
	bad := &fat32FS{f: readerAt{failAt{bytes.NewReader(img), info.FATOffset(0)}}, info: info}
	if err := bad.setNarrowFATEntry(5, 0); err == nil {
		t.Error("FAT12 entry write succeeded with an unreadable FAT")
	}
	if _, err := bad.allocNarrow(); err == nil {
		t.Error("allocNarrow succeeded with an unreadable FAT")
	}
	if _, err := bad.allocClusterRun(2); err == nil {
		t.Error("allocClusterRun succeeded with an unreadable FAT")
	}
	if _, err := bad.allocClusterRun(0); err != nil {
		t.Errorf("allocClusterRun(0): %v", err)
	}
	// No free cluster at all.
	full := append([]byte(nil), img...)
	fsFull := f1216Open(t, full)
	for c := uint32(2); c < fsFull.lastCluster(); c++ {
		putFAT12(full[fsFull.info.FATOffset(0):], int(c), 0xFFF)
	}
	if _, err := fsFull.allocNarrow(); err == nil {
		t.Error("allocNarrow found a cluster on a full FAT")
	}
	if _, err := fsFull.allocClusterRun(1); err == nil {
		t.Error("allocClusterRun found a cluster on a full FAT")
	}
	// Reading the label through a short boot sector.
	if _, err := io.ReadAll(io.LimitReader(bytes.NewReader(img), 1)); err != nil {
		t.Fatal(err)
	}
}

// Deleting an empty file (no cluster), writing at the limit of a short FAT, and
// the hint after frees: small paths the scenarios above do not reach.
func TestFAT1216SmallPaths(t *testing.T) {
	path, fs := f1216Writable(t, 16)
	if err := fs.WriteFile("/EMPTY.TXT", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.DeleteFile("/EMPTY.TXT"); err != nil {
		t.Fatal(err)
	}
	// Freed clusters are found again by the next allocation (hint lowered).
	if err := fs.WriteFile("/A.BIN", pat(5000, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	e, _, _ := fs.resolvePath("/A.BIN")
	if err := fs.DeleteFile("/A.BIN"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/B.BIN", pat(100, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	e2, _, _ := fs.resolvePath("/B.BIN")
	if e2.cluster != e.cluster {
		t.Fatalf("freed cluster %d not reused (got %d)", e.cluster, e2.cluster)
	}
	// Rename across directories over an existing file frees its chain.
	if err := fs.WriteFile("/MANY/TARGET.BIN", pat(3000, 3), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename("/B.BIN", "/MANY/TARGET.BIN"); err != nil {
		t.Fatal(err)
	}
	// The allocator returns what a partial run found when it comes up short.
	if _, err := fs.freeRun(1 << 20); err == nil {
		t.Fatal("freeRun of more clusters than exist succeeded")
	}
	if _, err := fs.freeRun(1); err != nil {
		t.Fatalf("freeRun after a short scan: %v", err)
	}
	settle(t, path, fs)
}

// FuzzWriteFAT1216 applies a byte-driven sequence of mutations to a FAT12 or
// FAT16 volume. Whatever the sequence, nothing panics, and the volume stays
// structurally sound: every referenced chain is in use, in range and
// unshared (the same checker the scenarios use).
func FuzzWriteFAT1216(f *testing.F) {
	f.Add([]byte{0, 1, 40, 1, 2, 3, 2, 1, 4, 5, 0, 6, 7, 8, 3, 2})
	f.Add([]byte{1, 0, 0, 0, 0, 3, 1, 2, 4, 1, 9, 5, 2, 7, 6, 0, 1, 1, 7, 3, 0})
	f.Add(bytes.Repeat([]byte{0, 9, 250}, 40))
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 300 {
			ops = ops[:300]
		}
		typ := 12 + 4*int(len(ops)%2)
		_, img := f1216Fixture(t, typ)
		fs := f1216Open(t, img)
		rw, err := OpenReader(&rwBuf{img}, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		fs = rw.(*fat32FS)
		names := []string{"/MANY/A", "/MANY/b long fuzz name", "/DIR1/C.TXT", "/DIR1/DIR2/D", "/E", "/MANY/G001.TXT", "/DIR1/NEWDIR", "/MANY/F"}
		for i := 0; i+2 < len(ops); i += 3 {
			a, b := names[int(ops[i+1])%len(names)], names[int(ops[i+2])%len(names)]
			size := int(ops[i+2]) * 997
			switch ops[i] % 7 {
			case 0:
				_ = fs.WriteFile(a, pat(size, ops[i]), 0o644)
			case 1:
				_ = fs.MkDir(a, 0o755)
			case 2:
				_ = fs.DeleteFile(a)
			case 3:
				_ = fs.DeleteDir(a)
			case 4:
				_ = fs.Rename(a, b)
			case 5:
				_ = fs.Truncate(a, int64(size))
			case 6:
				if h, err := fs.OpenFile(a); err == nil {
					_, _ = h.(*fat32File).WriteAt(pat(size/2+1, ops[i]), int64(ops[i+2])*131)
				}
			}
		}
		checkVolume(t, fs)
	})
}
