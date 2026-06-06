package filesystem_fat32

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildFullRootDirImage(t *testing.T) string {
	t.Helper()
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	root := image[int(info.RootDirOffset(0)):]
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	cs := int(info.ClusterSize())
	slots := cs / dirEntrySize
	for i := 0; i < slots; i++ {
		var name [11]byte
		for k := range name {
			name[k] = ' '
		}
		name[0] = byte('A' + i%26)
		name[1] = byte('0' + (i/26)%10)
		name[2] = byte('0' + i%10)
		copy(root[i*dirEntrySize:], name[:])
		root[i*dirEntrySize+11] = 0x20
	}
	// Mark every other FAT entry as allocated so allocCluster fails — this
	// keeps the "directory full" semantic test meaningful now that writeDirBuf
	// transparently extends the root chain when free clusters are available.
	clusterCount := info.TotalSectors / uint32(info.SectorsPerCluster)
	for c := uint32(2); c < clusterCount+2; c++ {
		binary.LittleEndian.PutUint32(image[fatBase+int(c)*4:], 0x0FFFFFFF)
	}
	path := filepath.Join(t.TempDir(), "fat32-fullroot.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}

func TestWriteFileRootDirFull(t *testing.T) {
	path := buildFullRootDirImage(t)
	fs := openTestFS(t, path)
	defer fs.Close()
	if err := fs.WriteFile("/newfile.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile with full root dir error = nil, want error")
	}
}

func TestMkDirRootDirFull(t *testing.T) {
	path := buildFullRootDirImage(t)
	fs := openTestFS(t, path)
	defer fs.Close()
	if err := fs.MkDir("/newdir", 0o755); err == nil {
		t.Fatal("MkDir with full root dir error = nil, want error")
	}
}

func TestReadClusterChainBadCluster(t *testing.T) {
	// FAT[3] = 1 (< 2) — exercises the cluster < 2 guard in readClusterChain.
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "DATA    TXT", 0x20, 3, 5)
		root[32] = 0x00
	}, map[uint32]uint32{3: 1}, map[uint32][]byte{3: []byte("hello")})
	fs := openTestFS(t, path)
	defer fs.Close()
	data, err := fs.ReadFile("/data.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want %q", data, "hello")
	}
}

func TestDeleteDirHasDeletedEntry(t *testing.T) {
	clusterBuf := make([]byte, 4096)
	copy(clusterBuf[0:11], []byte(".          "))
	clusterBuf[11] = fatAttrDirectory
	copy(clusterBuf[32:43], []byte("..         "))
	clusterBuf[43] = fatAttrDirectory
	copy(clusterBuf[64:75], []byte("OLDFILE    "))
	clusterBuf[64] = 0xE5
	clusterBuf[75] = 0x20
	clusterBuf[96] = 0x00

	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "MYDIR      ", fatAttrDirectory, 3, 0)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, map[uint32][]byte{3: clusterBuf})

	fs := openTestFS(t, path)
	defer fs.Close()
	if err := fs.DeleteDir("/mydir"); err != nil {
		t.Fatalf("DeleteDir(dir with deleted entry): %v", err)
	}
}

func TestFindRootDirSlotLFN(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		root[0] = 0x41
		root[11] = fatAttrLongName
		writeFAT32ShortEntry(root[32:64], "EXIST   TXT", 0x20, 3)
		root[64] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, nil)

	fs := openTestFS(t, path)
	defer fs.Close()
	if err := fs.WriteFile("/newfile.txt", []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile past LFN entry: %v", err)
	}
}

func TestWriteDataPartialAllocCleanup(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	clusterCount := info.TotalSectors / uint32(info.SectorsPerCluster)
	for c := uint32(0); c < clusterCount+4; c++ {
		off := fatBase + int(c)*4
		if off+4 > len(image) {
			break
		}
		val := uint32(0x0FFFFFFF)
		if c == 3 {
			val = 0
		}
		binary.LittleEndian.PutUint32(image[off:], val)
	}
	root := image[int(info.RootDirOffset(0)):]
	root[0] = 0x00

	path := filepath.Join(t.TempDir(), "fat32-onefreecluster.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	fs := openTestFS(t, path)
	defer fs.Close()

	bigData := make([]byte, int(info.ClusterSize())+1)
	if err := fs.WriteFile("/big.txt", bigData, 0o644); err == nil {
		t.Fatal("WriteFile partial alloc error = nil, want error")
	}
}

// buildImageWithEntry returns a 1 MiB FAT32 image with one root entry
// "file.txt" at cluster 3, containing "hello", plus the info struct.
func buildImageWithEntry(t *testing.T) ([]byte, Info) {
	t.Helper()
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeFAT32ShortEntrySized(root[0:32], "FILE    TXT", 0x20, 3, 5)
	root[32] = 0x00
	dataBase := int(info.DataOffset(0))
	cs := int(info.ClusterSize())
	copy(image[dataBase+(3-2)*cs:], []byte("hello"))
	return image, info
}

func TestSetFATEntryReadError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("read error")
		}
		return nil
	}, nil)
	if err := fs.setFATEntry(3, 0); err == nil {
		t.Fatal("setFATEntry read error = nil, want error")
	}
}

