package filesystem_fat32

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf16"

	filesystem "github.com/go-filesystems/interface"
)

const (
	sectorSize       = 512
	dirEntrySize     = 32
	fatAttrReadOnly  = 0x01
	fatAttrDirectory = 0x10
	fatAttrLongName  = 0x0F
	fatModeDir       = 0o040755
	fatModeDirRO     = 0o040555
	fatModeFile      = 0o100644
	fatModeFileRO    = 0o100444
)

type rootDirEntry struct {
	cluster uint32
	name    string
	attr    uint8
	size    uint32
}

// Info holds the fields decoded from the FAT32 BIOS parameter block.
type Info struct {
	OEMName           string
	BytesPerSector    uint16
	SectorsPerCluster uint8
	ReservedSectors   uint16
	FATCount          uint8
	TotalSectors      uint32
	FATSize           uint32
	HiddenSectors     uint32
	RootCluster       uint32
	FSInfoSector      uint16
	BackupBootSector  uint16
	DriveNumber       uint8
	VolumeID          uint32
	VolumeLabel       string
	TypeLabel         string
}

// ClusterSize returns the size of one allocation cluster in bytes.
func (info Info) ClusterSize() uint32 {
	return uint32(info.BytesPerSector) * uint32(info.SectorsPerCluster)
}

// FATOffset returns the absolute byte offset of the first FAT.
func (info Info) FATOffset(partOffset int64) int64 {
	return partOffset + int64(info.ReservedSectors)*int64(info.BytesPerSector)
}

// DataOffset returns the absolute byte offset of the first data cluster.
func (info Info) DataOffset(partOffset int64) int64 {
	return partOffset +
		(int64(info.ReservedSectors)+int64(info.FATCount)*int64(info.FATSize))*int64(info.BytesPerSector)
}

// RootDirOffset returns the absolute byte offset of the root directory cluster.
func (info Info) RootDirOffset(partOffset int64) int64 {
	return info.DataOffset(partOffset) + int64(info.RootCluster-2)*int64(info.ClusterSize())
}

// diskRW combines the read, write, and close operations needed by FS.
type diskRW interface {
	io.ReaderAt
	io.WriterAt
	io.Closer
}

// FS represents an opened FAT32 image.
type fat32FS struct {
	f          diskRW
	partOffset int64
	info       Info
}

var (
	openFile            = os.OpenFile
	openPartitionOffset = partitionOffset
	openReadInfo        = readInfo
)

// Verify implementation of the common filesystem interface.
var _ filesystem.Filesystem = (*fat32FS)(nil)

// Open opens imagePath, optionally selecting a partition, and parses the FAT32 BPB.
func Open(imagePath string, partIndex int) (filesystem.Filesystem, error) {
	f, err := openFile(imagePath, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fat32: open %s: %w", imagePath, err)
	}
	off, err := openPartitionOffset(f, partIndex)
	if err != nil {
		f.Close()
		return nil, err
	}
	info, err := openReadInfo(f, off)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &fat32FS{f: f, partOffset: off, info: info}, nil
}

// Close releases the underlying file handle.
func (fs *fat32FS) Close() error { return fs.f.Close() }

// Info returns the decoded boot-sector metadata.
func (fs *fat32FS) Info() Info { return fs.info }

// PartitionOffset returns the byte offset of the selected partition.
func (fs *fat32FS) PartitionOffset() int64 { return fs.partOffset }

// Stat returns basic metadata for the root directory or a path entry.
func (fs *fat32FS) Stat(path string) (filesystem.Stat, error) {
	if path == "/" {
		return filesystem.NewStat(fatModeDir, uint64(fs.info.ClusterSize()), uint64(fs.info.RootCluster)), nil
	}
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(entry.mode(), uint64(entry.size), uint64(entry.cluster)), nil
}

// ListDir lists the entries of the directory at path.
func (fs *fat32FS) ListDir(path string) ([]filesystem.DirEntry, error) {
	var dirCluster uint32
	if path == "/" {
		dirCluster = fs.info.RootCluster
	} else {
		entry, _, err := fs.resolvePath(path)
		if err != nil {
			return nil, err
		}
		if entry.attr&fatAttrDirectory == 0 {
			return nil, fmt.Errorf("fat32: %q is not a directory", path)
		}
		dirCluster = entry.cluster
	}
	buf, err := fs.readDirBuf(dirCluster)
	if err != nil {
		return nil, err
	}
	return parseRootDirEntries(buf), nil
}

// ReadFile reads and returns the contents of the regular file at path.
func (fs *fat32FS) ReadFile(path string) ([]byte, error) {
	if path == "/" {
		return nil, fmt.Errorf("fat32: %q is not a regular file", path)
	}
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		return nil, err
	}
	if entry.attr&fatAttrDirectory != 0 {
		return nil, fmt.Errorf("fat32: %q is not a regular file", path)
	}
	return fs.readClusterChain(entry.cluster, uint64(entry.size))
}

