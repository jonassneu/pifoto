package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for the admin dashboard's API: sign-up links and storage limits.

const goodPw = "violet tractor seven lagoon"

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %d %s", w.Code, w.Body)
	}
	return m
}

// newLink makes a sign-up link as the owner; returns its JSON.
func newLink(t *testing.T, s *Server, tok string, body map[string]any) map[string]any {
	t.Helper()
	w := call(s.routes(""), lan("192.168.178.20"), "POST", "/api/links", body, cookie(tok)...)
	if w.Code != 200 {
		t.Fatalf("create link %v: %d %s", body, w.Code, w.Body)
	}
	return decode(t, w)
}

func TestSignupLinks(t *testing.T) {
	s, h, _ := secLibrary(t)
	_, owner := login(t, h, lan("192.168.178.20"), "", "ownerpw")
	away := funnel("203.0.113.9")

	// Only admins manage links.
	mama := addMember(t, h, owner, "Mama", goodPw)
	if w := call(h, lan("192.168.178.20"), "GET", "/api/links", nil, cookie(mama)...); w.Code != 403 {
		t.Fatalf("member lists links: %d", w.Code)
	}
	if w := call(h, lan("192.168.178.20"), "POST", "/api/links", map[string]any{"hours": 24, "max": 1}, cookie(mama)...); w.Code != 403 {
		t.Fatalf("member makes a link: %d", w.Code)
	}
	for _, bad := range []map[string]any{
		{"hours": 0, "max": 1}, {"hours": 24, "max": 0}, {"hours": 24, "max": 51}, {"hours": 24 * 91, "max": 1},
		{"hours": 24, "max": 1, "quota": -1}, {"hours": 24, "max": 1, "label": strings.Repeat("x", 41)},
	} {
		if w := call(h, lan("192.168.178.20"), "POST", "/api/links", bad, cookie(owner)...); w.Code != 400 {
			t.Fatalf("bad link %v: %d %s", bad, w.Code, w.Body)
		}
	}

	l := newLink(t, s, owner, map[string]any{"label": " Cousins ", "hours": 24 * 7, "max": 2, "quota": 5e9})
	code := l["code"].(string)
	if l["label"] != "Cousins" || l["max"] != 2.0 || l["used"] != 0.0 || l["live"] != true || l["by"] != "Jonas" || len(code) != 32 {
		t.Fatalf("link: %v", l)
	}

	// Whoever opens it learns who sent it and how much space they get, not the admin's note.
	w := call(h, away, "GET", "/api/join/"+code, nil)
	info := decode(t, w)
	if w.Code != 200 || info["open"] != true || info["by"] != "Jonas" || info["quota"] != 5e9 || strings.Contains(w.Body.String(), "Cousins") {
		t.Fatalf("join info: %d %s", w.Code, w.Body)
	}

	join := func(name, pw string) *httptest.ResponseRecorder {
		return call(h, away, "POST", "/api/join", map[string]string{"code": code, "name": name, "password": pw})
	}
	if w := join("", goodPw); w.Code != 400 || !strings.HasPrefix(w.Body.String(), "Enter your name") {
		t.Fatalf("no name: %d %s", w.Code, w.Body)
	}
	if w := join("Lena", "lena123456"); w.Code != 400 {
		t.Fatalf("weak password: %d %s", w.Code, w.Body)
	}
	if w := join("mama", goodPw); w.Code != 409 {
		t.Fatalf("taken name: %d %s", w.Code, w.Body)
	}
	w = join("  Lena   K ", goodPw)
	if w.Code != 200 {
		t.Fatalf("signup: %d %s", w.Code, w.Body)
	}
	lena := sessionOf(w)
	got := decode(t, w)
	if got["name"] != "Lena K" || got["admin"] != false || got["quota"] != 5e9 || lena == "" {
		t.Fatalf("signup reply: %v", got)
	}
	if !strings.Contains(strings.Join(w.Result().Header.Values("Set-Cookie"), ";"), deviceCookie+"=") {
		t.Fatal("signup sets no device cookie")
	}
	// She can log in with the name she chose, and is in the people list with the link's limit.
	if c, _ := login(t, h, away, "lena k", goodPw); c != 200 {
		t.Fatalf("login after signup: %d", c)
	}
	var users struct {
		Users []map[string]any
		Free  float64
		Total float64
	}
	json.Unmarshal(call(h, lan("192.168.178.20"), "GET", "/api/users", nil, cookie(owner)...).Body.Bytes(), &users)
	var found map[string]any
	for _, u := range users.Users {
		if u["name"] == "Lena K" {
			found = u
		}
	}
	if found == nil || found["via"] != l["id"] || found["quota"] != 5e9 || users.Total <= 0 {
		t.Fatalf("people list: %+v", users)
	}

	// The list shows the use; the second (last) use spends it.
	var links struct{ Links []map[string]any }
	json.Unmarshal(call(h, lan("192.168.178.20"), "GET", "/api/links", nil, cookie(owner)...).Body.Bytes(), &links)
	if len(links.Links) != 1 || links.Links[0]["used"] != 1.0 || fmt.Sprint(links.Links[0]["users"]) != "[Lena K]" {
		t.Fatalf("links: %v", links)
	}
	if w := join("Tom", goodPw); w.Code != 200 {
		t.Fatalf("second signup: %d %s", w.Code, w.Body)
	}
	if w := join("Tim", goodPw); w.Code != 404 {
		t.Fatalf("used-up link: %d %s", w.Code, w.Body)
	}
	if w := call(h, away, "GET", "/api/join/"+code, nil); w.Code != 404 {
		t.Fatalf("used-up link info: %d", w.Code)
	}
	json.Unmarshal(call(h, lan("192.168.178.20"), "GET", "/api/links", nil, cookie(owner)...).Body.Bytes(), &links)
	if links.Links[0]["live"] != false || links.Links[0]["used"] != 2.0 {
		t.Fatalf("used-up link listed as %v", links.Links[0])
	}

	// Expired and deleted links stop working.
	l2 := newLink(t, s, owner, map[string]any{"hours": 1, "max": 5})
	s.users.mu.Lock()
	s.users.findLinkID(l2["id"].(string)).Exp = time.Now().Add(-time.Second)
	s.users.mu.Unlock()
	if w := call(h, away, "GET", "/api/join/"+l2["code"].(string), nil); w.Code != 404 {
		t.Fatalf("expired link: %d", w.Code)
	}
	l3 := newLink(t, s, owner, map[string]any{"hours": 48, "max": 5})
	if w := call(h, lan("192.168.178.20"), "DELETE", "/api/links/"+l3["id"].(string), nil, cookie(owner)...); w.Code != 204 {
		t.Fatalf("delete link: %d", w.Code)
	}
	if w := call(h, lan("192.168.178.30"), "POST", "/api/join", map[string]string{"code": l3["code"].(string), "name": "Eve", "password": goodPw}); w.Code != 404 {
		t.Fatalf("deleted link still works: %d", w.Code)
	}

	// A link stops working when whoever made it is no longer an admin.
	papa := addMember(t, h, owner, "Papa", goodPw)
	var pid string
	json.Unmarshal(call(h, lan("192.168.178.20"), "GET", "/api/me", nil, cookie(papa)...).Body.Bytes(), &struct{ ID *string }{&pid})
	call(h, lan("192.168.178.20"), "PATCH", "/api/users/"+pid, map[string]any{"admin": true}, cookie(owner)...)
	l4 := newLink(t, s, papa, map[string]any{"hours": 48, "max": 5})
	if w := call(h, away, "GET", "/api/join/"+l4["code"].(string), nil); w.Code != 200 {
		t.Fatalf("link of an admin: %d", w.Code)
	}
	call(h, lan("192.168.178.20"), "PATCH", "/api/users/"+pid, map[string]any{"admin": false}, cookie(owner)...)
	if w := call(h, funnel("203.0.113.77"), "GET", "/api/join/"+l4["code"].(string), nil); w.Code != 404 {
		t.Fatalf("link of a former admin: %d", w.Code)
	}
	json.Unmarshal(call(h, lan("192.168.178.20"), "GET", "/api/links", nil, cookie(owner)...).Body.Bytes(), &links)
	if links.Links[0]["id"] != l4["id"] || links.Links[0]["live"] != false {
		t.Fatalf("former admin's link listed as %v", links.Links[0])
	}
	call(h, lan("192.168.178.20"), "DELETE", "/api/links/"+l4["id"].(string), nil, cookie(owner)...)

	// Links survive a restart; dead ones are dropped a while after they expired.
	again, err := OpenUsers(s.dir, "Jonas")
	if err != nil || len(again.Links()) != 2 {
		t.Fatalf("reload: %v %d", err, len(again.Links()))
	}
	again.mu.Lock()
	again.links[0].Exp = time.Now().Add(-linkKeep - time.Hour)
	again.pruneLinks(time.Now())
	n := len(again.links)
	again.mu.Unlock()
	if n != 1 {
		t.Fatalf("prune kept %d", n)
	}
}

