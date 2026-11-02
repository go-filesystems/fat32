package filesystem_fat32

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// CopyResized: every case copies into a file, then judges the new image by
// fsck_msdos -n, by every file's SHA-256, by names, attributes and timestamps,
// and checks that the source file is byte-identical afterwards.

func fileSHA(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// treeDump lists every path of a volume with what CopyResized promises to keep.
func treeDump(t testing.TB, fs *fat32FS) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(dir uint32, path string)
	walk = func(dir uint32, path string) {
		buf, err := fs.readDirBuf(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range parseRootDirMetadata(buf) {
			if e.name == "." || e.name == ".." {
				continue
			}
			p := path + "/" + e.name
			// attributes without the archive bit (the new entry sets its own),
			// create/access/write stamps
			meta := fmt.Sprintf("attr=%02x stamps=%x/%x", e.attr&^0x20, e.short[13:20], e.short[22:26])
			if e.attr&fatAttrDirectory != 0 {
				out[p] = "dir " + meta
				walk(e.cluster, p)
				continue
			}
			var data []byte
			if e.size > 0 {
				if data, err = fs.readClusterChain(e.cluster, uint64(e.size)); err != nil {
					t.Fatal(err)
				}
			}
			h := sha256.Sum256(data)
			out[p] = fmt.Sprintf("file %d %x %s", len(data), h[:6], meta)
		}
	}
	walk(fs.rootDir(), "")
	return out
}

func sameTree(t *testing.T, a, b map[string]string) {
	t.Helper()
	var ka []string
	for k := range a {
		ka = append(ka, k)
	}
	sort.Strings(ka)
	for _, k := range ka {
		if b[k] != a[k] {
			t.Errorf("%s: source %q, copy %q", k, a[k], b[k])
		}
	}
	if len(a) != len(b) {
		t.Errorf("source has %d entries, copy %d", len(a), len(b))
	}
}

// fat32Source makes a FAT32 volume (formatted by this package) holding a tree
// with long names, nested directories, empty and multi-cluster files, a
// read-only file and a hidden one.
func fat32Source(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src32.img")
	fs, err := Format(path, size, FormatConfig{Label: "SRC32"})
	if err != nil {
		t.Fatal(err)
	}
	s := fs.(*fat32FS)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.MkDir("/Docs and settings", 0o755))
	must(s.MkDir("/Docs and settings/deep", 0o755))
	must(s.WriteFile("/Docs and settings/deep/a long file name.txt", pat(9000, 1), 0o644))
	must(s.WriteFile("/Docs and settings/readme.TXT", pat(10, 2), 0o444))
	must(s.WriteFile("/EMPTY.BIN", nil, 0o644))
	must(s.WriteFile("/big file.bin", pat(70000, 3), 0o644))
	must(s.WriteFile("/HIDDEN.DAT", pat(600, 4), 0o644))
	hidden := make([]byte, dirEntrySize)
	hidden[11] = 0x02 | 0x20
	must(s.setEntryMeta("/HIDDEN.DAT", hidden))
	must(s.Close())
	return path
}

