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

// BenchmarkWriteFileFATWidth writes (overwrites) the same 300000-byte file on a
// FAT16 volume (the fixture, as a file) and on a FAT32 volume.
func BenchmarkWriteFileFATWidth(b *testing.B) {
	f, err := os.Open("testdata/fat16.img.gz")
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
	p16 := filepath.Join(b.TempDir(), "w16.img")
	if err := os.WriteFile(p16, img, 0o600); err != nil {
		b.Fatal(err)
	}
	fat16, err := fat32.Open(p16, -1)
	if err != nil {
		b.Fatal(err)
	}
	defer fat16.Close()
	fat32fs, err := fat32.Format(filepath.Join(b.TempDir(), "w32.img"), 64<<20, fat32.FormatConfig{})
	if err != nil {
		b.Fatal(err)
	}
	defer fat32fs.Close()
	if err := fat32fs.MkDir("/MANY", 0o755); err != nil {
		b.Fatal(err)
	}
	data := make([]byte, 300000)
	for _, c := range []struct {
		name string
		fs   filesystem.Filesystem
	}{{"FAT16", fat16}, {"FAT32", fat32fs}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for i := 0; i < b.N; i++ {
				if err := c.fs.WriteFile("/MANY/BIGW.BIN", data, 0o644); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
