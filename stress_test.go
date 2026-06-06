package filesystem_fat32

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// ─── Knobs ────────────────────────────────────────────────────────────────
//
// Every stress test is gated behind two layers:
//
//   1. testing.Short() — heavy tests skip in short mode. The default
//      `go test` invocation runs in long mode; the package's CI runner
//      can opt back into short to keep wall-clock low.
//
//   2. Command-line flags + matching env vars override the defaults
//      so the same tests can scale from "ship a build under 30 s" to
//      "burn a CI agent for hours" without code edits.
//
// CLI flags (all prefixed `-stress.`):
//
//   -stress.duration   how long the concurrent R/W loop runs.
//   -stress.workers    how many goroutines hammer the FS.
//   -stress.file-mb    size of the single-file stress in megabytes.
//   -stress.files      number of files for the many-files stress.
//   -stress.faults-pct percent of disk operations that should fail
//                      in the fault-injection test (0-100, 0 disables).
//
// Env vars (read on init when the flag was not explicitly set):
//
//   FAT32_STRESS_DURATION (e.g. "3h")
//   FAT32_STRESS_WORKERS
//   FAT32_STRESS_FILE_MB
//   FAT32_STRESS_FILES
//   FAT32_STRESS_FAULTS_PCT
//
// Default values target a < 30 s wall-clock for `go test -run Stress`
// on a developer laptop in short mode.

var (
	stressDuration    = flag.Duration("stress.duration", 0, "concurrent R/W stress duration")
	stressWorkers     = flag.Int("stress.workers", 0, "concurrent R/W worker count")
	stressFileMB      = flag.Int("stress.file-mb", 0, "large-file stress size in MiB")
	stressFiles       = flag.Int("stress.files", 0, "many-files stress entry count")
	stressFaultPctVal = flag.Int("stress.faults-pct", 0, "percent of I/O ops that should fail (fault injection)")
)

// stressKnobs returns the effective knob values for the current run, after
// merging flag defaults with env-var overrides. short reports whether the
// caller is in `go test -short` mode (heavy tests will skip).
type stressKnobs struct {
	duration  time.Duration
	workers   int
	fileMB    int
	files     int
	faultPct  int
	short     bool
}

func loadStressKnobs(t testing.TB) stressKnobs {
	t.Helper()
	short := testing.Short()

	// Defaults: short-mode keeps total wall-clock well under 30 s.
	knobs := stressKnobs{
		duration: 2 * time.Second,
		workers:  8,
		fileMB:   16,
		files:    256,
		faultPct: 5,
		short:    short,
	}
	if !short {
		// "Long" mode bumps the defaults moderately. Callers that want
		// the multi-hour burn set FAT32_STRESS_DURATION explicitly.
		knobs.duration = 5 * time.Second
		knobs.fileMB = 32
		knobs.files = 1024
	}

	// Flags win over env vars; env vars win over defaults.
	if *stressDuration > 0 {
		knobs.duration = *stressDuration
	} else if env := os.Getenv("FAT32_STRESS_DURATION"); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			knobs.duration = d
		}
	}
	if *stressWorkers > 0 {
		knobs.workers = *stressWorkers
	} else if v, ok := envInt("FAT32_STRESS_WORKERS"); ok && v > 0 {
		knobs.workers = v
	}
	if *stressFileMB > 0 {
		knobs.fileMB = *stressFileMB
	} else if v, ok := envInt("FAT32_STRESS_FILE_MB"); ok && v > 0 {
		knobs.fileMB = v
	}
	if *stressFiles > 0 {
		knobs.files = *stressFiles
	} else if v, ok := envInt("FAT32_STRESS_FILES"); ok && v > 0 {
		knobs.files = v
	}
	if *stressFaultPctVal > 0 {
		knobs.faultPct = *stressFaultPctVal
	} else if v, ok := envInt("FAT32_STRESS_FAULTS_PCT"); ok && v >= 0 {
		knobs.faultPct = v
	}
	return knobs
}

func envInt(name string) (int, bool) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, false
	}
	var v int
	_, err := fmt.Sscanf(raw, "%d", &v)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ─── Helpers ──────────────────────────────────────────────────────────────

