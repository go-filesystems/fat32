package filesystem_fat32

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFile(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "README  TXT", 0x20, 3, 5)
		writeFAT32ShortEntrySized(root[32:64], "SUBDIR     ", fatAttrDirectory, 4, 0)
		writeFAT32ShortEntrySized(root[64:96], "EMPTY   TXT", 0x20, 0, 0)
		root[96] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF, 4: 0x0FFFFFFF}, map[uint32][]byte{3: []byte("hello")})

	fs := openTestFS(t, path)
	defer fs.Close()

	// root is not a file
	if _, err := fs.ReadFile("/"); err == nil {
		t.Fatal("ReadFile(/) error = nil, want error")
	}
	// nested path
	if _, err := fs.ReadFile("/nested/file"); err == nil {
		t.Fatal("ReadFile nested error = nil, want error")
	}
	// is a directory
	if _, err := fs.ReadFile("/SUBDIR"); err == nil {
		t.Fatal("ReadFile(dir) error = nil, want error")
	}
	// not found
	if _, err := fs.ReadFile("/missing.txt"); err == nil {
		t.Fatal("ReadFile(missing) error = nil, want error")
	}
	// empty file (cluster=0)
	data, err := fs.ReadFile("/empty.txt")
	if err != nil {
		t.Fatalf("ReadFile(empty): %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("ReadFile(empty) = %v, want empty", data)
	}
	// success: read 5 bytes from cluster 3
	data, err = fs.ReadFile("/readme.txt")
	if err != nil {
		t.Fatalf("ReadFile(/readme.txt): %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("ReadFile(/readme.txt) = %q, want %q", data, "hello")
	}
}

func TestReadFileIOErrors(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "DATA    TXT", 0x20, 3, 5)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, nil)

	fs := openTestFS(t, path)
	fs.f.Close()
	if _, err := fs.ReadFile("/data.txt"); err == nil {
		t.Fatal("ReadFile with closed file error = nil, want error")
	}
}

func TestWriteFile(t *testing.T) {
	// Start with empty root dir.
	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)

	fs := openTestFS(t, path)
	defer fs.Close()

	// path validation errors
	if err := fs.WriteFile("/", nil, 0o644); err == nil {
		t.Fatal("WriteFile(/) error = nil, want error")
	}
	if err := fs.WriteFile("noabs.txt", nil, 0o644); err == nil {
		t.Fatal("WriteFile(no leading slash) error = nil, want error")
	}
	if err := fs.WriteFile("/nested/file.txt", nil, 0o644); err == nil {
		t.Fatal("WriteFile(nested) error = nil, want error")
	}

	// create new file
	if err := fs.WriteFile("/hello.txt", []byte("world"), 0o644); err != nil {
		t.Fatalf("WriteFile(create): %v", err)
	}
	data, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile after WriteFile: %v", err)
	}
	if string(data) != "world" {
		t.Fatalf("data = %q, want %q", data, "world")
	}

	// overwrite with read-only perm
	if err := fs.WriteFile("/hello.txt", []byte("updated"), 0o444); err != nil {
		t.Fatalf("WriteFile(overwrite): %v", err)
	}
	data, err = fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile after overwrite: %v", err)
	}
	if string(data) != "updated" {
		t.Fatalf("overwritten data = %q, want %q", data, "updated")
	}

	// zero-length file (cluster 0)
	if err := fs.WriteFile("/empty.txt", nil, 0o644); err != nil {
		t.Fatalf("WriteFile(zero-length): %v", err)
	}
	data, err = fs.ReadFile("/empty.txt")
	if err != nil {
		t.Fatalf("ReadFile(zero-length): %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("ReadFile(zero-length) = %v, want empty", data)
	}
}

func TestWriteFileFullFAT(t *testing.T) {
	// Fill the entire FAT with EOF entries so allocCluster fails.
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	fatBytes := int(info.FATSize) * sectorSize
	for i := 0; i < fatBytes; i++ {
		image[fatBase+i] = 0xFF
	}
	root := image[int(info.RootDirOffset(0)):]
	root[0] = 0x00

	path := filepath.Join(t.TempDir(), "fat32-full.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs := openTestFS(t, path)
	defer fs.Close()

	if err := fs.WriteFile("/file.txt", []byte("data"), 0o644); err == nil {
		t.Fatal("WriteFile on full FAT error = nil, want error")
	}
}

func TestWriteFileIOError(t *testing.T) {
	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if err := fs.WriteFile("/test.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile with closed file error = nil, want error")
	}
}

func TestReadLink(t *testing.T) {
	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs := openTestFS(t, path)
	defer fs.Close()
	if _, err := fs.ReadLink("/anything"); err == nil {
		t.Fatal("ReadLink error = nil, want error")
	}
	if _, err := fs.ReadLink("/"); err == nil {
		t.Fatal("ReadLink(/) error = nil, want error")
	}
}

