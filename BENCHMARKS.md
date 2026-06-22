# Performance parity — go-filesystems/fat32 vs mkfs.vfat / kernel vfat  (2026-06-22)

## Methodology

- **Where**: the `debian` Tart VM (linux/arm64) on an Apple-silicon (M4) host.
  Our pure-Go driver and the reference C tools run in the same VM, same kernel,
  same hardware. Reads are **cold** (caches dropped before every iteration).
- **CPU / kernel**: 4 vCPU aarch64, Linux 6.12.74 (Debian 13).
- **Go**: 1.26.4 linux/arm64, CGO disabled.
- **Reference tools**: dosfstools 4.2 (`mkfs.vfat -F 32`, `fsck.vfat`), in-tree
  kernel vfat.
- **Image set**: 2008 files — 2000 small (1–4 KiB) + 8 large (4 MiB) ≈ 38 MB in
  a 111 MiB image.
- **Sampling**: best-of-5; format and read timed separately; read cold;
  throughput on the ~38 MB payload.
- **Format**: ours `fat32.Format(path, size, cfg)` vs `truncate` + `mkfs.vfat`.
- **Read**: image created+populated by `mkfs.vfat` + loop-mount + `cp -a`, then
  read by ours and the kernel (`mount -o loop` + `tar`). No userspace FAT reader
  shipped by default → no peer column.
- **Correctness gate (verified)**: our extraction returns exactly 2008 files
  byte-for-byte; **our `fat32.Format` output loop-mounts and is `fsck.vfat -n`
  clean.**

## Results

| op | size | ours (MB/s, wall) | reference (MB/s, wall) | ratio | verdict |
|----|------|-------------------|------------------------|-------|---------|
| Format | 111 MiB | — , **0.031 ms** | mkfs.vfat: — , 13.83 ms | **0.002×** | ours 440× faster† |
| Read (cold) | 38 MB | 311 MB/s, 118.4 ms | kernel: 1184 MB/s, 31.1 ms | 3.81× | ours 3.8× slower |

† See caveat below.

## Summary

- **Format: nominally 440× faster, but not like-for-like.** FAT32 format is
  intrinsically cheap (boot sector + FSInfo + two zeroed FAT tables + root
  cluster). Our `Format` writes only those structures sparsely; `mkfs.vfat`
  additionally zeroes reserved areas and does extra validation. Our output is
  `fsck.vfat`-clean and mountable, so it's a valid, very fast provisioning path.
- **Read: we lag the kernel 3.8×.** No userspace peer ships for FAT, so the
  kernel is the only reference; the kernel number also includes loop+mount+tar
  overhead, so the *pure-parse* gap is somewhat smaller than 3.8×.

### Root causes (read)

1. **Cluster chain walk is one `ReadAt` per cluster** (no coalescing of
   contiguous chains). For the 4 MiB files this is the dominant cost.
2. **FAT table re-read** during chain following instead of caching the active
   FAT in memory.
3. Per-file buffer allocation → GC pressure across 2008 files.

### Action items

- [ ] Cache the active FAT (one read of the table, then in-memory chain walk).
- [ ] Coalesce contiguous cluster runs into a single `ReadAt`.
- [ ] Pool read buffers.
- [ ] Optional parallel extract.

## Reproduce

```sh
sudo ./benchmarks/run.sh fat32 <repo_dir> <work_dir> 5
```

`benchmarks/run.sh` is shared across the go-filesystems drivers;
`benchmarks/bench.go` is the fat32 harness. Standalone `main` package, excluded
from the coverage gate.
