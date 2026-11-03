package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ThumbPending = 0
	ThumbOK      = 1
	ThumbFailed  = 2
	ThumbNone    = 3 // type we can't thumbnail (video, HEIC, ...)
)

// Item is one stored file. Files are content-addressed, so a photo two family members both
// have is kept once and lists both as owners. Fam puts it in the shared Family album.
type Item struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Ext       string    `json:"ext"`
	Kind      string    `json:"kind"`
	Size      int64     `json:"size"`
	Taken     time.Time `json:"taken"`
	Added     time.Time `json:"added"`
	Rel       string    `json:"rel"`
	W         int       `json:"w,omitempty"`
	H         int       `json:"h,omitempty"`
	Thumb     int       `json:"thumb"`
	Crashed   bool      `json:"crashed,omitempty"`   // thumbnailing once killed the process: never retry
	NoPreview bool      `json:"nopreview,omitempty"` // preview generation crashed: serve original instead
	Dims      bool      `json:"dims,omitempty"`      // W/H are verified as displayed (rotation applied)
	Owners    []string  `json:"owners,omitempty"`    // user ids whose library has it
	Fam       bool      `json:"fam,omitempty"`       // in the Family album
	FamBy     string    `json:"famBy,omitempty"`     // who added it to the Family album
}

func (it *Item) OwnedBy(uid string) bool   { return slices.Contains(it.Owners, uid) }
func (it *Item) VisibleTo(uid string) bool { return it.Fam || it.OwnedBy(uid) }

// Row flags (index 9).
const (
	RowMine     = 1 // in the asking user's own library
	RowFamily   = 2 // in the Family album
	RowSharedMe = 4 // the asking user put it in the Family album
)

// RowFor is the compact form used by the clients:
// [id, kind, takenUnix, thumb, ext, name, size, width, height, flags, sharedBy]
// (flags: RowMine | RowFamily | RowSharedMe)
// (width/height 0 = unknown; sharedBy = name of who put it in the Family album, "" if not shared).
func (s *Store) RowFor(it *Item, uid string) []any {
	flags, by := 0, ""
	if it.OwnedBy(uid) {
		flags |= RowMine
	}
	if it.Fam {
		flags |= RowFamily
		if it.FamBy == uid {
			flags |= RowSharedMe
		}
		if s.names != nil {
			by = s.names(it.FamBy)
		}
	}
	return []any{it.ID, it.Kind, it.Taken.Unix(), it.Thumb, it.Ext, it.Name, it.Size, it.W, it.H, flags, by}
}

// Event records that an item changed. Who gets told: its owners before and after the change,
// and everybody if it was or is in the Family album. The change itself is read at poll time.
type Event struct {
	Seq   uint64
	ID    string
	Users []string
	All   bool
}

// Change is what a client receives: "add" (insert or update) with the row, or "del".
type Change struct {
	Op  string `json:"op"`
	Row []any  `json:"row,omitempty"`
	ID  string `json:"id,omitempty"`
}

const maxEvents = 2000

var (
	errNotFound  = errors.New("not found")
	errForbidden = errors.New("not allowed")
)

// Store is the whole library index, held in memory and persisted as one JSON file.
// Writes are batched (at most one every 2 s) to spare the SD card; originals are fsynced
// before they're indexed and a startup reconcile re-adds anything the index missed.
type Store struct {
	mu      sync.RWMutex
	flushMu sync.Mutex
	path    string
	items   map[string]*Item
	cache   map[string][]byte // per-user /api/items response
	dirty   bool
	seq     uint64
	events  []Event
	kick    chan struct{}
	names   func(uid string) string // user id -> display name
}

func OpenStore(dir string) (*Store, error) {
	var rb [4]byte
	rand.Read(rb[:])
	s := &Store{
		path:  filepath.Join(dir, "index.json"),
		items: map[string]*Item{},
		cache: map[string][]byte{},
		seq:   uint64(binary.LittleEndian.Uint32(rb[:])) + 1, // random per process: stale clients see a mismatch
		kick:  make(chan struct{}, 1),
	}
	var list []*Item
	if err := readJSON(s.path, &list); err != nil && !isNotExist(err) {
		return nil, err
	}
	for _, it := range list {
		if it != nil && it.ID != "" {
			s.items[it.ID] = it
		}
	}
	return s, nil
}

