package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- helpers: where a request comes from ----------

// origin is how a request reaches the server: directly from the LAN, or through tailscale
// serve/Funnel (from 127.0.0.1, with the proxy's headers).
type origin struct {
	remote string
	hdr    map[string]string
}

func lan(ip string) origin { return origin{remote: net.JoinHostPort(ip, "50000")} }

func tailnet(ip string) origin {
	return origin{remote: "127.0.0.1:50000", hdr: map[string]string{"X-Forwarded-For": ip, "X-Forwarded-Proto": "https"}}
}

func funnel(ip string) origin {
	o := tailnet(ip)
	o.hdr["Tailscale-Funnel-Request"] = "?1"
	return o
}

// call runs one request through the whole handler. body: nil, []byte, or something to send as JSON.
func call(h http.Handler, o origin, method, path string, body any, hdr ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = o.remote
	for k, v := range o.hdr {
		r.Header.Set(k, v)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func sessionOf(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

func cookie(tok string) []string {
	return []string{"Cookie", sessionCookie + "=" + tok, "X-PiFoto", "1"}
}

// secLibrary: a library whose owner Jonas has the password "ownerpw" (set before the rules
// existed), plus the handler and server.
func secLibrary(t *testing.T) (*Server, http.Handler, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	c := Config{Salt: randHex(16), Iter: 1000}
	c.Hash = hashPw("ownerpw", c)
	writeJSON(filepath.Join(dir, "config.json"), c)
	s, hs := startServer(t, dir)
	return s, s.routes(""), hs
}

func login(t *testing.T, h http.Handler, o origin, name, pw string) (int, string) {
	t.Helper()
	w := call(h, o, "POST", "/api/login", map[string]string{"name": name, "password": pw}, "Content-Type", "application/json")
	return w.Code, sessionOf(w)
}

// addMember invites name as the owner and joins with pw; returns their session.
func addMember(t *testing.T, h http.Handler, ownerTok, name, pw string) string {
	t.Helper()
	w := call(h, lan("192.168.178.20"), "POST", "/api/users", map[string]any{"name": name}, cookie(ownerTok)...)
	if w.Code != 200 {
		t.Fatalf("invite %s: %d %s", name, w.Code, w.Body)
	}
	var inv struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &inv)
	w = call(h, lan("192.168.178.21"), "POST", "/api/join", map[string]string{"code": inv.Code, "password": pw})
	if w.Code != 200 {
		t.Fatalf("join %s: %d %s", name, w.Code, w.Body)
	}
	return sessionOf(w)
}

func me(h http.Handler, tok string) int {
	return call(h, lan("192.168.178.20"), "GET", "/api/me", nil, cookie(tok)...).Code
}

// ---------- S1: home and tailnet vs internet ----------

func TestIsPublic(t *testing.T) {
	cases := []struct {
		o    origin
		want bool
	}{
		{lan("192.168.178.20"), false},
		{lan("10.1.2.3"), false},
		{lan("172.16.5.4"), false},
		{lan("172.32.0.1"), true}, // just outside 172.16/12
		{lan("169.254.10.10"), false},
		{lan("100.101.102.103"), false}, // tailnet, direct over WireGuard
		{lan("fe80::1%eth0"), false},
		{lan("fd00::1"), false},
		{lan("::ffff:192.168.178.20"), false},
		{lan("2001:db8::1"), true},
		{lan("203.0.113.9"), true},
		{tailnet("100.101.102.103"), false}, // tailscale serve
		{tailnet("fd7a:115c:a1e0::6739:f927"), false},
		{origin{remote: "127.0.0.1:50000"}, false}, // something on the Pi itself
		{tailnet("203.0.113.9"), true},             // public address, even without the Funnel mark
		{funnel("203.0.113.9"), true},
		{funnel("100.101.102.103"), true}, // marked by Funnel: public whatever the address
		{origin{remote: "127.0.0.1:50000", hdr: map[string]string{"Tailscale-Funnel-Request": "?1"}}, true},
		{origin{remote: "192.168.178.20:50000", hdr: map[string]string{"X-Forwarded-For": "203.0.113.9"}}, false}, // only the local proxy is believed
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.o.remote
		for k, v := range c.o.hdr {
			r.Header.Set(k, v)
		}
		if got := isPublic(r); got != c.want {
			t.Errorf("isPublic(%s %v) = %v, want %v", c.o.remote, c.o.hdr, got, c.want)
		}
	}

	// The LAN has global IPv6 addresses too: a direct client in our own /64 is at home.
	local := &net.TCPAddr{IP: net.ParseIP("2001:db8:1:2::10"), Port: 80}
	for remote, want := range map[string]bool{"[2001:db8:1:2:aaaa::99]:5000": false, "[2001:db8:9:9::99]:5000": true} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
		if got := isPublic(r); got != want {
			t.Errorf("isPublic(%s on %s) = %v, want %v", remote, local, got, want)
		}
	}
}

// A name-less login means the owner only at home and on the tailnet; from the internet it is
// refused without counting against the owner.
func TestNamelessLoginOnlyAtHome(t *testing.T) {
	_, h, _ := secLibrary(t)
	for i := 0; i < 15; i++ {
		w := call(h, funnel(fmt.Sprintf("203.0.113.%d", i+1)), "POST", "/api/login", map[string]string{"password": "ownerpw"})
		if w.Code != 400 || !strings.Contains(w.Body.String(), "Enter your name") {
			t.Fatalf("name-less login over Funnel: %d %q", w.Code, w.Body)
		}
	}
	if code, _ := login(t, h, funnel("203.0.113.99"), "Jonas", "ownerpw"); code != 200 {
		t.Fatalf("owner locked by name-less attempts: %d", code)
	}
	for _, o := range []origin{lan("192.168.178.20"), tailnet("100.101.102.103"), tailnet("fd7a:115c:a1e0::1")} {
		if code, tok := login(t, h, o, "", "ownerpw"); code != 200 || tok == "" {
			t.Fatalf("name-less login from %v: %d", o, code)
		}
	}
	// takeout-import: JSON, no X-PiFoto, from the LAN
	w := call(h, lan("192.168.178.40"), "POST", "/api/login", []byte(`{"password":"ownerpw"}`), "Content-Type", "application/json")
	if w.Code != 200 || sessionOf(w) == "" {
		t.Fatalf("importer login: %d %s", w.Code, w.Body)
	}
}

// Many wrong passwords from the internet lock an account for the internet only; wrong
// passwords at home don't lock it at all (only the guessing address).
func TestAccountLockOnlyFromInternet(t *testing.T) {
	_, h, _ := secLibrary(t)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			login(t, h, funnel(fmt.Sprintf("198.51.100.%d", i+1)), "Jonas", "wrong")
		}(i)
	}
	wg.Wait()
	if code, _ := login(t, h, funnel("198.51.100.200"), "jonas", "ownerpw"); code != 429 {
		t.Fatalf("account not locked for the internet: %d", code)
	}
	for _, o := range []origin{lan("192.168.178.20"), tailnet("100.101.102.103")} {
		if code, _ := login(t, h, o, "Jonas", "ownerpw"); code != 200 {
			t.Fatalf("family locked out at home (%v): %d", o, code)
		}
	}

	// Fresh library: a dozen wrong passwords from the LAN (different devices) lock nothing...
	_, h, _ = secLibrary(t)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			login(t, h, lan(fmt.Sprintf("192.168.178.%d", 100+i)), "Jonas", "wrong")
		}(i)
	}
	wg.Wait()
	if code, _ := login(t, h, funnel("198.51.100.1"), "Jonas", "ownerpw"); code != 200 {
		t.Fatalf("LAN failures locked the account: %d", code)
	}
	// ...but one LAN device guessing is still limited, and a correct login in between doesn't reset it.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); login(t, h, lan("192.168.178.30"), "Mama", "guess") }()
	}
	wg.Wait()
	if code, _ := login(t, h, lan("192.168.178.30"), "Jonas", "ownerpw"); code != 200 {
		t.Fatalf("own login: %d", code)
	}
	login(t, h, lan("192.168.178.30"), "Mama", "guess")
	if code, _ := login(t, h, lan("192.168.178.30"), "Jonas", "ownerpw"); code != 429 {
		t.Fatalf("guessing LAN address not limited: %d", code)
	}
}

