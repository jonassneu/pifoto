package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type client struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, c: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		r = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("X-PiFoto", "1")
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (c *client) json(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	code, out := c.do(method, path, body)
	if code != want {
		c.t.Fatalf("%s %s: HTTP %d %s (want %d)", method, path, code, out, want)
	}
	m := map[string]any{}
	json.Unmarshal(out, &m)
	return m
}

// rows returns id -> [flags, sharedBy] of everything the client sees.
func (c *client) rows() map[string][2]any {
	c.t.Helper()
	m := c.json("GET", "/api/items", nil, 200)
	out := map[string][2]any{}
	for _, r := range m["items"].([]any) {
		row := r.([]any)
		out[row[0].(string)] = [2]any{int(row[9].(float64)), row[10]}
	}
	return out
}

func (c *client) upload(name string, data []byte) map[string]any {
	c.t.Helper()
	return c.json("POST", "/api/upload?name="+name, data, 200)
}

// startServer mirrors main(): users (with upgrade from a single password), sessions, store.
func startServer(t *testing.T, dir string) (*Server, *httptest.Server) {
	for _, d := range []string{"originals", "thumbs", "previews", "tmp", "trash"} {
		os.MkdirAll(filepath.Join(dir, d), 0o750)
	}
	users, err := OpenUsers(dir, "Jonas")
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := users.Owner()
	if !ok {
		t.Fatal("no owner after upgrade")
	}
	s := &Server{dir: dir, users: users}
	s.initLimits()
	s.sessions = loadSessions(filepath.Join(dir, "sessions.json"), owner.ID)
	if s.store, err = OpenStore(dir); err != nil {
		t.Fatal(err)
	}
	s.store.SetNames(users.Name)
	reconcile(dir, s.store)
	s.store.AssignUnowned(owner.ID)
	s.thumbs = NewWorker(s)
	hs := httptest.NewServer(s.routes(""))
	t.Cleanup(hs.Close)
	return s, hs
}