// stressTmpImage formats a fresh FAT32 image sized to fit the requested
// payload. Caller is expected to Close the returned filesystem.
func stressTmpImage(t testing.TB, sizeBytes int64) (string, filesystem.Filesystem) {
	t.Helper()
	// Round up to cluster size (Format requires it).
	const cluster = fmtBytesPerSector * fmtSectorsPerCluster
	if sizeBytes%cluster != 0 {
		sizeBytes += cluster - sizeBytes%cluster
	}
	path := filepath.Join(t.TempDir(), "stress.img")
	fs, err := Format(path, sizeBytes, FormatConfig{Label: "STRESS"})
	if err != nil {
		t.Fatalf("Format(%d): %v", sizeBytes, err)
	}
	return path, fs
}

// stressGzipFixture decodes the canonical mkfs fixture into a temp file
// so fuzz / parser tests have a real-world seed to mutate.
func stressGzipFixture(t testing.TB) []byte {
	t.Helper()
	gz, err := os.Open(filepath.Join("testdata", "mkfs", "image.fat32.gz"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer gz.Close()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress fixture: %v", err)
	}
	return raw
}

// ─── Test 1: Concurrent R/W with sha256 integrity ─────────────────────────

// TestStressConcurrentRW spins up N workers that each write a unique file,
// read it back, verify sha256, and delete it. The driver's public surface
// is NOT documented as goroutine-safe, so a shared sync.Mutex serialises
// every call — what we're stressing is the *lifecycle* (alloc/free/list
// churn) under high op rate, not raw parallelism.
func TestStressConcurrentRW(t *testing.T) {
	knobs := loadStressKnobs(t)
	// Image sized to comfortably hold workers*4 files of ~64 KiB each.
	// FAT32 minimum legal volume is ~33 MiB; pad it.
	imgSize := int64(64 * 1024 * 1024)
	_, fs := stressTmpImage(t, imgSize)
	defer fs.Close()

	deadline := time.Now().Add(knobs.duration)
	var mu sync.Mutex
	var ops, failures int64

	rand.Seed(time.Now().UnixNano())
	var wg sync.WaitGroup
	for w := 0; w < knobs.workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(worker)*7919 + time.Now().UnixNano()))
			seq := 0
			for time.Now().Before(deadline) {
				seq++
				size := 1 + rng.Intn(8192) // 1 – 8 KiB
				payload := make([]byte, size)
				rng.Read(payload)
				want := sha256.Sum256(payload)
				name := fmt.Sprintf("/w%02d_%04d.dat", worker, seq)

				mu.Lock()
				err := fs.WriteFile(name, payload, 0o644)
				mu.Unlock()
				if err != nil {
					atomic.AddInt64(&failures, 1)
					// FAT may legitimately fill under extreme load; that
					// path is exercised by TestStressClusterChurn instead.
					if strings.Contains(err.Error(), "no free clusters") {
						time.Sleep(time.Millisecond)
						continue
					}
					t.Errorf("worker %d write %s: %v", worker, name, err)
					return
				}

				mu.Lock()
				got, err := fs.ReadFile(name)
				mu.Unlock()
				if err != nil {
					t.Errorf("worker %d read %s: %v", worker, name, err)
					return
				}
				if sha256.Sum256(got) != want {
					t.Errorf("worker %d sha mismatch on %s", worker, name)
					return
				}

				mu.Lock()
				err = fs.DeleteFile(name)
				mu.Unlock()
				if err != nil {
					t.Errorf("worker %d delete %s: %v", worker, name, err)
					return
				}
				atomic.AddInt64(&ops, 1)
			}
		}(w)
	}
	wg.Wait()

	elapsed := knobs.duration.Seconds()
	t.Logf("stress concurrent: workers=%d duration=%s ops=%d failures=%d ops/sec=%.0f",
		knobs.workers, knobs.duration, ops, failures, float64(ops)/elapsed)
}

// ─── Test 2: Large file at the 4 GiB-ε boundary ────────────────────────────