// WriteFile creates or overwrites the regular file at path with data and permission bits.
func (fs *fat32FS) WriteFile(path string, data []byte, perm os.FileMode) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("fat32: %q is not a regular file", path)
	}
	buf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	startOff, count, exists := fat32FindEntry(buf, name)
	if exists {
		old8dot3 := buf[startOff+(count-1)*dirEntrySize : startOff+count*dirEntrySize]
		oldCluster := uint32(binary.LittleEndian.Uint16(old8dot3[20:22]))<<16 |
			uint32(binary.LittleEndian.Uint16(old8dot3[26:28]))
		if oldCluster >= 2 {
			if err := fs.freeChain(oldCluster); err != nil {
				return err
			}
		}
		for i := 0; i < count; i++ {
			buf[startOff+i*dirEntrySize] = 0xE5
		}
	}
	var firstCluster uint32
	if len(data) > 0 {
		firstCluster, err = fs.writeData(data)
		if err != nil {
			return err
		}
	}
	nLFN := 0
	if needsLFN(name) {
		nLFN = (len(utf16.Encode([]rune(name))) + 12) / 13
	}
	var slotOff int
	buf, slotOff = fs.ensureDirSlots(buf, nLFN+1)
	if slotOff < 0 {
		return fmt.Errorf("fat32: directory is full")
	}
	attr := byte(0x20)
	if perm&0o200 == 0 {
		attr |= fatAttrReadOnly
	}
	writeDirEntry(buf, slotOff, name, attr, firstCluster, uint32(len(data)))
	return fs.writeDirBuf(parentCluster, buf)
}

// ReadLink always returns an error; FAT32 does not support symbolic links.
func (fs *fat32FS) ReadLink(path string) (string, error) {
	return "", fmt.Errorf("fat32: %q is not a symbolic link", path)
}

// MkDir creates a new empty directory at path.
func (fs *fat32FS) MkDir(path string, perm os.FileMode) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("fat32: %q already exists", path)
	}
	buf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	_, _, exists := fat32FindEntry(buf, name)
	if exists {
		return fmt.Errorf("fat32: %q already exists", path)
	}
	nLFN := 0
	if needsLFN(name) {
		nLFN = (len(utf16.Encode([]rune(name))) + 12) / 13
	}
	var slotOff int
	buf, slotOff = fs.ensureDirSlots(buf, nLFN+1)
	if slotOff < 0 {
		return fmt.Errorf("fat32: directory is full")
	}
	cluster, err := fs.allocCluster()
	if err != nil {
		return err
	}
	if err := fs.setFATEntry(cluster, 0x0FFFFFFF); err != nil {
		return err
	}
	clusterBuf := make([]byte, fs.info.ClusterSize())
	copy(clusterBuf[0:11], []byte(".          "))
	clusterBuf[11] = fatAttrDirectory
	binary.LittleEndian.PutUint16(clusterBuf[20:22], uint16(cluster>>16))
	binary.LittleEndian.PutUint16(clusterBuf[26:28], uint16(cluster))
	copy(clusterBuf[32:43], []byte("..         "))
	clusterBuf[43] = fatAttrDirectory
	binary.LittleEndian.PutUint16(clusterBuf[52:54], uint16(parentCluster>>16))
	binary.LittleEndian.PutUint16(clusterBuf[58:60], uint16(parentCluster))
	clusterOff := fs.info.DataOffset(fs.partOffset) + int64(cluster-2)*int64(fs.info.ClusterSize())
	if _, err := fs.f.WriteAt(clusterBuf, clusterOff); err != nil {
		return fmt.Errorf("fat32: write directory cluster: %w", err)
	}
	attr := byte(fatAttrDirectory)
	if perm&0o200 == 0 {
		attr |= fatAttrReadOnly
	}
	writeDirEntry(buf, slotOff, name, attr, cluster, 0)
	return fs.writeDirBuf(parentCluster, buf)
}

// DeleteFile removes the regular file at path, freeing its cluster chain.
func (fs *fat32FS) DeleteFile(path string) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("fat32: %q is not a regular file", path)
	}
	buf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	startOff, count, found := fat32FindEntry(buf, name)
	if !found {
		return fmt.Errorf("fat32: %q not found", path)
	}
	e8dot3 := buf[startOff+(count-1)*dirEntrySize : startOff+count*dirEntrySize]
	if e8dot3[11]&fatAttrDirectory != 0 {
		return fmt.Errorf("fat32: %q is a directory", path)
	}
	firstCluster := uint32(binary.LittleEndian.Uint16(e8dot3[20:22]))<<16 |
		uint32(binary.LittleEndian.Uint16(e8dot3[26:28]))
	if firstCluster >= 2 {
		if err := fs.freeChain(firstCluster); err != nil {
			return err
		}
	}
	for i := 0; i < count; i++ {
		buf[startOff+i*dirEntrySize] = 0xE5
	}
	return fs.writeDirBuf(parentCluster, buf)
}

// DeleteDir removes the directory at path, recursively deleting any contents.
func (fs *fat32FS) DeleteDir(path string) error {
	name, parentCluster, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("fat32: cannot delete root directory")
	}
	buf, err := fs.readDirBuf(parentCluster)
	if err != nil {
		return err
	}
	startOff, count, found := fat32FindEntry(buf, name)
	if !found {
		return fmt.Errorf("fat32: %q not found", path)
	}
	e8dot3 := buf[startOff+(count-1)*dirEntrySize : startOff+count*dirEntrySize]
	if e8dot3[11]&fatAttrDirectory == 0 {
		return fmt.Errorf("fat32: %q is not a directory", path)
	}
	firstCluster := uint32(binary.LittleEndian.Uint16(e8dot3[20:22]))<<16 |
		uint32(binary.LittleEndian.Uint16(e8dot3[26:28]))
	if firstCluster >= 2 {
		if err := fs.deleteAllContents(firstCluster); err != nil {
			return err
		}
		if err := fs.freeChain(firstCluster); err != nil {
			return err
		}
	}
	for i := 0; i < count; i++ {
		buf[startOff+i*dirEntrySize] = 0xE5
	}
	return fs.writeDirBuf(parentCluster, buf)
}