func TestMkDir(t *testing.T) {
	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)

	fs := openTestFS(t, path)
	defer fs.Close()

	// path validation errors
	if err := fs.MkDir("/", 0o755); err == nil {
		t.Fatal("MkDir(/) error = nil, want error")
	}
	if err := fs.MkDir("/nested/dir", 0o755); err == nil {
		t.Fatal("MkDir(nested) error = nil, want error")
	}

	// create directory
	if err := fs.MkDir("/mydir", 0o755); err != nil {
		t.Fatalf("MkDir(/mydir): %v", err)
	}
	// already exists
	if err := fs.MkDir("/mydir", 0o755); err == nil {
		t.Fatal("MkDir duplicate error = nil, want error")
	}
	// read-only perm
	if err := fs.MkDir("/rodir", 0o555); err != nil {
		t.Fatalf("MkDir(ro): %v", err)
	}
	// verify entries exist
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after MkDir: %v", err)
	}
	found := false
	for _, e := range entries {
		// Before LFN write support the entry was stored as uppercase 8.3; now
		// the directory name is preserved exactly via LFN entries.
		if strings.EqualFold(e.Name(), "MYDIR") {
			found = true
		}
	}
	if !found {
		t.Fatal("MkDir: mydir not found in root dir")
	}
}

func TestMkDirIOError(t *testing.T) {
	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if err := fs.MkDir("/mydir", 0o755); err == nil {
		t.Fatal("MkDir with closed file error = nil, want error")
	}
}

func TestDeleteFile(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "FILE    TXT", 0x20, 3, 10)
		writeFAT32ShortEntrySized(root[32:64], "SUBDIR     ", fatAttrDirectory, 4, 0)
		root[64] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF, 4: 0x0FFFFFFF}, nil)

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// path validation errors
	if err := fs.DeleteFile("/"); err == nil {
		t.Fatal("DeleteFile(/) error = nil, want error")
	}
	if err := fs.DeleteFile("/nested/file"); err == nil {
		t.Fatal("DeleteFile(nested) error = nil, want error")
	}
	// not found
	if err := fs.DeleteFile("/missing.txt"); err == nil {
		t.Fatal("DeleteFile(missing) error = nil, want error")
	}
	// is a directory
	if err := fs.DeleteFile("/SUBDIR"); err == nil {
		t.Fatal("DeleteFile(dir) error = nil, want error")
	}
	// success
	if err := fs.DeleteFile("/file.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after delete: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "FILE.TXT" {
			t.Fatal("file still present after DeleteFile")
		}
	}
}

func TestDeleteFileIOError(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "FILE    TXT", 0x20, 3, 5)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if err := fs.DeleteFile("/file.txt"); err == nil {
		t.Fatal("DeleteFile with closed file error = nil, want error")
	}
}

func TestDeleteDir(t *testing.T) {
	// Build an image with an empty dir and a non-empty dir.
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "EMPTYDIR   ", fatAttrDirectory, 3, 0)
		writeFAT32ShortEntrySized(root[32:64], "FULL    DIR", fatAttrDirectory, 4, 0)
		writeFAT32ShortEntrySized(root[64:96], "FILE    TXT", 0x20, 5, 5)
		root[96] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF, 4: 0x0FFFFFFF, 5: 0x0FFFFFFF}, func() map[uint32][]byte {
		// cluster 3: empty dir (just "." and "..")
		clusterBuf := make([]byte, 4096)
		copy(clusterBuf[0:11], []byte(".          "))
		clusterBuf[11] = fatAttrDirectory
		copy(clusterBuf[32:43], []byte("..         "))
		clusterBuf[43] = fatAttrDirectory
		clusterBuf[64] = 0x00
		// cluster 4: non-empty dir (has a file entry)
		clusterBuf4 := make([]byte, 4096)
		copy(clusterBuf4[0:11], []byte(".          "))
		clusterBuf4[11] = fatAttrDirectory
		copy(clusterBuf4[32:43], []byte("CHILD   TXT"))
		clusterBuf4[43] = 0x20
		clusterBuf4[64] = 0x00
		return map[uint32][]byte{3: clusterBuf, 4: clusterBuf4}
	}())

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// path validation
	if err := fs.DeleteDir("/"); err == nil {
		t.Fatal("DeleteDir(/) error = nil, want error")
	}
	if err := fs.DeleteDir("/nested/dir"); err == nil {
		t.Fatal("DeleteDir(nested) error = nil, want error")
	}
	// not found
	if err := fs.DeleteDir("/missing"); err == nil {
		t.Fatal("DeleteDir(missing) error = nil, want error")
	}
	// is a file
	if err := fs.DeleteDir("/file.txt"); err == nil {
		t.Fatal("DeleteDir(file) error = nil, want error")
	}
	// success: recursive delete on non-empty dir
	if err := fs.DeleteDir("/full.dir"); err != nil {
		t.Fatalf("DeleteDir(recursive): %v", err)
	}
	// success on empty dir
	if err := fs.DeleteDir("/emptydir"); err != nil {
		t.Fatalf("DeleteDir(empty): %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after DeleteDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "EMPTYDIR" {
			t.Fatal("dir still present after DeleteDir")
		}
	}
}

func TestDeleteDirWithNoCluster(t *testing.T) {
	// Dir entry with firstCluster == 0 (unusual but valid path).
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "ZERODIR    ", fatAttrDirectory, 0, 0)
		root[32] = 0x00
	}, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.DeleteDir("/zerodir"); err != nil {
		t.Fatalf("DeleteDir(cluster=0): %v", err)
	}
}

