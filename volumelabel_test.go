package filesystem_fat32

import (
	"path/filepath"
	"strings"
	"testing"
)

// countVolumeLabelEntries reads the root directory of the opened fs and returns
// the volume-label entries (ATTR_VOLUME_ID, not LFN) found, decoded to their
// trimmed 11-byte names.
func countVolumeLabelEntries(t *testing.T, fs *fat32FS) []string {
	t.Helper()
	buf, err := fs.readDirBuf(fs.info.RootCluster)
	if err != nil {
		t.Fatalf("readDirBuf: %v", err)
	}
	var names []string
	for off := 0; off+dirEntrySize <= len(buf); off += dirEntrySize {
		b0 := buf[off]
		if b0 == 0x00 {
			break
		}
		if b0 == 0xE5 {
			continue
		}
		attr := buf[off+11]
		if attr != fatAttrLongName && attr&fatAttrVolumeID != 0 {
			names = append(names, strings.TrimRight(string(buf[off:off+11]), " \x00"))
		}
	}
	return names
}

func openLabeledFS(t *testing.T, label string) *fat32FS {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vol.img")
	fsi, err := Format(p, fat32TestSize, FormatConfig{Label: label})
	if err != nil {
		t.Fatalf("Format(%q): %v", label, err)
	}
	t.Cleanup(func() { _ = fsi.Close() })
	return fsi.(*fat32FS)
}

// TestFormatWritesVolumeLabelEntry verifies that Format with a non-empty label
// creates exactly one matching ATTR_VOLUME_ID entry in the root directory — the
// invariant canonical fsck.fat requires (a BPB label with no root entry fails
// `fsck -n`).
func TestFormatWritesVolumeLabelEntry(t *testing.T) {
	fs := openLabeledFS(t, "GOFAT32")
	got := countVolumeLabelEntries(t, fs)
	if len(got) != 1 || got[0] != "GOFAT32" {
		t.Fatalf("volume-label entries = %v, want exactly [GOFAT32]", got)
	}
	if fs.Label() != "GOFAT32" {
		t.Fatalf("Label() = %q, want GOFAT32", fs.Label())
	}
}

// TestFormatNoLabelNoVolumeEntry verifies that an unlabeled volume has no
// ATTR_VOLUME_ID entry (fsck accepts a label-less volume, but a spurious empty
// label entry would be flagged).
func TestFormatNoLabelNoVolumeEntry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nolabel.img")
	fsi, err := Format(p, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsi.Close()
	if got := countVolumeLabelEntries(t, fsi.(*fat32FS)); len(got) != 0 {
		t.Fatalf("unlabeled volume has volume-label entries %v, want none", got)
	}
}

// TestVolumeLabelEntryHiddenFromListing makes sure the volume-label entry is
// never reported as a file by ListDir / Stat.
func TestVolumeLabelEntryHiddenFromListing(t *testing.T) {
	fs := openLabeledFS(t, "HIDDENVOL")
	if err := fs.WriteFile("/real.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), "HIDDENVOL") || strings.TrimSpace(e.Name()) == "HIDDENVOL" {
			t.Fatalf("ListDir surfaced the volume-label entry: %q", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "real.txt" {
		t.Fatalf("ListDir = %v, want exactly [real.txt]", entries)
	}
	// A name lookup for the label must not resolve to a phantom file.
	if _, err := fs.Stat("/HIDDENVOL"); err == nil {
		t.Fatal("Stat(/HIDDENVOL) resolved the volume-label entry, want error")
	}
}

// TestSetLabelSyncsRootEntry checks SetLabel keeps the single root volume-label
// entry in agreement with the BPB label across create / update / clear.
func TestSetLabelSyncsRootEntry(t *testing.T) {
	// Start unlabeled → SetLabel creates the entry.
	p := filepath.Join(t.TempDir(), "sync.img")
	fsi, err := Format(p, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsi.(*fat32FS)
	defer fs.Close()

	if err := fs.SetLabel("FIRST"); err != nil {
		t.Fatalf("SetLabel FIRST: %v", err)
	}
	if got := countVolumeLabelEntries(t, fs); len(got) != 1 || got[0] != "FIRST" {
		t.Fatalf("after SetLabel FIRST: entries = %v, want [FIRST]", got)
	}

	// Update → still exactly one entry, now matching.
	if err := fs.SetLabel("SECOND"); err != nil {
		t.Fatalf("SetLabel SECOND: %v", err)
	}
	if got := countVolumeLabelEntries(t, fs); len(got) != 1 || got[0] != "SECOND" {
		t.Fatalf("after SetLabel SECOND: entries = %v, want [SECOND]", got)
	}

	// Clear → entry removed.
	if err := fs.SetLabel(""); err != nil {
		t.Fatalf("SetLabel empty: %v", err)
	}
	if got := countVolumeLabelEntries(t, fs); len(got) != 0 {
		t.Fatalf("after SetLabel empty: entries = %v, want none", got)
	}
	// Clearing again is a no-op (existing < 0 branch).
	if err := fs.SetLabel(""); err != nil {
		t.Fatalf("SetLabel empty (no-op): %v", err)
	}
}

// TestSetLabelReplacesPreexistingEntry confirms a labeled volume's existing
// root entry is overwritten in place (not duplicated) by SetLabel.
func TestSetLabelReplacesPreexistingEntry(t *testing.T) {
	fs := openLabeledFS(t, "ORIGINAL")
	if err := fs.SetLabel("RENAMED"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	got := countVolumeLabelEntries(t, fs)
	if len(got) != 1 || got[0] != "RENAMED" {
		t.Fatalf("entries after rename = %v, want [RENAMED]", got)
	}
}

// TestFmt_WriteVolumeLabelEntryFails covers Format's volume-label-entry write
// error path (format.go:202). With a label set, the writes are: boot (1),
// backup boot (2), FSInfo (3), FAT0 (4), FAT1 (5), volume-label entry (6).
func TestFmt_WriteVolumeLabelEntryFails(t *testing.T) {
	injectCountingFile(t, 6)
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), fat32TestSize, FormatConfig{Label: "BOOMVOL"}); err == nil {
		t.Fatal("Format with failing volume-label-entry write: want error, got nil")
	}
}

// TestSetLabelSyncNoFreeSlot covers syncVolumeLabelEntry's "no free slot"
// guard: a root directory with every slot occupied, no end-of-directory
// terminator, no existing volume entry, and a fully-allocated FAT (so the
// chain cannot grow) leaves nowhere to place the new volume-label entry.
func TestSetLabelSyncNoFreeSlot(t *testing.T) {
	path := buildFullRootDirImage(t)
	fs := openTestFS(t, path)
	if err := fs.SetLabel("NEW"); err == nil {
		t.Fatal("SetLabel into a full root directory: want error, got nil")
	}
}

// TestSetLabelSyncReadError exercises the readDirBuf failure path inside
// syncVolumeLabelEntry.
func TestSetLabelSyncReadError(t *testing.T) {
	fs := openLabeledFS(t, "RDFAIL")
	w := wrapFault(fs)
	// The boot-sector writes (primary + backup) succeed; fail the root-dir read
	// that syncVolumeLabelEntry issues afterwards.
	w.failReadLo = fs.info.RootDirOffset(fs.partOffset)
	w.failReadHi = w.failReadLo + int64(fs.info.ClusterSize())
	if err := fs.SetLabel("NEW"); err == nil {
		t.Fatal("SetLabel with root-dir read error: want error, got nil")
	}
}
