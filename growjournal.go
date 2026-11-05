// SPDX-License-Identifier: BSD-3-Clause

package filesystem_fat32

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// A crash-safe layout move for Grow.
//
// When a grow needs a bigger FAT, the FATs, the fixed root (FAT12/16) and the
// data region all move forward, and the new FAT0 overlaps the old FAT1 and the
// start of the old data. Until the boot sector is rewritten the old BPB
// describes a layout that is half overwritten, and no ordering of the writes can
// avoid that: the new layout needs bytes the old layout still owns. So the move
// is journaled and ROLLED FORWARD: Open finishes an interrupted grow before it
// serves the volume.
//
// Bibliography (opened 2026-11-05):
//   - e2fsprogs v1.47.2 resize/resize2fs.c resize_fs, lines 128-130 and 242:
//     resize2fs sets EXT2_ERROR_FS, flushes, moves everything, and clears the flag
//     last; an interrupted run is left to e2fsck, and its only undo is the
//     optional -z undo file (resize/main.c resize2fs_setup_tdb, line 171).
//     Neither is a record that the tool itself replays.
//   - GNU parted libparted/fs/r/fat/resize.c fat_resize (line 825): the new FAT
//     (fat_table_write_all) is written over the old one before the boot sector
//     (fat_boot_sector_write): the same window, no journal.
//   - dosfstools v4.2 src/boot.c, check_backup_boot / reserved-sector search
//     (lines 262-375): fsck.fat reads only the boot sector, the FSInfo sector and
//     the backup boot sector, so a record that lives elsewhere is invisible to it.
//   - Linux fs/fat/inode.c fat_fill_super reads the BPB and the FAT, nothing else.
//
// Where the record lives: the LAST SECTOR of the image. Grow extends the image
// first, so that sector is outside the old volume (fsck.fat and the kernel never
// look there), and Grow refuses a grow too small for the moved region to end
// before it, so it is also free in the new volume (it lies in an unallocated
// cluster or in the slack). It is a 512-byte-or-larger sector holding a magic,
// the old and new geometry, the number of bytes of the region still to move and a
// CRC-32, so that a data sector that merely resembles it is not taken for one.
//
// The protocol, every write a whole sector or a chunk:
//
//  1. extend the image (invisible), write the record, Sync;
//  2. move the region backwards from the end in chunks, Sync, then rewrite
//     the record with the bytes still to move. A chunk is moved before its
//     progress is recorded, so a crash re-moves at most that chunk, whose source
//     no later chunk has reached (a chunk's destination lies above its source and
//     later chunks lie below);
//  3. copy FAT0 over the other FAT copies and zero every FAT tail. FAT0's old
//     bytes are never overwritten, so this step can be redone from scratch;
//  4. write the new geometry in the backup boot sector, then the primary: one
//     write each, and the commit;
//  5. FAT32: recount the FSInfo free clusters; zero the record.
//
// Open (recoverGrow) replays whatever remains. A volume whose record is valid
// and whose BPB is the old geometry is finished; one whose BPB is already the new
// geometry only has steps 5 left; any other record is stale and ignored.
//
// What this does and does not give: with this library every interruption of
// Grow (an error, a kill, a crash) leaves an image that Open completes to the
// grown volume, or, when the record was never written, the untouched old
// volume. Between the first move and the commit, a tool that does not know the
// record (fsck_msdos, the kernel, Windows) sees the half-moved layout: it must
// be opened with this library first. The writes are ordered with Sync where the
// backing store has it; the sector write is assumed atomic, as for the boot
// sector commit.

const (
	growJournalMagic = "FATGRW01"
	// growJournalChunk is the data-move unit: one Sync pair per chunk.
	growJournalChunk = 1 << 20
	growJournalLen   = 44
)

// growBarrierMin is the smallest chunk that gets a Sync before and after its
// record (a variable so a test can lower it).
var growBarrierMin int64 = 256 << 10

// ErrGrowTooSmall is returned by Grow when the grow is too small to move the
// layout crash-safely: the record needs a free sector past the moved data.
var ErrGrowTooSmall = errors.New("fat32: grow too small to journal the layout move")

type growJournal struct {
	oldFAT, newFAT, oldTotal, newTotal uint32
	remaining, region                  uint64
}

