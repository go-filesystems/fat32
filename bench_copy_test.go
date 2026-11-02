package filesystem_fat32_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	fat32 "github.com/go-filesystems/fat32"
)

// BenchmarkCopyResized copies the FAT12 and FAT16 fixtures (a tree with long
// names, nested directories and a 300000-byte file) into a new 16 MiB image.
// Compare allocs/op; see BENCHMARKS.md for the mtools reference.
func BenchmarkCopyResized(b *testing.B) {
	for _, name := range []string{"fat12", "fat16"} {
		f, err := os.Open("testdata/" + name + ".img.gz")
		if err != nil {
			b.Fatal(err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			b.Fatal(err)
		}
		img, err := io.ReadAll(zr)
		f.Close()
		if err != nil {
			b.Fatal(err)
		}
		src, err := fat32.OpenReader(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			b.Fatal(err)
		}
		dst, err := os.Create(filepath.Join(b.TempDir(), "dst.img"))
		if err != nil {
			b.Fatal(err)
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				out, err := fat32.CopyResized(dst, 16<<20, src, fat32.CopyConfig{VolumeID: 1})
				if err != nil {
					b.Fatal(err)
				}
				out.Close()
			}
		})
		dst.Close()
		src.Close()
	}
}
