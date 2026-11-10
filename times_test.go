package filesystem_fat32

import (
	"compress/gzip"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// timesFixture is a FAT32 image whose entries were written by the reference
// tools: formatted by newfs_msdos (macOS) and populated by mtools 4.0.49
// `mcopy -m` from host files whose mtimes were set to the instants below, with
// TZ=UTC. Regenerate with FAT32_REGEN_TIMES=1 (needs both tools).
const timesFixture = "testdata/times-fat32.img.gz"

var timesSources = []struct {
	name  string
	mtime time.Time
}{
	{"EVEN.TXT", time.Date(2024, 5, 17, 13, 45, 30, 0, time.UTC)},
	{"ODD.TXT", time.Date(2021, 12, 31, 23, 59, 59, 0, time.UTC)},
	{"NEW80.TXT", time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)},
	{"LEAP.TXT", time.Date(2024, 2, 29, 12, 0, 2, 0, time.UTC)},
}

// makeTimesImage builds the image with the reference tools at path.
func makeTimesImage(t *testing.T, path string) {
	t.Helper()
	newfs, err1 := exec.LookPath("newfs_msdos")
	mcopy, err2 := exec.LookPath("mcopy")
	if err1 != nil || err2 != nil {
		t.Skip("newfs_msdos or mcopy not installed")
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(40 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if o, err := exec.Command(newfs, "-F", "32", "-S", "512", "-c", "1", "-s", "81920", path).CombinedOutput(); err != nil {
		t.Fatalf("newfs_msdos: %v\n%s", err, o)
	}
	src := t.TempDir()
	for _, s := range timesSources {
		p := filepath.Join(src, s.name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, s.mtime, s.mtime); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(mcopy, "-m", "-n", "-i", path, p, "::/")
		cmd.Env = append(os.Environ(), "TZ=UTC")
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("mcopy: %v\n%s", err, o)
		}
	}
}

func pinLocalUTC(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
}

func TestTimesRegenerateFixture(t *testing.T) {
	if os.Getenv("FAT32_REGEN_TIMES") != "1" {
		t.Skip("set FAT32_REGEN_TIMES=1 to rebuild " + timesFixture)
	}
	img := filepath.Join(t.TempDir(), "t.img")
	makeTimesImage(t, img)
	in, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(timesFixture)
	if err != nil {
		t.Fatal(err)
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	if _, err := io.Copy(zw, in); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

func openTimesFixture(t *testing.T) filesystem.TimesReader {
	t.Helper()
	gz, err := os.Open(timesFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "t.img")
	out, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		t.Fatal(err)
	}
	out.Close()
	fs, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	tr, ok := fs.(filesystem.TimesReader)
	if !ok {
		t.Fatal("fat32 is not a TimesReader")
	}
	return tr
}

// TestTimesReferenceImage checks the decoder against entries the reference
// tools wrote for known source times.
func TestTimesReferenceImage(t *testing.T) {
	pinLocalUTC(t)
	tr := openTimesFixture(t)
	for _, s := range timesSources {
		ft, err := tr.Times("/" + s.name)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		m, ok := ft.Mtime.Get()
		if !ok {
			t.Fatalf("%s: mtime not recorded", s.name)
		}
		if ft.Mtime.Resolution() != 2*time.Second || ft.Atime.Resolution() != 24*time.Hour && ft.Atime.Recorded() {
			t.Fatalf("%s: resolutions %v %v", s.name, ft.Mtime.Resolution(), ft.Atime.Resolution())
		}
		// FAT keeps 2 s: the stored value is the source rounded DOWN to an
		// even second by the reference tool.
		want := s.mtime.Truncate(2 * time.Second)
		if !m.Equal(want) {
			t.Errorf("%s: mtime = %v, want %v (source %v)", s.name, m, want, s.mtime)
		}
		if ft.Ctime.Recorded() {
			t.Errorf("%s: FAT has no ctime", s.name)
		}
		if b, ok := ft.Btime.Get(); ok && ft.Btime.Resolution() != 10*time.Millisecond {
			t.Errorf("%s: btime %v res %v", s.name, b, ft.Btime.Resolution())
		}
		t.Logf("%s: %+v", s.name, ft)
	}
}

// TestTimesLiveReferenceTools rebuilds an image with the tools on this host,
// so a new tool version that encodes differently fails here.
func TestTimesLiveReferenceTools(t *testing.T) {
	pinLocalUTC(t)
	img := filepath.Join(t.TempDir(), "t.img")
	makeTimesImage(t, img)
	fs, err := Open(img, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	tr := fs.(filesystem.TimesReader)
	ft, err := tr.Times("/EVEN.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := ft.Mtime.Get(); !m.Equal(time.Date(2024, 5, 17, 13, 45, 30, 0, time.UTC)) {
		t.Fatalf("mtime = %v", m)
	}
}

func TestTimesRootAndMissing(t *testing.T) {
	tr := openTimesFixture(t)
	ft, err := tr.Times("/")
	if err != nil || ft != (filesystem.FileTimes{}) {
		t.Fatalf("root: %+v %v", ft, err)
	}
	if _, err := tr.Times("/NOPE.TXT"); !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}

func entryWith(date, tod uint16, tenth uint8) []byte {
	e := make([]byte, 32)
	e[13] = tenth
	put := func(off int, v uint16) { e[off] = byte(v); e[off+1] = byte(v >> 8) }
	put(14, tod)
	put(16, date)
	put(18, date)
	put(22, tod)
	put(24, date)
	return e
}

// TestDecodeFATTimes covers the encoding exactly, by hand-built words: the
// bit layout, the tenth byte, and every way a field can be absent or invalid.
func TestDecodeFATTimes(t *testing.T) {
	pinLocalUTC(t)
	date := uint16(44<<9 | 5<<5 | 17)  // 2024-05-17
	tod := uint16(13<<11 | 45<<5 | 15) // 13:45:30
	ft := decodeFATTimes(entryWith(date, tod, 135))
	if m, _ := ft.Mtime.Get(); !m.Equal(time.Date(2024, 5, 17, 13, 45, 30, 0, time.UTC)) {
		t.Errorf("mtime %v", m)
	}
	// tenth 135 = 1 s + 350 ms on top of 13:45:30.
	if b, _ := ft.Btime.Get(); !b.Equal(time.Date(2024, 5, 17, 13, 45, 31, 350_000_000, time.UTC)) {
		t.Errorf("btime %v", b)
	}
	if a, _ := ft.Atime.Get(); !a.Equal(time.Date(2024, 5, 17, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("atime %v", a)
	}
	if ft.Btime.Resolution() != 10*time.Millisecond || ft.Atime.Resolution() != 24*time.Hour || ft.Mtime.Resolution() != 2*time.Second {
		t.Error("resolutions")
	}

	absent := []struct {
		name      string
		date, tod uint16
		tenth     uint8
	}{
		{"all zero", 0, 0, 0},
		{"month 0", 44<<9 | 0<<5 | 17, tod, 0},
		{"month 13", 44<<9 | 13<<5 | 17, tod, 0},
		{"day 0", 44<<9 | 5<<5 | 0, tod, 0},
		{"31 April", 44<<9 | 4<<5 | 31, tod, 0},
		{"29 Feb 2023", 43<<9 | 2<<5 | 29, tod, 0},
		{"hour 24", date, 24 << 11, 0},
		{"minute 60", date, 60 << 5, 0},
		{"seconds 60", date, 30, 0},
	}
	for _, c := range absent {
		got := decodeFATTime(c.date, c.tod, 0, 2*time.Second, false)
		if got.Recorded() {
			t.Errorf("%s decoded to %v", c.name, got)
		}
	}
	if decodeFATTime(date, tod, 200, 10*time.Millisecond, true).Recorded() {
		t.Error("tenth 200 must be invalid")
	}
	if !decodeFATTime(44<<9|2<<5|29, 0, 0, 2*time.Second, false).Recorded() {
		t.Error("29 Feb 2024 is valid")
	}
	if got := decodeFATTime(1<<5|1, 0, 0, 2*time.Second, false); !got.Recorded() {
		t.Error("1980-01-01 is the FAT epoch date, not absent")
	}
}

func FuzzDecodeFATTime(f *testing.F) {
	f.Add(uint16(0), uint16(0), uint8(0))
	f.Add(uint16(0xFFFF), uint16(0xFFFF), uint8(255))
	f.Add(uint16(44<<9|5<<5|17), uint16(13<<11|45<<5|15), uint8(135))
	f.Fuzz(func(t *testing.T, date, tod uint16, tenth uint8) {
		ts := decodeFATTime(date, tod, tenth, 10*time.Millisecond, true)
		if m, ok := ts.Get(); ok && (m.IsZero() || m.In(time.Local).Year() < 1980 || m.In(time.Local).Year() > 2108) {
			t.Fatalf("recorded %v from %#x %#x", m, date, tod)
		}
		_ = decodeFATTimes(entryWith(date, tod, tenth))
	})
}
