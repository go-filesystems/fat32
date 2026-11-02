package filesystem_fat32

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkGrowBiggerFAT times the in-place grow that needs a bigger FAT
// against the alternative a caller had before v0.10.0, CopyResized onto a new
// image of the same size, interleaved round by round on one core (run it with
// GOMAXPROCS=1 and never under a 1-minute load above 8). It reports ns/grow and
// ns/copy; the benchmark's own ns/op is the pair.
func BenchmarkGrowBiggerFAT(b *testing.B) {
	for _, typ := range []int{12, 16} {
		size := map[int]int64{12: 2 << 20, 16: 16 << 20}[typ]
		b.Run(map[int]string{12: "FAT12", 16: "FAT16"}[typ], func(b *testing.B) {
			_, img := f1216Fixture(b, typ)
			dir := b.TempDir()
			var grow, cp time.Duration
			for i := 0; i < b.N; i++ {
				p := filepath.Join(dir, "g.img")
				if err := os.WriteFile(p, img, 0o600); err != nil {
					b.Fatal(err)
				}
				fs, err := Open(p, -1)
				if err != nil {
					b.Fatal(err)
				}
				t0 := time.Now()
				if err := fs.(*fat32FS).Grow(size); err != nil {
					b.Fatal(err)
				}
				grow += time.Since(t0)
				fs.Close()

				src, err := OpenReader(&rwBuf{img},int64(len(img)))
				if err != nil {
					b.Fatal(err)
				}
				dst, err := os.Create(filepath.Join(dir, "c.img"))
				if err != nil {
					b.Fatal(err)
				}
				t0 = time.Now()
				out, err := CopyResized(dst, size, src, CopyConfig{VolumeID: 1})
				if err != nil {
					b.Fatal(err)
				}
				cp += time.Since(t0)
				out.Close()
				dst.Close()
				src.Close()
			}
			b.ReportMetric(float64(grow.Nanoseconds())/float64(b.N), "ns/grow")
			b.ReportMetric(float64(cp.Nanoseconds())/float64(b.N), "ns/copy")
		})
	}
}
