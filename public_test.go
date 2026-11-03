package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Behind tailscale serve / funnel every request arrives from 127.0.0.1; the proxy says who it is.
func TestClientIPBehindProxy(t *testing.T) {
	cases := []struct {
		remote, xff, proto, wantIP string
		https                      bool
	}{
		{"127.0.0.1:5000", "203.0.113.7", "https", "203.0.113.7", true},
		{"127.0.0.1:5000", "6.6.6.6, 203.0.113.7", "https", "203.0.113.7", true}, // the proxy's own entry is last
		{"[::1]:5000", "2001:db8::1", "", "2001:db8::1", false},
		{"127.0.0.1:5000", "not-an-ip", "", "127.0.0.1", false},
		{"127.0.0.1:5000", "", "", "127.0.0.1", false},
		{"192.168.178.20:5000", "203.0.113.7", "https", "192.168.178.20", false}, // LAN clients can't fake it
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		if got := clientIP(r); got != c.wantIP {
			t.Errorf("clientIP(%s, xff %q) = %s, want %s", c.remote, c.xff, got, c.wantIP)
		}
		if got := isHTTPS(r); got != c.https {
			t.Errorf("isHTTPS(%s, proto %q) = %v, want %v", c.remote, c.proto, got, c.https)
		}
	}
}

func TestPublicLogin(t *testing.T) {
	dir := t.TempDir()
	c := Config{Salt: randHex(16), Iter: 1000}
	c.Hash = hashPw("ownerpw", c)
	writeJSON(filepath.Join(dir, "config.json"), c)
	_, hs := startServer(t, dir)

	login := func(name, pw, ip, proto string) *http.Response {
		b, _ := json.Marshal(map[string]string{"name": name, "password": pw})
		req, _ := http.NewRequest("POST", hs.URL+"/api/login", bytes.NewReader(b))
		req.Header.Set("X-Forwarded-For", ip) // the test client connects over loopback, like the proxy
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// Over HTTPS the session cookie is Secure; on the plain-HTTP home address it can't be.
	if r := login("Jonas", "ownerpw", "203.0.113.1", "https"); r.StatusCode != 200 || !strings.Contains(r.Header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("https login: %d %q", r.StatusCode, r.Header.Get("Set-Cookie"))
	}
	if r := login("Jonas", "ownerpw", "203.0.113.1", ""); r.StatusCode != 200 || strings.Contains(r.Header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("http login: %d %q", r.StatusCode, r.Header.Get("Set-Cookie"))
	}

	// One address guessing gets blocked, without blocking everyone else behind the proxy.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); login("Jonas", "wrong", "203.0.113.66", "https") }()
	}
	wg.Wait()
	if r := login("Jonas", "ownerpw", "203.0.113.66", "https"); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("guessing IP not blocked: %d", r.StatusCode)
	}
	if r := login("Jonas", "ownerpw", "203.0.113.2", "https"); r.StatusCode != 200 {
		t.Fatalf("other IP blocked too: %d", r.StatusCode)
	}

	// Guessing one account from many addresses: after 10 failures that account takes no new logins
	// for a while (5 above + 5 more here); other accounts are unaffected.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); login("jonas", "wrong", fmt.Sprintf("198.51.100.%d", i), "https") }(i)
	}
	wg.Wait()
	if r := login("Jonas", "ownerpw", "198.51.100.200", "https"); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("account not locked after 10 failures from many IPs: %d", r.StatusCode)
	}
	if r := login("", "ownerpw", "198.51.100.201", "https"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("name-less login (the owner) from the internet: %d, want 400", r.StatusCode)
	}
	if r := login("Someone", "x", "198.51.100.202", "https"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other names affected: %d", r.StatusCode)
	}
}

func TestRobotsAndIcons(t *testing.T) {
	dir := t.TempDir()
	c := Config{Salt: randHex(16), Iter: 1000}
	c.Hash = hashPw("ownerpw", c)
	writeJSON(filepath.Join(dir, "config.json"), c)
	_, hs := startServer(t, dir)
	for path, ctype := range map[string]string{"/robots.txt": "text/plain", "/apple-touch-icon.png": "image/png",
		"/icon-192.png": "image/png", "/icon-512.png": "image/png", "/icon-maskable.png": "image/png"} {
		resp, err := http.Get(hs.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if resp.Header.Get("X-Robots-Tag") == "" {
			t.Errorf("%s: no X-Robots-Tag", path)
		}
	}
}

// Through the proxy (slow link) the photo list is gzipped; on the home network it is not.
func TestItemsGzipOnlyThroughProxy(t *testing.T) {
	dir := t.TempDir()
	c := Config{Salt: randHex(16), Iter: 1000}
	c.Hash = hashPw("ownerpw", c)
	writeJSON(filepath.Join(dir, "config.json"), c)
	s, hs := startServer(t, dir)
	cl := newClient(t, hs.URL)
	cl.json("POST", "/api/login", map[string]string{"password": "ownerpw"}, 200)
	cl.upload("a.jpg", []byte("\xff\xd8\xff\xe0 not really a jpeg"))
	owner, _ := s.users.Owner()
	plain := s.store.ListJSON(owner.ID)

	get := func(remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/items", nil)
		r.RemoteAddr = remote
		r.Header.Set("Accept-Encoding", "gzip, deflate, br")
		w := httptest.NewRecorder()
		s.handleItems(w, r, owner)
		return w
	}
	lan := get("192.168.178.20:4000")
	if lan.Header().Get("Content-Encoding") != "" || !bytes.Equal(lan.Body.Bytes(), plain) {
		t.Fatalf("LAN got %q encoding", lan.Header().Get("Content-Encoding"))
	}
	unzip := func(w *httptest.ResponseRecorder) []byte {
		t.Helper()
		if w.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("proxied request not gzipped")
		}
		zr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(zr)
		return b
	}
	if b := unzip(get("127.0.0.1:4000")); !bytes.Equal(b, plain) {
		t.Fatal("gzipped list differs")
	}
	// After a change the list is new, and so is the compressed copy.
	cl.upload("b.jpg", []byte("\xff\xd8\xff\xe0 another one"))
	if b := unzip(get("127.0.0.1:4000")); !bytes.Equal(b, s.store.ListJSON(owner.ID)) || bytes.Equal(b, plain) {
		t.Fatal("stale gzipped list after a change")
	}

	// The page itself is compressed for everyone who accepts it.
	req, _ := http.NewRequest("GET", hs.URL+"/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("page not gzipped")
	}
}