func TestSetFATEntryWriteError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("write error")
		}
		return nil
	})
	if err := fs.setFATEntry(3, 0); err == nil {
		t.Fatal("setFATEntry write error = nil, want error")
	}
}

func TestAllocClusterReadError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == fatBase+2*4 {
			return errors.New("disk error")
		}
		return nil
	}, nil)
	if _, err := fs.allocCluster(); err == nil {
		t.Fatal("allocCluster read error = nil, want error")
	}
}

func TestFreeChainReadError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("read error in freeChain")
		}
		return nil
	}, nil)
	if err := fs.freeChain(3); err == nil {
		t.Fatal("freeChain read error = nil, want error")
	}
}

func TestFreeChainSetFATError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("write error in setFATEntry via freeChain")
		}
		return nil
	})
	if err := fs.freeChain(3); err == nil {
		t.Fatal("freeChain via setFATEntry write error = nil, want error")
	}
}

func TestReadClusterChainFATReadError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	dataBase := info.DataOffset(0)
	cs := int64(info.ClusterSize())
	clusterDataOff := dataBase + (3-2)*cs
	readClusterDone := false
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == clusterDataOff {
			readClusterDone = true
			return nil
		}
		if readClusterDone && off == fatBase+3*4 {
			return errors.New("FAT read error")
		}
		return nil
	}, nil)
	if _, err := fs.readClusterChain(3, 5); err == nil {
		t.Fatal("readClusterChain FAT read error = nil, want error")
	}
}

func TestWriteDataSetFATEOFError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	// cluster 4 is free (image has 0 there by default after cluster 3 is EOF).
	writeCount := 0
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+4*4 {
			writeCount++
			if writeCount == 1 {
				return errors.New("write EOF mark error")
			}
		}
		return nil
	})
	if _, err := fs.writeData([]byte("x")); err == nil {
		t.Fatal("writeData setFAT EOF error = nil, want error")
	}
}

func TestWriteDataLinkError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	// cluster 4 and 5 are free.
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0)
	binary.LittleEndian.PutUint32(image[fatBase+5*4:], 0)
	clusterSize := int64(info.ClusterSize())
	bigData := make([]byte, clusterSize+1)
	// First write to FAT[4] = mark EOF (alloc), then FAT[5] = mark EOF (alloc),
	// then FAT[4] = 5 (link) must fail.
	writes := map[int64]int{}
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		writes[off]++
		if off == fatBase+4*4 && writes[off] == 2 {
			return errors.New("link write error")
		}
		return nil
	})
	if _, err := fs.writeData(bigData); err == nil {
		t.Fatal("writeData link error = nil, want error")
	}
}

func TestWriteDataWriteClusterPaddedError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0)
	clusterSize := int64(info.ClusterSize())
	dataBase := info.DataOffset(0)
	clusterOff := dataBase + (4-2)*clusterSize
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == clusterOff {
			return errors.New("padded cluster write error")
		}
		return nil
	})
	// 1 byte → needs padding → uses WriteAt(clusterBuf, off).
	if _, err := fs.writeData([]byte("x")); err == nil {
		t.Fatal("writeData padded write error = nil, want error")
	}
}

func TestWriteDataWriteClusterExactError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0)
	clusterSize := int64(info.ClusterSize())
	dataBase := info.DataOffset(0)
	clusterOff := dataBase + (4-2)*clusterSize
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == clusterOff {
			return errors.New("exact cluster write error")
		}
		return nil
	})
	exactData := make([]byte, clusterSize)
	if _, err := fs.writeData(exactData); err == nil {
		t.Fatal("writeData exact write error = nil, want error")
	}
}

func TestWriteRootDirError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	rootOff := info.RootDirOffset(0)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == rootOff {
			return errors.New("root dir write error")
		}
		return nil
	})
	buf := make([]byte, info.ClusterSize())
	if err := fs.writeRootDir(buf); err == nil {
		t.Fatal("writeRootDir write error = nil, want error")
	}
}

func TestWriteFileFreeChainError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("freeChain write error")
		}
		return nil
	})
	if err := fs.WriteFile("/file.txt", []byte("new"), 0o644); err == nil {
		t.Fatal("WriteFile freeChain error = nil, want error")
	}
}

