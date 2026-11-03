// PiFoto — a minimal self-hosted photo library for low-end hardware (Raspberry Pi 1).
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/pbkdf2"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

//go:embed web/index.html web/app.html web/manifest.json web/icon.svg web/*.png
var webFS embed.FS

const sessionCookie = "pf_s"

type Config struct {
	Salt string `json:"salt"`
	Hash string `json:"hash"`
	Iter int    `json:"iter"`
}

type Server struct {
	dir      string
	store    *Store
	users    *Users
	sessMu   sync.Mutex
	sessions map[string]Session
	limiter  *loginLimiter // logins and password checks, per client IP
	accLim   *loginLimiter // failed logins per account from the internet (many IPs guessing one person's password)
	joinLim  *loginLimiter // invite-link lookups (a dead link reloaded must not block logins)
	pwLim    *loginLimiter // wrong current passwords when changing it, per account (someone else's open session)
	helloLim *loginLimiter // /api/hello misses, per client IP
	devLim   *loginLimiter // failed logins from the internet per trusted device (see trustedDevice)
	devOnce  sync.Once
	devKey   []byte     // signs device cookies (device.key in the data dir)
	commitMu sync.Mutex // serialises dedupe check + rename + index add, and changes that trash files
	thumbs   *Worker
	gzMu     sync.Mutex
	gzItems  map[string]gzEntry // per person: /api/items compressed, for the slow way in (Funnel, Tailscale)
	appMu    sync.Mutex
	appSum   appSum // SHA-256 of the published APK, recomputed when it changes
	flightMu sync.Mutex
	inflight map[string]int64 // bytes of uploads arriving right now, per limited person (roomWriter)
	remote   string           // -remote-url: the public address (download page)
}

type gzEntry struct {
	src *byte // the cached JSON it was made from (the store's cache makes a new one on every change)
	gz  []byte
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

func gzipBytes(b []byte) []byte {
	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestSpeed) // ~0.6 s for 1.7 MB on a Pi 1, to a third
	zw.Write(b)
	zw.Close()
	return out.Bytes()
}

func main() {
	dataDir := flag.String("data", "./data", "data directory")
	addr := flag.String("addr", ":8080", "listen address")
	setPw := flag.Bool("set-password", false, "read a new password from stdin for -user (created as an admin if missing) and exit")
	userName := flag.String("user", "", "who -set-password is for (default: the owner)")
	ownerName := flag.String("owner-name", "Owner", "name for the first account when upgrading a single-password library")
	remoteURL := flag.String("remote-url", "", "address that reaches this server from anywhere (e.g. Tailscale Funnel); the apps use it away from home and invite links point to it")
	flag.Parse()

	for _, d := range []string{"originals", "thumbs", "previews", "tmp", "trash"} {
		if err := os.MkdirAll(filepath.Join(*dataDir, d), 0o750); err != nil {
			log.Fatal(err)
		}
	}
	users, err := OpenUsers(*dataDir, *ownerName)
	if err != nil {
		log.Fatal(err)
	}
	sessPath := filepath.Join(*dataDir, "sessions.json")

	if *setPw {
		pw, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		u, err := cliSetPassword(users, sessPath, *userName, *ownerName, strings.TrimRight(pw, "\r\n"), os.Stderr)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("password set for %s\n", u.Name)
		return
	}

	owner, ok := users.Owner()
	if !ok {
		log.Fatal("no accounts yet: run with -set-password first")
	}
	s := &Server{dir: *dataDir, users: users}
	s.initLimits()
	s.sessions = loadSessions(sessPath, owner.ID)
	cleanupTemp(s.dir)
	if s.store, err = OpenStore(s.dir); err != nil {
		log.Fatal(err)
	}
	s.store.SetNames(users.Name)
	reconcile(s.dir, s.store)
	if n := s.store.AssignUnowned(owner.ID); n > 0 {
		log.Printf("put %d items without an owner into %s's library", n, owner.Name)
	}
	s.thumbs = NewWorker(s)
	s.thumbs.RecoverCrash()
	s.thumbs.QueueMissing()
	if err := s.store.Flush(); err != nil {
		log.Printf("initial index write: %v", err)
	}
	s.store.StartFlusher()
	s.thumbs.Start()

	go func() { // write pending index changes on systemctl stop/restart
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM, os.Interrupt)
		<-c
		s.thumbs.Shutdown() // the job running now was interrupted, not a crash
		if err := s.store.Flush(); err != nil {
			log.Printf("final index write: %v", err)
		}
		os.Exit(0)
	}()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.routes(*remoteURL),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("PiFoto listening on %s, %d items, %d people", *addr, s.store.Len(), users.Len())
	log.Fatal(srv.ListenAndServe())
}

// cliSetPassword is -set-password: it sets the password of userName (the owner if empty; an
// admin called userName, or ownerName, is created if missing) and logs that person out
// everywhere. It takes any password but warns about one the app would refuse: it is the way
// back in, and local development uses short ones.
func cliSetPassword(users *Users, sessPath, userName, ownerName, pw string, warn io.Writer) (User, error) {
	var u User
	var ok bool
	if userName == "" {
		u, ok = users.Owner()
	} else {
		u, ok = users.ByName(userName)
	}
	name := u.Name
	if !ok {
		if name = userName; name == "" {
			name = ownerName
		}
	}
	if pw == "" {
		return User{}, errPwEmpty
	}
	if err := pwPolicy(pw, name); err != nil {
		fmt.Fprintf(warn, "warning: the app would refuse this password: %v\n", err)
	}
	var err error
	if ok {
		err = users.SetPassword(u.ID, pw)
	} else {
		u, err = users.CreateWithPassword(name, pw, true)
	}
	if err != nil {
		return User{}, err
	}
	owner, _ := users.Owner()
	sess := loadSessions(sessPath, owner.ID)
	for k, v := range sess { // log that person out everywhere
		if v.U == u.ID {
			delete(sess, k)
		}
	}
	return u, writeJSON(sessPath, sess)
}

