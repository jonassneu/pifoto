package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	thumbMax      = 480  // grid tiles
	previewMax    = 1600 // full-screen viewer
	maxJPEGPixels = 100_000_000
	// PNG/GIF are fully decoded in Go: keep CPU time and RAM bounded on a 512 MB Pi. 12 MP at 4
	// bytes per pixel; a 16-bit PNG takes 8, and an interlaced one twice that while decoding.
	maxOtherPixels = 12_000_000
	maxDecodeBytes = 48_000_000
)

// djpegTimeout: a preview of a big photo takes seconds on a Pi 1, so a djpeg still running
// after this is stuck on a crafted or broken file. It counts as a failed thumbnail.
var djpegTimeout = 5 * time.Minute

var errDjpegTimeout = errors.New("djpeg took too long")

// ---------- JPEG metadata (dimensions, EXIF orientation + capture date) ----------

type JPEGInfo struct {
	W, H        int
	Orientation int
	Taken       time.Time
}

// Displayed returns width and height as shown, i.e. swapped for EXIF orientations 5-8 (90° turns).
func (i JPEGInfo) Displayed() (int, int) {
	if i.Orientation >= 5 && i.Orientation <= 8 {
		return i.H, i.W
	}
	return i.W, i.H
}

// imgSize reads only an image's header (JPEG, PNG, GIF). 0, 0 if unknown.
func imgSize(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	c, _, err := image.DecodeConfig(bufio.NewReader(f))
	if err != nil {
		return 0, 0
	}
	return c.Width, c.Height
}

// ReadJPEGInfo walks JPEG markers, skipping segment payloads without reading them into memory.
func ReadJPEGInfo(path string) (JPEGInfo, error) {
	info := JPEGInfo{Orientation: 1}
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 16<<10)
	var soi [2]byte
	if _, err := io.ReadFull(r, soi[:]); err != nil || soi != [2]byte{0xFF, 0xD8} {
		return info, errors.New("not a jpeg")
	}
	for {
		b, err := r.ReadByte()
		if err != nil {
			return info, err
		}
		if b != 0xFF {
			return info, errors.New("bad marker")
		}
		m, err := r.ReadByte()
		for err == nil && m == 0xFF { // fill bytes
			m, err = r.ReadByte()
		}
		if err != nil {
			return info, err
		}
		if m == 0x01 || (m >= 0xD0 && m <= 0xD7) {
			continue
		}
		if m == 0xD9 || m == 0xDA {
			return info, nil
		}
		var lb [2]byte
		if _, err := io.ReadFull(r, lb[:]); err != nil {
			return info, err
		}
		l := int(binary.BigEndian.Uint16(lb[:])) - 2
		if l < 0 {
			return info, errors.New("bad length")
		}
		isSOF := m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC
		switch {
		case m == 0xE1 && info.Taken.IsZero():
			seg := make([]byte, l)
			if _, err := io.ReadFull(r, seg); err != nil {
				return info, err
			}
			if bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
				parseEXIF(seg[6:], &info)
			}
		case isSOF:
			seg := make([]byte, l)
			if _, err := io.ReadFull(r, seg); err != nil || len(seg) < 5 {
				return info, err
			}
			info.H = int(binary.BigEndian.Uint16(seg[1:]))
			info.W = int(binary.BigEndian.Uint16(seg[3:]))
			return info, nil // SOF is the last thing we need
		default:
			if _, err := r.Discard(l); err != nil {
				return info, err
			}
		}
	}
}

