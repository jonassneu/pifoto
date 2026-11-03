package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// User is one family member. Passwords are PBKDF2 hashes; a new member (or one whose password
// was reset) has no hash yet, only a pending invite code they redeem to choose a password.
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Admin     bool      `json:"admin,omitempty"`
	Salt      string    `json:"salt,omitempty"`
	Hash      string    `json:"hash,omitempty"`
	Iter      int       `json:"iter,omitempty"`
	Invite    string    `json:"invite,omitempty"` // sha256 of the pending invite code
	InviteExp time.Time `json:"inviteExp,omitempty"`
	Created   time.Time `json:"created"`
	Quota     int64     `json:"quota,omitempty"` // storage limit in bytes for their library, 0 = none
	Via       string    `json:"via,omitempty"`   // the sign-up link they made their account with
}

// Public is what the API shows about a user.
func (u User) Public() map[string]any {
	return map[string]any{"id": u.ID, "name": u.Name, "admin": u.Admin, "pending": u.Hash == "", "quota": u.Quota}
}

const (
	inviteTTL  = 7 * 24 * time.Hour
	maxUsers   = 100                 // accounts in total (a leaked sign-up link can't fill users.json)
	maxLinks   = 50                  // sign-up links kept, live or dead
	maxLinkUse = 50                  // accounts one sign-up link may make
	maxLinkTTL = 90 * 24 * time.Hour // longest a sign-up link may stay valid
	maxQuota   = int64(1) << 50      // 1 PiB: anything bigger is a typo
	linkKeep   = 30 * 24 * time.Hour // dead links are dropped this long after they expired
)

// Link is a sign-up link: whoever has it can make their own account (choosing their name and
// password) until it expires or has made Max accounts. Those accounts are never admins and
// start with the link's storage limit. The code is kept as it is (not hashed like invite codes)
// so admins can copy the link again; anyone who can read the data folder sees the photos anyway.
type Link struct {
	ID      string    `json:"id"`
	Code    string    `json:"code"`
	Label   string    `json:"label,omitempty"` // the admin's note, e.g. "Cousins" (never shown to whoever opens it)
	By      string    `json:"by"`              // admin who made it
	Created time.Time `json:"created"`
	Exp     time.Time `json:"exp"`
	Max     int       `json:"max"`
	Quota   int64     `json:"quota,omitempty"`
	Users   []string  `json:"users,omitempty"` // accounts made with it (removed people still count)
	Live    bool      `json:"-"`               // filled in by Links: it can make an account right now
}

// linkLive: not expired, not used up, and whoever made it is still an admin (a link must not
// outlive its maker's rights); caller holds u.mu.
func (u *Users) linkLive(l *Link, now time.Time) bool {
	if !now.Before(l.Exp) || len(l.Users) >= l.Max {
		return false
	}
	by := u.find(l.By)
	return by != nil && by.Admin
}

var (
	errNameTaken   = errors.New("that name is already used")
	errNameInvalid = errors.New("names need 1 to 32 characters")
	errPwEmpty     = errors.New("the password is empty")
	errNoUser      = errors.New("no such person")
	errLastAdmin   = errors.New("there must be at least one admin")
	errTooMany     = errors.New("PiFoto can't take more people")
	errQuota       = errors.New("that storage limit isn't possible")
	errLinkBad     = errors.New("links last 1 hour to 90 days and are for 1 to 50 people")
	errLinkLabel   = errors.New("notes need at most 40 characters")
	errTooManyLink = errors.New("too many links: delete some first")
	errLinkGone    = errors.New("This invite link is no longer valid. Ask for a new one.")
)