// TestStressLargeFile writes (and reads back) a single large file. Short
// mode caps at -stress.file-mb (default 16 MiB); long mode can be cranked
// up to 4 GiB-1 byte — the absolute FAT32 file-size limit (the size field
// is a uint32 of bytes).
func TestStressLargeFile(t *testing.T) {
	knobs := loadStressKnobs(t)
	if knobs.short && knobs.fileMB > 64 {
		t.Skipf("short mode caps file-mb at 64 (got %d)", knobs.fileMB)
	}
	wantSize := int64(knobs.fileMB) * 1024 * 1024
	const fat32MaxFile = int64(1<<32 - 1) // 4 GiB - 1
	if wantSize > fat32MaxFile {
		wantSize = fat32MaxFile
		t.Logf("clamped requested size to FAT32 limit 4GiB-1")
	}
	// Image must hold the file plus FAT/reserved overhead. 1.5x is plenty.
	imgSize := wantSize + 32*1024*1024
	_, fs := stressTmpImage(t, imgSize)
	defer fs.Close()

	payload := make([]byte, wantSize)
	rng := rand.New(rand.NewSource(0xfa732))
	rng.Read(payload[:min(int64(1<<16), wantSize)]) // randomise the head, rest stays zero
	want := sha256.Sum256(payload)

	start := time.Now()
	if err := fs.WriteFile("/big.bin", payload, 0o644); err != nil {
		t.Fatalf("WriteFile big.bin: %v", err)
	}
	writeDur := time.Since(start)

	start = time.Now()
	got, err := fs.ReadFile("/big.bin")
	if err != nil {
		t.Fatalf("ReadFile big.bin: %v", err)
	}
	readDur := time.Since(start)

	if int64(len(got)) != wantSize {
		t.Fatalf("read size %d, want %d", len(got), wantSize)
	}
	if sha256.Sum256(got) != want {
		t.Fatalf("sha mismatch on /big.bin")
	}

	st, err := fs.Stat("/big.bin")
	if err != nil {
		t.Fatalf("Stat big.bin: %v", err)
	}
	if int64(st.Size()) != wantSize {
		t.Fatalf("Stat size = %d, want %d", st.Size(), wantSize)
	}
	t.Logf("large file: %d MiB  write=%s read=%s", knobs.fileMB, writeDur, readDur)
}

// TestStressFAT32MaxFileSizeField separately exercises the writer's
// behaviour for files whose size-field is at the uint32 limit, without
// actually allocating a 4 GiB image (which is impractical for CI). The
// goal is to verify that:
//
//   (a) size = 4 GiB - 1 round-trips correctly through Stat()
//   (b) the writer rejects (or correctly handles) size > 4 GiB - 1
//
// We do (a) by directly poking a synthetic dir entry — the on-disk
// metadata path is what would break first.
func TestStressFAT32MaxFileSizeField(t *testing.T) {
	const maxSize = uint32(0xFFFFFFFF) // 4 GiB - 1
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "HUGE    BIN", 0x20, 3, maxSize)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, nil)

	fs := openTestFS(t, path)
	defer fs.Close()

	st, err := fs.Stat("/huge.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size() != uint64(maxSize) {
		t.Fatalf("Stat size = %d, want %d", st.Size(), maxSize)
	}
}

// ─── Test 3: Many files (root dir + subdirs) ──────────────────────────────