func parseEXIF(t []byte, info *JPEGInfo) {
	if len(t) < 8 {
		return
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return
	}
	str := func(e []byte) string {
		cnt := bo.Uint32(e[4:])
		var v []byte
		if cnt <= 4 {
			v = e[8 : 8+cnt]
		} else {
			off := bo.Uint32(e[8:])
			if uint64(off)+uint64(cnt) > uint64(len(t)) {
				return ""
			}
			v = t[off : off+cnt]
		}
		return strings.TrimRight(string(v), "\x00 ")
	}
	var dateTime, dateOrig, offTime, offOrig string
	var exifOff uint32
	walk := func(off uint32, fn func(tag uint16, e []byte)) {
		if uint64(off)+2 > uint64(len(t)) {
			return
		}
		n := int(bo.Uint16(t[off:]))
		for i := 0; i < n; i++ {
			p := int(off) + 2 + i*12
			if p+12 > len(t) {
				return
			}
			e := t[p : p+12]
			fn(bo.Uint16(e), e)
		}
	}
	walk(bo.Uint32(t[4:]), func(tag uint16, e []byte) {
		switch tag {
		case 0x0112: // Orientation (SHORT)
			if o := int(bo.Uint16(e[8:])); o >= 1 && o <= 8 {
				info.Orientation = o
			}
		case 0x0132:
			dateTime = str(e)
		case 0x8769:
			exifOff = bo.Uint32(e[8:])
		}
	})
	if exifOff != 0 {
		walk(exifOff, func(tag uint16, e []byte) {
			switch tag {
			case 0x9003:
				dateOrig = str(e)
			case 0x9010:
				offTime = str(e)
			case 0x9011:
				offOrig = str(e)
			}
		})
	}
	if offOrig == "" {
		offOrig = offTime
	}
	for _, c := range [][2]string{{dateOrig, offOrig}, {dateTime, offTime}} {
		if tm, err := time.ParseInLocation("2006:01:02 15:04:05", c[0], exifZone(c[1])); err == nil && tm.Year() > 1990 {
			info.Taken = tm
			return
		}
	}
}

// exifZone parses an EXIF offset like "+02:00"; falls back to the Pi's local zone.
func exifZone(s string) *time.Location {
	if len(s) == 6 && (s[0] == '+' || s[0] == '-') && s[3] == ':' {
		h, err1 := strconv.Atoi(s[1:3])
		m, err2 := strconv.Atoi(s[4:6])
		if err1 == nil && err2 == nil && h <= 14 && m < 60 {
			sec := h*3600 + m*60
			if s[0] == '-' {
				sec = -sec
			}
			return time.FixedZone(s, sec)
		}
	}
	return time.Local
}

// ---------- thumbnail generation ----------

func CanThumb(ext string) bool {
	switch ext {
	case "jpg", "jpeg", "png", "gif":
		return true
	}
	return false
}

var errTooLarge = errors.New("image too large")

