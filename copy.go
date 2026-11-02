package filesystem_fat32

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"unicode/utf16"

	filesystem "github.com/go-filesystems/interface"
)

// CopyResized: a resize that moves nothing in place.
//
// Growing the FAT of a FAT12/16 volume in place would shift the fixed root and
// every data cluster, and an interrupted shift leaves an image that neither
// layout describes: that needs a journal, and is not done. CopyResized instead
// formats a NEW image of the wanted size and copies the tree into it. The
// source is only read, so it is the same after a success and after a failure.
//
// Bibliography: dosfstools src/mkfs.fat.c (opened 2026-11-02, master).
// establish_params: FAT12/FAT16 start at 4 sectors per cluster, 512 root
// entries, one reserved sector; FAT32 starts at the fatgen103 table (<=260 MiB:
// 1, else 8, 16, 32, 64) and has 32 reserved sectors. setup_tables: for each
// sectors-per-cluster value it computes, per FAT width,
//
//	FAT12: clust = 2*(data*512 + nfats*3) / (2*spc*512 + nfats*3)
//	FAT16: clust = (data*512 + nfats*4) / (spc*512 + nfats*2)
//	FAT32: clust = (data*512 + nfats*8) / (spc*512 + nfats*4)
//	fatlen = ceil(entries bytes / 512), clust = (data - nfats*fatlen) / spc
//
// and takes the first width whose count is within its limit (FAT12 <= 4084,
// FAT16 4087..65524, FAT32 >= 65525), trying FAT12, then FAT16, then FAT32;
// when none fits it doubles the cluster size (up to 128 sectors). The 4085 and
// 4086 clusters a FAT16 volume could have are skipped so that no reader takes
// the volume for FAT12.

// ErrCopyNoFit is returned (wrapped) when the tree does not fit the new image.
var ErrCopyNoFit = errors.New("fat32: the tree does not fit the new image")

// ErrCopyRootFull is returned (wrapped) when the root directory of the tree
// needs more entries than the fixed root of a FAT12/16 image has.
var ErrCopyRootFull = errors.New("fat32: the root directory needs more entries than the new image's fixed root has")

// CopyConfig holds the optional parameters of CopyResized.
type CopyConfig struct {
	// Type forces the FAT type of the new image: FAT12, FAT16 or FAT32. The zero
	// value lets the size decide, as mkfs.fat does. A size that cannot be
	// formatted as the forced type is refused.
	Type FATType
	// RootEntries is BPB_RootEntCnt of a FAT12/16 image: a multiple of 16, 512 when
	// zero. It is ignored for FAT32, whose root directory is a cluster chain.
	RootEntries uint16
	// Label is the volume label (11 bytes at most); the source's when empty.
	Label string
	// VolumeID is the serial number; random when zero.
	VolumeID uint32
}

// copyGeometry is the layout the new image gets.
type copyGeometry struct {
	typ         FATType
	total       int64 // sectors
	spc         int64
	reserved    int64
	fatLen      int64
	rootEntries int64
	clusters    int64
}

func (g copyGeometry) rootSectors() int64 { return g.rootEntries * dirEntrySize / fmtBytesPerSector }

const (
	copyFATCount   = 2
	copyMaxSPC     = 128
	copyMinFAT16   = 4087 // mkfs.fat MIN_CLUST_16
	copyMinSectors = 64   // below this nothing sensible fits
)

func cdiv(a, b int64) int64 { return (a + b - 1) / b }

