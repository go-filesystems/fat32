package filesystem_fat32

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"

	filesystem "github.com/go-filesystems/interface"
)

// FormatConfig holds optional parameters for Format.
// All fields are optional; sensible defaults are used when left at their zero value.
type FormatConfig struct {
	// Label is the volume label stored in the BPB (trimmed to 11 bytes).
	Label string
	// VolumeID is the 32-bit volume serial number. A random value is generated when zero.
	VolumeID uint32
}

// Layout parameters for a freshly formatted FAT32 volume.
const (
	fmtBytesPerSector    = 512
	fmtSectorsPerCluster = 8  // 4 KiB clusters
	fmtReservedSectors   = 32 // includes boot sector + FSInfo + backup
	fmtFATCount          = 2
	fmtFSInfoSector      = 1
	fmtBackupBootSector  = 6
	fmtRootCluster       = 2
	fmtClusterSize       = fmtBytesPerSector * fmtSectorsPerCluster
)

type formatFile interface {
	WriteAt([]byte, int64) (int, error)
	Truncate(int64) error
	Close() error
}

var formatOpenFile = func(path string) (formatFile, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
}

var formatRandUint32 = func() uint32 {
	return rand.Uint32()
}

var formatOpenFS = Open

// hasNonSpace reports whether s contains any byte other than ASCII space,
// i.e. whether the (space-padded) label carries an actual volume name.
func hasNonSpace(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			return true
		}
	}
	return false
}

// fmtLayout is what a format computes from the size before writing anything.
type fmtLayout struct {
	totalSectors   int64
	fatSizeSectors int64
}

// formatLayout validates sizeBytes and sizes the FATs. It writes nothing, so
// an invalid size is refused before a file is created or a byte is written.
func formatLayout(sizeBytes int64) (fmtLayout, error) {
	if sizeBytes%fmtClusterSize != 0 {
		return fmtLayout{}, fmt.Errorf("fat32: format: size %d is not a multiple of cluster size %d",
			sizeBytes, fmtClusterSize)
	}
	totalSectors := sizeBytes / fmtBytesPerSector
	// The BPB's total-sector field is 32 bits wide: a larger size would be
	// written truncated and describe a different, smaller volume.
	if totalSectors > 0xFFFFFFFF {
		return fmtLayout{}, fmt.Errorf("fat32: format: size %d is larger than FAT32 can describe", sizeBytes)
	}

	// FAT size in sectors: enough entries for every cluster (4 bytes each).
	// Clusters = (total - reserved) / sectorsPerCluster, rounded up.
	dataSectors := totalSectors - fmtReservedSectors
	// Approximate FAT size: each FAT entry is 4 bytes; entries cover all data clusters + 2 reserved.
	// We iterate to converge FAT size (since FAT itself occupies space).
	fatSizeSectors := int64(1)
	for {
		clusterCount := (dataSectors - 2*fatSizeSectors) / fmtSectorsPerCluster
		needed := (clusterCount+2)*4/fmtBytesPerSector + 1
		if needed <= fatSizeSectors {
			break
		}
		fatSizeSectors = needed
	}

	minSectors := int64(fmtReservedSectors) + 2*fatSizeSectors + int64(fmtSectorsPerCluster)
	if totalSectors < minSectors {
		return fmtLayout{}, fmt.Errorf("fat32: format: size %d too small (minimum %d bytes)",
			sizeBytes, minSectors*fmtBytesPerSector)
	}
	return fmtLayout{totalSectors: totalSectors, fatSizeSectors: fatSizeSectors}, nil
}

// Format creates a new FAT32 filesystem in the file at path.
// The file is created (or truncated) and formatted. sizeBytes must be a
// multiple of the cluster size (4096) and large enough to hold the reserved
// area, two FATs, and at least one data cluster.
//
// On success the newly formatted filesystem is opened and returned; the
// caller must Close it when done.
func Format(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error) {
	l, err := formatLayout(sizeBytes)
	if err != nil {
		return nil, err
	}

	f, err := formatOpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("fat32: format: %w", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		f.Close()
		return nil, fmt.Errorf("fat32: format: truncate: %w", err)
	}

	// A file just truncated to its size reads back as zeros, so only the
	// metadata that is not zero is written.
	if err := writeFormat(f, l, cfg, false); err != nil {
		f.Close()
		return nil, err
	}

	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("fat32: format: close: %w", err)
	}

	return formatOpenFS(path, -1)
}

