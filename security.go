package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- who is asking: home network and tailnet, or the internet ----------

// privateNets can only be this machine's neighbours: the home network, link-local, and the
// tailnet (Tailscale gives out 100.64.0.0/10 and fd7a:115c:a1e0::/48, inside fc00::/7).
var privateNets = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
		"100.64.0.0/10", "fc00::/7", "fe80::/10"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func parseIP(s string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.WithZone("").Unmap(), true // a zone (fe80::1%eth0) would match no prefix
}

func isPrivateIP(s string) bool {
	ip, ok := parseIP(s)
	if !ok {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, p := range privateNets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// isPublic: the request comes from the internet (Tailscale Funnel), not from home or the
// tailnet. Funnel marks its requests; tailnet requests through `tailscale serve` arrive the same
// way (from 127.0.0.1) but without the mark and with a tailnet address in X-Forwarded-For. A
// direct IPv6 connection from our own /64 is the home network too (the FritzBox hands out
// global IPv6 addresses on the LAN), however public the address looks.
func isPublic(r *http.Request) bool {
	if r.Header.Get("Tailscale-Funnel-Request") != "" {
		return true
	}
	ip := clientIP(r)
	return !isPrivateIP(ip) && !sameLink(r, ip)
}

// sameLink: a direct (not proxied) IPv6 client in the /64 of the address it reached us on.
// Nobody outside the LAN can complete a TCP handshake from inside our /64.
func sameLink(r *http.Request, client string) bool {
	if fromProxy(r) {
		return false
	}
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if la == nil {
		return false
	}
	h, _, err := net.SplitHostPort(la.String())
	if err != nil {
		return false
	}
	l, ok1 := parseIP(h)
	c, ok2 := parseIP(client)
	if !ok1 || !ok2 || !l.Is6() || !c.Is6() {
		return false
	}
	p, err := l.Prefix(64)
	return err == nil && p.Contains(c)
}

// limitKey is what the per-IP limiters count by. A whole IPv6 /64 is one key: every home and
// phone gets a /64 and can pick any address in it, so counting single addresses would let one
// guesser rotate forever (and fill the limiter's memory). Tailnet and LAN addresses stay exact:
// the tailnet puts all its devices in one /64.
func limitKey(ip string) string {
	a, ok := parseIP(ip)
	if !ok || !a.Is6() || isPrivateIP(ip) {
		return ip
	}
	p, _ := a.Prefix(64)
	return p.String()
}

// ---------- body read deadlines ----------

var (
	// readTimeout bounds reading a request body (headers have ReadHeaderTimeout), so a client that
	// stops sending in the middle can't hold a connection and its goroutine forever.
	readTimeout = 60 * time.Second
	// uploadIdle: uploads of big videos over Funnel (~100 KB/s) take hours, so they get no overall
	// limit, only this much time without a single byte arriving.
	uploadIdle = 2 * time.Minute
)

// readDeadlines gives every request with a body readTimeout to send it. Requests without a body
// get none: there is nothing to read, and a deadline left on the connection would make the
// server's background read fail during a long response (a video streaming for minutes).
func readDeadlines(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			rc := http.NewResponseController(w)
			rc.SetReadDeadline(time.Now().Add(readTimeout))
			r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc}
		}
		h.ServeHTTP(w, r)
	})
}

// deadlineBody lifts the read deadline once the body has been read to the end (the response may
// take longer than the body did). With idle set, it also pushes the deadline idle into the
// future while bytes keep coming (only every idle/8: Read runs for every few KB).
type deadlineBody struct {
	io.ReadCloser
	rc     *http.ResponseController
	idle   time.Duration
	pushed time.Time
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	if b.idle > 0 {
		if now := time.Now(); now.Sub(b.pushed) >= b.idle/8 {
			b.rc.SetReadDeadline(now.Add(b.idle))
			b.pushed = now
		}
	}
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// idleDeadline switches a request from readTimeout to a rolling uploadIdle deadline.
func idleDeadline(w http.ResponseWriter, r *http.Request) {
	r.Body = &deadlineBody{ReadCloser: r.Body, rc: http.NewResponseController(w), idle: uploadIdle}
}

// ---------- login CSRF ----------

// formBody: the request body has a type that any web site can make a browser send here without
// asking first (a plain HTML form, or fetch without a CORS preflight). Login and join only take
// JSON, so another site can't log a browser in to an account of its choosing.
func formBody(r *http.Request) bool {
	mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "application/x-www-form-urlencoded", "multipart/form-data", "text/plain":
		return true
	}
	return false
}

