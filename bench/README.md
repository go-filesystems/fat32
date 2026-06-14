# Performance benchmarks

Two halves that measure the **same standard operations** so the pure-Go driver
can be read side by side with the in-kernel vfat/FAT32 implementation.

## Go-driver side (portable, runs anywhere)

```sh
go test -bench=. -benchmem -run='^$'
```

Benchmarks (in `../bench_test.go`, public-API only): `Format`, `WriteFileSeq`,
`ReadFileSeq`, `Stat`, `ListDir`, `CreateFiles`, `DeleteFiles`. File-backed
image under `b.TempDir()` so the numbers include real block I/O.

## Reference side (in-kernel vfat, Linux only, needs root)

```sh
scp bench/compare.sh dc1-r1-h1:/tmp/ && ssh dc1-r1-h1 'sudo bash /tmp/compare.sh'
```

`compare.sh` runs the same ops via `mkfs.vfat -F 32` + `mount -o loop` + `dd`
(with `fsync`/`drop_caches`) + coreutils.

> **Caveat — not apples-to-apples.** The kernel has a page cache and writeback;
> the Go driver does synchronous user-space block I/O. Treat the kernel numbers
> as a rough upper-bound reference, not a literal target.

## First findings (2026-06, Apple M4 Max, 64 MiB image, `-benchtime=3x`)

| Operation        | go-filesystems/fat32  |
|------------------|-----------------------|
| Sequential read  | ~1.06 GB/s            |
| Sequential write | ~4.7 MB/s             |
| Create file      | ~0.17 ms/file         |
| Delete file      | ~0.13 ms/file         |
| Format           | ~13.5 ms              |
| Stat             | ~3.4 ms               |
| ListDir (150)    | ~3.2 ms               |

**Reads are fast; the sequential write path is the clear bottleneck** (~4.7
MB/s for an 8 MiB file). FAT32 has no journal or block bitmap, so the cost is
dominated by per-write FAT/cluster-chain updates rather than batched dirty
blocks — an algorithmic write-path issue, identical across all target
architectures (no SIMD involved).

This is the top optimization target; profile with
`go test -bench=BenchmarkWriteFileSeq -cpuprofile=cpu.out -memprofile=mem.out`.
