package filesystem_fat32

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Resizing a FAT12 or FAT16 volume.
//
// Bibliography: Linux fs/fat/inode.c fat_fill_super (opened 2026-11-02) reads
// total_sectors from BPB_TotSec16 and, when that is 0, from BPB_TotSec32, then
//
//	data_start     = reserved + fats*fat_length + rootdir_sectors
//	total_clusters = (total_sectors - data_start) / sec_per_clus
//	fat_bits       = total_clusters > MAX_FAT12 (4084) ? 16 : 12
//	total_clusters = min(total_clusters, fat_clusters - FAT_START_ENT)
//
// where fat_clusters is what fat_length sectors can hold. fatgen103 writes the
// same rule and says TotSec16 is used when the count fits 16 bits.
//
// Layout of the narrow variants: reserved | FAT0 | FAT1 | fixed root | data.
// The FATs sit before the root directory and the data, so the FAT cannot grow
// without moving both. The design choice:
//
//   - Grow works while the FAT already has room for the new cluster count (the
//     FAT size, the root and every cluster offset stay where they are). A grow
//     that needs more FAT sectors is refused with ErrResizeFATFull. A shrink
//     leaves the FAT at its size, so shrink-then-grow always fits.
//   - Shrink refuses while any cluster past the new end is in use (as FAT32's
//     Shrink does: nothing is relocated).
//   - A resize that moves the cluster count across 4085 or 65525 changes the
//     FAT type and is refused with ErrResizeType.
//
// Crash safety (the ordering of the write path): the volume geometry lives in
// one sector, the boot sector, and is written last on Grow (everything it will
// expose is in place) and first on Shrink (nothing it still exposes is touched),
// so a crash between two writes leaves a volume that is the old size or the new
// one, never an inconsistent mix. The image file is extended before the BPB
// grows and cut after the BPB shrinks.

// narrowGeometry returns the data-cluster count that totalSectors gives this
// volume's layout (not yet clamped), and how many clusters its FAT can address.
func (fs *fat32FS) narrowGeometry(totalSectors int64) (count, capacity int64) {
	info := fs.info
	dataStart := int64(info.ReservedSectors) + int64(info.FATCount)*int64(info.FATSize) + int64(info.RootDirSectors())
	if totalSectors > dataStart {
		count = (totalSectors - dataStart) / int64(info.SectorsPerCluster)
	}
	bits := int64(16)
	if info.Type == FAT12 {
		bits = 12
	}
	capacity = int64(info.FATSize)*int64(info.BytesPerSector)*8/bits - 2
	return count, capacity
}

// typeOfCount is the FAT type a data-cluster count gives (fatgen103).
func typeOfCount(count int64) FATType {
	switch {
	case count <= maxFAT12Clusters:
		return FAT12
	case count <= maxFAT16Clusters:
		return FAT16
	}
	return FAT32
}

// narrowResizeChecks are the refusals shared by Grow and Shrink: the volume
// must be a whole image, the size positive, a whole number of clusters, and a
// sector count that fits BPB_TotSec32 on every platform.
func (fs *fat32FS) narrowResizeChecks(op string, newSize int64) (newTotal int64, err error) {
	if fs.partOffset != 0 {
		return 0, fmt.Errorf("fat32: %s: refusing to resize filesystem inside a partition (offset %d); resize the partition first", op, fs.partOffset)
	}
	bps := int64(fs.info.BytesPerSector)
	if newSize <= 0 {
		return 0, fmt.Errorf("fat32: %s: new size %d is not positive", op, newSize)
	}
	// A whole number of sectors, not of clusters: the data region starts after
	// the FATs and the fixed root, at a sector that need not be a multiple of
	// the cluster size, so a volume of whole clusters is rarely a multiple of
	// the cluster size. The partial cluster left at the end is slack.
	if newSize%bps != 0 {
		return 0, fmt.Errorf("fat32: %s: new size %d is not a multiple of sector size %d", op, newSize, bps)
	}
	newTotal = newSize / bps
	if newTotal > math.MaxUint32 {
		return 0, fmt.Errorf("fat32: %s: new size %d exceeds 32-bit sector count", op, newSize)
	}
	return newTotal, nil
}

