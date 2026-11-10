package filesystem_fat32

import (
	"bytes"
	"errors"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

type chtimesFS interface {
	filesystem.Filesystem
	filesystem.TimesReader
	Chtimes(path string, atime, mtime time.Time) error
}

func formatChtimes(t *testing.T) (chtimesFS, string) {
	t.Helper()
	img := filepath.Join(t.TempDir(), "ct.img")
	fs, err := Format(img, 32<<20, FormatConfig{Label: "CT"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"/A.TXT", "/B.TXT"} {
		if err := fs.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.MkDir("/SUB", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/SUB/C.TXT", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fs.(chtimesFS), img
}

// TestMetadataSetter: the driver satisfies the whole interface (the assertion
// smb's SET_INFO path, go-fileshare's lock wrapper, NFS and SFTP make), and
// Chmod and Chown refuse with a typed error and change nothing.
func TestMetadataSetter(t *testing.T) {
	fs, _ := formatChtimes(t)
	defer fs.Close()
	var f filesystem.Filesystem = fs
	ms, ok := f.(filesystem.MetadataSetter)
	if !ok {
		t.Fatal("fat32 is not a filesystem.MetadataSetter")
	}
	before, _ := fs.Times("/A.TXT")
	for name, err := range map[string]error{
		"Chmod": ms.Chmod("/A.TXT", 0o600),
		"Chown": ms.Chown("/A.TXT", 1, 2),
	} {
		if !errors.Is(err, ErrNotSupported) || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s = %v, want ErrNotSupported (wrapping errors.ErrUnsupported)", name, err)
		}
	}
	if after, _ := fs.Times("/A.TXT"); after != before {
		t.Error("a refused Chmod/Chown changed the entry")
	}
	now := time.Now()
	if err := ms.Chtimes("/A.TXT", now, now); err != nil {
		t.Fatalf("Chtimes through the interface: %v", err)
	}
}

func timesOf(t *testing.T, fs chtimesFS, p string) (a, m time.Time) {
	t.Helper()
	ft, err := fs.Times(p)
	if err != nil {
		t.Fatalf("Times(%s): %v", p, err)
	}
	a, _ = ft.Atime.Get()
	m, _ = ft.Mtime.Get()
	return a, m
}

// TestChtimesRoundTrip: what Times reports after Chtimes is the value rounded
// to the format's resolution (mtime down to 2 s, atime down to the local day),
// in the zone Times decodes in, for a file, a nested file and a directory.
func TestChtimesRoundTrip(t *testing.T) {
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("CET", 3600), time.FixedZone("NYC", -5*3600)} {
		old := time.Local
		time.Local = zone
		fs, img := formatChtimes(t)
		at := time.Date(2024, 5, 17, 13, 45, 31, 999_000_000, zone)
		mt := time.Date(2021, 12, 31, 23, 59, 59, 5, zone)
		for _, p := range []string{"/A.TXT", "/SUB", "/SUB/C.TXT"} {
			btBefore, _ := fs.Times(p)
			if err := fs.Chtimes(p, at, mt); err != nil {
				t.Fatalf("%s: Chtimes: %v", p, err)
			}
			a, m := timesOf(t, fs, p)
			if want := time.Date(2024, 5, 17, 0, 0, 0, 0, zone); !a.Equal(want) {
				t.Errorf("%s: atime %v, want %v", p, a, want)
			}
			if want := time.Date(2021, 12, 31, 23, 59, 58, 0, zone); !m.Equal(want) {
				t.Errorf("%s: mtime %v, want %v", p, m, want)
			}
			ft, _ := fs.Times(p)
			if ft.Btime != btBefore.Btime {
				t.Errorf("%s: birth time changed: %v -> %v", p, btBefore.Btime, ft.Btime)
			}
		}
		// A time given in another zone is the same instant.
		if err := fs.Chtimes("/B.TXT", time.Time{}, time.Date(2000, 1, 1, 0, 0, 0, 0, zone).UTC()); err != nil {
			t.Fatal(err)
		}
		if _, m := timesOf(t, fs, "/B.TXT"); !m.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, zone)) {
			t.Errorf("mtime from a UTC instant: %v", m)
		}
		// It survives a close and a reopen.
		fs.Close()
		re, err := Open(img, -1)
		if err != nil {
			t.Fatal(err)
		}
		if _, m := timesOf(t, re.(chtimesFS), "/SUB/C.TXT"); !m.Equal(time.Date(2021, 12, 31, 23, 59, 58, 0, zone)) {
			t.Errorf("after reopen: %v", m)
		}
		re.Close()
		time.Local = old
	}
}