func TestSignupNameProbingIsLimited(t *testing.T) {
	s, h, _ := secLibrary(t)
	_, owner := login(t, h, lan("192.168.178.20"), "", "ownerpw")
	code := newLink(t, s, owner, map[string]any{"hours": 24, "max": 5})["code"].(string)
	o := funnel("203.0.113.50")
	for i := 0; i < 5; i++ {
		if w := call(h, o, "POST", "/api/join", map[string]string{"code": code, "name": "Jonas", "password": goodPw}); w.Code != 409 {
			t.Fatalf("try %d: %d", i, w.Code)
		}
	}
	if w := call(h, o, "POST", "/api/join", map[string]string{"code": code, "name": "Somebody", "password": goodPw}); w.Code != 429 {
		t.Fatalf("after 5 taken names: %d", w.Code)
	}
}

func TestMaxUsers(t *testing.T) {
	s, h, _ := secLibrary(t)
	_, owner := login(t, h, lan("192.168.178.20"), "", "ownerpw")
	code := newLink(t, s, owner, map[string]any{"hours": 24, "max": 1})["code"].(string)
	for i := s.users.Len(); i < maxUsers; i++ {
		if _, _, err := s.users.Create(fmt.Sprintf("P%d", i), false, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.users.Create("One more", false, 0); err != errTooMany {
		t.Fatalf("create past the cap: %v", err)
	}
	if w := call(h, lan("192.168.178.30"), "POST", "/api/join", map[string]string{"code": code, "name": "Late", "password": goodPw}); w.Code != 403 {
		t.Fatalf("signup past the cap: %d %s", w.Code, w.Body)
	}
}

func mamaID(t *testing.T, h http.Handler, owner string) string {
	var users struct{ Users []map[string]any }
	json.Unmarshal(call(h, lan("192.168.178.20"), "GET", "/api/users", nil, cookie(owner)...).Body.Bytes(), &users)
	for _, u := range users.Users {
		if u["name"] == "Mama" {
			return u["id"].(string)
		}
	}
	t.Fatal("no Mama")
	return ""
}

func TestStorageLimit(t *testing.T) {
	s, h, _ := secLibrary(t)
	home := lan("192.168.178.20")
	_, owner := login(t, h, home, "", "ownerpw")

	// An admin adds Mama with a 1000-byte limit.
	w := call(h, home, "POST", "/api/users", map[string]any{"name": "Mama", "quota": 1000}, cookie(owner)...)
	if w.Code != 200 || decode(t, w)["user"].(map[string]any)["quota"] != 1000.0 {
		t.Fatalf("add with quota: %d %s", w.Code, w.Body)
	}
	var inv struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &inv)
	mama := sessionOf(call(h, home, "POST", "/api/join", map[string]string{"code": inv.Code, "password": goodPw}))

	up := func(tok, name string, data []byte) *httptest.ResponseRecorder {
		return call(h, home, "POST", "/api/upload?name="+name, data, cookie(tok)...)
	}
	chunked := func(tok, name string, data []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/upload?name="+name, bytes.NewReader(data))
		r.RemoteAddr = home.remote
		r.ContentLength = -1
		r.Header.Set("Cookie", sessionCookie+"="+tok)
		r.Header.Set("X-PiFoto", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	blob := func(n int, b byte) []byte { return []byte(strings.Repeat(string(b), n)) }
	status := func(tok string) map[string]any {
		return decode(t, call(h, home, "GET", "/api/status", nil, cookie(tok)...))
	}

	if w := up(mama, "a.mp4", blob(600, 'a')); w.Code != 200 {
		t.Fatalf("first upload: %d %s", w.Code, w.Body)
	}
	if st := status(mama); st["used"] != 600.0 || st["quota"] != 1000.0 {
		t.Fatalf("status: %v", st)
	}
	// Doesn't fit: refused with 507 (the app retries later), nothing stored, temp file gone.
	w = chunked(mama, "b.mp4", blob(600, 'b')) // the final check, after the whole file arrived
	if w.Code != 507 || !strings.Contains(w.Body.String(), "full") {
		t.Fatalf("over the limit: %d %s", w.Code, w.Body)
	}
	if st := status(mama); st["used"] != 600.0 || st["count"] != 1.0 {
		t.Fatalf("after refused upload: %v", st)
	}
	// The same file again, sent without a size (like the Android app, chunked), is still fine:
	// it's already hers and takes nothing. With a size that might not fit, it's refused up front.
	// A file she already has is still a dup when it fits in what's left; one that doesn't is
	// refused like any other (with or without a size), before more than her free space is written.
	if w := chunked(mama, "a.mp4", blob(600, 'a')); w.Code != 507 {
		t.Fatalf("own file bigger than what's left: %d %s", w.Code, w.Body)
	}
	if w := up(mama, "a.mp4", blob(600, 'a')); w.Code != 507 {
		t.Fatalf("dup with a size that doesn't fit: %d %s", w.Code, w.Body)
	}
	if w := chunked(mama, "s.mp4", blob(50, 's')); w.Code != 200 {
		t.Fatalf("small file: %d %s", w.Code, w.Body)
	}
	if w := chunked(mama, "s.mp4", blob(50, 's')); w.Code != 200 || decode(t, w)["dup"] != true {
		t.Fatalf("dup that fits: %d %s", w.Code, w.Body)
	}
	if left, _ := os.ReadDir(filepath.Join(s.dir, "tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %d", len(left))
	}
	// Two at once can't both take the space: the bytes arriving count for both.
	s.flightMu.Lock()
	s.inflight = map[string]int64{mamaID(t, h, owner): 300}
	s.flightMu.Unlock()
	if w := chunked(mama, "t.mp4", blob(100, 't')); w.Code != 507 {
		t.Fatalf("with another upload in flight: %d %s", w.Code, w.Body)
	}
	s.flightMu.Lock()
	s.inflight = nil
	s.flightMu.Unlock()
	// Bytes someone else already stored count for her too.
	if w := up(owner, "c.mp4", blob(300, 'c')); w.Code != 200 {
		t.Fatalf("owner upload: %d", w.Code)
	}
	if w := up(owner, "d.mp4", blob(500, 'd')); w.Code != 200 {
		t.Fatalf("owner upload: %d", w.Code)
	}
	if w := up(mama, "c.mp4", blob(300, 'c')); w.Code != 200 {
		t.Fatalf("claim that fits: %d %s", w.Code, w.Body)
	}
	if w := chunked(mama, "d.mp4", blob(500, 'd')); w.Code != 507 {
		t.Fatalf("claim over the limit: %d %s", w.Code, w.Body)
	}
	// Saving a Family photo to her library counts too.
	d := decode(t, up(owner, "e.mp4", blob(200, 'e')))
	eid := d["row"].([]any)[0].(string)
	call(h, home, "POST", "/api/family", map[string]any{"ids": []string{eid}, "on": true}, cookie(owner)...)
	if k := decode(t, call(h, home, "POST", "/api/keep", map[string]any{"ids": []string{eid}}, cookie(mama)...)); k["full"] != true || k["done"] != 0.0 {
		t.Fatalf("keep over the limit: %v", k)
	}

	// A known size is refused before the body is read.
	r := httptest.NewRequest("POST", "/api/upload?name=big.mp4", strings.NewReader("x"))
	r.RemoteAddr = home.remote
	r.ContentLength = 5000
	r.Header.Set("Cookie", sessionCookie+"="+mama)
	r.Header.Set("X-PiFoto", "1")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 507 {
		t.Fatalf("early refusal: %d %s", w.Code, w.Body)
	}

	// Raising the limit (or removing it) lets it in; only admins may change it.
	mid := ""
	var users struct{ Users []map[string]any }
	json.Unmarshal(call(h, home, "GET", "/api/users", nil, cookie(owner)...).Body.Bytes(), &users)
	for _, u := range users.Users {
		if u["name"] == "Mama" {
			mid = u["id"].(string)
		}
	}
	if w := call(h, home, "PATCH", "/api/users/"+mid, map[string]any{"quota": 0}, cookie(mama)...); w.Code != 403 {
		t.Fatalf("member changes own limit: %d", w.Code)
	}
	if w := call(h, home, "PATCH", "/api/users/"+mid, map[string]any{"quota": -5}, cookie(owner)...); w.Code != 400 {
		t.Fatalf("negative limit: %d", w.Code)
	}
	if w := call(h, home, "PATCH", "/api/users/"+mid, map[string]any{"quota": 2000}, cookie(owner)...); w.Code != 200 || decode(t, w)["quota"] != 2000.0 {
		t.Fatalf("raise limit: %d %s", w.Code, w.Body)
	}
	if w := up(mama, "b.mp4", blob(600, 'b')); w.Code != 200 {
		t.Fatalf("after raising: %d %s", w.Code, w.Body)
	}
	call(h, home, "PATCH", "/api/users/"+mid, map[string]any{"quota": 0}, cookie(owner)...)
	if w := up(mama, "f.mp4", blob(5000, 'f')); w.Code != 200 {
		t.Fatalf("no limit: %d %s", w.Code, w.Body)
	}
	if st := status(mama); st["quota"] != 0.0 {
		t.Fatalf("status after removing the limit: %v", st)
	}
}

func TestShares(t *testing.T) {
	st := &Store{items: map[string]*Item{
		"a": {ID: "a", Size: 10, Owners: []string{"x"}},
		"b": {ID: "b", Size: 7, Owners: []string{"x", "y"}},
		"c": {ID: "c", Size: 5, Fam: true},
	}}
	per, fam := st.Shares()
	_, total := st.Totals()
	if per["x"] != 14 || per["y"] != 3 || fam != 5 || per["x"]+per["y"]+fam != total {
		t.Fatalf("shares %v fam %d total %d", per, fam, total)
	}
}

func TestAppUpdates(t *testing.T) {
	s, h, _ := secLibrary(t)
	home := lan("192.168.178.20")
	_, owner := login(t, h, home, "", "ownerpw")
	mama := addMember(t, h, owner, "Mama", goodPw)
	ask := func(tok, hdr string) *httptest.ResponseRecorder {
		return call(h, home, "GET", "/api/app", nil, append(cookie(tok), "X-PiFoto-App", hdr)...)
	}
	if w := ask(mama, "5 1.4"); w.Code != 404 {
		t.Fatalf("nothing published: %d", w.Code)
	}
	if w := call(h, home, "GET", "/api/app", nil); w.Code != 401 {
		t.Fatalf("without login: %d", w.Code)
	}
	apk := []byte("PK fake apk bytes")
	os.MkdirAll(filepath.Join(s.dir, "app"), 0o750)
	os.WriteFile(filepath.Join(s.dir, "app", "PiFoto.apk"), apk, 0o644)
	writeJSON(filepath.Join(s.dir, "app", "PiFoto.json"), map[string]any{"versionCode": 6, "versionName": "1.4", "notes": "Storage limits"})
	w := ask(mama, "5 1.4-test")
	a := decode(t, w)
	sum := sha256.Sum256(apk)
	if w.Code != 200 || a["versionCode"] != 6.0 || a["versionName"] != "1.4" || a["size"] != float64(len(apk)) ||
		a["sha256"] != hex.EncodeToString(sum[:]) || a["notes"] != "Storage limits" {
		t.Fatalf("release: %d %s", w.Code, w.Body)
	}
	// The download is the same file anyone can get from /app/PiFoto.apk.
	if w := call(h, funnel("203.0.113.9"), "GET", "/app/PiFoto.apk", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), apk) {
		t.Fatalf("download: %d", w.Code)
	}
	// The admin page sees which version runs where; junk headers are ignored.
	ask(owner, "garbage")
	ask(owner, "4 <b>x</b>")
	var users struct {
		Users []map[string]any
		App   map[string]any
	}
	json.Unmarshal(call(h, home, "GET", "/api/users", nil, cookie(owner)...).Body.Bytes(), &users)
	if users.App["versionCode"] != 6.0 {
		t.Fatalf("app in people list: %v", users.App)
	}
	for _, u := range users.Users {
		got := fmt.Sprint(u["apps"])
		if u["name"] == "Mama" && got != "[map[code:5 name:1.4-test]]" || u["name"] == "Jonas" && got != "[map[code:4 name:]]" {
			t.Fatalf("%s apps: %s", u["name"], got)
		}
	}
	// It is kept with the session (sessions.json), so a restart doesn't forget it.
	if ss := loadSessions(filepath.Join(s.dir, "sessions.json"), ""); ss[mama].App != 5 {
		t.Fatalf("not saved: %+v", ss[mama])
	}
}

func TestAppPage(t *testing.T) {
	s, h, _ := secLibrary(t)
	away := funnel("203.0.113.9")
	page := func(ua string) *httptest.ResponseRecorder { return call(h, away, "GET", "/app", nil, "User-Agent", ua) }
	desktop := "Mozilla/5.0 (X11; Linux x86_64) Firefox/140.0"
	phone := "Mozilla/5.0 (Linux; Android 15; Pixel 8) Chrome/140.0 Mobile"
	if w := page(desktop); w.Code != 200 || !strings.Contains(w.Body.String(), "isn't on this Pi yet") {
		t.Fatalf("nothing published: %d %s", w.Code, w.Body)
	}
	if w := call(h, away, "GET", "/app/PiFoto.apk", nil); w.Code != 404 {
		t.Fatalf("apk without one: %d", w.Code)
	}
	apk := []byte("PK fake apk bytes")
	os.MkdirAll(filepath.Join(s.dir, "app"), 0o750)
	os.WriteFile(filepath.Join(s.dir, "app", "PiFoto.apk"), apk, 0o644)
	writeJSON(filepath.Join(s.dir, "app", "PiFoto.json"), map[string]any{"versionCode": 8, "versionName": "1.4.2\"x", "notes": "New <b>things</b>"})

	w := page(desktop)
	b := w.Body.String()
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(b, "Version 1.4.2&#34;x") ||
		!strings.Contains(b, "New &lt;b&gt;things&lt;/b&gt;") || !strings.Contains(b, `href="/app/PiFoto.apk"`) || strings.Contains(b, "/app/qr.png") != hasQR {
		t.Fatalf("page: %d %s", w.Code, b)
	}
	if b := page(phone).Body.String(); strings.Contains(b, "/app/qr.png") || !strings.Contains(b, "Download PiFoto") {
		t.Fatal("phone page shows the QR code, or no download button")
	}
	// The APK, with a safe versioned name; Android's HTTP client (app 1.4.2's updater) gets it at /app too.
	w = call(h, away, "GET", "/app/PiFoto.apk", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), apk) || w.Header().Get("Content-Disposition") != `attachment; filename="PiFoto-1.4.2x.apk"` {
		t.Fatalf("apk: %d %q", w.Code, w.Header().Get("Content-Disposition"))
	}
	if w := page("Dalvik/2.1.0 (Linux; U; Android 15; sdk_gphone64_x86_64)"); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), apk) {
		t.Fatalf("updater at /app: %d", w.Code)
	}
	if w := call(h, away, "GET", "/app/qr.png", nil); hasQR && (w.Code != 200 || w.Header().Get("Content-Type") != "image/png") {
		t.Fatalf("qr: %d", w.Code)
	}
}