// initLimits sets up the login limiters (see loginLimiter).
func (s *Server) initLimits() {
	s.limiter = newLoginLimiter()
	s.accLim = newAccountLimiter()
	s.joinLim = newLoginLimiter()
	s.pwLim = newLimiter(5, 15*time.Minute)
	s.helloLim = newLimiter(30, 5*time.Minute) // the app asks once per start or backup run
	s.devLim = newLimiter(5, 15*time.Minute)
}

// routes wires up the HTTP API.
func (s *Server) routes(remoteURL string) http.Handler {
	s.remote = remoteURL
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.static("web/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /manifest.json", s.static("web/manifest.json", "application/manifest+json"))
	mux.HandleFunc("GET /icon.svg", s.static("web/icon.svg", "image/svg+xml"))
	for _, n := range []string{"icon-192.png", "icon-512.png", "icon-maskable.png", "apple-touch-icon.png"} {
		mux.HandleFunc("GET /"+n, s.static("web/"+n, "image/png")) // home-screen icons (iPhone, browsers)
	}
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "User-agent: *\nDisallow: /\n")
	})
	// The Android app's download page and the APK itself (no login needed: they hold no data).
	// tools/publish-app.sh puts them at <data>/app/ (see appupdate.go).
	mux.HandleFunc("GET /app", s.handleAppPage)
	mux.HandleFunc("GET /app/PiFoto.apk", s.serveAPK)
	if hasQR {
		mux.HandleFunc("GET /app/qr.png", s.static("web/install-qr.png", "image/png"))
	}
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/hello", s.handleHello)
	mux.HandleFunc("GET /api/join/{code}", s.handleJoinInfo)
	mux.HandleFunc("POST /api/join", s.handleJoin)
	mux.HandleFunc("POST /api/logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /api/me", s.auth(func(w http.ResponseWriter, r *http.Request, u User) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSONResp(w, u.Public())
	}))
	mux.HandleFunc("POST /api/password", s.auth(s.handlePassword))
	mux.HandleFunc("GET /api/items", s.auth(s.handleItems))
	mux.HandleFunc("GET /api/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /api/info", s.auth(func(w http.ResponseWriter, r *http.Request, _ User) {
		writeJSONResp(w, map[string]any{"remote": remoteURL})
	}))
	mux.HandleFunc("GET /api/app", s.auth(s.handleApp))
	mux.HandleFunc("POST /api/upload", s.auth(s.handleUpload))
	mux.HandleFunc("DELETE /api/items/{id}", s.auth(s.handleDelete))
	mux.HandleFunc("POST /api/family", s.auth(s.handleFamily))
	mux.HandleFunc("POST /api/keep", s.auth(s.handleKeep))
	mux.HandleFunc("GET /media/{kind}/{id}", s.auth(s.handleMedia))
	mux.HandleFunc("GET /api/users", s.admin(s.handleUsers))
	mux.HandleFunc("POST /api/users", s.admin(s.handleUserCreate))
	mux.HandleFunc("PATCH /api/users/{id}", s.admin(s.handleUserUpdate))
	mux.HandleFunc("POST /api/users/{id}/invite", s.admin(s.handleUserInvite))
	mux.HandleFunc("DELETE /api/users/{id}", s.admin(s.handleUserDelete))
	mux.HandleFunc("GET /api/links", s.admin(s.handleLinks))
	mux.HandleFunc("POST /api/links", s.admin(s.handleLinkCreate))
	mux.HandleFunc("DELETE /api/links/{id}", s.admin(s.handleLinkDelete))

	return secHeaders(readDeadlines(mux))
}

func hashPw(pw string, c Config) string {
	salt, _ := hex.DecodeString(c.Salt)
	k, _ := pbkdf2.Key(sha256.New, pw, salt, c.Iter, 32)
	return hex.EncodeToString(k)
}

func secHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow") // a private library, even when reachable from anywhere
		if isHTTPS(r) {
			// Browsers that once used the https address never fall back to plain http for it (no
			// includeSubDomains/preload: ts.net names aren't ours to commit).
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) static(name, ctype string) http.HandlerFunc {
	b, _ := webFS.ReadFile(name)
	var gz []byte
	if strings.HasSuffix(name, ".html") {
		gz = gzipBytes(b) // the page: 180 KB, a quarter of that compressed; done once
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		if strings.HasSuffix(name, ".html") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob:; media-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		}
		if gz != nil {
			w.Header().Set("Vary", "Accept-Encoding")
			if acceptsGzip(r) {
				w.Header().Set("Content-Encoding", "gzip")
				w.Write(gz)
				return
			}
		}
		w.Write(b)
	}
}

// ---------- auth ----------

// loginLimiter: after max failures within window a key (client IP, account) is refused without hashing.
//
// Its memory stays bounded however many addresses an attacker has: an IPv6 /64 counts as one
// address (limitKey), a key keeps only its last max failures, expired keys are dropped once per
// window, and there are at most limiterMaxKeys keys. Most failures cost a password hash, and
// only two run at once (hashSlots), so on a Pi these maps stay far smaller by themselves; the
// cap is for the cheap failures (bad invite codes, hello misses) from many networks. When it is
// full, the key whose last failure is oldest goes: that forgets whoever stopped trying first,
// while a key under attack keeps getting fresh failures and stays.
type loginLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
	pruned time.Time
}

