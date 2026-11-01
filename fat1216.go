package filesystem_fat32

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// FATType is the width of one FAT entry: the variant of the format.
//
// Bibliography: Microsoft, "FAT: General Overview of On-Disk Format" (fatgen103)
// section "FAT Type Determination" is the rule for FAT12 versus FAT16, and
// Linux fs/fat/inode.c fat_fill_super (opened 2026-11-01, torvalds/linux
// master) is the reference implementation of it:
//
//   - the FAT32 layout is recognised by structure: BPB_FATSz16 == 0 and
//     BPB_FATSz32 != 0 ("Must be FAT32");
//   - otherwise the CLUSTER COUNT decides, never a label: total_clusters >
//     MAX_FAT12 (4084) is FAT16, else FAT12;
//   - the count is (total_sectors - data_start) / sec_per_clus, with data_start
//     = reserved + fats*fat_length + rootdir_sectors, then clamped to what one
//     FAT can address;
//   - fatent.c: end of chain is >= 0xFF8 / 0xFFF8 / 0x0FFFFFF8, and the bad
//     cluster mark is 0xFF7 / 0xFFF7 / 0x0FFFFFF7.
type FATType uint8

// The three FAT variants. The zero value is treated as FAT32 so that an Info
// built by hand (as the tests do) keeps meaning what it always meant.
const (
	FAT12 FATType = 12
	FAT16 FATType = 16
	FAT32 FATType = 32
)

func (t FATType) String() string {
	switch t {
	case FAT12:
		return "FAT12"
	case FAT16:
		return "FAT16"
	}
	return "FAT32"
}

// Cluster-count thresholds from fatgen103: fewer than 4085 data clusters is
// FAT12, fewer than 65525 is FAT16, anything else FAT32.
const (
	maxFAT12Clusters = 4084
	maxFAT16Clusters = 65524
)

// fixedRootDir is the internal directory reference for the FAT12/16 root
// directory, which is a fixed region and has no cluster number. It lies above
// every cluster a FAT can hold (the top valid FAT32 cluster is 0x0FFFFFF6), so
// it can never collide with a real chain, and readClusterChain refuses it.
const fixedRootDir = 0xFFFFFFFF

// ErrReadOnlyFAT is returned by every operation that would modify a FAT12 or
// FAT16 volume. Reading is supported; writing is a follow-up.
var ErrReadOnlyFAT = errors.New("fat32: FAT12/FAT16 volumes are read-only")

// fixedRoot reports whether the volume is FAT12/16, whose root directory is a
// fixed region right after the FATs rather than a cluster chain.
func (info Info) fixedRoot() bool { return info.Type == FAT12 || info.Type == FAT16 }

// RootDirSectors is ((BPB_RootEntCnt*32) + (BPB_BytsPerSec-1)) / BPB_BytsPerSec.
func (info Info) RootDirSectors() uint32 {
	bps := uint32(info.BytesPerSector)
	if bps == 0 {
		return 0
	}
	return (uint32(info.RootEntries)*dirEntrySize + bps - 1) / bps
}

// rootRegionOffset is the absolute offset of the FAT12/16 fixed root region.
func (info Info) rootRegionOffset(partOffset int64) int64 {
	return partOffset +
		(int64(info.ReservedSectors)+int64(info.FATCount)*int64(info.FATSize))*int64(info.BytesPerSector)
}

// rootDir is the directory reference of the root, as readDirBuf takes it.
func (fs *fat32FS) rootDir() uint32 {
	if fs.info.fixedRoot() {
		return fixedRootDir
	}
	return fs.info.RootCluster
}

// checkWritable refuses mutation of a FAT12/16 volume.
func (fs *fat32FS) checkWritable() error {
	if fs.info.fixedRoot() {
		return ErrReadOnlyFAT
	}
	return nil
}

// readFixedRoot reads the whole FAT12/16 root directory region. The region is
// proven to exist before the buffer is allocated, so a forged BPB_RootEntCnt
// pointing past the image is refused, not reserved.
func (fs *fat32FS) readFixedRoot() ([]byte, error) {
	n := int64(fs.info.RootEntries) * dirEntrySize
	return readBacked(fs.f, fs.info.rootRegionOffset(fs.partOffset), n)
}

// badCluster is the lowest value that no longer names a data cluster: the bad
// cluster mark of the volume's width.
func (fs *fat32FS) badCluster() uint32 {
	switch fs.info.Type {
	case FAT12:
		return 0xFF7
	case FAT16:
		return 0xFFF7
	}
	return 0x0FFFFFF7
}

// eocCluster is the lowest end-of-chain mark of the volume's width.
func (fs *fat32FS) eocCluster() uint32 { return fs.badCluster() + 1 }

