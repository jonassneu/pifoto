package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// exifBlob builds a little-endian TIFF/EXIF block with DateTimeOriginal (+ optional OffsetTimeOriginal).
func exifBlob(orientation int, dt, off string) []byte {
	le := binary.LittleEndian
	t := &bytes.Buffer{}
	t.WriteString("II")
	binary.Write(t, le, uint16(42))
	binary.Write(t, le, uint32(8))
	binary.Write(t, le, uint16(2))
	binary.Write(t, le, [6]uint16{0x0112, 3, 1, 0, uint16(orientation), 0})
	binary.Write(t, le, uint16(0x8769))
	binary.Write(t, le, uint16(4))
	binary.Write(t, le, uint32(1))
	binary.Write(t, le, uint32(38))
	binary.Write(t, le, uint32(0))
	// Exif IFD at 38
	n := 1
	if off != "" {
		n = 2
	}
	data := uint32(38 + 2 + 12*n + 4)
	binary.Write(t, le, uint16(n))
	binary.Write(t, le, uint16(0x9003))
	binary.Write(t, le, uint16(2))
	binary.Write(t, le, uint32(20))
	binary.Write(t, le, data)
	if off != "" {
		binary.Write(t, le, uint16(0x9011))
		binary.Write(t, le, uint16(2))
		binary.Write(t, le, uint32(7))
		binary.Write(t, le, data+20)
	}
	binary.Write(t, le, uint32(0))
	t.WriteString(dt + "\x00")
	if off != "" {
		t.WriteString(off + "\x00")
	}
	return t.Bytes()
}

func jpegWithExif(t *testing.T, w, h, orientation int, dt, off string) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	seg := append([]byte("Exif\x00\x00"), exifBlob(orientation, dt, off)...)
	app1 := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(app1[2:], uint16(len(seg)+2))
	b := buf.Bytes()
	return append(append(append([]byte{}, b[:2]...), append(app1, seg...)...), b[2:]...)
}

func TestEXIFOffset(t *testing.T) {
	var info JPEGInfo
	parseEXIF(exifBlob(6, "2026:10:31 12:00:00", "+09:00"), &info)
	want := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	if !info.Taken.Equal(want) || info.Orientation != 6 {
		t.Fatalf("got %v orient %d, want %v", info.Taken, info.Orientation, want)
	}
	info = JPEGInfo{}
	parseEXIF(exifBlob(1, "2026:10:31 12:00:00", ""), &info)
	if !info.Taken.Equal(time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)) {
		t.Fatalf("no-offset: got %v", info.Taken)
	}
	info = JPEGInfo{}
	parseEXIF(exifBlob(1, "2026:10:31 12:00:00", "-05:30"), &info)
	if !info.Taken.Equal(time.Date(2026, 10, 31, 17, 30, 0, 0, time.UTC)) {
		t.Fatalf("negative offset: got %v", info.Taken)
	}
}

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	return append(b, payload...)
}

func TestMvhd(t *testing.T) {
	when := time.Date(2026, 7, 14, 18, 30, 0, 0, time.UTC)
	secs := uint64(when.Unix()) + mp4Epoch
	// v0
	mv0 := make([]byte, 100)
	binary.BigEndian.PutUint32(mv0[4:], uint32(secs))
	// v1
	mv1 := make([]byte, 112)
	mv1[0] = 1
	binary.BigEndian.PutUint64(mv1[4:], secs)
	// big "free" box using 64-bit largesize before moov
	large := make([]byte, 16+40)
	binary.BigEndian.PutUint32(large, 1)
	copy(large[4:], "free")
	binary.BigEndian.PutUint64(large[8:], uint64(len(large)))
	mdat0 := []byte{0, 0, 0, 0, 'm', 'd', 'a', 't', 1, 2, 3, 4} // size 0 = to EOF

	cases := map[string][]byte{
		"v0":    bytes.Join([][]byte{box("ftyp", []byte("isom0000")), box("moov", box("mvhd", mv0)), mdat0}, nil),
		"v1":    bytes.Join([][]byte{box("ftyp", []byte("isom0000")), large, box("moov", append(box("trak", nil), box("mvhd", mv1)...))}, nil),
		"mdat0": bytes.Join([][]byte{box("ftyp", []byte("qt  0000")), box("moov", box("mvhd", mv0))}, nil),
	}
	for name, b := range cases {
		p := filepath.Join(t.TempDir(), "v.mp4")
		os.WriteFile(p, b, 0o600)
		got, ok := mp4Created(p)
		if !ok || !got.Equal(when) {
			t.Errorf("%s: got %v %v", name, got, ok)
		}
	}
	// zero creation time / garbage → not ok
	bad := map[string][]byte{
		"zero":    bytes.Join([][]byte{box("ftyp", nil), box("moov", box("mvhd", make([]byte, 100)))}, nil),
		"garbage": []byte("not an mp4 at all, just text"),
		"badsize": {0, 0, 0, 3, 'm', 'o', 'o', 'v'},
	}
	for name, b := range bad {
		p := filepath.Join(t.TempDir(), "v.mp4")
		os.WriteFile(p, b, 0o600)
		if got, ok := mp4Created(p); ok {
			t.Errorf("%s: unexpectedly ok %v", name, got)
		}
	}
}