// limiterMaxKeys: at most ~1.5 MB per limiter when full (a key of up to 64 runes, up to 10 times).
const limiterMaxKeys = 4096

func newLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, fails: map[string][]time.Time{}, pruned: time.Now()}
}

// newLoginLimiter is per client IP: 5 failures within 5 minutes.
func newLoginLimiter() *loginLimiter { return newLimiter(5, 5*time.Minute) }

// newAccountLimiter is per account, against guessing from many IPs: 10 failures within 15 minutes.
// It only stops new logins; devices already logged in keep working.
func newAccountLimiter() *loginLimiter { return newLimiter(10, 15*time.Minute) }

func (l *loginLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-l.window)
	n := 0
	for _, t := range l.fails[key] {
		if t.After(cut) {
			n++
		}
	}
	return n >= l.max
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.pruned) > l.window || len(l.fails) >= limiterMaxKeys {
		l.prune(now)
	}
	ts, ok := l.fails[key]
	if !ok && len(l.fails) >= limiterMaxKeys {
		l.evictOldest()
	}
	if len(ts) >= l.max { // only the last max failures matter
		ts = append(ts[:0], ts[len(ts)-l.max+1:]...)
	}
	l.fails[key] = append(ts, now)
}

// prune drops keys whose failures are all older than the window; caller holds l.mu.
func (l *loginLimiter) prune(now time.Time) {
	cut := now.Add(-l.window)
	for k, ts := range l.fails {
		if !ts[len(ts)-1].After(cut) {
			delete(l.fails, k)
		}
	}
	l.pruned = now
}

// evictOldest drops the key whose last failure is oldest; caller holds l.mu.
func (l *loginLimiter) evictOldest() {
	var old string
	var oldT time.Time
	first := true
	for k, ts := range l.fails {
		if t := ts[len(ts)-1]; first || t.Before(oldT) {
			old, oldT, first = k, t, false
		}
	}
	delete(l.fails, old)
}

func (l *loginLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.fails)
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}

// fromProxy: the request came through a reverse proxy on this machine (tailscale serve / funnel),
// whose forwarding headers can be trusted. Nothing else reaches us over loopback.
func fromProxy(r *http.Request) bool {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// clientIP is who sent the request. Behind the local proxy every request comes from 127.0.0.1, so
// there the proxy's X-Forwarded-For says who it really is (its last entry: the one the proxy added).
func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		h = r.RemoteAddr
	}
	if fromProxy(r) {
		xff := r.Header.Values("X-Forwarded-For")
		if n := len(xff); n > 0 {
			parts := strings.Split(xff[n-1], ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				return ip.String()
			}
		}
	}
	return h
}

// isHTTPS: the browser talks to us over TLS (directly, or to the local proxy in front of us).
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || (fromProxy(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}

// hashSlots caps concurrent password hashing (~1 s each on a Pi 1), so a flood of login attempts
// from many addresses can't starve the CPU and memory: extra attempts wait up to hashWait, then get 503.
// Requests from the internet must also hold publicHash, so they use at most one of the slots and
// a flood from out there can't make logins at home fail.
var (
	hashSlots  = make(chan struct{}, 2)
	publicHash = make(chan struct{}, 1)
	hashWait   = 5 * time.Second
)

func acquireHash(public bool) bool {
	t := time.NewTimer(hashWait)
	defer t.Stop()
	if public {
		select {
		case publicHash <- struct{}{}:
		case <-t.C:
			return false
		}
	}
	select {
	case hashSlots <- struct{}{}:
		return true
	case <-t.C:
		if public {
			<-publicHash
		}
		return false
	}
}

func releaseHash(public bool) {
	<-hashSlots
	if public {
		<-publicHash
	}
}

type userHandler func(http.ResponseWriter, *http.Request, User)

func (s *Server) sessionUser(r *http.Request) (User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return User{}, false
	}
	s.sessMu.Lock()
	ss, ok := s.sessions[c.Value]
	s.sessMu.Unlock()
	if !ok || time.Now().After(ss.Exp) {
		return User{}, false
	}
	return s.users.Get(ss.U)
}

func (s *Server) auth(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.sessionUser(r)
		if !ok {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		// CSRF guard: state-changing requests need a custom header (forces CORS preflight).
		if r.Method != http.MethodGet && r.Header.Get("X-PiFoto") != "1" {
			http.Error(w, "bad request", http.StatusForbidden)
			return
		}
		h(w, r, u)
	}
}

func (s *Server) admin(h userHandler) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, u User) {
		if !u.Admin {
			http.Error(w, "only an admin can do that", http.StatusForbidden)
			return
		}
		h(w, r, u)
	})
}

func (s *Server) saveSessions() { writeJSON(filepath.Join(s.dir, "sessions.json"), s.sessions) }

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, uid string) {
	t := randHex(32)
	exp := time.Now().Add(365 * 24 * time.Hour)
	s.sessMu.Lock()
	for k, v := range s.sessions {
		if time.Now().After(v.Exp) {
			delete(s.sessions, k)
		}
	}
	s.sessions[t] = Session{U: uid, Exp: exp}
	s.saveSessions()
	s.sessMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: t, Path: "/", Expires: exp,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode})
}