func TestLongLoginName(t *testing.T) {
	_, h, _ := secLibrary(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	if code, _ := login(t, h, funnel("203.0.113.5"), strings.Repeat("ä", 65), "x"); code != 400 {
		t.Fatalf("65-rune name: %d", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("over-long name was logged: %q", buf.String())
	}
	name := strings.Repeat("b", 64)
	if code, _ := login(t, h, funnel("203.0.113.5"), name, "x"); code != 401 {
		t.Fatalf("64-rune name: %d", code)
	}
	if strings.Contains(buf.String(), name[:41]) || !strings.Contains(buf.String(), name[:40]) {
		t.Fatalf("name not cut to 40 in the log: %q", buf.String())
	}
}

// The limiter's memory stays bounded under address rotation.
func TestLimiterBounded(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < 10000; i++ { // one phone rotating through its /64
		l.fail(limitKey(fmt.Sprintf("2001:db8:1:2:%x:%x:1:1", i>>8, i&0xff)))
	}
	if l.size() != 1 || !l.blocked(limitKey("2001:db8:1:2::77")) || l.blocked(limitKey("2001:db8:1:3::77")) {
		t.Fatalf("a /64 isn't one key: %d keys", l.size())
	}
	if len(l.fails[limitKey("2001:db8:1:2::1")]) != l.max {
		t.Fatalf("a key keeps %d failures, want %d", len(l.fails[limitKey("2001:db8:1:2::1")]), l.max)
	}
	if limitKey("fd7a:115c:a1e0::1") == limitKey("fd7a:115c:a1e0::2") || limitKey("192.168.178.20") != "192.168.178.20" {
		t.Fatal("tailnet and LAN addresses must stay exact")
	}

	// Many networks: capped; the key whose last failure is oldest goes, one under attack stays.
	l = newLoginLimiter()
	for i := 0; i < l.max; i++ {
		l.fail("victim")
	}
	l.fail("first")
	for i := 0; i < limiterMaxKeys+500; i++ {
		l.fail(limitKey(fmt.Sprintf("2001:db8:%x::1", i)))
		if i%500 == 0 {
			l.fail("victim")
		}
	}
	if l.size() > limiterMaxKeys {
		t.Fatalf("%d keys, cap %d", l.size(), limiterMaxKeys)
	}
	if _, ok := l.fails["first"]; ok {
		t.Fatal("oldest key not evicted")
	}
	if !l.blocked("victim") {
		t.Fatal("the key under attack was evicted")
	}

	// Expired keys go.
	l = newLimiter(5, 30*time.Millisecond)
	for i := 0; i < 100; i++ {
		l.fail(fmt.Sprint(i))
	}
	time.Sleep(40 * time.Millisecond)
	l.fail("new")
	if l.size() != 1 {
		t.Fatalf("expired keys kept: %d", l.size())
	}
}

// ---------- S2: sessions ----------

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	_, h, _ := secLibrary(t)
	_, a := login(t, h, lan("192.168.178.20"), "Jonas", "ownerpw")
	_, b := login(t, h, lan("192.168.178.22"), "Jonas", "ownerpw")
	mama := addMember(t, h, a, "Mama", "violet harbor mama")

	w := call(h, lan("192.168.178.20"), "POST", "/api/password", map[string]string{"old": "ownerpw", "new": "correct horse battery"}, cookie(a)...)
	a2 := sessionOf(w)
	if w.Code != 204 || a2 == "" || a2 == a {
		t.Fatalf("password change: %d, new session %q", w.Code, a2)
	}
	if me(h, a2) != 200 || me(h, a) != 401 || me(h, b) != 401 {
		t.Fatalf("sessions after change: this %d, old cookie %d, other device %d", me(h, a2), me(h, a), me(h, b))
	}
	if me(h, mama) != 200 {
		t.Fatal("someone else was logged out")
	}
	if code, _ := login(t, h, lan("192.168.178.23"), "Jonas", "ownerpw"); code != 401 {
		t.Fatalf("old password still works: %d", code)
	}
	if code, _ := login(t, h, lan("192.168.178.23"), "Jonas", "correct horse battery"); code != 200 {
		t.Fatalf("new password: %d", code)
	}
}

func TestLogoutAll(t *testing.T) {
	_, h, _ := secLibrary(t)
	_, a := login(t, h, lan("192.168.178.20"), "Jonas", "ownerpw")
	_, b := login(t, h, lan("192.168.178.22"), "Jonas", "ownerpw")
	mama := addMember(t, h, a, "Mama", "violet harbor mama")

	// plain logout (no body, like older apps): only this device
	if w := call(h, lan("192.168.178.22"), "POST", "/api/logout", nil, cookie(b)...); w.Code != 204 {
		t.Fatalf("logout: %d", w.Code)
	}
	if me(h, b) != 401 || me(h, a) != 200 {
		t.Fatal("plain logout ended the wrong sessions")
	}
	_, b = login(t, h, lan("192.168.178.22"), "Jonas", "ownerpw")
	if w := call(h, lan("192.168.178.20"), "POST", "/api/logout", map[string]bool{"all": true}, cookie(a)...); w.Code != 204 {
		t.Fatalf("logout all: %d", w.Code)
	}
	if me(h, a) != 401 || me(h, b) != 401 {
		t.Fatal("logout all left a session")
	}
	if me(h, mama) != 200 {
		t.Fatal("logout all ended someone else's session")
	}
}

func TestPasswordChangeLimits(t *testing.T) {
	_, h, _ := secLibrary(t)
	_, jonas := login(t, h, lan("192.168.178.20"), "Jonas", "ownerpw")
	mama := addMember(t, h, jonas, "Mama", "violet harbor mama")
	change := func(tok string, o origin, old string) int {
		return call(h, o, "POST", "/api/password", map[string]string{"old": old, "new": "correct horse battery"}, cookie(tok)...).Code
	}
	for i := 0; i < 5; i++ { // a stolen session guessing from several addresses
		if code := change(jonas, funnel(fmt.Sprintf("203.0.113.%d", i+1)), "guess"); code != 403 {
			t.Fatalf("wrong old password: %d", code)
		}
	}
	if code := change(jonas, funnel("203.0.113.50"), "ownerpw"); code != 429 {
		t.Fatalf("per-account limit missing: %d", code)
	}
	if code := change(mama, lan("192.168.178.21"), "violet harbor mama"); code != 204 {
		t.Fatalf("other account affected: %d", code)
	}

	// Hashing goes through hashSlots: when they're all busy, the change waits, then gives up.
	old := hashWait
	hashWait = 50 * time.Millisecond
	t.Cleanup(func() { hashWait = old })
	_, h, _ = secLibrary(t)
	_, jonas2 := login(t, h, lan("192.168.178.25"), "Jonas", "ownerpw")
	fillHashSlots(t)
	if code := call(h, lan("192.168.178.25"), "POST", "/api/password", map[string]string{"old": "ownerpw", "new": "another fine password"}, cookie(jonas2)...).Code; code != 503 {
		t.Fatalf("password change with hashing busy: %d", code)
	}
}

// fillHashSlots takes all hashing slots until the test ends.
func fillHashSlots(t *testing.T) {
	for i := 0; i < cap(hashSlots); i++ {
		hashSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(hashSlots); i++ {
			<-hashSlots
		}
	})
}