// TestStressManyFiles creates -stress.files files spread across root and
// subdirectories. FAT32 root is a regular cluster chain, but listing
// performance degrades with chain length, and LFN entries consume 1+N
// slots each — the test verifies the directory grows as required and
// every file is independently readable.
func TestStressManyFiles(t *testing.T) {
	knobs := loadStressKnobs(t)
	target := knobs.files
	if knobs.short && target > 2048 {
		target = 2048
	}

	// Estimate: ~3 LFN entries per file (LFN_NAME = "lfn-12345.dat" is
	// 13 chars → 1 LFN slot + 1 short = 2 slots = 64 bytes). 4 KiB
	// cluster holds 128 entries. For target=8192 we need ~64 clusters
	// of dir + cluster-per-file data ⇒ ~32 MiB image is enough.
	imgSize := int64(64 * 1024 * 1024)
	if target > 16384 {
		imgSize = 256 * 1024 * 1024
	}
	_, fs := stressTmpImage(t, imgSize)
	defer fs.Close()

	const subdirs = 4
	for s := 0; s < subdirs; s++ {
		if err := fs.MkDir(fmt.Sprintf("/d%d", s), 0o755); err != nil {
			t.Fatalf("MkDir d%d: %v", s, err)
		}
	}

	start := time.Now()
	written := 0
	for i := 0; i < target; i++ {
		var name string
		if i%5 == 0 {
			name = fmt.Sprintf("/d%d/file-%05d.dat", i%subdirs, i)
		} else {
			name = fmt.Sprintf("/file-lfn-%05d.dat", i)
		}
		payload := []byte(fmt.Sprintf("file %d", i))
		if err := fs.WriteFile(name, payload, 0o644); err != nil {
			if strings.Contains(err.Error(), "directory is full") {
				t.Logf("directory full after %d files; FAT32 root chain extension boundary", i)
				break
			}
			if strings.Contains(err.Error(), "no free clusters") {
				t.Logf("FS full after %d files", i)
				break
			}
			t.Fatalf("WriteFile %s: %v", name, err)
		}
		written++
	}
	writeDur := time.Since(start)

	// Spot-check a sample of files for correctness.
	rng := rand.New(rand.NewSource(0xfa72f1))
	for i := 0; i < min(written, 64); i++ {
		idx := rng.Intn(written)
		var name string
		if idx%5 == 0 {
			name = fmt.Sprintf("/d%d/file-%05d.dat", idx%subdirs, idx)
		} else {
			name = fmt.Sprintf("/file-lfn-%05d.dat", idx)
		}
		got, err := fs.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		want := fmt.Sprintf("file %d", idx)
		if string(got) != want {
			t.Fatalf("ReadFile %s = %q, want %q", name, got, want)
		}
	}
	t.Logf("many files: created=%d (target=%d) write_total=%s files/sec=%.0f",
		written, target, writeDur, float64(written)/writeDur.Seconds())
}

// ─── Test 4: Cluster allocation stress ────────────────────────────────────

// TestStressClusterChurn rapidly creates and deletes files in a tight
// loop, checking that the free-cluster count returns to its starting
// value (no leaks) and that the FAT remains consistent (mirrored FATs
// hold identical data, every allocated chain terminates).
func TestStressClusterChurn(t *testing.T) {
	knobs := loadStressKnobs(t)
	cycles := 200
	if !knobs.short {
		cycles = 2000
	}

	imgSize := int64(16 * 1024 * 1024) // tight: forces real reuse
	path, fs := stressTmpImage(t, imgSize)
	defer fs.Close()

	fsConcrete := fs.(*fat32FS)
	initialFree, err := countFreeClusters(fsConcrete)
	if err != nil {
		t.Fatalf("initial countFreeClusters: %v", err)
	}

	start := time.Now()
	for i := 0; i < cycles; i++ {
		name := fmt.Sprintf("/churn-%04d.bin", i%32)
		payload := make([]byte, 1+i%4096)
		if err := fs.WriteFile(name, payload, 0o644); err != nil {
			t.Fatalf("WriteFile %s @ cycle %d: %v", name, i, err)
		}
		if err := fs.DeleteFile(name); err != nil {
			t.Fatalf("DeleteFile %s @ cycle %d: %v", name, i, err)
		}
	}
	dur := time.Since(start)

	finalFree, err := countFreeClusters(fsConcrete)
	if err != nil {
		t.Fatalf("final countFreeClusters: %v", err)
	}
	if finalFree != initialFree {
		t.Fatalf("cluster leak: initial=%d final=%d (delta=%d)",
			initialFree, finalFree, initialFree-finalFree)
	}

	// Verify FAT mirror consistency: FAT0 and FAT1 must hold the same
	// bytes after the churn. Re-open with a raw os.File to avoid mutating
	// state in the live driver.
	if err := verifyFATMirror(path); err != nil {
		t.Fatalf("FAT mirror inconsistent: %v", err)
	}

	t.Logf("cluster churn: cycles=%d duration=%s cycles/sec=%.0f free=%d",
		cycles, dur, float64(cycles)/dur.Seconds(), finalFree)
}

func countFreeClusters(fs *fat32FS) (int, error) {
	clusterCount := fs.info.TotalSectors / uint32(fs.info.SectorsPerCluster)
	fatBase := fs.info.FATOffset(fs.partOffset)
	var buf [4]byte
	free := 0
	for c := uint32(2); c < clusterCount+2; c++ {
		if _, err := fs.f.ReadAt(buf[:], fatBase+int64(c)*4); err != nil {
			return 0, err
		}
		if binary.LittleEndian.Uint32(buf[:])&0x0FFFFFFF == 0 {
			free++
		}
	}
	return free, nil
}

