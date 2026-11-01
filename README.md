<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-fat32.png" alt="go-filesystems/fat32" width="720"></p>

# fat32

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/fat32.svg)](https://pkg.go.dev/github.com/go-filesystems/fat32)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/fat32/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/fat32/actions/workflows/ci.yml)

Pure-Go read/write access to FAT32 filesystem images — no root privileges, no external tools, no CGO.

Supports bare filesystem images and MBR/GPT partitioned disks, full directory traversal, file mutation and filesystem creation.

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Supports bare images and partitioned disks |
| Format | ✅ | Creates FAT32 images |
| ReadFile | ✅ | Full file reads supported |
| Read at an offset | ✅ | `OpenFile(path)` → `io.ReaderAt` + `Size()` (`filesystem.Opener` / `filesystem.File`) |
| Write at an offset | ✅ | `OpenFile(path)` → `io.WriterAt` + `Truncate` + `Sync` (`filesystem.WritableFile`) |
| WriteFile | ✅ | Full file writes supported |
| MkDir / Delete / Rename | ✅ | Directory operations supported |
| ReadLink / Symlinks | ⚠️ No | FAT32 has no POSIX symlinks |
| Volume label | ✅ | `Label` / `SetLabel` (`filesystem.Labeller`) |
| Truncate | ✅ | `Truncate(path, newSize)` (`filesystem.Truncater`) |
| Grow / Shrink / Resize | ✅ | `filesystem.Resizer` — `Resize` dispatches to `Grow` or `Shrink` |
| Partitioned images | ✅ | MBR/GPT supported |
| FAT12 / FAT16 (read) | ✅ | `Open` / `OpenReader` read what `newfs_msdos -F 12\|16` and `mkfs.vfat` make below ~33 MiB, including `OpenFile` + `ReadAt`; see [FAT12 and FAT16](#fat12-and-fat16) |
| FAT12 / FAT16 (write) | ⚠️ No | every mutation (`WriteFile`, `MkDir`, `Delete*`, `Rename`, `Truncate`, `SetLabel`, `Resize`, `File.WriteAt`) returns `ErrReadOnlyFAT` |
| fsck-clean images | ✅ | `fsck_msdos -n` (macOS) and `fsck.vfat -n` (dosfstools) exit 0 on what the driver writes: `..` of a directory under the root holds cluster 0, and the FSInfo free count and next-free hint are exact at format and kept current (written after each directory write and on `Close`; counted once per open, never per allocation, and never trusted from the image) |

## FAT12 and FAT16

One driver reads all three variants (`Info.Type` is `FAT12`, `FAT16` or
`FAT32`). The rules, from Microsoft's FAT specification (fatgen103) and checked
against Linux `fs/fat/inode.c`:

- a boot sector with `BPB_FATSz16 == 0` has the FAT32 layout (Linux does the
  same); otherwise the variant is decided by the **count of data clusters**
  alone, never by the `FAT12`/`FAT16` label: below 4085 is FAT12, below 65525
  is FAT16, and a FAT12/16 layout claiming more is refused;
- the root directory of FAT12/16 is a fixed region of `BPB_RootEntCnt` entries
  right after the FATs, not a cluster chain (`Info.RootCluster` is 0,
  `Info.RootDirOffset` is the region); `..` of a first-level directory holds
  cluster 0;
- FAT12 entries are 12 bits packed (`offset = N + N/2`, odd `N` the high 12
  bits), and one may straddle a sector boundary; end of chain is `>= 0xFF8` /
  `>= 0xFFF8`, bad cluster `0xFF7` / `0xFFF7`;
- all geometry is computed in 64 bits and checked against the volume, so a
  forged `BytsPerSec`, `NumFATs`, `RootEntCnt` or sector count is an error on
  a 32-bit host too.

**FAT12/16 volumes are read-only in this release**: writing returns
`ErrReadOnlyFAT` and leaves the image untouched. Write support is tracked as a
follow-up. The test images in `testdata/` were formatted by `newfs_msdos` and
populated by mtools (`testdata/mkfixtures`), and `FuzzOpenFAT1216` drives the
read surface.

## Untrusted input

An image is attacker-controlled data. Every allocation a value in the image can
size is bounded before it happens, and a hostile image gets an error, not an
out-of-memory kill:

- a cluster buffer is allocated only after the cluster's last byte has been
  read, so a small file declaring 512 KiB clusters costs nothing to probe;
- the logical sector size is 512..4096 (Linux `fat_read_bpb`); the field is a
  `uint16`, and 32768-byte sectors made every cluster buffer 4 MiB;
- a directory is at most 65536 entries of 32 bytes, 2 MiB (Linux
  `FAT_MAX_DIR_SIZE`), however large the cluster;
- a long file name is at most 255 characters (20 entries of 13); a longer run
  of long-name entries is not gathered and the file keeps its short name. The
  parsers used to prepend each entry to everything gathered so far, so a
  2 MiB directory of long-name entries copied about 55 GB;
- `Resize` reads the first FAT only after proving the whole FAT is in the
  image (a 128 GiB FAT claim allocated 137 GB before this).

`FuzzOpenAndRead` drives the read surface; the regression tests are in
`bombs_test.go`.

## Limitations

- FAT32 has no POSIX symlinks, permissions, or ACLs.
- Filename charset and legacy constraints (8.3 compatibility concerns) may apply in some tooling contexts.
- Intended for test and tooling scenarios; not recommended as a production POSIX filesystem.

## Module

```text
github.com/go-filesystems/fat32
```

## Supported operations

| Operation    | Status         |
|--------------|----------------|
| Open / Close | ✅ implemented |
| Format       | ✅ implemented |
| Stat         | ✅ implemented |
| ListDir      | ✅ implemented |
| ReadFile     | ✅ implemented |
| OpenFile     | ✅ implemented (`filesystem.Opener`) |
| WriteAt      | ✅ implemented (`filesystem.WritableFile`, on the `File`) |
| WriteFile    | ✅ implemented |
| MkDir        | ✅ implemented |
| DeleteFile   | ✅ implemented |
| DeleteDir    | ✅ implemented (recursive) |
| Rename       | ✅ implemented |
| ReadLink     | ⚠️ stub — FAT32 has no symlinks |

## API

`Open` and `Format` return the plain `filesystem.Filesystem` interface (there
is no richer exported `FS` type) — FAT32-specific extras (`Info`,
`PartitionOffset`, volume label, truncate, resize) are reached by
type-asserting to the optional interfaces from
`github.com/go-filesystems/interface`, or to the package's own `Info` getter.

### Format / Open

```go
type FormatConfig struct {
    Label    string
    VolumeID uint32 // 0 = randomly generated
}

func Format(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error)
func Open(imagePath string, partIndex int) (filesystem.Filesystem, error)

// Format without a host file: an image in memory, a WebAssembly page.
// rw must already hold sizeBytes; the metadata regions are zeroed first,
// so a reused buffer is safe.
func FormatImage(rw ReaderWriterAt, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error)
func OpenReader(r io.ReaderAt, size int64) (filesystem.Filesystem, error)
```

### Read / Write

The returned value's `Stat` / `ListDir` / `ReadFile` / `WriteFile` / `MkDir` /
`DeleteFile` / `DeleteDir` / `Rename` are exactly the
[`filesystem.Filesystem`](https://github.com/go-filesystems/interface)
contract — see that package's README for the full signatures.

### FAT32-specific extras (type-assert)

```go
// Read part of a file without materialising all of it: OpenFile resolves the
// FAT chain once (cluster numbers, not data) and then serves byte ranges, so a
// 4 KiB read out of a 4 GiB file costs 4 KiB. Required by anything that mounts
// or exports the volume.
if o, ok := fs.(filesystem.Opener); ok {
    f, err := o.OpenFile("/big.bin")
    if err != nil {
        return err
    }
    defer f.Close()
    buf := make([]byte, 4096)
    n, err := f.ReadAt(buf, 1<<30) // io.ReaderAt semantics, exactly
    _, _ = n, err
    _ = f.Size()                   // from metadata; reads nothing

    // ...and write part of one, in place. Without this a positional write can
    // only be ReadFile + splice + WriteFile: the whole file read, reallocated
    // and written back on every request, so writing a file in blocks is
    // quadratic. Measured over a real NFS mount, 2 MiB in 64 KiB blocks took
    // 23 s (90 kB/s) that way.
    if w, ok := f.(filesystem.WritableFile); ok {
        _, err = w.WriteAt(buf, 1<<30) // io.WriterAt semantics, exactly
        _ = w.Truncate(1 << 20)        // grow zero-fills; shrink frees clusters
        _ = w.Sync()                   // fsync(2) on a file-backed image
    }
}
if l, ok := fs.(filesystem.Labeller); ok {
    _ = l.SetLabel("MYVOL")
}
if t, ok := fs.(filesystem.Truncater); ok {
    _ = t.Truncate("/big.bin", 4096)
}
if r, ok := fs.(filesystem.Resizer); ok {
    _ = r.Resize(newSizeBytes) // dispatches to the package's Grow/Shrink
}
if i, ok := fs.(interface{ Info() Info }); ok {
    fmt.Println(i.Info().ClusterSize())
}
```

## Implements

This package implements the `filesystem.Filesystem` interface defined in
`github.com/go-filesystems/interface`. Callers can treat the value returned
by `Open`/`Format` as a `filesystem.Filesystem` to write generic tooling that
works across the other filesystem modules in this repository.

Example:

```go
import (
    filesystem "github.com/go-filesystems/interface"
    fsfat "github.com/go-filesystems/fat32"
)

f, _ := fsfat.Open("fat32.img", -1)
defer f.Close()
var fs filesystem.Filesystem = f
_, _ = fs.ListDir("/")
```
