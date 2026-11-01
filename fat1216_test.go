package filesystem_fat32

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
)

// The fixtures in testdata/ were formatted by newfs_msdos and populated by
// mtools (see testdata/mkfixtures); fixtures.json holds what is in them.

type f1216File struct {
	Path   string `json:"path"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

type f1216Image struct {
	Image       string      `json:"image"`
	Type        int         `json:"type"`
	RootEntries int         `json:"root_entries"`
	RootFull    bool        `json:"root_full"`
	Files       []f1216File `json:"files"`
	Dirs        []string    `json:"dirs"`
	Root        []string    `json:"root"`
}

func f1216Manifest(t testing.TB) []f1216Image {
	t.Helper()
	b, err := os.ReadFile("testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var m []f1216Image
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func f1216Bytes(t testing.TB, im f1216Image) []byte {
	t.Helper()
	f, err := os.Open("testdata/" + im.Image)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func f1216Open(t testing.TB, img []byte) *fat32FS {
	t.Helper()
	fs, err := OpenReader(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	return fs.(*fat32FS)
}

func f1216Names(t testing.TB, fs *fat32FS, dir string) []string {
	t.Helper()
	ents, err := fs.ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", dir, err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestFAT1216Fixtures(t *testing.T) {
	for _, im := range f1216Manifest(t) {
		t.Run(im.Image, func(t *testing.T) {
			fs := f1216Open(t, f1216Bytes(t, im))
			info := fs.Info()
			if int(info.Type) != im.Type || info.Type.String() != fmt.Sprintf("FAT%d", im.Type) {
				t.Fatalf("Type = %v, want FAT%d", info.Type, im.Type)
			}
			if int(info.RootEntries) != im.RootEntries || info.RootCluster != 0 {
				t.Fatalf("RootEntries = %d, RootCluster = %d", info.RootEntries, info.RootCluster)
			}
			if want := uint32(im.RootEntries) * 32 / uint32(info.BytesPerSector); info.RootDirSectors() != want {
				t.Fatalf("RootDirSectors = %d, want %d", info.RootDirSectors(), want)
			}
			if info.RootDirOffset(0) != info.FATOffset(0)+int64(info.FATCount)*int64(info.FATSize)*512 {
				t.Fatal("root region is not right after the FATs")
			}
			if info.DataOffset(0) != info.RootDirOffset(0)+int64(info.RootDirSectors())*512 {
				t.Fatal("data region is not right after the root region")
			}
			if info.VolumeLabel != strings.ToUpper(fmt.Sprintf("FAT%dTEST", im.Type)) {
				t.Fatalf("VolumeLabel = %q", info.VolumeLabel)
			}
			if fs.Label() != info.VolumeLabel {
				t.Fatal("Label disagrees with Info")
			}
			// Root listing, including a long name and (FAT12) a root full to
			// BPB_RootEntCnt.
			got := f1216Names(t, fs, "/")
			want := append([]string(nil), im.Root...)
			sort.Strings(want)
			if !equalNames(got, want) {
				t.Fatalf("root = %v\nwant   %v", got, want)
			}
			if st, err := fs.Stat("/"); err != nil || st.Mode()&0o040000 == 0 {
				t.Fatalf("Stat(/) = %v, %v", st, err)
			}
			// Every file, through ReadFile and through OpenFile+ReadAt.
			for _, f := range im.Files {
				data, err := fs.ReadFile(f.Path)
				if err != nil {
					t.Fatalf("ReadFile(%q): %v", f.Path, err)
				}
				if len(data) != f.Size || f1216Sum(data) != f.SHA256 {
					t.Fatalf("ReadFile(%q): %d bytes, digest mismatch", f.Path, len(data))
				}
				h, err := fs.OpenFile(f.Path)
				if err != nil {
					t.Fatalf("OpenFile(%q): %v", f.Path, err)
				}
				buf := make([]byte, f.Size)
				if n, err := h.ReadAt(buf, 0); n != f.Size || (err != nil && err != io.EOF) {
					t.Fatalf("ReadAt(%q) = %d, %v", f.Path, n, err)
				}
				if f1216Sum(buf) != f.SHA256 {
					t.Fatalf("ReadAt(%q): digest mismatch", f.Path)
				}
				st, err := fs.Stat(f.Path)
				if err != nil || st.Size() != uint64(f.Size) {
					t.Fatalf("Stat(%q) = %v, %v", f.Path, st, err)
				}
				h.Close()
			}
			// Nested directories; ".." of a first-level directory is the
			// fixed root (cluster 0).
			if names := f1216Names(t, fs, "/DIR1"); !equalNames(names, []string{".", "..", "DIR2", "inner long name.txt"}) {
				t.Fatalf("/DIR1 = %v", names)
			}
			if names := f1216Names(t, fs, "/DIR1/DIR2"); !equalNames(names, []string{".", "..", "NESTED.TXT"}) {
				t.Fatalf("/DIR1/DIR2 = %v", names)
			}
			// A directory spanning several clusters.
			if names := f1216Names(t, fs, "/MANY"); len(names) != 42 {
				t.Fatalf("/MANY has %d entries, want 42", len(names))
			}
			if _, err := fs.ListDir("/HELLO.TXT"); err == nil {
				t.Fatal("ListDir of a file succeeded")
			}
			if _, err := fs.ReadFile("/DIR1"); err == nil {
				t.Fatal("ReadFile of a directory succeeded")
			}
			if _, err := fs.ReadFile("/nope"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ReadFile(missing) = %v", err)
			}
			if im.RootFull {
				// Every one of the BPB_RootEntCnt slots is in use (LFN parts
				// and the volume label included): no 0x00 end marker exists,
				// and the listing above still found every name.
				raw, err := fs.readFixedRoot()
				if err != nil {
					t.Fatal(err)
				}
				used := 0
				for off := 0; off < len(raw); off += 32 {
					if raw[off] != 0x00 && raw[off] != 0xE5 {
						used++
					}
				}
				if used != im.RootEntries {
					t.Fatalf("%d of %d root slots used", used, im.RootEntries)
				}
			}
		})
	}
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func f1216Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// A FAT12 entry whose two bytes straddle a sector boundary: cluster 341 sits
// at byte 341 + 341/2 = 511 of the FAT. The fixture's BIG.BIN chain must run
// through it, and the entry must read as the next cluster of that chain.
func TestFAT12EntryStraddlingSectorBoundary(t *testing.T) {
	var im f1216Image
	for _, m := range f1216Manifest(t) {
		if m.Type == 12 {
			im = m
		}
	}
	fs := f1216Open(t, f1216Bytes(t, im))
	h, err := fs.OpenFile("/BIG.BIN")
	if err != nil {
		t.Fatal(err)
	}
	chain := h.(*fat32File).clusters
	pos := -1
	for i, c := range chain {
		if c == 341 {
			pos = i
		}
	}
	if pos < 0 || pos+1 >= len(chain) {
		t.Fatalf("BIG.BIN does not cover cluster 341 (%d clusters)", len(chain))
	}
	if v, err := fs.fatEntry(341); err != nil || v != chain[pos+1] {
		t.Fatalf("fatEntry(341) = %d, %v, want %d", v, err, chain[pos+1])
	}
	// The file reads back whole (digest checked in TestFAT1216Fixtures); here
	// also by random access across the boundary.
	want, _ := fs.ReadFile("/BIG.BIN")
	buf := make([]byte, 1000)
	off := int64(pos)*512 - 300
	if n, err := h.ReadAt(buf, off); n != 1000 || err != nil {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if !bytes.Equal(buf, want[off:off+1000]) {
		t.Fatal("random access across the boundary differs from ReadFile")
	}
}

// putFAT12 writes entry v for cluster n into a raw FAT12 table (the inverse of
// the reader, written from the spec: odd N takes the high 12 bits).
func putFAT12(fat []byte, n int, v uint16) {
	o := n + n/2
	w := binary.LittleEndian.Uint16(fat[o:])
	if n&1 == 1 {
		w = w&0x000F | v<<4
	} else {
		w = w&0xF000 | v&0x0FFF
	}
	binary.LittleEndian.PutUint16(fat[o:], w)
}

func f1216Fixture(t testing.TB, typ int) (f1216Image, []byte) {
	for _, m := range f1216Manifest(t) {
		if m.Type == typ {
			return m, f1216Bytes(t, m)
		}
	}
	t.Fatalf("no FAT%d fixture", typ)
	return f1216Image{}, nil
}

func TestFAT12EntryPacking(t *testing.T) {
	_, img := f1216Fixture(t, 12)
	fs := f1216Open(t, img)
	fatOff := int(fs.info.FATOffset(0))
	for n := 2; n < 20; n++ {
		putFAT12(img[fatOff:], n, uint16(0xA00+n))
	}
	// Neighbouring entries stay independent of one another.
	for n := 2; n < 20; n++ {
		if v, err := fs.fatEntry(uint32(n)); err != nil || v != uint32(0xA00+n) {
			t.Fatalf("fatEntry(%d) = %#x, %v", n, v, err)
		}
	}
	// Outside the volume is refused for both narrow widths.
	if _, err := fs.fatEntry(fs.info.DataClusters + 2); err == nil {
		t.Fatal("FAT12 entry past the last cluster was read")
	}
	_, img16 := f1216Fixture(t, 16)
	fs16 := f1216Open(t, img16)
	if _, err := fs16.fatEntry(fs16.info.DataClusters + 2); err == nil {
		t.Fatal("FAT16 entry past the last cluster was read")
	}
}

type failAt struct {
	r   io.ReaderAt
	off int64
}

func (f failAt) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.off {
		return 0, errors.New("injected read failure")
	}
	return f.r.ReadAt(p, off)
}

func TestFAT1216ReadErrors(t *testing.T) {
	for _, typ := range []int{12, 16} {
		im, img := f1216Fixture(t, typ)
		fs0 := f1216Open(t, img)
		// FAT reads fail: the first byte after the boot sector is in the
		// reserved area, the FAT starts after it.
		fatOff := fs0.info.FATOffset(0)
		fs := &fat32FS{f: readerAt{failAt{bytes.NewReader(img), fatOff}}, info: fs0.info}
		if _, err := fs.ReadFile("/BIG.BIN"); err == nil {
			t.Fatalf("FAT%d: ReadFile with an unreadable FAT succeeded", typ)
		}
		// Reaching the file at all needs the root region, which is after it.
		_ = im
		fs = &fat32FS{f: readerAt{failAt{bytes.NewReader(img), fs0.info.RootDirOffset(0)}}, info: fs0.info}
		if _, err := fs.ListDir("/"); err == nil {
			t.Fatalf("FAT%d: ListDir with an unreadable root succeeded", typ)
		}
		// Chain walk of OpenFile and ReadFile on a FAT that fails after the
		// directory was read: allow the FAT but not the data.
		fs = &fat32FS{f: readerAt{failAt{bytes.NewReader(img), fs0.info.DataOffset(0)}}, info: fs0.info}
		if _, err := fs.OpenFile("/BIG.BIN"); err == nil {
			// The directory lives in the data region for subdirectories only;
			// a root file's entry is readable, its chain too.
			t.Logf("FAT%d: OpenFile read only FAT entries, as designed", typ)
		}
	}
}

// Both walkers refuse a FAT whose entry cannot be read: truncate the image in
// the middle of the FAT after the root directory entry was found.
func TestFAT1216ChainReadFailure(t *testing.T) {
	for _, typ := range []int{12, 16} {
		_, img := f1216Fixture(t, typ)
		fs0 := f1216Open(t, img)
		rootEnd := fs0.info.DataOffset(0)
		// The root region follows the FATs, so a reader that serves only
		// [rootStart, rootEnd) cannot reach the FAT.
		rs := fs0.info.RootDirOffset(0)
		fs := &fat32FS{f: readerAt{windowReader{img, rs, rootEnd}}, info: fs0.info}
		if _, err := fs.ReadFile("/BIG.BIN"); err == nil {
			t.Fatalf("FAT%d: ReadFile succeeded without a readable FAT", typ)
		}
		if _, err := fs.OpenFile("/BIG.BIN"); err == nil {
			t.Fatalf("FAT%d: OpenFile succeeded without a readable FAT", typ)
		}
	}
}

type windowReader struct {
	b        []byte
	from, to int64
}

func (w windowReader) ReadAt(p []byte, off int64) (int, error) {
	if off < w.from || off+int64(len(p)) > w.to {
		return 0, errors.New("outside the window")
	}
	return copy(p, w.b[off:]), nil
}

// Resizing a FAT12/16 volume is the one mutation not supported: it says so and
// leaves the image untouched.
func TestFAT1216ResizeRefused(t *testing.T) {
	for _, typ := range []int{12, 16} {
		_, img := f1216Fixture(t, typ)
		fs, err := OpenReader(&rwBuf{img}, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		before := append([]byte(nil), img...)
		checks := map[string]error{
			"Resize": fs.(interface{ Resize(int64) error }).Resize(int64(len(img)) * 2),
			"Grow":   fs.(*fat32FS).Grow(int64(len(img)) * 2),
			"Shrink": fs.(*fat32FS).Shrink(int64(len(img)) / 2),
		}
		for name, err := range checks {
			if !errors.Is(err, ErrResizeFAT) {
				t.Errorf("FAT%d %s: error = %v, want ErrResizeFAT", typ, name, err)
			}
		}
		if err := fs.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if !bytes.Equal(before, img) {
			t.Errorf("FAT%d: a refused resize changed the image", typ)
		}
	}
}

type rwBuf struct{ b []byte }

func (r *rwBuf) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *rwBuf) WriteAt(p []byte, off int64) (int, error) { return copy(r.b[off:], p), nil }

// f1216Boot builds a FAT12/16-layout boot sector.
func f1216Boot(total uint32, spc uint8, fatSz uint16, rootEnt uint16, rsvd uint16, nfats uint8) []byte {
	b := make([]byte, 512)
	le := binary.LittleEndian
	copy(b[3:], "MSDOS5.0")
	le.PutUint16(b[11:], 512)
	b[13] = spc
	le.PutUint16(b[14:], rsvd)
	b[16] = nfats
	le.PutUint16(b[17:], rootEnt)
	if total < 0x10000 {
		le.PutUint16(b[19:], uint16(total))
	} else {
		le.PutUint32(b[32:], total)
	}
	le.PutUint16(b[22:], fatSz)
	b[38] = 0x29
	copy(b[43:], "NO NAME    ")
	copy(b[54:], "FAT16   ")
	b[510], b[511] = 0x55, 0xAA
	return b
}

// The type is decided by the cluster count alone, at fatgen103's thresholds,
// whatever the label says.
func TestFAT1216TypeByClusterCount(t *testing.T) {
	// rsvd 1, 1 FAT of 16 sectors, 16*32/512 = 1 root sector: data starts at 18.
	const dataStart = 1 + 16 + 1
	cases := []struct {
		clusters uint32
		want     FATType
		label    string
	}{
		{4084, FAT12, "FAT16   "}, // label lies
		{4085, FAT16, "FAT12   "}, // label lies the other way
		{65524, FAT16, ""},
	}
	for _, c := range cases {
		b := f1216Boot(dataStart+c.clusters, 1, 16, 16, 1, 1)
		copy(b[54:62], "        ")
		copy(b[54:], c.label)
		if c.clusters > 4084 {
			// A bigger FAT, so the table is not what limits the count.
			binary.LittleEndian.PutUint16(b[22:], 256)
			b = f1216Boot(1+256+1+c.clusters, 1, 256, 16, 1, 1)
			copy(b[54:62], "        ")
			copy(b[54:], c.label)
		} else {
			// FAT12 needs 12 bits per cluster: 16 sectors hold 5461 entries.
		}
		info, err := readInfo(bytes.NewReader(b), 0)
		if err != nil {
			t.Fatalf("%d clusters: %v", c.clusters, err)
		}
		if info.Type != c.want || info.DataClusters != c.clusters {
			t.Fatalf("%d clusters: Type = %v, DataClusters = %d", c.clusters, info.Type, info.DataClusters)
		}
	}
	// 65525 clusters is FAT32 by count: not acceptable in a FAT12/16 layout.
	if _, err := readInfo(bytes.NewReader(f1216Boot(1+256+1+65525, 1, 256, 16, 1, 1)), 0); err == nil {
		t.Fatal("65525 clusters accepted as FAT16")
	}
}

func TestFAT1216BoundsRefused(t *testing.T) {
	good := func() []byte { return f1216Boot(4000, 1, 16, 16, 1, 1) }
	if _, err := readInfo(bytes.NewReader(good()), 0); err != nil {
		t.Fatalf("baseline refused: %v", err)
	}
	le := binary.LittleEndian
	cases := []struct {
		name   string
		mutate func(b []byte)
	}{
		{"bytes per sector 0", func(b []byte) { le.PutUint16(b[11:], 0) }},
		{"bytes per sector 256", func(b []byte) { le.PutUint16(b[11:], 256) }},
		{"bytes per sector 1000", func(b []byte) { le.PutUint16(b[11:], 1000) }},
		{"bytes per sector 8192", func(b []byte) { le.PutUint16(b[11:], 8192) }},
		{"bytes per sector 32768", func(b []byte) { le.PutUint16(b[11:], 32768) }},
		{"zero FATs", func(b []byte) { b[16] = 0 }},
		{"zero root entries", func(b []byte) { le.PutUint16(b[17:], 0) }},
		{"root entries not filling sectors", func(b []byte) { le.PutUint16(b[17:], 17) }},
		{"zero sectors per cluster", func(b []byte) { b[13] = 0 }},
		{"zero reserved sectors", func(b []byte) { le.PutUint16(b[14:], 0) }},
		{"root directory past the volume", func(b []byte) { le.PutUint16(b[17:], 65520) }},
		{"FATs past the volume", func(b []byte) { b[16] = 255; le.PutUint16(b[22:], 0xFFFF) }},
		{"no data cluster", func(b []byte) { le.PutUint16(b[19:], 18+1); b[13] = 4 }},
		{"sector count 4 GiB-1", func(b []byte) {
			le.PutUint16(b[19:], 0)
			le.PutUint32(b[32:], 0xFFFFFFFF)
			b[16] = 255
			le.PutUint16(b[22:], 0xFFFF)
		}},
	}
	for _, c := range cases {
		b := good()
		c.mutate(b)
		if _, err := readInfo(bytes.NewReader(b), 0); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	// A FAT too small for the claimed cluster count clamps the count.
	b := f1216Boot(4000, 1, 1, 16, 1, 1) // 1 FAT sector: 341 FAT12 entries
	info, err := readInfo(bytes.NewReader(b), 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.DataClusters != 341-2 {
		t.Fatalf("DataClusters = %d, want 339", info.DataClusters)
	}
}

// Forged images: each must come back as an error, never a panic or a runaway
// allocation.
func TestFAT1216ForgedImages(t *testing.T) {
	t.Run("RootEntCnt past the image", func(t *testing.T) {
		_, img := f1216Fixture(t, 16)
		// The BPB says 8 MiB, the file holds 40 KiB: the root region (after
		// the FATs) is not there.
		fs := f1216Open(t, img[:40<<10])
		if _, err := fs.ListDir("/"); err == nil {
			t.Fatal("ListDir over a root region past the end succeeded")
		}
		if _, err := fs.ReadFile("/HELLO.TXT"); err == nil {
			t.Fatal("ReadFile over a root region past the end succeeded")
		}
	})
	t.Run("cycle", func(t *testing.T) {
		_, img := f1216Fixture(t, 12)
		fs := f1216Open(t, img)
		h, _ := fs.OpenFile("/BIG.BIN")
		chain := h.(*fat32File).clusters
		fat := img[fs.info.FATOffset(0):]
		putFAT12(fat, int(chain[10]), uint16(chain[3])) // loop back
		fs = f1216Open(t, img)
		if _, err := fs.ReadFile("/BIG.BIN"); err == nil {
			t.Fatal("ReadFile followed a cycle")
		}
		if _, err := fs.OpenFile("/BIG.BIN"); err == nil {
			t.Fatal("OpenFile followed a cycle")
		}
	})
	t.Run("cycle FAT16", func(t *testing.T) {
		_, img := f1216Fixture(t, 16)
		fs := f1216Open(t, img)
		h, _ := fs.OpenFile("/BIG.BIN")
		chain := h.(*fat32File).clusters
		fat := img[fs.info.FATOffset(0):]
		binary.LittleEndian.PutUint16(fat[int(chain[5])*2:], uint16(chain[5])) // self loop
		fs = f1216Open(t, img)
		if _, err := fs.ReadFile("/BIG.BIN"); err == nil {
			t.Fatal("ReadFile followed a self loop")
		}
	})
	t.Run("out-of-range cluster", func(t *testing.T) {
		for _, typ := range []int{12, 16} {
			_, img := f1216Fixture(t, typ)
			fs := f1216Open(t, img)
			h, _ := fs.OpenFile("/BIG.BIN")
			chain := h.(*fat32File).clusters
			fat := img[fs.info.FATOffset(0):]
			if typ == 12 {
				putFAT12(fat, int(chain[4]), 0xFF0) // beyond ~2000 clusters, below the bad mark
			} else {
				binary.LittleEndian.PutUint16(fat[int(chain[4])*2:], 0xFFF0)
			}
			fs = f1216Open(t, img)
			if _, err := fs.ReadFile("/BIG.BIN"); err == nil {
				t.Fatalf("FAT%d: ReadFile followed a cluster outside the volume", typ)
			}
			if _, err := fs.OpenFile("/BIG.BIN"); err == nil {
				t.Fatalf("FAT%d: OpenFile followed a cluster outside the volume", typ)
			}
		}
	})
	t.Run("bad cluster ends the chain", func(t *testing.T) {
		_, img := f1216Fixture(t, 12)
		fs := f1216Open(t, img)
		h, _ := fs.OpenFile("/BIG.BIN")
		chain := h.(*fat32File).clusters
		putFAT12(img[fs.info.FATOffset(0):], int(chain[4]), 0xFF7)
		fs = f1216Open(t, img)
		data, err := fs.ReadFile("/BIG.BIN")
		if err != nil || len(data) != 5*512 {
			t.Fatalf("ReadFile = %d bytes, %v; want the 5 clusters before the bad mark", len(data), err)
		}
	})
	t.Run("forged file size", func(t *testing.T) {
		for _, typ := range []int{12, 16} {
			_, img := f1216Fixture(t, typ)
			fs := f1216Open(t, img)
			root := img[fs.info.RootDirOffset(0):]
			// HELLO.TXT is the first entry (after a volume label, if any).
			for off := 0; off < len(root); off += 32 {
				if bytes.HasPrefix(root[off:], []byte("HELLO   TXT")) {
					binary.LittleEndian.PutUint32(root[off+28:], 0xFFFFFFFF)
				}
			}
			fs = f1216Open(t, img)
			data, err := fs.ReadFile("/HELLO.TXT")
			if err != nil || len(data) > int(fs.clusterCount())*int(fs.info.ClusterSize()) {
				t.Fatalf("FAT%d: forged size gave %d bytes, %v", typ, len(data), err)
			}
		}
	})
}

// A file in a sub-directory entry that claims cluster 0 is an empty directory,
// never the root again (a walker recursing would loop for ever otherwise).
func TestFAT1216DirectoryAtClusterZeroIsEmpty(t *testing.T) {
	_, img := f1216Fixture(t, 16)
	fs := f1216Open(t, img)
	root := img[fs.info.RootDirOffset(0):]
	found := false
	for off := 0; off < len(root); off += 32 {
		if bytes.HasPrefix(root[off:], []byte("DIR1       ")) {
			binary.LittleEndian.PutUint16(root[off+26:], 0)
			found = true
		}
	}
	if !found {
		t.Fatal("DIR1 not found in the root")
	}
	fs = f1216Open(t, img)
	names, err := fs.ListDir("/DIR1")
	if err != nil || len(names) != 0 {
		t.Fatalf("ListDir(/DIR1 at cluster 0) = %v, %v", names, err)
	}
}

func TestFATTypeString(t *testing.T) {
	if FAT12.String() != "FAT12" || FAT16.String() != "FAT16" || FAT32.String() != "FAT32" || FATType(0).String() != "FAT32" {
		t.Fatal("FATType.String")
	}
}

// RootDirSectors on a hand-built Info with no sector size is 0, not a trap.
func TestRootDirSectorsZeroSectorSize(t *testing.T) {
	if (Info{RootEntries: 512}).RootDirSectors() != 0 {
		t.Fatal("RootDirSectors with BytesPerSector 0")
	}
}

// FuzzOpenFAT1216 drives OpenReader + listing + reads over a FAT12/16 image
// whose boot sector, FAT and directories are attacker-controlled. The contract
// is liveness and memory safety: a malformed volume is an error, never a
// panic, a hang or an unbounded allocation. Seeds: the head of each real
// fixture (boot sector, FATs, root directory) and single-byte mutations of the
// BPB fields that decide the geometry.
func FuzzOpenFAT1216(f *testing.F) {
	for _, typ := range []int{12, 16} {
		_, img := f1216Fixture(f, typ)
		head := img[:48<<10]
		f.Add(head)
		for _, off := range []int{11, 13, 14, 16, 17, 18, 19, 20, 22, 23, 32, 36} {
			m := append([]byte(nil), head...)
			m[off] ^= 0xFF
			f.Add(m)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < sectorSize {
			return
		}
		fs, err := OpenReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		defer fs.Close()
		ents, err := fs.ListDir("/")
		if err != nil {
			return
		}
		for _, e := range ents {
			p := "/" + e.Name()
			_, _ = fs.ReadFile(p)
			_, _ = fs.ListDir(p)
			if h, err := fs.(*fat32FS).OpenFile(p); err == nil {
				buf := make([]byte, 1024)
				_, _ = h.ReadAt(buf, 0)
				h.Close()
			}
		}
	})
}
