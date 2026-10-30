package filesystem_fat32

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
)

// newMemFS formats an in-memory volume and returns it with its backing image.
func newMemFS(t *testing.T) (*fat32FS, *memImage) {
	t.Helper()
	img := &memImage{b: make([]byte, fat32TestSize)}
	f, err := FormatImage(img, fat32TestSize, FormatConfig{Label: "FSINFO", VolumeID: 1})
	if err != nil {
		t.Fatalf("FormatImage: %v", err)
	}
	return f.(*fat32FS), img
}

// fatFreeCount counts free FAT entries by an independent walk of the first FAT
// (it deliberately does not call the driver's own scan).
func fatFreeCount(t *testing.T, fs *fat32FS) int64 {
	t.Helper()
	fatBase := fs.info.FATOffset(fs.partOffset)
	data := int64(fs.info.TotalSectors) - int64(fs.info.ReservedSectors) -
		int64(fs.info.FATCount)*int64(fs.info.FATSize)
	n := data / int64(fs.info.SectorsPerCluster)
	var free int64
	var b [4]byte
	for c := int64(2); c < n+2; c++ {
		if _, err := fs.f.ReadAt(b[:], fatBase+c*4); err != nil {
			t.Fatalf("read FAT entry %d: %v", c, err)
		}
		if binary.LittleEndian.Uint32(b[:])&0x0FFFFFFF == 0 {
			free++
		}
	}
	return free
}

// fsInfoFields reads FSI_Free_Count and FSI_Nxt_Free from the sector.
func fsInfoFields(t *testing.T, fs *fat32FS) (free, next uint32) {
	t.Helper()
	sec := make([]byte, fs.info.BytesPerSector)
	off := fs.partOffset + int64(fs.info.FSInfoSector)*int64(fs.info.BytesPerSector)
	if _, err := fs.f.ReadAt(sec, off); err != nil {
		t.Fatalf("read FSInfo: %v", err)
	}
	return binary.LittleEndian.Uint32(sec[488:]), binary.LittleEndian.Uint32(sec[492:])
}

func checkFSInfo(t *testing.T, fs *fat32FS, when string) {
	t.Helper()
	want := fatFreeCount(t, fs)
	free, next := fsInfoFields(t, fs)
	if int64(free) != want {
		t.Fatalf("%s: FSI_Free_Count = %#x, free FAT entries = %d", when, free, want)
	}
	if next != fsInfoUnknown && (next < 2 || int64(next) >= fs.dataClusters()+2) {
		t.Fatalf("%s: FSI_Nxt_Free = %d is outside the volume", when, next)
	}
}

// dirFirstCluster returns the first cluster of the directory at path.
func dirFirstCluster(t *testing.T, fs *fat32FS, path string) uint32 {
	t.Helper()
	name, parent, err := fs.getParentDir(path)
	if err != nil {
		t.Fatalf("getParentDir(%q): %v", path, err)
	}
	buf, err := fs.readDirBuf(parent)
	if err != nil {
		t.Fatalf("readDirBuf: %v", err)
	}
	start, count, ok := fat32FindEntry(buf, name)
	if !ok {
		t.Fatalf("%q not found", path)
	}
	e := buf[start+(count-1)*dirEntrySize:]
	return uint32(binary.LittleEndian.Uint16(e[20:]))<<16 | uint32(binary.LittleEndian.Uint16(e[26:]))
}

// dotDotOf reads the raw ".." entry of the directory starting at cluster.
func dotDotOf(t *testing.T, fs *fat32FS, cluster uint32) uint32 {
	t.Helper()
	off := fs.info.DataOffset(fs.partOffset) + int64(cluster-2)*int64(fs.info.ClusterSize()) + dirEntrySize
	var e [dirEntrySize]byte
	if _, err := fs.f.ReadAt(e[:], off); err != nil {
		t.Fatalf("read dotdot: %v", err)
	}
	if string(e[:11]) != "..         " {
		t.Fatalf("second entry is %q, not ..", e[:11])
	}
	return uint32(binary.LittleEndian.Uint16(e[20:]))<<16 | uint32(binary.LittleEndian.Uint16(e[26:]))
}

