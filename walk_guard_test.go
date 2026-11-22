package filesystem_fat32

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"github.com/go-volumes/safeio"
)

// An unreadable directory is an error of the check, not a pass.
func TestCheckDirTreeReportsUnreadableDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	w, err := Format(path, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer w.Close()
	if err := w.(*fat32FS).checkDirTree(0x0FFFFFF0); err == nil {
		t.Fatal("checkDirTree of a cluster outside the volume = nil, want an error")
	}
}

// TestDeleteDirCycleIsRefused: a directory entry whose first cluster is the
// cluster of the directory holding it used to make DeleteDir recurse until the
// goroutine stack overflowed (a fatal error, no recover()). It is now refused
// before the entry is marked deleted or any chain is freed.
func TestDeleteDirCycleIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	w, err := Format(path, fat32TestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := w.MkDir("/loop", 0o755); err != nil {
		t.Fatalf("MkDir(/loop): %v", err)
	}
	if err := w.MkDir("/loop/self", 0o755); err != nil {
		t.Fatalf("MkDir(/loop/self): %v", err)
	}
	wfs := w.(*fat32FS)
	loopEntry, _, err := wfs.resolvePath("/loop")
	if err != nil {
		t.Fatalf("resolvePath(/loop): %v", err)
	}
	loopCluster := loopEntry.cluster
	buf, err := wfs.readDirBuf(loopCluster)
	if err != nil {
		t.Fatalf("readDirBuf: %v", err)
	}
	startOff, count, found := fat32FindEntry(buf, "self")
	if !found {
		t.Fatal("entry self not found")
	}
	e := buf[startOff+(count-1)*dirEntrySize:]
	binary.LittleEndian.PutUint16(e[20:22], uint16(loopCluster>>16))
	binary.LittleEndian.PutUint16(e[26:28], uint16(loopCluster))
	if err := wfs.writeDirBuf(loopCluster, buf); err != nil {
		t.Fatalf("writeDirBuf: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs := openTestFS(t, path)
	if _, err := fs.ListDir("/loop"); err != nil {
		t.Fatalf("ListDir(/loop): %v", err)
	}
	err = fs.DeleteDir("/loop")
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("DeleteDir(/loop) = %v, want safeio.ErrCycle", err)
	}
	if _, err := fs.ListDir("/loop"); err != nil {
		t.Fatalf("directory gone after a refused delete: %v", err)
	}
}