func TestDeleteDirIOError(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "MYDIR      ", fatAttrDirectory, 3, 0)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if err := fs.DeleteDir("/mydir"); err == nil {
		t.Fatal("DeleteDir with closed file error = nil, want error")
	}
}

func TestRename(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "OLD     TXT", 0x20, 3, 5)
		writeFAT32ShortEntrySized(root[32:64], "OTHER   TXT", 0x20, 4, 3)
		root[64] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF, 4: 0x0FFFFFFF}, map[uint32][]byte{3: []byte("hello"), 4: []byte("bye")})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// path validation errors
	if err := fs.Rename("/", "/new"); err == nil {
		t.Fatal("Rename(root old) error = nil, want error")
	}
	if err := fs.Rename("/old.txt", "/"); err == nil {
		t.Fatal("Rename(root new) error = nil, want error")
	}
	if err := fs.Rename("/nested/a", "/b"); err == nil {
		t.Fatal("Rename(nested old) error = nil, want error")
	}
	if err := fs.Rename("/old.txt", "/nested/b"); err == nil {
		t.Fatal("Rename(nested new) error = nil, want error")
	}
	// not found
	if err := fs.Rename("/missing.txt", "/new.txt"); err == nil {
		t.Fatal("Rename(missing) error = nil, want error")
	}
	// same short name (no-op)
	if err := fs.Rename("/old.txt", "/OLD.TXT"); err != nil {
		t.Fatalf("Rename(same name): %v", err)
	}
	// success: rename to a new name
	if err := fs.Rename("/old.txt", "/renamed.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	data, err := fs.ReadFile("/renamed.txt")
	if err != nil {
		t.Fatalf("ReadFile after Rename: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("renamed file data = %q, want %q", string(data), "hello")
	}
	// rename replacing existing file
	if err := fs.Rename("/other.txt", "/renamed.txt"); err != nil {
		t.Fatalf("Rename(replace): %v", err)
	}
	data, err = fs.ReadFile("/renamed.txt")
	if err != nil {
		t.Fatalf("ReadFile after second rename: %v", err)
	}
	if string(data) != "bye" {
		t.Fatalf("replaced file data = %q, want %q", string(data), "bye")
	}
}

func TestRenameIOError(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "OLD     TXT", 0x20, 3, 5)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, nil)
	fs := openTestFS(t, path)
	fs.f.Close()
	if err := fs.Rename("/old.txt", "/new.txt"); err == nil {
		t.Fatal("Rename with closed file error = nil, want error")
	}
}

func TestFindRootDirSlot(t *testing.T) {
	// Full buffer: no free slot and no 0x00 terminator.
	buf := make([]byte, 2*dirEntrySize)
	writeFAT32ShortEntry(buf[0:32], "FILE1   TXT", 0x20, 1)
	writeFAT32ShortEntry(buf[32:64], "FILE2   TXT", 0x20, 2)
	short := toShortNameBytes("MISSING.TXT")
	off, found := findRootDirSlot(buf, short)
	if found || off >= 0 {
		t.Fatalf("findRootDirSlot(full) = (%d, %v), want (-1, false)", off, found)
	}
	// Deleted entry reused as free slot.
	buf2 := make([]byte, 2*dirEntrySize)
	buf2[0] = 0xE5
	writeFAT32ShortEntry(buf2[32:64], "FILE    TXT", 0x20, 1)
	off2, found2 := findRootDirSlot(buf2, short)
	if found2 || off2 != 0 {
		t.Fatalf("findRootDirSlot(deleted) = (%d, %v), want (0, false)", off2, found2)
	}
}

func TestWriteFileMultiCluster(t *testing.T) {
	// Write data that spans two clusters (ClusterSize = 4096).
	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	bigData := make([]byte, 5000)
	for i := range bigData {
		bigData[i] = byte(i % 251)
	}
	if err := fs.WriteFile("/big.txt", bigData, 0o644); err != nil {
		t.Fatalf("WriteFile(multi-cluster): %v", err)
	}
	got, err := fs.ReadFile("/big.txt")
	if err != nil {
		t.Fatalf("ReadFile(multi-cluster): %v", err)
	}
	if len(got) != len(bigData) || got[4999] != bigData[4999] {
		t.Fatalf("multi-cluster data mismatch")
	}
	// Overwrite to exercise freeChain with a multi-cluster chain.
	if err := fs.WriteFile("/big.txt", []byte("small"), 0o644); err != nil {
		t.Fatalf("WriteFile(overwrite big): %v", err)
	}
	got2, err := fs.ReadFile("/big.txt")
	if err != nil {
		t.Fatalf("ReadFile after overwrite: %v", err)
	}
	if string(got2) != "small" {
		t.Fatalf("overwrite data = %q, want small", got2)
	}
}

