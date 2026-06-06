package filesystem_fat32

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

func openFreshFat32(t *testing.T) (*fat32FS, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	return fs.(*fat32FS), p
}

func TestFat32SetLabel_Roundtrip(t *testing.T) {
	fs, _ := openFreshFat32(t)
	defer fs.Close()

	if err := fs.SetLabel("ROOTFS"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if got := fs.Label(); got != "ROOTFS" {
		t.Errorf("Label() = %q, want %q", got, "ROOTFS")
	}
}

func TestFat32SetLabel_PersistsAcrossReopen(t *testing.T) {
	fs, img := openFreshFat32(t)
	if err := fs.SetLabel("DATA1"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	fs.Close()

	fs2, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs2.Close()
	l, ok := fs2.(filesystem.Labeller)
	if !ok {
		t.Fatal("reopened fs does not implement Labeller")
	}
	if got := l.Label(); got != "DATA1" {
		t.Errorf("after reopen Label() = %q, want %q", got, "DATA1")
	}
}

func TestFat32SetLabel_FormatConfigSeedsLabel(t *testing.T) {
	// Format(...).Label flows into BPB; verify driver's Label() reflects it.
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, fat32TestSize, FormatConfig{Label: "SEEDED"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	l := fs.(*fat32FS)
	if got := l.Label(); got != "SEEDED" {
		t.Errorf("Label() = %q, want %q", got, "SEEDED")
	}
}

func TestFat32SetLabel_RejectsTooLong(t *testing.T) {
	fs, _ := openFreshFat32(t)
	defer fs.Close()
	before := fs.Label()
	if err := fs.SetLabel(strings.Repeat("X", MaxLabelLen+1)); err == nil {
		t.Error("SetLabel with oversize input unexpectedly succeeded")
	}
	if after := fs.Label(); after != before {
		t.Errorf("Label() changed after rejected SetLabel: %q -> %q", before, after)
	}
}

func TestFat32SetLabel_ShorterClearsTrailingBytes(t *testing.T) {
	fs, img := openFreshFat32(t)
	if err := fs.SetLabel("LONGLABEL10"); err != nil { // 11 bytes exact
		t.Fatalf("first SetLabel: %v", err)
	}
	if err := fs.SetLabel("HI"); err != nil { // shorter
		t.Fatalf("second SetLabel: %v", err)
	}
	fs.Close()

	// Verify on-disk: bytes 71..82 should be "HI         " (space-padded).
	f, err := os.Open(img)
	if err != nil {
		t.Fatalf("open img: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	want := []byte("HI         ") // 2 + 9 spaces = 11 bytes
	got := buf[bsOffVolLab : bsOffVolLab+MaxLabelLen]
	if !bytes.Equal(got, want) {
		t.Errorf("on-disk label slot = %q, want %q", got, want)
	}
}

func TestFat32SetLabel_UpdatesBackupBootSector(t *testing.T) {
	fs, img := openFreshFat32(t)
	backupOff := int64(fs.info.BackupBootSector) * int64(fs.info.BytesPerSector)
	if backupOff == 0 {
		t.Skip("no backup boot sector recorded for this image — nothing to verify")
	}
	if err := fs.SetLabel("BKUPTEST"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	fs.Close()

	f, err := os.Open(img)
	if err != nil {
		t.Fatalf("open img: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, backupOff); err != nil {
		t.Fatalf("ReadAt backup: %v", err)
	}
	if !bytes.HasPrefix(buf[bsOffVolLab:bsOffVolLab+MaxLabelLen], []byte("BKUPTEST")) {
		t.Errorf("backup boot sector label slot = %q, want prefix %q",
			buf[bsOffVolLab:bsOffVolLab+MaxLabelLen], "BKUPTEST")
	}
}

func TestFat32SetLabel_LabelerInterface(t *testing.T) {
	// Confirms a freshly-opened Filesystem-typed handle is still a
	// Labeller (the capability assertion lives in label.go).
	fs, _ := openFreshFat32(t)
	defer fs.Close()
	var f filesystem.Filesystem = fs
	if _, ok := f.(filesystem.Labeller); !ok {
		t.Error("fat32FS does not satisfy filesystem.Labeller")
	}
}