// ---------- S3: password rules ----------

func TestPasswordPolicy(t *testing.T) {
	cases := []struct {
		pw, name string
		want     error
	}{
		{"short123", "Mama", errPwShort},
		{"äöüäöüäöü", "Mama", errPwShort}, // 18 bytes, but 9 characters
		{"aaaaaaaaaaaa", "Mama", errPwWeak},
		{"1234567890", "Mama", errPwWeak},
		{"0987654321", "Mama", errPwWeak},
		{"12345678901234567890", "Mama", errPwWeak},
		{"abcdefghij", "Mama", errPwWeak},
		{"QWERTZUIOP", "Mama", errPwWeak},
		{"qwertyuiop", "Mama", errPwWeak},
		{"asdfghjkl!", "Mama", errPwWeak},
		{"qwertzuiopqwertzuiop", "Mama", errPwWeak},
		{"asdfasdfasdf", "Mama", errPwWeak},
		{"hahahahahaha", "Mama", errPwWeak},
		{"abcdef1234", "Mama", errPwWeak},
		{"Password123", "Mama", errPwWeak},  // on the list
		{"password1234", "Mama", errPwWeak}, // a listed word with another ending
		{"Lassmichrein", "Mama", errPwWeak}, // German list
		{"lassmichrein2024", "Mama", errPwWeak},
		{"Mama123456", "Mama", errPwName},
		{"mama2024!!", "Mama", errPwName},
		{"MamaMamaMama", "Mama", errPwWeak}, // a short piece repeated
		{"jonas1234567890", "Jonas", errPwName},
		{"Oma Erika 1234", "Oma Erika", errPwName},
		{"erikaerika12", "Oma Erika", errPwName},
		{"violet harbor mama", "Mama", nil},
		{"Jonas is my favourite brother", "Jonas", nil},
		{"correct horse battery staple", "Mama", nil},
		{"Grüße aus Köln", "Mama", nil},
		{"Tr0ub4dor&3xyz", "Mama", nil},
		{"8302917465", "Mama", nil},
		{"jo12398765x", "Jo", nil}, // names under 3 letters aren't checked
	}
	for _, c := range cases {
		if got := pwPolicy(c.pw, c.name); got != c.want {
			t.Errorf("pwPolicy(%q, %q) = %v, want %v", c.pw, c.name, got, c.want)
		}
	}
}