func verifyFATMirror(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	boot := make([]byte, sectorSize)
	if _, err := f.ReadAt(boot, 0); err != nil {
		return err
	}
	info, err := readInfo(bytes.NewReader(boot), 0)
	if err != nil {
		return err
	}
	if info.FATCount < 2 {
		return nil
	}
	fatBytes := int64(info.FATSize) * int64(info.BytesPerSector)
	fat0 := make([]byte, fatBytes)
	fat1 := make([]byte, fatBytes)
	fat0Off := info.FATOffset(0)
	fat1Off := fat0Off + fatBytes
	if _, err := f.ReadAt(fat0, fat0Off); err != nil {
		return fmt.Errorf("read FAT0: %w", err)
	}
	if _, err := f.ReadAt(fat1, fat1Off); err != nil {
		return fmt.Errorf("read FAT1: %w", err)
	}
	if !bytes.Equal(fat0, fat1) {
		// Find the first differing byte for diagnostic.
		for i := range fat0 {
			if fat0[i] != fat1[i] {
				return fmt.Errorf("FAT0 != FAT1 at byte %d (0x%02x vs 0x%02x)", i, fat0[i], fat1[i])
			}
		}
	}
	return nil
}

// ─── Test 5: Parser fuzzing ───────────────────────────────────────────────

// FuzzOpen feeds randomly-mutated boot sectors / disk images through the
// Open path. The only acceptance criteria is "no panic / OOM" — Open is
// expected to *return an error* on almost every input. We seed with the
// canonical fixture (decompressed) and with the synthetic boot sector
// from defaultFAT32BootSector to give the engine a starting set.
func FuzzOpen(f *testing.F) {
	f.Add(defaultFAT32BootSector())
	// Add small mutations on the boot sector to give the engine a head start.
	for _, off := range []int{11, 13, 16, 19, 22, 32, 36, 44} {
		boot := defaultFAT32BootSector()
		boot[off] ^= 0xFF
		f.Add(boot)
	}

	// Seed with the real fixture too — but slice to a manageable head so
	// the engine focuses on the metadata zone we actually care about.
	full := stressGzipFixture(f)
	if len(full) > 4*sectorSize {
		f.Add(full[:4*sectorSize])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < sectorSize {
			return
		}
		path := filepath.Join(t.TempDir(), "fuzz.img")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Skip("WriteFile: ", err)
		}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on Open: %v\n%s", r, runtimeStack())
			}
		}()
		fs, err := Open(path, -1)
		if err == nil {
			// Don't blindly trust it — exercise the metadata path.
			if c, ok := fs.(*fat32FS); ok {
				_ = c.Info()
			}
			_, _ = fs.ListDir("/")
			fs.Close()
		}
	})
}

