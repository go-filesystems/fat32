package filesystem_fat32_test

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	fat32 "github.com/go-filesystems/fat32"
)

type resizer interface {
	Grow(int64) error
	Shrink(int64) error
}

// BenchmarkResizeFATWidth shrinks a volume by 100 clusters and grows it back,
// on a FAT16 volume (the newfs_msdos fixture, as a file) and on a FAT32 volume
// this package formatted. Each sub-benchmark times one direction; the other is
// done with the timer stopped (allocations are not counted then either).
// Compare allocs/op; timings under load are not comparable.
func BenchmarkResizeFATWidth(b *testing.B) {
	dir := b.TempDir()
	f, err := os.Open("testdata/fat16.img.gz")
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		b.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		b.Fatal(err)
	}
	p16 := filepath.Join(dir, "fat16.img")
	if err := os.WriteFile(p16, img, 0o600); err != nil {
		b.Fatal(err)
	}
	fs16, err := fat32.Open(p16, -1)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { fs16.Close() })
	const size32 = int64(64 << 20)
	fs32, err := fat32.Format(filepath.Join(dir, "fat32.img"), size32, fat32.FormatConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { fs32.Close() })

	for _, c := range []struct {
		name string
		fs   resizer
		full int64
		cut  int64
	}{
		{"FAT16", fs16.(resizer), int64(len(img)), 200 * 512}, // 100 clusters of 2 sectors
		{"FAT32", fs32.(resizer), size32, 100 * 4096},         // 100 clusters of 8 sectors
	} {
		b.Run(c.name+"/Shrink", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := c.fs.Shrink(c.full - c.cut); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := c.fs.Grow(c.full); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
		b.Run(c.name+"/Grow", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				if err := c.fs.Shrink(c.full - c.cut); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := c.fs.Grow(c.full); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