func TestChtimesRangeEdges(t *testing.T) {
	pinLocalUTC(t)
	fs, _ := formatChtimes(t)
	defer fs.Close()
	ok := []time.Time{
		time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2107, 12, 31, 23, 59, 59, 999_999_999, time.UTC),
		time.Date(2000, 2, 29, 12, 0, 1, 0, time.UTC),
	}
	for _, tm := range ok {
		if err := fs.Chtimes("/A.TXT", tm, tm); err != nil {
			t.Fatalf("%v: %v", tm, err)
		}
		a, m := timesOf(t, fs, "/A.TXT")
		if !m.Equal(tm.Truncate(2*time.Second)) || !a.Equal(tm.Truncate(24*time.Hour)) {
			t.Errorf("%v: got atime %v mtime %v", tm, a, m)
		}
	}
	a0, m0 := timesOf(t, fs, "/A.TXT")
	bad := []time.Time{
		time.Date(1979, 12, 31, 23, 59, 59, 0, time.UTC),
		time.Date(2108, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Unix(1<<40, 0),
	}
	for _, tm := range bad {
		for _, which := range []string{"atime", "mtime"} {
			good := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
			var err error
			if which == "atime" {
				err = fs.Chtimes("/A.TXT", tm, good)
			} else {
				err = fs.Chtimes("/A.TXT", good, tm)
			}
			var re *TimeRangeError
			if !errors.Is(err, ErrTimeOutOfRange) || !errors.As(err, &re) || re.Field != which || !strings.Contains(err.Error(), which) {
				t.Errorf("%v as %s: err = %v", tm, which, err)
			}
			// Nothing is written, not even the valid field.
			if a, m := timesOf(t, fs, "/A.TXT"); !a.Equal(a0) || !m.Equal(m0) {
				t.Errorf("%v as %s: a refused call changed the times", tm, which)
			}
		}
	}
}

