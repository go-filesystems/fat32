package filesystem_fat32

import (
	"encoding/binary"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

var _ filesystem.MetadataSetter = (*fat32FS)(nil)

// ErrNotSupported is what Chmod and Chown return: FAT records no owner and no
// POSIX mode bits (only the read-only, hidden, system and archive attributes),
// so there is nothing to store. It wraps errors.ErrUnsupported.
var ErrNotSupported = fmt.Errorf("fat32: the format has no POSIX mode bits or owners: %w", errors.ErrUnsupported)

// Chmod is part of filesystem.MetadataSetter, which the SMB, NFS and SFTP
// servers probe for as a whole before they use Chtimes. FAT has no mode bits to
// replace, so it always returns ErrNotSupported and writes nothing.
func (fs *fat32FS) Chmod(path string, perm os.FileMode) error { return ErrNotSupported }

// Chown returns ErrNotSupported: FAT records no owner. See Chmod.
func (fs *fat32FS) Chown(path string, uid, gid uint32) error { return ErrNotSupported }

// ErrTimeOutOfRange is what Chtimes returns, wrapped in a *TimeRangeError, for
// a time the FAT date word cannot hold. FAT has a 7-bit year counted from 1980,
// so the representable local years are 1980..2107; the driver never clamps.
var ErrTimeOutOfRange = errors.New("fat32: time outside the FAT range 1980-2107")

// TimeRangeError names the field and the value Chtimes refused.
type TimeRangeError struct {
	Field string // "atime" or "mtime"
	Time  time.Time
}

func (e *TimeRangeError) Error() string {
	return fmt.Sprintf("fat32: %s %s is outside the FAT range 1980-2107", e.Field, e.Time.Format(time.RFC3339))
}

func (e *TimeRangeError) Unwrap() error { return ErrTimeOutOfRange }

// checkFATRange reports whether t, read as local wall-clock time (the zone Times
// decodes in), falls in the years a FAT date word can hold.
func checkFATRange(field string, t time.Time) error {
	if y := t.Local().Year(); y < 1980 || y > 1980+127 {
		return &TimeRangeError{Field: field, Time: t}
	}
	return nil
}

// Chtimes sets the access and modification times of path (Microsoft, "FAT
// Specification", "File Times").
//
//   - mtime is written to DIR_WrtDate/DIR_WrtTime: local wall clock, resolution
//     2 s, rounded DOWN to an even second (the sub-2 s part is dropped, as
//     Times reports it and as mtools does), so Times returns the value rounded
//     to 2 s.
//   - atime is written to DIR_LstAccDate only: FAT keeps a DATE, so the local
//     calendar day is stored and Times reports 00:00 of it.
//   - The creation time (DIR_CrtDate, DIR_CrtTime, DIR_CrtTimeTenth) is not
//     touched, and FAT has no change time to refresh.
//
// A zero time.Time leaves that field unchanged. A time whose local year is
// outside 1980..2107 is refused with a *TimeRangeError (errors.Is
// ErrTimeOutOfRange) before anything is written; neither field is changed in
// that case. The root directory has no entry, so it has no times to set.
//
// Only the 8.3 record changes, and it is rewritten through writeDirBuf like
// every other directory-entry update, so a failed write leaves the old times.
func (fs *fat32FS) Chtimes(path string, atime, mtime time.Time) error {
	if !atime.IsZero() {
		if err := checkFATRange("atime", atime); err != nil {
			return err
		}
	}
	if !mtime.IsZero() {
		if err := checkFATRange("mtime", mtime); err != nil {
			return err
		}
	}
	name, parent, err := fs.getParentDir(path)
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("fat32: the root directory has no times to set")
	}
	buf, err := fs.readDirBuf(parent)
	if err != nil {
		return err
	}
	off, count, ok := fat32FindEntry(buf, name)
	if !ok {
		return fmt.Errorf("fat32: %q not found: %w", path, iofs.ErrNotExist)
	}
	if atime.IsZero() && mtime.IsZero() {
		return nil
	}
	e := buf[off+(count-1)*dirEntrySize : off+count*dirEntrySize]
	encodeFATTimes(e, atime, mtime)
	return fs.writeDirBuf(parent, buf)
}

// encodeFATTimes stores atime and mtime into the 8.3 record e, skipping a zero
// time. It is the inverse of decodeFATTimes for those two fields.
func encodeFATTimes(e []byte, atime, mtime time.Time) {
	le := binary.LittleEndian
	if !atime.IsZero() {
		date, _, _ := encodeFATDateTime(atime)
		le.PutUint16(e[18:20], date)
	}
	if !mtime.IsZero() {
		date, tod, _ := encodeFATDateTime(mtime)
		le.PutUint16(e[22:24], tod)
		le.PutUint16(e[24:26], date)
	}
}
