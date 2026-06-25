package daemon

import (
	"context"
	"fmt"
	"testing"
)

// helpers for the refresh tests

func baseFakeLibrary() *fakeFetcher {
	return &fakeFetcher{
		liked: []catalogItem{
			{Name: "A", Uri: "spotify:track:a"},
			{Name: "B", Uri: "spotify:track:b"},
			{Name: "C", Uri: "spotify:track:c"},
		},
		artists:     []catalogItem{{Name: "Drake", Uri: "spotify:artist:d"}},
		playlists:   []catalogItem{{Name: "Workout", Uri: "spotify:playlist:w"}},
		albums:      []catalogItem{{Name: "Graduation", Uri: "spotify:album:g"}},
		topArtists_: []catalogItem{{Name: "Kanye West", Uri: "spotify:artist:k"}},
		plTracks: map[string][]catalogItem{
			"spotify:playlist:w": {{Name: "Stronger", Uri: "spotify:track:s"}},
		},
	}
}

func hasURI(entries []indexEntry, uri string) bool {
	for _, e := range entries {
		if e.Uri == uri {
			return true
		}
	}
	return false
}

func runThenRefresh(t *testing.T, f *fakeFetcher, mutate func()) (*routedIndex, bool) {
	t.Helper()
	s := newCatalogSyncer(f, fakeG2P(), t.TempDir(), nopLogger{}, func(*routedIndex) {})
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mutate()
	f.likedCalls = 0
	idx, changed, err := s.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return idx, changed
}

// tests

func TestCatalogRefreshNoChange(t *testing.T) {
	f := baseFakeLibrary()
	idx, changed := runThenRefresh(t, f, func() {})
	if changed || idx != nil {
		t.Fatalf("expected no change, got changed=%v idx=%v", changed, idx)
	}
}

func TestCatalogRefreshNoFalsePositiveOnFilterGap(t *testing.T) {
	f := baseFakeLibrary()
	f.likedTotalPad = 7
	idx, changed := runThenRefresh(t, f, func() {})
	if changed || idx != nil {
		t.Fatalf("filter-gap false-positive: refresh reported a change with no real edit (idx=%v)", idx)
	}
}

func TestCatalogRefreshLikedAdd(t *testing.T) {
	f := baseFakeLibrary()
	idx, changed := runThenRefresh(t, f, func() {
		f.liked = append([]catalogItem{{Name: "NEW", Uri: "spotify:track:new"}}, f.liked...)
	})
	if !changed {
		t.Fatal("expected change after adding a liked song")
	}
	if !hasURI(idx.Tracks, "spotify:track:new") {
		t.Fatal("new liked song not in the index")
	}
	if !hasURI(idx.Liked, "spotify:track:new") || len(idx.Liked) != 4 {
		t.Fatalf("liked list not updated: %d entries", len(idx.Liked))
	}
}

func TestCatalogRefreshLikedAddIsIncremental(t *testing.T) {
	var big []catalogItem
	for i := 0; i < catalogPageSize*3; i++ {
		big = append(big, catalogItem{Name: fmt.Sprintf("t%d", i), Uri: fmt.Sprintf("spotify:track:%d", i)})
	}
	f := &fakeFetcher{liked: big}
	idx, changed := runThenRefresh(t, f, func() {
		f.liked = append([]catalogItem{{Name: "NEW", Uri: "spotify:track:new"}}, f.liked...)
	})
	if !changed || !hasURI(idx.Tracks, "spotify:track:new") {
		t.Fatal("incremental add not reflected")
	}
	if f.likedCalls > 2 {
		t.Fatalf("incremental add fetched %d liked pages, want <=2 (not a full re-fetch)", f.likedCalls)
	}
}

func TestCatalogRefreshLikedRemoveMiddle(t *testing.T) {
	f := baseFakeLibrary()
	idx, changed := runThenRefresh(t, f, func() {
		f.liked = []catalogItem{{Name: "A", Uri: "spotify:track:a"}, {Name: "C", Uri: "spotify:track:c"}}
	})
	if !changed {
		t.Fatal("expected change after removing a liked song")
	}
	if hasURI(idx.Tracks, "spotify:track:b") {
		t.Fatal("removed liked song still in the index")
	}
	if len(idx.Liked) != 2 {
		t.Fatalf("liked list = %d, want 2", len(idx.Liked))
	}
}