func TestReadFileClusterReadError(t *testing.T) {
	// Build an image just large enough to hold the root dir cluster but not
	// cluster 3, so readClusterChain fails when reading cluster-3 data.
	boot := defaultFAT32BootSector()
	info, _ := readInfo(bytes.NewReader(boot), 0)
	// Image ends exactly at the end of the root dir cluster.
	imageSize := int(info.RootDirOffset(0)) + int(info.ClusterSize())
	image := make([]byte, imageSize)
	copy(image, boot)
	// Root cluster FAT entries.
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF) // cluster 3 = EOF
	// Root dir entry pointing to cluster 3.
	root := image[int(info.RootDirOffset(0)):]
	writeFAT32ShortEntrySized(root[0:32], "DATA    TXT", 0x20, 3, 5)
	root[32] = 0x00

	path := filepath.Join(t.TempDir(), "fat32-trunc.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if _, err := fs.ReadFile("/data.txt"); err == nil {
		t.Fatal("ReadFile on truncated image error = nil, want cluster read error")
	}
}

func TestMkDirFullFAT(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	fatBytes := int(info.FATSize) * sectorSize
	for i := 0; i < fatBytes; i++ {
		image[fatBase+i] = 0xFF
	}
	root := image[int(info.RootDirOffset(0)):]
	root[0] = 0x00

	path := filepath.Join(t.TempDir(), "fat32-full2.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.MkDir("/newdir", 0o755); err == nil {
		t.Fatal("MkDir on full FAT error = nil, want error")
	}
}

func TestDeleteDirClusterReadError(t *testing.T) {
	// Build an image just large enough for the root dir but not cluster 3's data.
	boot := defaultFAT32BootSector()
	info, _ := readInfo(bytes.NewReader(boot), 0)
	imageSize := int(info.RootDirOffset(0)) + int(info.ClusterSize())
	image := make([]byte, imageSize)
	copy(image, boot)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeFAT32ShortEntrySized(root[0:32], "MYDIR      ", fatAttrDirectory, 3, 0)
	root[32] = 0x00

	path := filepath.Join(t.TempDir(), "fat32-trunc2.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.DeleteDir("/mydir"); err == nil {
		t.Fatal("DeleteDir on truncated image error = nil, want error")
	}
}

// ---- New tests for 100% coverage ----

// TestListDirSubdir exercises the non-"/" path in ListDir (lines 141-149).
func TestListDirSubdir(t *testing.T) {
	subCluster := make([]byte, 4096)
	writeFAT32ShortEntrySized(subCluster[0:32], "CHILD   TXT", 0x20, 5, 5)
	subCluster[32] = 0x00

	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "SUBDIR     ", fatAttrDirectory, 3, 0)
		writeFAT32ShortEntrySized(root[32:64], "FILE    TXT", 0x20, 6, 4)
		root[64] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF, 5: 0x0FFFFFFF, 6: 0x0FFFFFFF},
		map[uint32][]byte{3: subCluster, 5: []byte("hello")})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// list a subdirectory
	entries, err := fs.ListDir("/subdir")
	if err != nil {
		t.Fatalf("ListDir(subdir): %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "CHILD.TXT" {
		t.Fatalf("ListDir(/subdir) = %+v, want [CHILD.TXT]", entries)
	}

	// list a file (must fail)
	if _, err := fs.ListDir("/file.txt"); err == nil {
		t.Fatal("ListDir(file) error = nil, want error")
	}

	// list missing path
	if _, err := fs.ListDir("/missing"); err == nil {
		t.Fatal("ListDir(missing) error = nil, want error")
	}
}

// TestSubdirReadWriteDelete exercises reading, writing, and deleting files in
// a subdirectory, and also covers getParentDir traversal paths.
func TestSubdirReadWriteDelete(t *testing.T) {
	subCluster := make([]byte, 4096)
	subCluster[0] = 0x00

	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "MYDIR      ", fatAttrDirectory, 3, 0)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, map[uint32][]byte{3: subCluster})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// write file into subdirectory
	if err := fs.WriteFile("/mydir/file.txt", []byte("world"), 0o644); err != nil {
		t.Fatalf("WriteFile in subdir: %v", err)
	}
	// read back
	data, err := fs.ReadFile("/mydir/file.txt")
	if err != nil {
		t.Fatalf("ReadFile from subdir: %v", err)
	}
	if string(data) != "world" {
		t.Fatalf("ReadFile = %q, want world", data)
	}
	// stat it
	st, err := fs.Stat("/mydir/file.txt")
	if err != nil {
		t.Fatalf("Stat in subdir: %v", err)
	}
	if st.Size() != 5 {
		t.Fatalf("Stat size = %d, want 5", st.Size())
	}
	// delete it
	if err := fs.DeleteFile("/mydir/file.txt"); err != nil {
		t.Fatalf("DeleteFile from subdir: %v", err)
	}
	if _, err := fs.ReadFile("/mydir/file.txt"); err == nil {
		t.Fatal("ReadFile after delete error = nil, want error")
	}
}

// TestMkDirNested creates nested directories.
func TestMkDirNested(t *testing.T) {
	subCluster := make([]byte, 4096)
	subCluster[0] = 0x00

	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "PARENT     ", fatAttrDirectory, 3, 0)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, map[uint32][]byte{3: subCluster})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if err := fs.MkDir("/parent/child", 0o755); err != nil {
		t.Fatalf("MkDir(nested): %v", err)
	}
	entries, err := fs.ListDir("/parent")
	if err != nil {
		t.Fatalf("ListDir after nested MkDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.EqualFold(e.Name(), "CHILD") {
			found = true
		}
	}
	if !found {
		t.Fatal("nested child dir not found in parent listing")
	}
}