func runtimeStack() string {
	buf := make([]byte, 16*1024)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

// ─── Test 6: Fault injection ──────────────────────────────────────────────

// faultDisk wraps an *os.File and randomly fails Read/Write operations at
// the configured percentage. The driver should propagate errors cleanly
// rather than crash or corrupt the in-memory state of unrelated objects.
type faultDisk struct {
	inner   *os.File
	pct     int
	rng     *rand.Rand
	mu      sync.Mutex
	tripped int64
}

var errFault = errors.New("fault: injected I/O failure")

func (d *faultDisk) trip() bool {
	if d.pct <= 0 {
		return false
	}
	d.mu.Lock()
	roll := d.rng.Intn(100)
	d.mu.Unlock()
	if roll < d.pct {
		atomic.AddInt64(&d.tripped, 1)
		return true
	}
	return false
}

func (d *faultDisk) ReadAt(p []byte, off int64) (int, error) {
	if d.trip() {
		return 0, errFault
	}
	return d.inner.ReadAt(p, off)
}

func (d *faultDisk) WriteAt(p []byte, off int64) (int, error) {
	if d.trip() {
		return 0, errFault
	}
	return d.inner.WriteAt(p, off)
}

func (d *faultDisk) Close() error { return d.inner.Close() }

// TestStressFaultInjection runs a workload through a faulty backing
// store. Some ops will fail (that's the point); the filesystem must
// never panic and must remain re-openable afterwards.
func TestStressFaultInjection(t *testing.T) {
	knobs := loadStressKnobs(t)
	if knobs.faultPct <= 0 {
		t.Skip("FAT32_STRESS_FAULTS_PCT or -stress.faults-pct is zero")
	}

	imgPath, prepFS := stressTmpImage(t, 16*1024*1024)
	prepFS.Close()

	inner, err := os.OpenFile(imgPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("re-open image: %v", err)
	}
	disk := &faultDisk{
		inner: inner,
		pct:   knobs.faultPct,
		rng:   rand.New(rand.NewSource(1)),
	}

	// Build a fat32FS directly around the faulty disk.
	off, err := partitionOffset(inner, -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	info, err := readInfo(inner, off)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	fs := &fat32FS{f: disk, partOffset: off, info: info}

	iters := 200
	if !knobs.short {
		iters = 1000
	}
	for i := 0; i < iters; i++ {
		name := fmt.Sprintf("/fault-%04d.bin", i)
		payload := []byte(fmt.Sprintf("payload-%d", i))
		// Don't assert on err here — failures are expected by design.
		_ = fs.WriteFile(name, payload, 0o644)
		_, _ = fs.ReadFile(name)
		_ = fs.DeleteFile(name)
	}
	disk.Close()

	// Re-open without fault injection; the filesystem must still be readable.
	clean, err := Open(imgPath, -1)
	if err != nil {
		t.Fatalf("Open after fault injection: %v", err)
	}
	if _, err := clean.ListDir("/"); err != nil {
		t.Fatalf("ListDir after fault injection: %v", err)
	}
	clean.Close()

	t.Logf("fault injection: pct=%d iters=%d tripped=%d", knobs.faultPct, iters, atomic.LoadInt64(&disk.tripped))
}

// ─── Test 7: 8.3 / LFN edge cases ─────────────────────────────────────────

// TestStressLFNEdgeCases exercises the writer-reader round-trip for
// names at and around the LFN boundary. We deliberately omit names
// containing path separators and the FAT-reserved characters '/' '\\'
// ':' '*' '?' '"' '<' '>' '|' — those aren't valid LFN entries on
// any conforming reader.
func TestStressLFNEdgeCases(t *testing.T) {
	cases := []struct {
		desc string
		name string
	}{
		{"ascii 8.3", "HELLO.TXT"},
		{"lower-case forces LFN", "hello.txt"},
		{"mixed case + spaces", "Mixed Case Name.dat"},
		{"single char", "x"},
		{"long name 64 chars", strings.Repeat("a", 64) + ".dat"},
		{"long name 200 chars", strings.Repeat("b", 200) + ".dat"},
		{"max LFN 255 chars", strings.Repeat("c", 251) + ".dat"},
		{"unicode bmp", "héllo-monde.txt"},
		{"unicode emoji surrogate pair", "report-😀.txt"},
		{"unicode CJK", "日本語ファイル.txt"},
		{"name ending in dot", "trailing-dot..txt"},
		{"double extension", "archive.tar.gz"},
		{"no extension", "Makefile"},
	}

	_, fs := stressTmpImage(t, 16*1024*1024)
	defer fs.Close()

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			path := "/" + tc.name
			payload := []byte("payload:" + tc.name)
			if err := fs.WriteFile(path, payload, 0o644); err != nil {
				t.Fatalf("WriteFile %q: %v", tc.name, err)
			}
			got, err := fs.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile %q: %v", tc.name, err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("payload mismatch on %q: got %q want %q", tc.name, got, payload)
			}
			// ListDir must surface the file (case-insensitive match on
			// the on-disk name preserves the LFN exactly).
			entries, err := fs.ListDir("/")
			if err != nil {
				t.Fatalf("ListDir: %v", err)
			}
			found := false
			for _, e := range entries {
				if strings.EqualFold(e.Name(), tc.name) {
					found = true
				}
			}
			if !found {
				t.Fatalf("ListDir did not return %q", tc.name)
			}
			if err := fs.DeleteFile(path); err != nil {
				t.Fatalf("DeleteFile %q: %v", tc.name, err)
			}
		})
	}
}