// refuseTypeChange names both types when count would change the volume's.
func (fs *fat32FS) refuseTypeChange(op, gerund string, count int64) error {
	if t := typeOfCount(count); t != fs.info.Type {
		return fmt.Errorf("fat32: %s: %s to %d clusters would make this %s volume %s; copy it into a new image of that size with CopyResized (or reformat): %w",
			op, gerund, count, fs.info.Type, t, ErrResizeType)
	}
	return nil
}

// growNarrow grows a FAT12/16 volume while its FAT has room (see the design
// note above).
func (fs *fat32FS) growNarrow(newSize int64) error {
	cur, err := fs.currentSize()
	if err != nil {
		return err
	}
	if newSize <= cur {
		return fmt.Errorf("fat32: Grow: new size %d <= current size %d", newSize, cur)
	}
	newTotal, err := fs.narrowResizeChecks("Grow", newSize)
	if err != nil {
		return err
	}
	newFAT, count, err := fs.narrowGrowGeometry(newTotal)
	if err != nil {
		return err
	}
	if err := fs.refuseTypeChange("Grow", "growing", count); err != nil {
		return err
	}
	oldFAT := int64(fs.info.FATSize)
	if newFAT > 0xFFFF {
		return fmt.Errorf("fat32: Grow: %d clusters need a FAT of %d sectors, more than BPB_FATSz16 holds: %w", count, newFAT, ErrResizeFAT)
	}
	if count < int64(fs.info.DataClusters) {
		return fmt.Errorf("fat32: Grow: new size %d is too small: with the FAT grown to %d sectors it holds %d clusters, fewer than the %d the volume has: %w",
			newSize, newFAT, count, fs.info.DataClusters, ErrResizeFAT)
	}
	t, ok := fs.f.(truncater)
	if !ok {
		return fmt.Errorf("fat32: Grow: backing store does not support Truncate")
	}
	// Step 1, invisible to the old volume: zero the dead FAT entries the grown
	// volume exposes, in the old layout (only those the old FAT holds).
	oldLast := fs.lastCluster()
	_, capacity := fs.narrowGeometry(int64(fs.info.TotalSectors))
	last := uint32(count)
	if newFAT != oldFAT || int64(last) > capacity {
		last = uint32(capacity)
	}
	if last+2 > oldLast {
		if err := fs.clearFATTail(oldLast, last+2); err != nil {
			return fmt.Errorf("fat32: Grow: %w", err)
		}
	}
	// Step 2, invisible: extend the image (new bytes are zero).
	if err := t.Truncate(newSize); err != nil {
		return fmt.Errorf("fat32: Grow: truncate to %d: %w", newSize, err)
	}
	if newFAT != oldFAT {
		// Steps 3-5 are the window (see the design note): the volume is not
		// consistent until step 6.
		if err := fs.moveNarrowLayout(oldFAT, newFAT); err != nil {
			return fmt.Errorf("fat32: Grow: %w (the volume is inconsistent: the layout move was interrupted)", err)
		}
	}
	// Step 6: one write per boot sector, the backup first, exposes the whole
	// new geometry (FAT size and total sectors together).
	if err := fs.writeNarrowGeometry(uint32(newFAT), uint32(newTotal)); err != nil {
		return fmt.Errorf("fat32: Grow: write boot sector: %w", err)
	}
	fs.info.FATSize = uint32(newFAT)
	fs.info.TotalSectors = uint32(newTotal)
	fs.info.DataClusters = uint32(count)
	fs.invalidateFreeCount()
	return nil
}

// narrowGrowGeometry picks the smallest FAT, no smaller than the current one,
// that addresses the clusters left once it is deducted from newTotal sectors
// (the same fixpoint Format solves, as parted's calc_sizes does), and returns it
// with the cluster count.
func (fs *fat32FS) narrowGrowGeometry(newTotal int64) (fat, count int64, err error) {
	info := fs.info
	bits := int64(16)
	if info.Type == FAT12 {
		bits = 12
	}
	bps := int64(info.BytesPerSector)
	fixed := int64(info.ReservedSectors) + int64(info.RootDirSectors())
	fat = int64(info.FATSize)
	for i := 0; i < 64; i++ {
		count = (newTotal - fixed - int64(info.FATCount)*fat) / int64(info.SectorsPerCluster)
		if count <= 0 {
			return 0, 0, fmt.Errorf("fat32: Grow: %d sectors leave no data cluster: %w", newTotal, ErrResizeFAT)
		}
		need := ((count+2)*bits + 8*bps - 1) / (8 * bps)
		if need <= fat {
			return fat, count, nil
		}
		fat = need
	}
	return 0, 0, fmt.Errorf("fat32: Grow: the FAT size did not converge for %d sectors: %w", newTotal, ErrResizeFAT)
}

