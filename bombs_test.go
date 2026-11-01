package filesystem_fat32

// Memory-bomb regression tests. Each hands the driver a few kilobytes of image
// that claim far more, and asserts the read is refused and that less than the
// stated budget was allocated doing it (runtime.MemStats TotalAlloc
// before/after).

import (
	"bytes"
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

// lfnRun returns a directory buffer of n consecutive long-name entries with no
// short entry to end the run, each carrying 13 characters.
func lfnRun(n int) []byte {
	buf := make([]byte, n*32)
	for i := 0; i < n; i++ {
		e := buf[i*32 : (i+1)*32]
		e[0] = byte(i%20 + 1)
		e[11] = fatAttrLongName
		for _, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
			e[off] = 'a'
		}
	}
	return buf
}

// A long name is at most 255 characters (20 entries). The parsers prepended
// each entry's characters to everything gathered so far, so a run of k
// long-name entries copied O(k^2) characters: 8192 entries (a 256 KiB
// directory) allocated about 872 MB, and a 2 MiB directory about 55 GB.
func TestBomb_LongNameRunIsLinear(t *testing.T) {
	buf := lfnRun(8192)
	n := totalAlloc(func() { _ = parseRootDirMetadata(buf) })
	if n >= 1<<20 {
		t.Errorf("parseRootDirMetadata allocated %d bytes for 8192 long-name entries", n)
	}
	n = totalAlloc(func() { _, _, _ = fat32FindEntry(buf, "nothing") })
	if n >= 1<<20 {
		t.Errorf("fat32FindEntry allocated %d bytes for 8192 long-name entries", n)
	}
}

// lfnDir builds a directory buffer holding a file whose long name is n copies
// of 'a', as the pieces a formatter writes (last piece first), then the short
// entry SHORT.TXT.
func lfnDir(n int) []byte {
	pieces := (n + 12) / 13
	var buf []byte
	for k := pieces - 1; k >= 0; k-- {
		e := make([]byte, 32)
		e[0] = byte(k + 1)
		e[11] = fatAttrLongName
		for j, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
			switch idx := k*13 + j; {
			case idx < n:
				e[off] = 'a'
			case idx == n:
				// terminator 0x0000, then padding
			default:
				e[off], e[off+1] = 0xFF, 0xFF
			}
		}
		buf = append(buf, e...)
	}
	short := make([]byte, 32)
	copy(short, "SHORT   TXT")
	short[11] = 0x20
	return append(buf, short...)
}

// The bound on a long name is exact: 255 characters is a name, 256 is a run
// the parsers refuse to gather, and the file keeps its short name.
func TestBomb_LongNameCeilingIsExact(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{255, string(bytes.Repeat([]byte("a"), 255))}, {256, "SHORT.TXT"}} {
		got := parseRootDirMetadata(lfnDir(tc.n))
		if len(got) != 1 || got[0].name != tc.want {
			t.Errorf("%d characters: entries=%v, want one named %q", tc.n, got, tc.want)
		}
		if _, _, found := fat32FindEntry(lfnDir(tc.n), tc.want); !found {
			t.Errorf("%d characters: fat32FindEntry did not find %q", tc.n, tc.want)
		}
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