// Rename moves the entry at oldPath to newPath.
// If newPath already exists it is replaced.
func (fs *fat32FS) Rename(oldPath, newPath string) error {
	oldName, oldParentCluster, err := fs.getParentDir(oldPath)
	if err != nil {
		return err
	}
	if oldName == "" {
		return fmt.Errorf("fat32: cannot rename root directory")
	}
	newName, newParentCluster, err := fs.getParentDir(newPath)
	if err != nil {
		return err
	}
	if newName == "" {
		return fmt.Errorf("fat32: cannot rename to root directory")
	}
	if oldParentCluster == newParentCluster && strings.EqualFold(oldName, newName) {
		return nil
	}
	oldBuf, err := fs.readDirBuf(oldParentCluster)
	if err != nil {
		return err
	}
	oldStart, oldCount, oldFound := fat32FindEntry(oldBuf, oldName)
	if !oldFound {
		return fmt.Errorf("fat32: %q not found", oldPath)
	}
	old8dot3 := oldBuf[oldStart+(oldCount-1)*dirEntrySize : oldStart+oldCount*dirEntrySize]
	oldCluster := uint32(binary.LittleEndian.Uint16(old8dot3[20:22]))<<16 |
		uint32(binary.LittleEndian.Uint16(old8dot3[26:28]))
	oldAttr := old8dot3[11]
	oldSize := binary.LittleEndian.Uint32(old8dot3[28:32])

	var newBuf []byte
	if newParentCluster == oldParentCluster {
		newBuf = oldBuf
	} else {
		newBuf, err = fs.readDirBuf(newParentCluster)
		if err != nil {
			return err
		}
	}
	newStart, newCount, newFound := fat32FindEntry(newBuf, newName)
	if newFound {
		new8dot3 := newBuf[newStart+(newCount-1)*dirEntrySize : newStart+newCount*dirEntrySize]
		existCluster := uint32(binary.LittleEndian.Uint16(new8dot3[20:22]))<<16 |
			uint32(binary.LittleEndian.Uint16(new8dot3[26:28]))
		if existCluster >= 2 {
			if err := fs.freeChain(existCluster); err != nil {
				return err
			}
		}
		for i := 0; i < newCount; i++ {
			newBuf[newStart+i*dirEntrySize] = 0xE5
		}
	}
	for i := 0; i < oldCount; i++ {
		oldBuf[oldStart+i*dirEntrySize] = 0xE5
	}
	nLFN := 0
	if needsLFN(newName) {
		nLFN = (len(utf16.Encode([]rune(newName))) + 12) / 13
	}
	destBuf := oldBuf
	if newParentCluster != oldParentCluster {
		destBuf = newBuf
	}
	var slotOff int
	destBuf, slotOff = fs.ensureDirSlots(destBuf, nLFN+1)
	if slotOff < 0 {
		return fmt.Errorf("fat32: directory is full")
	}
	writeDirEntry(destBuf, slotOff, newName, oldAttr, oldCluster, oldSize)
	if newParentCluster != oldParentCluster {
		// destBuf points to the (possibly grown) newBuf; reassign back so the
		// post-grow buffer is what we persist.
		newBuf = destBuf
		if err := fs.writeDirBuf(oldParentCluster, oldBuf); err != nil {
			return err
		}
		return fs.writeDirBuf(newParentCluster, newBuf)
	}
	// Same-parent rename: destBuf is the (possibly grown) oldBuf.
	oldBuf = destBuf
	return fs.writeDirBuf(oldParentCluster, oldBuf)
}

func readInfo(r io.ReaderAt, partOffset int64) (Info, error) {
	buf := make([]byte, sectorSize)
	if _, err := r.ReadAt(buf, partOffset); err != nil {
		return Info{}, fmt.Errorf("fat32: read boot sector: %w", err)
	}
	if buf[510] != 0x55 || buf[511] != 0xAA {
		return Info{}, fmt.Errorf("fat32: invalid boot sector signature")
	}

	le := binary.LittleEndian
	bytesPerSector := le.Uint16(buf[11:])
	if !isPowerOfTwo(uint32(bytesPerSector)) || bytesPerSector < uint16(sectorSize) {
		return Info{}, fmt.Errorf("fat32: invalid bytes per sector %d", bytesPerSector)
	}
	sectorsPerCluster := buf[13]
	if !isPowerOfTwo(uint32(sectorsPerCluster)) {
		return Info{}, fmt.Errorf("fat32: invalid sectors per cluster %d", sectorsPerCluster)
	}
	reservedSectors := le.Uint16(buf[14:])
	if reservedSectors == 0 {
		return Info{}, fmt.Errorf("fat32: reserved sector count is zero")
	}
	fatCount := buf[16]
	if fatCount == 0 {
		return Info{}, fmt.Errorf("fat32: FAT count is zero")
	}
	rootEntries := le.Uint16(buf[17:])
	if rootEntries != 0 {
		return Info{}, fmt.Errorf("fat32: root entry count %d indicates FAT12/16", rootEntries)
	}

	totalSectors := uint32(le.Uint16(buf[19:]))
	if totalSectors == 0 {
		totalSectors = le.Uint32(buf[32:])
	}
	if totalSectors == 0 {
		return Info{}, fmt.Errorf("fat32: total sector count is zero")
	}

	fatSize := uint32(le.Uint16(buf[22:]))
	if fatSize == 0 {
		fatSize = le.Uint32(buf[36:])
	}
	if fatSize == 0 {
		return Info{}, fmt.Errorf("fat32: FAT size is zero")
	}

	rootCluster := le.Uint32(buf[44:])
	if rootCluster < 2 {
		return Info{}, fmt.Errorf("fat32: invalid root cluster %d", rootCluster)
	}

	typeLabel := trimASCII(buf[82:90])
	if typeLabel != "" && typeLabel != "FAT32" {
		return Info{}, fmt.Errorf("fat32: unexpected filesystem type label %q", typeLabel)
	}

	return Info{
		OEMName:           trimASCII(buf[3:11]),
		BytesPerSector:    bytesPerSector,
		SectorsPerCluster: sectorsPerCluster,
		ReservedSectors:   reservedSectors,
		FATCount:          fatCount,
		TotalSectors:      totalSectors,
		FATSize:           fatSize,
		HiddenSectors:     le.Uint32(buf[28:]),
		RootCluster:       rootCluster,
		FSInfoSector:      le.Uint16(buf[48:]),
		BackupBootSector:  le.Uint16(buf[50:]),
		DriveNumber:       buf[64],
		VolumeID:          le.Uint32(buf[67:]),
		VolumeLabel:       trimASCII(buf[71:82]),
		TypeLabel:         typeLabel,
	}, nil
}