// copyFATSize returns the clusters and FAT length (sectors) a FAT of the given
// width gets, or 0 clusters when the count is outside the width's limits.
func copyFATSize(typ FATType, total, spc, rootEntries int64) (clusters, fatLen int64) {
	const ss = fmtBytesPerSector
	reserved := int64(1)
	root := rootEntries * dirEntrySize / ss
	if typ == FAT32 {
		reserved, root = fmtReservedSectors, 0
	}
	data := total - reserved - root
	switch typ {
	case FAT12:
		clusters = 2 * (data*ss + copyFATCount*3) / (2*spc*ss + copyFATCount*3)
		fatLen = cdiv(((clusters+2)*3+1)>>1, ss)
		clusters = (data - copyFATCount*fatLen) / spc
		if clusters > min(fatLen*2*ss/3, maxFAT12Clusters) {
			return 0, 0
		}
	case FAT16:
		clusters = (data*ss + copyFATCount*4) / (spc*ss + copyFATCount*2)
		fatLen = cdiv((clusters+2)*2, ss)
		clusters = (data - copyFATCount*fatLen) / spc
		if clusters > min(fatLen*ss/2, maxFAT16Clusters) || clusters < copyMinFAT16 {
			return 0, 0
		}
	default:
		clusters = (data*ss + copyFATCount*8) / (spc*ss + copyFATCount*4)
		fatLen = cdiv((clusters+2)*4, ss)
		clusters = (data - copyFATCount*fatLen) / spc
		if clusters > fatLen*ss/4 || clusters < fat32SpecMinClusters {
			return 0, 0
		}
	}
	return clusters, fatLen
}

// pickCopyGeometry chooses the layout for a volume of size bytes.
func pickCopyGeometry(size int64, cfg CopyConfig) (copyGeometry, error) {
	if size <= 0 || size%fmtBytesPerSector != 0 {
		return copyGeometry{}, fmt.Errorf("fat32: CopyResized: size %d is not a positive multiple of the %d-byte sector", size, fmtBytesPerSector)
	}
	total := size / fmtBytesPerSector
	if total > 0xFFFFFFFF {
		return copyGeometry{}, fmt.Errorf("fat32: CopyResized: size %d exceeds the 32-bit sector count of a FAT volume", size)
	}
	if total < copyMinSectors {
		return copyGeometry{}, fmt.Errorf("fat32: CopyResized: size %d is too small for a FAT volume", size)
	}
	switch cfg.Type {
	case 0, FAT12, FAT16, FAT32:
	default:
		return copyGeometry{}, fmt.Errorf("fat32: CopyResized: unknown FAT type %d", cfg.Type)
	}
	root := int64(cfg.RootEntries)
	if root == 0 {
		root = 512
	}
	if root%16 != 0 {
		return copyGeometry{}, fmt.Errorf("fat32: CopyResized: %d root entries do not fill whole %d-byte sectors (use a multiple of 16)", root, fmtBytesPerSector)
	}
	spc := int64(4)
	if cfg.Type == FAT32 {
		switch {
		case total > 32*1024*1024*2:
			spc = 64
		case total > 16*1024*1024*2:
			spc = 32
		case total > 8*1024*1024*2:
			spc = 16
		case total > 260*1024*2:
			spc = 8
		default:
			spc = 1
		}
	}
	order := []FATType{FAT12, FAT16, FAT32}
	if cfg.Type != 0 {
		order = []FATType{cfg.Type}
	}
	for ; spc <= copyMaxSPC; spc <<= 1 {
		for _, typ := range order {
			c, fl := copyFATSize(typ, total, spc, root)
			if c < resizeMinDataClusters {
				continue
			}
			g := copyGeometry{typ: typ, total: total, spc: spc, reserved: 1, fatLen: fl, rootEntries: root, clusters: c}
			if typ == FAT32 {
				g.reserved, g.rootEntries = fmtReservedSectors, 0
			}
			return g, nil
		}
	}
	want := "any FAT type"
	if cfg.Type != 0 {
		want = cfg.Type.String()
	}
	return copyGeometry{}, fmt.Errorf("fat32: CopyResized: a volume of %d bytes cannot be formatted as %s (FAT12 holds up to 4084 clusters, FAT16 4087..65524, FAT32 at least 65525)", size, want)
}