// SetNames tells the store how to show who shared a Family photo.
func (s *Store) SetNames(f func(string) string) {
	s.mu.Lock()
	s.names = f
	s.cache = map[string][]byte{}
	s.mu.Unlock()
}

// SharerRenamed tells every client about the new "shared by" name on uid's Family photos.
func (s *Store) SharerRenamed(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = map[string][]byte{}
	for id, it := range s.items {
		if it.Fam && it.FamBy == uid {
			s.changed(id, it, it)
		}
	}
}

// StartFlusher runs the background writer.
func (s *Store) StartFlusher() {
	go func() {
		for range s.kick {
			time.Sleep(2 * time.Second)
			if err := s.Flush(); err != nil {
				log.Printf("writing index failed (will retry): %v", err)
				time.Sleep(8 * time.Second)
				s.poke()
			}
		}
	}()
}

func (s *Store) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func union(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// changed records that item id changed from before to after (nil = didn't exist / gone).
// Caller holds s.mu for writing.
func (s *Store) changed(id string, before, after *Item) {
	ev := Event{ID: id}
	if before != nil {
		ev.Users, ev.All = union(ev.Users, before.Owners), ev.All || before.Fam
	}
	if after != nil {
		ev.Users, ev.All = union(ev.Users, after.Owners), ev.All || after.Fam
	}
	s.cache = map[string][]byte{}
	s.dirty = true
	s.seq++
	ev.Seq = s.seq
	s.events = append(s.events, ev)
	if len(s.events) > maxEvents+maxEvents/4 {
		s.events = append([]Event(nil), s.events[len(s.events)-maxEvents:]...)
	}
	s.poke()
}

// Flush writes the index now if anything changed. On failure the store stays dirty.
func (s *Store) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	list := make([]Item, 0, len(s.items))
	for _, it := range s.sorted() {
		list = append(list, *it)
	}
	s.dirty = false
	s.mu.Unlock()
	if err := writeJSON(s.path, list); err != nil { // slow fsync happens outside the lock
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// Stats: the user's own library (count, bytes) and the Family album size.
func (s *Store) Stats(uid string) (mine int, bytes int64, fam int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, it := range s.items {
		if it.OwnedBy(uid) {
			mine++
			bytes += it.Size
		}
		if it.Fam {
			fam++
		}
	}
	return
}

// Used is the size of the user's library in bytes: what their storage limit counts. A file two
// people have counts for both (each could keep it if the other deleted theirs).
func (s *Store) Used(uid string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n int64
	for _, it := range s.items {
		if it.OwnedBy(uid) {
			n += it.Size
		}
	}
	return n
}

// Totals: number and size of all stored files (each counted once).
func (s *Store) Totals() (n int, bytes int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, it := range s.items {
		n++
		bytes += it.Size
	}
	return
}

// Shares splits the stored bytes between people for the admin page's disk chart: a file two
// people have counts half for each, so the parts add up to Totals. famOnly is what's only in the
// Family album (shared by someone who was removed).
func (s *Store) Shares() (per map[string]int64, famOnly int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	per = map[string]int64{}
	for _, it := range s.items {
		if len(it.Owners) == 0 {
			famOnly += it.Size
			continue
		}
		part := it.Size / int64(len(it.Owners))
		for i, uid := range it.Owners {
			if i == 0 {
				per[uid] += it.Size - part*int64(len(it.Owners)-1) // the remainder, so nothing is lost
			} else {
				per[uid] += part
			}
		}
	}
	return
}

// Get returns a copy (its Owners slice is never modified in place, so sharing it is safe).
func (s *Store) Get(id string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.items[id]
	if !ok {
		return Item{}, false
	}
	return *it, true
}

func (s *Store) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.items))
	for id := range s.items {
		ids = append(ids, id)
	}
	return ids
}

// sorted: newest first, ties by id. Caller holds s.mu.
func (s *Store) sorted() []*Item {
	list := make([]*Item, 0, len(s.items))
	for _, it := range s.items {
		list = append(list, it)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Taken.Equal(list[j].Taken) {
			return list[i].Taken.After(list[j].Taken)
		}
		return list[i].ID < list[j].ID
	})
	return list
}

