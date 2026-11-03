// mkphotos generates synthetic test JPEGs with EXIF DateTimeOriginal + Orientation.
// Usage: mkphotos OUTDIR [count]
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func exifAPP1(orientation int, dt string) []byte {
	le := binary.LittleEndian
	t := &bytes.Buffer{}
	t.WriteString("II")
	binary.Write(t, le, uint16(42))
	binary.Write(t, le, uint32(8)) // IFD0 at 8
	// IFD0: 2 entries -> 2 + 24 + 4 = 30 bytes, ends at 38
	binary.Write(t, le, uint16(2))
	binary.Write(t, le, [6]uint16{0x0112, 3, 1, 0, uint16(orientation), 0}) // tag,type,count(lo,hi),value(short)+pad
	exifIFD := uint32(38)
	binary.Write(t, le, uint16(0x8769))
	binary.Write(t, le, uint16(4))
	binary.Write(t, le, uint32(1))
	binary.Write(t, le, exifIFD)
	binary.Write(t, le, uint32(0))
	// Exif IFD at 38: 1 entry -> 2+12+4 = 18, data at 56
	binary.Write(t, le, uint16(1))
	binary.Write(t, le, uint16(0x9003))
	binary.Write(t, le, uint16(2))
	binary.Write(t, le, uint32(20))
	binary.Write(t, le, uint32(56))
	binary.Write(t, le, uint32(0))
	t.WriteString(dt + "\x00")
	seg := append([]byte("Exif\x00\x00"), t.Bytes()...)
	out := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(out[2:], uint16(len(seg)+2))
	return append(out, seg...)
}

func main() {
	dir := os.Args[1]
	n := 8
	if len(os.Args) > 2 {
		n, _ = strconv.Atoi(os.Args[2])
	}
	os.MkdirAll(dir, 0o755)
	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.Local)
	for i := 0; i < n; i++ {
		w, h := 4000, 3000
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		hue := float64(i) / float64(n)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				fx, fy := float64(x)/float64(w), float64(y)/float64(h)
				r := 0.5 + 0.5*math.Sin(6.28*(hue+fx))
				g := 0.5 + 0.5*math.Sin(6.28*(hue+fy+0.33))
				b := 0.5 + 0.5*math.Sin(6.28*(hue+fx*fy+0.66))
				img.Pix[y*img.Stride+x*4+0] = uint8(r * 255)
				img.Pix[y*img.Stride+x*4+1] = uint8(g * 255)
				img.Pix[y*img.Stride+x*4+2] = uint8(b * 255)
				img.Pix[y*img.Stride+x*4+3] = 255
			}
		}
		// "TOP" marker: white bar on the top-left so orientation is visible in thumbnails
		for y := 0; y < 300; y++ {
			for x := 0; x < 1400; x++ {
				img.Set(x, y, color.White)
			}
		}
		var buf bytes.Buffer
		jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88})
		b := buf.Bytes()
		orient := []int{1, 6, 1, 8, 3, 1, 1, 6}[i%8]
		// spread over months so the timeline shows several sections
		when := base.AddDate(0, -i/3, -i)
		app1 := exifAPP1(orient, when.Format("2006:01:02 15:04:05"))
		out := append(append(append([]byte{}, b[:2]...), app1...), b[2:]...)
		name := filepath.Join(dir, fmt.Sprintf("TEST_%s_o%d.jpg", when.Format("20060102_150405"), orient))
		os.WriteFile(name, out, 0o644)
		fmt.Println(name)
	}
}