func TestChtimesZeroLeavesUnchanged(t *testing.T) {
	pinLocalUTC(t)
	fs, _ := formatChtimes(t)
	defer fs.Close()
	at := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	mt := time.Date(2022, 9, 10, 11, 12, 14, 0, time.UTC)
	if err := fs.Chtimes("/A.TXT", at, mt); err != nil {
		t.Fatal(err)
	}
	mt2 := time.Date(2023, 1, 2, 3, 4, 6, 0, time.UTC)
	if err := fs.Chtimes("/A.TXT", time.Time{}, mt2); err != nil {
		t.Fatal(err)
	}
	if a, m := timesOf(t, fs, "/A.TXT"); !a.Equal(at.Truncate(24*time.Hour)) || !m.Equal(mt2) {
		t.Errorf("zero atime: got %v %v", a, m)
	}
	at2 := time.Date(2024, 6, 7, 0, 0, 0, 0, time.UTC)
	if err := fs.Chtimes("/A.TXT", at2, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if a, m := timesOf(t, fs, "/A.TXT"); !a.Equal(at2) || !m.Equal(mt2) {
		t.Errorf("zero mtime: got %v %v", a, m)
	}
	if err := fs.Chtimes("/A.TXT", time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if a, m := timesOf(t, fs, "/A.TXT"); !a.Equal(at2) || !m.Equal(mt2) {
		t.Errorf("both zero: got %v %v", a, m)
	}
}

func TestChtimesPaths(t *testing.T) {
	fs, _ := formatChtimes(t)
	defer fs.Close()
	now := time.Now()
	for _, p := range []string{"/NOPE.TXT", "/NOPE/X.TXT"} {
		if err := fs.Chtimes(p, now, now); !errors.Is(err, iofs.ErrNotExist) {
			t.Errorf("%s: %v, want ErrNotExist", p, err)
		}
	}
	if err := fs.Chtimes("/NOPE.TXT", time.Time{}, time.Time{}); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("zero times on a missing path: %v", err)
	}
	if err := fs.Chtimes("/", now, now); err == nil {
		t.Error("the root has no entry: want an error")
	}
	if err := fs.Chtimes("A.TXT", now, now); err == nil {
		t.Error("relative path accepted")
	}
	// Setting a time does not disturb the neighbours.
	if a, m := timesOf(t, fs, "/B.TXT"); a.IsZero() || m.IsZero() {
		t.Error("neighbour lost its times")
	}
}

// TestChtimesReadOnly: a reader that cannot be written refuses with the
// driver's read-only error and the times are unchanged.
func TestChtimesReadOnly(t *testing.T) {
	_, img := formatChtimes(t)
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	now := time.Now()
	if err := ro.(chtimesFS).Chtimes("/A.TXT", now, now); !errors.Is(err, ErrReadOnlyReader) {
		t.Errorf("Chtimes on a read-only reader = %v, want ErrReadOnlyReader", err)
	}
}

// TestChtimesFaultInjection: a failed directory read or write is returned and
// leaves the old times, then the next call succeeds (same style as the
// Truncate and label fault tests).
func TestChtimesFaultInjection(t *testing.T) {
	pinLocalUTC(t)
	fsi, _ := formatChtimes(t)
	fs := fsi.(*fat32FS)
	defer fs.Close()
	old := time.Date(2010, 1, 2, 3, 4, 6, 0, time.UTC)
	if err := fs.Chtimes("/SUB/C.TXT", old, old); err != nil {
		t.Fatal(err)
	}
	w := wrapFault(fs)
	nw := time.Date(2030, 7, 8, 9, 10, 12, 0, time.UTC)

	// Every disk write the call makes, then every read, fails in turn.
	for _, kind := range []string{"write", "read"} {
		for n := 1; n <= 6; n++ {
			w.failReadAt, w.failWriteAt = 0, 0
			if kind == "write" {
				w.failWriteAt = w.writes + n
			} else {
				w.failReadAt = w.reads + n
			}
			err := fs.Chtimes("/SUB/C.TXT", nw, nw)
			w.failReadAt, w.failWriteAt = 0, 0
			if err == nil {
				// The Nth access did not happen (the call needs fewer): nothing to assert
				// about the failure, but the times must now be the new ones.
				if a, m := timesOf(t, fs, "/SUB/C.TXT"); !m.Equal(nw) || !a.Equal(nw.Truncate(24*time.Hour)) {
					t.Fatalf("%s %d: success left %v %v", kind, n, a, m)
				}
				if err := fs.Chtimes("/SUB/C.TXT", old, old); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if !errors.Is(err, errInjected) {
				t.Fatalf("%s %d: err = %v", kind, n, err)
			}
			if a, m := timesOf(t, fs, "/SUB/C.TXT"); !m.Equal(old) || !a.Equal(old.Truncate(24*time.Hour)) {
				t.Errorf("%s %d: a failed Chtimes changed the times to %v %v", kind, n, a, m)
			}
		}
	}
	// The faults are spent: the volume still takes the change.
	if err := fs.Chtimes("/SUB/C.TXT", nw, nw); err != nil {
		t.Fatalf("after the faults: %v", err)
	}
	if _, m := timesOf(t, fs, "/SUB/C.TXT"); !m.Equal(nw) {
		t.Errorf("mtime %v", m)
	}
}

// TestChtimesReferenceTools: mtools reads the mtime back and fsck_msdos accepts
// the volume, with the image written only through Chtimes.
func TestChtimesReferenceTools(t *testing.T) {
	mdir, err := exec.LookPath("mdir")
	if err != nil {
		if os.Getenv("FAT32_REQUIRE_MTOOLS") != "" {
			t.Fatalf("FAT32_REQUIRE_MTOOLS is set but mtools is not installed: %v", err)
		}
		t.Skip("mtools `mdir` not found in PATH")
	}
	pinLocalUTC(t)
	fs, img := formatChtimes(t)
	cases := []struct {
		path string
		mt   time.Time
		want string
	}{
		{"/A.TXT", time.Date(2024, 5, 17, 13, 45, 31, 0, time.UTC), "2024-05-17  13:45"},
		{"/B.TXT", time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), "1980-01-01   0:00"}, // mdir pads the hour to 2 columns
		{"/SUB/C.TXT", time.Date(2107, 12, 31, 23, 59, 58, 0, time.UTC), "2107-12-31  23:59"},
	}
	for _, c := range cases {
		if err := fs.Chtimes(c.path, c.mt, c.mt); err != nil {
			t.Fatal(err)
		}
	}
	fs.Close()
	for _, c := range cases {
		cmd := exec.Command(mdir, "-i", img, "::"+filepath.Dir(c.path))
		cmd.Env = append(os.Environ(), "MTOOLS_SKIP_CHECK=1", "TZ=UTC")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mdir: %v\n%s", err, out)
		}
		found := false
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(strings.ToUpper(line), strings.ToUpper(filepath.Base(c.path[:len(c.path)-4]))) && strings.Contains(line, c.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("mdir does not show %s at %s:\n%s", c.path, c.want, out)
		}
	}
	if fsck, err := exec.LookPath("fsck_msdos"); err == nil {
		out, err := exec.Command(fsck, "-n", img).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Phase 3 - Checking for Orphan Clusters") {
			t.Errorf("fsck_msdos -n: %v\n%s", err, out)
		}
		t.Logf("fsck_msdos -n:\n%s", out)
	}
}