// writeCopyLayout writes the boot sector(s), FSInfo, the first FAT entries and
// the label entry over regions it first zeroes.
func writeCopyLayout(w ReaderWriterAt, g copyGeometry, label string, volumeID uint32) error {
	le := binary.LittleEndian
	const ss = fmtBytesPerSector
	metaEnd := (g.reserved + copyFATCount*g.fatLen + g.rootSectors()) * ss
	if g.typ == FAT32 {
		metaEnd += g.spc * ss // the root cluster
	}
	if err := zeroRange(w, 0, metaEnd); err != nil {
		return fmt.Errorf("fat32: CopyResized: clear metadata: %w", err)
	}
	for len(label) < 11 {
		label += " "
	}
	boot := make([]byte, ss)
	copy(boot[3:11], "MSDOS5.0")
	le.PutUint16(boot[11:], ss)
	boot[13] = byte(g.spc)
	le.PutUint16(boot[14:], uint16(g.reserved))
	boot[16] = copyFATCount
	boot[21] = 0xF8
	le.PutUint16(boot[24:], 63)
	le.PutUint16(boot[26:], 255)
	if g.typ == FAT32 {
		boot[0], boot[1], boot[2] = 0xEB, 0x58, 0x90
		le.PutUint32(boot[32:], uint32(g.total))
		le.PutUint32(boot[36:], uint32(g.fatLen))
		le.PutUint32(boot[44:], fmtRootCluster)
		le.PutUint16(boot[48:], fmtFSInfoSector)
		le.PutUint16(boot[50:], fmtBackupBootSector)
		boot[64], boot[66] = 0x80, 0x29
		le.PutUint32(boot[67:], volumeID)
		copy(boot[71:82], label)
		copy(boot[82:90], "FAT32   ")
	} else {
		boot[0], boot[1], boot[2] = 0xEB, 0x3C, 0x90
		le.PutUint16(boot[17:], uint16(g.rootEntries))
		if g.total <= 0xFFFF {
			le.PutUint16(boot[19:], uint16(g.total))
		} else {
			le.PutUint32(boot[32:], uint32(g.total))
		}
		le.PutUint16(boot[22:], uint16(g.fatLen))
		boot[36], boot[38] = 0x80, 0x29
		le.PutUint32(boot[39:], volumeID)
		copy(boot[43:54], label)
		copy(boot[54:62], fmt.Sprintf("%-8s", g.typ.String()))
	}
	boot[510], boot[511] = 0x55, 0xAA
	if _, err := w.WriteAt(boot, 0); err != nil {
		return fmt.Errorf("fat32: CopyResized: write boot sector: %w", err)
	}
	if g.typ == FAT32 {
		if _, err := w.WriteAt(boot, fmtBackupBootSector*ss); err != nil {
			return fmt.Errorf("fat32: CopyResized: write backup boot sector: %w", err)
		}
		fsinfo := make([]byte, ss)
		le.PutUint32(fsinfo[0:], 0x41615252)
		le.PutUint32(fsinfo[484:], 0x61417272)
		le.PutUint32(fsinfo[488:], uint32(g.clusters-1))
		le.PutUint32(fsinfo[492:], fmtRootCluster+1)
		le.PutUint32(fsinfo[508:], 0xAA550000)
		if _, err := w.WriteAt(fsinfo, fmtFSInfoSector*ss); err != nil {
			return fmt.Errorf("fat32: CopyResized: write FSInfo: %w", err)
		}
		if _, err := w.WriteAt(fsinfo, (fmtBackupBootSector+1)*ss); err != nil {
			return fmt.Errorf("fat32: CopyResized: write backup FSInfo: %w", err)
		}
	}
	// FAT entries 0 and 1 (media descriptor, end of chain), FAT32's root cluster.
	var fat []byte
	switch g.typ {
	case FAT12:
		fat = []byte{0xF8, 0xFF, 0xFF}
	case FAT16:
		fat = []byte{0xF8, 0xFF, 0xFF, 0xFF}
	default:
		fat = make([]byte, 12)
		le.PutUint32(fat[0:], 0x0FFFFFF8)
		le.PutUint32(fat[4:], 0x0FFFFFFF)
		le.PutUint32(fat[8:], 0x0FFFFFFF)
	}
	for i := int64(0); i < copyFATCount; i++ {
		if _, err := w.WriteAt(fat, (g.reserved+i*g.fatLen)*ss); err != nil {
			return fmt.Errorf("fat32: CopyResized: write FAT %d: %w", i, err)
		}
	}
	if hasNonSpace(label) {
		var ent [dirEntrySize]byte
		copy(ent[:11], label)
		ent[11] = fatAttrVolumeID
		root := (g.reserved + copyFATCount*g.fatLen) * ss // FAT12/16 fixed root; FAT32's first cluster
		if _, err := w.WriteAt(ent[:], root); err != nil {
			return fmt.Errorf("fat32: CopyResized: write volume-label entry: %w", err)
		}
	}
	return nil
}