func partitionOffset(r io.ReaderAt, partIndex int) (int64, error) {
	var sig [8]byte
	if _, err := r.ReadAt(sig[:], sectorSize); err == nil && string(sig[:]) == "EFI PART" {
		return gptPartOffset(r, partIndex)
	}

	var magic [2]byte
	if _, err := r.ReadAt(magic[:], 510); err == nil && magic[0] == 0x55 && magic[1] == 0xAA {
		return mbrPartOffset(r, partIndex)
	}

	return 0, nil
}

func gptPartOffset(r io.ReaderAt, partIndex int) (int64, error) {
	hdr := make([]byte, 92)
	if _, err := r.ReadAt(hdr, sectorSize); err != nil {
		return 0, fmt.Errorf("fat32: read GPT header: %w", err)
	}
	le := binary.LittleEndian
	partEntryLBA := le.Uint64(hdr[72:])
	numParts := le.Uint32(hdr[80:])
	entrySize := le.Uint32(hdr[84:])
	if entrySize < 128 {
		return 0, fmt.Errorf("fat32: unexpected GPT entry size %d", entrySize)
	}

	tableOff := int64(partEntryLBA) * sectorSize
	buf := make([]byte, entrySize)
	for index := uint32(0); index < numParts; index++ {
		if _, err := r.ReadAt(buf, tableOff+int64(index)*int64(entrySize)); err != nil {
			break
		}
		var typeGUID [16]byte
		copy(typeGUID[:], buf[:16])
		startLBA := le.Uint64(buf[32:])

		if partIndex >= 0 {
			if int(index) != partIndex {
				continue
			}
			if typeGUID == [16]byte{} || startLBA == 0 {
				return 0, fmt.Errorf("fat32: GPT partition index %d not found", partIndex)
			}
			return int64(startLBA) * sectorSize, nil
		}

		if typeGUID != [16]byte{} && startLBA != 0 {
			return int64(startLBA) * sectorSize, nil
		}
	}

	if partIndex >= 0 {
		return 0, fmt.Errorf("fat32: GPT partition index %d not found", partIndex)
	}
	return 0, fmt.Errorf("fat32: no populated GPT partition found")
}

func mbrPartOffset(r io.ReaderAt, partIndex int) (int64, error) {
	table := make([]byte, 64)
	if _, err := r.ReadAt(table, 446); err != nil {
		return 0, fmt.Errorf("fat32: read MBR partition table: %w", err)
	}
	for index := 0; index < 4; index++ {
		entry := table[index*16:]
		startLBA := binary.LittleEndian.Uint32(entry[8:])

		if partIndex >= 0 {
			if index != partIndex {
				continue
			}
			if startLBA == 0 {
				return 0, fmt.Errorf("fat32: MBR partition index %d not found", partIndex)
			}
			return int64(startLBA) * sectorSize, nil
		}

		if startLBA != 0 {
			return int64(startLBA) * sectorSize, nil
		}
	}

	if partIndex >= 0 {
		return 0, fmt.Errorf("fat32: MBR partition index %d not found", partIndex)
	}
	return 0, nil
}

// maxDirClusters caps how many clusters a single directory may occupy.
// Exposed as a package-level var so tests can shrink it without having to
// build images millions of cluster-sized entries large.
var maxDirClusters = 256

// readDirBuf reads the full cluster chain of the directory at startCluster.
func (fs *fat32FS) readDirBuf(startCluster uint32) ([]byte, error) {
	maxBytes := uint64(maxDirClusters) * uint64(fs.info.ClusterSize())
	return fs.readClusterChain(startCluster, maxBytes)
}