func TestDeleteFileFreeChainError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+3*4 {
			return errors.New("freeChain write error")
		}
		return nil
	})
	if err := fs.DeleteFile("/file.txt"); err == nil {
		t.Fatal("DeleteFile freeChain error = nil, want error")
	}
}

func TestDeleteDirFreeChainError(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeFAT32ShortEntrySized(root[0:32], "EMPTYDIR   ", fatAttrDirectory, 3, 0)
	root[32] = 0x00
	dataBase := int(info.DataOffset(0))
	cs := int(info.ClusterSize())
	dirBuf := image[dataBase+(3-2)*cs:]
	copy(dirBuf[0:11], []byte(".          "))
	dirBuf[11] = fatAttrDirectory
	copy(dirBuf[32:43], []byte("..         "))
	dirBuf[43] = fatAttrDirectory
	dirBuf[64] = 0x00

	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == int64(fatBase)+3*4 {
			return errors.New("freeChain write error")
		}
		return nil
	})
	if err := fs.DeleteDir("/emptydir"); err == nil {
		t.Fatal("DeleteDir freeChain error = nil, want error")
	}
}

func TestMkDirSetFATError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0)
	writeCount := 0
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == fatBase+4*4 {
			writeCount++
			if writeCount == 1 {
				return errors.New("setFATEntry write error")
			}
		}
		return nil
	})
	if err := fs.MkDir("/newdir", 0o755); err == nil {
		t.Fatal("MkDir setFATEntry error = nil, want error")
	}
}

func TestMkDirWriteClusterError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := info.FATOffset(0)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0)
	clusterSize := int64(info.ClusterSize())
	dataBase := info.DataOffset(0)
	clusterOff := dataBase + (4-2)*clusterSize
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == clusterOff {
			return errors.New("cluster write error")
		}
		return nil
	})
	if err := fs.MkDir("/newdir", 0o755); err == nil {
		t.Fatal("MkDir WriteAt cluster error = nil, want error")
	}
}

func TestRenameFreeChainError(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0x0FFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeFAT32ShortEntrySized(root[0:32], "OLD     TXT", 0x20, 3, 5)
	writeFAT32ShortEntrySized(root[32:64], "NEW     TXT", 0x20, 4, 3)
	root[64] = 0x00

	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == int64(fatBase)+4*4 {
			return errors.New("freeChain write error for target")
		}
		return nil
	})
	if err := fs.Rename("/old.txt", "/new.txt"); err == nil {
		t.Fatal("Rename freeChain error = nil, want error")
	}
}

// TestWriteDirBufWriteError covers the WriteAt error branch in writeDirBuf (line 638).
func TestWriteDirBufWriteError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	dataBase := info.DataOffset(0)
	cs := int64(info.ClusterSize())
	rootClusterOff := dataBase + int64(info.RootCluster-2)*cs
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == rootClusterOff {
			return errors.New("inject write error")
		}
		return nil
	})
	buf := make([]byte, info.ClusterSize())
	if err := fs.writeDirBuf(info.RootCluster, buf); err == nil {
		t.Fatal("writeDirBuf write error = nil, want error")
	}
}

// TestWriteDirBufPartialBuf covers the buf-trim branch in writeDirBuf (line 633)
// by passing a buffer smaller than one cluster.
func TestWriteDirBufPartialBuf(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	// Pass a buf smaller than ClusterSize so that end = ClusterSize > len(buf)
	buf := make([]byte, 16) // tiny buf — well under ClusterSize
	if err := fs.writeDirBuf(info.RootCluster, buf); err != nil {
		t.Fatalf("writeDirBuf partial buf: %v", err)
	}
}

// TestWriteDirBufMultiCluster covers multi-cluster iteration (lines 644-650) in writeDirBuf.
func TestWriteDirBufMultiCluster(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := int(info.FATOffset(0))
	cs := int(info.ClusterSize())
	rootCluster := info.RootCluster
	nextCluster := rootCluster + 1
	// chain: rootCluster → nextCluster → EOC
	binary.LittleEndian.PutUint32(image[fatBase+int(rootCluster)*4:], nextCluster)
	binary.LittleEndian.PutUint32(image[fatBase+int(nextCluster)*4:], 0x0FFFFFFF)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	buf := make([]byte, cs*2)
	if err := fs.writeDirBuf(rootCluster, buf); err != nil {
		t.Fatalf("writeDirBuf multi-cluster: %v", err)
	}
}

