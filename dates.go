package main

import (
	"encoding/binary"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"
)

// plausible reports whether a capture date is believable.
func plausible(t time.Time) bool {
	return !t.IsZero() && t.Year() >= 1990 && t.Before(time.Now().Add(48*time.Hour))
}

// takenFor works out when a photo/video was taken:
// JPEG EXIF → MP4/MOV mvhd → date in file name → client mtime. ok=false if nothing usable.
func takenFor(path, ext, name string, mtimeMs int64) (t time.Time, info JPEGInfo, ok bool) {
	switch ext {
	case "jpg", "jpeg":
		info, _ = ReadJPEGInfo(path)
		if plausible(info.Taken) {
			return info.Taken, info, true
		}
	case "mp4", "mov", "m4v", "3gp":
		if t, ok := mp4Created(path); ok {
			return t, info, true
		}
	}
	if t, ok := dateFromName(name); ok {
		return t, info, true
	}
	if mtimeMs > 0 {
		if t := time.UnixMilli(mtimeMs); plausible(t) {
			return t, info, true
		}
	}
	return time.Time{}, info, false
}

var nameDateRe = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})(0[1-9]|1[0-2])(0[1-9]|[12]\d|3[01])(?:[_-]?([01]\d|2[0-3])([0-5]\d)([0-5]\d))?`)

// dateFromName parses dates like IMG_20261031_123456.jpg, PXL_20261031_123456789.jpg,
// Screenshot_20261031-123456_App.png or IMG-20260831-WA0017.jpg (date only → 12:00 local).
func dateFromName(name string) (time.Time, bool) {
	m := nameDateRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	h, mi, sec := 12, 0, 0
	if m[4] != "" {
		h, mi, sec = n(m[4]), n(m[5]), n(m[6])
	}
	t := time.Date(n(m[1]), time.Month(n(m[2])), n(m[3]), h, mi, sec, 0, time.Local)
	if t.Day() != n(m[3]) || !plausible(t) { // e.g. 20260231 rolled over
		return time.Time{}, false
	}
	return t, true
}

const mp4Epoch = 2082844800 // seconds from 1904-01-01 to 1970-01-01

// mp4Created reads the movie header creation time (UTC) from an ISO-BMFF file (mp4/mov/3gp).
func mp4Created(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}
	moov, moovEnd, ok := findBox(f, 0, st.Size(), "moov")
	if !ok {
		return time.Time{}, false
	}
	mvhd, mvhdEnd, ok := findBox(f, moov, moovEnd, "mvhd")
	if !ok || mvhdEnd-mvhd < 12 {
		return time.Time{}, false
	}
	var b [12]byte
	if _, err := f.ReadAt(b[:], mvhd); err != nil {
		return time.Time{}, false
	}
	var secs uint64
	if b[0] == 1 {
		secs = binary.BigEndian.Uint64(b[4:12])
	} else {
		secs = uint64(binary.BigEndian.Uint32(b[4:8]))
	}
	if secs <= mp4Epoch || secs > mp4Epoch+1<<40 {
		return time.Time{}, false
	}
	t := time.Unix(int64(secs-mp4Epoch), 0)
	if !plausible(t) {
		return time.Time{}, false
	}
	return t, true
}

// findBox scans sibling boxes in [start,end) and returns the payload range of the first box of type typ.
func findBox(r io.ReaderAt, start, end int64, typ string) (int64, int64, bool) {
	pos := start
	for i := 0; i < 4096 && pos+8 <= end; i++ {
		var h [16]byte
		if _, err := r.ReadAt(h[:8], pos); err != nil {
			return 0, 0, false
		}
		size := int64(binary.BigEndian.Uint32(h[:4]))
		hdr := int64(8)
		switch size {
		case 0:
			size = end - pos
		case 1:
			if pos+16 > end {
				return 0, 0, false
			}
			if _, err := r.ReadAt(h[8:16], pos+8); err != nil {
				return 0, 0, false
			}
			u := binary.BigEndian.Uint64(h[8:16])
			if u > uint64(end-pos) {
				return 0, 0, false
			}
			size, hdr = int64(u), 16
		}
		if size < hdr || pos+size > end {
			return 0, 0, false
		}
		if string(h[4:8]) == typ {
			return pos + hdr, pos + size, true
		}
		pos += size
	}
	return 0, 0, false
}