// makeImage writes a JPEG of at most maxDim px on the long side to out.
func makeImage(src, ext string, maxDim, quality int, out string) error {
	var img *image.RGBA
	if ext == "jpg" || ext == "jpeg" {
		info, err := ReadJPEGInfo(src)
		if info.W <= 0 || info.H <= 0 {
			return fmt.Errorf("no jpeg dimensions: %v", err)
		}
		if int64(info.W)*int64(info.H) > maxJPEGPixels {
			return errTooLarge
		}
		dec, err := djpegScaled(src, info, maxDim)
		if err != nil {
			return err
		}
		img = orient(resizeFrom(dec, maxDim), info.Orientation)
	} else {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		var head [29]byte
		io.ReadFull(f, head[:])
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		cfg, format, err := image.DecodeConfig(f)
		if err != nil {
			return err
		}
		px := int64(cfg.Width) * int64(cfg.Height)
		if cfg.Width <= 0 || cfg.Height <= 0 || px > maxOtherPixels || decodeBytes(cfg, format, head[:]) > maxDecodeBytes {
			return errTooLarge
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		dec, _, err := image.Decode(f)
		if err != nil {
			return err
		}
		img = resizeFrom(dec, maxDim) // no full-size RGBA copy
	}

	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	err = jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}

// decodeBytes estimates the memory image.Decode needs for an image (cfg from DecodeConfig, head:
// the file's first 29 bytes), before it allocates any of it.
func decodeBytes(cfg image.Config, format string, head []byte) int64 {
	px := int64(cfg.Width) * int64(cfg.Height)
	var per int64
	switch cfg.ColorModel {
	case color.GrayModel:
		per = 1
	case color.Gray16Model:
		per = 2
	case color.RGBAModel, color.NRGBAModel:
		per = 4
	default:
		if _, ok := cfg.ColorModel.(color.Palette); ok {
			per = 1
		} else {
			per = 8 // RGBA64, NRGBA64, and whatever else: assume the worst
		}
	}
	if format == "jpeg" {
		// a JPEG under another name, decoded in Go: progressive ones keep 2 bytes per
		// coefficient for 3 components besides the YCbCr result
		per = 9
	}
	// PNG header: signature, IHDR length and type, width, height, depth, color, compression,
	// filter, then the interlace method. Adam7 decodes passes and merges them into the image.
	if format == "png" && len(head) >= 29 && head[28] == 1 {
		per *= 2
	}
	return px * per
}

// djpegScaled decodes with libjpeg-turbo at n/8 scale (much less CPU/RAM than a full decode),
// streaming its PNM output and validating dimensions before allocating.
func djpegScaled(src string, info JPEGInfo, maxDim int) (*image.RGBA, error) {
	n := 8
	long := max(info.W, info.H)
	for k := 1; k <= 8; k++ {
		if long*k/8 >= maxDim {
			n = k
			break
		}
	}
	wantW, wantH := (info.W*n+7)/8, (info.H*n+7)/8
	ctx, cancel := context.WithTimeout(context.Background(), djpegTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nice", "-n", "10", "djpeg", "-maxmemory", "65536",
		"-scale", fmt.Sprintf("%d/8", n), "-dct", "fast", "-pnm", src)
	cmd.WaitDelay = 10 * time.Second // then its pipes are closed too, even if something still holds them
	stderr := &capBuf{max: 300}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	img, perr := readPNM(bufio.NewReaderSize(stdout, 64<<10), wantW, wantH, -1)
	if perr != nil {
		cmd.Process.Kill()
	} else {
		io.Copy(io.Discard, stdout)
	}
	werr := cmd.Wait()
	msg := strings.TrimSpace(stderr.String())
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%w (%v)", errDjpegTimeout, djpegTimeout)
	}
	if perr != nil {
		return nil, fmt.Errorf("djpeg: %v (%v) %s", perr, werr, msg)
	}
	if werr != nil {
		// exit status 2 = warnings only (e.g. truncated file); the pixels were read completely
		var ee *exec.ExitError
		if !errors.As(werr, &ee) || ee.ExitCode() != 2 {
			return nil, fmt.Errorf("djpeg: %v %s", werr, msg)
		}
		log.Printf("djpeg warning for %s: %s", filepath.Base(src), msg)
	}
	return img, nil
}

// capBuf keeps only the first max bytes written to it.
type capBuf struct {
	bytes.Buffer
	max int
}