// writeDirBuf writes buf back to the cluster chain starting at startCluster.
// When buf is longer than the existing chain, the chain is extended by
// allocating fresh clusters from the FAT and linking them in. This makes the
// root directory (and any subdirectory) grow uniformly past the first cluster.
func (fs *fat32FS) writeDirBuf(startCluster uint32, buf []byte) error {
	clusterSize := int64(fs.info.ClusterSize())
	dataBase := fs.info.DataOffset(fs.partOffset)
	fatBase := fs.info.FATOffset(fs.partOffset)
	cluster := startCluster
	for pos := 0; pos < len(buf); pos += int(clusterSize) {
		off := dataBase + int64(cluster-2)*clusterSize
		end := pos + int(clusterSize)
		if end > len(buf) {
			end = len(buf)
		}
		padded := make([]byte, clusterSize)
		copy(padded, buf[pos:end])
		if _, err := fs.f.WriteAt(padded, off); err != nil {
			return fmt.Errorf("fat32: write directory cluster %d: %w", cluster, err)
		}
		if end >= len(buf) {
			break
		}
		var nextEntry [4]byte
		if _, err := fs.f.ReadAt(nextEntry[:], fatBase+int64(cluster)*4); err != nil {
			return fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
		}
		next := binary.LittleEndian.Uint32(nextEntry[:]) & 0x0FFFFFFF
		if next >= 0x0FFFFFF8 {
			// End of current chain — extend with a freshly allocated cluster.
			newCluster, err := fs.allocCluster()
			if err != nil {
				return fmt.Errorf("fat32: extend directory chain: %w", err)
			}
			if err := fs.setFATEntry(newCluster, 0x0FFFFFFF); err != nil {
				return err
			}
			if err := fs.setFATEntry(cluster, newCluster); err != nil {
				return err
			}
			// Zero the new cluster so that the unused 32-byte slots end with
			// the 0x00 sentinel byte that directory parsers expect.
			zeroBuf := make([]byte, clusterSize)
			newOff := dataBase + int64(newCluster-2)*clusterSize
			if _, err := fs.f.WriteAt(zeroBuf, newOff); err != nil {
				return fmt.Errorf("fat32: zero new directory cluster %d: %w", newCluster, err)
			}
			cluster = newCluster
			continue
		}
		if next < 2 {
			// Mirrors the tolerant behaviour in readClusterChain: an out-of-
			// range FAT pointer terminates the walk silently.
			break
		}
		cluster = next
	}
	return nil
}

// fat32FindEntry searches buf for a directory entry whose parsed name (LFN or
// 8.3 short name) matches name case-insensitively.
// Returns (startOff, totalCount, found) where startOff is the offset of the
// first LFN entry (or the 8.3 entry when no LFN precedes it) and totalCount
// is the total number of 32-byte slots in the entry chain.
func fat32FindEntry(buf []byte, name string) (int, int, bool) {
	le := binary.LittleEndian
	var lfnChars []uint16
	lfnStart := -1
	lfnCount := 0
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		entry := buf[offset : offset+dirEntrySize]
		b0 := entry[0]
		if b0 == 0x00 {
			return -1, 0, false
		}
		if b0 == 0xE5 {
			lfnChars = nil
			lfnStart = -1
			lfnCount = 0
			continue
		}
		if entry[11] == fatAttrLongName {
			if lfnStart < 0 {
				lfnStart = offset
			}
			lfnCount++
			var chars [13]uint16
			n := 0
			for _, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
				c := le.Uint16(entry[off:])
				if c == 0x0000 || c == 0xFFFF {
					break
				}
				chars[n] = c
				n++
			}
			lfnChars = append(chars[:n], lfnChars...)
			continue
		}
		// 8.3 short entry
		var entryName string
		if len(lfnChars) > 0 {
			entryName = string(utf16.Decode(lfnChars))
		} else {
			entryName = shortName(entry)
		}
		if strings.EqualFold(entryName, name) {
			startOff := offset
			count := 1
			if lfnStart >= 0 {
				startOff = lfnStart
				count = lfnCount + 1
			}
			return startOff, count, true
		}
		lfnChars = nil
		lfnStart = -1
		lfnCount = 0
	}
	return -1, 0, false
}

// fat32FindFreeSlot returns the offset of the first free (0x00 or 0xE5) 32-byte
// slot in buf, or -1 if none exists.
func fat32FindFreeSlot(buf []byte) int {
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		b := buf[offset]
		if b == 0x00 || b == 0xE5 {
			return offset
		}
	}
	return -1
}

// fat32FindNFreeSlots returns the offset of the first run of n consecutive free
// (0x00 or 0xE5) 32-byte slots in buf, or -1 if no such run exists.
func fat32FindNFreeSlots(buf []byte, n int) int {
	for offset := 0; offset+n*dirEntrySize <= len(buf); offset += dirEntrySize {
		run := 0
		for i := 0; i < n; i++ {
			b := buf[offset+i*dirEntrySize]
			if b == 0x00 || b == 0xE5 {
				run++
			} else {
				break
			}
		}
		if run == n {
			return offset
		}
	}
	return -1
}

// ensureDirSlots returns buf, possibly extended by one or more zero-filled
// clusters, such that it contains at least n consecutive free 32-byte slots.
// Together with writeDirBuf's chain-extension logic this lets a directory
// grow past its initial single-cluster footprint.
//
// The returned offset is the position of the n-slot run inside the (possibly
// grown) buffer.
func (fs *fat32FS) ensureDirSlots(buf []byte, n int) ([]byte, int) {
	off := fat32FindNFreeSlots(buf, n)
	if off >= 0 {
		return buf, off
	}
	clusterSize := int(fs.info.ClusterSize())
	// Grow one cluster at a time until either we fit or we exceed the
	// safety cap shared with readDirBuf (256 clusters worth of dir buf
	// by default; tests may shrink this).
	for {
		if len(buf) >= maxDirClusters*clusterSize {
			return buf, -1
		}
		buf = append(buf, make([]byte, clusterSize)...)
		off = fat32FindNFreeSlots(buf, n)
		if off >= 0 {
			return buf, off
		}
	}
}

// needsLFN reports whether name requires LFN directory entries because it cannot
// be represented exactly as an 8.3 short name (e.g. lowercase, too long, etc.).
func needsLFN(name string) bool {
	short := toShortNameBytes(name)
	return shortName(short[:]) != name
}