// timeOK reports whether t can be stored (encoding/json rejects years outside 0..9999).
func timeOK(t time.Time) bool { return t.Year() >= 1 && t.Year() <= 9999 }

func (s *Store) Add(it Item) error {
	if !timeOK(it.Taken) || !timeOK(it.Added) {
		return fmt.Errorf("invalid time for %s", it.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[it.ID]; exists {
		return fmt.Errorf("duplicate id %s", it.ID)
	}
	it.Owners = slices.Clone(it.Owners)
	s.items[it.ID] = &it
	s.changed(it.ID, nil, &it)
	return nil
}

// Delete drops an item from the index for everybody.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return false
	}
	delete(s.items, id)
	s.changed(id, it, nil)
	return true
}

// Update mutates an item. fn must not modify Owners in place (assign a new slice instead).
// A change clients can see (thumbnail state, size, sharing, owners) is published as an event.
func (s *Store) Update(id string, fn func(*Item)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return false
	}
	before := *it
	fn(it)
	it.ID = id
	if it.Thumb != before.Thumb || it.W != before.W || it.H != before.H || it.Fam != before.Fam ||
		it.FamBy != before.FamBy || !slices.Equal(it.Owners, before.Owners) || it.Name != before.Name ||
		!it.Taken.Equal(before.Taken) {
		s.changed(id, &before, it)
	} else {
		s.dirty = true
		s.poke()
	}
	return true
}

func (s *Store) SetThumb(id string, state int) {
	s.Update(id, func(it *Item) { it.Thumb = state })
}