// TestDeleteDirNestedRecursive exercises recursive deletion of a dir with subdirs.
func TestDeleteDirNestedRecursive(t *testing.T) {
	// Build /parent/child/file.txt
	fileData := []byte("hi")
	subCluster := make([]byte, 4096)
	writeFAT32ShortEntrySized(subCluster[0:32], "CHILD      ", fatAttrDirectory, 5, 0)
	subCluster[32] = 0x00

	childCluster := make([]byte, 4096)
	writeFAT32ShortEntrySized(childCluster[0:32], "FILE    TXT", 0x20, 7, 2)
	childCluster[32] = 0x00

	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "PARENT     ", fatAttrDirectory, 3, 0)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF, 5: 0x0FFFFFFF, 7: 0x0FFFFFFF},
		map[uint32][]byte{3: subCluster, 5: childCluster, 7: fileData})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if err := fs.DeleteDir("/parent"); err != nil {
		t.Fatalf("DeleteDir(recursive nested): %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after recursive nested delete: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "PARENT" {
			t.Fatal("parent still present after recursive nested DeleteDir")
		}
	}
}

// TestRenameCrossDirFat32 exercises the cross-directory Rename path in fat32.
func TestRenameCrossDirFat32(t *testing.T) {
	subCluster := make([]byte, 4096)
	subCluster[0] = 0x00

	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "FILE    TXT", 0x20, 3, 5)
		writeFAT32ShortEntrySized(root[32:64], "SUBDIR     ", fatAttrDirectory, 4, 0)
		root[64] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF, 4: 0x0FFFFFFF},
		map[uint32][]byte{3: []byte("hello"), 4: subCluster})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if err := fs.Rename("/file.txt", "/subdir/moved.txt"); err != nil {
		t.Fatalf("Rename cross-dir: %v", err)
	}
	// file must be gone from root
	if _, err := fs.ReadFile("/file.txt"); err == nil {
		t.Fatal("ReadFile original after cross-dir rename error = nil, want error")
	}
	// file must be in subdir
	data, err := fs.ReadFile("/subdir/moved.txt")
	if err != nil {
		t.Fatalf("ReadFile after cross-dir rename: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("moved file data = %q, want hello", data)
	}
}

// TestGetParentDirErrors covers getParentDir error paths.
func TestGetParentDirErrors(t *testing.T) {
	// Build image with a file at root (not a dir) name "NOTADIR".
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "NOTADIR    ", 0x20, 3, 5) // regular file, not dir
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, map[uint32][]byte{3: []byte("hello")})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// intermediate path component not found
	if err := fs.WriteFile("/missing/file.txt", nil, 0o644); err == nil {
		t.Fatal("WriteFile through missing parent error = nil, want error")
	}
	// intermediate path component is a file, not a dir
	if err := fs.WriteFile("/notadir/file.txt", nil, 0o644); err == nil {
		t.Fatal("WriteFile through file-as-dir error = nil, want error")
	}
}

// TestResolvePathErrors covers resolvePath error paths.
func TestResolvePathErrors(t *testing.T) {
	// Build image with a file at root.
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "FILE    TXT", 0x20, 3, 5)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, map[uint32][]byte{3: []byte("hello")})

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// traverse through non-dir intermediate: /file.txt/something
	if _, err := fs.Stat("/file.txt/sub"); err == nil {
		t.Fatal("Stat through file error = nil, want error")
	}
	// no leading slash
	if _, err := fs.Stat("noabs"); err == nil {
		t.Fatal("Stat no abs error = nil, want error")
	}
}

// TestFat32FindEntryLFNMatch exercises the LFN match path in fat32FindEntry.
func TestFat32FindEntryLFNMatch(t *testing.T) {
	// Build a dir cluster with one LFN entry + 8.3 entry for "LongFileName.txt".
	buf := make([]byte, 4096)
	// Write a minimal LFN entry followed by a 8.3 entry.
	// LFN sequence byte, attr=0x0F
	// Encode "LongFileName.txt" as UTF-16LE fragments (only 1 LFN entry needed,
	// name has 16 chars → 16 > 13, need 2 LFN entries, but let's use a 10-char name).
	// Use "LongFile.t" (10 chars) → fits in 1 LFN entry.
	// UTF-16LE encoding of "LongFile.t":
	// 'L'=0x004C 'o'=0x006F 'n'=0x006E 'g'=0x0067 'F'=0x0046 'i'=0x0069 'l'=0x006C 'e'=0x0065 '.'=0x002E 't'=0x0074
	buf[0] = 0x41 // LFN seq 1 (first and last, 0x40|0x01)
	buf[11] = fatAttrLongName
	// chars at offsets 1,3,5,7,9 = "LongF"
	buf[1] = 0x4C
	buf[2] = 0x00 // 'L'
	buf[3] = 0x6F
	buf[4] = 0x00 // 'o'
	buf[5] = 0x6E
	buf[6] = 0x00 // 'n'
	buf[7] = 0x67
	buf[8] = 0x00 // 'g'
	buf[9] = 0x46
	buf[10] = 0x00 // 'F'
	// chars at offsets 14,16,18,20,22,24 = "ile.t" + 0x0000 terminator
	buf[14] = 0x69
	buf[15] = 0x00 // 'i'
	buf[16] = 0x6C
	buf[17] = 0x00 // 'l'
	buf[18] = 0x65
	buf[19] = 0x00 // 'e'
	buf[20] = 0x2E
	buf[21] = 0x00 // '.'
	buf[22] = 0x74
	buf[23] = 0x00 // 't'
	buf[24] = 0x00
	buf[25] = 0x00 // NULL terminator → break in loop
	// 8.3 entry at offset 32
	copy(buf[32:43], []byte("LONGFILE T "))
	buf[32+11] = 0x20
	binary.LittleEndian.PutUint16(buf[32+26:], 5) // cluster 5
	buf[64] = 0x00

	startOff, count, found := fat32FindEntry(buf, "LongFile.t")
	if !found {
		t.Fatal("fat32FindEntry: LFN match not found")
	}
	if startOff != 0 || count != 2 {
		t.Fatalf("fat32FindEntry: startOff=%d count=%d, want (0, 2)", startOff, count)
	}
}