// treeNeeds is what a tree takes from a new image.
type treeNeeds struct {
	clusters    int64 // data clusters, directories included
	rootEntries int64 // 32-byte slots of the root directory, LFN records included
}

// dirSlots is the number of 32-byte records the entry of this name takes.
func entrySlots(name string) int64 {
	if needsLFN(name) {
		return 1 + int64(len(utf16.Encode([]rune(name)))+12)/13
	}
	return 1
}

// measure walks the source tree once, refusing a loop, and totals what copying
// it into volumes of the given cluster size takes.
func (src *fat32FS) measure(dir uint32, clusterSize int64, depth int, seen map[uint32]bool, n *treeNeeds, root bool) error {
	if depth > 64 {
		return fmt.Errorf("fat32: CopyResized: directories nested deeper than 64")
	}
	if seen[dir] {
		return fmt.Errorf("fat32: CopyResized: directory cluster %d is reached twice (a loop in the source)", dir)
	}
	seen[dir] = true
	buf, err := src.readDirBuf(dir)
	if err != nil {
		return err
	}
	slots := int64(2) // "." and ".."
	for _, e := range parseRootDirMetadata(buf) {
		if e.name == "." || e.name == ".." {
			continue
		}
		slots += entrySlots(e.name)
		if e.attr&fatAttrDirectory != 0 {
			if e.cluster < 2 {
				return fmt.Errorf("fat32: CopyResized: directory %q has no cluster", e.name)
			}
			if err := src.measure(e.cluster, clusterSize, depth+1, seen, n, false); err != nil {
				return err
			}
			continue
		}
		n.clusters += cdiv(int64(e.size), clusterSize)
	}
	if root {
		n.rootEntries = slots - 2
	} else {
		n.clusters += cdiv(slots*dirEntrySize, clusterSize)
	}
	return nil
}

// stampOf is the part of an 8.3 record CopyResized carries over besides the
// name, the size and the cluster: creation and access times, write time.
func copyStamps(dst, short []byte) {
	copy(dst[13:20], short[13:20])
	copy(dst[22:26], short[22:26])
}

// setEntryMeta gives the entry at path the attributes and timestamps of a source
// record.
func (fs *fat32FS) setEntryMeta(path string, short []byte) error {
	name, parent, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	buf, err := fs.readDirBuf(parent)
	if err != nil {
		return err
	}
	off, count, ok := fat32FindEntry(buf, name)
	if !ok {
		return fmt.Errorf("fat32: CopyResized: %q vanished from the new image", path)
	}
	e := off + (count-1)*dirEntrySize
	// The attribute bits of the source, but the new entry stays what it was made:
	// directory or not.
	buf[e+11] = short[11]&^(fatAttrVolumeID|fatAttrDirectory) | buf[e+11]&fatAttrDirectory
	copyStamps(buf[e:e+dirEntrySize], short)
	return fs.writeDirBuf(parent, buf)
}

// copyTree copies the directory src at cluster dir into dstPath of dst.
func (src *fat32FS) copyTree(dst *fat32FS, dir uint32, dstPath string) error {
	buf, err := src.readDirBuf(dir)
	if err != nil {
		return err
	}
	for _, e := range parseRootDirMetadata(buf) {
		if e.name == "." || e.name == ".." {
			continue
		}
		p := dstPath + "/" + e.name
		perm := os.FileMode(0o644)
		if e.attr&fatAttrReadOnly != 0 {
			perm = 0o444
		}
		if e.attr&fatAttrDirectory != 0 {
			if err := dst.MkDir(p, perm|0o111); err != nil {
				return fmt.Errorf("copy %s: %w", p, err)
			}
			if err := src.copyTree(dst, e.cluster, p); err != nil {
				return err
			}
		} else {
			var data []byte
			if e.size > 0 {
				if data, err = src.readClusterChain(e.cluster, uint64(e.size)); err != nil {
					return fmt.Errorf("read %s: %w", p, err)
				}
			}
			if err := dst.WriteFile(p, data, perm); err != nil {
				return fmt.Errorf("copy %s: %w", p, err)
			}
		}
		if err := dst.setEntryMeta(p, e.short[:]); err != nil {
			return fmt.Errorf("copy %s: %w", p, err)
		}
	}
	return nil
}