// endSessions logs a person out on all devices.
func (s *Server) endSessions(uid string) {
	s.sessMu.Lock()
	for k, v := range s.sessions {
		if v.U == uid {
			delete(s.sessions, k)
		}
	}
	s.saveSessions()
	s.sessMu.Unlock()
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// maxNameRunes: longer login names are refused before they become a limiter key or a log line.
const maxNameRunes = 64

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if formBody(r) {
		log.Printf("login refused from %s: body sent as %.40q, not JSON", clientIP(r), r.Header.Get("Content-Type"))
		http.Error(w, "send the login as JSON", http.StatusUnsupportedMediaType)
		return
	}
	ip := clientIP(r)
	key := limitKey(ip)
	if s.limiter.blocked(key) {
		http.Error(w, "too many attempts, wait a few minutes", http.StatusTooManyRequests)
		return
	}
	var req struct{ Name, Password string }
	if err := readBody(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if utf8.RuneCountInString(req.Name) > maxNameRunes {
		http.Error(w, "that name is too long", 400)
		return
	}
	public := isPublic(r)
	var u User
	var ok bool
	if strings.TrimSpace(req.Name) == "" {
		// Older apps and scripts log in without a name, meaning the owner. Only at home and on the
		// tailnet: from the internet that would hand everyone the one name worth guessing for.
		if public {
			http.Error(w, "Enter your name", 400)
			return
		}
		u, ok = s.users.Owner()
	} else {
		u, ok = s.users.ByName(req.Name)
	}
	acc := "?" + strings.ToLower(strings.Join(strings.Fields(req.Name), " ")) // unknown names count too
	var dev string
	trusted := false
	if ok {
		acc = u.ID
		dev, trusted = s.trustedDevice(r, u.ID)
	}
	// Only failures from the internet count against an account, and only logins from there are
	// refused while it's locked: otherwise anyone out there could lock the family out at home.
	// At home and on the tailnet the per-IP limit applies. A device the person logged in on
	// before (its pf_d cookie) skips the account lock and has its own (see trustedDevice), so
	// strangers guessing a name can't keep its owner from logging in again from their phone.
	if public {
		if trusted && s.devLim.blocked(dev) || !trusted && s.accLim.blocked(acc) {
			log.Printf("login refused for %.40q from %s: too many wrong passwords for this account", req.Name, ip)
			http.Error(w, "too many wrong passwords for this name, wait a few minutes", http.StatusTooManyRequests)
			return
		}
	}
	if len(req.Password) > 4*maxPwRunes { // no password can be this long; don't hash a megabyte
		ok = false
		req.Password = ""
	}
	if !acquireHash(public) {
		http.Error(w, "the server is busy, try again in a moment", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		hashPw(req.Password, dummy) // same time as a real check
	}
	good := ok && CheckPassword(u, req.Password)
	releaseHash(public)
	if !good {
		s.limiter.fail(key)
		if public && trusted {
			s.devLim.fail(dev)
		} else if public {
			s.accLim.fail(acc)
		}
		log.Printf("login failed for %.40q from %s", req.Name, ip)
		time.Sleep(time.Second)
		http.Error(w, "wrong name or password", http.StatusUnauthorized)
		return
	}
	// The address's failures stay counted: someone who knows their own password could otherwise
	// guess someone else's in between logins.
	s.startSession(w, r, u.ID)
	s.setDeviceCookie(w, r, u.ID)
	writeJSONResp(w, u.Public())
}

// handleLogout ends this device's session, or with {"all": true} all of the person's sessions
// (a lost phone, a session someone else may have).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, u User) {
	var req struct{ All bool }
	readBody(r, &req) // no body (older apps, the plain log out): just this device
	if req.All {
		s.endSessions(u.ID)
	} else if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessMu.Lock()
		delete(s.sessions, c.Value)
		s.saveSessions()
		s.sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode})
	w.WriteHeader(204)
}

