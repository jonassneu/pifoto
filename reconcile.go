package main

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cleanupTemp removes leftovers of interrupted uploads and thumbnail writes.
func cleanupTemp(dir string) {
	if ents, err := os.ReadDir(filepath.Join(dir, "tmp")); err == nil {
		for _, e := range ents {
			os.RemoveAll(filepath.Join(dir, "tmp", e.Name()))
		}
	}
	for _, sub := range []string{"thumbs", "previews"} {
		m, _ := filepath.Glob(filepath.Join(dir, sub, "*.tmp"))
		for _, p := range m {
			os.Remove(p)
		}
	}
}

func validID(id string) bool {
	if len(id) != 20 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// reconcile makes the index match originals/ on disk: files missing from the index (e.g. the
// index write after an upload was lost in a power cut) are re-added; entries whose original is
// gone (e.g. deleted just before a crash) are dropped.
func reconcile(dir string, st *Store) (added, removed int) {
	seen := map[string]bool{}
	filepath.WalkDir(filepath.Join(dir, "originals"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		base := d.Name()
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(base), "."))
		id := strings.TrimSuffix(base, filepath.Ext(base))
		if !validID(id) || kindOf(ext) == "" {
			return nil
		}
		seen[id] = true
		if _, ok := st.Get(id); ok {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		taken, info, ok := takenFor(p, ext, "", 0)
		if !ok {
			taken = folderTime(rel, fi.ModTime())
		}
		it := Item{ID: id, Name: base, Ext: ext, Kind: kindOf(ext), Size: fi.Size(), Taken: taken,
			Added: fi.ModTime(), Rel: rel}
		it.W, it.H = info.Displayed()
		switch {
		case !CanThumb(ext):
			it.Thumb = ThumbNone
		case fileExists(filepath.Join(dir, "thumbs", id+".jpg")):
			it.Thumb = ThumbOK
		}
		if st.Add(it) == nil {
			added++
		}
		return nil
	})
	for _, id := range st.IDs() {
		it, ok := st.Get(id)
		if !ok || seen[id] || fileExists(filepath.Join(dir, it.Rel)) {
			continue
		}
		st.Delete(id)
		os.Remove(filepath.Join(dir, "thumbs", id+".jpg"))
		os.Remove(filepath.Join(dir, "previews", id+".jpg"))
		removed++
	}
	if added+removed > 0 {
		log.Printf("reconcile: re-added %d, dropped %d index entries", added, removed)
	}
	return
}

// folderTime: originals/YYYY/MM/... → the file mtime if it falls in that month, else the 1st at noon.
func folderTime(rel string, mtime time.Time) time.Time {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 4 {
		y, e1 := strconv.Atoi(parts[1])
		m, e2 := strconv.Atoi(parts[2])
		if e1 == nil && e2 == nil && y >= 1 && y <= 9999 && m >= 1 && m <= 12 {
			if mtime.Year() == y && int(mtime.Month()) == m {
				return mtime
			}
			return time.Date(y, time.Month(m), 1, 12, 0, 0, 0, time.Local)
		}
	}
	return mtime
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
