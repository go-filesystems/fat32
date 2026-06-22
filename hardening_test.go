package filesystem_fat32

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/go-volumes/safeio"
)

// These tests harden the FAT32 parser against malicious / corrupt images.
// The threat model: an untrusted image must NEVER panic the host, read out of
// bounds, integer-overflow into a bad allocation, loop forever, or OOM. Every
// vector below must turn into a graceful error.

// TestReadClusterChainCycle verifies that a forged cyclic FAT chain
// (cluster 3 → 4 → 3) terminates with a cycle error instead of looping
// forever and appending unboundedly (C1).
func TestReadClusterChainCycle(t *testing.T) {
	// README.TXT points at cluster 3; the FAT links 3→4→3, a cycle. FileSize
	// is large enough that the byte cap would not stop the walk first.
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "README  TXT", 0x20, 3, 1<<20)
		root[32] = 0x00
	}, map[uint32]uint32{3: 4, 4: 3}, nil)

	fs := openTestFS(t, path)
	defer fs.Close()

	done := make(chan struct{})
	var readErr error
	go func() {
		defer close(done)
		_, readErr = fs.ReadFile("/readme.txt")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ReadFile on a cyclic chain did not terminate (hang)")
	}

	if readErr == nil {
		t.Fatal("ReadFile(cyclic chain) error = nil, want cycle error")
	}
	if !errors.Is(readErr, safeio.ErrCycle) {
		t.Fatalf("ReadFile(cyclic chain) err = %v, want ErrCycle", readErr)
	}
}

// TestReadClusterChainSelfCycle covers the immediate self-loop 3→3.
func TestReadClusterChainSelfCycle(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "SELF    TXT", 0x20, 3, 1<<20)
		root[32] = 0x00
	}, map[uint32]uint32{3: 3}, nil)

	fs := openTestFS(t, path)
	defer fs.Close()

	if _, err := fs.ReadFile("/self.txt"); !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("ReadFile(self-cycle) err = %v, want ErrCycle", err)
	}
}

// TestReadClusterChainHugeFileSize verifies that a forged FileSize of
// 0xFFFFFFFF (~4 GB) on a file with a short real chain neither over-allocates
// (the requested size is clamped to the volume's real capacity) nor reads past
// the genuine end-of-chain (M3). The result must be the real cluster's data,
// not 4 GB of buffer.
func TestReadClusterChainHugeFileSize(t *testing.T) {
	const huge = uint32(0xFFFFFFFF)
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "BIG     TXT", 0x20, 3, huge)
		root[32] = 0x00
	}, map[uint32]uint32{3: 0x0FFFFFFF}, map[uint32][]byte{3: []byte("hi")})

	fs := openTestFS(t, path)
	defer fs.Close()

	data, err := fs.ReadFile("/big.txt")
	if err != nil {
		t.Fatalf("ReadFile(huge FileSize): %v", err)
	}
	// The chain is a single 4 KiB cluster, so the returned buffer is bounded by
	// the real cluster capacity, never the forged 4 GB FileSize.
	if got := len(data); got != int(fs.info.ClusterSize()) {
		t.Fatalf("ReadFile(huge FileSize) len = %d, want one cluster (%d)", got, fs.info.ClusterSize())
	}
	if !bytes.HasPrefix(data, []byte("hi")) {
		t.Fatalf("ReadFile(huge FileSize) prefix = %q, want %q", data[:2], "hi")
	}
}

// TestPartitionOffsetForgedEntrySize verifies that a GPT header advertising
// entrySize = 0xFFFFFFFF — the original ~4 GB allocation / offset-overflow
// vector — is rejected with a graceful error rather than panicking or OOMing
// (M2). It exercises the migrated go-volumes/gpt parser.
func TestPartitionOffsetForgedEntrySize(t *testing.T) {
	image := make([]byte, 8*sectorSize)
	writeGPTHeaderOnly(image, 2, 0xFFFFFFFF, 1)
	if _, err := partitionOffset(bytes.NewReader(image), int64(len(image)), -1); err == nil {
		t.Fatal("partitionOffset(entrySize=0xFFFFFFFF) error = nil, want error")
	}
}

// TestFreeChainCycle verifies that freeing a cyclic chain (3→4→3) terminates
// with a cycle error instead of spinning forever. freeChain runs when a file
// is deleted/overwritten, so it must tolerate a corrupt on-disk chain (C1).
func TestFreeChainCycle(t *testing.T) {
	path := fatTestImage(t, func(root []byte) {
		root[0] = 0x00
	}, map[uint32]uint32{3: 4, 4: 3}, nil)

	fs := openTestFS(t, path)
	defer fs.Close()

	if err := fs.freeChain(3); !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("freeChain(cyclic) err = %v, want ErrCycle", err)
	}
}