// fatCopiesEqual32 requires the FAT copies of a FAT32 image to be identical.
func fatCopiesEqual32(t *testing.T, path string) {
	t.Helper()
	re, err := Open(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	fs := re.(*fat32FS)
	img, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(fs.info.FATSize) * int64(fs.info.BytesPerSector)
	base := fs.info.FATOffset(0)
	if !bytes.Equal(img[base:base+size], img[base+size:base+2*size]) {
		t.Fatal("FAT copies differ")
	}
}

func openCopySource(t *testing.T, path string) *fat32FS {
	t.Helper()
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	return fs.(*fat32FS)
}

// copyInto runs CopyResized into a new file and returns it.
func copyInto(t *testing.T, src *fat32FS, size int64, cfg CopyConfig) (string, *fat32FS, error) {
	t.Helper()
	dstPath := filepath.Join(t.TempDir(), "dst.img")
	f, err := os.Create(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	out, err := CopyResized(f, size, src, cfg)
	if err != nil {
		return dstPath, nil, err
	}
	return dstPath, out.(*fat32FS), nil
}

func TestCopyResizedAcrossTypes(t *testing.T) {
	const MiB = 1 << 20
	f12, _ := f1216Fixture(t, 12)
	f16, _ := f1216Fixture(t, 16)
	_ = f12
	_ = f16
	cases := []struct {
		name  string
		src   func(t *testing.T) string
		size  int64
		cfg   CopyConfig
		want  FATType
		label string
	}{
		{"FAT12 to FAT16", func(t *testing.T) string { p, fs := f1216Writable(t, 12); fs.Close(); return p }, 16 * MiB, CopyConfig{}, FAT16, "FAT12TEST"},
		{"FAT16 to FAT32", func(t *testing.T) string { p, fs := f1216Writable(t, 16); fs.Close(); return p }, 70 * MiB, CopyConfig{Type: FAT32}, FAT32, "FAT16TEST"},
		{"FAT32 to FAT16 (shrink by copy)", func(t *testing.T) string { return fat32Source(t, 64*MiB) }, 20 * MiB, CopyConfig{}, FAT16, "SRC32"},
		{"FAT32 to FAT12 (shrink by copy)", func(t *testing.T) string { return fat32Source(t, 64*MiB) }, 2 * MiB, CopyConfig{}, FAT12, "SRC32"},
		{"FAT16 to FAT16, too big for in-place Grow", func(t *testing.T) string { p, fs := f1216Writable(t, 16); fs.Close(); return p }, 48 * MiB, CopyConfig{}, FAT16, "FAT16TEST"},
		{"FAT12 to FAT12 grown", func(t *testing.T) string { p, fs := f1216Writable(t, 12); fs.Close(); return p }, 2 * MiB, CopyConfig{Type: FAT12}, FAT12, "FAT12TEST"},
		{"relabelled", func(t *testing.T) string { p, fs := f1216Writable(t, 16); fs.Close(); return p }, 20 * MiB, CopyConfig{Label: "NEW LABEL", VolumeID: 0xCAFEF00D, RootEntries: 128}, FAT16, "NEW LABEL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srcPath := c.src(t)
			before := fileSHA(t, srcPath)
			src := openCopySource(t, srcPath)
			want := treeDump(t, src)
			dstPath, dst, err := copyInto(t, src, c.size, c.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if dst.info.Type != c.want {
				t.Fatalf("new image is %v, want %v", dst.info.Type, c.want)
			}
			if got := dst.Label(); got != c.label {
				t.Fatalf("label %q, want %q", got, c.label)
			}
			if c.cfg.VolumeID != 0 && dst.info.VolumeID != c.cfg.VolumeID {
				t.Fatalf("volume ID %#x", dst.info.VolumeID)
			}
			if c.cfg.RootEntries != 0 && dst.info.RootEntries != c.cfg.RootEntries {
				t.Fatalf("root entries %d", dst.info.RootEntries)
			}
			sameTree(t, want, treeDump(t, dst))
			if st, _ := os.Stat(dstPath); st.Size() != c.size {
				t.Fatalf("image is %d bytes, want %d", st.Size(), c.size)
			}
			if dst.info.Type == FAT32 {
				// checkVolume is the FAT12/16 checker; FAT32 gets fsck and the
				// two FAT copies.
				if err := dst.Close(); err != nil {
					t.Fatal(err)
				}
				fatCopiesEqual32(t, dstPath)
				fsckClean(t, dstPath)
			} else {
				settle(t, dstPath, dst)
			}
			re, err := Open(dstPath, -1)
			if err != nil {
				t.Fatal(err)
			}
			sameTree(t, want, treeDump(t, re.(*fat32FS)))
			re.Close()
			src.Close()
			if after := fileSHA(t, srcPath); after != before {
				t.Fatal("the source image changed")
			}
		})
	}
}

// The same-type growth that needs a bigger FAT is done in place now; the type
// change is still refused, and says where to go.
func TestCopyResizedWhereGrowRefuses(t *testing.T) {
	_, fs := f1216Writable(t, 16)
	if err := fs.Grow(48 << 20); err != nil {
		t.Fatalf("Grow = %v", err)
	}
	err := fs.Grow(sizeForCount(fs, 65600))
	if !errors.Is(err, ErrResizeType) || !strings.Contains(err.Error(), "CopyResized") {
		t.Fatalf("Grow across the type = %v", err)
	}
}

func TestCopyResizedRefusals(t *testing.T) {
	p, fs := f1216Writable(t, 16)
	fs.Close()
	src := openCopySource(t, p)
	before := fileSHA(t, p)
	const MiB = 1 << 20
	cases := []struct {
		name string
		size int64
		cfg  CopyConfig
		want string
	}{
		{"zero", 0, CopyConfig{}, "not a positive multiple"},
		{"negative", -512, CopyConfig{}, "not a positive multiple"},
		{"not a sector multiple", 4*MiB + 1, CopyConfig{}, "not a positive multiple"},
		{"2^32 sectors", int64(1) << 32 * 512, CopyConfig{}, "exceeds the 32-bit sector count"},
		{"too small", 4096, CopyConfig{}, "too small"},
		{"unknown type", 8 * MiB, CopyConfig{Type: 7}, "unknown FAT type"},
		{"root entries not a sector multiple", 8 * MiB, CopyConfig{RootEntries: 100}, "multiple of 16"},
		{"FAT12 too big", 1024 * MiB, CopyConfig{Type: FAT12}, "cannot be formatted as FAT12"},
		{"FAT16 too small", 1 * MiB, CopyConfig{Type: FAT16}, "cannot be formatted as FAT16"},
		{"FAT32 too small", 16 * MiB, CopyConfig{Type: FAT32}, "cannot be formatted as FAT32"},
		{"FAT16 too big", 8192 * MiB, CopyConfig{Type: FAT16}, "cannot be formatted as FAT16"},
	}
	for _, c := range cases {
		dstPath := filepath.Join(t.TempDir(), "d.img")
		f, _ := os.Create(dstPath)
		_, err := CopyResized(f, c.size, src, c.cfg)
		f.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		if st, _ := os.Stat(dstPath); st.Size() != 0 {
			t.Errorf("%s: a refused copy wrote to the destination", c.name)
		}
	}
	if _, err := CopyResized(nil, 1<<20, src, CopyConfig{}); err == nil {
		t.Error("nil destination accepted")
	}
	if _, err := CopyResized(&rwBuf{make([]byte, 1<<20)}, 1<<20, nil, CopyConfig{}); err == nil {
		t.Error("nil source accepted")
	}
	src.Close()
	if fileSHA(t, p) != before {
		t.Fatal("the source image changed")
	}
}

// A tree that needs more clusters than the image has is refused before dst is
// touched, with both numbers named.
func TestCopyResizedTreeTooBig(t *testing.T) {
	p, fs := f1216Writable(t, 16)
	fs.Close()
	src := openCopySource(t, p)
	dstPath := filepath.Join(t.TempDir(), "d.img")
	f, _ := os.Create(dstPath)
	defer f.Close()
	_, err := CopyResized(f, 192<<10, src, CopyConfig{Type: FAT12})
	if !errors.Is(err, ErrCopyNoFit) || !strings.Contains(err.Error(), "needs") || !strings.Contains(err.Error(), "clusters") {
		t.Fatalf("CopyResized = %v", err)
	}
	if b, _ := os.ReadFile(dstPath); len(b) != 0 {
		t.Fatal("the destination was written")
	}
}

// A root with more entries than a FAT12/16 fixed root has is refused, naming the
// entry count (long names take several records each).
func TestCopyResizedRootOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root.img")
	fs, err := Format(path, 8<<20, FormatConfig{Label: "ROOTY"})
	if err != nil {
		t.Fatal(err)
	}
	s := fs.(*fat32FS)
	for i := 0; i < 12; i++ {
		if err := s.WriteFile(fmt.Sprintf("/file number %02d.txt", i), pat(5, byte(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	before := fileSHA(t, path)
	src := openCopySource(t, path)
	defer src.Close()
	// 12 files with long names: 3 records each, plus the label: 37 entries.
	dstPath := filepath.Join(t.TempDir(), "d.img")
	f, _ := os.Create(dstPath)
	defer f.Close()
	_, err = CopyResized(f, 16<<20, src, CopyConfig{RootEntries: 16})
	if !errors.Is(err, ErrCopyRootFull) || !strings.Contains(err.Error(), "needs 37 entries") || !strings.Contains(err.Error(), "has 16") {
		t.Fatalf("CopyResized = %v", err)
	}
	// 64 entries are enough.
	_, d, err := copyInto(t, src, 16<<20, CopyConfig{RootEntries: 64})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if fileSHA(t, path) != before {
		t.Fatal("the source image changed")
	}
}

type copyFailFile struct {
	*os.File
	left    int
	truncEr error
}

var errCopyFault = errors.New("injected destination fault")

func (f *copyFailFile) WriteAt(p []byte, off int64) (int, error) {
	if f.left == 0 {
		f.left = -1 // one failed write; the disk works again afterwards
		return 0, errCopyFault
	}
	if f.left > 0 {
		f.left--
	}
	return f.File.WriteAt(p, off)
}

func (f *copyFailFile) Truncate(n int64) error {
	if f.truncEr != nil {
		return f.truncEr
	}
	return f.File.Truncate(n)
}

// A destination that fails after k writes: the error comes back, the source is
// unchanged, the half-written image is not a volume. Run for every k until a
// copy succeeds.
func TestCopyResizedFaultInjection(t *testing.T) {
	// Into FAT16 from FAT12 and FAT16 sources, and into FAT32 (whose layout
	// writes the backup boot sector, the FSInfo sectors and a root cluster too).
	for _, tc := range []struct {
		typ  int
		size int64
		cfg  CopyConfig
	}{{12, 16 << 20, CopyConfig{}}, {16, 16 << 20, CopyConfig{}}, {12, 70 << 20, CopyConfig{Type: FAT32}}} {
		typ := tc.typ
		p, fs := f1216Writable(t, typ)
		fs.Close()
		before := fileSHA(t, p)
		src := openCopySource(t, p)
		want := treeDump(t, src)
		done := false
		for k := 0; k < 3000 && !done; k++ {
			dstPath := filepath.Join(t.TempDir(), "d.img")
			raw, _ := os.Create(dstPath)
			ff := &copyFailFile{File: raw, left: k}
			out, err := CopyResized(ff, tc.size, src, tc.cfg)
			if err == nil {
				sameTree(t, want, treeDump(t, out.(*fat32FS)))
				done = true
			} else {
				if !errors.Is(err, errCopyFault) {
					t.Fatalf("FAT%d k=%d: %v", typ, k, err)
				}
				b := make([]byte, 512)
				raw.ReadAt(b, 0)
				if !bytes.Equal(b, make([]byte, 512)) {
					t.Fatalf("FAT%d k=%d: the failed image still has a boot sector", typ, k)
				}
			}
			raw.Close()
			if fileSHA(t, p) != before {
				t.Fatalf("FAT%d k=%d: the source image changed", typ, k)
			}
		}
		if !done {
			t.Fatalf("FAT%d: no copy succeeded within 3000 writes", typ)
		}
		src.Close()
	}
}

// FAT32 sizes pick the cluster size of the fatgen103 table, and a source whose
// file data cannot be read, a destination that cannot be read back, a long
// label and a random volume ID of 0 are each handled.
func TestCopyResizedGeometryAndEdges(t *testing.T) {
	const GiB = 1 << 30
	for _, c := range []struct {
		size int64
		spc  int64
	}{{300 << 20, 8}, {6 * GiB, 8}, {9 * GiB, 16}, {17 * GiB, 32}, {40 * GiB, 64}} {
		g, err := pickCopyGeometry(c.size, CopyConfig{Type: FAT32})
		if err != nil || g.spc != c.spc || g.typ != FAT32 {
			t.Errorf("FAT32 at %d bytes: %+v, %v; want %d sectors per cluster", c.size, g, err, c.spc)
		}
	}
	if g, err := pickCopyGeometry(80<<20, CopyConfig{Type: FAT32}); err != nil || g.spc != 1 {
		t.Errorf("FAT32 at 80 MiB: %+v, %v", g, err)
	}

	p, fs := f1216Writable(t, 12)
	fs.Close()
	src := openCopySource(t, p)
	defer src.Close()

	// Reads that fail one by one: some land in the file data (after the
	// directories were measured), and every one is an error, not a panic.
	_, ff, fsr := faultyVolume(t, 12)
	sawData := false
	for k := 0; k < 400; k++ {
		ff.readsLeft = k
		f, _ := os.Create(filepath.Join(t.TempDir(), "d.img"))
		_, err := CopyResized(f, 8<<20, fsr, CopyConfig{})
		f.Close()
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "read /") {
			sawData = true
		}
	}
	if !sawData {
		t.Error("no failing read landed in a file's data")
	}

	// The new image cannot be read back.
	raw, _ := os.Create(filepath.Join(t.TempDir(), "d.img"))
	defer raw.Close()
	if _, err := CopyResized(readFailFrom{raw, 0}, 8<<20, src, CopyConfig{}); !errors.Is(err, errCopyFault) {
		t.Errorf("unreadable destination: %v", err)
	}

	// A label over 11 bytes is cut; a volume ID that comes out 0 is replaced.
	old := formatRandUint32
	formatRandUint32 = func() uint32 { return 0 }
	defer func() { formatRandUint32 = old }()
	_, d, err := copyInto(t, src, 8<<20, CopyConfig{Label: "A VERY LONG LABEL"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Label() != "A VERY LONG " && d.Label() != "A VERY LONG" {
		t.Errorf("label %q", d.Label())
	}
	if d.info.VolumeID != 0x12345678 {
		t.Errorf("volume ID %#x", d.info.VolumeID)
	}
	d.Close()
}

// readFailFrom fails every read at or past an offset.
type readFailFrom struct {
	*os.File
	from int64
}

func (r readFailFrom) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > r.from {
		return 0, errCopyFault
	}
	return r.File.ReadAt(p, off)
}

func TestCopyResizedTruncateFails(t *testing.T) {
	p, fs := f1216Writable(t, 12)
	fs.Close()
	src := openCopySource(t, p)
	defer src.Close()
	raw, _ := os.Create(filepath.Join(t.TempDir(), "d.img"))
	defer raw.Close()
	_, err := CopyResized(&copyFailFile{File: raw, left: -1, truncEr: errCopyFault}, 8<<20, src, CopyConfig{})
	if !errors.Is(err, errCopyFault) {
		t.Fatalf("CopyResized = %v", err)
	}
}

// A hostile source: a directory that contains itself, directories nested past
// the limit, a directory with no cluster, a file whose chain is cut.
func TestCopyResizedHostileSource(t *testing.T) {
	dstFor := func() (*os.File, func()) {
		f, _ := os.Create(filepath.Join(t.TempDir(), "d.img"))
		return f, func() { f.Close() }
	}
	patchEntry := func(fs *fat32FS, dir uint32, name string, f func(e []byte)) {
		buf, err := fs.readDirBuf(dir)
		if err != nil {
			t.Fatal(err)
		}
		off, count, ok := fat32FindEntry(buf, name)
		if !ok {
			t.Fatalf("no %s", name)
		}
		f(buf[off+(count-1)*dirEntrySize:])
		if err := fs.writeDirBuf(dir, buf); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("loop", func(t *testing.T) {
		_, fs := f1216Writable(t, 16)
		if err := fs.MkDir("/DIR1/LOOP", 0o755); err != nil {
			t.Fatal(err)
		}
		d1, _, _ := fs.resolvePath("/DIR1")
		patchEntry(fs, d1.cluster, "LOOP", func(e []byte) {
			binary.LittleEndian.PutUint16(e[26:], uint16(d1.cluster))
		})
		f, done := dstFor()
		defer done()
		if _, err := CopyResized(f, 8<<20, fs, CopyConfig{}); err == nil || !strings.Contains(err.Error(), "reached twice") {
			t.Fatalf("CopyResized = %v", err)
		}
	})
	t.Run("too deep", func(t *testing.T) {
		_, fs := f1216Writable(t, 16)
		path := ""
		for i := 0; i < 66; i++ {
			path += "/D"
			if err := fs.MkDir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		f, done := dstFor()
		defer done()
		if _, err := CopyResized(f, 8<<20, fs, CopyConfig{}); err == nil || !strings.Contains(err.Error(), "deeper than 64") {
			t.Fatalf("CopyResized = %v", err)
		}
	})
	t.Run("directory without a cluster", func(t *testing.T) {
		_, fs := f1216Writable(t, 16)
		patchEntry(fs, fs.rootDir(), "DIR1", func(e []byte) {
			binary.LittleEndian.PutUint16(e[26:], 0)
		})
		f, done := dstFor()
		defer done()
		if _, err := CopyResized(f, 8<<20, fs, CopyConfig{}); err == nil || !strings.Contains(err.Error(), "has no cluster") {
			t.Fatalf("CopyResized = %v", err)
		}
	})
	t.Run("unreadable directory", func(t *testing.T) {
		_, fs := f1216Writable(t, 16)
		patchEntry(fs, fs.rootDir(), "DIR1", func(e []byte) {
			binary.LittleEndian.PutUint16(e[26:], 0xFFF0) // far outside the volume
		})
		f, done := dstFor()
		defer done()
		if _, err := CopyResized(f, 8<<20, fs, CopyConfig{}); err == nil {
			t.Fatal("CopyResized accepted a directory outside the volume")
		}
	})
	t.Run("unreadable file data", func(t *testing.T) {
		_, ff, fs := faultyVolume(t, 16)
		e, _, err := fs.resolvePath("/BIG.BIN")
		if err != nil {
			t.Fatal(err)
		}
		// Reads at and beyond BIG.BIN's first cluster fail: the directories
		// before it were read, its data cannot be.
		from := fs.info.DataOffset(0) + int64(e.cluster-2)*int64(fs.info.ClusterSize())
		fs.f = readFailFrom{ff.File, from}
		f, done := dstFor()
		defer done()
		if _, err := CopyResized(f, 8<<20, fs, CopyConfig{}); !errors.Is(err, errCopyFault) {
			t.Fatalf("CopyResized = %v", err)
		}
	})
	t.Run("source is not ours", func(t *testing.T) {
		f, done := dstFor()
		defer done()
		_, err := CopyResized(f, 8<<20, struct{ filesystem.Filesystem }{}, CopyConfig{})
		if err == nil || !strings.Contains(err.Error(), "opened by this package") {
			t.Fatalf("CopyResized = %v", err)
		}
	})
}

// The destination of a copy is writable like any volume, and a name that
// matches nothing in the source is not invented.
func TestCopyResizedResultIsWritable(t *testing.T) {
	p, fs := f1216Writable(t, 12)
	fs.Close()
	src := openCopySource(t, p)
	defer src.Close()
	dstPath, dst, err := copyInto(t, src, 16<<20, CopyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	data := pat(300000, 9)
	if err := dst.WriteFile("/MANY/NEW.BIN", data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := dst.ReadFile("/MANY/NEW.BIN"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back: %v", err)
	}
	settle(t, dstPath, dst)
}

// A failing metadata patch is reported (the entry vanishes between write and patch).
func TestSetEntryMetaMissing(t *testing.T) {
	_, fs := f1216Writable(t, 12)
	defer fs.Close()
	if err := fs.setEntryMeta("/NOPE.TXT", make([]byte, dirEntrySize)); err == nil {
		t.Fatal("setEntryMeta on a missing entry succeeded")
	}
	if err := fs.setEntryMeta("/NODIR/X", make([]byte, dirEntrySize)); err == nil {
		t.Fatal("setEntryMeta under a missing directory succeeded")
	}
}

// FuzzCopyResized copies a fuzzed volume into a new image of a fuzzed size and
// type: whatever the source claims, nothing panics, hangs or allocates without
// bound, and a copy that reports success opens as a volume.
func FuzzCopyResized(f *testing.F) {
	_, img := f1216Fixture(f, 12)
	f.Add(img, byte(0), byte(0))
	f.Add(img, byte(1), byte(2))
	for _, off := range []int{13, 14, 17, 22, 32} {
		m := append([]byte(nil), img...)
		m[off] ^= 0xFF
		f.Add(m, byte(3), byte(1))
	}
	f.Fuzz(func(t *testing.T, data []byte, sizeSel, typeSel byte) {
		if len(data) < sectorSize || len(data) > 4<<20 {
			return
		}
		src, err := OpenReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		defer src.Close()
		size := int64(1+sizeSel%16) << 20
		cfg := CopyConfig{Type: []FATType{0, FAT12, FAT16, FAT32}[typeSel%4]}
		dst := &rwTrunc{}
		out, err := CopyResized(dst, size, src, cfg)
		if err != nil {
			return
		}
		out.Close()
		re, err := OpenReader(bytes.NewReader(dst.b), int64(len(dst.b)))
		if err != nil {
			t.Fatalf("a successful copy does not open: %v", err)
		}
		re.Close()
	})
}