// TestWriteDirBufFATReadError covers the FAT read error after first cluster (line 645).
func TestWriteDirBufFATReadError(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := int(info.FATOffset(0))
	cs := int(info.ClusterSize())
	rootCluster := info.RootCluster
	nextCluster := rootCluster + 1
	// chain: rootCluster → nextCluster → EOC
	binary.LittleEndian.PutUint32(image[fatBase+int(rootCluster)*4:], nextCluster)
	binary.LittleEndian.PutUint32(image[fatBase+int(nextCluster)*4:], 0x0FFFFFFF)

	fatEntry := int64(info.FATOffset(0)) + int64(rootCluster)*4
	dataBase := info.DataOffset(0)
	rootClusterOff := dataBase + int64(rootCluster-2)*int64(cs)
	clusterWritten := false
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if clusterWritten && off == fatEntry {
			return errors.New("FAT read error in writeDirBuf")
		}
		return nil
	}, func(off int64) error {
		if off == rootClusterOff {
			clusterWritten = true
		}
		return nil
	})
	buf := make([]byte, cs*2)
	if err := fs.writeDirBuf(rootCluster, buf); err == nil {
		t.Fatal("writeDirBuf FAT read error = nil, want error")
	}
}

// TestWriteDirBufClusterBreak covers the cluster<2 break after FAT read (lines 648-650).
func TestWriteDirBufClusterBreak(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fatBase := int(info.FATOffset(0))
	cs := int(info.ClusterSize())
	rootCluster := info.RootCluster
	// Point FAT entry to cluster 1 (< 2) — triggers break guard after first cluster
	binary.LittleEndian.PutUint32(image[fatBase+int(rootCluster)*4:], 1)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	buf := make([]byte, cs*2)
	// Should not error — just stops after first cluster due to invalid next cluster
	if err := fs.writeDirBuf(rootCluster, buf); err != nil {
		t.Fatalf("writeDirBuf cluster<2 break: %v", err)
	}
}

// TestDeleteAllContentsSubdirError covers the recursive subdir error path (line 814).
func TestDeleteAllContentsSubdirError(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	// dir at cluster 3, subdir at cluster 5, file in subdir at cluster 7
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+5*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+7*4:], 0x0FFFFFFF)
	dataBase := int(info.DataOffset(0))
	cs := int(info.ClusterSize())
	// Cluster 3: subdir entry pointing to cluster 5
	dir3 := image[dataBase+(3-2)*cs:]
	writeFAT32ShortEntrySized(dir3[0:32], "SUBDIR     ", fatAttrDirectory, 5, 0)
	dir3[32] = 0x00
	// Cluster 5: file entry pointing to cluster 7
	dir5 := image[dataBase+(5-2)*cs:]
	writeFAT32ShortEntrySized(dir5[0:32], "FILE    TXT", 0x20, 7, 3)
	dir5[32] = 0x00

	// Inject freeChain write error for cluster 7 (the file inside the subdir)
	// This causes deleteAllContents(5) to fail → deleteAllContents(3) returns error (line 814)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == int64(fatBase)+7*4 {
			return errors.New("freeChain error nested file")
		}
		return nil
	})
	if err := fs.deleteAllContents(3); err == nil {
		t.Fatal("deleteAllContents nested subdir error = nil, want error")
	}
}

// TestDeleteAllContentsFileError covers the file freeChain error path (line 819).
func TestDeleteAllContentsFileError(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	// dir at cluster 3, file at cluster 5
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+5*4:], 0x0FFFFFFF)
	dataBase := int(info.DataOffset(0))
	cs := int(info.ClusterSize())
	// Cluster 3: file entry pointing to cluster 5 (no subdirs)
	dir3 := image[dataBase+(3-2)*cs:]
	writeFAT32ShortEntrySized(dir3[0:32], "FILE    TXT", 0x20, 5, 3)
	dir3[32] = 0x00

	// Inject freeChain write error for cluster 5 → hits line 819
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == int64(fatBase)+5*4 {
			return errors.New("freeChain error for file")
		}
		return nil
	})
	if err := fs.deleteAllContents(3); err == nil {
		t.Fatal("deleteAllContents file freeChain error = nil, want error")
	}
}

// TestGetParentDirIntermediateNotFound covers the intermediate not-found error in getParentDir.
func TestGetParentDirIntermediateNotFound(t *testing.T) {
	image, info := buildImageWithEntry(t)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	// "/missing/file.txt" — "missing" not found in root
	_, _, err := fs.getParentDir("/missing/file.txt")
	if err == nil {
		t.Fatal("getParentDir intermediate not found = nil, want error")
	}
}