// encodeDecode is the pure round trip the fuzzer drives.
func encodeDecode(atime, mtime time.Time) filesystem.FileTimes {
	e := make([]byte, 32)
	encodeFATTimes(e, atime, mtime)
	return decodeFATTimes(e)
}

func FuzzChtimesEncode(f *testing.F) {
	f.Add(int64(0), int64(0), int64(1_000_000_000), int64(0))
	f.Add(int64(315532800), int64(0), int64(4354819199), int64(999_999_999)) // 1980-01-01 .. 2107-12-31
	f.Add(int64(-1), int64(5), int64(4354819200), int64(0))                  // just outside both ends
	f.Fuzz(func(t *testing.T, as, an, ms, mn int64) {
		pinLocalUTC(t)
		if as < -1<<40 || as > 1<<40 || ms < -1<<40 || ms > 1<<40 {
			t.Skip()
		}
		at, mt := time.Unix(as, an%1_000_000_000).UTC(), time.Unix(ms, mn%1_000_000_000).UTC()
		if checkFATRange("atime", at) != nil || checkFATRange("mtime", mt) != nil {
			return // refused before any encoding; the range tests cover the edges
		}
		ft := encodeDecode(at, mt)
		a, ok1 := ft.Atime.Get()
		m, ok2 := ft.Mtime.Get()
		if !ok1 || !ok2 {
			t.Fatalf("%v %v encoded to an unrecorded time", at, mt)
		}
		if !a.Equal(at.Truncate(24*time.Hour)) || !m.Equal(mt.Truncate(2*time.Second)) {
			t.Fatalf("%v %v round-tripped to %v %v", at, mt, a, m)
		}
	})
}