// withTinyChainGuard shrinks the FAT-chain iteration ceiling so that a short,
// acyclic-but-overlong chain trips the LoopGuard. This models a forged chain
// that walks distinct cluster numbers beyond the volume's real geometry — the
// VisitSet alone would not catch it (all clusters are distinct), so the
// LoopGuard is the backstop. Restored on cleanup.
func withTinyChainGuard(t *testing.T, limit int) {
	t.Helper()
	orig := chainGuardLimit
	t.Cleanup(func() { chainGuardLimit = orig })
	chainGuardLimit = func(*fat32FS) int { return limit }
}

// TestReadClusterChainLoopGuard verifies the LoopGuard backstop fires (with
// ErrLoopLimit) on an acyclic chain that exceeds the iteration budget.
func TestReadClusterChainLoopGuard(t *testing.T) {
	withTinyChainGuard(t, 2)
	// Acyclic chain 3→4→5→EOF; with a guard budget of 2 the third hop trips it.
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "LONG    TXT", 0x20, 3, 1<<20)
		root[32] = 0x00
	}, map[uint32]uint32{3: 4, 4: 5, 5: 0x0FFFFFFF}, nil)

	fs := openTestFS(t, path)
	defer fs.Close()

	if _, err := fs.ReadFile("/long.txt"); !errors.Is(err, safeio.ErrLoopLimit) {
		t.Fatalf("ReadFile(overlong) err = %v, want ErrLoopLimit", err)
	}
}

// TestReadClusterChainSizeCap verifies the read stops at the requested size
// even when the on-disk chain is longer (the FileSize-bounded early break).
func TestReadClusterChainSizeCap(t *testing.T) {
	// File claims 3 bytes but its chain spans two clusters; only the first
	// cluster (one clusterSize, then trimmed to 3) should be read.
	path := fatTestImage(t, func(root []byte) {
		writeFAT32ShortEntrySized(root[0:32], "SMALL   TXT", 0x20, 3, 3)
		root[32] = 0x00
	}, map[uint32]uint32{3: 4, 4: 0x0FFFFFFF},
		map[uint32][]byte{3: []byte("abcdef")})

	fs := openTestFS(t, path)
	defer fs.Close()

	data, err := fs.ReadFile("/small.txt")
	if err != nil {
		t.Fatalf("ReadFile(size cap): %v", err)
	}
	if string(data) != "abc" {
		t.Fatalf("ReadFile(size cap) = %q, want %q", data, "abc")
	}
}

// TestFreeChainLoopGuard verifies freeChain's LoopGuard backstop.
func TestFreeChainLoopGuard(t *testing.T) {
	withTinyChainGuard(t, 2)
	path := fatTestImage(t, func(root []byte) {
		root[0] = 0x00
	}, map[uint32]uint32{3: 4, 4: 5, 5: 0x0FFFFFFF}, nil)

	fs := openTestFS(t, path)
	defer fs.Close()

	if err := fs.freeChain(3); !errors.Is(err, safeio.ErrLoopLimit) {
		t.Fatalf("freeChain(overlong) err = %v, want ErrLoopLimit", err)
	}
}

// TestGrowChainCorruptExistingChain verifies that extending a file whose
// existing on-disk chain is corrupt terminates gracefully: a cyclic existing
// chain trips the VisitSet, and an acyclic-but-overlong one the LoopGuard.
func TestGrowChainCorruptExistingChain(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		// Existing chain 3→4→3 (cycle). growChain walks to the tail to link the
		// new run on and must reject the cycle.
		path := fatTestImage(t, func(root []byte) {
			root[0] = 0x00
		}, map[uint32]uint32{3: 4, 4: 3}, nil)
		fs := openTestFS(t, path)
		defer fs.Close()
		// oldClusters small so the slack-zeroing pre-walk does not run; need>0
		// so the link-to-end walk executes.
		if _, err := fs.growChain(3, 1, 2, 0); !errors.Is(err, safeio.ErrCycle) {
			t.Fatalf("growChain(cyclic existing) err = %v, want ErrCycle", err)
		}
	})

	t.Run("loop limit", func(t *testing.T) {
		withTinyChainGuard(t, 2)
		path := fatTestImage(t, func(root []byte) {
			root[0] = 0x00
		}, map[uint32]uint32{3: 4, 4: 5, 5: 6, 6: 0x0FFFFFFF}, nil)
		fs := openTestFS(t, path)
		defer fs.Close()
		if _, err := fs.growChain(3, 1, 2, 0); !errors.Is(err, safeio.ErrLoopLimit) {
			t.Fatalf("growChain(overlong existing) err = %v, want ErrLoopLimit", err)
		}
	})
}