func TestPasswordRulesInTheAPI(t *testing.T) {
	_, h, _ := secLibrary(t)
	_, jonas := login(t, h, lan("192.168.178.20"), "Jonas", "ownerpw")
	w := call(h, lan("192.168.178.20"), "POST", "/api/users", map[string]any{"name": "Mama"}, cookie(jonas)...)
	var inv struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &inv)
	for pw, want := range map[string]error{"kurz": errPwShort, "1234567890": errPwWeak, "Mama123456": errPwName} {
		w := call(h, lan("192.168.178.21"), "POST", "/api/join", map[string]string{"code": inv.Code, "password": pw})
		if w.Code != 400 || strings.TrimSpace(w.Body.String()) != want.Error() {
			t.Errorf("join with %q: %d %q", pw, w.Code, w.Body)
		}
	}
	if w := call(h, lan("192.168.178.21"), "POST", "/api/join", map[string]string{"code": inv.Code, "password": "violet harbor mama"}); w.Code != 200 {
		t.Fatalf("join with a good password: %d %s", w.Code, w.Body)
	}

	_, other := login(t, h, lan("192.168.178.22"), "Jonas", "ownerpw")
	w = call(h, lan("192.168.178.20"), "POST", "/api/password", map[string]string{"old": "ownerpw", "new": "jonas12345"}, cookie(jonas)...)
	if w.Code != 400 || strings.TrimSpace(w.Body.String()) != errPwName.Error() || sessionOf(w) != "" {
		t.Fatalf("weak new password: %d %q", w.Code, w.Body)
	}
	if me(h, other) != 200 || me(h, jonas) != 200 {
		t.Fatal("a refused change logged devices out")
	}
}

