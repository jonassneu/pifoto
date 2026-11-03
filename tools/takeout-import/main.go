// takeout-import copies a Google Photos library from Google Takeout into PiFoto.
//
//	takeout-import -server http://raspberrypi.fritz.box -password-file pw.txt [-dry-run] takeout-*.zip | dir...
//
// It reads the ZIP files directly (ZIP64 too), so nothing needs to be unpacked first.
//   - Photos that are already on the Pi are skipped: identical files by content hash (checked
//     locally before uploading), the same photo with different bytes by the server's
//     "same name + same moment" rule.
//   - The same photo appears once per album in Takeout; it is uploaded only once.
//   - Google's edited copies ("-edited", "-bearbeitet", ...) are skipped when the original is there
//     (-keep-edited to import them too). Short video halves of iPhone Live Photos are skipped
//     (-keep-live to import them).
//   - Capture time comes from the photo itself; Google's JSON sidecar fills in when it has none.
//   - Safe to stop and run again: files already on the Pi are skipped.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var mediaExt = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true, "heic": true, "heif": true,
	"avif": true, "dng": true, "mp4": true, "mov": true, "webm": true, "3gp": true, "m4v": true, "mkv": true,
}

// Google's "edited copy" suffix in the languages Takeout uses.
var editedRe = regexp.MustCompile(`(?i)-(edited|bearbeitet|modifié|modifie|editado|modificato|bewerkt|redigerad|redigeret|muokattu|edytowane|upravené|szerkesztett)(\(\d+\))?$`)

// counterRe matches a trailing "(1)" copy counter in a file stem.
var counterRe = regexp.MustCompile(`\((\d+)\)$`)

type source interface {
	open() (io.ReadCloser, error)
}

type zipSrc struct{ f *zip.File }

func (z zipSrc) open() (io.ReadCloser, error) { return z.f.Open() }

type fileSrc struct{ p string }

func (f fileSrc) open() (io.ReadCloser, error) { return os.Open(f.p) }

type entry struct {
	dir, name string // directory inside the export, file name
	size      int64
	src       source
	where     string // for messages: zip name or path
}

type media struct {
	entry
	ext     string
	takenMs int64 // from the JSON sidecar, 0 if none
	id      string
	skip    string // reason, "" = import
}