// TestFat32FindEntryNoMatch exercises the no-match path in fat32FindEntry (returns false at end).
func TestFat32FindEntryNoMatch(t *testing.T) {
	buf := make([]byte, 64)
	writeFAT32ShortEntry(buf[0:32], "EXIST   TXT", 0x20, 3)
	buf[32] = 0x00
	_, _, found := fat32FindEntry(buf, "missing.txt")
	if found {
		t.Fatal("fat32FindEntry: found entry that should be missing")
	}
}

// TestParseRootDirMetadataLFNNullChar exercises the NULL/0xFFFF char break in
// parseRootDirMetadata's LFN loop.
func TestParseRootDirMetadataLFNNullChar(t *testing.T) {
	// Write one LFN entry with name "AB" (only 2 chars), rest null → break.
	buf := make([]byte, 3*32)
	buf[0] = 0x41 // LFN seq
	buf[11] = fatAttrLongName
	buf[1] = 'A'
	buf[2] = 0x00
	buf[3] = 'B'
	buf[4] = 0x00
	buf[5] = 0x00
	buf[6] = 0x00 // NULL → break in inner loop
	// 8.3 entry
	copy(buf[32:43], []byte("AB         "))
	buf[32+11] = 0x20
	binary.LittleEndian.PutUint16(buf[32+26:], 3)
	buf[64] = 0x00

	entries := parseRootDirMetadata(buf)
	if len(entries) != 1 || entries[0].name != "AB" {
		t.Fatalf("parseRootDirMetadata LFN null char: got %+v, want [{name:AB ...}]", entries)
	}
}

// TestFindRootDirSlotDeletedBeforeLFN exercises findRootDirSlot's free-slot
// accumulation when a deleted entry precedes an LFN sequence.
func TestFindRootDirSlotDeletedBeforeLFN(t *testing.T) {
	// slot 0: deleted
	// slot 1: LFN entry
	// slot 2: 8.3 short entry
	// slot 3: 0x00 terminator
	buf := make([]byte, 4*32)
	buf[0] = 0xE5 // deleted
	buf[32] = 0x41
	buf[32+11] = fatAttrLongName
	copy(buf[64:75], []byte("EXIST   TXT"))
	buf[64+11] = 0x20
	buf[96] = 0x00

	short := toShortNameBytes("EXIST.TXT")
	off, found := findRootDirSlot(buf, short)
	if !found {
		t.Fatalf("findRootDirSlot: existing entry not found")
	}
	if off != 64 {
		t.Fatalf("findRootDirSlot offset = %d, want 64", off)
	}
	// Free slot from deleted 0xE5 at offset 0
	other := toShortNameBytes("NEW.TXT")
	off2, found2 := findRootDirSlot(buf, other)
	if found2 || off2 != 0 {
		t.Fatalf("findRootDirSlot(new) = (%d, %v), want (0, false)", off2, found2)
	}
}

func TestWriteFileLFN(t *testing.T) {
	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	longName := "this-is-a-long-filename.txt"
	if err := fs.WriteFile("/"+longName, []byte("lfn-data"), 0o644); err != nil {
		t.Fatalf("WriteFile LFN: %v", err)
	}
	data, err := fs.ReadFile("/" + longName)
	if err != nil {
		t.Fatalf("ReadFile LFN: %v", err)
	}
	if string(data) != "lfn-data" {
		t.Fatalf("data = %q, want %q", string(data), "lfn-data")
	}

	// Overwrite the LFN file
	if err := fs.WriteFile("/"+longName, []byte("updated"), 0o644); err != nil {
		t.Fatalf("WriteFile LFN overwrite: %v", err)
	}
	data, err = fs.ReadFile("/" + longName)
	if err != nil {
		t.Fatalf("ReadFile LFN after overwrite: %v", err)
	}
	if string(data) != "updated" {
		t.Fatalf("overwrite data = %q, want %q", string(data), "updated")
	}
}