func (c *capBuf) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// readPNM reads binary PPM (P6) / PGM (P5). If wantW/wantH > 0 the size must match (±1).
// avail (if ≥ 0) is the number of bytes available, checked before allocating.
func readPNM(r *bufio.Reader, wantW, wantH int, avail int64) (*image.RGBA, error) {
	tok := func() (string, error) {
		var b []byte
		for {
			c, err := r.ReadByte()
			if err != nil {
				return "", err
			}
			switch {
			case c == '#' && len(b) == 0:
				for c != '\n' {
					if c, err = r.ReadByte(); err != nil {
						return "", err
					}
				}
			case c == ' ' || c == '\t' || c == '\n' || c == '\r':
				if len(b) > 0 {
					return string(b), nil // consumes exactly one trailing whitespace byte
				}
			default:
				if b = append(b, c); len(b) > 12 {
					return "", errors.New("bad pnm header")
				}
			}
		}
	}
	var f [4]string
	for i := range f {
		var err error
		if f[i], err = tok(); err != nil {
			return nil, err
		}
	}
	w, err1 := strconv.Atoi(f[1])
	h, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil || f[3] != "255" || w <= 0 || h <= 0 {
		return nil, errors.New("unsupported pnm")
	}
	ch := 3
	switch f[0] {
	case "P6":
	case "P5":
		ch = 1
	default:
		return nil, errors.New("unsupported pnm " + f[0])
	}
	if wantW > 0 && (abs(w-wantW) > 1 || abs(h-wantH) > 1) {
		return nil, fmt.Errorf("unexpected size %dx%d, want %dx%d", w, h, wantW, wantH)
	}
	need := int64(w) * int64(h) * int64(ch)
	if int64(w)*int64(h) > maxJPEGPixels/4 || (avail >= 0 && need > avail) {
		return nil, errors.New("pnm size mismatch")
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	row := make([]byte, w*ch)
	for y := 0; y < h; y++ {
		if _, err := io.ReadFull(r, row); err != nil {
			return nil, fmt.Errorf("short pnm: %v", err)
		}
		d := img.Pix[y*img.Stride : y*img.Stride+w*4]
		if ch == 3 {
			for i, j := 0, 0; i < len(row); i, j = i+3, j+4 {
				d[j], d[j+1], d[j+2], d[j+3] = row[i], row[i+1], row[i+2], 255
			}
		} else {
			for i, j := 0, 0; i < len(row); i, j = i+1, j+4 {
				d[j], d[j+1], d[j+2], d[j+3] = row[i], row[i], row[i], 255
			}
		}
	}
	return img, nil
}

// decodePNM parses an in-memory PNM (used by tests/fuzzing).
func decodePNM(b []byte) (*image.RGBA, error) {
	br := bufio.NewReader(bytes.NewReader(b))
	return readPNM(br, 0, 0, int64(len(b)))
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// resizeFrom box-filters any image down to fit maxDim, converting one source row at a time
// (so a decoded PNG never needs a second full-size copy). Transparent areas become white.
func resizeFrom(src image.Image, maxDim int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := sw, sh
	if long := max(sw, sh); long > maxDim {
		dw, dh = max(1, sw*maxDim/long), max(1, sh*maxDim/long)
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	opaque := false
	if o, ok := src.(interface{ Opaque() bool }); ok {
		opaque = o.Opaque()
	}
	if opaque && dw == sw && dh == sh {
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
		return dst
	}
	row := image.NewRGBA(image.Rect(0, 0, sw, 1))
	white := image.NewUniform(color.White)
	xs := make([]int, dw+1)
	for x := 0; x <= dw; x++ {
		xs[x] = x * sw / dw
	}
	sums := make([]uint32, dw*3)
	for dy := 0; dy < dh; dy++ {
		y0 := dy * sh / dh
		y1 := max((dy+1)*sh/dh, y0+1)
		clear(sums)
		for y := y0; y < y1; y++ {
			if opaque {
				draw.Draw(row, row.Bounds(), src, image.Pt(b.Min.X, b.Min.Y+y), draw.Src)
			} else {
				draw.Draw(row, row.Bounds(), white, image.Point{}, draw.Src)
				draw.Draw(row, row.Bounds(), src, image.Pt(b.Min.X, b.Min.Y+y), draw.Over)
			}
			p := row.Pix
			for dx := 0; dx < dw; dx++ {
				x0, x1 := xs[dx], max(xs[dx+1], xs[dx]+1)
				var r, g, bl uint32
				for i := x0 * 4; i < x1*4; i += 4 {
					r += uint32(p[i])
					g += uint32(p[i+1])
					bl += uint32(p[i+2])
				}
				sums[dx*3] += r
				sums[dx*3+1] += g
				sums[dx*3+2] += bl
			}
		}
		rows := uint32(y1 - y0)
		for dx := 0; dx < dw; dx++ {
			n := uint32(max(xs[dx+1], xs[dx]+1)-xs[dx]) * rows
			o := dy*dst.Stride + dx*4
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(sums[dx*3]/n), uint8(sums[dx*3+1]/n), uint8(sums[dx*3+2]/n), 255
		}
	}
	return dst
}

// orient applies EXIF orientation so thumbnails are upright.
func orient(src *image.RGBA, o int) *image.RGBA {
	if o <= 1 || o > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch o {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			}
			copy(dst.Pix[ny*dst.Stride+nx*4:ny*dst.Stride+nx*4+4], src.Pix[y*src.Stride+x*4:y*src.Stride+x*4+4])
		}
	}
	return dst
}

// ---------- background worker ----------

type job struct {
	id      string
	preview bool
}

// Worker makes thumbnails one at a time (the Pi 1 has a single core).
// All grid thumbnails are made before any full-screen previews.
// A marker file names the job in progress, so a file that crashes or OOM-kills the
// process is marked as failed on the next start instead of crashing it again.
type Worker struct {
	s      *Server
	mu     sync.Mutex
	thumbs []job
	prevs  []job
	cur    string
	wake   chan struct{}
	// stopping: a deliberate stop (systemctl stop/restart) is under way. A job it interrupts was
	// not the cause, so it must not be remembered as a crash or a failure: it simply runs again
	// after the restart.
	stopping bool
}

// Shutdown starts no more jobs and clears the crash marker of the one running now.
// Call it on SIGTERM, before the final index write.
func (w *Worker) Shutdown() {
	w.mu.Lock()
	w.stopping = true
	os.Remove(w.markerPath())
	w.mu.Unlock()
}

func (w *Worker) isStopping() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopping
}