// -set-password takes any password (the way back in, and local development) but warns.
func TestCLISetPassword(t *testing.T) {
	dir := t.TempDir()
	users, err := OpenUsers(dir, "Owner")
	if err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(dir, "sessions.json")
	var warn bytes.Buffer
	u, err := cliSetPassword(users, sess, "Jonas", "Owner", "devpass", &warn)
	if err != nil || u.Name != "Jonas" || !u.Admin {
		t.Fatalf("devpass refused: %v %+v", err, u)
	}
	if !strings.Contains(warn.String(), "warning") {
		t.Fatalf("no warning for a short password: %q", warn.String())
	}
	if x, _ := users.ByName("jonas"); !CheckPassword(x, "devpass") {
		t.Fatal("devpass not set")
	}
	warn.Reset()
	if _, err := cliSetPassword(users, sess, "Jonas", "Owner", "correct horse battery", &warn); err != nil || warn.Len() != 0 {
		t.Fatalf("good password: %v, warning %q", err, warn.String())
	}
	if _, err := cliSetPassword(users, sess, "", "Owner", "", &warn); err != errPwEmpty {
		t.Fatalf("empty password: %v", err)
	}
}

// ---------- S4: invites are checked before any hashing ----------

func TestJoinChecksCodeFirst(t *testing.T) {
	_, h, _ := secLibrary(t)
	_, jonas := login(t, h, lan("192.168.178.20"), "Jonas", "ownerpw")
	w := call(h, lan("192.168.178.20"), "POST", "/api/users", map[string]any{"name": "Mama"}, cookie(jonas)...)
	var inv struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &inv)

	old := hashWait
	hashWait = 50 * time.Millisecond
	t.Cleanup(func() { hashWait = old })
	fillHashSlots(t) // nothing can be hashed now
	join := func(o origin, code, pw string) int {
		return call(h, o, "POST", "/api/join", map[string]string{"code": code, "password": pw}).Code
	}
	if code := join(funnel("203.0.113.1"), strings.Repeat("ab", 16), "violet harbor mama"); code != 404 {
		t.Fatalf("bad code with hashing busy: %d (it must not need a hash)", code)
	}
	if code := join(funnel("203.0.113.2"), inv.Code, "1234567890"); code != 400 {
		t.Fatalf("weak password with a valid code: %d (rules come before hashing)", code)
	}
	if code := join(funnel("203.0.113.2"), inv.Code, "violet harbor mama"); code != 503 {
		t.Fatalf("valid code with hashing busy: %d", code)
	}
	// bad codes count against the address
	for i := 0; i < 4; i++ {
		join(funnel("203.0.113.1"), fmt.Sprintf("%032x", i), "violet harbor mama")
	}
	if code := join(funnel("203.0.113.1"), inv.Code, "violet harbor mama"); code != 429 {
		t.Fatalf("bad codes not counted: %d", code)
	}
}

// ---------- S5: read deadlines ----------

func TestReadDeadlines(t *testing.T) {
	oldRead, oldIdle := readTimeout, uploadIdle
	readTimeout, uploadIdle = 300*time.Millisecond, 600*time.Millisecond
	t.Cleanup(func() { readTimeout, uploadIdle = oldRead, oldIdle })
	s, _, hs := secLibrary(t)

	cl := newClient(t, hs.URL)
	cl.c.Transport = &http.Transport{MaxConnsPerHost: 1}
	cl.json("POST", "/api/login", map[string]string{"password": "ownerpw"}, 200)
	reused := false
	do := func(req *http.Request) (*http.Response, error) {
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}))
		req.Header.Set("X-PiFoto", "1")
		return cl.c.Do(req)
	}

	// Slow but steady: 1.5 s of body, five times readTimeout, never idle for long.
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 15; i++ {
			pw.Write(bytes.Repeat([]byte{byte(i)}, 1000))
			time.Sleep(100 * time.Millisecond)
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", hs.URL+"/api/upload?name=slow.mp4", pr)
	resp, err := do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("slow upload: %d %s", resp.StatusCode, body)
	}
	var up struct{ Row []any }
	json.Unmarshal(body, &up)
	if it, _ := s.store.Get(up.Row[0].(string)); it.Size != 15000 {
		t.Fatalf("stored %d bytes, want 15000", it.Size)
	}

	// The same connection serves the next requests, also after waiting longer than readTimeout.
	for _, wait := range []time.Duration{0, 400 * time.Millisecond} {
		time.Sleep(wait)
		req, _ = http.NewRequest("POST", hs.URL+"/api/keep", strings.NewReader(`{"ids":[]}`))
		resp, err = do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !reused {
			t.Fatalf("after %v: %d, connection reused %v", wait, resp.StatusCode, reused)
		}
	}

	// Stalled bodies are cut: an upload after uploadIdle, anything else after readTimeout.
	stalled := func(path string) (time.Duration, int) {
		pr, pw := io.Pipe()
		defer pw.Close()
		go pw.Write([]byte(`{"ids":`))
		stop := time.AfterFunc(5*time.Second, func() { pw.CloseWithError(errors.New("never cut")) }) // fail, don't hang
		defer stop.Stop()
		req, _ := http.NewRequest("POST", hs.URL+path, pr)
		req.Header.Set("X-PiFoto", "1")
		c := &http.Client{Jar: cl.c.Jar, Transport: &http.Transport{}}
		t0 := time.Now()
		resp, err := c.Do(req)
		if err != nil {
			return time.Since(t0), 0
		}
		resp.Body.Close()
		return time.Since(t0), resp.StatusCode
	}
	if d, code := stalled("/api/upload?name=stuck.mp4"); d < uploadIdle/2 || d > uploadIdle+time.Second || (code != 0 && code != 500) {
		t.Fatalf("stalled upload: cut after %v with %d", d, code)
	}
	if d, code := stalled("/api/keep"); d < readTimeout/2 || d > readTimeout+time.Second || (code != 0 && code != 400) {
		t.Fatalf("stalled body: cut after %v with %d", d, code)
	}
}