// TestWriteDirBufCorruptChain verifies writeDirBuf rejects a corrupt directory
// chain: a cycle trips the VisitSet and an overlong run the LoopGuard, rather
// than writing forever.
func TestWriteDirBufCorruptChain(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		path := fatTestImage(t, func(root []byte) {
			root[0] = 0x00
		}, map[uint32]uint32{3: 4, 4: 3}, nil)
		fs := openTestFS(t, path)
		defer fs.Close()
		// A buffer spanning three clusters forces the walk to follow the cyclic
		// chain 3→4→3, revisiting cluster 3 on the third hop.
		buf := make([]byte, 3*int(fs.info.ClusterSize()))
		if err := fs.writeDirBuf(3, buf); !errors.Is(err, safeio.ErrCycle) {
			t.Fatalf("writeDirBuf(cyclic) err = %v, want ErrCycle", err)
		}
	})

	t.Run("loop limit", func(t *testing.T) {
		withTinyChainGuard(t, 1)
		// writeDirBuf's budget is chainGuardLimit + maxDirClusters (fresh
		// extension clusters also count as visits), so shrink maxDirClusters too
		// to keep the test image small.
		origMax := maxDirClusters
		t.Cleanup(func() { maxDirClusters = origMax })
		maxDirClusters = 1
		path := fatTestImage(t, func(root []byte) {
			root[0] = 0x00
		}, map[uint32]uint32{3: 4, 4: 5, 5: 6, 6: 0x0FFFFFFF}, nil)
		fs := openTestFS(t, path)
		defer fs.Close()
		buf := make([]byte, 5*int(fs.info.ClusterSize()))
		if err := fs.writeDirBuf(3, buf); !errors.Is(err, safeio.ErrLoopLimit) {
			t.Fatalf("writeDirBuf(overlong) err = %v, want ErrLoopLimit", err)
		}
	})
}

// TestOpenStatError covers the Open path where stat'ing the backing file fails
// (here: the openFile seam returns an already-closed handle), exercising the
// deviceFileSize error branch.
func TestOpenStatError(t *testing.T) {
	orig := openFile
	t.Cleanup(func() { openFile = orig })

	openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		f, err := orig(name, flag, perm)
		if err != nil {
			return nil, err
		}
		f.Close() // Stat on a closed *os.File fails.
		return f, nil
	}

	path := fatTestImage(t, func(root []byte) { root[0] = 0x00 }, nil, nil)
	if _, err := Open(path, -1); err == nil {
		t.Fatal("Open() error = nil, want stat error")
	}
}

// fuzzImageFromBytes lays the fuzz input over a valid FAT32 boot sector so the
// fuzzer reaches the parser's interior (FAT walks, directory iteration, cluster
// reads) rather than bouncing off readInfo's geometry validation immediately.
// The first 512 bytes are forced to a sane BPB; the attacker controls the FAT
// region, the data region, and the directory entries that follow.
func fuzzImageFromBytes(t *testing.T, data []byte) string {
	t.Helper()
	// Size the backing file to just cover the root-directory region (boot
	// sector + both FATs + a handful of data clusters). Reads past EOF simply
	// error, so we do not need to materialise the full 32 MiB the BPB declares —
	// keeping each fuzz exec cheap (no multi-MiB write per iteration).
	boot := defaultFAT32BootSector()
	info, err := readInfo(bytes.NewReader(boot), 0)
	if err != nil {
		t.Fatalf("readInfo: %v", err)
	}
	size := int(info.DataOffset(0)) + 8*int(info.ClusterSize())
	image := make([]byte, size)
	copy(image, boot)
	// Overlay the fuzz bytes after the boot sector. Keep the boot sector intact
	// (the overlay starts at sectorSize) so Open succeeds and the FAT / data /
	// directory read paths are exercised with attacker-controlled bytes.
	if len(data) > size-sectorSize {
		data = data[:size-sectorSize]
	}
	copy(image[sectorSize:], data)
	// Use a freshly created temp file removed at the end of THIS exec rather
	// than t.TempDir() (whose cleanup only runs when the whole test ends, so it
	// would accumulate one directory per fuzz iteration and eventually exhaust
	// the temp filesystem / inode table mid-run).
	f, err := os.CreateTemp("", "fat32fuzz-*.img")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	if _, err := f.Write(image); err != nil {
		f.Close()
		t.Fatalf("write image: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close image: %v", err)
	}
	return path
}