// makeShortAlias generates a unique-ish 8.3 alias for a long filename using the
// tilde notation (e.g. "LONGFI~1" + ext).
func makeShortAlias(name string) [11]byte {
	upper := strings.ToUpper(name)
	var result [11]byte
	for i := range result {
		result[i] = ' '
	}
	dot := strings.LastIndex(upper, ".")
	var base, ext string
	if dot >= 0 {
		base = upper[:dot]
		ext = upper[dot+1:]
	} else {
		base = upper
	}
	// Strip spaces and dots from base (not valid in short name stems).
	var clean strings.Builder
	for _, c := range base {
		if c != ' ' && c != '.' {
			clean.WriteRune(c)
		}
	}
	base = clean.String()
	if len(base) > 6 {
		base = base[:6]
	}
	base = base + "~1"
	for i := 0; i < 8 && i < len(base); i++ {
		result[i] = base[i]
	}
	for i := 0; i < 3 && i < len(ext); i++ {
		result[8+i] = ext[i]
	}
	return result
}

// lfnChecksum computes the FAT LFN checksum of an 8.3 short name entry.
func lfnChecksum(short [11]byte) byte {
	var sum byte
	for _, b := range short {
		sum = (sum>>1 | sum<<7) + b
	}
	return sum
}

// writeDirEntry writes LFN entries (when needed) followed by the 8.3 entry into
// buf at slotOff. The caller must ensure enough consecutive free slots exist.
func writeDirEntry(buf []byte, slotOff int, name string, attr byte, cluster uint32, size uint32) {
	le := binary.LittleEndian
	lfnOffsets := []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}

	nameWords := utf16.Encode([]rune(name))
	var short [11]byte
	N := 0
	if needsLFN(name) {
		N = (len(nameWords) + 12) / 13
		short = makeShortAlias(name)
	} else {
		short = toShortNameBytes(name)
	}
	csum := lfnChecksum(short)

	// Write LFN entries: seq N is first on disk (last portion of name + 0x40),
	// seq 1 is last on disk before the 8.3 entry (first portion of name).
	for seq := N; seq >= 1; seq-- {
		diskSlot := N - seq
		entryOff := slotOff + diskSlot*dirEntrySize
		for i := 0; i < dirEntrySize; i++ {
			buf[entryOff+i] = 0
		}
		seqByte := byte(seq)
		if seq == N {
			seqByte |= 0x40
		}
		buf[entryOff] = seqByte
		buf[entryOff+11] = fatAttrLongName
		buf[entryOff+13] = csum
		charStart := (seq - 1) * 13
		for i, pos := range lfnOffsets {
			idx := charStart + i
			var c uint16
			if idx < len(nameWords) {
				c = nameWords[idx]
			} else if idx == len(nameWords) {
				c = 0x0000
			} else {
				c = 0xFFFF
			}
			le.PutUint16(buf[entryOff+pos:], c)
		}
	}

	// Write 8.3 short-name entry.
	e8 := slotOff + N*dirEntrySize
	for i := 0; i < dirEntrySize; i++ {
		buf[e8+i] = 0
	}
	copy(buf[e8:e8+11], short[:])
	buf[e8+11] = attr
	le.PutUint16(buf[e8+20:], uint16(cluster>>16))
	le.PutUint16(buf[e8+26:], uint16(cluster))
	le.PutUint32(buf[e8+28:], size)
}

// pathComponents splits "/a/b/c" into ["a", "b", "c"].
func pathComponents(path string) []string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// resolvePath walks the full path, returning the final entry and its parent
// directory cluster. For "/" it returns a synthetic root entry.
func (fs *fat32FS) resolvePath(path string) (rootDirEntry, uint32, error) {
	if !strings.HasPrefix(path, "/") {
		return rootDirEntry{}, 0, fmt.Errorf("fat32: unsupported path %q", path)
	}
	components := pathComponents(path)
	parentCluster := fs.info.RootCluster
	var entry rootDirEntry
	for i, name := range components {
		buf, err := fs.readDirBuf(parentCluster)
		if err != nil {
			return rootDirEntry{}, 0, err
		}
		startOff, count, found := fat32FindEntry(buf, name)
		if !found {
			return rootDirEntry{}, 0, fmt.Errorf("fat32: %q not found", path)
		}
		off8dot3 := startOff + (count-1)*dirEntrySize
		entry = toRootDirEntry(buf[off8dot3 : off8dot3+dirEntrySize])
		if i < len(components)-1 {
			if entry.attr&fatAttrDirectory == 0 {
				return rootDirEntry{}, 0, fmt.Errorf("fat32: intermediate path %q is not a directory", path)
			}
			parentCluster = entry.cluster
		}
	}
	return entry, parentCluster, nil
}

// getParentDir returns the last path component and the cluster of its parent
// directory. Returns an error for paths that require traversing non-existent
// or non-directory intermediate components.
func (fs *fat32FS) getParentDir(path string) (name string, parentCluster uint32, err error) {
	if !strings.HasPrefix(path, "/") {
		return "", 0, fmt.Errorf("fat32: unsupported path %q", path)
	}
	components := pathComponents(path)
	if len(components) == 0 {
		return "", fs.info.RootCluster, nil
	}
	parentCluster = fs.info.RootCluster
	for i := 0; i < len(components)-1; i++ {
		buf, err := fs.readDirBuf(parentCluster)
		if err != nil {
			return "", 0, err
		}
		startOff, count, found := fat32FindEntry(buf, components[i])
		if !found {
			return "", 0, fmt.Errorf("fat32: parent directory %q not found", components[i])
		}
		off8dot3 := startOff + (count-1)*dirEntrySize
		e := toRootDirEntry(buf[off8dot3 : off8dot3+dirEntrySize])
		if e.attr&fatAttrDirectory == 0 {
			return "", 0, fmt.Errorf("fat32: intermediate path %q is not a directory", components[i])
		}
		parentCluster = e.cluster
	}
	return components[len(components)-1], parentCluster, nil
}