// ---------- S6: HSTS ----------

func TestHSTS(t *testing.T) {
	_, h, _ := secLibrary(t)
	for _, c := range []struct {
		o    origin
		want string
	}{
		{tailnet("100.101.102.103"), "max-age=31536000"},
		{funnel("203.0.113.9"), "max-age=31536000"},
		{lan("192.168.178.20"), ""},
		{origin{remote: "192.168.178.20:5000", hdr: map[string]string{"X-Forwarded-Proto": "https"}}, ""}, // not from the proxy
		{origin{remote: "127.0.0.1:5000"}, ""},
	} {
		if got := call(h, c.o, "GET", "/robots.txt", nil).Header().Get("Strict-Transport-Security"); got != c.want {
			t.Errorf("%v: HSTS %q, want %q", c.o, got, c.want)
		}
	}
	r := httptest.NewRequest("GET", "https://raspberrypi.example/robots.txt", nil) // TLS set
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Error("no HSTS over direct TLS")
	}
}

// ---------- S7: login CSRF ----------

func TestLoginRefusesFormBodies(t *testing.T) {
	_, h, _ := secLibrary(t)
	body := []byte(`{"password":"ownerpw"}`)
	for _, ct := range []string{"application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "text/plain;charset=UTF-8", " Text/Plain "} {
		if w := call(h, lan("192.168.178.20"), "POST", "/api/login", body, "Content-Type", ct); w.Code != 415 {
			t.Errorf("login as %q: %d", ct, w.Code)
		}
		if w := call(h, lan("192.168.178.20"), "POST", "/api/join", body, "Content-Type", ct); w.Code != 415 {
			t.Errorf("join as %q: %d", ct, w.Code)
		}
	}
	for _, ct := range []string{"", "application/json", "application/json; charset=utf-8"} {
		var hdr []string
		if ct != "" {
			hdr = []string{"Content-Type", ct}
		}
		if w := call(h, lan("192.168.178.20"), "POST", "/api/login", body, hdr...); w.Code != 200 {
			t.Errorf("login with Content-Type %q: %d", ct, w.Code)
		}
	}
}

// ---------- S8: hello ----------

