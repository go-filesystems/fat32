package filesystem_fat32

import (
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
//   - Grow works in place. While the FAT already has room for the new cluster
//     count nothing moves (the root and every cluster offset stay put). When it
//     does not, the FAT is made bigger and root + data shift forward by the
//     delta (see moveNarrowLayout); cluster numbers do not change, so no FAT
//     entry, directory entry or chain is rewritten. This differs from GNU
//     parted (libparted/fs/r/fat/resize.c, opened 2026-11-02), which builds a
//     whole new layout, duplicates clusters into it (fat_duplicate_clusters),
//     renumbers them, rewrites the FAT and directory tree, and writes the boot
//     sector last (fat_resize). Parted pads the reserved area
//     (fat_calc_align_sectors) to keep old and new clusters aligned; the FAT
//     size comes from the same fixpoint as calc_sizes in calc.c, reproduced by
//     narrowGrowGeometry. A shrink leaves the FAT at its size.
//   - Crash safety: step 1 (zero dead FAT slack) and step 2 (extend the image)
//     are invisible to the old volume. Steps 3-5 (move root+data, move FAT
//     copies, zero FAT tails) overwrite the old layout in place and are
//     journaled and rolled forward by Open (growjournal.go): an interrupted grow
//     is finished by the next Open, or never started when the record was not
//     written. TestFAT1216GrowFailureAtEveryWrite fails every write in turn,
//     reopens, and asserts the volume is fsck-clean with every hash intact.
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
		// Steps 3-6: the journaled layout move (growjournal.go).
		if err := fs.journaledGrow(newFAT, newTotal, newSize); err != nil {
			return fmt.Errorf("fat32: Grow: %w (an interrupted grow is finished by the next Open)", err)
		}
	} else {
		// One write per boot sector, the backup first, exposes the new total.
		if err := fs.writeGeometry(uint32(newFAT), uint32(newTotal)); err != nil {
			return fmt.Errorf("fat32: Grow: write boot sector: %w", err)
		}
		fs.info.TotalSectors = uint32(newTotal)
	}
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
			if fat > 0xFFFF {
				return 0, 0, fmt.Errorf("fat32: Grow: a FAT of %d sectors does not fit BPB_FATSz16: %w", fat, ErrResizeFAT)
			}
			return fat, count, nil
		}
		fat = need
	}
	return 0, 0, fmt.Errorf("fat32: Grow: the FAT size did not converge for %d sectors: %w", newTotal, ErrResizeFAT)
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
