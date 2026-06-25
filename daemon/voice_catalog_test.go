package daemon

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	catalogMinInterval = time.Millisecond
	catalogJitterMax = time.Millisecond
	catalogBackoffStart = time.Millisecond
	catalogBackoffCap = 5 * time.Millisecond
	os.Exit(m.Run())
}

type nopLogger struct{}

func (nopLogger) Infof(string, ...any)  {}
func (nopLogger) Warnf(string, ...any)  {}
func (nopLogger) Debugf(string, ...any) {}

type fakeFetcher struct {
	mu sync.Mutex

	liked     []catalogItem
	artists   []catalogItem
	playlists []catalogItem
	albums    []catalogItem
	plTracks  map[string][]catalogItem

	topArtists_   []catalogItem
	likedTotalPad int
	likedErr      func(offset int) error

	calls      int
	likedCalls int
}

func (f *fakeFetcher) likedTracks(_ context.Context, offset, limit int, force bool) ([]catalogItem, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.likedCalls++
	if f.likedErr != nil {
		err := f.likedErr(offset)
		if err != nil {
			f.likedErr = nil
			return nil, 0, err
		}
	}
	return page(f.liked, offset, limit), len(f.liked) + f.likedTotalPad, nil
}
func (f *fakeFetcher) libraryList(_ context.Context, filter string, offset, limit int, force bool) ([]catalogItem, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	var src []catalogItem
	switch filter {
	case "Playlists":
		src = f.playlists
	case "Artists":
		src = f.artists
	case "Albums":
		src = f.albums
	}
	return page(src, offset, limit), len(src), nil
}
func (f *fakeFetcher) playlistTracks(_ context.Context, uri string, offset, limit int, force bool) ([]catalogItem, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	src := f.plTracks[uri]
	return page(src, offset, limit), len(src), nil
}
func (f *fakeFetcher) topArtists(_ context.Context, offset, limit int, force bool) ([]catalogItem, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return page(f.topArtists_, offset, limit), len(f.topArtists_), nil
}

func page(src []catalogItem, offset, limit int) []catalogItem {
	if offset >= len(src) {
		return nil
	}
	end := offset + limit
	if end > len(src) {
		end = len(src)
	}
	return src[offset:end]
}

func newTestSyncer(t *testing.T, f *fakeFetcher) (*catalogSyncer, *routedIndex) {
	t.Helper()
	var published *routedIndex
	s := newCatalogSyncer(f, fakeG2P(), t.TempDir(), nopLogger{}, func(idx *routedIndex) { published = idx })
	return s, published
}

func TestCatalogSyncBasic(t *testing.T) {
	f := &fakeFetcher{
		liked: []catalogItem{
			{Name: "Heartless", Artist: "Kanye West", Uri: "spotify:track:h"},
			{Name: "Vultures", Artist: "Kanye West", Uri: "spotify:track:v"},
		},
		artists:   []catalogItem{{Name: "Drake", Uri: "spotify:artist:d"}},
		playlists: []catalogItem{{Name: "Workout", Uri: "spotify:playlist:w"}},
		albums:    []catalogItem{{Name: "Graduation", Artist: "Kanye West", Uri: "spotify:album:g"}},
		plTracks: map[string][]catalogItem{
			"spotify:playlist:w": {{Name: "Stronger", Artist: "Kanye West", Uri: "spotify:track:s"}},
		},
	}
	s, _ := newTestSyncer(t, f)
	idx, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(idx.Tracks) != 3 {
		t.Errorf("tracks = %d, want 3", len(idx.Tracks))
	}
	if len(idx.Artists) != 1 || len(idx.Playlists) != 1 || len(idx.Albums) != 1 {
		t.Errorf("artists/playlists/albums = %d/%d/%d, want 1/1/1", len(idx.Artists), len(idx.Playlists), len(idx.Albums))
	}
	if idx.Tracks[0].Ipa == "" {
		t.Errorf("track IPA not computed")
	}
	if _, ok := s.loadCachedIndex(); !ok {
		t.Errorf("index not cached")
	}
}