// TestGetParentDirReadDirBufError covers the readDirBuf error path inside
// getParentDir's for loop (line 784).
func TestGetParentDirReadDirBufError(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF) // subdir cluster
	root := image[int(info.RootDirOffset(0)):]
	writeFAT32ShortEntrySized(root[0:32], "SUBDIR     ", fatAttrDirectory, 3, 0)
	root[32] = 0x00

	// Inject read error on the subdir cluster (cluster 3)
	dataBase := info.DataOffset(0)
	cs := int64(info.ClusterSize())
	subdirOff := dataBase + int64(3-2)*cs
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == subdirOff {
			return errors.New("read error in getParentDir")
		}
		return nil
	}, nil)
	// "/subdir/child/file.txt" — getParentDir walks ["subdir","child"], first reads root (ok),
	// finds "subdir", then tries to readDirBuf(3) which fails → line 784-786
	_, _, err := fs.getParentDir("/subdir/child/file.txt")
	if err == nil {
		t.Fatal("getParentDir readDirBuf error = nil, want error")
	}
}

// TestFindRootDirSlotFoundAfterFree covers the return (offset, true) branch
// when a free slot was already seen before finding the matching entry (line 1120).
// It also covers the freeSlot assignment inside the 0x00 branch (line 1103-1105).
func TestFindRootDirSlotFoundAfterFree(t *testing.T) {
	// First test: slot 0 = 0x00 (free, freeSlot = -1 → sets freeSlot = 0 via line 1103)
	// then break. Returns (0, false).
	buf0 := make([]byte, 2*dirEntrySize)
	buf0[0] = 0x00
	var short0 [11]byte
	copy(short0[:], []byte("NOTHERE    "))
	off0, found0 := findRootDirSlot(buf0, short0)
	if found0 || off0 != 0 {
		t.Fatalf("findRootDirSlot(0x00 first) = (%d, %v), want (0, false)", off0, found0)
	}

	// Second test: slot 0 = deleted (0xE5, freeSlot = 0), slot 1 = matching 8.3 entry
	// → return (dirEntrySize, true) via line 1120, with freeSlot already set.
	buf := make([]byte, 3*dirEntrySize)
	buf[0] = 0xE5 // deleted slot
	copy(buf[dirEntrySize:dirEntrySize+11], []byte("EXIST   TXT"))
	buf[dirEntrySize+11] = 0x20
	buf[2*dirEntrySize] = 0x00

	var short [11]byte
	copy(short[:], []byte("EXIST   TXT"))
	off, found := findRootDirSlot(buf, short)
	if !found || off != dirEntrySize {
		t.Fatalf("findRootDirSlot(found after free) = (%d, %v), want (%d, true)", off, found, dirEntrySize)
	}
}

// TestRenameCrossDirReadDestError covers the readDirBuf error when reading
// the new parent directory in a cross-directory rename (line 400).
func TestRenameCrossDirReadDestError(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0x0FFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeFAT32ShortEntrySized(root[0:32], "FILE    TXT", 0x20, 3, 5)
	writeFAT32ShortEntrySized(root[32:64], "SUBDIR     ", fatAttrDirectory, 4, 0)
	root[64] = 0x00

	// Inject read error on the subdir cluster (cluster 4) — this is the new parent
	dataBase := info.DataOffset(0)
	cs := int64(info.ClusterSize())
	subdirOff := dataBase + int64(4-2)*cs
	fs := newMockFSWithErrors(image, info, 0, func(off int64) error {
		if off == subdirOff {
			return errors.New("read dest dir error")
		}
		return nil
	}, nil)
	if err := fs.Rename("/file.txt", "/subdir/moved.txt"); err == nil {
		t.Fatal("Rename cross-dir readDirBuf dest error = nil, want error")
	}
}

// TestRenameCrossDirDestFull covers the slotOff < 0 case for a full destination
// directory during a cross-directory rename (line 423).
func TestRenameCrossDirDestFull(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF) // file cluster
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0x0FFFFFFF) // subdir cluster
	root := image[int(info.RootDirOffset(0)):]
	writeFAT32ShortEntrySized(root[0:32], "FILE    TXT", 0x20, 3, 5)
	writeFAT32ShortEntrySized(root[32:64], "SUBDIR     ", fatAttrDirectory, 4, 0)
	root[64] = 0x00

	// Fill the subdir cluster completely so fat32FindFreeSlot returns -1
	dataBase := int(info.DataOffset(0))
	cs := int(info.ClusterSize())
	subBuf := image[dataBase+(4-2)*cs:]
	slots := cs / dirEntrySize
	for i := 0; i < slots; i++ {
		var name [11]byte
		for k := range name {
			name[k] = ' '
		}
		name[0] = byte('A' + i%26)
		name[1] = byte('0' + i%10)
		copy(subBuf[i*dirEntrySize:], name[:])
		subBuf[i*dirEntrySize+11] = 0x20
	}
	// Mark every FAT entry as allocated so the chain-extension code path
	// inside ensureDirSlots cannot grow the destination directory.
	clusterCount := info.TotalSectors / uint32(info.SectorsPerCluster)
	for c := uint32(2); c < clusterCount+2; c++ {
		if c == 3 || c == 4 || c == info.RootCluster {
			continue
		}
		binary.LittleEndian.PutUint32(image[fatBase+int(c)*4:], 0x0FFFFFFF)
	}

	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	if err := fs.Rename("/file.txt", "/subdir/moved.txt"); err == nil {
		t.Fatal("Rename cross-dir to full dest dir = nil, want error")
	}
}