func main() {
	server := flag.String("server", "http://raspberrypi.fritz.box", "PiFoto address")
	pwFile := flag.String("password-file", "", "file whose line 'PiFoto login password: ...' (or whole content) is the password")
	dry := flag.Bool("dry-run", false, "only scan and report, upload nothing")
	keepEdited := flag.Bool("keep-edited", false, "also import Google's edited copies")
	keepLive := flag.Bool("keep-live", false, "also import the short video half of Live Photos")
	stateFile := flag.String("state", filepath.Join(os.Getenv("HOME"), ".cache", "pifoto-takeout-done.txt"), "remembers finished files so a re-run skips them")
	flag.Parse()
	if flag.NArg() == 0 {
		log.Fatal("give Takeout .zip files or unpacked folders")
	}
	log.SetFlags(log.Ltime)

	// ---- 1. collect entries
	var all []entry
	for _, arg := range flag.Args() {
		st, err := os.Stat(arg)
		if err != nil {
			log.Fatal(err)
		}
		if st.IsDir() {
			filepath.WalkDir(arg, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				info, _ := d.Info()
				rel, _ := filepath.Rel(arg, p)
				all = append(all, entry{dir: filepath.ToSlash(filepath.Dir(rel)), name: d.Name(), size: info.Size(), src: fileSrc{p}, where: p})
				return nil
			})
			continue
		}
		zr, err := zip.OpenReader(arg) // stays open until exit
		if err != nil {
			log.Fatalf("%s: %v", arg, err)
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			all = append(all, entry{dir: path.Dir(f.Name), name: path.Base(f.Name), size: int64(f.UncompressedSize64), src: zipSrc{f}, where: filepath.Base(arg) + ":" + f.Name})
		}
	}

	// ---- 2. sidecars: (dir, media file name) -> taken time
	taken := map[string]int64{}
	titles := map[string][]string{} // dir -> sidecar media names, for truncated-name fallback
	var medias []*media
	for _, e := range all {
		ext := strings.ToLower(strings.TrimPrefix(path.Ext(e.name), "."))
		if ext == "json" {
			if ms, mediaName, ok := readSidecar(e); ok {
				taken[e.dir+"/"+mediaName] = ms
				titles[e.dir] = append(titles[e.dir], mediaName)
			}
			continue
		}
		if mediaExt[ext] {
			medias = append(medias, &media{entry: e, ext: ext})
		}
	}
	for _, m := range medias {
		m.takenMs = taken[m.dir+"/"+m.name]
		if m.takenMs == 0 && len(stem(m.name)) >= 30 { // Takeout truncates long file names
			for _, t := range titles[m.dir] {
				if strings.HasPrefix(stem(t), stem(m.name)) {
					m.takenMs = taken[m.dir+"/"+t]
					break
				}
			}
		}
	}

	// ---- 3. edited copies and live-photo videos (per folder)
	byDir := map[string]map[string]*media{} // dir -> lower stem -> media
	for _, m := range medias {
		d := byDir[m.dir]
		if d == nil {
			d = map[string]*media{}
			byDir[m.dir] = d
		}
		d[strings.ToLower(stem(m.name))+"."+kindOf(m.ext)] = m
	}
	for _, m := range medias {
		st := stem(m.name)
		if !*keepEdited && editedRe.MatchString(st) {
			orig := strings.ToLower(editedRe.ReplaceAllString(st, "")) + "." + kindOf(m.ext)
			if _, ok := byDir[m.dir][orig]; ok {
				m.skip = "edited copy"
				continue
			}
		}
		if !*keepLive && kindOf(m.ext) == "video" && (m.ext == "mov" || m.ext == "mp4") && m.size < 8<<20 {
			if _, ok := byDir[m.dir][strings.ToLower(st)+".image"]; ok {
				m.skip = "live photo video"
			}
		}
	}

	// ---- 4. hash, drop duplicates inside the export
	log.Printf("scanning %d photos and videos (%s)…", len(medias), human(sumSize(medias)))
	seen := map[string]*media{}
	for i, m := range medias {
		if m.skip != "" {
			continue
		}
		id, err := hashOf(m.entry)
		if err != nil {
			m.skip = "unreadable: " + err.Error()
			continue
		}
		m.id = id
		if first, ok := seen[id]; ok {
			m.skip = "same file as " + first.name
			// prefer a sidecar date from any copy
			if first.takenMs == 0 {
				first.takenMs = m.takenMs
			}
			continue
		}
		seen[id] = m
		if (i+1)%500 == 0 {
			log.Printf("  hashed %d/%d", i+1, len(medias))
		}
	}

	// ---- 5. log in, skip what's already on the Pi
	client := &http.Client{Timeout: 0}
	jar, _ := cookiejar.New(nil)
	client.Jar = jar
	base := strings.TrimRight(*server, "/")
	onPi := map[string]bool{}
	done := map[string]bool{}
	if b, err := os.ReadFile(*stateFile); err == nil {
		for _, l := range strings.Fields(string(b)) {
			done[l] = true
		}
	}
	if !*dry || *pwFile != "" {
		pw := readPassword(*pwFile)
		if err := login(client, base, pw); err != nil {
			log.Fatalf("login: %v", err)
		}
		ids, err := existingIDs(client, base)
		if err != nil {
			log.Fatalf("reading library: %v", err)
		}
		onPi = ids
	}
	var todo []*media
	skips := map[string]int{}
	for _, m := range medias {
		switch {
		case m.skip != "":
			k := m.skip
			if strings.HasPrefix(k, "same file as") {
				k = "same file in another album"
			}
			skips[k]++
		case onPi[m.id] || done[m.id]:
			skips["already on the Pi"]++
		default:
			todo = append(todo, m)
		}
	}
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].takenMs > todo[j].takenMs }) // newest first
	total := sumSize(todo)
	log.Printf("to import: %d files, %s", len(todo), human(total))
	for k, v := range skips {
		log.Printf("  skipped %d: %s", v, k)
	}
	if *dry {
		return
	}

	// ---- 6. upload, one at a time (the Pi has one core)
	os.MkdirAll(filepath.Dir(*stateFile), 0o755)
	state, err := os.OpenFile(*stateFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer state.Close()
	var sent int64
	start := time.Now()
	stored, similar, dups, failed := 0, 0, 0, 0
	for i, m := range todo {
		var res string
		var err error
		for attempt := 1; attempt <= 4; attempt++ {
			res, err = upload(client, base, m)
			if err == nil || errors.Is(err, errRejected) {
				break
			}
			if errors.Is(err, errAuth) {
				pw := readPassword(*pwFile)
				if lerr := login(client, base, pw); lerr != nil {
					log.Fatalf("login again: %v", lerr)
				}
				continue
			}
			wait := time.Duration(attempt*attempt) * 10 * time.Second
			log.Printf("  %s: %v — retrying in %s", m.name, err, wait)
			time.Sleep(wait)
		}
		sent += m.size
		if err == nil {
			fmt.Fprintln(state, m.id)
		}
		switch {
		case err != nil:
			failed++
			log.Printf("FAILED %s (%s): %v", m.name, m.where, err)
		case res == "similar":
			similar++
		case res == "dup":
			dups++
		default:
			stored++
		}
		el := time.Since(start).Seconds()
		rate := float64(sent) / max(el, 1)
		eta := time.Duration(float64(total-sent)/max(rate, 1)) * time.Second
		log.Printf("[%d/%d] %s/%s  %.1f MB/s  ETA %s  %s → %s", i+1, len(todo), human(sent), human(total), rate/1e6, eta.Round(time.Minute), m.name, orErr(res, err))
	}
	log.Printf("done: %d imported, %d already there (same file), %d already there (same photo, other copy), %d failed, in %s",
		stored, dups, similar, failed, time.Since(start).Round(time.Minute))
	if failed > 0 {
		os.Exit(1)
	}
}

