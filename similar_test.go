package main

import (
	"testing"
	"time"
)

func TestFindSimilar(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2024, 5, 12, 12, 34, 56, 0, time.UTC)
	s.Add(Item{ID: "a", Name: "IMG_20240512_123456.jpg", Kind: "image", Taken: t0, Added: t0, Owners: []string{"me"}})
	s.Add(Item{ID: "b", Name: "PXL_20240512_123456789.PORTRAIT.ORIGINAL.jpg", Kind: "image", Taken: t0, Added: t0, Owners: []string{"me"}})
	s.Add(Item{ID: "v", Name: "VID_20240512_123456.mp4", Kind: "video", Taken: t0, Added: t0, Owners: []string{"me"}})
	cases := []struct {
		name, kind string
		taken      time.Time
		want       string
	}{
		{"IMG_20240512_123456.jpg", "image", t0, "a"},
		{"img_20240512_123456(1).JPG", "image", t0.Add(time.Second), "a"},  // copy counter, 1 s off
		{"IMG_20240512_123456.jpg", "image", t0.Add(2 * time.Hour), "a"},   // time-zone shift
		{"PXL_20240512_123456789.PORTRAIT.ORIGINA.jpg", "image", t0, "b"},  // Takeout truncation
		{"IMG_20240512_123457.jpg", "image", t0, ""},                       // burst: other name
		{"IMG_20240512_123456.jpg", "image", t0.Add(10 * time.Minute), ""}, // other moment
		{"IMG_20240512_123456.jpg", "video", t0, ""},                       // other kind
		{"VID_20240512_123456.mp4", "video", t0, "v"},
	}
	// someone else's photo with the same name and moment is a different photo
	if _, ok := s.FindSimilar("IMG_20240512_123456.jpg", "image", t0, "other"); ok {
		t.Error("matched another person's library")
	}
	for _, c := range cases {
		it, ok := s.FindSimilar(c.name, c.kind, c.taken, "me")
		got := ""
		if ok {
			got = it.ID
		}
		if got != c.want {
			t.Errorf("FindSimilar(%q, %s, %v) = %q, want %q", c.name, c.kind, c.taken, got, c.want)
		}
	}
}