func NewWorker(s *Server) *Worker {
	return &Worker{s: s, wake: make(chan struct{}, 1)}
}

func (w *Worker) Start() { go w.loop() }

func (w *Worker) markerPath() string { return filepath.Join(w.s.dir, "inprogress") }

// RecoverCrash handles a marker left by a previous process that died mid-job.
func (w *Worker) RecoverCrash() {
	b, err := os.ReadFile(w.markerPath())
	if err != nil {
		return
	}
	if f := strings.Fields(string(b)); len(f) > 0 {
		preview := len(f) > 1 && f[1] == "preview"
		log.Printf("previous run died while processing %s (preview=%v); not retrying it", f[0], preview)
		w.s.store.Update(f[0], func(it *Item) {
			it.Crashed = true
			if preview {
				it.NoPreview = true
			} else {
				it.Thumb = ThumbFailed
			}
		})
		if err := w.s.store.Flush(); err != nil {
			log.Printf("flush after crash recovery: %v", err)
		}
	}
	os.Remove(w.markerPath())
}

func (w *Worker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) Queue(id string) {
	w.mu.Lock()
	w.thumbs = append(w.thumbs, job{id, false})
	w.prevs = append(w.prevs, job{id, true})
	w.mu.Unlock()
	w.signal()
}

func (w *Worker) QueueMissing() {
	// retry earlier failures once per start (e.g. after an update fixed a decoder bug),
	// except files that crashed the process
	for _, id := range w.s.store.IDsWithThumb(ThumbFailed) {
		w.s.store.Update(id, func(it *Item) {
			if !it.Crashed {
				it.Thumb = ThumbPending
			}
		})
	}
	for _, id := range w.s.store.IDsWithThumb(ThumbPending) {
		w.Queue(id)
	}
	for _, id := range w.s.store.IDsWithThumb(ThumbOK) {
		w.fixDims(id) // backfill for libraries from before dimensions were tracked
	}
	w.mu.Lock()
	for _, id := range w.s.store.IDsWithThumb(ThumbOK) {
		if it, _ := w.s.store.Get(id); it.NoPreview {
			continue
		}
		if _, err := os.Stat(filepath.Join(w.s.dir, "previews", id+".jpg")); err != nil {
			w.prevs = append(w.prevs, job{id, true})
		}
	}
	w.mu.Unlock()
	w.signal()
}

func (w *Worker) Pending() (int, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.thumbs) + len(w.prevs), w.cur
}

func (w *Worker) next() (job, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var j job
	switch {
	case len(w.thumbs) > 0:
		j, w.thumbs = w.thumbs[0], w.thumbs[1:]
	case len(w.prevs) > 0:
		j, w.prevs = w.prevs[0], w.prevs[1:]
	default:
		w.cur = ""
		return j, false
	}
	w.cur = j.id
	return j, true
}