// truncater is declared in resize.go.

// CopyResized formats the first size bytes of dst as a new FAT volume and copies
// the whole tree of src into it: names (long ones too), directories, contents,
// attributes, timestamps and the volume label. It is the resize that FAT12/16
// refuses in place (ErrResizeFATFull, ErrResizeType): a bigger FAT, or another
// FAT type, needs a new image.
//
// src must be a FAT volume opened by this package; it is only read, so it is
// unchanged whether CopyResized succeeds or fails. cfg.Type picks the FAT type
// of the new image (the size decides when it is zero; an impossible
// combination is refused before anything is written). dst is truncated to
// size when it can be; otherwise it must already hold size bytes.
//
// Refusals happen before dst is written when they can be known: a tree that
// needs more clusters than the image has (ErrCopyNoFit), a root directory
// needing more entries than a FAT12/16 fixed root has (ErrCopyRootFull, which
// names both counts). If a write fails half-way, the error is returned and the
// boot sector of dst is cleared so that the half-written image is not taken
// for a volume.
//
// On success the new filesystem is open over dst, as FormatImage returns it; it
// does not close dst.
func CopyResized(dst ReaderWriterAt, size int64, src filesystem.Filesystem, cfg CopyConfig) (filesystem.Filesystem, error) {
	if dst == nil {
		return nil, errors.New("fat32: CopyResized needs a destination image")
	}
	s, ok := src.(*fat32FS)
	if !ok || s == nil {
		return nil, errors.New("fat32: CopyResized: the source must be a FAT volume opened by this package")
	}
	g, err := pickCopyGeometry(size, cfg)
	if err != nil {
		return nil, err
	}
	var need treeNeeds
	if err := s.measure(s.rootDir(), g.spc*fmtBytesPerSector, 0, map[uint32]bool{}, &need, true); err != nil {
		return nil, err
	}
	label := cfg.Label
	if label == "" {
		label = s.Label()
	}
	if len(label) > 11 {
		label = label[:11]
	}
	rootSlots := need.rootEntries
	if hasNonSpace(label) {
		rootSlots++ // the label record
	}
	if g.typ != FAT32 {
		if rootSlots > g.rootEntries {
			return nil, fmt.Errorf("%w: it needs %d entries (long names take extra ones, and the label one), the root of this %s image has %d",
				ErrCopyRootFull, rootSlots, g.typ, g.rootEntries)
		}
	}
	avail := g.clusters
	if g.typ == FAT32 {
		avail-- // the root directory's cluster
	}
	if need.clusters > avail {
		return nil, fmt.Errorf("%w: it needs %d clusters of %d bytes, the %s image has %d",
			ErrCopyNoFit, need.clusters, g.spc*fmtBytesPerSector, g.typ, avail)
	}
	volumeID := cfg.VolumeID
	if volumeID == 0 {
		volumeID = formatRandUint32()
		if volumeID == 0 {
			volumeID = 0x12345678
		}
	}
	if t, ok := dst.(truncater); ok {
		if err := t.Truncate(size); err != nil {
			return nil, fmt.Errorf("fat32: CopyResized: truncate to %d: %w", size, err)
		}
	}
	fail := func(err error) (filesystem.Filesystem, error) {
		// Not a volume any more: whatever was copied is not offered as one.
		_, _ = dst.WriteAt(make([]byte, fmtBytesPerSector), 0)
		return nil, err
	}
	if err := writeCopyLayout(dst, g, label, volumeID); err != nil {
		return fail(err)
	}
	out, err := OpenReader(dst, size)
	if err != nil {
		return fail(fmt.Errorf("fat32: CopyResized: open the new image: %w", err))
	}
	d := out.(*fat32FS)
	if err := s.copyTree(d, s.rootDir(), ""); err != nil {
		return fail(err)
	}
	if err := d.flushFSInfo(); err != nil {
		return fail(err)
	}
	return out, nil
}
