package filesystem_fat32

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

const fat32TestSize = 4 * 1024 * 1024 // 4 MiB

var errFmtBoom = errors.New("format injected error")

// ── Validation errors ─────────────────────────────────────────────────────

func TestFmt_NotMultipleOfClusterSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.img")
	if _, err := Format(path, 4097, FormatConfig{}); err == nil {
		t.Error("expected error: size not a multiple of cluster size")
	}
}

func TestFmt_TooSmall(t *testing.T) {
	// 20480 = 5×4096; below the minimum required sectors.
	path := filepath.Join(t.TempDir(), "tiny.img")
	if _, err := Format(path, 20480, FormatConfig{}); err == nil {
		t.Error("expected error: size too small")
	}
}

// ── Happy-path basics ─────────────────────────────────────────────────────

func TestFmt_CreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.img")
	fs, err := Format(path, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("image file not created: %v", err)
	}
}

func TestFmt_FileSizePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != fat32TestSize {
		t.Errorf("size = %d, want %d", info.Size(), fat32TestSize)
	}
}

func TestFmt_TruncatesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.img")
	if err := os.WriteFile(path, make([]byte, 512*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := Format(path, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
}

func TestFmt_StatRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	st, err := fs.Stat("/")
	if err != nil {
		t.Fatalf("Stat /: %v", err)
	}
	if st.Mode()&0xF000 != 0x4000 {
		t.Errorf("root mode 0x%04X is not a directory", st.Mode())
	}
}

func TestFmt_ListDirRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if _, err := fs.ListDir("/"); err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
}

func TestFmt_WriteReadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	const data = "hello from Format\n"
	if err := fs.WriteFile("/hello.txt", []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != data {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestFmt_CustomLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, fat32TestSize, FormatConfig{Label: "MYVOL"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	fs2, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open after Format: %v", err)
	}
	defer fs2.Close()
}

func TestFmt_LongLabelTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, fat32TestSize, FormatConfig{Label: "THISLABELISTOOLONG"})
	if err != nil {
		t.Fatalf("Format with long label: %v", err)
	}
	fs.Close()
}

func TestFmt_ZeroVolumeIDFallback(t *testing.T) {
	old := formatRandUint32
	formatRandUint32 = func() uint32 { return 0 }
	t.Cleanup(func() { formatRandUint32 = old })
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format with zero rand VolumeID: %v", err)
	}
	fs.Close()
}

func TestFmt_ReOpenAndWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	{
		fs, err := Format(path, fat32TestSize, FormatConfig{})
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		if err := fs.WriteFile("/data.bin", []byte("original"), 0o600); err != nil {
			fs.Close()
			t.Fatalf("WriteFile: %v", err)
		}
		fs.Close()
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	got, err := fs.ReadFile("/data.bin")
	if err != nil {
		t.Fatalf("ReadFile after re-open: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("got %q, want %q", got, "original")
	}
}

// ── Error injection ───────────────────────────────────────────────────────

// fmtCountingFile wraps a real file and fails on the Nth WriteAt call.
type fmtCountingFile struct {
	inner     formatFile
	writeCall int
	failAt    int
}

func (f *fmtCountingFile) WriteAt(p []byte, off int64) (int, error) {
	f.writeCall++
	if f.writeCall == f.failAt {
		return 0, errFmtBoom
	}
	return f.inner.WriteAt(p, off)
}
func (f *fmtCountingFile) Truncate(n int64) error { return f.inner.Truncate(n) }
func (f *fmtCountingFile) Close() error           { return f.inner.Close() }

// fmtTruncFailFile is a formatFile whose Truncate always fails.
type fmtTruncFailFile struct{}

func (f *fmtTruncFailFile) WriteAt([]byte, int64) (int, error) { return 0, nil }
func (f *fmtTruncFailFile) Truncate(int64) error               { return errFmtBoom }
func (f *fmtTruncFailFile) Close() error                       { return nil }

// fmtCloseFailFile wraps a real file, making Close fail.
type fmtCloseFailFile struct{ inner formatFile }

func (f *fmtCloseFailFile) WriteAt(p []byte, off int64) (int, error) { return f.inner.WriteAt(p, off) }
func (f *fmtCloseFailFile) Truncate(n int64) error                   { return f.inner.Truncate(n) }
func (f *fmtCloseFailFile) Close() error                             { return errFmtBoom }

func injectCountingFile(t *testing.T, failAt int) {
	t.Helper()
	old := formatOpenFile
	formatOpenFile = func(path string) (formatFile, error) {
		inner, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		return &fmtCountingFile{inner: inner, failAt: failAt}, nil
	}
	t.Cleanup(func() { formatOpenFile = old })
}

func TestFmt_OpenFileFails(t *testing.T) {
	old := formatOpenFile
	formatOpenFile = func(string) (formatFile, error) { return nil, errFmtBoom }
	t.Cleanup(func() { formatOpenFile = old })
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), fat32TestSize, FormatConfig{}); !errors.Is(err, errFmtBoom) {
		t.Fatalf("expected errFmtBoom, got %v", err)
	}
}

func TestFmt_TruncateFails(t *testing.T) {
	old := formatOpenFile
	formatOpenFile = func(string) (formatFile, error) { return &fmtTruncFailFile{}, nil }
	t.Cleanup(func() { formatOpenFile = old })
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), fat32TestSize, FormatConfig{}); !errors.Is(err, errFmtBoom) {
		t.Fatalf("expected errFmtBoom, got %v", err)
	}
}

func TestFmt_WriteBootFails(t *testing.T)       { injectCountingFile(t, 1); fmtExpectBoom(t) }
func TestFmt_WriteBackupBootFails(t *testing.T) { injectCountingFile(t, 2); fmtExpectBoom(t) }
func TestFmt_WriteFSInfoFails(t *testing.T)     { injectCountingFile(t, 3); fmtExpectBoom(t) }
func TestFmt_WriteFAT0Fails(t *testing.T)       { injectCountingFile(t, 4); fmtExpectBoom(t) }
func TestFmt_WriteFAT1Fails(t *testing.T)       { injectCountingFile(t, 5); fmtExpectBoom(t) }

func fmtExpectBoom(t *testing.T) {
	t.Helper()
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), fat32TestSize, FormatConfig{}); !errors.Is(err, errFmtBoom) {
		t.Fatalf("expected errFmtBoom, got %v", err)
	}
}

func TestFmt_CloseFails(t *testing.T) {
	old := formatOpenFile
	formatOpenFile = func(path string) (formatFile, error) {
		inner, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		return &fmtCloseFailFile{inner: inner}, nil
	}
	t.Cleanup(func() { formatOpenFile = old })
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), fat32TestSize, FormatConfig{}); !errors.Is(err, errFmtBoom) {
		t.Fatalf("expected errFmtBoom, got %v", err)
	}
}

func TestFmt_OpenFSFails(t *testing.T) {
	old := formatOpenFS
	formatOpenFS = func(string, int) (filesystem.Filesystem, error) { return nil, errFmtBoom }
	t.Cleanup(func() { formatOpenFS = old })
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), fat32TestSize, FormatConfig{}); !errors.Is(err, errFmtBoom) {
		t.Fatalf("expected errFmtBoom, got %v", err)
	}
}