func (j growJournal) encode(bps int) []byte {
	b := make([]byte, bps)
	copy(b, growJournalMagic)
	binary.LittleEndian.PutUint32(b[8:], j.oldFAT)
	binary.LittleEndian.PutUint32(b[12:], j.newFAT)
	binary.LittleEndian.PutUint32(b[16:], j.oldTotal)
	binary.LittleEndian.PutUint32(b[20:], j.newTotal)
	binary.LittleEndian.PutUint64(b[24:], j.remaining)
	binary.LittleEndian.PutUint64(b[32:], j.region)
	binary.LittleEndian.PutUint32(b[40:], crc32.ChecksumIEEE(b[:40]))
	return b
}

func decodeGrowJournal(b []byte) (growJournal, bool) {
	if len(b) < growJournalLen || !bytes.Equal(b[:8], []byte(growJournalMagic)) ||
		crc32.ChecksumIEEE(b[:40]) != binary.LittleEndian.Uint32(b[40:]) {
		return growJournal{}, false
	}
	return growJournal{
		oldFAT:    binary.LittleEndian.Uint32(b[8:]),
		newFAT:    binary.LittleEndian.Uint32(b[12:]),
		oldTotal:  binary.LittleEndian.Uint32(b[16:]),
		newTotal:  binary.LittleEndian.Uint32(b[20:]),
		remaining: binary.LittleEndian.Uint64(b[24:]),
		region:    binary.LittleEndian.Uint64(b[32:]),
	}, true
}

// movePlan is the layout move of one grow, from the old BPB and the new FAT size.
type movePlan struct {
	n                        int64 // FAT copies
	bps                      int64
	fatBase                  int64
	oldFATBytes, newFATBytes int64
	moveStart, delta, region int64
	journalOff               int64
	oldFAT, newFAT, newTotal int64
}

// dataClusterCount is the number of data clusters the BPB describes.
func (fs *fat32FS) dataClusterCount() int64 {
	if fs.info.fixedRoot() {
		return int64(fs.info.DataClusters)
	}
	sectors := int64(fs.info.TotalSectors) - int64(fs.info.ReservedSectors) -
		int64(fs.info.FATCount)*int64(fs.info.FATSize)
	if sectors <= 0 {
		return 0
	}
	return sectors / int64(fs.info.SectorsPerCluster)
}

// planMove computes the move for a new FAT of newFAT sectors and a volume of
// newTotal sectors in an image of size bytes, and refuses one whose destination
// would reach the journal sector or the end of the image.
func (fs *fat32FS) planMove(newFAT, newTotal, size int64) (movePlan, error) {
	info := fs.info
	p := movePlan{
		n:        int64(info.FATCount),
		bps:      int64(info.BytesPerSector),
		oldFAT:   int64(info.FATSize),
		newFAT:   newFAT,
		newTotal: newTotal,
	}
	p.fatBase = int64(info.ReservedSectors) * p.bps
	p.oldFATBytes = p.oldFAT * p.bps
	p.newFATBytes = newFAT * p.bps
	p.moveStart = p.fatBase + p.n*p.oldFATBytes
	p.delta = p.n * (p.newFATBytes - p.oldFATBytes)
	p.region = (int64(info.RootDirSectors()) +
		fs.dataClusterCount()*int64(info.SectorsPerCluster)) * p.bps
	p.journalOff = (size/p.bps - 1) * p.bps
	if newFAT <= p.oldFAT || p.delta <= 0 || p.journalOff < 0 ||
		p.moveStart+p.delta+p.region > p.journalOff ||
		p.fatBase+p.n*p.newFATBytes > p.moveStart+p.delta ||
		p.journalOff+p.bps > newTotal*p.bps {
		return movePlan{}, fmt.Errorf("%w: the moved layout must end %d bytes before the end of the image", ErrGrowTooSmall, p.bps)
	}
	return p, nil
}

// syncBacking flushes the backing store when it can be flushed.
func (fs *fat32FS) syncBacking() error {
	if s, ok := fs.f.(interface{ Sync() error }); ok {
		if err := s.Sync(); err != nil {
			return fmt.Errorf("sync: %w", err)
		}
	}
	return nil
}