type Users struct {
	mu        sync.RWMutex
	path      string
	list      []*User
	linksPath string
	links     []*Link
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

func newSalted(pw string) (salt, hash string, iter int) {
	c := Config{Salt: randHex(16), Iter: 60000}
	return c.Salt, hashPw(pw, c), c.Iter
}

// OpenUsers loads users.json. Without it, a single-password config.json from before family
// accounts becomes the first (admin) user called ownerName, keeping the same password.
func OpenUsers(dir, ownerName string) (*Users, error) {
	u := &Users{path: filepath.Join(dir, "users.json"), linksPath: filepath.Join(dir, "links.json")}
	if err := readJSON(u.linksPath, &u.links); err != nil && !isNotExist(err) {
		return nil, err
	}
	err := readJSON(u.path, &u.list)
	if err == nil {
		return u, nil
	}
	if !isNotExist(err) {
		return nil, err
	}
	var c Config
	if readJSON(filepath.Join(dir, "config.json"), &c) == nil && c.Hash != "" {
		u.list = []*User{{ID: randHex(6), Name: ownerName, Admin: true, Salt: c.Salt, Hash: c.Hash,
			Iter: c.Iter, Created: time.Now()}}
		if err := u.save(); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// save writes users.json; caller holds u.mu (or is the only goroutine).
func (u *Users) save() error { return writeJSON(u.path, u.list) }

func (u *Users) Len() int {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return len(u.list)
}

func (u *Users) find(id string) *User {
	for _, x := range u.list {
		if x.ID == id {
			return x
		}
	}
	return nil
}

func (u *Users) Get(id string) (User, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if x := u.find(id); x != nil {
		return *x, true
	}
	return User{}, false
}

// Name of a user, "" if unknown (e.g. removed).
func (u *Users) Name(id string) string {
	x, _ := u.Get(id)
	return x.Name
}

func (u *Users) ByName(name string) (User, bool) {
	name = strings.TrimSpace(name)
	u.mu.RLock()
	defer u.mu.RUnlock()
	for _, x := range u.list {
		if strings.EqualFold(x.Name, name) {
			return *x, true
		}
	}
	return User{}, false
}

// Owner is the oldest admin: the account a name-less login (old apps, scripts) means.
func (u *Users) Owner() (User, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	var best *User
	for _, x := range u.list {
		if x.Admin && (best == nil || x.Created.Before(best.Created)) {
			best = x
		}
	}
	if best == nil {
		return User{}, false
	}
	return *best, true
}

func (u *Users) List() []User {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make([]User, len(u.list))
	for i, x := range u.list {
		out[i] = *x
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

var dummy = Config{Salt: randHex(16), Iter: 60000}

// CheckPassword compares in constant time; for a user without a password it still spends the
// hashing time so a login can't tell names apart by speed.
func CheckPassword(x User, pw string) bool {
	if x.Hash == "" {
		hashPw(pw, dummy)
		return false
	}
	h := hashPw(pw, Config{Salt: x.Salt, Iter: x.Iter})
	return subtle.ConstantTimeCompare([]byte(h), []byte(x.Hash)) == 1
}

func cleanName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if n := utf8.RuneCountInString(name); n < 1 || n > 32 {
		return "", errNameInvalid
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return "", errNameInvalid
		}
	}
	return name, nil
}

// nameFree: caller holds u.mu.
func (u *Users) nameFree(name, except string) bool {
	for _, x := range u.list {
		if x.ID != except && strings.EqualFold(x.Name, name) {
			return false
		}
	}
	return true
}

// Create adds a person who still has to choose a password; returns their invite code.
// quota is their storage limit in bytes (0 = none).
func (u *Users) Create(name string, admin bool, quota int64) (User, string, error) {
	name, err := cleanName(name)
	if err != nil {
		return User{}, "", err
	}
	if quota < 0 || quota > maxQuota {
		return User{}, "", errQuota
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.nameFree(name, "") {
		return User{}, "", errNameTaken
	}
	if len(u.list) >= maxUsers {
		return User{}, "", errTooMany
	}
	code := randHex(16)
	x := &User{ID: u.newID(), Name: name, Admin: admin, Invite: hashCode(code),
		InviteExp: time.Now().Add(inviteTTL), Created: time.Now(), Quota: quota}
	u.list = append(u.list, x)
	if err := u.save(); err != nil {
		u.list = u.list[:len(u.list)-1]
		return User{}, "", err
	}
	return *x, code, nil
}

// CreateWithPassword is for the command line (first setup / recovery).
func (u *Users) CreateWithPassword(name, pw string, admin bool) (User, error) {
	if pw == "" {
		return User{}, errPwEmpty
	}
	x, _, err := u.Create(name, admin, 0)
	if err != nil {
		return User{}, err
	}
	return x, u.SetPassword(x.ID, pw)
}

// newID is an unused user id; caller holds u.mu.
func (u *Users) newID() string {
	id := randHex(6)
	for u.find(id) != nil {
		id = randHex(6)
	}
	return id
}

// Reinvite drops the password (if any) and returns a fresh invite code.
func (u *Users) Reinvite(id string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(id)
	if x == nil {
		return "", errNoUser
	}
	code := randHex(16)
	old := *x
	x.Salt, x.Hash, x.Iter = "", "", 0
	x.Invite, x.InviteExp = hashCode(code), time.Now().Add(inviteTTL)
	if err := u.save(); err != nil {
		*x = old
		return "", err
	}
	return code, nil
}

// ByInvite finds the person a (still valid) invite code belongs to.
func (u *Users) ByInvite(code string) (User, bool) {
	if len(code) < 16 {
		return User{}, false
	}
	h := hashCode(code)
	u.mu.RLock()
	defer u.mu.RUnlock()
	for _, x := range u.list {
		if x.Invite != "" && subtle.ConstantTimeCompare([]byte(x.Invite), []byte(h)) == 1 && time.Now().Before(x.InviteExp) {
			return *x, true
		}
	}
	return User{}, false
}

// Join redeems an invite: sets the password and uses up the code (atomically, so a code
// can't be redeemed twice). The password rules (pwPolicy) are the caller's: the command line
// only warns about them.
func (u *Users) Join(code, pw string) (User, error) {
	if pw == "" {
		return User{}, errPwEmpty
	}
	if len(code) < 16 {
		return User{}, errNoUser
	}
	salt, hash, iter := newSalted(pw) // slow: outside the lock
	h := hashCode(code)
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, x := range u.list {
		if x.Invite == "" || subtle.ConstantTimeCompare([]byte(x.Invite), []byte(h)) != 1 || !time.Now().Before(x.InviteExp) {
			continue
		}
		old := *x
		x.Salt, x.Hash, x.Iter, x.Invite, x.InviteExp = salt, hash, iter, "", time.Time{}
		if err := u.save(); err != nil {
			*x = old
			return User{}, err
		}
		return *x, nil
	}
	return User{}, errNoUser
}

// SetPassword sets a new password and cancels any pending invite (rules: see Join).
func (u *Users) SetPassword(id, pw string) error {
	if pw == "" {
		return errPwEmpty
	}
	salt, hash, iter := newSalted(pw) // slow: outside the lock
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(id)
	if x == nil {
		return errNoUser
	}
	old := *x
	x.Salt, x.Hash, x.Iter, x.Invite, x.InviteExp = salt, hash, iter, "", time.Time{}
	if err := u.save(); err != nil {
		*x = old
		return err
	}
	return nil
}

func (u *Users) Rename(id, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(id)
	if x == nil {
		return errNoUser
	}
	if !u.nameFree(name, id) {
		return errNameTaken
	}
	old := x.Name
	x.Name = name
	if err := u.save(); err != nil {
		x.Name = old
		return err
	}
	return nil
}

// SetQuota sets someone's storage limit in bytes (0 = none). Nothing is deleted when it's below
// what they already have: they just can't add more.
func (u *Users) SetQuota(id string, quota int64) error {
	if quota < 0 || quota > maxQuota {
		return errQuota
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(id)
	if x == nil {
		return errNoUser
	}
	old := x.Quota
	x.Quota = quota
	if err := u.save(); err != nil {
		x.Quota = old
		return err
	}
	return nil
}

func (u *Users) SetAdmin(id string, admin bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(id)
	if x == nil {
		return errNoUser
	}
	if !admin && x.Admin && u.admins() == 1 {
		return errLastAdmin
	}
	x.Admin = admin
	if err := u.save(); err != nil {
		x.Admin = !admin
		return err
	}
	return nil
}

// admins counts admins; caller holds u.mu.
func (u *Users) admins() int {
	n := 0
	for _, x := range u.list {
		if x.Admin {
			n++
		}
	}
	return n
}

func (u *Users) Delete(id string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, x := range u.list {
		if x.ID != id {
			continue
		}
		if x.Admin && u.admins() == 1 {
			return errLastAdmin
		}
		old := u.list
		u.list = append(append([]*User(nil), u.list[:i]...), u.list[i+1:]...)
		if err := u.save(); err != nil {
			u.list = old
			return err
		}
		return nil
	}
	return errNoUser
}

// ---------- sign-up links ----------

// saveLinks writes links.json; caller holds u.mu.
func (u *Users) saveLinks() error { return writeJSON(u.linksPath, u.links) }

// pruneLinks drops links that expired more than linkKeep ago; caller holds u.mu.
func (u *Users) pruneLinks(now time.Time) {
	keep := u.links[:0:0]
	for _, l := range u.links {
		if now.Before(l.Exp.Add(linkKeep)) {
			keep = append(keep, l)
		}
	}
	u.links = keep
}

// Links lists the sign-up links, newest first.
func (u *Users) Links() []Link {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make([]Link, 0, len(u.links))
	now := time.Now()
	for _, l := range u.links {
		c := *l
		c.Users = slices.Clone(l.Users)
		c.Live = u.linkLive(l, now)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func cleanLabel(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 40 {
		return "", errLinkLabel
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return "", errLinkLabel
		}
	}
	return s, nil
}

// CreateLink makes a sign-up link valid for ttl and max accounts, which get the storage limit
// quota (0 = none).
func (u *Users) CreateLink(by, label string, ttl time.Duration, max int, quota int64) (Link, error) {
	label, err := cleanLabel(label)
	if err != nil {
		return Link{}, err
	}
	if ttl < time.Hour || ttl > maxLinkTTL || max < 1 || max > maxLinkUse {
		return Link{}, errLinkBad
	}
	if quota < 0 || quota > maxQuota {
		return Link{}, errQuota
	}
	now := time.Now()
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pruneLinks(now)
	if len(u.links) >= maxLinks {
		return Link{}, errTooManyLink
	}
	l := &Link{ID: randHex(4), Code: randHex(16), Label: label, By: by, Created: now, Exp: now.Add(ttl), Max: max, Quota: quota}
	for u.findLinkID(l.ID) != nil {
		l.ID = randHex(4)
	}
	u.links = append(u.links, l)
	if err := u.saveLinks(); err != nil {
		u.links = u.links[:len(u.links)-1]
		return Link{}, err
	}
	c := *l
	c.Live = u.linkLive(l, now)
	return c, nil
}

// findLinkID: caller holds u.mu.
func (u *Users) findLinkID(id string) *Link {
	for _, l := range u.links {
		if l.ID == id {
			return l
		}
	}
	return nil
}

// findLink is the live link with this code; caller holds u.mu.
func (u *Users) findLink(code string, now time.Time) *Link {
	if len(code) < 16 {
		return nil
	}
	for _, l := range u.links {
		if subtle.ConstantTimeCompare([]byte(l.Code), []byte(code)) == 1 && u.linkLive(l, now) {
			return l
		}
	}
	return nil
}

// LinkByCode finds a sign-up link that can still make an account.
func (u *Users) LinkByCode(code string) (Link, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if l := u.findLink(code, time.Now()); l != nil {
		c := *l
		c.Live = true
		return c, true
	}
	return Link{}, false
}

// DeleteLink removes a sign-up link: it stops working at once. Accounts made with it stay.
func (u *Users) DeleteLink(id string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, l := range u.links {
		if l.ID != id {
			continue
		}
		old := u.links
		u.links = append(append([]*Link(nil), u.links[:i]...), u.links[i+1:]...)
		if err := u.saveLinks(); err != nil {
			u.links = old
			return err
		}
		return nil
	}
	return errNoUser
}

// Signup makes an account with a sign-up link. It is atomic: the link must still be live and
// the name free when the account is stored, so a link for one person can't make two. The
// password rules are the caller's (pwPolicy).
func (u *Users) Signup(code, name, pw string) (User, Link, error) {
	name, err := cleanName(name)
	if err != nil {
		return User{}, Link{}, err
	}
	if pw == "" {
		return User{}, Link{}, errPwEmpty
	}
	salt, hash, iter := newSalted(pw) // slow: outside the lock
	now := time.Now()
	u.mu.Lock()
	defer u.mu.Unlock()
	l := u.findLink(code, now)
	if l == nil {
		return User{}, Link{}, errLinkGone
	}
	if !u.nameFree(name, "") {
		return User{}, Link{}, errNameTaken
	}
	if len(u.list) >= maxUsers {
		return User{}, Link{}, errTooMany
	}
	x := &User{ID: u.newID(), Name: name, Salt: salt, Hash: hash, Iter: iter, Created: now, Quota: l.Quota, Via: l.ID}
	u.list = append(u.list, x)
	if err := u.save(); err != nil {
		u.list = u.list[:len(u.list)-1]
		return User{}, Link{}, err
	}
	l.Users = append(slices.Clone(l.Users), x.ID)
	if err := u.saveLinks(); err != nil {
		// The account exists; only the file lags (the link is used up in memory until a restart).
		log.Printf("sign-up link %s: saving its use: %v", l.ID, err)
	}
	c := *l
	c.Live = u.linkLive(l, now)
	return *x, c, nil
}

// ---------- sessions ----------

type Session struct {
	U    string    `json:"u"`
	Exp  time.Time `json:"exp"`
	App  int       `json:"app,omitempty"`  // Android app versionCode on this device (told by /api/app)
	AppV string    `json:"appV,omitempty"` // and its versionName
}

// loadSessions reads sessions.json, upgrading the old {token: expiry} form (from before family
// accounts) so those devices stay logged in as the owner.
func loadSessions(path, ownerID string) map[string]Session {
	out := map[string]Session{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	if json.Unmarshal(b, &out) == nil {
		return out
	}
	var old map[string]time.Time
	out = map[string]Session{}
	if json.Unmarshal(b, &old) == nil && ownerID != "" {
		for tok, exp := range old {
			out[tok] = Session{U: ownerID, Exp: exp}
		}
	}
	return out
}