// fatEntry reads the FAT entry of cluster, masked to the volume's width.
//
// FAT12 entries are 12 bits packed: the entry for N sits at byte offset
// N + N/2; an odd N takes the high 12 bits of the little-endian word there, an
// even N the low 12. The word is read as two bytes at a byte offset, so an
// entry that straddles a sector boundary is read like any other.
func (fs *fat32FS) fatEntry(cluster uint32) (uint32, error) {
	fatBase := fs.info.FATOffset(fs.partOffset)
	switch fs.info.Type {
	case FAT12:
		if cluster >= uint32(fs.info.DataClusters)+2 {
			return 0, fmt.Errorf("fat32: cluster %d is outside the volume", cluster)
		}
		var b [2]byte
		if _, err := fs.f.ReadAt(b[:], fatBase+int64(cluster)+int64(cluster)/2); err != nil {
			return 0, err
		}
		v := uint32(binary.LittleEndian.Uint16(b[:]))
		if cluster&1 == 1 {
			return v >> 4, nil
		}
		return v & 0x0FFF, nil
	case FAT16:
		if cluster >= uint32(fs.info.DataClusters)+2 {
			return 0, fmt.Errorf("fat32: cluster %d is outside the volume", cluster)
		}
		var b [2]byte
		if _, err := fs.f.ReadAt(b[:], fatBase+int64(cluster)*2); err != nil {
			return 0, err
		}
		return uint32(binary.LittleEndian.Uint16(b[:])), nil
	}
	var b [4]byte
	if _, err := fs.f.ReadAt(b[:], fatBase+int64(cluster)*4); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]) & 0x0FFFFFFF, nil
}

// readInfoFAT1216 decodes a boot sector whose BPB_FATSz16 is non-zero: the
// FAT12/16 layout. The caller has validated the fields common to all variants.
//
// The variant is decided by the data-cluster count alone (fatgen103), never by
// the "FAT12"/"FAT16" label at offset 54 and never by BPB_RootEntCnt alone:
//
//	RootDirSectors = (RootEntCnt*32 + BytsPerSec-1) / BytsPerSec
//	FirstDataSector = RsvdSecCnt + NumFATs*FATSz + RootDirSectors
//	CountOfClusters = (TotSec - FirstDataSector) / SecPerClus
//
// All arithmetic is in uint64 so a forged BPB cannot overflow it, whatever
// the width of int on the host.
func readInfoFAT1216(buf []byte, bytesPerSector uint16, sectorsPerCluster uint8, reservedSectors uint16,
	fatCount uint8, rootEntries uint16, totalSectors, fatSize uint32) (Info, error) {
	le := binary.LittleEndian
	if rootEntries == 0 {
		return Info{}, errors.New("fat32: FAT12/16 boot sector has no root directory entries")
	}
	rootBytes := uint64(rootEntries) * dirEntrySize
	if rootBytes%uint64(bytesPerSector) != 0 {
		return Info{}, fmt.Errorf("fat32: root entry count %d does not fill whole %d-byte sectors", rootEntries, bytesPerSector)
	}
	dataStart := uint64(reservedSectors) + uint64(fatCount)*uint64(fatSize) + rootBytes/uint64(bytesPerSector)
	if uint64(totalSectors) <= dataStart {
		return Info{}, fmt.Errorf("fat32: data area starts at sector %d, beyond the %d-sector volume", dataStart, totalSectors)
	}
	count := (uint64(totalSectors) - dataStart) / uint64(sectorsPerCluster)
	if count > maxFAT16Clusters {
		return Info{}, fmt.Errorf("fat32: %d data clusters is too many for a FAT12/16 layout", count)
	}
	typ, bits := FAT16, uint64(16)
	if count <= maxFAT12Clusters {
		typ, bits = FAT12, 12
	}
	// One FAT can describe only so many clusters; a BPB claiming more is
	// clamped to what the table holds (Linux does the same).
	// fatSize >= 1 here, so the table holds at least 341 entries and the
	// subtraction cannot wrap.
	if fatEntries := uint64(fatSize) * uint64(bytesPerSector) * 8 / bits; fatEntries < count+2 {
		count = fatEntries - 2
	}
	if count == 0 {
		return Info{}, errors.New("fat32: volume has no data clusters")
	}
	return Info{
		Type:              typ,
		RootEntries:       rootEntries,
		DataClusters:      uint32(count),
		OEMName:           trimASCII(buf[3:11]),
		BytesPerSector:    bytesPerSector,
		SectorsPerCluster: sectorsPerCluster,
		ReservedSectors:   reservedSectors,
		FATCount:          fatCount,
		TotalSectors:      totalSectors,
		FATSize:           fatSize,
		HiddenSectors:     le.Uint32(buf[28:]),
		DriveNumber:       buf[36],
		VolumeID:          le.Uint32(buf[39:]),
		VolumeLabel:       trimASCII(buf[43:54]),
		TypeLabel:         trimASCII(buf[54:62]),
	}, nil
}