func TestFamily(t *testing.T) {
	dir := t.TempDir()
	// A library from before family accounts: one password, an old-style session, one photo.
	c := Config{Salt: randHex(16), Iter: 1000}
	c.Hash = hashPw("ownerpw", c)
	writeJSON(filepath.Join(dir, "config.json"), c)
	writeJSON(filepath.Join(dir, "sessions.json"), map[string]time.Time{"oldtoken": time.Now().Add(time.Hour)})
	old := Item{ID: "0123456789abcdef0123", Name: "old.jpg", Ext: "jpg", Kind: "image", Taken: time.Now(), Added: time.Now(),
		Rel: "originals/2026/01/0123456789abcdef0123.jpg"}
	writeJSON(filepath.Join(dir, "index.json"), []Item{old})
	os.MkdirAll(filepath.Join(dir, "originals/2026/01"), 0o750)
	os.WriteFile(filepath.Join(dir, old.Rel), []byte("x"), 0o640)

	_, hs := startServer(t, dir)

	// The old session still works and belongs to the owner, who owns the old photo.
	legacy := newClient(t, hs.URL)
	u, _ := http.NewRequest("GET", hs.URL, nil)
	legacy.c.Jar.SetCookies(u.URL, []*http.Cookie{{Name: sessionCookie, Value: "oldtoken"}})
	if me := legacy.json("GET", "/api/me", nil, 200); me["name"] != "Jonas" || me["admin"] != true {
		t.Fatalf("legacy session: %v", me)
	}
	if r := legacy.rows(); r[old.ID][0] != RowMine {
		t.Fatalf("old photo not in owner's library: %v", r)
	}

	// Owner logs in without a name (old apps, the importer) and with it (any case).
	jonas := newClient(t, hs.URL)
	jonas.json("POST", "/api/login", map[string]string{"password": "ownerpw"}, 200)
	jonas2 := newClient(t, hs.URL)
	jonas2.json("POST", "/api/login", map[string]string{"name": " jonas ", "password": "ownerpw"}, 200)
	if code, _ := newClient(t, hs.URL).do("POST", "/api/login", map[string]string{"name": "Jonas", "password": "nope"}); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}

	// Invite Mama and a kid; they choose their passwords through the invite link.
	join := func(name string) *client {
		inv := jonas.json("POST", "/api/users", map[string]any{"name": name}, 200)
		code := inv["code"].(string)
		cl := newClient(t, hs.URL)
		if info := cl.json("GET", "/api/join/"+code, nil, 200); info["name"] != name {
			t.Fatalf("join info %v", info)
		}
		cl.json("POST", "/api/join", map[string]string{"code": code, "password": "violet harbor " + name}, 200)
		if code2, _ := newClient(t, hs.URL).do("POST", "/api/join", map[string]string{"code": code, "password": "again123"}); code2 != 404 {
			t.Fatalf("invite reusable: %d", code2)
		}
		return cl
	}
	mama, kid := join("Mama"), join("Kid")
	jonas.json("POST", "/api/users", map[string]any{"name": "mama"}, 409) // names are unique
	if code, _ := mama.do("GET", "/api/users", nil); code != 403 {
		t.Fatalf("non-admin listed users: %d", code)
	}
	newClient(t, hs.URL).json("POST", "/api/login", map[string]string{"name": "kid", "password": "violet harbor Kid"}, 200)

	// Mama's photo is private to her.
	photo := jpegWithExif(t, 64, 48, 1, "2025:07:01 10:00:00", "")
	r := mama.upload("IMG_1.jpg", photo)
	id := r["row"].([]any)[0].(string)
	if r["dup"] != false || r["row"].([]any)[9] != float64(RowMine) {
		t.Fatalf("mama upload %v", r)
	}
	if _, ok := jonas.rows()[id]; ok {
		t.Fatal("jonas sees mama's private photo")
	}
	for _, k := range []string{"thumb", "preview", "orig"} {
		if code, _ := jonas.do("GET", "/media/"+k+"/"+id, nil); code != 404 {
			t.Fatalf("jonas fetched mama's %s: %d", k, code)
		}
	}
	if code, _ := mama.do("GET", "/media/orig/"+id, nil); code != 200 {
		t.Fatalf("mama can't fetch own photo: %d", code)
	}

	// Jonas uploads the same bytes: it lands in his library, stored once.
	r = jonas.upload("copy.jpg", photo)
	if r["dup"] != false || r["row"].([]any)[0] != id {
		t.Fatalf("jonas same bytes %v", r)
	}
	if r = jonas.upload("copy.jpg", photo); r["dup"] != true {
		t.Fatalf("second upload not dup %v", r)
	}

	// Similar detection only within your own library.
	other := jpegWithExif(t, 64, 48, 1, "2025:07:01 10:00:00", "")
	other = append(other[:len(other)-2], 0, 0xFF, 0xD9) // different bytes, same name + moment
	if r = kid.upload("IMG_1.jpg", other); r["dup"] != false {
		t.Fatalf("kid's photo matched mama's by name: %v", r)
	}
	if r = mama.upload("IMG_1.jpg", other); r["dup"] != true || r["similar"] != true {
		t.Fatalf("mama's recompressed copy not similar: %v", r)
	}

	// Mama shares to Family: everybody sees it, with her name.
	seqKid := kid.json("GET", "/api/status", nil, 200)["seq"].(float64)
	mama.json("POST", "/api/family", map[string]any{"ids": []string{id}, "on": true}, 200)
	if got := mama.rows()[id]; got[0] != RowMine|RowFamily|RowSharedMe {
		t.Fatalf("mama view %v", got)
	}
	if got := kid.rows()[id]; got[0] != RowFamily || got[1] != "Mama" {
		t.Fatalf("kid view %v", got)
	}
	if got := jonas.rows()[id]; got[0] != RowMine|RowFamily {
		t.Fatalf("jonas view %v", got)
	}
	st := kid.json("GET", "/api/status?since="+jsonNum(seqKid), nil, 200)
	if ch := st["changes"].([]any); len(ch) != 1 || ch[0].(map[string]any)["op"] != "add" || st["family"] != float64(1) {
		t.Fatalf("kid changes %v", st)
	}
	if code, _ := kid.do("GET", "/media/orig/"+id, nil); code != 200 {
		t.Fatalf("kid can't open family photo: %d", code)
	}
	// The kid can't delete it or unshare it, but can save it to their library.
	if code, _ := kid.do("DELETE", "/api/items/"+id, nil); code != 404 {
		t.Fatalf("kid deleted family photo: %d", code)
	}
	if m := kid.json("POST", "/api/family", map[string]any{"ids": []string{id}, "on": false}, 200); m["failed"] != float64(1) || m["failedIds"].([]any)[0] != id {
		t.Fatalf("kid unshared: %v", m)
	}
	kid.json("POST", "/api/keep", map[string]any{"ids": []string{id}}, 200)
	if got := kid.rows()[id]; got[0] != RowMine|RowFamily {
		t.Fatalf("kept %v", got)
	}

	// Mama deletes it from her library: it leaves Family (she shared it), the others keep it.
	if code, _ := mama.do("DELETE", "/api/items/"+id, nil); code != 204 {
		t.Fatalf("mama delete: %d", code)
	}
	if _, ok := mama.rows()[id]; ok {
		t.Fatal("still in mama's view")
	}
	if got := jonas.rows()[id]; got[0] != RowMine {
		t.Fatalf("jonas after mama deleted %v", got)
	}
	jonas.do("DELETE", "/api/items/"+id, nil)
	it, _ := mustGet(t, hs, dir, id)
	if !fileExists(filepath.Join(dir, it.Rel)) {
		t.Fatal("file trashed while the kid still has it")
	}
	kid.do("DELETE", "/api/items/"+id, nil)
	if fileExists(filepath.Join(dir, it.Rel)) || !fileExists(filepath.Join(dir, "trash", id+".jpg")) {
		t.Fatal("last owner deleted, file not in trash")
	}

	// Admin: rename, reset (new invite logs them out), remove.
	users := jonas.json("GET", "/api/users", nil, 200)["users"].([]any)
	ids := map[string]string{}
	for _, x := range users {
		m := x.(map[string]any)
		ids[m["name"].(string)] = m["id"].(string)
	}
	jonas.json("PATCH", "/api/users/"+ids["Mama"], map[string]any{"name": "Mom"}, 200)
	if me := mama.json("GET", "/api/me", nil, 200); me["name"] != "Mom" {
		t.Fatalf("rename: %v", me)
	}
	jonas.json("PATCH", "/api/users/"+ids["Jonas"], map[string]any{"admin": false}, 400)
	jonas.json("POST", "/api/users/"+ids["Mama"]+"/invite", nil, 200)
	if code, _ := mama.do("GET", "/api/me", nil); code != 401 {
		t.Fatalf("reset didn't log out: %d", code)
	}
	// kid shares a photo, then is removed: the shared photo stays in Family
	kr := kid.upload("kid.jpg", jpegWithExif(t, 32, 32, 1, "2025:08:01 10:00:00", ""))
	kidID := kr["row"].([]any)[0].(string)
	kid.json("POST", "/api/family", map[string]any{"ids": []string{kidID}, "on": true}, 200)
	jonas.json("DELETE", "/api/users/"+ids["Kid"], nil, 200)
	if code, _ := kid.do("GET", "/api/me", nil); code != 401 {
		t.Fatal("removed kid still logged in")
	}
	if got := jonas.rows()[kidID]; got[0] != RowFamily {
		t.Fatalf("kid's shared photo after removal %v", got)
	}
	// an admin can take it out of Family; nobody has it then, so it's trashed
	jonas.json("POST", "/api/family", map[string]any{"ids": []string{kidID}, "on": false}, 200)
	if _, ok := jonas.rows()[kidID]; ok || !fileExists(filepath.Join(dir, "trash", kidID+".jpg")) {
		t.Fatal("orphaned family photo not trashed")
	}
	jonas.json("DELETE", "/api/users/"+ids["Jonas"], nil, 400) // can't remove yourself

	// Everything survives a restart (users, sessions, ownership).
	time.Sleep(10 * time.Millisecond)
	_, hs2 := startServer(t, dir)
	j2 := &client{t: t, base: hs2.URL, c: jonas.c}
	if me := j2.json("GET", "/api/me", nil, 200); me["name"] != "Jonas" {
		t.Fatalf("after restart %v", me)
	}
}

func jsonNum(f float64) string { b, _ := json.Marshal(uint64(f)); return string(b) }

// mustGet reads an item straight from the index file the server keeps (via a fresh store).
func mustGet(t *testing.T, hs *httptest.Server, dir, id string) (Item, bool) {
	t.Helper()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	it, ok := st.Get(id)
	if ok {
		return it, true
	}
	// not flushed yet: the original is at a predictable place
	m, _ := filepath.Glob(filepath.Join(dir, "originals", "*", "*", id+".*"))
	if len(m) == 1 {
		rel, _ := filepath.Rel(dir, m[0])
		return Item{ID: id, Rel: rel, Ext: strings.TrimPrefix(filepath.Ext(m[0]), ".")}, true
	}
	t.Fatalf("item %s not found", id)
	return Item{}, false
}
