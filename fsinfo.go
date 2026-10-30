package filesystem_fat32

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
)

// FSInfo maintenance.
//
// Microsoft's FAT specification (fatgen103, section 5) calls the FSInfo
// structure advisory: FSI_Free_Count "contains the last known free cluster
// count", 0xFFFFFFFF means "not known", the value "must be validated at volume
// mount (and subsequently maintained in memory by the file system driver)", and
// it "is recommended that this field contain an accurate count of the number of
// free clusters at volume dismount". fsck tools (dosfstools update_free,
// macOS fsck_msdos) report a volume whose count is 0xFFFFFFFF or wrong.
//
// So the driver never trusts the value stored in an image: the first time a FAT
// entry is about to change it counts the free entries once (one sequential pass
// over the FAT, never per allocation), then keeps the count exact from the
// delta of every setFATEntry. The count and the FSI_Nxt_Free hint are written
// back to the sector when a directory write completes and on Close; nothing
// ever reads them to decide where to allocate, so a forged FSInfo cannot steer
// an allocation.

// fsInfoState is the in-memory FSInfo cache of one open volume. The zero value
// is "not loaded", which is what a read-only open and a freshly built fat32FS
// both are.
type fsInfoState struct {
	mu     sync.Mutex   // serialises the first scan and the sector write
	loaded atomic.Bool  // free/next hold a count taken from the FAT
	dirty  atomic.Bool  // the sector is behind the cache
	free   atomic.Int64 // free data clusters
	next   atomic.Uint32
	off    int64 // absolute offset of the FSInfo sector; 0 = none or invalid
}

const fsInfoUnknown = 0xFFFFFFFF

// dataClusters is the number of data clusters the volume really has: bounded
// by the sectors after the FATs and by what one FAT can describe.
func (fs *fat32FS) dataClusters() int64 {
	spc := int64(fs.info.SectorsPerCluster)
	if spc == 0 {
		return 0
	}
	data := int64(fs.info.TotalSectors) - int64(fs.info.ReservedSectors) -
		int64(fs.info.FATCount)*int64(fs.info.FATSize)
	n := data / spc
	if fatEntries := int64(fs.info.FATSize)*int64(fs.info.BytesPerSector)/4 - 2; fatEntries < n {
		n = fatEntries
	}
	if n < 0 {
		return 0
	}
	return n
}

// fsInfoSectorOffset returns the absolute offset of a well-formed FSInfo
// sector, or 0 when the volume has none (FSInfoSector 0, outside the reserved
// area, or missing signatures).
func (fs *fat32FS) fsInfoSectorOffset() int64 {
	s := int64(fs.info.FSInfoSector)
	if s == 0 || s >= int64(fs.info.ReservedSectors) || fs.info.BytesPerSector < 512 {
		return 0
	}
	off := fs.partOffset + s*int64(fs.info.BytesPerSector)
	var sig [4]byte
	le := binary.LittleEndian
	if _, err := fs.f.ReadAt(sig[:], off); err != nil || le.Uint32(sig[:]) != 0x41615252 {
		return 0
	}
	if _, err := fs.f.ReadAt(sig[:], off+484); err != nil || le.Uint32(sig[:]) != 0x61417272 {
		return 0
	}
	if _, err := fs.f.ReadAt(sig[:], off+508); err != nil || le.Uint32(sig[:]) != 0xAA550000 {
		return 0
	}
	return off
}

// scanFreeCount counts the free data clusters with one pass over the first
// FAT, reading it in large blocks, and returns the count and the lowest free
// cluster (0 when none).
func (fs *fat32FS) scanFreeCount() (free int64, first uint32, err error) {
	n := fs.dataClusters()
	fatBase := fs.info.FATOffset(fs.partOffset)
	const block = 16384 // entries per read
	buf := make([]byte, block*4)
	for c := int64(2); c < n+2; {
		want := min(int64(block), n+2-c)
		b := buf[:want*4]
		if _, err := fs.f.ReadAt(b, fatBase+c*4); err != nil {
			return 0, 0, err
		}
		for i := int64(0); i < want; i++ {
			if binary.LittleEndian.Uint32(b[i*4:])&0x0FFFFFFF == 0 {
				if first == 0 {
					first = uint32(c + i)
				}
				free++
			}
		}
		c += want
	}
	return free, first, nil
}

// ensureFreeCount loads the cache on first use. It is best effort: a volume
// without a usable FSInfo, or a FAT that cannot be read, simply stays
// unloaded and nothing is written.
func (fs *fat32FS) ensureFreeCount() {
	st := &fs.fsi
	if st.loaded.Load() {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.loaded.Load() {
		return
	}
	off := fs.fsInfoSectorOffset()
	if off == 0 {
		return
	}
	free, first, err := fs.scanFreeCount()
	if err != nil {
		return
	}
	st.off = off
	st.free.Store(free)
	st.next.Store(first)
	st.dirty.Store(true)
	st.loaded.Store(true)
}

// invalidateFreeCount drops the cache after a FAT write failed half-way, so
// the next change recounts instead of writing a count that may be off.
func (fs *fat32FS) invalidateFreeCount() {
	fs.fsi.loaded.Store(false)
}

// noteFATChange applies one FAT entry transition to the cache.
func (fs *fat32FS) noteFATChange(cluster, old, value uint32) {
	st := &fs.fsi
	if !st.loaded.Load() {
		return
	}
	wasFree := old&0x0FFFFFFF == 0
	isFree := value&0x0FFFFFFF == 0
	switch {
	case isFree && !wasFree:
		st.free.Add(1)
		if cur := st.next.Load(); cur == 0 || cluster < cur {
			st.next.Store(cluster)
		}
	case !isFree && wasFree:
		st.free.Add(-1)
		if st.next.Load() == cluster {
			st.next.Store(cluster + 1)
		}
	default:
		return
	}
	st.dirty.Store(true)
}

// flushFSInfo writes the cached free count and next-free hint into the FSInfo
// sector if they changed. Values that do not fit the volume are written as
// 0xFFFFFFFF ("unknown"), never as a number a reader would act on.
func (fs *fat32FS) flushFSInfo() error {
	st := &fs.fsi
	if !st.loaded.Load() || !st.dirty.Load() {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.dirty.Swap(false) {
		return nil
	}
	n := fs.dataClusters()
	var rec [8]byte
	free, next := uint32(fsInfoUnknown), uint32(fsInfoUnknown)
	if f := st.free.Load(); f >= 0 && f <= n {
		free = uint32(f)
	}
	if x := int64(st.next.Load()); x >= 2 && x < n+2 {
		next = uint32(x)
	}
	binary.LittleEndian.PutUint32(rec[0:], free)
	binary.LittleEndian.PutUint32(rec[4:], next)
	if _, err := fs.f.WriteAt(rec[:], st.off+488); err != nil {
		st.dirty.Store(true)
		return err
	}
	return nil
}
