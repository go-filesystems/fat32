package filesystem_fat32

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type errorReaderAt struct {
	data       []byte
	failOffset int64
}

func (reader errorReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off == reader.failOffset {
		return 0, io.ErrUnexpectedEOF
	}
	if off >= int64(len(reader.data)) {
		return 0, io.EOF
	}
	n := copy(p, reader.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// mockDisk is an in-memory disk that can inject read/write errors at specific offsets.
type mockDisk struct {
	data     []byte
	readErr  func(off int64) error
	writeErr func(off int64) error
}

func (m *mockDisk) ReadAt(p []byte, off int64) (int, error) {
	if m.readErr != nil {
		if err := m.readErr(off); err != nil {
			return 0, err
		}
	}
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *mockDisk) WriteAt(p []byte, off int64) (int, error) {
	if m.writeErr != nil {
		if err := m.writeErr(off); err != nil {
			return 0, err
		}
	}
	need := int(off) + len(p)
	if need > len(m.data) {
		m.data = append(m.data, make([]byte, need-len(m.data))...)
	}
	copy(m.data[off:], p)
	return len(p), nil
}

func (m *mockDisk) Close() error { return nil }

// newMockFS builds a fat32FS backed by a mockDisk pre-loaded with data.
func newMockFS(data []byte, info Info, partOffset int64) *fat32FS {
	return &fat32FS{f: &mockDisk{data: data}, partOffset: partOffset, info: info}
}

func newMockFSWithErrors(data []byte, info Info, partOffset int64, readErr, writeErr func(int64) error) *fat32FS {
	return &fat32FS{f: &mockDisk{data: data, readErr: readErr, writeErr: writeErr}, partOffset: partOffset, info: info}
}

func TestOpenBareImageAndInfoHelpers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fat32.img")
	if err := os.WriteFile(path, defaultFAT32BootSector(), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs := openTestFS(t, path)

	info := fs.Info()
	if fs.PartitionOffset() != 0 {
		t.Fatalf("PartitionOffset() = %d, want 0", fs.PartitionOffset())
	}
	if info.OEMName != "MSDOS5.0" {
		t.Fatalf("OEMName = %q, want MSDOS5.0", info.OEMName)
	}
	if info.VolumeLabel != "MOCKFS" {
		t.Fatalf("VolumeLabel = %q, want MOCKFS", info.VolumeLabel)
	}
	if info.TypeLabel != "FAT32" {
		t.Fatalf("TypeLabel = %q, want FAT32", info.TypeLabel)
	}
	if got, want := info.ClusterSize(), uint32(4096); got != want {
		t.Fatalf("ClusterSize() = %d, want %d", got, want)
	}
	if got, want := info.FATOffset(0), int64(32*512); got != want {
		t.Fatalf("FATOffset() = %d, want %d", got, want)
	}
	if got, want := info.DataOffset(0), int64((32+2*128)*512); got != want {
		t.Fatalf("DataOffset() = %d, want %d", got, want)
	}
	if got, want := info.RootDirOffset(0), int64((32+2*128)*512); got != want {
		t.Fatalf("RootDirOffset() = %d, want %d", got, want)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOpenWithMBRPartition(t *testing.T) {
	image := make([]byte, 4096*sectorSize)
	writeMBRPartition(image, 0, 2048)
	copy(image[2048*sectorSize:], defaultFAT32BootSector())

	path := filepath.Join(t.TempDir(), "fat32-mbr.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs := openTestFS(t, path)
	defer fs.Close()

	if got, want := fs.PartitionOffset(), int64(2048*sectorSize); got != want {
		t.Fatalf("PartitionOffset() = %d, want %d", got, want)
	}
}

func TestListDirRoot(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	root := image[rootDirOffsetFromBoot(boot):]
	writeFAT32ShortEntry(root[0:32], "README  TXT", 0x20, 5)
	writeFAT32LFNEntry(root[32:64])
	writeFAT32ShortEntry(root[64:96], "SUBDIR     ", 0x10, 7)
	root[96] = 0xE5
	root[128] = 0x00

	path := filepath.Join(t.TempDir(), "fat32-list.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].Name() != "README.TXT" || entries[0].Inode() != 5 || entries[0].FileType() != 0x20 {
		t.Fatalf("entries[0] = %+v, want README.TXT cluster 5 file", entries[0])
	}
	if entries[1].Name() != "SUBDIR" || entries[1].Inode() != 7 || entries[1].FileType() != 0x10 {
		t.Fatalf("entries[1] = %+v, want SUBDIR cluster 7 dir", entries[1])
	}
}

func TestStatRootAndEntries(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	root := image[rootDirOffsetFromBoot(boot):]
	writeFAT32ShortEntrySized(root[0:32], "README  TXT", fatAttrReadOnly, 5, 1234)
	writeFAT32ShortEntrySized(root[32:64], "SUBDIR     ", fatAttrDirectory, 7, 0)
	root[64] = 0x00

	path := filepath.Join(t.TempDir(), "fat32-stat.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs := openTestFS(t, path)
	defer fs.Close()

	info := fs.Info()

	rootStat, err := fs.Stat("/")
	if err != nil {
		t.Fatalf("Stat(/): %v", err)
	}
	if rootStat.Mode() != fatModeDir || rootStat.Size() != uint64(info.ClusterSize()) || rootStat.Inode() != uint64(info.RootCluster) {
		t.Fatalf("rootStat = (%o,%d,%d), want (%o,%d,%d)", rootStat.Mode(), rootStat.Size(), rootStat.Inode(), fatModeDir, info.ClusterSize(), info.RootCluster)
	}

	fileStat, err := fs.Stat("/readme.txt")
	if err != nil {
		t.Fatalf("Stat(/readme.txt): %v", err)
	}
	if fileStat.Mode() != fatModeFileRO || fileStat.Size() != 1234 || fileStat.Inode() != 5 {
		t.Fatalf("fileStat = (%o,%d,%d), want (%o,1234,5)", fileStat.Mode(), fileStat.Size(), fileStat.Inode(), fatModeFileRO)
	}

	dirStat, err := fs.Stat("/SUBDIR")
	if err != nil {
		t.Fatalf("Stat(/SUBDIR): %v", err)
	}
	if dirStat.Mode() != fatModeDir || dirStat.Size() != 0 || dirStat.Inode() != 7 {
		t.Fatalf("dirStat = (%o,%d,%d), want (%o,0,7)", dirStat.Mode(), dirStat.Size(), dirStat.Inode(), fatModeDir)
	}
}

func TestListDirErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fat32.img")
	boot := defaultFAT32BootSector()
	if err := os.WriteFile(path, boot, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if _, err := fs.ListDir("/nested"); err == nil {
		t.Fatal("ListDir() error = nil, want unsupported path error")
	}
	if _, err := fs.ListDir("/"); err == nil {
		t.Fatal("ListDir() error = nil, want root read error on truncated image")
	}
}

func TestStatErrors(t *testing.T) {
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	root := image[rootDirOffsetFromBoot(boot):]
	writeFAT32ShortEntrySized(root[0:32], "README  TXT", 0x20, 5, 42)
	root[32] = 0x00

	path := filepath.Join(t.TempDir(), "fat32-stat-errors.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	if _, err := fs.Stat("README.TXT"); err == nil {
		t.Fatal("Stat() error = nil, want unsupported relative path error")
	}
	if _, err := fs.Stat("/nested/file"); err == nil {
		t.Fatal("Stat() error = nil, want nested path error")
	}
	if _, err := fs.Stat("/missing.txt"); err == nil {
		t.Fatal("Stat() error = nil, want not found error")
	}

	truncatedPath := filepath.Join(t.TempDir(), "fat32-truncated.img")
	if err := os.WriteFile(truncatedPath, boot, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	truncated, err := Open(truncatedPath, -1)
	if err != nil {
		t.Fatalf("Open(truncated): %v", err)
	}
	defer truncated.Close()
	if _, err := truncated.Stat("/README.TXT"); err == nil {
		t.Fatal("Stat() error = nil, want root read error on truncated image")
	}
}

func TestParseRootDirEntries(t *testing.T) {
	buf := make([]byte, 5*32)
	writeFAT32ShortEntry(buf[0:32], "KERNEL     ", 0x20, 9)
	writeFAT32LFNEntry(buf[32:64])
	writeFAT32ShortEntry(buf[64:96], "CONFIG  SYS", 0x20, 11)
	buf[96] = 0xE5
	buf[128] = 0x00

	entries := parseRootDirEntries(buf)
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].Name() != "KERNEL" {
		t.Fatalf("entries[0].Name() = %q, want KERNEL", entries[0].Name())
	}
	if entries[1].Name() != "CONFIG.SYS" {
		t.Fatalf("entries[1].Name() = %q, want CONFIG.SYS", entries[1].Name())
	}

	noTerminator := make([]byte, 32)
	writeFAT32ShortEntry(noTerminator, "BOOT       ", 0x20, 3)
	entries = parseRootDirEntries(noTerminator)
	if len(entries) != 1 || entries[0].Name() != "BOOT" {
		t.Fatalf("parseRootDirEntries(no terminator) = %+v, want single BOOT entry", entries)
	}
}

func TestRootHelpers(t *testing.T) {
	if got := (rootDirEntry{attr: fatAttrDirectory | fatAttrReadOnly}).mode(); got != fatModeDirRO {
		t.Fatalf("mode(readonly dir) = %o, want %o", got, fatModeDirRO)
	}
	if got := (rootDirEntry{attr: 0x20}).mode(); got != fatModeFile {
		t.Fatalf("mode(normal file) = %o, want %o", got, fatModeFile)
	}
	name, err := rootPathName("/", "fat32")
	if err != nil {
		t.Fatalf("rootPathName(/): %v", err)
	}
	if name != "" {
		t.Fatalf("rootPathName(/) = %q, want empty string", name)
	}
	// normal name
	name2, err := rootPathName("/file.txt", "fat32")
	if err != nil || name2 != "file.txt" {
		t.Fatalf("rootPathName(/file.txt) = (%q, %v), want (file.txt, nil)", name2, err)
	}
	// no leading slash
	if _, err := rootPathName("noabs.txt", "fat32"); err == nil {
		t.Fatal("rootPathName(no-slash) error = nil, want error")
	}
	// nested path (contains /)
	if _, err := rootPathName("/nested/file.txt", "fat32"); err == nil {
		t.Fatal("rootPathName(nested) error = nil, want error")
	}
}

func TestOpenErrorPaths(t *testing.T) {
	origOpenFile := openFile
	origOpenPartitionOffset := openPartitionOffset
	origOpenReadInfo := openReadInfo
	t.Cleanup(func() {
		openFile = origOpenFile
		openPartitionOffset = origOpenPartitionOffset
		openReadInfo = origOpenReadInfo
	})

	openFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, errors.New("boom")
	}
	if _, err := Open("missing.img", -1); err == nil {
		t.Fatal("Open() error = nil, want error")
	}

	path := filepath.Join(t.TempDir(), "fat32.img")
	if err := os.WriteFile(path, defaultFAT32BootSector(), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	openFile = origOpenFile
	openPartitionOffset = func(io.ReaderAt, int) (int64, error) {
		return 0, errors.New("partition")
	}
	if _, err := Open(path, -1); err == nil {
		t.Fatal("Open() error = nil, want partition error")
	}

	openPartitionOffset = origOpenPartitionOffset
	openReadInfo = func(io.ReaderAt, int64) (Info, error) {
		return Info{}, errors.New("read")
	}
	if _, err := Open(path, -1); err == nil {
		t.Fatal("Open() error = nil, want read error")
	}
}

func TestReadInfoValidationErrors(t *testing.T) {
	if _, err := readInfo(bytes.NewReader([]byte("short")), 0); err == nil {
		t.Fatal("readInfo() error = nil, want short-read error")
	}

	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{name: "bad signature", mutate: func(buf []byte) { buf[510] = 0 }},
		{name: "bad bytes per sector", mutate: func(buf []byte) { binary.LittleEndian.PutUint16(buf[11:], 1000) }},
		{name: "bad sectors per cluster", mutate: func(buf []byte) { buf[13] = 3 }},
		{name: "zero reserved sectors", mutate: func(buf []byte) { binary.LittleEndian.PutUint16(buf[14:], 0) }},
		{name: "zero fat count", mutate: func(buf []byte) { buf[16] = 0 }},
		{name: "fat16 root entries", mutate: func(buf []byte) { binary.LittleEndian.PutUint16(buf[17:], 16) }},
		{name: "zero total sectors", mutate: func(buf []byte) {
			binary.LittleEndian.PutUint16(buf[19:], 0)
			binary.LittleEndian.PutUint32(buf[32:], 0)
		}},
		{name: "zero fat size", mutate: func(buf []byte) {
			binary.LittleEndian.PutUint16(buf[22:], 0)
			binary.LittleEndian.PutUint32(buf[36:], 0)
		}},
		{name: "bad root cluster", mutate: func(buf []byte) { binary.LittleEndian.PutUint32(buf[44:], 1) }},
		{name: "bad type label", mutate: func(buf []byte) { copy(buf[82:90], []byte("NOPE    ")) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buf := defaultFAT32BootSector()
			test.mutate(buf)
			if _, err := readInfo(bytes.NewReader(buf), 0); err == nil {
				t.Fatalf("readInfo() error = nil, want error")
			}
		})
	}
}

func TestPartitionOffsetVariants(t *testing.T) {
	t.Run("bare image", func(t *testing.T) {
		off, err := partitionOffset(bytes.NewReader(make([]byte, sectorSize)), -1)
		if err != nil {
			t.Fatalf("partitionOffset: %v", err)
		}
		if off != 0 {
			t.Fatalf("partitionOffset() = %d, want 0", off)
		}
	})

	t.Run("gpt auto and index", func(t *testing.T) {
		image := make([]byte, 12*sectorSize)
		writeGPT(image, 2, []uint64{2048, 4096})
		if off, err := partitionOffset(bytes.NewReader(image), -1); err != nil || off != int64(2048*sectorSize) {
			t.Fatalf("partitionOffset(auto) = (%d, %v), want (%d, nil)", off, err, 2048*sectorSize)
		}
		if off, err := partitionOffset(bytes.NewReader(image), 1); err != nil || off != int64(4096*sectorSize) {
			t.Fatalf("partitionOffset(index) = (%d, %v), want (%d, nil)", off, err, 4096*sectorSize)
		}
	})

	t.Run("gpt errors", func(t *testing.T) {
		short := make([]byte, sectorSize+8)
		copy(short[sectorSize:], []byte("EFI PART"))
		if _, err := partitionOffset(bytes.NewReader(short), -1); err == nil {
			t.Fatal("partitionOffset() error = nil, want GPT header error")
		}

		badEntrySize := make([]byte, 4*sectorSize)
		writeGPTHeaderOnly(badEntrySize, 2, 64, 1)
		if _, err := partitionOffset(bytes.NewReader(badEntrySize), -1); err == nil {
			t.Fatal("partitionOffset() error = nil, want GPT entry-size error")
		}

		truncated := make([]byte, 4*sectorSize)
		writeGPTHeaderOnly(truncated, 2, 128, 1)
		if _, err := partitionOffset(errorReaderAt{data: truncated, failOffset: 2 * sectorSize}, -1); err == nil {
			t.Fatal("partitionOffset() error = nil, want truncated GPT table error")
		}

		empty := make([]byte, 4*sectorSize)
		writeGPTHeaderOnly(empty, 2, 128, 1)
		if _, err := partitionOffset(bytes.NewReader(empty), -1); err == nil {
			t.Fatal("partitionOffset() error = nil, want missing GPT partition error")
		}
		if _, err := partitionOffset(bytes.NewReader(empty), 0); err == nil {
			t.Fatal("partitionOffset() error = nil, want missing GPT index error")
		}
		if _, err := partitionOffset(bytes.NewReader(empty), 3); err == nil {
			t.Fatal("partitionOffset() error = nil, want out-of-range GPT index error")
		}
	})

	t.Run("mbr auto and index", func(t *testing.T) {
		image := make([]byte, sectorSize)
		writeMBRPartition(image, 1, 4096)
		if off, err := partitionOffset(bytes.NewReader(image), -1); err != nil || off != int64(4096*sectorSize) {
			t.Fatalf("partitionOffset(auto) = (%d, %v), want (%d, nil)", off, err, 4096*sectorSize)
		}
		if off, err := partitionOffset(bytes.NewReader(image), 1); err != nil || off != int64(4096*sectorSize) {
			t.Fatalf("partitionOffset(index) = (%d, %v), want (%d, nil)", off, err, 4096*sectorSize)
		}
	})

	t.Run("mbr errors", func(t *testing.T) {
		image := make([]byte, sectorSize)
		image[510] = 0x55
		image[511] = 0xAA
		if _, err := partitionOffset(errorReaderAt{data: image, failOffset: 446}, -1); err == nil {
			t.Fatal("partitionOffset() error = nil, want MBR read error")
		}
		if off, err := partitionOffset(bytes.NewReader(image), -1); err != nil || off != 0 {
			t.Fatalf("partitionOffset() = (%d, %v), want (0, nil)", off, err)
		}
		if _, err := partitionOffset(bytes.NewReader(image), 0); err == nil {
			t.Fatal("partitionOffset() error = nil, want missing MBR index error")
		}
		if _, err := partitionOffset(bytes.NewReader(image), 5); err == nil {
			t.Fatal("partitionOffset() error = nil, want out-of-range MBR index error")
		}
	})
}

func defaultFAT32BootSector() []byte {
	buf := make([]byte, sectorSize)
	copy(buf[3:11], []byte("MSDOS5.0"))
	binary.LittleEndian.PutUint16(buf[11:], 512)
	buf[13] = 8
	binary.LittleEndian.PutUint16(buf[14:], 32)
	buf[16] = 2
	binary.LittleEndian.PutUint32(buf[32:], 65536)
	binary.LittleEndian.PutUint32(buf[36:], 128)
	binary.LittleEndian.PutUint32(buf[44:], 2)
	binary.LittleEndian.PutUint16(buf[48:], 1)
	binary.LittleEndian.PutUint16(buf[50:], 6)
	buf[64] = 0x80
	buf[66] = 0x29
	binary.LittleEndian.PutUint32(buf[67:], 0x12345678)
	copy(buf[71:82], []byte("MOCKFS     "))
	copy(buf[82:90], []byte("FAT32   "))
	buf[510] = 0x55
	buf[511] = 0xAA
	return buf
}

func writeMBRPartition(image []byte, index int, startLBA uint32) {
	image[510] = 0x55
	image[511] = 0xAA
	entry := image[446+index*16:]
	binary.LittleEndian.PutUint32(entry[8:], startLBA)
	entry[4] = 0x0C
}

func writeGPT(image []byte, entryLBA uint64, starts []uint64) {
	writeGPTHeaderOnly(image, entryLBA, 128, uint32(len(starts)))
	for index, start := range starts {
		entry := image[int(entryLBA)*sectorSize+index*128:]
		entry[0] = byte(index + 1)
		binary.LittleEndian.PutUint64(entry[32:], start)
	}
}

func writeGPTHeaderOnly(image []byte, entryLBA uint64, entrySize uint32, numParts uint32) {
	copy(image[sectorSize:], []byte("EFI PART"))
	binary.LittleEndian.PutUint64(image[sectorSize+72:], entryLBA)
	binary.LittleEndian.PutUint32(image[sectorSize+80:], numParts)
	binary.LittleEndian.PutUint32(image[sectorSize+84:], entrySize)
}

func rootDirOffsetFromBoot(boot []byte) int {
	info, err := readInfo(bytes.NewReader(boot), 0)
	if err != nil {
		panic(err)
	}
	return int(info.RootDirOffset(0))
}

func writeFAT32ShortEntry(entry []byte, name string, attr byte, cluster uint32) {
	writeFAT32ShortEntrySized(entry, name, attr, cluster, 0)
}

func writeFAT32ShortEntrySized(entry []byte, name string, attr byte, cluster uint32, size uint32) {
	copy(entry[0:11], []byte(name))
	entry[11] = attr
	binary.LittleEndian.PutUint16(entry[20:22], uint16(cluster>>16))
	binary.LittleEndian.PutUint16(entry[26:28], uint16(cluster))
	binary.LittleEndian.PutUint32(entry[28:32], size)
}

func writeFAT32LFNEntry(entry []byte) {
	entry[0] = 0x41
	entry[11] = fatAttrLongName
}

// fatTestImage builds a 1 MiB FAT32 image with a given root-dir setup and
// optionally a FAT chain.  fatEntries maps cluster number → next-cluster value
// (use 0x0FFFFFFF for EOF).  clusterData maps cluster number → data bytes.
// FAT entries 0, 1 and the root cluster (2) are always pre-initialised to EOF.
func fatTestImage(t *testing.T, setupRoot func(root []byte), fatEntries map[uint32]uint32, clusterData map[uint32][]byte) string {
	t.Helper()
	image := make([]byte, 1024*1024)
	boot := defaultFAT32BootSector()
	copy(image, boot)
	info, err := readInfo(bytes.NewReader(boot), 0)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	root := image[int(info.RootDirOffset(0)):]
	if setupRoot != nil {
		setupRoot(root)
	}
	fatBase := int(info.FATOffset(0))
	// Pre-fill standard reserved / root-cluster FAT entries.
	for _, c := range []uint32{0, 1, info.RootCluster} {
		binary.LittleEndian.PutUint32(image[fatBase+int(c)*4:], 0x0FFFFFFF)
	}
	for cluster, next := range fatEntries {
		binary.LittleEndian.PutUint32(image[fatBase+int(cluster)*4:], next)
	}
	dataBase := int(info.DataOffset(0))
	cs := int(info.ClusterSize())
	for cluster, data := range clusterData {
		off := dataBase + int(cluster-2)*cs
		copy(image[off:], data)
	}
	path := filepath.Join(t.TempDir(), "fat32-rw.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}