// FuzzOpenAndRead drives Open + a directory listing + a file read over a
// FAT32 image whose FAT/data/directory regions are attacker-controlled. The
// only contract is liveness and memory safety: NO panic, NO hang, NO OOM —
// every malformed structure must surface as an error. The f.Add seeds encode
// the exact regression vectors (cyclic FAT chain, FileSize=0xFFFFFFFF) and run
// under a plain `go test`.
func FuzzOpenAndRead(f *testing.F) {
	// Seed 1: a directory entry for a 5-byte file at cluster 3, chain 3→EOF.
	f.Add(seedValidFile())
	// Seed 2: cyclic FAT chain 3→4→3 with a file pointing at cluster 3.
	f.Add(seedCyclicChain())
	// Seed 3: FileSize = 0xFFFFFFFF on a single-cluster file.
	f.Add(seedHugeFileSize())

	f.Fuzz(func(t *testing.T, data []byte) {
		// The parser's chain walks and directory iteration are all bounded
		// (LoopGuard + VisitSet + fixed 32-byte directory advance), so a real
		// hang would manifest as the test binary's overall -timeout firing. The
		// fuzz contract here is memory safety: no panic, no OOB, no OOM, and a
		// graceful error for every malformed structure.
		path := fuzzImageFromBytes(t, data)
		defer os.Remove(path)
		fsIfc, err := Open(path, -1)
		if err != nil {
			return // a rejected image is a fine outcome
		}
		defer fsIfc.Close()
		entries, derr := fsIfc.ListDir("/")
		if derr != nil {
			return
		}
		for _, e := range entries {
			_, _ = fsIfc.ReadFile("/" + e.Name())
		}
	})
}

// seedValidFile builds the FAT+data overlay (everything after the boot sector)
// for a single 5-byte file "README.TXT" at cluster 3.
func seedValidFile() []byte {
	return buildOverlay(
		[]dirSeed{{name: "README  TXT", attr: 0x20, cluster: 3, size: 5}},
		map[uint32]uint32{3: 0x0FFFFFFF},
		map[uint32][]byte{3: []byte("hello")},
	)
}

// seedCyclicChain builds an overlay whose FAT links 3→4→3 (a cycle), with a
// file pointing at cluster 3 and a large FileSize.
func seedCyclicChain() []byte {
	return buildOverlay(
		[]dirSeed{{name: "LOOP    TXT", attr: 0x20, cluster: 3, size: 1 << 20}},
		map[uint32]uint32{3: 4, 4: 3},
		nil,
	)
}

// seedHugeFileSize builds an overlay with a single-cluster file whose FileSize
// is the forged 0xFFFFFFFF.
func seedHugeFileSize() []byte {
	return buildOverlay(
		[]dirSeed{{name: "HUGE    TXT", attr: 0x20, cluster: 3, size: 0xFFFFFFFF}},
		map[uint32]uint32{3: 0x0FFFFFFF},
		map[uint32][]byte{3: []byte("x")},
	)
}

type dirSeed struct {
	name    string
	attr    byte
	cluster uint32
	size    uint32
}

// buildOverlay renders the bytes that sit after the boot sector for a default
// FAT32 geometry: the FAT region (entries) and the data region (root directory
// + file clusters). It mirrors fatTestImage but produces a flat byte slice
// suitable as a fuzz seed.
func buildOverlay(dir []dirSeed, fat map[uint32]uint32, clusterData map[uint32][]byte) []byte {
	boot := defaultFAT32BootSector()
	info, err := readInfo(bytes.NewReader(boot), 0)
	if err != nil {
		panic(err)
	}
	// Overlay starts at sectorSize, so all absolute offsets are shifted left by
	// sectorSize when written into the slice.
	const base = sectorSize
	size := int(info.DataOffset(0)) + 32*int(info.ClusterSize()) - base
	overlay := make([]byte, size)

	put := func(absOff int, b []byte) {
		copy(overlay[absOff-base:], b)
	}

	fatBase := int(info.FATOffset(0))
	for _, c := range []uint32{0, 1, info.RootCluster} {
		var e [4]byte
		binary.LittleEndian.PutUint32(e[:], 0x0FFFFFFF)
		put(fatBase+int(c)*4, e[:])
	}
	for cluster, next := range fat {
		var e [4]byte
		binary.LittleEndian.PutUint32(e[:], next)
		put(fatBase+int(cluster)*4, e[:])
	}

	rootOff := int(info.RootDirOffset(0))
	for i, d := range dir {
		entry := make([]byte, 32)
		writeFAT32ShortEntrySized(entry, d.name, d.attr, d.cluster, d.size)
		put(rootOff+i*32, entry)
	}

	dataBase := int(info.DataOffset(0))
	cs := int(info.ClusterSize())
	for cluster, b := range clusterData {
		put(dataBase+int(cluster-2)*cs, b)
	}
	return overlay
}