// ---------- hello: is this address really our server? ----------

// handleHello lets the app check a plain-HTTP home address before it sends its session there
// (anything could answer on someone else's Wi-Fi). The app sends the SHA-256 of its session token
// and a nonce, without the session; only a server that knows the token can answer with
// HMAC-SHA256(token, "pifoto-hello:" + nonce).
//
// Only at home and on the tailnet: the public https address is proven by its certificate and
// never needs this. Answering from the internet would let a fake home address pass the check by
// relaying the app's challenge to the public address and passing the answer back.
func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if isPublic(r) {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	sum, nonce := q.Get("s"), q.Get("n")
	if !lowerHex(sum, 64, 64) || !lowerHex(nonce, 32, 64) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key := limitKey(clientIP(r))
	if s.helloLim.blocked(key) {
		http.Error(w, "too many attempts, wait a few minutes", http.StatusTooManyRequests)
		return
	}
	want, _ := hex.DecodeString(sum)
	tok, ok := s.sessionBySum(want)
	if !ok {
		s.helloLim.fail(key)
		http.NotFound(w, r)
		return
	}
	m := hmac.New(sha256.New, []byte(tok))
	io.WriteString(m, "pifoto-hello:"+nonce)
	writeJSONResp(w, map[string]string{"mac": hex.EncodeToString(m.Sum(nil))})
}

// sessionBySum finds the unexpired session whose token hashes to sum. It looks at every
// session, so the time taken doesn't say how close a guess was.
func (s *Server) sessionBySum(sum []byte) (string, bool) {
	now := time.Now()
	tok, found := "", false
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	for t, ss := range s.sessions {
		h := sha256.Sum256([]byte(t))
		if subtle.ConstantTimeCompare(h[:], sum) == 1 && now.Before(ss.Exp) {
			tok, found = t, true
		}
	}
	return tok, found
}

// lowerHex: s is lowercase hex, min to max characters long.
func lowerHex(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ---------- trusted devices: the internet lock doesn't lock out phones that logged in before ----------

// deviceCookie marks a device that once logged in as someone (OWASP's "device cookies"). Wrong
// passwords from the internet lock an account for a while (accLim), so a stranger who knows a
// name could keep that person from logging in again. A login that brings the person's device
// cookie skips that lock and is limited per device instead (devLim), so a stolen cookie still
// only allows a few guesses. It opens nothing by itself: the password is still needed.
// Value: <user id>.<32 hex nonce>.<HMAC-SHA256(device.key, "pifoto-device:" + id + "." + nonce)>.
const deviceCookie = "pf_d"

func (s *Server) deviceKey() []byte {
	s.devOnce.Do(func() {
		path := filepath.Join(s.dir, "device.key")
		if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
			s.devKey = b
			return
		}
		s.devKey = make([]byte, 32)
		rand.Read(s.devKey)
		if s.dir != "" {
			if err := os.WriteFile(path, s.devKey, 0o600); err != nil {
				log.Printf("device key: %v (device cookies end at the next restart)", err)
			}
		}
	})
	return s.devKey
}

func (s *Server) deviceMAC(uid, nonce string) string {
	m := hmac.New(sha256.New, s.deviceKey())
	io.WriteString(m, "pifoto-device:"+uid+"."+nonce)
	return hex.EncodeToString(m.Sum(nil))
}

// setDeviceCookie remembers this device for uid. Path /api/login: nothing else needs it.
func (s *Server) setDeviceCookie(w http.ResponseWriter, r *http.Request, uid string) {
	n := randHex(16)
	http.SetCookie(w, &http.Cookie{Name: deviceCookie, Value: uid + "." + n + "." + s.deviceMAC(uid, n),
		Path: "/api/login", MaxAge: 2 * 365 * 24 * 3600, HttpOnly: true, Secure: isHTTPS(r),
		SameSite: http.SameSiteStrictMode})
}

// trustedDevice: the request brings a valid device cookie for uid. dev names the device for devLim.
func (s *Server) trustedDevice(r *http.Request, uid string) (dev string, ok bool) {
	c, err := r.Cookie(deviceCookie)
	if err != nil {
		return "", false
	}
	p := strings.Split(c.Value, ".")
	if len(p) != 3 || p[0] != uid || !lowerHex(p[1], 32, 32) || !lowerHex(p[2], 64, 64) {
		return "", false
	}
	if !hmac.Equal([]byte(p[2]), []byte(s.deviceMAC(uid, p[1]))) {
		return "", false
	}
	return p[1], true
}