// TestRenameCrossDirWriteOldError covers the writeDirBuf(oldParentCluster) error
// during cross-directory rename (line 426).
func TestRenameCrossDirWriteOldError(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+3*4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(image[fatBase+4*4:], 0x0FFFFFFF)
	root := image[int(info.RootDirOffset(0)):]
	writeFAT32ShortEntrySized(root[0:32], "FILE    TXT", 0x20, 3, 5)
	writeFAT32ShortEntrySized(root[32:64], "SUBDIR     ", fatAttrDirectory, 4, 0)
	root[64] = 0x00

	dataBase := int(info.DataOffset(0))
	cs := int(info.ClusterSize())
	subBuf := image[dataBase+(4-2)*cs:]
	subBuf[0] = 0x00 // subdir is empty

	// During cross-dir rename, writeDirBuf(oldParentCluster=root) is called FIRST.
	// Inject write error on the root cluster write.
	rootDirOff := info.DataOffset(0) + int64(info.RootCluster-2)*int64(cs)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == rootDirOff {
			return errors.New("write old parent dir error")
		}
		return nil
	})
	if err := fs.Rename("/file.txt", "/subdir/moved.txt"); err == nil {
		t.Fatal("Rename cross-dir write old parent error = nil, want error")
	}
}

// TestRenameSameDirFull covers the slotOff < 0 case after deleting the old
// entry leaving no free slot in a same-directory rename (line 445).
// We fill the root dir completely with entries, then rename one of them so that
// after marking it as deleted the whole directory is still full of in-use entries
// (no 0xE5 free slot — because we use 0x00 terminator only at the very end and
// delete marks won't help if there are none left).
// Actually the simplest approach: fill root dir except leave exactly one non-zero
// entry at every slot (no 0xE5 and no 0x00 before the last slot), then attempt
// to rename that single entry. After deletion the slot becomes 0xE5, freeing it —
// so this path is tricky. Instead we test it by crafting a dir where after the
// rename-delete the remaining 0xE5 slots are all consumed by LFN detection logic.
//
// Simpler: use a dir with ONLY the source entry and NO trailing 0x00 — so
// fat32FindFreeSlot finds no 0x00 and no 0xE5 (after we overwrite the deleted
// slot ourselves, but that's done by the code). This is hard to trigger via the
// high-level API because the code itself marks the old slot 0xE5.
//
// Easiest: manually test writeDirBuf path by calling Rename where the same-dir
// buf has all slots occupied and the old entry is at the last slot (so after
// marking it 0xE5, fat32FindFreeSlot finds that 0xE5 and returns 0 which is ≥ 0).
// This actually can't hit slotOff < 0 in the same-dir path because the deleted
// slot itself becomes 0xE5 which is then found as free.
//
// Conclusion: line 445 (slotOff < 0 in same-dir) is a defensive guard that
// cannot be triggered via the public API because the deleted-old-entry slot is
// always available as a free slot. We cover it by calling writeDirBuf directly
// with a write error so that the branch is reachable through the same-dir code.
//
// Revised approach: fill root dir fully (no 0x00 terminator, no 0xE5), but set
// source entry to 0xE5 already (so fat32FindFreeSlot finds that as the free slot)
// — this won't hit slotOff < 0 either.
//
// The only way to hit slotOff < 0 in same-dir is if the buffer has every slot
// occupied with a valid non-LFN entry AND the source entry was NOT in the buf
// (which can't happen if the source was found). So this branch is truly
// unreachable via normal logic. We cover it via a mock that:
//  1. Returns a *different* buf for the second readDirBuf call (after delete mark)
//     where all slots are full.
//
// Since newMockFSWithErrors doesn't support that, we skip the public-API test
// and instead verify the guard via a direct unit test on fat32FindFreeSlot.
func TestFat32FindFreeSlotFull(t *testing.T) {
	// All slots occupied (valid non-LFN entries, no 0x00, no 0xE5)
	slots := 4
	buf := make([]byte, slots*dirEntrySize)
	for i := 0; i < slots; i++ {
		buf[i*dirEntrySize] = byte('A' + i)
		buf[i*dirEntrySize+11] = 0x20
	}
	if got := fat32FindFreeSlot(buf); got != -1 {
		t.Fatalf("fat32FindFreeSlot(full) = %d, want -1", got)
	}
}