func TestCatalogRefreshLikedSwapSameCount(t *testing.T) {
	f := baseFakeLibrary()
	idx, changed := runThenRefresh(t, f, func() {
		f.liked = []catalogItem{
			{Name: "NEW", Uri: "spotify:track:new"},
			{Name: "A", Uri: "spotify:track:a"},
			{Name: "B", Uri: "spotify:track:b"},
		}
	})
	if !changed {
		t.Fatal("expected change on a same-count swap")
	}
	if !hasURI(idx.Tracks, "spotify:track:new") || hasURI(idx.Tracks, "spotify:track:c") {
		t.Fatal("same-count swap not reconciled (new missing or old retained)")
	}
}

func TestCatalogRefreshPlaylistAdded(t *testing.T) {
	f := baseFakeLibrary()
	idx, changed := runThenRefresh(t, f, func() {
		f.playlists = append(f.playlists, catalogItem{Name: "Chill", Uri: "spotify:playlist:c"})
		f.plTracks["spotify:playlist:c"] = []catalogItem{{Name: "Lofi", Uri: "spotify:track:lofi"}}
	})
	if !changed {
		t.Fatal("expected change after adding a playlist")
	}
	if !hasURI(idx.Playlists, "spotify:playlist:c") {
		t.Fatal("new playlist name not indexed")
	}
	if !hasURI(idx.Tracks, "spotify:track:lofi") {
		t.Fatal("new playlist's track not indexed")
	}
}

func TestCatalogRefreshPlaylistTrackChange(t *testing.T) {
	f := baseFakeLibrary()
	idx, changed := runThenRefresh(t, f, func() {
		f.plTracks["spotify:playlist:w"] = append(f.plTracks["spotify:playlist:w"],
			catalogItem{Name: "Power", Uri: "spotify:track:power"})
	})
	if !changed {
		t.Fatal("expected change after adding a track to a playlist")
	}
	if !hasURI(idx.Tracks, "spotify:track:power") {
		t.Fatal("added playlist track not indexed")
	}
}

func TestCatalogRefreshPlaylistRemoved(t *testing.T) {
	f := baseFakeLibrary()
	idx, changed := runThenRefresh(t, f, func() {
		f.playlists = nil
	})
	if !changed {
		t.Fatal("expected change after removing a playlist")
	}
	if hasURI(idx.Playlists, "spotify:playlist:w") {
		t.Fatal("removed playlist still indexed")
	}
	if hasURI(idx.Tracks, "spotify:track:s") {
		t.Fatal("removed playlist's track still in the index")
	}
}

func TestCatalogRefreshArtistsAndAlbums(t *testing.T) {
	f := baseFakeLibrary()
	idx, changed := runThenRefresh(t, f, func() {
		f.artists = append(f.artists, catalogItem{Name: "SZA", Uri: "spotify:artist:z"})
		f.albums = append(f.albums, catalogItem{Name: "CTRL", Uri: "spotify:album:ctrl"})
	})
	if !changed {
		t.Fatal("expected change after adding an artist + album")
	}
	if !hasURI(idx.Artists, "spotify:artist:z") {
		t.Fatal("new followed artist not indexed")
	}
	if !hasURI(idx.Albums, "spotify:album:ctrl") {
		t.Fatal("new album not indexed")
	}
}

func TestCatalogRefreshNoPriorSyncFallsBackToFull(t *testing.T) {
	f := baseFakeLibrary()
	s := newCatalogSyncer(f, fakeG2P(), t.TempDir(), nopLogger{}, func(*routedIndex) {})
	idx, changed, err := s.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !changed || idx == nil || len(idx.Tracks) == 0 {
		t.Fatalf("expected a full sync fallback, got changed=%v idx=%v", changed, idx)
	}
}
