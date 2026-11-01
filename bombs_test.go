package filesystem_fat32

// Memory-bomb regression tests. Each hands the driver a few kilobytes of image
// that claim far more, and asserts the read is refused and that less than the
// stated budget was allocated doing it (runtime.MemStats TotalAlloc
// before/after).

import (
	"encoding/binary"
	"runtime"
	"testing"
)

func totalAlloc(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// A volume declaring 4096-byte sectors and 128 sectors per cluster has
// 512 KiB clusters; a 64 KiB image does not hold even one. The cluster buffer
// used to be made before the read could fail.
func TestBomb_ReadClusterChainDoesNotReserveAClusterTheImageLacks(t *testing.T) {
	info := Info{BytesPerSector: 4096, SectorsPerCluster: 128, ReservedSectors: 1, FATCount: 1,
		TotalSectors: 1 << 20, FATSize: 1, RootCluster: 2}
	image := make([]byte, 64<<10)
	binary.LittleEndian.PutUint32(image[int(info.FATOffset(0))+2*4:], 0x0FFFFFFF)
	fs := newMockFS(image, info, 0)
	var err error
	n := totalAlloc(func() { _, err = fs.readClusterChain(2, 1<<30) })
	if err == nil {
		t.Error("a 512 KiB cluster in a 64 KiB image was read")
	}
	if n >= 64<<10 {
		t.Errorf("allocated %d bytes before failing", n)
	}
}

// readInfo bounds the logical sector size to 512..4096 (Linux fat_read_bpb).
func TestBomb_SectorSizeCeilingIsExact(t *testing.T) {
	for _, tc := range []struct {
		bps uint16
		ok  bool
	}{{4096, true}, {8192, false}, {32768, false}} {
		boot := defaultFAT32BootSector()
		binary.LittleEndian.PutUint16(boot[11:], tc.bps)
		_, err := readInfo(newMockFS(boot, Info{}, 0).f, 0)
		if (err == nil) != tc.ok {
			t.Errorf("bytes per sector %d: err=%v, want ok=%v", tc.bps, err, tc.ok)
		}
	}
}

// The FAT size is a boot-sector claim; Resize reads the whole first FAT.
func TestBomb_RelocateFATCopiesDoesNotReserveAFATTheImageLacks(t *testing.T) {
	info := Info{BytesPerSector: 512, SectorsPerCluster: 1, ReservedSectors: 1, FATCount: 2,
		TotalSectors: 1 << 20, FATSize: 8, RootCluster: 2}
	fs := newMockFS(make([]byte, 64<<10), info, 0)
	var err error
	n := totalAlloc(func() { err = fs.relocateFATCopies(1<<28, 1<<28+8) })
	if err == nil {
		t.Error("a 128 GiB FAT was read from a 64 KiB image")
	}
	if n >= 1<<20 {
		t.Errorf("allocated %d bytes before failing", n)
	}
}