// handleJoinInfo tells an invite link's page whose invite it is.
func (s *Server) handleJoinInfo(w http.ResponseWriter, r *http.Request) {
	key := limitKey(clientIP(r))
	if s.joinLim.blocked(key) {
		http.Error(w, "too many attempts, wait a few minutes", http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	code := r.PathValue("code")
	if u, ok := s.users.ByInvite(code); ok {
		writeJSONResp(w, map[string]any{"name": u.Name})
		return
	}
	// A sign-up link: the person chooses their own name. "by" is who sent it.
	if l, ok := s.users.LinkByCode(code); ok {
		writeJSONResp(w, map[string]any{"open": true, "by": s.users.Name(l.By), "quota": l.Quota})
		return
	}
	s.joinLim.fail(key)
	http.Error(w, errLinkGone.Error(), http.StatusNotFound)
}

// handleJoin redeems an invite: the person chooses a password and is logged in.
func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if formBody(r) {
		http.Error(w, "send the request as JSON", http.StatusUnsupportedMediaType)
		return
	}
	key := limitKey(clientIP(r))
	if s.joinLim.blocked(key) {
		http.Error(w, "too many attempts, wait a few minutes", http.StatusTooManyRequests)
		return
	}
	var req struct{ Code, Name, Password string }
	if err := readBody(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	// The code first: a password is only hashed (a second of the Pi's CPU) for a valid invite.
	inv, ok := s.users.ByInvite(req.Code)
	if !ok {
		if l, ok := s.users.LinkByCode(req.Code); ok {
			s.signup(w, r, l, req.Name, req.Password, key)
			return
		}
		s.joinLim.fail(key)
		http.Error(w, errLinkGone.Error(), http.StatusNotFound)
		return
	}
	if err := pwPolicy(req.Password, inv.Name); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	public := isPublic(r)
	if !acquireHash(public) {
		http.Error(w, "the server is busy, try again in a moment", http.StatusServiceUnavailable)
		return
	}
	u, err := s.users.Join(req.Code, req.Password)
	releaseHash(public)
	if err == errNoUser { // used up a moment ago
		http.Error(w, "This invite link is no longer valid. Ask for a new one.", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.startSession(w, r, u.ID)
	s.setDeviceCookie(w, r, u.ID)
	writeJSONResp(w, u.Public())
}

// signup makes an account with a sign-up link (see handleJoin): the person chose their name.
func (s *Server) signup(w http.ResponseWriter, r *http.Request, l Link, name, pw, key string) {
	name, err := cleanName(name)
	if err != nil {
		http.Error(w, "Enter your name (at most 32 characters)", 400)
		return
	}
	if _, taken := s.users.ByName(name); taken {
		s.joinLim.fail(key) // a link must not become a way to list everyone's login name
		http.Error(w, "That name is already used. Add something, like your last name.", http.StatusConflict)
		return
	}
	if s.users.Len() >= maxUsers { // before the password costs a second of CPU
		s.joinLim.fail(key)
		http.Error(w, "PiFoto can't take more people. Ask whoever sent you the link.", http.StatusForbidden)
		return
	}
	if err := pwPolicy(pw, name); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	public := isPublic(r)
	if !acquireHash(public) {
		http.Error(w, "the server is busy, try again in a moment", http.StatusServiceUnavailable)
		return
	}
	u, _, err := s.users.Signup(l.Code, name, pw)
	releaseHash(public)
	if err == errLinkGone || err == errNameTaken || err == errTooMany {
		s.joinLim.fail(key) // lost a race, or someone retrying: either way it cost a hash
	}
	switch err {
	case nil:
	case errLinkGone: // used up or deleted a moment ago
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case errNameTaken:
		http.Error(w, "That name is already used. Add something, like your last name.", http.StatusConflict)
		return
	case errTooMany:
		http.Error(w, "PiFoto can't take more people. Ask whoever sent you the link.", http.StatusForbidden)
		return
	default:
		http.Error(w, err.Error(), 500)
		return
	}
	log.Printf("%.40q made an account with sign-up link %s (from %s)", u.Name, l.ID, clientIP(r))
	s.startSession(w, r, u.ID)
	s.setDeviceCookie(w, r, u.ID)
	writeJSONResp(w, u.Public())
}

// handlePassword changes the caller's password. Someone else may know the old one or have a
// session, so every other device is logged out; this one gets a fresh session.
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request, u User) {
	ip := clientIP(r)
	key := limitKey(ip)
	if s.limiter.blocked(key) || s.pwLim.blocked(u.ID) {
		http.Error(w, "too many attempts, wait a few minutes", http.StatusTooManyRequests)
		return
	}
	var req struct{ Old, New string }
	if err := readBody(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := pwPolicy(req.New, u.Name); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	public := isPublic(r)
	if !acquireHash(public) {
		http.Error(w, "the server is busy, try again in a moment", http.StatusServiceUnavailable)
		return
	}
	good := CheckPassword(u, req.Old)
	var err error
	if good {
		err = s.users.SetPassword(u.ID, req.New)
	}
	releaseHash(public)
	if !good {
		s.limiter.fail(key)
		s.pwLim.fail(u.ID)
		log.Printf("password change for %.40q from %s: wrong current password", u.Name, ip)
		http.Error(w, "the current password is wrong", http.StatusForbidden)
		return
	}
	if err != nil {
		userErr(w, err)
		return
	}
	s.endSessions(u.ID)
	s.startSession(w, r, u.ID)
	w.WriteHeader(204)
}

// ---------- API ----------

func (s *Server) handleItems(w http.ResponseWriter, r *http.Request, u User) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Accept-Encoding")
	b := s.store.ListJSON(u.ID)
	// Compress only what comes through the local proxy (Funnel from the internet, or Tailscale):
	// there the link is slow (about 100 KB/s) and 1.7 MB take 18 s. On the home network the
	// transfer is fast and the Pi's CPU is what's short, so it gets the plain JSON.
	if !fromProxy(r) || !acceptsGzip(r) {
		w.Write(b)
		return
	}
	s.gzMu.Lock()
	e := s.gzItems[u.ID]
	if e.src != &b[0] {
		e = gzEntry{src: &b[0], gz: gzipBytes(b)}
		if s.gzItems == nil {
			s.gzItems = map[string]gzEntry{}
		}
		s.gzItems[u.ID] = e
	}
	s.gzMu.Unlock()
	w.Header().Set("Content-Encoding", "gzip")
	w.Write(e.gz)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, u User) {
	var st syscall.Statfs_t
	syscall.Statfs(s.dir, &st)
	q, cur := s.thumbs.Pending()
	since, perr := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	seq, reset, changes := s.store.Changes(since, r.URL.Query().Has("since") && perr == nil, u.ID)
	if r.URL.Query().Has("since") && perr != nil {
		reset = true
	}
	mine, bytes, fam := s.store.Stats(u.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResp(w, map[string]any{
		"count":   mine,
		"family":  fam,
		"queue":   q,
		"working": cur,
		"free":    uint64(st.Bavail) * uint64(st.Bsize),
		"total":   uint64(st.Blocks) * uint64(st.Bsize),
		"used":    bytes,
		"quota":   u.Quota,
		"seq":     seq,
		"reset":   reset,
		"changes": changes,
	})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, u User) {
	name := filepath.Base(strings.ReplaceAll(r.URL.Query().Get("name"), "\\", "/"))
	if len(name) > 200 {
		name = name[len(name)-200:]
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	kind := kindOf(ext)
	if kind == "" {
		http.Error(w, "unsupported file type: "+ext, http.StatusUnsupportedMediaType)
		return
	}
	share := r.URL.Query().Get("family") == "1"
	// Storage limit: refuse before reading a file that can't fit (the browser sends its size;
	// the Android app asks /api/status first). While the file comes in, no more than what's left
	// is written (counting this person's other uploads running at the same time), so a chunked
	// upload can't fill the card first and be refused after. The final check is below.
	var room *roomWriter
	if u.Quota > 0 {
		used := s.store.Used(u.ID)
		if used >= u.Quota || r.ContentLength > 0 && used+r.ContentLength > u.Quota {
			quotaFull(w, used, u.Quota)
			return
		}
		room = &roomWriter{s: s, uid: u.ID, limit: u.Quota - used, used: used, quota: u.Quota}
		defer room.done()
	}
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "up-*")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename
	idleDeadline(w, r)          // no time limit for the whole file, but it has to keep coming
	h := sha256.New()
	sink := io.MultiWriter(tmp, h)
	if room != nil {
		sink = io.MultiWriter(room, tmp, h) // room first: it stops the bytes before they reach the card
	}
	n, err := io.Copy(sink, r.Body)
	if err == nil {
		err = tmp.Sync() // the bytes must be on the card before we tell the phone it's stored
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, errOverRoom) {
		quotaFull(w, room.used, room.quota)
		return
	}
	if err != nil {
		// Disk full, I/O error or an interrupted body: transient from the client's view, so 5xx
		// (a 4xx would make the Android app treat the file as permanently unacceptable).
		log.Printf("upload of %.60q by %s failed after %d bytes: %v", name, u.Name, n, err)
		http.Error(w, "upload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "empty file", 400)
		return
	}
	id := hex.EncodeToString(h.Sum(nil))[:20]
	mtimeMs, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64)
	taken, info, ok := takenFor(tmp.Name(), ext, name, mtimeMs)
	if !ok {
		taken = time.Now()
	}

	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	reply := func(it Item, dup bool, extra map[string]any) {
		if share && it.OwnedBy(u.ID) && !it.Fam {
			it, _, _ = s.store.SetFamily(it.ID, u.ID, true, u.Admin)
		}
		m := map[string]any{"dup": dup, "row": s.store.RowFor(&it, u.ID)}
		for k, v := range extra {
			m[k] = v
		}
		writeJSONResp(w, m)
	}
	existing, exists := s.store.Get(id)
	if exists && existing.OwnedBy(u.ID) {
		reply(existing, true, nil)
		return
	}
	// Same photo, different bytes (e.g. Google Photos' recompressed copy of a phone picture):
	// same original file name + same capture moment in this person's library. Opt out with
	// ?similar=keep.
	if ok && r.URL.Query().Get("similar") != "keep" {
		if it, found := s.store.FindSimilar(name, kind, taken, u.ID); found {
			reply(it, true, map[string]any{"similar": true})
			return
		}
	}
	// From here on the file is added to their library: it has to fit. (Under commitMu, so two
	// uploads at once can't both take the last free space. The limit may have changed meanwhile.)
	if cur, ok := s.users.Get(u.ID); ok && cur.Quota > 0 {
		if used := s.store.Used(u.ID); used+n > cur.Quota {
			quotaFull(w, used, cur.Quota)
			return
		}
	}
	if exists {
		// Someone else in the family already has these exact bytes: share the stored file.
		it, _, err := s.store.Claim(id, u.ID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		reply(it, false, nil)
		return
	}
	rel := filepath.Join("originals", taken.Format("2006"), taken.Format("01"), id+"."+ext)
	dst := filepath.Join(s.dir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	syncDir(filepath.Dir(dst))
	syncDir(filepath.Dir(filepath.Dir(dst)))
	syncDir(filepath.Join(s.dir, "originals"))
	it := Item{ID: id, Name: name, Ext: ext, Kind: kind, Size: n, Taken: taken, Added: time.Now(),
		Rel: rel, Owners: []string{u.ID}}
	it.W, it.H = info.Displayed()
	if !CanThumb(ext) {
		it.Thumb = ThumbNone
	}
	if err := s.store.Add(it); err != nil {
		os.Remove(dst)
		http.Error(w, err.Error(), 500)
		return
	}
	if it.Thumb == ThumbPending {
		s.thumbs.Queue(id)
	}
	reply(it, false, nil)
}

var errOverRoom = errors.New("over the storage limit")

// roomWriter counts the bytes of a limited person's uploads while they arrive, across all their
// uploads at once (Server.inflight), and fails before more than their free space would be written.
type roomWriter struct {
	s           *Server
	uid         string
	limit       int64 // quota - used when the upload started
	used, quota int64 // for the message
	n           int64
}

func (q *roomWriter) Write(p []byte) (int, error) {
	q.s.flightMu.Lock()
	defer q.s.flightMu.Unlock()
	if q.s.inflight[q.uid]+int64(len(p)) > q.limit {
		return 0, errOverRoom
	}
	if q.s.inflight == nil {
		q.s.inflight = map[string]int64{}
	}
	q.s.inflight[q.uid] += int64(len(p))
	q.n += int64(len(p))
	return len(p), nil
}

func (q *roomWriter) done() {
	q.s.flightMu.Lock()
	defer q.s.flightMu.Unlock()
	if q.s.inflight[q.uid] -= q.n; q.s.inflight[q.uid] <= 0 {
		delete(q.s.inflight, q.uid)
	}
	q.n = 0
}

// quotaFull answers an upload that doesn't fit the person's storage limit. 507, not 413: the
// Android app gives up on a file for good on 4xx, and this one can go up once there's room.
func quotaFull(w http.ResponseWriter, used, quota int64) {
	http.Error(w, fmt.Sprintf("Your storage on PiFoto is full (%s of %s used). Ask an admin for more space.",
		fmtGB(used), fmtGB(quota)), http.StatusInsufficientStorage)
}

// fmtGB writes a size like the web app does (1 GB = 10^9 bytes).
func fmtGB(n int64) string {
	switch {
	case n < 1e6:
		return fmt.Sprintf("%d KB", (n+999)/1000)
	case n < 1e9:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
}

// trash moves an original nobody has any more to trash/ (recoverable by hand) and deletes
// its thumbnail and preview. Caller holds s.commitMu.
func (s *Server) trash(it Item) {
	if err := os.Rename(filepath.Join(s.dir, it.Rel), filepath.Join(s.dir, "trash", it.ID+"."+it.Ext)); err != nil && !isNotExist(err) {
		log.Printf("trash %s: %v", it.ID, err)
	}
	os.Remove(filepath.Join(s.dir, "thumbs", it.ID+".jpg"))
	os.Remove(filepath.Join(s.dir, "previews", it.ID+".jpg"))
}

// handleDelete removes a photo from the user's own library. The file itself is only trashed
// when nobody else has it and it isn't in the Family album.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, u User) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	it, gone, err := s.store.Unown(r.PathValue("id"), u.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if gone {
		s.trash(it)
	}
	w.WriteHeader(204)
}

type idsReq struct {
	IDs []string `json:"ids"`
	On  bool     `json:"on"`
}

// handleFamily adds photos from the user's library to the Family album, or takes them out.
func (s *Server) handleFamily(w http.ResponseWriter, r *http.Request, u User) {
	var req idsReq
	if err := readBody(r, &req); err != nil || len(req.IDs) > 20000 {
		http.Error(w, "bad request", 400)
		return
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	done, failed := 0, []string{}
	for _, id := range req.IDs {
		it, gone, err := s.store.SetFamily(id, u.ID, req.On, u.Admin)
		if err != nil {
			failed = append(failed, id)
			continue
		}
		if gone {
			s.trash(it)
		}
		done++
	}
	writeJSONResp(w, map[string]any{"done": done, "failed": len(failed), "failedIds": failed})
}

// handleKeep copies Family album photos into the user's own library (no extra disk space).
func (s *Server) handleKeep(w http.ResponseWriter, r *http.Request, u User) {
	var req idsReq
	if err := readBody(r, &req); err != nil || len(req.IDs) > 20000 {
		http.Error(w, "bad request", 400)
		return
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	done, failed, full := 0, []string{}, false
	var used int64
	if u.Quota > 0 {
		used = s.store.Used(u.ID)
	}
	for _, id := range req.IDs {
		it, ok := s.store.Get(id)
		if !ok || !it.VisibleTo(u.ID) {
			failed = append(failed, id)
			continue
		}
		// It takes no more space on the Pi, but everything in their photos counts for the limit.
		if u.Quota > 0 && !it.OwnedBy(u.ID) && used+it.Size > u.Quota {
			failed, full = append(failed, id), true
			continue
		}
		added, ok, err := s.store.Claim(id, u.ID)
		if err != nil {
			failed = append(failed, id)
			continue
		}
		if ok {
			used += added.Size
		}
		done++
	}
	writeJSONResp(w, map[string]any{"done": done, "failed": len(failed), "failedIds": failed, "full": full})
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request, u User) {
	id := r.PathValue("id")
	it, ok := s.store.Get(id)
	if !ok || !it.VisibleTo(u.ID) {
		http.NotFound(w, r)
		return
	}
	var path, ctype string
	cache := "private, max-age=31536000, immutable"
	switch r.PathValue("kind") {
	case "thumb", "preview":
		dir := "thumbs"
		if r.PathValue("kind") == "preview" {
			dir = "previews"
		}
		path = filepath.Join(s.dir, dir, id+".jpg")
		ctype = "image/jpeg"
		if _, err := os.Stat(path); err != nil {
			if r.PathValue("kind") == "preview" && it.Kind == "image" && browserShows(it.Ext) {
				// fall back to the original, but don't let the browser cache it as the preview
				path, ctype, cache = filepath.Join(s.dir, it.Rel), mimeOf(it.Ext), "no-cache"
			} else {
				http.NotFound(w, r)
				return
			}
		}
	case "orig":
		path, ctype = filepath.Join(s.dir, it.Rel), mimeOf(it.Ext)
		if r.URL.Query().Get("dl") == "1" {
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": it.Name}))
		}
	default:
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", cache)
	http.ServeContent(w, r, "", st.ModTime(), f) // supports Range (video seeking)
}

// ---------- family members (admins only) ----------

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, me User) {
	list := []map[string]any{}
	shares, famOnly := s.store.Shares()
	for _, u := range s.users.List() {
		p := u.Public()
		n, b, _ := s.store.Stats(u.ID)
		p["count"], p["bytes"], p["share"] = n, b, shares[u.ID]
		if u.Hash == "" && !u.InviteExp.IsZero() {
			p["inviteExpires"] = u.InviteExp.Unix()
		}
		if u.Via != "" {
			p["via"] = u.Via
		}
		p["created"] = u.Created.Unix()
		list = append(list, p)
	}
	apps := s.appsByUser()
	for _, p := range list {
		if a := apps[p["id"].(string)]; len(a) > 0 {
			p["apps"] = a
		}
	}
	var st syscall.Statfs_t
	syscall.Statfs(s.dir, &st)
	files, lib := s.store.Totals()
	resp := map[string]any{"me": me.ID, "users": list,
		"free":  uint64(st.Bavail) * uint64(st.Bsize),
		"total": uint64(st.Blocks) * uint64(st.Bsize),
		"files": files, "library": lib, "famOnly": famOnly}
	if a, ok := s.appRelease(); ok {
		resp["app"] = a
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResp(w, resp)
}

func userErr(w http.ResponseWriter, err error) {
	switch err {
	case errNameTaken:
		http.Error(w, err.Error(), http.StatusConflict)
	case errNameInvalid, errPwEmpty, errPwShort, errPwLong, errPwWeak, errPwName, errLastAdmin,
		errQuota, errLinkBad, errLinkLabel, errTooManyLink, errTooMany:
		http.Error(w, err.Error(), 400)
	case errNoUser:
		http.Error(w, err.Error(), 404)
	default:
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request, _ User) {
	var req struct {
		Name  string `json:"name"`
		Admin bool   `json:"admin"`
		Quota int64  `json:"quota"`
	}
	if err := readBody(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	u, code, err := s.users.Create(req.Name, req.Admin, req.Quota)
	if err != nil {
		userErr(w, err)
		return
	}
	writeJSONResp(w, map[string]any{"user": u.Public(), "code": code})
}

func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request, me User) {
	id := r.PathValue("id")
	var req struct {
		Name  *string `json:"name"`
		Admin *bool   `json:"admin"`
		Quota *int64  `json:"quota"`
	}
	if err := readBody(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if req.Admin != nil && id == me.ID && !*req.Admin {
		http.Error(w, "you can't take away your own admin rights", 400)
		return
	}
	if req.Name != nil {
		if err := s.users.Rename(id, *req.Name); err != nil {
			userErr(w, err)
			return
		}
		s.store.SharerRenamed(id)
	}
	if req.Admin != nil {
		if err := s.users.SetAdmin(id, *req.Admin); err != nil {
			userErr(w, err)
			return
		}
	}
	if req.Quota != nil {
		if err := s.users.SetQuota(id, *req.Quota); err != nil {
			userErr(w, err)
			return
		}
	}
	u, ok := s.users.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSONResp(w, u.Public())
}

// handleUserInvite makes a new invite link for someone (also how a forgotten password is
// reset): their old password stops working and they're logged out everywhere.
func (s *Server) handleUserInvite(w http.ResponseWriter, r *http.Request, me User) {
	id := r.PathValue("id")
	if id == me.ID {
		http.Error(w, "change your own password in the menu instead", 400)
		return
	}
	code, err := s.users.Reinvite(id)
	if err != nil {
		userErr(w, err)
		return
	}
	s.endSessions(id)
	writeJSONResp(w, map[string]any{"code": code})
}

// handleUserDelete removes a person and their library. Photos they put in the Family album
// stay there; files nobody has any more go to trash/.
func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request, me User) {
	id := r.PathValue("id")
	if id == me.ID {
		http.Error(w, "you can't remove yourself", 400)
		return
	}
	if err := s.users.Delete(id); err != nil {
		userErr(w, err)
		return
	}
	s.endSessions(id)
	s.commitMu.Lock()
	gone := s.store.RemoveUser(id)
	for _, it := range gone {
		s.trash(it)
	}
	s.commitMu.Unlock()
	writeJSONResp(w, map[string]any{"trashed": len(gone)})
}

// ---------- sign-up links (admins only) ----------

// linkJSON is a link as the admin page shows it: who made it and who joined with it by name.
func (s *Server) linkJSON(l Link, now time.Time) map[string]any {
	names := []string{}
	for _, id := range l.Users {
		if n := s.users.Name(id); n != "" {
			names = append(names, n)
		}
	}
	return map[string]any{"id": l.ID, "code": l.Code, "label": l.Label, "by": s.users.Name(l.By),
		"created": l.Created.Unix(), "expires": l.Exp.Unix(), "max": l.Max, "used": len(l.Users),
		"quota": l.Quota, "users": names, "live": l.Live}
}

func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request, _ User) {
	now := time.Now()
	list := []map[string]any{}
	for _, l := range s.users.Links() {
		list = append(list, s.linkJSON(l, now))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResp(w, map[string]any{"links": list})
}

func (s *Server) handleLinkCreate(w http.ResponseWriter, r *http.Request, me User) {
	var req struct {
		Label string `json:"label"`
		Hours int    `json:"hours"`
		Max   int    `json:"max"`
		Quota int64  `json:"quota"`
	}
	if err := readBody(r, &req); err != nil || req.Hours < 0 || req.Hours > 24*365 {
		http.Error(w, "bad request", 400)
		return
	}
	l, err := s.users.CreateLink(me.ID, req.Label, time.Duration(req.Hours)*time.Hour, req.Max, req.Quota)
	if err != nil {
		userErr(w, err)
		return
	}
	log.Printf("%s made sign-up link %s (%d people, %d h)", me.Name, l.ID, l.Max, req.Hours)
	writeJSONResp(w, s.linkJSON(l, time.Now()))
}

func (s *Server) handleLinkDelete(w http.ResponseWriter, r *http.Request, _ User) {
	if err := s.users.DeleteLink(r.PathValue("id")); err != nil {
		userErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// ---------- helpers ----------

var kinds = map[string]string{
	"jpg": "image", "jpeg": "image", "png": "image", "gif": "image", "webp": "image",
	"heic": "image", "heif": "image", "avif": "image", "dng": "image",
	"mp4": "video", "mov": "video", "webm": "video", "3gp": "video", "m4v": "video", "mkv": "video",
}

func kindOf(ext string) string { return kinds[ext] }

func browserShows(ext string) bool {
	switch ext {
	case "jpg", "jpeg", "png", "gif", "webp", "avif":
		return true
	}
	return false
}

func mimeOf(ext string) string {
	switch ext {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png", "gif", "webp", "heic", "heif", "avif":
		return "image/" + ext
	case "mp4", "m4v":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	case "webm":
		return "video/webm"
	case "3gp":
		return "video/3gpp"
	case "mkv":
		return "video/x-matroska"
	}
	return "application/octet-stream"
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// syncDir fsyncs a directory so a rename/create in it survives a power cut.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// writeJSON writes atomically (temp file + fsync + rename + dir fsync) so a power cut can't corrupt it.
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}