// ReaderWriterAt is the image FormatImage formats: anything that can be read
// and written at an offset, such as a buffer in memory.
type ReaderWriterAt interface {
	io.ReaderAt
	io.WriterAt
}

// FormatImage creates a new FAT32 filesystem in the first sizeBytes of rw and
// opens it, with the same layout and size rules as Format.
//
// It is Format without a path: for a caller that has no host file to give --
// a WebAssembly page building an image in memory, a test, a partition inside
// a larger image. rw must already hold at least sizeBytes; FormatImage never
// grows it.
//
// Unlike Format, which writes a file it has just truncated, FormatImage cannot
// assume rw reads back as zeros. It therefore writes zeros over every region
// a FAT32 reader interprets -- the reserved sectors, both FATs and the root
// directory cluster -- so a reused buffer holding an older volume does not
// leak entries into the new one. Data clusters are left as they are: they are
// free in the new FAT, and nothing reads a free cluster.
//
// The returned filesystem reads and writes through rw (see OpenReader); it
// does not close rw.
func FormatImage(rw ReaderWriterAt, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error) {
	if rw == nil {
		return nil, errors.New("fat32: format: FormatImage needs an image")
	}
	l, err := formatLayout(sizeBytes)
	if err != nil {
		return nil, err
	}
	if err := writeFormat(rw, l, cfg, true); err != nil {
		return nil, err
	}
	return OpenReader(rw, sizeBytes)
}

// fmtZeroChunk bounds the buffer used to zero the metadata regions, so
// formatting a large image does not allocate the size of its FATs.
const fmtZeroChunk = 64 << 10

// zeroRange writes n zero bytes at off, in chunks of at most fmtZeroChunk.
func zeroRange(w io.WriterAt, off, n int64) error {
	buf := make([]byte, min(n, fmtZeroChunk))
	for n > 0 {
		k := min(n, int64(len(buf)))
		if _, err := w.WriteAt(buf[:k], off); err != nil {
			return err
		}
		off += k
		n -= k
	}
	return nil
}