// fatgen103 6.5: ".." of a child of the root is 0; of a deeper directory, the
// parent's first cluster.
func TestDotDotCluster(t *testing.T) {
	fs, _ := newMemFS(t)
	for _, d := range []string{"/docs", "/docs/sub", "/images"} {
		if err := fs.MkDir(d, 0o755); err != nil {
			t.Fatalf("MkDir(%s): %v", d, err)
		}
	}
	if got := dotDotOf(t, fs, dirFirstCluster(t, fs, "/docs")); got != 0 {
		t.Errorf("/docs '..' = %d, want 0 (parent is the root)", got)
	}
	if got := dotDotOf(t, fs, dirFirstCluster(t, fs, "/images")); got != 0 {
		t.Errorf("/images '..' = %d, want 0", got)
	}
	docs := dirFirstCluster(t, fs, "/docs")
	if got := dotDotOf(t, fs, dirFirstCluster(t, fs, "/docs/sub")); got != docs {
		t.Errorf("/docs/sub '..' = %d, want %d", got, docs)
	}
}

// A directory moved between parents must have its ".." follow.
func TestRenameDirectoryRewritesDotDot(t *testing.T) {
	fs, _ := newMemFS(t)
	for _, d := range []string{"/a", "/a/b", "/c"} {
		if err := fs.MkDir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.Rename("/a/b", "/c/b"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got, want := dotDotOf(t, fs, dirFirstCluster(t, fs, "/c/b")), dirFirstCluster(t, fs, "/c"); got != want {
		t.Errorf("after move under /c, '..' = %d, want %d", got, want)
	}
	if err := fs.Rename("/c/b", "/b"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := dotDotOf(t, fs, dirFirstCluster(t, fs, "/b")); got != 0 {
		t.Errorf("after move to the root, '..' = %d, want 0", got)
	}
}

// Images written by older versions hold the root's cluster in "..". They must
// still be readable and writable.
func TestDotDotRootClusterStillAccepted(t *testing.T) {
	fs, _ := newMemFS(t)
	if err := fs.MkDir("/old", 0o755); err != nil {
		t.Fatal(err)
	}
	c := dirFirstCluster(t, fs, "/old")
	off := fs.info.DataOffset(fs.partOffset) + int64(c-2)*int64(fs.info.ClusterSize()) + dirEntrySize + 26
	var v [2]byte
	binary.LittleEndian.PutUint16(v[:], uint16(fs.info.RootCluster))
	if _, err := fs.f.WriteAt(v[:], off); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/old/x.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile in old-style directory: %v", err)
	}
	if got, err := fs.ReadFile("/old/x.txt"); err != nil || string(got) != "x" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if _, err := fs.ListDir("/old"); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
}

// FSInfo is exact after format and after every kind of operation, without
// needing Close, and Close leaves it exact too.
func TestFSInfoTracksEveryOperation(t *testing.T) {
	fs, _ := newMemFS(t)
	checkFSInfo(t, fs, "after format")
	steps := []struct {
		name string
		do   func() error
	}{
		{"MkDir", func() error { return fs.MkDir("/d", 0o755) }},
		{"WriteFile small", func() error { return fs.WriteFile("/d/a.txt", []byte("hi"), 0o644) }},
		{"WriteFile large", func() error { return fs.WriteFile("/big.bin", bytes.Repeat([]byte{7}, 300_000), 0o644) }},
		{"Truncate shrink", func() error { return fs.Truncate("/big.bin", 1000) }},
		{"Truncate grow", func() error { return fs.Truncate("/big.bin", 100_000) }},
		{"Rename over existing", func() error { return fs.Rename("/big.bin", "/d/a.txt") }},
		{"DeleteFile", func() error { return fs.DeleteFile("/d/a.txt") }},
		{"WriteFile again", func() error { return fs.WriteFile("/e.bin", bytes.Repeat([]byte{1}, 70_000), 0o644) }},
		{"DeleteDir", func() error { return fs.DeleteDir("/d") }},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		checkFSInfo(t, fs, "after "+s.name)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkFSInfo(t, fs, "after Close")
}

// The file-handle write path (OpenFile + WriteAt) goes through setFATEntry too.
func TestFSInfoTracksFileHandleWrites(t *testing.T) {
	fs, _ := newMemFS(t)
	if err := fs.WriteFile("/f.bin", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := fs.OpenFile("/f.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	w, ok := f.(interface {
		WriteAt([]byte, int64) (int, error)
	})
	if !ok {
		t.Skip("handle has no WriteAt")
	}
	if _, err := w.WriteAt(bytes.Repeat([]byte{9}, 50_000), 0); err != nil {
		t.Fatal(err)
	}
	checkFSInfo(t, fs, "after handle WriteAt")
	f.Close()
}

// A write that fails half-way must not leave a wrong count behind: after the
// next successful operation the count is exact again.
func TestFSInfoSurvivesFailedWrite(t *testing.T) {
	fs, img := newMemFS(t)
	if err := fs.WriteFile("/ok.bin", bytes.Repeat([]byte{1}, 40_000), 0o644); err != nil {
		t.Fatal(err)
	}
	checkFSInfo(t, fs, "before failure")
	for _, skip := range []int{1, 3, 8, 20} {
		img.failAt = img.writes + skip
		_ = fs.WriteFile("/fail.bin", bytes.Repeat([]byte{2}, 90_000), 0o644)
		img.failAt = 0
		if err := fs.WriteFile(fmt.Sprintf("/after%d.bin", skip), []byte("z"), 0o644); err != nil {
			t.Fatalf("recovery write: %v", err)
		}
		// Leaked clusters from the failed write are allowed (fsck repairs
		// them); a count that disagrees with the FAT is not.
		checkFSInfo(t, fs, fmt.Sprintf("after failure at write +%d", skip))
	}
}

// The count and hint stored in an image are never believed.
func TestFSInfoForgedValuesAreNotTrusted(t *testing.T) {
	fs, img := newMemFS(t)
	off := int(fs.partOffset) + int(fs.info.FSInfoSector)*int(fs.info.BytesPerSector)
	binary.LittleEndian.PutUint32(img.b[off+488:], 0)          // "no free clusters"
	binary.LittleEndian.PutUint32(img.b[off+492:], 0x7FFFFFFF) // far outside the volume
	re, err := OpenReader(img, fat32TestSize)
	if err != nil {
		t.Fatal(err)
	}
	fs = re.(*fat32FS)
	// Allocation must still succeed (the forged "0 free" is ignored)...
	if err := fs.WriteFile("/x.bin", bytes.Repeat([]byte{3}, 20_000), 0o644); err != nil {
		t.Fatalf("WriteFile on an image whose FSInfo says full: %v", err)
	}
	// ...and the sector is rewritten with the truth and an in-range hint.
	checkFSInfo(t, fs, "after forged FSInfo")
	if _, next := fsInfoFields(t, fs); next == 0x7FFFFFFF {
		t.Fatal("forged FSI_Nxt_Free survived")
	}
}

// A volume without a valid FSInfo sector is left alone.
func TestFSInfoAbsentIsLeftAlone(t *testing.T) {
	fs, img := newMemFS(t)
	off := int(fs.partOffset) + int(fs.info.FSInfoSector)*int(fs.info.BytesPerSector)
	binary.LittleEndian.PutUint32(img.b[off:], 0) // wreck the lead signature
	before := append([]byte(nil), img.b[off:off+512]...)
	if err := fs.WriteFile("/x.bin", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, img.b[off:off+512]) {
		t.Fatal("a sector that is not an FSInfo was modified")
	}
}

// Read-only use never writes the FSInfo.
func TestFSInfoReadOnlyOpenWritesNothing(t *testing.T) {
	fs, img := newMemFS(t)
	if err := fs.WriteFile("/x.bin", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	writes := img.writes
	re, err := OpenReader(img, fat32TestSize)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = re.ReadFile("/x.bin")
	_, _ = re.ListDir("/")
	_ = re.Close()
	if img.writes != writes {
		t.Fatalf("read-only use performed %d writes", img.writes-writes)
	}
}

// The canonical fsck, when installed, must find nothing to say about an image
// with directories under the root, nested directories, and files.
func TestFsckCleanWithDirectories(t *testing.T) {
	fsck, ok := lookupFsck()
	if !ok {
		t.Skip("canonical FAT fsck not found (need dosfstools `fsck.vfat` or macOS `fsck_msdos`)")
	}
	path := filepath.Join(t.TempDir(), "dirs.fat32")
	fs, err := Format(path, 8<<20, FormatConfig{Label: "FSCK"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"/docs", "/images", "/empty", "/docs/deep"} {
		if err := fs.MkDir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, n := range map[string]int{"/README.txt": 100, "/docs/a.txt": 5000, "/docs/deep/b.bin": 70_000, "/images/c.bin": 9000} {
		if err := fs.WriteFile(p, bytes.Repeat([]byte{'x'}, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.DeleteFile("/images/c.bin"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename("/docs/deep", "/deep"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(fsck, "-n", path).CombinedOutput()
	if err != nil {
		t.Fatalf("%s -n: %v\n%s", filepath.Base(fsck), err, out)
	}
	t.Logf("%s -n:\n%s", filepath.Base(fsck), out)
}
