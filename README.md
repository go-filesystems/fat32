# fat32

Pure-Go read/write access to FAT32 filesystem images — no root privileges, no external tools, no CGO.

Supports bare filesystem images and MBR/GPT partitioned disks, full directory traversal, file mutation and filesystem creation.

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Supports bare images and partitioned disks |
| Format | ✅ | Creates FAT32 images |
| ReadFile | ✅ | Full file reads supported |
| WriteFile | ✅ | Full file writes supported |
| MkDir / Delete / Rename | ✅ | Directory operations supported |
| ReadLink / Symlinks | ⚠️ No | FAT32 has no POSIX symlinks |
| Partitioned images | ✅ | MBR/GPT supported |

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
| WriteFile    | ✅ implemented |
| MkDir        | ✅ implemented |
| DeleteFile   | ✅ implemented |
| DeleteDir    | ✅ implemented (recursive) |
| Rename       | ✅ implemented |
| ReadLink     | ⚠️ stub — FAT32 has no symlinks |

## API

### Format

```go
type FormatConfig struct {
    Label    string
    VolumeID uint32 // 0 = randomly generated
}

func Format(path string, sizeBytes int64, cfg FormatConfig) (*FS, error)
```

### Open

```go
func Open(imagePath string, partIndex int) (*FS, error)
func (fs *FS) Close() error
func (fs *FS) Info() Info
func (fs *FS) PartitionOffset() int64
```

### Read

```go
func (fs *FS) Stat(path string) (filesystem.Stat, error)
func (fs *FS) ListDir(path string) ([]filesystem.DirEntry, error)
func (fs *FS) ReadFile(path string) ([]byte, error)
```

### Write

```go
func (fs *FS) WriteFile(path string, data []byte, perm os.FileMode) error
func (fs *FS) MkDir(path string, perm os.FileMode) error
func (fs *FS) DeleteFile(path string) error
func (fs *FS) DeleteDir(path string) error
func (fs *FS) Rename(oldPath, newPath string) error
```

## Implements

This package implements the `filesystem.Filesystem` interface defined in
`github.com/go-filesystems/interface`. Callers can treat an opened `*FS`
as a `filesystem.Filesystem` to write generic tooling that works across the
other filesystem modules in this repository.

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