func TestCatalogSyncPagination(t *testing.T) {
	var liked []catalogItem
	for i := 0; i < catalogPageSize*2+7; i++ {
		liked = append(liked, catalogItem{Name: "T", Uri: "spotify:track:" + string(rune('a'+i%26)) + string(rune('0'+i/26))})
	}
	f := &fakeFetcher{liked: liked}
	s, _ := newTestSyncer(t, f)
	idx, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(idx.Tracks) != len(liked) {
		t.Errorf("paginated tracks = %d, want %d", len(idx.Tracks), len(liked))
	}
}

func TestCatalogSyncRemintRetry(t *testing.T) {
	f := &fakeFetcher{
		liked:    []catalogItem{{Name: "Heartless", Uri: "spotify:track:h"}},
		likedErr: func(offset int) error { return &pathfinderError{Status: 401, msg: "401"} },
	}
	s, _ := newTestSyncer(t, f)
	idx, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(idx.Tracks) != 1 {
		t.Errorf("after re-mint retry, tracks = %d, want 1", len(idx.Tracks))
	}
}

func TestCatalogSyncPersistedQueryStop(t *testing.T) {
	f := &fakeFetcher{
		liked: []catalogItem{{Name: "X", Uri: "spotify:track:x"}},
		likedErr: func(offset int) error {
			return &pathfinderError{Status: 200, PersistedQuery: true, msg: "PersistedQueryNotFound"}
		},
		artists: []catalogItem{{Name: "Drake", Uri: "spotify:artist:d"}},
	}
	s, _ := newTestSyncer(t, f)
	idx, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run should not hard-error on a stopped entity: %v", err)
	}
	if len(idx.Tracks) != 0 {
		t.Errorf("liked stopped on persisted-query, tracks = %d, want 0", len(idx.Tracks))
	}
	if len(idx.Artists) != 1 {
		t.Errorf("artists should still sync after liked stopped, got %d", len(idx.Artists))
	}
}

func TestCatalogSyncRetryAfter(t *testing.T) {
	f := &fakeFetcher{
		liked: []catalogItem{{Name: "X", Uri: "spotify:track:x"}},
		likedErr: func(offset int) error {
			return &pathfinderError{Status: 429, RetryAfter: 10 * time.Millisecond, msg: "429"}
		},
	}
	s, _ := newTestSyncer(t, f)
	start := time.Now()
	idx, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(idx.Tracks) != 1 {
		t.Errorf("after 429 backoff, tracks = %d, want 1", len(idx.Tracks))
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Errorf("did not honor Retry-After delay")
	}
}

func TestCatalogCheckpointResume(t *testing.T) {
	f := &fakeFetcher{
		liked:   []catalogItem{{Name: "X", Uri: "spotify:track:x"}},
		artists: []catalogItem{{Name: "Drake", Uri: "spotify:artist:d"}},
	}
	dir := t.TempDir()
	s := newCatalogSyncer(f, fakeG2P(), dir, nopLogger{}, func(*routedIndex) {})
	cp := &checkpoint{Version: catalogIndexVersion, LikedDone: true, Liked: []catalogItem{{Name: "Cached", Uri: "spotify:track:c"}}}
	s.saveCheckpoint(cp)

	idx, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(idx.Tracks) != 1 || idx.Tracks[0].Name != "Cached" {
		t.Errorf("checkpoint not resumed, tracks = %+v", idx.Tracks)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("5"); d != 5*time.Second {
		t.Errorf("parseRetryAfter(5) = %v, want 5s", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("parseRetryAfter(empty) = %v, want 0", d)
	}
	if d := parseRetryAfter("Wed, 21 Oct 2099 07:28:00 GMT"); d != 0 {
		t.Errorf("parseRetryAfter(http-date) = %v, want 0 (unsupported)", d)
	}
}