func TestFilenameDates(t *testing.T) {
	L := time.Local
	cases := []struct {
		name string
		want time.Time
		ok   bool
	}{
		{"20261031_123456.jpg", time.Date(2026, 10, 31, 12, 34, 56, 0, L), true},
		{"IMG_20261031_123456.jpg", time.Date(2026, 10, 31, 12, 34, 56, 0, L), true},
		{"VID_20260101_000001.mp4", time.Date(2026, 1, 1, 0, 0, 1, 0, L), true},
		{"PXL_20261031_123456789.jpg", time.Date(2026, 10, 31, 12, 34, 56, 0, L), true},
		{"Screenshot_20261031-123456_Chrome.png", time.Date(2026, 10, 31, 12, 34, 56, 0, L), true},
		{"IMG-20260831-WA0017.jpg", time.Date(2026, 8, 31, 12, 0, 0, 0, L), true},
		{"IMG_1234.jpg", time.Time{}, false},
		{"20260231_120000.jpg", time.Time{}, false}, // Feb 31
		{"photo.jpg", time.Time{}, false},
		{"99920261031.jpg", time.Time{}, false}, // embedded in a longer number
	}
	for _, c := range cases {
		got, ok := dateFromName(c.name)
		if ok != c.ok || (ok && !got.Equal(c.want)) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func newTestServer(t *testing.T) *Server {
	dir := t.TempDir()
	for _, d := range []string{"originals", "thumbs", "previews", "tmp", "trash"} {
		os.MkdirAll(filepath.Join(dir, d), 0o750)
	}
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{dir: dir, store: st}
	s.thumbs = NewWorker(s)
	return s
}

func TestCrashMarker(t *testing.T) {
	s := newTestServer(t)
	s.store.Add(Item{ID: "aaaaaaaaaaaaaaaaaaaa", Ext: "jpg", Kind: "image", Taken: time.Now(), Added: time.Now()})
	s.store.Add(Item{ID: "bbbbbbbbbbbbbbbbbbbb", Ext: "jpg", Kind: "image", Taken: time.Now(), Added: time.Now(), Thumb: ThumbOK})
	s.store.Add(Item{ID: "cccccccccccccccccccc", Ext: "jpg", Kind: "image", Taken: time.Now(), Added: time.Now(), Thumb: ThumbFailed})
	os.WriteFile(filepath.Join(s.dir, "inprogress"), []byte("aaaaaaaaaaaaaaaaaaaa thumb\n"), 0o600)
	s.thumbs.RecoverCrash()
	it, _ := s.store.Get("aaaaaaaaaaaaaaaaaaaa")
	if it.Thumb != ThumbFailed || !it.Crashed {
		t.Fatalf("thumb crash not recorded: %+v", it)
	}
	if fileExists(filepath.Join(s.dir, "inprogress")) {
		t.Fatal("marker not removed")
	}
	// it must have been persisted immediately
	st2, _ := OpenStore(s.dir)
	if it2, _ := st2.Get("aaaaaaaaaaaaaaaaaaaa"); !it2.Crashed {
		t.Fatal("crash state not flushed to disk")
	}
	// preview crash
	os.WriteFile(filepath.Join(s.dir, "inprogress"), []byte("bbbbbbbbbbbbbbbbbbbb preview\n"), 0o600)
	s.thumbs.RecoverCrash()
	if it, _ := s.store.Get("bbbbbbbbbbbbbbbbbbbb"); !it.NoPreview || it.Thumb != ThumbOK {
		t.Fatalf("preview crash not recorded: %+v", it)
	}
	// QueueMissing retries the ordinary failure but not the crashed one
	s.thumbs.QueueMissing()
	if it, _ := s.store.Get("aaaaaaaaaaaaaaaaaaaa"); it.Thumb != ThumbFailed {
		t.Fatalf("crashed item was retried: %+v", it)
	}
	if it, _ := s.store.Get("cccccccccccccccccccc"); it.Thumb != ThumbPending {
		t.Fatalf("failed item not retried: %+v", it)
	}
	n, _ := s.thumbs.Pending()
	if n != 2 { // c: thumb + preview job
		t.Fatalf("queue len %d", n)
	}
}

func TestEvents(t *testing.T) {
	s := newTestServer(t)
	st := s.store
	seq0, reset, ch := st.Changes(0, false, "u1")
	if reset || len(ch) != 0 {
		t.Fatal("no since should give no changes")
	}
	st.Add(Item{ID: "a", Kind: "image", Taken: time.Now(), Added: time.Now(), Owners: []string{"u1"}})
	st.SetThumb("a", ThumbOK)
	st.SetThumb("a", ThumbOK) // no-op, no event
	seq, reset, ch := st.Changes(seq0, true, "u1")
	if reset || len(ch) != 1 || seq != seq0+2 || ch[0].Op != "add" || ch[0].Row[3] != ThumbOK {
		t.Fatalf("got seq %d reset %v changes %+v", seq, reset, ch)
	}
	if _, _, ch = st.Changes(seq0, true, "u2"); len(ch) != 0 {
		t.Fatalf("other user sees private changes: %+v", ch)
	}
	st.Delete("a")
	if _, reset, ch = st.Changes(seq0+1, true, "u1"); reset || len(ch) != 1 || ch[0].Op != "del" || ch[0].ID != "a" {
		t.Fatalf("partial: %+v", ch)
	}
	seq = st.seq
	if _, reset, ch = st.Changes(seq, true, "u1"); reset || len(ch) != 0 {
		t.Fatal("up to date should be empty")
	}
	if _, reset, _ = st.Changes(seq+1, true, "u1"); !reset {
		t.Fatal("future since must reset")
	}
	for i := 0; i < 3000; i++ {
		st.SetThumb("nope", 1) // no item: no event
		st.Add(Item{ID: "x" + string(rune('a'+i%26)) + time.Now().String(), Taken: time.Now(), Added: time.Now(), Owners: []string{"u1"}})
	}
	if _, reset, _ = st.Changes(seq0, true, "u1"); !reset {
		t.Fatal("too old since must reset")
	}
	// JSON shape
	b := st.ListJSON("u1")
	if !bytes.HasPrefix(b, []byte(`{"items":`)) && !bytes.HasPrefix(b, []byte(`{"seq":`)) {
		t.Fatalf("items json: %.60s", b)
	}
	if b := st.ListJSON("u2"); !bytes.Contains(b, []byte(`"items":[]`)) {
		t.Fatalf("u2 sees u1's items: %.80s", b)
	}
	// persistence round trip with batched writes
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	st2, _ := OpenStore(s.dir)
	if st2.Len() != st.Len() {
		t.Fatalf("persisted %d, have %d", st2.Len(), st.Len())
	}
	if st2.seq == st.seq {
		t.Fatal("seq should be randomised per process")
	}
}

func TestReconcile(t *testing.T) {
	s := newTestServer(t)
	// an original on disk that the index doesn't know (lost index write)
	id := "0123456789abcdef0123"
	p := filepath.Join(s.dir, "originals", "2026", "10", id+".jpg")
	os.MkdirAll(filepath.Dir(p), 0o750)
	os.WriteFile(p, jpegWithExif(t, 64, 48, 1, "2026:10:05 08:09:10", ""), 0o600)
	// a video with no metadata → folder month fallback
	vid := "fedcba9876543210fedc"
	pv := filepath.Join(s.dir, "originals", "2025", "03", vid+".mp4")
	os.MkdirAll(filepath.Dir(pv), 0o750)
	os.WriteFile(pv, []byte("nothing useful"), 0o600)
	os.Chtimes(pv, time.Now(), time.Now())
	// an index entry whose original is gone (deleted before the index write)
	s.store.Add(Item{ID: "ffffffffffffffffffff", Ext: "jpg", Kind: "image", Rel: "originals/2026/01/ffffffffffffffffffff.jpg", Taken: time.Now(), Added: time.Now()})
	os.WriteFile(filepath.Join(s.dir, "thumbs", "ffffffffffffffffffff.jpg"), []byte("x"), 0o600)
	added, removed := reconcile(s.dir, s.store)
	if added != 2 || removed != 1 {
		t.Fatalf("added %d removed %d", added, removed)
	}
	it, ok := s.store.Get(id)
	if !ok || !it.Taken.Equal(time.Date(2026, 10, 5, 8, 9, 10, 0, time.Local)) || it.Thumb != ThumbPending || it.W != 64 {
		t.Fatalf("re-added item wrong: %+v", it)
	}
	v, _ := s.store.Get(vid)
	if v.Taken.Year() != 2025 || v.Taken.Month() != 3 || v.Thumb != ThumbNone || v.Kind != "video" {
		t.Fatalf("video fallback wrong: %+v", v)
	}
	if fileExists(filepath.Join(s.dir, "thumbs", "ffffffffffffffffffff.jpg")) {
		t.Fatal("orphan thumb not removed")
	}
	if a, r := reconcile(s.dir, s.store); a+r != 0 {
		t.Fatal("second reconcile should be a no-op")
	}
}

func TestCleanupTemp(t *testing.T) {
	s := newTestServer(t)
	os.WriteFile(filepath.Join(s.dir, "tmp", "up-123"), []byte("partial"), 0o600)
	os.WriteFile(filepath.Join(s.dir, "thumbs", "x.jpg.tmp"), []byte("partial"), 0o600)
	os.WriteFile(filepath.Join(s.dir, "thumbs", "keep.jpg"), []byte("ok"), 0o600)
	cleanupTemp(s.dir)
	if fileExists(filepath.Join(s.dir, "tmp", "up-123")) || fileExists(filepath.Join(s.dir, "thumbs", "x.jpg.tmp")) || !fileExists(filepath.Join(s.dir, "thumbs", "keep.jpg")) {
		t.Fatal("cleanup wrong")
	}
}

func TestTruncatedJPEG(t *testing.T) {
	b := jpegWithExif(t, 800, 600, 6, "2026:10:31 12:00:00", "")
	p := filepath.Join(t.TempDir(), "trunc.jpg")
	os.WriteFile(p, b[:len(b)*2/3], 0o600) // djpeg warns "premature end of data" → exit 2
	out := p + ".out.jpg"
	if err := makeImage(p, "jpg", 480, 75, out); err != nil {
		t.Fatalf("truncated jpeg should still thumbnail: %v", err)
	}
	f, _ := os.Open(out)
	cfg, err := jpeg.DecodeConfig(f)
	f.Close()
	if err != nil || cfg.Width != 360 || cfg.Height != 480 {
		t.Fatalf("thumb %v %dx%d (want rotated 360x480)", err, cfg.Width, cfg.Height)
	}
}

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < 5; i++ {
		if l.blocked("1.2.3.4") {
			t.Fatalf("blocked too early at %d", i)
		}
		l.fail("1.2.3.4")
	}
	if !l.blocked("1.2.3.4") || l.blocked("5.6.7.8") {
		t.Fatal("limiter wrong")
	}
	l.reset("1.2.3.4")
	if l.blocked("1.2.3.4") {
		t.Fatal("reset failed")
	}
}

// A deliberate stop (systemctl restart) must not leave a crash marker behind: the job it
// interrupts would otherwise be marked as crashed and never get a thumbnail.
func TestShutdownIsNotACrash(t *testing.T) {
	s := newTestServer(t)
	s.store.Add(Item{ID: "aaaaaaaaaaaaaaaaaaaa", Ext: "jpg", Kind: "image", Taken: time.Now(), Added: time.Now(),
		Rel: "originals/missing.jpg"})
	marker := filepath.Join(s.dir, "inprogress")
	os.WriteFile(marker, []byte("aaaaaaaaaaaaaaaaaaaa thumb\n"), 0o600) // the job running at SIGTERM
	s.thumbs.Shutdown()
	if fileExists(marker) {
		t.Fatal("marker survived Shutdown")
	}
	// A job picked up after Shutdown neither writes a marker nor records a failure.
	s.thumbs.run(job{"aaaaaaaaaaaaaaaaaaaa", false})
	if fileExists(marker) {
		t.Fatal("job after Shutdown wrote a marker")
	}
	if it, _ := s.store.Get("aaaaaaaaaaaaaaaaaaaa"); it.Thumb != ThumbPending || it.Crashed {
		t.Fatalf("interrupted job recorded as failed: %+v", it)
	}
	s.thumbs.RecoverCrash() // next start
	if it, _ := s.store.Get("aaaaaaaaaaaaaaaaaaaa"); it.Thumb != ThumbPending || it.Crashed {
		t.Fatalf("restart marked it crashed: %+v", it)
	}
}