func TestFat32FindFreeSlotFound(t *testing.T) {
	// First slot is used, second is free (0xE5).
	buf := make([]byte, 3*dirEntrySize)
	buf[0] = 0x41
	buf[dirEntrySize] = 0xE5
	if got := fat32FindFreeSlot(buf); got != dirEntrySize {
		t.Fatalf("fat32FindFreeSlot(free at slot 1) = %d, want %d", got, dirEntrySize)
	}
}

func TestWriteFileShortName(t *testing.T) {
	// "FILE.TXT" fits in 8.3 → writeDirEntry uses the short-name (non-LFN) path.
	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.WriteFile("/FILE.TXT", []byte("short"), 0o644); err != nil {
		t.Fatalf("WriteFile short name: %v", err)
	}
	data, err := fs.ReadFile("/FILE.TXT")
	if err != nil {
		t.Fatalf("ReadFile short name: %v", err)
	}
	if string(data) != "short" {
		t.Fatalf("data = %q, want %q", string(data), "short")
	}
}

// withMaxDirClusters temporarily shrinks the package-level maxDirClusters cap
// so the "directory grew past the cap" branch is testable without building
// gigantic dir chains.
func withMaxDirClusters(t *testing.T, n int) {
	t.Helper()
	old := maxDirClusters
	maxDirClusters = n
	t.Cleanup(func() { maxDirClusters = old })
}

// TestEnsureDirSlotsCap covers the slotOff < 0 branch of ensureDirSlots
// (and the surfacing "directory is full" error in WriteFile / MkDir).
func TestEnsureDirSlotsCap(t *testing.T) {
	withMaxDirClusters(t, 1)
	// Pre-fill the single root cluster with allocated entries so the only
	// way to fit a new entry is to grow — but the cap forbids growth.
	bootInfo, _ := readInfo(bytes.NewReader(defaultFAT32BootSector()), 0)
	cs := int(bootInfo.ClusterSize())
	slots := cs / dirEntrySize
	path := fatTestImage(t, func(root []byte) {
		for i := 0; i < slots; i++ {
			var name [11]byte
			for k := range name {
				name[k] = ' '
			}
			name[0] = byte('A' + i%26)
			name[1] = byte('0' + (i/26)%10)
			name[2] = byte('0' + i%10)
			copy(root[i*dirEntrySize:], name[:])
			root[i*dirEntrySize+11] = 0x20
		}
	}, nil, nil)
	fs := openTestFS(t, path)
	if err := fs.WriteFile("/extra.txt", []byte("x"), 0o644); err == nil ||
		!strings.Contains(err.Error(), "directory is full") {
		t.Fatalf("WriteFile beyond cap: err = %v, want \"directory is full\"", err)
	}
	if err := fs.MkDir("/extradir", 0o755); err == nil ||
		!strings.Contains(err.Error(), "directory is full") {
		t.Fatalf("MkDir beyond cap: err = %v, want \"directory is full\"", err)
	}
}

// TestRenameDestFullBeyondCap covers the Rename slotOff < 0 branch.
func TestRenameDestFullBeyondCap(t *testing.T) {
	withMaxDirClusters(t, 1)
	// Root dir holds one file "A.TXT" plus the maximum number of dummy
	// entries that fill the rest of the cluster, leaving no free slot for
	// the rename target — and the cap prevents extension.
	bootInfo, _ := readInfo(bytes.NewReader(defaultFAT32BootSector()), 0)
	cs := int(bootInfo.ClusterSize())
	slots := cs / dirEntrySize
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "A       TXT", 0x20, 3, 5)
		// Fill all remaining slots so there is zero free space for the rename.
		for i := 1; i < slots; i++ {
			var name [11]byte
			for k := range name {
				name[k] = ' '
			}
			name[0] = byte('A' + i%26)
			name[1] = byte('0' + (i/26)%10)
			name[2] = byte('0' + i%10)
			copy(root[i*dirEntrySize:], name[:])
			root[i*dirEntrySize+11] = 0x20
		}
	}, map[uint32]uint32{3: 0x0FFFFFFF}, nil)
	fs := openTestFS(t, path)
	if err := fs.Rename("/A.TXT", "/renamed-to-a-much-longer-lfn-name.txt"); err == nil ||
		!strings.Contains(err.Error(), "directory is full") {
		t.Fatalf("Rename beyond cap: err = %v, want \"directory is full\"", err)
	}
}