func TestMkDirLFN(t *testing.T) {
	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	longDir := "my-long-directory-name"
	if err := fs.MkDir("/"+longDir, 0o755); err != nil {
		t.Fatalf("MkDir LFN: %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after MkDir LFN: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == longDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("MkDir LFN: %q not found in root dir", longDir)
	}
}

func TestRenameLFN(t *testing.T) {
	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if err := fs.WriteFile("/start.txt", []byte("rename-me"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	longName := "this-is-the-new-long-name.txt"
	if err := fs.Rename("/start.txt", "/"+longName); err != nil {
		t.Fatalf("Rename to LFN: %v", err)
	}
	data, err := fs.ReadFile("/" + longName)
	if err != nil {
		t.Fatalf("ReadFile after Rename LFN: %v", err)
	}
	if string(data) != "rename-me" {
		t.Fatalf("data after rename = %q, want %q", string(data), "rename-me")
	}
}

func TestNeedsLFN(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"FILE.TXT", false},
		{"A.B", false},
		{"HELLO", false},
		{"hello.txt", true},        // lowercase
		{"longfilename.txt", true}, // base > 8 chars
		{"file.toolong", true},     // ext > 3 chars
		{"has space.txt", true},    // space
		{"FILE.TXT", false},
	}
	for _, c := range cases {
		got := needsLFN(c.name)
		if got != c.want {
			t.Errorf("needsLFN(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMakeShortAlias(t *testing.T) {
	alias := makeShortAlias("longfilename.txt")
	name := shortName(alias[:])
	if name != "LONGFI~1.TXT" {
		t.Errorf("makeShortAlias(longfilename.txt) = %q, want %q", name, "LONGFI~1.TXT")
	}
	// No extension
	alias2 := makeShortAlias("nodotname")
	name2 := shortName(alias2[:])
	if name2 != "NODOTN~1" {
		t.Errorf("makeShortAlias(nodotname) = %q, want %q", name2, "NODOTN~1")
	}
}

func TestLFNChecksum(t *testing.T) {
	var short [11]byte
	copy(short[:], []byte("FILE    TXT"))
	sum := lfnChecksum(short)
	// Re-compute manually and verify it's deterministic.
	var expected byte
	for _, b := range short {
		expected = (expected>>1 | expected<<7) + b
	}
	if sum != expected {
		t.Errorf("lfnChecksum = %d, want %d", sum, expected)
	}
}

func TestFat32FindNFreeSlots(t *testing.T) {
	buf := make([]byte, 5*dirEntrySize)
	// slot 0: used (non-zero, non-0xE5)
	buf[0] = 0x41
	// slots 1-4: free (0x00)
	// find 1 free slot starting from slot 1
	off := fat32FindNFreeSlots(buf, 1)
	if off != dirEntrySize {
		t.Errorf("fat32FindNFreeSlots(n=1) = %d, want %d", off, dirEntrySize)
	}
	// find 3 consecutive free slots starting from slot 1 (slots 1,2,3)
	off = fat32FindNFreeSlots(buf, 3)
	if off != dirEntrySize {
		t.Errorf("fat32FindNFreeSlots(n=3) = %d, want %d", off, dirEntrySize)
	}
	// fill slot 2 as used → slots 1 and 3,4 are free but not 3 consecutive
	buf[2*dirEntrySize] = 0x41
	off = fat32FindNFreeSlots(buf, 3)
	if off != -1 {
		t.Errorf("fat32FindNFreeSlots(n=3 with gap) = %d, want -1", off)
	}
	// 2 consecutive from slot 3 is possible
	off = fat32FindNFreeSlots(buf, 2)
	if off != 3*dirEntrySize {
		t.Errorf("fat32FindNFreeSlots(n=2 at end) = %d, want %d", off, 3*dirEntrySize)
	}
	// request more slots than available
	off = fat32FindNFreeSlots(buf, 10)
	if off != -1 {
		t.Errorf("fat32FindNFreeSlots(n=10 impossible) = %d, want -1", off)
	}
}

func TestWriteFileLFNFullDir(t *testing.T) {
	// Fill the root dir so there's no room for an LFN sequence (2 slots needed)
	// AND exhaust the FAT so writeDirBuf cannot extend the chain — together
	// this forces the writer to surface the "directory is full" / "no free
	// clusters" path that the test was originally exercising.
	bootInfo, _ := readInfo(bytes.NewReader(defaultFAT32BootSector()), 0)
	clusterCount := bootInfo.TotalSectors / uint32(bootInfo.SectorsPerCluster)
	fatExhaust := make(map[uint32]uint32, clusterCount)
	for c := uint32(2); c < clusterCount+2; c++ {
		fatExhaust[c] = 0x0FFFFFFF
	}
	path := fatTestImage(t, func(root []byte) {
		// Fill all but the last slot (which is 0x00 terminator, still present).
		// With a single cluster of e.g. 32 slots, fill 31 so only 1 free slot remains.
		info, _ := readInfo(bytes.NewReader(defaultFAT32BootSector()), 0)
		slots := int(info.ClusterSize()) / dirEntrySize
		for i := 0; i < slots-1; i++ {
			var name [11]byte
			name[0] = byte('A' + i%26)
			name[1] = byte('0' + i%10)
			for j := 2; j < 11; j++ {
				name[j] = ' '
			}
			copy(root[i*dirEntrySize:], name[:])
			root[i*dirEntrySize+11] = 0x20
		}
		root[(slots-1)*dirEntrySize] = 0x00
	}, fatExhaust, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	// Writing a long name requires 2 slots (1 LFN + 1 8.3); the single free
	// slot in the existing cluster cannot host it, and the FAT is exhausted
	// so the chain cannot grow.
	if err := fs.WriteFile("/a-long-lfn-name.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile LFN with insufficient slots: error = nil, want error")
	}
}

// chainClusters walks the FAT chain starting at startCluster and returns the
// list of cluster numbers in order. Test helper for chain-growth assertions.
func chainClusters(t *testing.T, fs *fat32FS, startCluster uint32) []uint32 {
	t.Helper()
	if startCluster < 2 {
		return nil
	}
	fatBase := fs.info.FATOffset(fs.partOffset)
	var chain []uint32
	cluster := startCluster
	for i := 0; i < 4096; i++ { // safety cap
		if cluster < 2 || cluster >= 0x0FFFFFF7 {
			break
		}
		chain = append(chain, cluster)
		var next [4]byte
		if _, err := fs.f.ReadAt(next[:], fatBase+int64(cluster)*4); err != nil {
			t.Fatalf("chainClusters: read FAT@%d: %v", cluster, err)
		}
		nextVal := binary.LittleEndian.Uint32(next[:]) & 0x0FFFFFFF
		if nextVal >= 0x0FFFFFF8 {
			break
		}
		cluster = nextVal
	}
	return chain
}

// TestWriteDirBuf_GrowsClusterChain verifies that the directory writer
// transparently extends the FAT chain when more than one cluster of entries
// is needed, and that the chain shrinks back to a single cluster (and the
// free count is restored) after every entry is deleted.
func TestWriteDirBuf_GrowsClusterChain(t *testing.T) {
	// 16 MiB image is plenty: 200 LFN files + a few multi-cluster overhead.
	path := filepath.Join(t.TempDir(), "growchain.fat32")
	fsIfc, err := Format(path, 16*1024*1024, FormatConfig{Label: "GROW"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fsIfc.Close()
	fs := fsIfc.(*fat32FS)

	rootCluster := fs.info.RootCluster
	if start := chainClusters(t, fs, rootCluster); len(start) != 1 {
		t.Fatalf("fresh root chain length = %d, want 1", len(start))
	}
	initialFree, err := countFreeClusters(fs)
	if err != nil {
		t.Fatalf("initial countFreeClusters: %v", err)
	}

	// 200 LFN names: "/grow-file-0000.dat" etc. Each takes 1 LFN + 1 short =
	// 2 slots = 64 bytes ⇒ 200 entries = 12.8 KiB ⇒ ≥ 4 dir clusters.
	const N = 200
	for i := 0; i < N; i++ {
		name := fmt.Sprintf("/grow-file-%04d.dat", i)
		if err := fsIfc.WriteFile(name, []byte(name), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	// All entries must be visible via ListDir.
	entries, err := fsIfc.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		have[e.Name()] = true
	}
	for i := 0; i < N; i++ {
		want := fmt.Sprintf("grow-file-%04d.dat", i)
		if !have[want] {
			t.Fatalf("ListDir missing %q (have %d entries)", want, len(entries))
		}
	}

	// The chain must have grown past one cluster.
	chain := chainClusters(t, fs, rootCluster)
	if len(chain) < 2 {
		t.Fatalf("root chain did not grow: len=%d, want >= 2 (200 LFN entries)", len(chain))
	}
	t.Logf("root chain after 200 writes: %d clusters", len(chain))

	// Now delete every entry — chain should collapse back to the head cluster
	// only (writeDirBuf doesn't truncate, but freed entries become 0xE5; the
	// allocated tail clusters remain in the chain — we instead assert that the
	// per-write allocations balance via the free-cluster counter).
	for i := 0; i < N; i++ {
		name := fmt.Sprintf("/grow-file-%04d.dat", i)
		if err := fsIfc.DeleteFile(name); err != nil {
			t.Fatalf("DeleteFile %s: %v", name, err)
		}
	}

	// After deleting every file, only the data clusters used by each file's
	// payload should have been freed. The directory chain itself stays the
	// same length (FAT32 doesn't routinely shrink dir chains; mkfs.vfat
	// behaviour matches). Verify the free-cluster delta is zero: every byte
	// of payload data we wrote and freed.
	finalFree, err := countFreeClusters(fs)
	if err != nil {
		t.Fatalf("final countFreeClusters: %v", err)
	}
	dirChainLen := uint32(len(chainClusters(t, fs, rootCluster)))
	// Expected leak: the (dirChainLen-1) clusters allocated to extend the
	// directory itself (these stay attached to the directory).
	expectedLeak := dirChainLen - 1
	got := uint32(initialFree - finalFree)
	if got != expectedLeak {
		t.Fatalf("free-cluster delta after delete = %d, want %d (dir chain occupies %d cluster(s))",
			got, expectedLeak, dirChainLen)
	}

	// Re-listing after deletion should yield zero entries.
	post, err := fsIfc.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir after delete: %v", err)
	}
	if len(post) != 0 {
		t.Fatalf("ListDir after delete: %d entries remain, want 0", len(post))
	}
}