func TestHello(t *testing.T) {
	s, _, hs := secLibrary(t)
	cl := newClient(t, hs.URL)
	cl.json("POST", "/api/login", map[string]string{"password": "ownerpw"}, 200)
	u, _ := http.NewRequest("GET", hs.URL, nil)
	var tok string
	for _, c := range cl.c.Jar.Cookies(u.URL) {
		if c.Name == sessionCookie {
			tok = c.Value
		}
	}
	sum := func(tok string) string { h := sha256.Sum256([]byte(tok)); return hex.EncodeToString(h[:]) }
	hello := func(s, n string, xff ...string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", hs.URL+"/api/hello?s="+s+"&n="+n, nil) // no cookie
		if len(xff) > 0 {
			req.Header.Set("X-Forwarded-For", xff[0])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	nonce := strings.Repeat("0123456789abcdef", 2)
	resp, body := hello(sum(tok), nonce)
	m := hmac.New(sha256.New, []byte(tok))
	m.Write([]byte("pifoto-hello:" + nonce))
	var got struct{ Mac string }
	if err := json.Unmarshal([]byte(body), &got); err != nil || resp.StatusCode != 200 || got.Mac != hex.EncodeToString(m.Sum(nil)) {
		t.Fatalf("hello: %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("hello headers: %v", resp.Header)
	}
	for _, n := range []string{strings.Repeat("a", 33), strings.Repeat("f", 64)} {
		if resp, _ := hello(sum(tok), n); resp.StatusCode != 200 {
			t.Errorf("nonce of %d: %d", len(n), resp.StatusCode)
		}
	}

	s.sessMu.Lock()
	s.sessions["expiredtoken"] = Session{U: "x", Exp: time.Now().Add(-time.Minute)}
	s.sessMu.Unlock()
	for _, x := range []string{sum("nope"), sum("expiredtoken")} {
		if resp, _ := hello(x, nonce); resp.StatusCode != 404 || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("unknown session: %d", resp.StatusCode)
		}
	}
	for _, c := range [][2]string{{strings.ToUpper(sum(tok)), nonce}, {sum(tok)[:63], nonce}, {sum(tok), nonce[:31]},
		{sum(tok), strings.Repeat("a", 65)}, {sum(tok), strings.Repeat("g", 32)}, {"", ""}} {
		if resp, _ := hello(c[0], c[1]); resp.StatusCode != 400 {
			t.Errorf("hello?s=%s&n=%s: %d, want 400", c[0], c[1], resp.StatusCode)
		}
	}

	// misses are limited per address; others carry on
	for i := 0; i < 30; i++ {
		hello(sum(fmt.Sprint(i)), nonce, "192.168.178.50")
	}
	if resp, _ := hello(sum(tok), nonce, "192.168.178.50"); resp.StatusCode != 429 {
		t.Fatalf("hello misses not limited: %d", resp.StatusCode)
	}
	if resp, _ := hello(sum(tok), nonce, "100.64.0.51"); resp.StatusCode != 200 {
		t.Fatalf("other address limited: %d", resp.StatusCode)
	}

	// Never over the internet: a fake home address could relay the challenge there and pass the
	// answer back. (The tailnet through tailscale serve still gets an answer, above.)
	if resp, _ := hello(sum(tok), nonce, "203.0.113.50"); resp.StatusCode != 404 {
		t.Fatalf("hello from an internet address: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", hs.URL+"/api/hello?s="+sum(tok)+"&n="+nonce, nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.52")
	req.Header.Set("Tailscale-Funnel-Request", "?1")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 404 {
		t.Fatalf("hello through Funnel: %v %v", resp, err)
	}

	cl.json("POST", "/api/logout", nil, 204)
	if resp, _ := hello(sum(tok), nonce); resp.StatusCode != 404 {
		t.Fatalf("hello after logout: %d", resp.StatusCode)
	}
}

// ---------- S9: media ----------

// fakeDjpeg puts a djpeg that never finishes first in PATH.
func fakeDjpeg(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "djpeg"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := djpegTimeout
	djpegTimeout = 300 * time.Millisecond
	t.Cleanup(func() { djpegTimeout = old })
}

func TestDjpegTimeout(t *testing.T) {
	fakeDjpeg(t)
	s := newTestServer(t)
	os.MkdirAll(filepath.Join(s.dir, "originals", "2026"), 0o750)
	os.WriteFile(filepath.Join(s.dir, "originals", "2026", "a.jpg"), jpegWithExif(t, 64, 48, 1, "2026:01:01 10:00:00", ""), 0o640)
	var pb bytes.Buffer
	png.Encode(&pb, image.NewRGBA(image.Rect(0, 0, 32, 24)))
	os.WriteFile(filepath.Join(s.dir, "originals", "2026", "b.png"), pb.Bytes(), 0o640)

	t0 := time.Now()
	err := makeImage(filepath.Join(s.dir, "originals", "2026", "a.jpg"), "jpg", thumbMax, 75, filepath.Join(t.TempDir(), "x.jpg"))
	if !errors.Is(err, errDjpegTimeout) || time.Since(t0) > 5*time.Second {
		t.Fatalf("stuck djpeg: %v after %v", err, time.Since(t0))
	}

	// The worker marks the stuck one failed and goes on with the next.
	for _, it := range []Item{{ID: "aaaaaaaaaaaaaaaaaaaa", Ext: "jpg", Rel: "originals/2026/a.jpg"}, {ID: "bbbbbbbbbbbbbbbbbbbb", Ext: "png", Rel: "originals/2026/b.png"}} {
		it.Kind, it.Taken, it.Added = "image", time.Now(), time.Now()
		s.store.Add(it)
		s.thumbs.Queue(it.ID)
	}
	s.thumbs.Start()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, _ := s.store.Get("aaaaaaaaaaaaaaaaaaaa")
		b, _ := s.store.Get("bbbbbbbbbbbbbbbbbbbb")
		n, cur := s.thumbs.Pending()
		if a.Thumb == ThumbFailed && b.Thumb == ThumbOK && n == 0 && cur == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker stuck: a %+v, b %+v, queue %d %q", a, b, n, cur)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pngHeader is just enough of a PNG for DecodeConfig: signature and IHDR.
func pngHeader(w, h int, depth, colorType, interlace byte) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9], ihdr[12] = depth, colorType, interlace
	chunk := append([]byte("IHDR"), ihdr...)
	b := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0d"), chunk...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(chunk))
}

// The decode cap counts bytes per pixel: a small 16-bit or interlaced PNG can't make the
// server allocate far more than an ordinary one.
func TestDecodeMemoryCap(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name                     string
		w, h                     int
		depth, colorType, interl byte
		tooLarge                 bool
	}{
		{"8-bit RGBA 8 MP", 4000, 2000, 8, 6, 0, false},
		{"16-bit RGBA 8 MP", 4000, 2000, 16, 6, 0, true},
		{"interlaced 8-bit RGBA 8 MP", 4000, 2000, 8, 6, 1, true},
		{"16-bit gray 10 MP", 4000, 2500, 16, 0, 0, false},
		{"8-bit RGB 13 MP", 4000, 3250, 8, 2, 0, true}, // over the pixel cap
		{"16-bit RGB 5 MP", 2500, 2000, 16, 2, 0, false},
	}
	for _, c := range cases {
		p := filepath.Join(dir, "x.png")
		os.WriteFile(p, pngHeader(c.w, c.h, c.depth, c.colorType, c.interl), 0o600)
		err := makeImage(p, "png", thumbMax, 75, filepath.Join(dir, "out.jpg"))
		if (err == errTooLarge) != c.tooLarge {
			t.Errorf("%s: %v, too large expected %v", c.name, err, c.tooLarge)
		}
	}
}

// ---------- review follow-ups: device cookies, hashing slots, password length ----------

func deviceOf(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == deviceCookie {
			return c.Value
		}
	}
	return ""
}

// A phone the person logged in on before isn't locked out by strangers guessing their name.
func TestDeviceCookieSkipsAccountLock(t *testing.T) {
	_, h, _ := secLibrary(t)
	_, jonas := login(t, h, lan("192.168.178.20"), "Jonas", "ownerpw")
	addMember(t, h, jonas, "Mama", "violet harbor mama")
	w := call(h, funnel("198.51.100.77"), "POST", "/api/login", map[string]string{"name": "Mama", "password": "violet harbor mama"}, "Content-Type", "application/json")
	dev := deviceOf(w)
	if w.Code != 200 || dev == "" {
		t.Fatalf("login: %d, device cookie %q", w.Code, dev)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == deviceCookie && (c.Path != "/api/login" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode) {
			t.Fatalf("device cookie attributes: %+v", c)
		}
	}
	parallel(10, func(i int) { // strangers lock Mama's account
		login(t, h, funnel(fmt.Sprintf("203.0.113.%d", i+1)), "Mama", "wrong")
	})
	if code, _ := login(t, h, funnel("198.51.100.78"), "Mama", "violet harbor mama"); code != 429 {
		t.Fatalf("account not locked without a device cookie: %d", code)
	}
	withDev := func(o origin, name, pw, devCookie string) int {
		return call(h, o, "POST", "/api/login", map[string]string{"name": name, "password": pw},
			"Content-Type", "application/json", "Cookie", deviceCookie+"="+devCookie).Code
	}
	if code := withDev(funnel("198.51.100.79"), "Mama", "violet harbor mama", dev); code != 200 {
		t.Fatalf("Mama's own phone locked out: %d", code)
	}
	// It is Mama's: it doesn't open Jonas's lock, and a forged one does nothing.
	parallel(10, func(i int) {
		login(t, h, funnel(fmt.Sprintf("203.0.114.%d", i+1)), "Jonas", "wrong")
	})
	if code := withDev(funnel("198.51.100.80"), "Jonas", "ownerpw", dev); code != 429 {
		t.Fatalf("Mama's device cookie unlocked Jonas: %d", code)
	}
	forged := dev[:len(dev)-1] + map[bool]string{true: "0", false: "1"}[dev[len(dev)-1] != '0']
	if code := withDev(funnel("198.51.100.81"), "Mama", "violet harbor mama", forged); code != 429 {
		t.Fatalf("forged device cookie accepted: %d", code)
	}
	// A stolen device cookie still only allows a few guesses.
	parallel(5, func(i int) {
		withDev(funnel(fmt.Sprintf("203.0.115.%d", i+1)), "Mama", "guess", dev)
	})
	if code := withDev(funnel("198.51.100.82"), "Mama", "violet harbor mama", dev); code != 429 {
		t.Fatalf("guessing with a device cookie not limited: %d", code)
	}
}

// The internet can use at most one hashing slot, so a flood from out there doesn't make logins
// at home fail.
func TestHashSlotKeptForHome(t *testing.T) {
	old := hashWait
	hashWait = 50 * time.Millisecond
	t.Cleanup(func() { hashWait = old })
	if !acquireHash(true) {
		t.Fatal("first public slot")
	}
	defer releaseHash(true)
	if acquireHash(true) {
		releaseHash(true)
		t.Fatal("a second public request got a slot")
	}
	if !acquireHash(false) {
		t.Fatal("home request found no slot")
	}
	releaseHash(false)
}

func TestPasswordLength(t *testing.T) {
	if err := pwPolicy(strings.Repeat("a", 80000)+"1x9", "Jonas"); err != errPwLong {
		t.Fatalf("80k characters: %v", err)
	}
	start := time.Now()
	pwPolicy(strings.Repeat("ab", 128)+"q", "Jonas")
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("policy check took %v", d)
	}
	if err := pwPolicy(strings.Repeat("ä", 256), "Jonas"); err == errPwLong {
		t.Fatal("256 runes counted as too long")
	}
	_, h, _ := secLibrary(t)
	_, jonas := login(t, h, lan("192.168.178.20"), "Jonas", "ownerpw")
	w := call(h, lan("192.168.178.20"), "POST", "/api/password", map[string]string{"old": "ownerpw", "new": strings.Repeat("x", 300)}, cookie(jonas)...)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "at most 256") {
		t.Fatalf("long new password: %d %s", w.Code, w.Body)
	}
	if code, _ := login(t, h, lan("192.168.178.21"), "Jonas", strings.Repeat("y", 1<<19)); code != 401 {
		t.Fatalf("huge login password: %d", code)
	}
}

// parallel runs f(0..n-1) at once and waits (failed logins each take a second).
func parallel(n int, f func(int)) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); f(i) }(i)
	}
	wg.Wait()
}
