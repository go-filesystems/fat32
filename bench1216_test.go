package filesystem_fat32_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
)

// The same 300000-byte multi-cluster file read from a FAT16 volume (the
// newfs_msdos/mtools fixture) and from a FAT32 volume this package formatted:
// the cost of the entry width and of the fixed root directory.
func benchPair(b *testing.B) (fat16, fat32fs filesystem.Filesystem, size int) {
	b.Helper()
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
	fat16, err = fat32.OpenReader(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		b.Fatal(err)
	}
	data, err := fat16.ReadFile("/BIG.BIN")
	if err != nil {
		b.Fatal(err)
	}
	fat32fs, err = fat32.Format(filepath.Join(b.TempDir(), "p.img"), 64<<20, fat32.FormatConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { fat32fs.Close() })
	if err := fat32fs.WriteFile("/BIG.BIN", data, 0o644); err != nil {
		b.Fatal(err)
	}
	return fat16, fat32fs, len(data)
}

func BenchmarkReadFileFATWidth(b *testing.B) {
	fat16, fat32fs, size := benchPair(b)
	for _, c := range []struct {
		name string
		fs   filesystem.Filesystem
	}{{"FAT16", fat16}, {"FAT32", fat32fs}} {
		b.Run(c.name+"/ReadFile", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				if got, err := c.fs.ReadFile("/BIG.BIN"); err != nil || len(got) != size {
					b.Fatal(err)
				}
			}
		})
		b.Run(c.name+"/OpenFileReadAt", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			buf := make([]byte, size)
			for i := 0; i < b.N; i++ {
				h, err := c.fs.(filesystem.Opener).OpenFile("/BIG.BIN")
				if err != nil {
					b.Fatal(err)
				}
				if n, err := h.ReadAt(buf, 0); n != size || (err != nil && err != io.EOF) {
					b.Fatal(n, err)
				}
				h.Close()
			}
		})
	}
}