// deleteAllContents recursively removes all files and subdirectories inside
// the directory at dirCluster, freeing their cluster chains. The directory
// cluster itself is not freed; that is the caller's responsibility.
func (fs *fat32FS) deleteAllContents(dirCluster uint32) error {
	buf, err := fs.readDirBuf(dirCluster)
	if err != nil {
		return err
	}
	for _, e := range parseRootDirMetadata(buf) {
		if e.name == "." || e.name == ".." {
			continue
		}
		if e.attr&fatAttrDirectory != 0 && e.cluster >= 2 {
			if err := fs.deleteAllContents(e.cluster); err != nil {
				return err
			}
		}
		if e.cluster >= 2 {
			if err := fs.freeChain(e.cluster); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseRootDirEntries(buf []byte) []filesystem.DirEntry {
	metadata := parseRootDirMetadata(buf)
	entries := make([]filesystem.DirEntry, 0, len(metadata))
	for _, entry := range metadata {
		entries = append(entries, filesystem.NewDirEntry(uint64(entry.cluster), entry.name, entry.attr))
	}
	return entries
}

func parseRootDirMetadata(buf []byte) []rootDirEntry {
	le := binary.LittleEndian
	entries := make([]rootDirEntry, 0)
	var lfnChars []uint16
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		entry := buf[offset : offset+dirEntrySize]
		switch entry[0] {
		case 0x00:
			return entries
		case 0xE5:
			lfnChars = nil
			continue
		}
		if entry[11] == fatAttrLongName {
			var chars [13]uint16
			n := 0
			for _, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
				c := le.Uint16(entry[off:])
				if c == 0x0000 || c == 0xFFFF {
					break
				}
				chars[n] = c
				n++
			}
			lfnChars = append(chars[:n], lfnChars...)
			continue
		}
		var name string
		if len(lfnChars) > 0 {
			name = string(utf16.Decode(lfnChars))
		} else {
			name = shortName(entry)
		}
		lfnChars = nil
		entries = append(entries, rootDirEntry{
			cluster: uint32(le.Uint16(entry[20:22]))<<16 | uint32(le.Uint16(entry[26:28])),
			name:    name,
			attr:    entry[11],
			size:    le.Uint32(entry[28:32]),
		})
	}
	return entries
}

func toRootDirEntry(entry []byte) rootDirEntry {
	clusterHigh := binary.LittleEndian.Uint16(entry[20:22])
	clusterLow := binary.LittleEndian.Uint16(entry[26:28])
	return rootDirEntry{
		cluster: uint32(clusterHigh)<<16 | uint32(clusterLow),
		name:    shortName(entry),
		attr:    entry[11],
		size:    binary.LittleEndian.Uint32(entry[28:32]),
	}
}

func (entry rootDirEntry) mode() uint16 {
	if entry.attr&fatAttrDirectory != 0 {
		if entry.attr&fatAttrReadOnly != 0 {
			return fatModeDirRO
		}
		return fatModeDir
	}
	if entry.attr&fatAttrReadOnly != 0 {
		return fatModeFileRO
	}
	return fatModeFile
}

func rootPathName(path string, prefix string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%s: unsupported path %q", prefix, path)
	}
	name := strings.TrimPrefix(path, "/")
	if name == "" {
		return "", nil
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("%s: nested paths are not supported %q", prefix, path)
	}
	return name, nil
}

func shortName(entry []byte) string {
	base := strings.TrimRight(string(entry[0:8]), " ")
	ext := strings.TrimRight(string(entry[8:11]), " ")
	if ext == "" {
		return base
	}
	return base + "." + ext
}

func isPowerOfTwo(value uint32) bool {
	return value != 0 && value&(value-1) == 0
}

func trimASCII(raw []byte) string {
	return strings.TrimRight(string(raw), " \x00")
}

// readClusterChain follows the FAT chain starting at start and returns up to size bytes.
func (fs *fat32FS) readClusterChain(start uint32, size uint64) ([]byte, error) {
	if start == 0 {
		return []byte{}, nil
	}
	clusterSize := int64(fs.info.ClusterSize())
	fatBase := fs.info.FATOffset(fs.partOffset)
	dataBase := fs.info.DataOffset(fs.partOffset)
	buf := make([]byte, 0, size)
	cluster := start
	for {
		if cluster < 2 || cluster >= 0x0FFFFFF7 {
			break
		}
		clusterBuf := make([]byte, clusterSize)
		off := dataBase + int64(cluster-2)*clusterSize
		if _, err := fs.f.ReadAt(clusterBuf, off); err != nil {
			return nil, fmt.Errorf("fat32: read cluster %d: %w", cluster, err)
		}
		buf = append(buf, clusterBuf...)
		var nextEntry [4]byte
		if _, err := fs.f.ReadAt(nextEntry[:], fatBase+int64(cluster)*4); err != nil {
			return nil, fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
		}
		next := binary.LittleEndian.Uint32(nextEntry[:]) & 0x0FFFFFFF
		if next >= 0x0FFFFFF8 {
			break
		}
		cluster = next
	}
	if uint64(len(buf)) > size {
		buf = buf[:size]
	}
	return buf, nil
}

