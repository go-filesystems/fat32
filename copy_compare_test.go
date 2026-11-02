package filesystem_fat32

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestCopyResizedVersusMtools is the reference comparison, run only with
// FAT32_COMPARE=1 (it measures time, so it is not part of the normal run).
//
// Ours: CopyResized of the FAT16 fixture into a new 16 MiB image.
// Reference: newfs_msdos -F 16 -C 16m, then `mcopy -s` of the same tree from a
// host directory into it. The extraction of the tree out of the source image
// (`mcopy -s -i src ::/ dir`) is done once, outside the timing, which favours
// the reference. Single core (GOMAXPROCS is left alone: both are one process
// doing sequential I/O), rounds interleaved, medians reported.
func TestCopyResizedVersusMtools(t *testing.T) {
	if os.Getenv("FAT32_COMPARE") != "1" {
		t.Skip("set FAT32_COMPARE=1 to run the timing comparison")
	}
	newfs, err1 := exec.LookPath("newfs_msdos")
	mcopy, err2 := exec.LookPath("mcopy")
	if err1 != nil || err2 != nil {
		t.Skip("newfs_msdos or mcopy not installed")
	}
	srcPath, fs := f1216Writable(t, 16)
	fs.Close()
	tree := filepath.Join(t.TempDir(), "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(mcopy, "-s", "-n", "-i", srcPath, "::/", tree).CombinedOutput(); err != nil {
		t.Fatalf("mcopy extract: %v\n%s", err, out)
	}
	top, err := os.ReadDir(tree)
	if err != nil {
		t.Fatal(err)
	}
	src := openCopySource(t, srcPath)
	defer src.Close()
	dir := t.TempDir()

	const rounds = 21
	var ours, ref []time.Duration
	for i := 0; i < rounds; i++ {
		p := filepath.Join(dir, "ours.img")
		os.Remove(p)
		f, _ := os.Create(p)
		start := time.Now()
		out, err := CopyResized(f, 16<<20, src, CopyConfig{})
		d := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		out.Close()
		f.Close()
		ours = append(ours, d)

		q := filepath.Join(dir, "ref.img")
		os.Remove(q)
		start = time.Now()
		qf, _ := os.Create(q)
		qf.Truncate(16 << 20)
		qf.Close()
		if o, err := exec.Command(newfs, "-F", "16", "-s", "32768", q).CombinedOutput(); err != nil {
			t.Fatalf("newfs_msdos: %v\n%s", err, o)
		}
		args := []string{"-s", "-n", "-i", q}
		for _, e := range top {
			args = append(args, filepath.Join(tree, e.Name()))
		}
		args = append(args, "::/")
		if o, err := exec.Command(mcopy, args...).CombinedOutput(); err != nil {
			t.Fatalf("mcopy: %v\n%s", err, o)
		}
		ref = append(ref, time.Since(start))
	}
	med := func(d []time.Duration) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2]
	}
	t.Logf("median of %d interleaved rounds: CopyResized %v, newfs_msdos + mcopy -s %v", rounds, med(ours), med(ref))
}