// fixDims stores an item's displayed width/height (the web app lays out rows by aspect ratio).
// The finished thumbnail is already upright, so its shape settles any rotation question.
// Only header reads: cheap enough to run for every photo at startup.
func (w *Worker) fixDims(id string) {
	it, ok := w.s.store.Get(id)
	if !ok || it.Dims {
		return
	}
	tw, th := imgSize(filepath.Join(w.s.dir, "thumbs", id+".jpg"))
	ow, oh := it.W, it.H
	if ow <= 0 || oh <= 0 {
		ow, oh = imgSize(filepath.Join(w.s.dir, it.Rel)) // PNG / GIF
	}
	if ow <= 0 || oh <= 0 {
		ow, oh = tw, th
	}
	if tw > 0 && th > 0 && ow != oh && (tw > th) != (ow > oh) {
		ow, oh = oh, ow
	}
	if ow <= 0 || oh <= 0 {
		return
	}
	w.s.store.Update(id, func(it *Item) { it.W, it.H, it.Dims = ow, oh, true })
}

func (w *Worker) loop() {
	for {
		j, ok := w.next()
		if !ok {
			<-w.wake
			continue
		}
		w.run(j)
	}
}

func (w *Worker) run(j job) {
	it, ok := w.s.store.Get(j.id)
	if !ok {
		return
	}
	var out string
	var maxDim, quality int
	if j.preview {
		if it.Thumb != ThumbOK || it.NoPreview {
			return
		}
		out = filepath.Join(w.s.dir, "previews", j.id+".jpg")
		if _, err := os.Stat(out); err == nil {
			return
		}
		maxDim, quality = previewMax, 80
	} else {
		if it.Thumb != ThumbPending {
			return
		}
		out = filepath.Join(w.s.dir, "thumbs", j.id+".jpg")
		maxDim, quality = thumbMax, 75
	}
	kind := "thumb"
	if j.preview {
		kind = "preview"
	}
	// No fsync needed: the marker only has to survive a process crash/OOM kill, not a power cut.
	// Written under w.mu so it can't reappear after Shutdown removed it.
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		return
	}
	os.WriteFile(w.markerPath(), []byte(j.id+" "+kind+"\n"), 0o600)
	w.mu.Unlock()
	defer os.Remove(w.markerPath())

	fail := func(why any) {
		if w.isStopping() { // e.g. djpeg killed by the shutdown: retried after the restart
			log.Printf("%s %s interrupted by shutdown: %v", kind, j.id, why)
			return
		}
		log.Printf("%s %s (%s) failed: %v", kind, j.id, it.Name, why)
		w.s.store.Update(j.id, func(it *Item) {
			if j.preview {
				it.NoPreview = true
			} else {
				it.Thumb = ThumbFailed
			}
		})
	}
	defer func() {
		if r := recover(); r != nil {
			os.Remove(out + ".tmp")
			w.s.store.Update(j.id, func(it *Item) { it.Crashed = true })
			fail(fmt.Sprintf("panic: %v\n%s", r, debug.Stack()))
		}
	}()

	start := time.Now()
	err := makeImage(filepath.Join(w.s.dir, it.Rel), it.Ext, maxDim, quality, out)
	if _, still := w.s.store.Get(j.id); !still { // deleted meanwhile
		os.Remove(out)
		return
	}
	if err != nil {
		if j.preview && !errors.Is(err, errDjpegTimeout) {
			log.Printf("preview %s (%s): %v", j.id, it.Name, err) // viewer falls back to original
			return
		}
		fail(err) // a stuck preview isn't retried on every start either
		return
	}
	if !j.preview {
		w.fixDims(j.id) // before the thumb event, which carries the row with the dimensions
		w.s.store.SetThumb(j.id, ThumbOK)
	}
	log.Printf("%s %s done in %v", kind, j.id, time.Since(start).Round(time.Millisecond))
}
