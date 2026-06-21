package filesystem_fat32

import (
	"encoding/binary"
	"testing"
	"time"
)

// decodeFATDateTime is the inverse of encodeFATDateTime, used only by tests to
// check the bytes the writer lays down round-trip to the expected instant.
func decodeFATDateTime(date, tod uint16) (year, month, day, hour, min, sec int) {
	year = 1980 + int(date>>9)
	month = int(date>>5) & 0x0F
	day = int(date) & 0x1F
	hour = int(tod >> 11)
	min = int(tod>>5) & 0x3F
	sec = int(tod&0x1F) * 2
	return
}

// rootEntry8dot3 returns the 32-byte 8.3 directory entry for name in the root.
func rootEntry8dot3(t *testing.T, fs *fat32FS, name string) []byte {
	t.Helper()
	buf, err := fs.readDirBuf(fs.info.RootCluster)
	if err != nil {
		t.Fatalf("readDirBuf: %v", err)
	}
	start, count, found := fat32FindEntry(buf, name)
	if !found {
		t.Fatalf("entry %q not found", name)
	}
	off := start + (count-1)*dirEntrySize
	return buf[off : off+dirEntrySize]
}

// TestWriteFileStampsTimestamps verifies WriteFile records valid, current
// creation/write/access timestamps in the 8.3 entry (previously left zero,
// which decodes to an invalid 1980-00-00 date that fsck.fat/Windows flag).
func TestWriteFileStampsTimestamps(t *testing.T) {
	fixed := time.Date(2026, 6, 15, 14, 30, 20, 0, time.Local)
	old := timeNow
	timeNow = func() time.Time { return fixed }
	defer func() { timeNow = old }()

	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs := openTestFS(t, path)
	if err := fs.WriteFile("/stamp.txt", []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	e := rootEntry8dot3(t, fs, "STAMP.TXT")
	le := binary.LittleEndian
	wrtTime := le.Uint16(e[22:])
	wrtDate := le.Uint16(e[24:])
	if wrtDate == 0 || wrtTime == 0 {
		t.Fatalf("write timestamp not stamped: date=%#04x time=%#04x", wrtDate, wrtTime)
	}
	y, mo, d, h, mi, s := decodeFATDateTime(wrtDate, wrtTime)
	if y != 2026 || mo != 6 || d != 15 || h != 14 || mi != 30 || s != 20 {
		t.Fatalf("decoded write time = %04d-%02d-%02d %02d:%02d:%02d, want 2026-06-15 14:30:20",
			y, mo, d, h, mi, s)
	}
	// Creation date must match too (same instant), and the create-date word
	// must be a valid (non-zero month/day) date.
	if cd := le.Uint16(e[16:]); cd != wrtDate {
		t.Fatalf("create date %#04x != write date %#04x", cd, wrtDate)
	}
	if mo := (wrtDate >> 5) & 0x0F; mo == 0 {
		t.Fatal("month field is 0 (invalid FAT date)")
	}
}

// TestRenamePreservesTimestamps verifies that renaming a file keeps its
// original write timestamp rather than restamping it to "now".
func TestRenamePreservesTimestamps(t *testing.T) {
	created := time.Date(2021, 3, 4, 8, 9, 10, 0, time.Local)
	old := timeNow
	timeNow = func() time.Time { return created }

	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs := openTestFS(t, path)
	if err := fs.WriteFile("/orig.txt", []byte("x"), 0o644); err != nil {
		timeNow = old
		t.Fatalf("WriteFile: %v", err)
	}
	wantDate := binary.LittleEndian.Uint16(rootEntry8dot3(t, fs, "ORIG.TXT")[24:])

	// Rename under a different "now" — the preserved write date must not change.
	timeNow = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local) }
	defer func() { timeNow = old }()
	if err := fs.Rename("/orig.txt", "/renamed.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	gotDate := binary.LittleEndian.Uint16(rootEntry8dot3(t, fs, "RENAMED.TXT")[24:])
	if gotDate != wantDate {
		t.Fatalf("write date after rename = %#04x, want preserved %#04x", gotDate, wantDate)
	}
}