// journaledGrow runs a layout move from the start: record, then the shared
// finish. The image is already extended to size.
func (fs *fat32FS) journaledGrow(newFAT, newTotal, size int64) error {
	p, err := fs.planMove(newFAT, newTotal, size)
	if err != nil {
		return err
	}
	j := growJournal{
		oldFAT: fs.info.FATSize, newFAT: uint32(newFAT),
		oldTotal: fs.info.TotalSectors, newTotal: uint32(newTotal),
		remaining: uint64(p.region), region: uint64(p.region),
	}
	if _, err := fs.f.WriteAt(j.encode(int(p.bps)), p.journalOff); err != nil {
		return fmt.Errorf("write journal at %d: %w", p.journalOff, err)
	}
	if err := fs.syncBacking(); err != nil {
		return err
	}
	return fs.finishGrow(p, j)
}

// finishGrow performs steps 2-5 of the protocol from the state j describes.
func (fs *fat32FS) finishGrow(p movePlan, j growJournal) error {
	buf := make([]byte, growJournalChunk)
	rem := int64(j.remaining)
	// A chunk no bigger than delta: its destination then lies wholly above its
	// own source, so a chunk whose write was torn can be redone from the source.
	chunk := min(int64(len(buf)), p.delta)
	// Barriers (Sync) around every chunk make the order hold across a power loss;
	// below 256 KiB a chunk costs more than the copy, so a small delta syncs only
	// at the phase boundaries: the order then holds for a crash of the process or
	// an I/O error, not for a power loss mid-move.
	barrier := chunk >= growBarrierMin
	for rem > 0 {
		c := min(chunk, rem)
		rem -= c
		b := buf[:c]
		if _, err := fs.f.ReadAt(b, p.moveStart+rem); err != nil {
			return fmt.Errorf("shift data region: read %d bytes at %d: %w", c, p.moveStart+rem, err)
		}
		if _, err := fs.f.WriteAt(b, p.moveStart+rem+p.delta); err != nil {
			return fmt.Errorf("shift data region: write %d bytes at %d: %w", c, p.moveStart+rem+p.delta, err)
		}
		if barrier {
			if err := fs.syncBacking(); err != nil {
				return err
			}
		}
		j.remaining = uint64(rem)
		if _, err := fs.f.WriteAt(j.encode(int(p.bps)), p.journalOff); err != nil {
			return fmt.Errorf("write journal at %d: %w", p.journalOff, err)
		}
		if barrier {
			if err := fs.syncBacking(); err != nil {
				return err
			}
		}
	}
	if err := fs.syncBacking(); err != nil {
		return err
	}
	if err := fs.rebuildFATCopies(p, buf); err != nil {
		return fmt.Errorf("rebuild FAT copies: %w", err)
	}
	if err := fs.syncBacking(); err != nil {
		return err
	}
	if err := fs.writeGeometry(uint32(p.newFAT), uint32(p.newTotal)); err != nil {
		return fmt.Errorf("write boot sector: %w", err)
	}
	fs.info.FATSize = uint32(p.newFAT)
	fs.info.TotalSectors = uint32(p.newTotal)
	return fs.finishCommitted(p)
}

// finishCommitted is step 5: what remains once the boot sector is the new one.
func (fs *fat32FS) finishCommitted(p movePlan) error {
	if err := fs.syncBacking(); err != nil {
		return err
	}
	if !fs.info.fixedRoot() {
		if err := fs.refreshFSInfo(); err != nil {
			return fmt.Errorf("refresh FSInfo: %w", err)
		}
	}
	if _, err := fs.f.WriteAt(make([]byte, p.bps), p.journalOff); err != nil {
		return fmt.Errorf("clear journal at %d: %w", p.journalOff, err)
	}
	return nil
}

// rebuildFATCopies copies FAT0's old content over every other copy at its new
// offset and zeroes every FAT tail. FAT0's old bytes [fatBase, fatBase+oldFAT)
// are never written, so a redo reads the same source.
func (fs *fat32FS) rebuildFATCopies(p movePlan, buf []byte) error {
	grown := p.newFATBytes - p.oldFATBytes
	for i := int64(1); i < p.n; i++ {
		dst := p.fatBase + i*p.newFATBytes
		for done := int64(0); done < p.oldFATBytes; {
			c := min(int64(len(buf)), p.oldFATBytes-done)
			b := buf[:c]
			if _, err := fs.f.ReadAt(b, p.fatBase+done); err != nil {
				return fmt.Errorf("read FAT0 at %d: %w", p.fatBase+done, err)
			}
			if _, err := fs.f.WriteAt(b, dst+done); err != nil {
				return fmt.Errorf("write FAT%d at %d: %w", i, dst+done, err)
			}
			done += c
		}
		if err := fs.zeroRange(dst+p.oldFATBytes, grown, buf); err != nil {
			return fmt.Errorf("zero FAT%d tail: %w", i, err)
		}
	}
	if err := fs.zeroRange(p.fatBase+p.oldFATBytes, grown, buf); err != nil {
		return fmt.Errorf("zero FAT0 tail: %w", err)
	}
	return nil
}