// writeFormat writes the boot sector, its backup, FSInfo, the first FAT
// entries of both FATs and the volume-label entry. With zero set it first
// clears every region those structures live in.
func writeFormat(w io.WriterAt, l fmtLayout, cfg FormatConfig, zero bool) error {
	dataOff := int64(fmtReservedSectors+fmtFATCount*l.fatSizeSectors) * fmtBytesPerSector
	rootOff := dataOff + int64(fmtRootCluster-2)*fmtClusterSize
	if zero {
		// Reserved area + both FATs are contiguous, and the root cluster
		// follows them directly.
		if err := zeroRange(w, 0, rootOff+fmtClusterSize); err != nil {
			return fmt.Errorf("fat32: format: clear metadata: %w", err)
		}
	}

	volumeID := cfg.VolumeID
	if volumeID == 0 {
		volumeID = formatRandUint32()
		if volumeID == 0 {
			volumeID = 0x12345678
		}
	}

	label := cfg.Label
	if len(label) > 11 {
		label = label[:11]
	}
	for len(label) < 11 {
		label += " "
	}

	le := binary.LittleEndian

	// ── Boot sector (sector 0) ────────────────────────────────────────────────
	boot := make([]byte, fmtBytesPerSector)
	boot[0] = 0xEB
	boot[1] = 0x58
	boot[2] = 0x90
	copy(boot[3:11], []byte("MSDOS5.0"))
	le.PutUint16(boot[11:], fmtBytesPerSector)
	boot[13] = fmtSectorsPerCluster
	le.PutUint16(boot[14:], fmtReservedSectors)
	boot[16] = fmtFATCount
	// boot[17:19] root entry count = 0 (FAT32)
	// boot[19:21] total sectors 16-bit = 0 (use 32-bit field)
	boot[21] = 0xF8 // media descriptor
	// boot[22:24] FAT16 size = 0 (use FAT32 field)
	le.PutUint16(boot[24:], 63)  // sectors per track (dummy)
	le.PutUint16(boot[26:], 255) // number of heads (dummy)
	// boot[28:32] hidden sectors = 0
	le.PutUint32(boot[32:], uint32(l.totalSectors))
	le.PutUint32(boot[36:], uint32(l.fatSizeSectors))
	// boot[40:42] ext flags = 0 (both FATs active, mirrored)
	le.PutUint16(boot[42:], 0) // filesystem version 0.0
	le.PutUint32(boot[44:], fmtRootCluster)
	le.PutUint16(boot[48:], fmtFSInfoSector)
	le.PutUint16(boot[50:], fmtBackupBootSector)
	boot[64] = 0x80 // drive number
	boot[66] = 0x29 // extended boot signature
	le.PutUint32(boot[67:], volumeID)
	copy(boot[71:82], label)
	copy(boot[82:90], []byte("FAT32   "))
	boot[510] = 0x55
	boot[511] = 0xAA
	if _, err := w.WriteAt(boot, 0); err != nil {
		return fmt.Errorf("fat32: format: write boot sector: %w", err)
	}
	// Write backup boot sector.
	if _, err := w.WriteAt(boot, int64(fmtBackupBootSector)*fmtBytesPerSector); err != nil {
		return fmt.Errorf("fat32: format: write backup boot sector: %w", err)
	}

	// ── FSInfo sector ────────────────────────────────────────────────────────
	fsinfo := make([]byte, fmtBytesPerSector)
	le.PutUint32(fsinfo[0:], 0x41615252)   // lead signature
	le.PutUint32(fsinfo[484:], 0x61417272) // structure signature
	// Free count: every data cluster but the root directory's. fsck tools
	// report an unknown (0xFFFFFFFF) count as unset. Next free: the cluster
	// after the root.
	dataClusters := (l.totalSectors - fmtReservedSectors - fmtFATCount*l.fatSizeSectors) / fmtSectorsPerCluster
	if fatEntries := l.fatSizeSectors*fmtBytesPerSector/4 - 2; fatEntries < dataClusters {
		dataClusters = fatEntries
	}
	le.PutUint32(fsinfo[488:], uint32(dataClusters-1))
	le.PutUint32(fsinfo[492:], fmtRootCluster+1)
	le.PutUint32(fsinfo[508:], 0xAA550000) // trail signature
	if _, err := w.WriteAt(fsinfo, int64(fmtFSInfoSector)*fmtBytesPerSector); err != nil {
		return fmt.Errorf("fat32: format: write FSInfo: %w", err)
	}
	// The specification keeps a copy at sector 7, after the backup boot sector.
	if _, err := w.WriteAt(fsinfo, int64(fmtBackupBootSector+1)*fmtBytesPerSector); err != nil {
		return fmt.Errorf("fat32: format: write backup FSInfo: %w", err)
	}

	// ── FAT regions ──────────────────────────────────────────────────────────
	// FAT entries 0 and 1 are reserved; entry 2 (root cluster) = EOC.
	fatEntry := make([]byte, 12)
	le.PutUint32(fatEntry[0:], 0x0FFFFFF8) // FAT[0]: media descriptor + 0xFFFFF
	le.PutUint32(fatEntry[4:], 0x0FFFFFFF) // FAT[1]: end-of-chain marker
	le.PutUint32(fatEntry[8:], 0x0FFFFFFF) // FAT[2] = root cluster: EOC

	for i := int64(0); i < fmtFATCount; i++ {
		fatOff := int64(fmtReservedSectors+i*l.fatSizeSectors) * fmtBytesPerSector
		if _, err := w.WriteAt(fatEntry, fatOff); err != nil {
			return fmt.Errorf("fat32: format: write FAT %d: %w", i, err)
		}
	}

	// ── Root-directory volume-label entry ────────────────────────────────────
	// When a label is set in the BPB, canonical fsck tools (dosfstools
	// fsck.fat >= 4.2) require a matching ATTR_VOLUME_ID (0x08) entry as the
	// first record of the root directory; a boot-sector label with no root
	// entry is reported as an error and makes `fsck -n` exit non-zero. Mirror
	// the space-padded BPB label into the root cluster so the volume is clean.
	if hasNonSpace(label) {
		var volEntry [dirEntrySize]byte
		copy(volEntry[0:11], label) // already space-padded to 11 bytes
		volEntry[11] = fatAttrVolumeID
		if _, err := w.WriteAt(volEntry[:], rootOff); err != nil {
			return fmt.Errorf("fat32: format: write volume-label entry: %w", err)
		}
	}
	return nil
}
