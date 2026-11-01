package filesystem_fat32

import "testing"

// A FAT directory is at most 65536 entries of 32 bytes, however large the
// cluster and however many clusters the cap allows.
func TestBomb_DirectoryByteCap(t *testing.T) {
	fs := newMockFS(nil, Info{BytesPerSector: 4096, SectorsPerCluster: 128}, 0)
	if got := fs.dirByteCap(); got != maxDirBytes {
		t.Errorf("512 KiB clusters: cap %d, want %d", got, maxDirBytes)
	}
	fs = newMockFS(nil, Info{BytesPerSector: 512, SectorsPerCluster: 1}, 0)
	if got, want := fs.dirByteCap(), uint64(256*512); got != want {
		t.Errorf("512-byte clusters: cap %d, want the 256-cluster cap %d", got, want)
	}
	// The boundary is exact: 2 MiB of clusters is allowed, no more.
	old := maxDirClusters
	defer func() { maxDirClusters = old }()
	fs = newMockFS(nil, Info{BytesPerSector: 4096, SectorsPerCluster: 1}, 0)
	maxDirClusters = maxDirBytes / 4096
	if got := fs.dirByteCap(); got != maxDirBytes {
		t.Errorf("at the cap: %d", got)
	}
	maxDirClusters++
	if got := fs.dirByteCap(); got != maxDirBytes {
		t.Errorf("one cluster over the cap: %d", got)
	}
}

func TestBomb_ReadBackedIsExact(t *testing.T) {
	d := newMockFS(make([]byte, 1000), Info{}, 0).f
	if b, err := readBacked(d, 900, 100); err != nil || len(b) != 100 {
		t.Errorf("up to the last byte: len=%d err=%v", len(b), err)
	}
	if _, err := readBacked(d, 900, 101); err == nil {
		t.Error("one byte past the end was read")
	}
	if _, err := readBacked(d, -1, 4); err == nil {
		t.Error("negative offset accepted")
	}
	if _, err := readBacked(d, 1<<62, 1<<62); err == nil {
		t.Error("overflowing range accepted")
	}
	if b, err := readBacked(d, 0, 0); err != nil || len(b) != 0 {
		t.Errorf("empty read: len=%d err=%v", len(b), err)
	}
}