// TestWriteDirBufExtendAllocCluster_Err exercises the allocCluster error path
// inside the chain-extension branch of writeDirBuf.
func TestWriteDirBufExtendAllocCluster_Err(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	// Root cluster is EOC; every other FAT slot is marked allocated.
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	clusterCount := info.TotalSectors / uint32(info.SectorsPerCluster)
	for c := uint32(2); c < clusterCount+2; c++ {
		if c == info.RootCluster {
			continue
		}
		binary.LittleEndian.PutUint32(image[fatBase+int(c)*4:], 0x0FFFFFFF)
	}
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	cs := int(info.ClusterSize())
	buf := make([]byte, cs*2) // forces writeDirBuf to seek a second cluster
	if err := fs.writeDirBuf(info.RootCluster, buf); err == nil ||
		!strings.Contains(err.Error(), "extend directory chain") {
		t.Fatalf("writeDirBuf extend-with-exhausted-FAT: err = %v, want \"extend directory chain\"", err)
	}
}

// TestWriteDirBufExtendSetFATEntry_Err exercises the setFATEntry error paths
// taken when the chain is being extended.
func TestWriteDirBufExtendSetFATEntry_Err(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	cs := int(info.ClusterSize())
	// allocCluster will find cluster 3 (the first zero entry after root).
	// Fail the WriteAt for the new-cluster EOC marker.
	newClusterFATOff := int64(fatBase) + 3*4
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == newClusterFATOff {
			return errors.New("inject: setFATEntry(new, EOC) failed")
		}
		return nil
	})
	buf := make([]byte, cs*2)
	if err := fs.writeDirBuf(info.RootCluster, buf); err == nil {
		t.Fatal("writeDirBuf setFATEntry(new, EOC) error = nil, want error")
	}
}

// TestWriteDirBufExtendLinkPrev_Err exercises the setFATEntry error path when
// linking the previous cluster to the freshly allocated one.
func TestWriteDirBufExtendLinkPrev_Err(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	cs := int(info.ClusterSize())
	// Fail the WriteAt that links the existing root cluster (2) to the newly
	// allocated cluster (3). The new-cluster EOC write must succeed first.
	rootFATOff := int64(fatBase) + int64(info.RootCluster)*4
	newClusterFATOff := int64(fatBase) + 3*4
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		// Allow the EOC write for the new cluster; fail the link-back to root.
		if off == rootFATOff && off != newClusterFATOff {
			return errors.New("inject: link prev cluster failed")
		}
		return nil
	})
	buf := make([]byte, cs*2)
	if err := fs.writeDirBuf(info.RootCluster, buf); err == nil {
		t.Fatal("writeDirBuf setFATEntry(prev, new) error = nil, want error")
	}
}

// TestWriteDirBufExtendZeroNewCluster_Err exercises the WriteAt error path
// when zero-filling the freshly allocated directory cluster.
func TestWriteDirBufExtendZeroNewCluster_Err(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 0x0FFFFFFF)
	cs := int(info.ClusterSize())
	newClusterDataOff := info.DataOffset(0) + int64(3-2)*int64(cs)
	fs := newMockFSWithErrors(image, info, 0, nil, func(off int64) error {
		if off == newClusterDataOff {
			return errors.New("inject: zero new cluster failed")
		}
		return nil
	})
	buf := make([]byte, cs*2)
	if err := fs.writeDirBuf(info.RootCluster, buf); err == nil ||
		!strings.Contains(err.Error(), "zero new directory cluster") {
		t.Fatalf("writeDirBuf zero-new-cluster error = %v, want \"zero new directory cluster\"", err)
	}
}

// TestWriteDirBufCorruptChainTolerated covers the "next < 2" tolerated-break
// branch (this matches readClusterChain's permissive semantics).
func TestWriteDirBufCorruptChainTolerated(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, _ := readInfo(bytes.NewReader(boot), 0)
	fatBase := int(info.FATOffset(0))
	// FAT[root] = 1 (< 2, invalid). writeDirBuf must terminate silently.
	binary.LittleEndian.PutUint32(image[fatBase+int(info.RootCluster)*4:], 1)
	fs := newMockFSWithErrors(image, info, 0, nil, nil)
	cs := int(info.ClusterSize())
	buf := make([]byte, cs*2)
	if err := fs.writeDirBuf(info.RootCluster, buf); err != nil {
		t.Fatalf("writeDirBuf next<2: %v, want nil (tolerated)", err)
	}
}