// moveChunk is the largest buffer the layout move allocates, whatever the
// volume's size.
const moveChunk = 64 << 10

// moveForward copies n bytes from src to src+delta (delta > 0, ranges may
// overlap) from the end backwards, in chunks, so no source byte is overwritten
// before it is read.
func (fs *fat32FS) moveForward(src, n, delta int64, buf []byte) error {
	for n > 0 {
		c := int64(len(buf))
		if c > n {
			c = n
		}
		n -= c
		b := buf[:c]
		if _, err := fs.f.ReadAt(b, src+n); err != nil {
			return fmt.Errorf("read %d bytes at %d: %w", c, src+n, err)
		}
		if _, err := fs.f.WriteAt(b, src+n+delta); err != nil {
			return fmt.Errorf("write %d bytes at %d: %w", c, src+n+delta, err)
		}
	}
	return nil
}

// zeroRange writes n zero bytes at off, in chunks.
func (fs *fat32FS) zeroRange(off, n int64, buf []byte) error {
	clear(buf)
	for n > 0 {
		c := int64(len(buf))
		if c > n {
			c = n
		}
		if _, err := fs.f.WriteAt(buf[:c], off); err != nil {
			return fmt.Errorf("zero %d bytes at %d: %w", c, off, err)
		}
		off += c
		n -= c
	}
	return nil
}

// moveNarrowLayout re-lays a FAT12/16 volume out for a bigger FAT:
//
//	before: reserved | FAT0 | FAT1 ... | root | data
//	after:  reserved | FAT0 + d | FAT1 + d ... | root | data   (shifted by delta)
//
// Step 3 moves root + data forward by delta = FATCount*(newFAT-oldFAT) sectors,
// from the end backwards. Step 4 moves FAT copy i (from the last to 1) to its
// new offset; the bytes it lands on are the stale old root/data start, already
// moved. Step 5 zeroes the new tail of every FAT copy. Cluster numbers do not
// change, so no FAT entry, directory entry or chain is rewritten.
func (fs *fat32FS) moveNarrowLayout(oldFAT, newFAT int64) error {
	info := fs.info
	bps := int64(info.BytesPerSector)
	n := int64(info.FATCount)
	fatBase := int64(info.ReservedSectors) * bps
	oldFATBytes := oldFAT * bps
	newFATBytes := newFAT * bps
	delta := n * (newFATBytes - oldFATBytes)
	rootStart := fatBase + n*oldFATBytes
	dataEnd := rootStart + int64(info.RootDirSectors())*bps +
		int64(info.DataClusters)*int64(info.SectorsPerCluster)*bps
	buf := make([]byte, moveChunk)
	if err := fs.moveForward(rootStart, dataEnd-rootStart, delta, buf); err != nil {
		return err
	}
	for i := n - 1; i >= 1; i-- {
		if err := fs.moveForward(fatBase+i*oldFATBytes, oldFATBytes, i*(newFATBytes-oldFATBytes), buf); err != nil {
			return err
		}
	}
	for i := int64(0); i < n; i++ {
		if err := fs.zeroRange(fatBase+i*newFATBytes+oldFATBytes, newFATBytes-oldFATBytes, buf); err != nil {
			return err
		}
	}
	return nil
}

// writeNarrowGeometry rewrites BPB_FATSz16 and the total-sector fields in one
// write per boot sector; a backup, when there is one, goes first and the
// primary last.
func (fs *fat32FS) writeNarrowGeometry(fatSize, total uint32) error {
	offs := fs.bootSectorOffsets()
	for i := len(offs) - 1; i >= 0; i-- {
		buf := make([]byte, fs.info.BytesPerSector)
		if _, err := fs.f.ReadAt(buf, offs[i]); err != nil {
			return fmt.Errorf("read boot sector at %d: %w", offs[i], err)
		}
		if buf[510] != 0x55 || buf[511] != 0xAA {
			return fmt.Errorf("boot signature missing at %d", offs[i])
		}
		binary.LittleEndian.PutUint16(buf[22:24], uint16(fatSize))
		if total <= 0xFFFF {
			binary.LittleEndian.PutUint16(buf[19:21], uint16(total))
			binary.LittleEndian.PutUint32(buf[32:36], 0)
		} else {
			binary.LittleEndian.PutUint16(buf[19:21], 0)
			binary.LittleEndian.PutUint32(buf[32:36], total)
		}
		if _, err := fs.f.WriteAt(buf, offs[i]); err != nil {
			return fmt.Errorf("write boot sector at %d: %w", offs[i], err)
		}
	}
	return nil
}