// zeroRange writes n zero bytes at off, in chunks of len(buf).
func (fs *fat32FS) zeroRange(off, n int64, buf []byte) error {
	clear(buf)
	for n > 0 {
		c := min(int64(len(buf)), n)
		if _, err := fs.f.WriteAt(buf[:c], off); err != nil {
			return fmt.Errorf("zero %d bytes at %d: %w", c, off, err)
		}
		off += c
		n -= c
	}
	return nil
}

// writeGeometry rewrites the FAT size and the total sector count in one write
// per boot sector: the backup first, the primary last, which is the commit.
func (fs *fat32FS) writeGeometry(fatSize, total uint32) error {
	offs := fs.bootSectorOffsets()
	for i := len(offs) - 1; i >= 0; i-- {
		buf := make([]byte, fs.info.BytesPerSector)
		if _, err := fs.f.ReadAt(buf, offs[i]); err != nil {
			return fmt.Errorf("read boot sector at %d: %w", offs[i], err)
		}
		if buf[510] != 0x55 || buf[511] != 0xAA {
			return fmt.Errorf("boot signature missing at %d", offs[i])
		}
		if fs.info.fixedRoot() {
			binary.LittleEndian.PutUint16(buf[22:24], uint16(fatSize))
			if total <= 0xFFFF {
				binary.LittleEndian.PutUint16(buf[19:21], uint16(total))
				binary.LittleEndian.PutUint32(buf[32:36], 0)
			} else {
				binary.LittleEndian.PutUint16(buf[19:21], 0)
				binary.LittleEndian.PutUint32(buf[32:36], total)
			}
		} else {
			binary.LittleEndian.PutUint16(buf[22:24], 0)
			binary.LittleEndian.PutUint32(buf[36:40], fatSize)
			binary.LittleEndian.PutUint16(buf[19:21], 0)
			binary.LittleEndian.PutUint32(buf[32:36], total)
		}
		if _, err := fs.f.WriteAt(buf, offs[i]); err != nil {
			return fmt.Errorf("write boot sector at %d: %w", offs[i], err)
		}
	}
	return nil
}

// recoverGrow finishes a grow that was interrupted. size is the image's size in
// bytes. It reads one sector at the end of the image; a missing, unreadable-as-EOF
// or foreign record is no record, and a record that is valid but for another
// geometry is stale.
func (fs *fat32FS) recoverGrow(size int64) error {
	bps := int64(fs.info.BytesPerSector)
	if fs.partOffset != 0 || bps <= 0 || size < 2*bps {
		return nil
	}
	off := (size/bps - 1) * bps
	b := make([]byte, bps)
	if _, err := fs.f.ReadAt(b, off); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("fat32: read grow journal at %d: %w", off, err)
	}
	j, ok := decodeGrowJournal(b)
	if !ok {
		return nil
	}
	switch {
	case j.oldFAT == fs.info.FATSize && j.oldTotal == fs.info.TotalSectors:
		// Interrupted before the commit: replay.
	case j.newFAT == fs.info.FATSize && j.newTotal == fs.info.TotalSectors:
		// Committed; the FSInfo count and the record are left.
		return fs.recoverCommitted(size)
	default:
		return nil
	}
	p, err := fs.planMove(int64(j.newFAT), int64(j.newTotal), size)
	if err != nil || j.region != uint64(p.region) || j.remaining > j.region ||
		p.newTotal*p.bps != size/p.bps*p.bps {
		return fmt.Errorf("fat32: interrupted grow: the journal does not fit this image (%v)", err)
	}
	if err := fs.finishGrow(p, j); err != nil {
		return fmt.Errorf("fat32: finishing an interrupted grow: %w", err)
	}
	return nil
}

// recoverCommitted completes step 5 for a volume whose boot sector is already
// the grown one.
func (fs *fat32FS) recoverCommitted(size int64) error {
	bps := int64(fs.info.BytesPerSector)
	if err := fs.finishCommitted(movePlan{bps: bps, journalOff: (size/bps - 1) * bps}); err != nil {
		return fmt.Errorf("fat32: finishing an interrupted grow: %w", err)
	}
	return nil
}
