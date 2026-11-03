package main

import (
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Android app updates. tools/publish-app.sh puts the release APK at <data>/app/PiFoto.apk and
// next to it PiFoto.json ({"versionCode","versionName","notes"}). Apps from 1.4 on ask
// /api/app every few hours, download GET /app when the versionCode is newer and install it
// themselves (Android checks that it is signed with the same key). They report their own
// version with that request, so the admin page can show who still runs an old one.

type appRelease struct {
	VersionCode int    `json:"versionCode"`
	VersionName string `json:"versionName"`
	Notes       string `json:"notes,omitempty"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Published   int64  `json:"published"` // unix time the APK was put there
}

type appSum struct {
	size int64
	mod  time.Time
	sum  string
}

// appRelease is the published app, if there is one with a version file.
func (s *Server) appRelease() (appRelease, bool) {
	dir := filepath.Join(s.dir, "app")
	var a appRelease
	if readJSON(filepath.Join(dir, "PiFoto.json"), &a) != nil || a.VersionCode <= 0 {
		return appRelease{}, false
	}
	apk := filepath.Join(dir, "PiFoto.apk")
	st, err := os.Stat(apk)
	if err != nil || st.Size() == 0 {
		return appRelease{}, false
	}
	s.appMu.Lock()
	defer s.appMu.Unlock()
	if s.appSum.size != st.Size() || !s.appSum.mod.Equal(st.ModTime()) {
		f, err := os.Open(apk)
		if err != nil {
			return appRelease{}, false
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return appRelease{}, false
		}
		s.appSum = appSum{size: st.Size(), mod: st.ModTime(), sum: hex.EncodeToString(h.Sum(nil))}
	}
	a.Size, a.SHA256, a.Published = st.Size(), s.appSum.sum, st.ModTime().Unix()
	return a, true
}

// ---------- the download page (GET /app) ----------

var appPageTmpl = template.Must(template.ParseFS(webFS, "web/app.html"))

// hasQR: web/install-qr.png (served as /app/qr.png) is a QR code of <-remote-url>/app. It is not
// part of the published source (it encodes this Pi's public address); without it the download
// page shows only the address.
var hasQR = func() bool { _, err := webFS.ReadFile("web/install-qr.png"); return err == nil }()

// handleAppPage shows the newest app with a download button, what's new and how to install it.
// Android's own HTTP client (user agent "Dalvik/…") gets the APK instead: app 1.4.2 downloads its
// updates from /app itself (later versions use /app/PiFoto.apk), and so did links from before the page.
func (s *Server) handleAppPage(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.UserAgent(), "Dalvik/") {
		s.serveAPK(w, r)
		return
	}
	data := struct {
		Has, Android, QR                        bool
		Version, Size, Date, Notes, File, QRURL string
	}{Android: strings.Contains(r.UserAgent(), "Android"), QR: hasQR, File: "PiFoto.apk", QRURL: r.Host + "/app"}
	if u := strings.TrimRight(s.remote, "/"); u != "" {
		data.QRURL = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://") + "/app"
	}
	if a, ok := s.appRelease(); ok {
		data.Has, data.Version, data.Notes = true, a.VersionName, strings.TrimSpace(a.Notes)
		data.Size, data.Date = fmtGB(a.Size), time.Unix(a.Published, 0).Format("2 Jan 2006")
		data.File = apkName(a.VersionName)
	} else if st, err := os.Stat(filepath.Join(s.dir, "app", "PiFoto.apk")); err == nil && st.Size() > 0 {
		data.Has, data.Size, data.Date = true, fmtGB(st.Size()), st.ModTime().Format("2 Jan 2006") // no version file
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := appPageTmpl.Execute(w, data); err != nil {
		log.Printf("app page: %v", err)
	}
}

// apkName is the download's file name, "PiFoto-1.4.2.apk" (only safe characters from the version).
func apkName(version string) string {
	v := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, version)
	if v == "" || len(v) > 20 {
		return "PiFoto.apk"
	}
	return "PiFoto-" + v + ".apk"
}

// serveAPK sends the published app (with Range support, for resumed downloads).
func (s *Server) serveAPK(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(filepath.Join(s.dir, "app", "PiFoto.apk"))
	if err != nil {
		http.Error(w, "The Android app hasn't been put on this Pi yet.", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	name := "PiFoto.apk"
	if a, ok := s.appRelease(); ok {
		name = apkName(a.VersionName)
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// handleApp tells the app which version is published (404: none), and notes which version this
// device runs (header X-PiFoto-App: "<versionCode> <versionName>").
func (s *Server) handleApp(w http.ResponseWriter, r *http.Request, u User) {
	if code, name, ok := parseAppHeader(r.Header.Get("X-PiFoto-App")); ok {
		s.noteApp(r, code, name)
	}
	w.Header().Set("Cache-Control", "no-store")
	a, ok := s.appRelease()
	if !ok {
		http.Error(w, "no app published", http.StatusNotFound)
		return
	}
	writeJSONResp(w, a)
}

func parseAppHeader(h string) (code int, name string, ok bool) {
	f := strings.Fields(h)
	if len(f) == 0 || len(f) > 2 {
		return 0, "", false
	}
	code, err := strconv.Atoi(f[0])
	if err != nil || code <= 0 || code > 1e9 {
		return 0, "", false
	}
	if len(f) == 2 {
		name = f[1]
		if len(name) > 20 || strings.Trim(name, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ.-_") != "" {
			name = ""
		}
	}
	return code, name, true
}

// noteApp stores the app version on this device's session (sessions.json is written only when it
// changes: once per update, not on every check).
func (s *Server) noteApp(r *http.Request, code int, name string) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	ss, ok := s.sessions[c.Value]
	if !ok || ss.App == code && ss.AppV == name {
		return
	}
	ss.App, ss.AppV = code, name
	s.sessions[c.Value] = ss
	s.saveSessions()
}

type appSeen struct {
	Code int    `json:"code"`
	Name string `json:"name"`
}

// appsByUser: the app versions on each person's logged-in devices, oldest first.
func (s *Server) appsByUser() map[string][]appSeen {
	out := map[string][]appSeen{}
	now := time.Now()
	s.sessMu.Lock()
	for _, ss := range s.sessions {
		if ss.App > 0 && now.Before(ss.Exp) {
			out[ss.U] = append(out[ss.U], appSeen{ss.App, ss.AppV})
		}
	}
	s.sessMu.Unlock()
	for _, a := range out {
		sort.Slice(a, func(i, j int) bool { return a[i].Code < a[j].Code })
	}
	return out
}