// writeData allocates FAT clusters, writes data into them, and returns the first cluster.
func (fs *fat32FS) writeData(data []byte) (uint32, error) {
	clusterSize := int64(fs.info.ClusterSize())
	numClusters := (int64(len(data)) + clusterSize - 1) / clusterSize
	allocated := make([]uint32, numClusters)
	for i := range allocated {
		c, err := fs.allocCluster()
		if err != nil {
			for _, ac := range allocated[:i] {
				_ = fs.setFATEntry(ac, 0)
			}
			return 0, err
		}
		if err := fs.setFATEntry(c, 0x0FFFFFFF); err != nil {
			return 0, err
		}
		allocated[i] = c
	}
	for i := 0; i < len(allocated)-1; i++ {
		if err := fs.setFATEntry(allocated[i], allocated[i+1]); err != nil {
			return 0, err
		}
	}
	dataBase := fs.info.DataOffset(fs.partOffset)
	for i, c := range allocated {
		off := dataBase + int64(c-2)*clusterSize
		start := int64(i) * clusterSize
		end := start + clusterSize
		if end > int64(len(data)) {
			clusterBuf := make([]byte, clusterSize)
			copy(clusterBuf, data[start:])
			if _, err := fs.f.WriteAt(clusterBuf, off); err != nil {
				return 0, fmt.Errorf("fat32: write cluster %d: %w", c, err)
			}
		} else {
			if _, err := fs.f.WriteAt(data[start:end], off); err != nil {
				return 0, fmt.Errorf("fat32: write cluster %d: %w", c, err)
			}
		}
	}
	return allocated[0], nil
}

// allocCluster scans the FAT and returns the first free cluster number (≥ 2).
func (fs *fat32FS) allocCluster() (uint32, error) {
	clusterCount := fs.info.TotalSectors / uint32(fs.info.SectorsPerCluster)
	fatBase := fs.info.FATOffset(fs.partOffset)
	var buf [4]byte
	for c := uint32(2); c < clusterCount+2; c++ {
		if _, err := fs.f.ReadAt(buf[:], fatBase+int64(c)*4); err != nil {
			return 0, fmt.Errorf("fat32: read FAT entry: %w", err)
		}
		if binary.LittleEndian.Uint32(buf[:])&0x0FFFFFFF == 0 {
			return c, nil
		}
	}
	return 0, fmt.Errorf("fat32: no free clusters")
}

// setFATEntry writes a 32-bit FAT entry for cluster, preserving the upper 4
// reserved bits, and mirrors the write across every FAT copy declared in the
// BPB (FAT0, FAT1, …). Keeping the mirror in sync is required for fsck.vfat /
// fsck_msdos to consider the volume clean.
func (fs *fat32FS) setFATEntry(cluster uint32, value uint32) error {
	fatBase := fs.info.FATOffset(fs.partOffset)
	fatBytes := int64(fs.info.FATSize) * int64(fs.info.BytesPerSector)
	off := fatBase + int64(cluster)*4
	var buf [4]byte
	if _, err := fs.f.ReadAt(buf[:], off); err != nil {
		return fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
	}
	existing := binary.LittleEndian.Uint32(buf[:])
	binary.LittleEndian.PutUint32(buf[:], (existing&0xF0000000)|(value&0x0FFFFFFF))
	for i := uint8(0); i < fs.info.FATCount; i++ {
		mirrorOff := off + int64(i)*fatBytes
		if _, err := fs.f.WriteAt(buf[:], mirrorOff); err != nil {
			return fmt.Errorf("fat32: write FAT%d entry for cluster %d: %w", i, cluster, err)
		}
	}
	return nil
}

// freeChain marks every cluster in the FAT chain starting at start as free.
func (fs *fat32FS) freeChain(start uint32) error {
	fatBase := fs.info.FATOffset(fs.partOffset)
	cluster := start
	for cluster >= 2 && cluster < 0x0FFFFFF7 {
		var next [4]byte
		if _, err := fs.f.ReadAt(next[:], fatBase+int64(cluster)*4); err != nil {
			return fmt.Errorf("fat32: read FAT entry for cluster %d: %w", cluster, err)
		}
		nextCluster := binary.LittleEndian.Uint32(next[:]) & 0x0FFFFFFF
		if err := fs.setFATEntry(cluster, 0); err != nil {
			return err
		}
		if nextCluster >= 0x0FFFFFF8 {
			break
		}
		cluster = nextCluster
	}
	return nil
}

// writeRootDir writes the root directory buffer back to disk.
func (fs *fat32FS) writeRootDir(buf []byte) error {
	return fs.writeDirBuf(fs.info.RootCluster, buf)
}

// toShortNameBytes converts a filename to an 11-byte FAT 8.3 short name (uppercase, space-padded).
func toShortNameBytes(name string) [11]byte {
	name = strings.ToUpper(name)
	var result [11]byte
	for i := range result {
		result[i] = ' '
	}
	dot := strings.LastIndex(name, ".")
	var base, ext string
	if dot >= 0 {
		base = name[:dot]
		ext = name[dot+1:]
	} else {
		base = name
	}
	for i := 0; i < 8 && i < len(base); i++ {
		result[i] = base[i]
	}
	for i := 0; i < 3 && i < len(ext); i++ {
		result[8+i] = ext[i]
	}
	return result
}

// findRootDirSlot returns (offset, true) if an entry with shortName exists,
// or (offset, false) if a free slot was found. Returns (-1, false) when full.
func findRootDirSlot(buf []byte, shortName [11]byte) (int, bool) {
	freeSlot := -1
	for offset := 0; offset+dirEntrySize <= len(buf); offset += dirEntrySize {
		first := buf[offset]
		if first == 0x00 {
			if freeSlot < 0 {
				freeSlot = offset
			}
			break
		}
		if first == 0xE5 {
			if freeSlot < 0 {
				freeSlot = offset
			}
			continue
		}
		if buf[offset+11] == fatAttrLongName {
			continue
		}
		var existing [11]byte
		copy(existing[:], buf[offset:offset+11])
		if existing == shortName {
			return offset, true
		}
	}
	return freeSlot, false
}