func orErr(res string, err error) string {
	if err != nil {
		return "error"
	}
	return res
}

func kindOf(ext string) string {
	switch ext {
	case "mp4", "mov", "webm", "3gp", "m4v", "mkv":
		return "video"
	}
	return "image"
}

func stem(name string) string { return strings.TrimSuffix(name, path.Ext(name)) }

func sumSize(ms []*media) (n int64) {
	for _, m := range ms {
		if m.skip == "" {
			n += m.size
		}
	}
	return
}

func human(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%d KB", n/1000)
}

// readSidecar parses a Takeout JSON sidecar. It returns the capture time and the media file name
// it belongs to: the JSON's "title" (the original name), with a "(n)" copy counter carried over
// from the sidecar's own file name ("IMG_1.jpg.supplemental-metadata(1).json" → "IMG_1(1).jpg").
func readSidecar(e entry) (int64, string, bool) {
	if e.size > 1<<20 {
		return 0, "", false
	}
	rc, err := e.src.open()
	if err != nil {
		return 0, "", false
	}
	defer rc.Close()
	var j struct {
		Title          string `json:"title"`
		PhotoTakenTime struct {
			Timestamp string `json:"timestamp"`
		} `json:"photoTakenTime"`
		CreationTime struct {
			Timestamp string `json:"timestamp"`
		} `json:"creationTime"`
	}
	if err := json.NewDecoder(rc).Decode(&j); err != nil || j.Title == "" {
		return 0, "", false // album metadata.json etc.
	}
	ts, _ := strconv.ParseInt(j.PhotoTakenTime.Timestamp, 10, 64)
	if ts <= 0 {
		ts, _ = strconv.ParseInt(j.CreationTime.Timestamp, 10, 64)
	}
	if ts <= 0 {
		return 0, "", false
	}
	name := j.Title
	if m := counterRe.FindStringSubmatch(strings.TrimSuffix(e.name, ".json")); m != nil {
		name = stem(name) + "(" + m[1] + ")" + path.Ext(name)
	}
	return ts * 1000, name, true
}

func hashOf(e entry) (string, error) {
	rc, err := e.src.open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:20], nil // the server's item id
}

func readPassword(file string) string {
	if file == "" {
		log.Fatal("-password-file is required")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		log.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PiFoto login password: "); ok {
			return strings.TrimSpace(v)
		}
	}
	return strings.TrimSpace(string(b))
}

func login(c *http.Client, base, pw string) error {
	body, _ := json.Marshal(map[string]string{"password": pw})
	resp, err := c.Post(base+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func existingIDs(c *http.Client, base string) (map[string]bool, error) {
	resp, err := c.Get(base + "/api/items")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var j struct {
		Items [][]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, r := range j.Items {
		if len(r) > 9 { // family-accounts server: only what's in my own library counts
			if f, ok := r[9].(float64); ok && int(f)&1 == 0 {
				continue
			}
		}
		if len(r) > 0 {
			if s, ok := r[0].(string); ok {
				ids[s] = true
			}
		}
	}
	return ids, nil
}

var (
	errAuth     = errors.New("logged out")
	errRejected = errors.New("rejected by the Pi")
)

// upload returns "stored", "dup" (identical file already there) or "similar" (same photo, other copy).
func upload(c *http.Client, base string, m *media) (string, error) {
	rc, err := m.src.open()
	if err != nil {
		return "", errRejected
	}
	defer rc.Close()
	q := url.Values{"name": {m.name}, "mtime": {strconv.FormatInt(m.takenMs, 10)}}
	req, _ := http.NewRequest("POST", base+"/api/upload?"+q.Encode(), rc)
	req.ContentLength = m.size
	req.Header.Set("X-PiFoto", "1")
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case 200:
		var j struct {
			Dup     bool `json:"dup"`
			Similar bool `json:"similar"`
		}
		if json.Unmarshal(b, &j) != nil {
			return "", fmt.Errorf("unexpected reply")
		}
		if j.Similar {
			return "similar", nil
		}
		if j.Dup {
			return "dup", nil
		}
		return "stored", nil
	case 401:
		return "", errAuth
	case 400, 413, 415:
		return "", fmt.Errorf("%w: %s", errRejected, strings.TrimSpace(string(b)))
	}
	return "", fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
}