func (s *Store) IDsWithThumb(state int) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for _, it := range s.sorted() { // newest first: what you see first gets thumbnailed first
		if it.Thumb == state {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// ListJSON returns {"seq": N, "items": [row, ...]} with everything uid may see.
func (s *Store) ListJSON(uid string) []byte {
	s.mu.RLock()
	if c := s.cache[uid]; c != nil {
		s.mu.RUnlock()
		return c
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache[uid] == nil {
		rows := [][]any{}
		for _, it := range s.sorted() {
			if it.VisibleTo(uid) {
				rows = append(rows, s.RowFor(it, uid))
			}
		}
		s.cache[uid], _ = json.Marshal(map[string]any{"seq": s.seq, "items": rows})
	}
	return s.cache[uid]
}

// Changes returns what changed for uid after `since`, one entry per item with its current
// state. reset=true means the client must reload /api/items.
func (s *Store) Changes(since uint64, has bool, uid string) (seq uint64, reset bool, ch []Change) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seq = s.seq
	ch = []Change{}
	if !has || since == s.seq {
		return
	}
	if since > s.seq || len(s.events) == 0 || s.events[0].Seq > since+1 {
		return seq, true, ch
	}
	i := sort.Search(len(s.events), func(i int) bool { return s.events[i].Seq > since })
	var ids []string
	seen := map[string]int{}
	for _, ev := range s.events[i:] {
		if !ev.All && !slices.Contains(ev.Users, uid) {
			continue
		}
		if k, ok := seen[ev.ID]; ok { // keep the latest position
			ids[k] = ""
		}
		seen[ev.ID] = len(ids)
		ids = append(ids, ev.ID)
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if it, ok := s.items[id]; ok && it.VisibleTo(uid) {
			ch = append(ch, Change{Op: "add", Row: s.RowFor(it, uid)})
		} else {
			ch = append(ch, Change{Op: "del", ID: id})
		}
	}
	return
}

// ---------- ownership & the Family album ----------

func without(list []string, x string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

// Claim puts an existing item into uid's library (same bytes uploaded by someone else, or
// "save to my photos" from the Family album). Returns whether it was newly added.
func (s *Store) Claim(id, uid string) (Item, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return Item{}, false, errNotFound
	}
	if it.OwnedBy(uid) {
		return *it, false, nil
	}
	before := *it
	it.Owners = append(slices.Clone(it.Owners), uid)
	s.changed(id, &before, it)
	return *it, true, nil
}

// Unown removes an item from uid's library (and from the Family album if uid put it there).
// When nobody has it any more it leaves the index: gone=true, and the caller trashes the files.
func (s *Store) Unown(id, uid string) (it Item, gone bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	if !ok || !p.OwnedBy(uid) {
		return Item{}, false, errNotFound
	}
	before := *p
	p.Owners = without(p.Owners, uid)
	if p.Fam && p.FamBy == uid {
		p.Fam, p.FamBy = false, ""
	}
	if len(p.Owners) == 0 && !p.Fam {
		delete(s.items, id)
		s.changed(id, &before, nil)
		return *p, true, nil
	}
	s.changed(id, &before, p)
	return *p, false, nil
}

// SetFamily adds an item to the Family album (uid must have it in their library) or takes it
// out (allowed for whoever added it, and admins). An item nobody owns that leaves the album is
// gone (its sharer was removed): the caller trashes the files.
func (s *Store) SetFamily(id, uid string, on, admin bool) (it Item, gone bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	if !ok || !p.VisibleTo(uid) {
		return Item{}, false, errNotFound
	}
	before := *p
	if on {
		if !p.OwnedBy(uid) {
			return *p, false, errForbidden
		}
		if p.Fam {
			return *p, false, nil
		}
		p.Fam, p.FamBy = true, uid
		s.changed(id, &before, p)
		return *p, false, nil
	}
	if !p.Fam {
		return *p, false, nil
	}
	if p.FamBy != uid && !admin {
		return *p, false, errForbidden
	}
	p.Fam, p.FamBy = false, ""
	if len(p.Owners) == 0 {
		delete(s.items, id)
		s.changed(id, &before, nil)
		return *p, true, nil
	}
	s.changed(id, &before, p)
	return *p, false, nil
}

// RemoveUser takes a person's library away. What they shared stays in the Family album;
// returns the items nobody has any more (the caller trashes their files).
func (s *Store) RemoveUser(uid string) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gone []Item
	for id, p := range s.items {
		if !p.OwnedBy(uid) {
			continue
		}
		before := *p
		p.Owners = without(p.Owners, uid)
		if len(p.Owners) == 0 && !p.Fam {
			delete(s.items, id)
			gone = append(gone, *p)
			s.changed(id, &before, nil)
		} else {
			s.changed(id, &before, p)
		}
	}
	return gone
}

// AssignUnowned gives items without owners or a Family share (libraries from before family
// accounts, files re-found on disk) to uid. Returns how many.
func (s *Store) AssignUnowned(uid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.items {
		if len(p.Owners) == 0 && !p.Fam {
			p.Owners = []string{uid}
			n++
		}
	}
	if n > 0 {
		s.cache = map[string][]byte{}
		s.dirty = true
		s.poke()
	}
	return n
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// ---------- "same photo, different bytes" detection ----------

// photoStem normalises an original file name for comparison: lower case, no extension,
// no copy counter ("IMG_1234(1).jpg" and "img_1234 (2).JPG" -> "img_1234").
func photoStem(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.TrimSuffix(s, filepath.Ext(s))
	if strings.HasSuffix(s, ")") {
		if i := strings.LastIndex(s, "("); i > 0 {
			if _, err := strconv.Atoi(s[i+1 : len(s)-1]); err == nil {
				s = strings.TrimSpace(s[:i])
			}
		}
	}
	return s
}

// sameStem also accepts the truncated names Google Takeout produces for long file names.
func sameStem(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(a) >= 30 && len(b) >= 30 {
		return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
	}
	return false
}

// sameMoment: within 2 s, or a whole number of hours apart (up to 14 h) when one copy's
// time was read in a different time zone.
func sameMoment(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	if d <= 2*time.Second {
		return true
	}
	if d <= 14*time.Hour+2*time.Second {
		r := d % time.Hour
		return r <= 2*time.Second || r >= time.Hour-2*time.Second
	}
	return false
}

// FindSimilar returns an item in uid's library that is almost certainly the same photo or video
// as the one described: same kind, same original file name and the same capture moment.
// Different photos taken in the same second (bursts) have different names, so they are kept.
// Only the user's own library counts: two phones can name different photos alike (e.g.
// 20261031_181500.jpg) at the same moment.
func (s *Store) FindSimilar(name, kind string, taken time.Time, uid string) (Item, bool) {
	stem := photoStem(name)
	if len(stem) < 3 {
		return Item{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, it := range s.items {
		if it.Kind == kind && it.OwnedBy(uid) && sameMoment(it.Taken, taken) && sameStem(photoStem(it.Name), stem) {
			return *it, true
		}
	}
	return Item{}, false
}
