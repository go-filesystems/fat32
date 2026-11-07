package filesystem_fat32

import (
	"encoding/binary"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

var _ filesystem.TimesReader = (*fat32FS)(nil)

// Resolutions of the FAT directory-entry times (Microsoft, "File Times": create
// 10 ms, write 2 s, access 1 day).
const (
	fatWriteRes  = 2 * time.Second
	fatAccessRes = 24 * time.Hour
	fatCreateRes = 10 * time.Millisecond
)

// Times returns the times recorded in the 8.3 directory entry of path.
//
//   - Mtime is DIR_WrtDate/DIR_WrtTime, resolution 2 s.
//   - Atime is DIR_LstAccDate, a DATE: it is reported as 00:00 of that day,
//     resolution 24 h.
//   - Btime is DIR_CrtDate/DIR_CrtTime plus DIR_CrtTimeTenth (0..199 units of
//     10 ms, the odd second and the fraction), resolution 10 ms.
//   - Ctime is not recorded: FAT has no change time.
//
// FAT stores the wall clock without a zone; it is interpreted in time.Local,
// as the writer of this package stamps it (a wall time that does not exist, or
// exists twice, around a daylight-saving change is resolved as time.Date does).
// A field that is all zero, or that does not decode to a real date (month 0,
// day 0, hour 24, ...), is reported as not recorded rather than normalised
// into some other date. The root directory has no entry, so none of its times
// are recorded.
func (fs *fat32FS) Times(path string) (filesystem.FileTimes, error) {
	if path == "/" {
		return filesystem.FileTimes{}, nil
	}
	entry, _, err := fs.resolvePath(path)
	if err != nil {
		return filesystem.FileTimes{}, err
	}
	return decodeFATTimes(entry.short[:]), nil
}

// decodeFATTimes decodes the timestamp fields of a 32-byte 8.3 entry.
func decodeFATTimes(e []byte) filesystem.FileTimes {
	le := binary.LittleEndian
	return filesystem.FileTimes{
		Btime: decodeFATTime(le.Uint16(e[16:]), le.Uint16(e[14:]), e[13], fatCreateRes, true),
		Atime: decodeFATTime(le.Uint16(e[18:]), 0, 0, fatAccessRes, false),
		Mtime: decodeFATTime(le.Uint16(e[24:]), le.Uint16(e[22:]), 0, fatWriteRes, false),
	}
}

// decodeFATTime decodes a FAT date word, time word and creation tenth. It
// returns the zero Timestamp for an absent (date 0) or invalid value.
func decodeFATTime(date, tod uint16, tenth uint8, res time.Duration, hasTenth bool) filesystem.Timestamp {
	if date == 0 {
		return filesystem.Timestamp{}
	}
	year := 1980 + int(date>>9)
	month := int(date>>5) & 0x0F
	day := int(date) & 0x1F
	hour := int(tod >> 11)
	min := int(tod>>5) & 0x3F
	sec := int(tod&0x1F) * 2
	if month < 1 || month > 12 || day < 1 || hour > 23 || min > 59 || sec > 58 {
		return filesystem.Timestamp{}
	}
	if day > daysIn(year, month) {
		return filesystem.Timestamp{}
	}
	var ns int
	if hasTenth {
		if tenth > 199 {
			return filesystem.Timestamp{}
		}
		sec += int(tenth) / 100
		ns = (int(tenth) % 100) * 10_000_000
	}
	return filesystem.NewTimestamp(time.Date(year, time.Month(month), day, hour, min, sec, ns, time.Local), res)
}

func daysIn(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