// shrinkNarrow shrinks a FAT12/16 volume when no cluster past the new end is
// in use. Nothing is relocated, and the FAT keeps its size.
func (fs *fat32FS) shrinkNarrow(newSize int64) error {
	cur, err := fs.currentSize()
	if err != nil {
		return err
	}
	if newSize >= cur {
		return fmt.Errorf("fat32: Shrink: new size %d >= current size %d", newSize, cur)
	}
	newTotal, err := fs.narrowResizeChecks("Shrink", newSize)
	if err != nil {
		return err
	}
	if newTotal >= int64(fs.info.TotalSectors) {
		return fmt.Errorf("fat32: Shrink: new size %d does not cut into the %d-sector volume", newSize, fs.info.TotalSectors)
	}
	count, _ := fs.narrowGeometry(newTotal)
	if count < resizeMinDataClusters {
		return fmt.Errorf("fat32: Shrink: new size %d yields %d data clusters, below floor %d",
			newSize, count, resizeMinDataClusters)
	}
	if err := fs.refuseTypeChange("Shrink", "shrinking", count); err != nil {
		return err
	}
	for c := uint32(count) + 2; c < fs.lastCluster(); c++ {
		v, err := fs.fatEntry(c)
		if err != nil {
			return fmt.Errorf("fat32: Shrink: read FAT entry %d: %w", c, err)
		}
		if v != 0 {
			return fmt.Errorf("fat32: Shrink: cluster %d is in use (FAT entry 0x%x); new size would lose data", c, v)
		}
	}
	t, ok := fs.f.(truncater)
	if !ok {
		return fmt.Errorf("fat32: Shrink: backing store does not support Truncate")
	}
	if err := fs.writeTotalSectors(uint32(newTotal)); err != nil {
		return fmt.Errorf("fat32: Shrink: write total sectors: %w", err)
	}
	fs.info.TotalSectors = uint32(newTotal)
	fs.info.DataClusters = uint32(count)
	fs.invalidateFreeCount()
	if err := t.Truncate(newSize); err != nil {
		return fmt.Errorf("fat32: Shrink: truncate to %d: %w", newSize, err)
	}
	return nil
}

// clearFATTail zeroes, in every FAT copy, the entries of clusters
// [oldLast, newLast) and the dead bits right after them, so that a grow
// exposes only free clusters whatever the slack held (another tool may have
// left bytes there). The entries below oldLast are not touched: FAT12 packs two
// entries into three bytes, so when oldLast is odd the byte it shares with the
// in-use entry below keeps that entry's nibble.
func (fs *fat32FS) clearFATTail(oldLast, newLast uint32) error {
	var start, end int64 // byte range within one FAT copy
	keep := false        // start is the byte shared with entry oldLast-1
	if fs.info.Type == FAT16 {
		start, end = int64(oldLast)*2, int64(newLast)*2
	} else {
		start = int64(oldLast) + int64(oldLast)/2
		end = int64(newLast-1) + int64(newLast-1)/2 + 2
		keep = oldLast&1 == 1
	}
	fatBase := fs.info.FATOffset(fs.partOffset)
	fatBytes := int64(fs.info.FATSize) * int64(fs.info.BytesPerSector)
	buf := make([]byte, end-start)
	for i := uint8(0); i < fs.info.FATCount; i++ {
		off := fatBase + int64(i)*fatBytes + start
		if keep {
			if _, err := fs.f.ReadAt(buf[:1], off); err != nil {
				return fmt.Errorf("read FAT%d tail at %d: %w", i, off, err)
			}
			buf[0] &= 0x0F
		}
		if _, err := fs.f.WriteAt(buf, off); err != nil {
			return fmt.Errorf("clear FAT%d tail at %d: %w", i, off, err)
		}
	}
	return nil
}
