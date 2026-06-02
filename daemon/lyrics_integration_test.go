package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// URL fields left empty, test must set them or it hits production
func newTestLyricsProviderForHTTP() *LyricsProvider {
	return &LyricsProvider{
		log:       &librespot.NullLogger{},
		client:    &http.Client{Timeout: 5 * time.Second},
		lpClient: &http.Client{Timeout: 5 * time.Second},
		cache:     make(map[string]*LyricsResult),
	}
}

// getPrimaryToken - token acquisition + caching + invalidation

func TestGetPrimaryToken_FetchesParsesAndCachesNewToken(t *testing.T) {
	t.Parallel()

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"the-token","app_id":"web-desktop-app-v1.0"}}}`))
	}))
	t.Cleanup(srv.Close)

	lp := newTestLyricsProviderForHTTP()
	lp.lpTokenURL = srv.URL

	got, err := lp.getPrimaryToken(context.Background())
	if err != nil {
		t.Fatalf("getPrimaryToken: %v", err)
	}
	if got != "the-token" {
		t.Errorf("token: got %q want %q", got, "the-token")
	}
	if atomic.LoadInt32(&requests) != 1 {
		t.Errorf("expected exactly 1 token fetch, got %d", requests)
	}
	if lp.lpToken != "the-token" {
		t.Errorf("token not cached on provider; got %q", lp.lpToken)
	}
	// lpExp must be in the future (8min window).
	if lp.lpExp.IsZero() || time.Until(lp.lpExp) <= 0 {
		t.Errorf("lpExp should be in the future, got %v (now=%v)", lp.lpExp, time.Now())
	}
}

func TestGetPrimaryToken_ReusesCachedTokenWithinExpiry(t *testing.T) {
	t.Parallel()

	// cache hit, second call within the 8-min window must not hit the network
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"the-token"}}}`))
	}))
	t.Cleanup(srv.Close)

	lp := newTestLyricsProviderForHTTP()
	lp.lpTokenURL = srv.URL

	if _, err := lp.getPrimaryToken(context.Background()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := lp.getPrimaryToken(context.Background()); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("expected 1 token fetch (cached on 2nd call), got %d", got)
	}
}

func TestGetPrimaryToken_ExpiredTokenTriggersRefresh(t *testing.T) {
	t.Parallel()

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"token-` + string(rune('0'+n)) + `"}}}`))
	}))
	t.Cleanup(srv.Close)

	lp := newTestLyricsProviderForHTTP()
	lp.lpTokenURL = srv.URL

	first, err := lp.getPrimaryToken(context.Background())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if first != "token-1" {
		t.Errorf("first token: got %q want %q", first, "token-1")
	}

	// force expiry by reaching past the deadline
	lp.lpMu.Lock()
	lp.lpExp = time.Now().Add(-time.Second)
	lp.lpMu.Unlock()

	second, err := lp.getPrimaryToken(context.Background())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second == first {
		t.Errorf("expired token reused: both calls returned %q", first)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Errorf("expected 2 token fetches, got %d", got)
	}
}

func TestGetPrimaryToken_Non200StatusCodeReturnsError(t *testing.T) {
	t.Parallel()

	// body wraps a non-200 status code
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":401},"body":{}}}`))
	}))
	t.Cleanup(srv.Close)

	lp := newTestLyricsProviderForHTTP()
	lp.lpTokenURL = srv.URL

	if _, err := lp.getPrimaryToken(context.Background()); err == nil {
		t.Error("status_code 401 should produce an error, got nil")
	}
}

func TestGetPrimaryToken_InvalidateForcesRefetch(t *testing.T) {
	t.Parallel()

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"t"}}}`))
	}))
	t.Cleanup(srv.Close)

	lp := newTestLyricsProviderForHTTP()
	lp.lpTokenURL = srv.URL

	if _, err := lp.getPrimaryToken(context.Background()); err != nil {
		t.Fatalf("first: %v", err)
	}
	lp.invalidatePrimaryToken()
	if _, err := lp.getPrimaryToken(context.Background()); err != nil {
		t.Fatalf("second: %v", err)
	}

	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Errorf("expected 2 fetches across invalidate, got %d", got)
	}
}

// FetchLyrics, end-to-end with primary source + LRCLIB fallback

// happyPrimarySubtitleResponse returns the deeply-nested JSON primary source
func happyPrimarySubtitleResponse() []byte {
	return []byte(`{"message":{"header":{"status_code":200},"body":{"macro_calls":{"track.subtitles.get":{"message":{"header":{"status_code":200},"body":{"subtitle_list":[{"subtitle":{"subtitle_body":"[{\"text\":\"Hello\",\"time\":{\"total\":0}},{\"text\":\"World\",\"time\":{\"total\":1.5}}]"}}]}}}}}}}`)
}

// noSyncedLyricsResponse returns the body=[] sentinel primary source ships
func noSyncedLyricsResponse() []byte {
	return []byte(`{"message":{"header":{"status_code":200},"body":{"macro_calls":{"track.subtitles.get":{"message":{"header":{"status_code":200},"body":[]}}}}}}`)
}

func TestFetchLyrics_PrimaryHappyPathReturnsSyncedLyrics(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "token.get") {
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"t"}}}`))
			return
		}
		_, _ = w.Write(happyPrimarySubtitleResponse())
	}))
	t.Cleanup(srv.Close)

	lp := newTestLyricsProviderForHTTP()
	lp.lpTokenURL = srv.URL + "/token.get"
	lp.lpSubtitleURL = srv.URL + "/macro.subtitles.get"
	lp.lrclibURL = "http://localhost:1/nope"

	result, err := lp.FetchLyrics(
		context.Background(),
		"track-abc",
		"Hello",
		"Test",
		"",
		60_000,
	)
	if err != nil {
		t.Fatalf("FetchLyrics: %v", err)
	}
	if result == nil {
		t.Fatal("got nil result")
	}
	if result.SyncType != "LINE_SYNCED" {
		t.Errorf("SyncType: got %q want LINE_SYNCED", result.SyncType)
	}
	if len(result.Lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(result.Lines))
	}
	cached, ok := lp.cache["track-abc"]
	if !ok || cached != result {
		t.Errorf("FetchLyrics did not cache the result by trackId")
	}
}

func TestFetchLyrics_PrimaryFailsFallsBackToLRCLIB(t *testing.T) {
	t.Parallel()

	// primary source returns the "no synced subtitles" sentinel; LRCLIB ships real synced lyrics.
	// End result is the LRCLIB-decoded LyricsResult
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "token.get") {
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"t"}}}`))
			return
		}
		_, _ = w.Write(noSyncedLyricsResponse())
	}))
	t.Cleanup(srv.Close)

	lrc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"instrumental": false,
			"syncedLyrics": "[00:00.000]LRC line one\n[00:05.000]LRC line two",
			"plainLyrics": ""
		}`))
	}))
	t.Cleanup(lrc.Close)

	lp := newTestLyricsProviderForHTTP()
	lp.lpTokenURL = srv.URL + "/token.get"
	lp.lpSubtitleURL = srv.URL + "/macro.subtitles.get"
	lp.lrclibURL = lrc.URL

	result, err := lp.FetchLyrics(
		context.Background(),
		"track-fallback",
		"Hello",
		"Test",
		"",
		60_000,
	)
	if err != nil {
		t.Fatalf("FetchLyrics: %v", err)
	}
	if result.SyncType != "LINE_SYNCED" {
		t.Errorf("SyncType: got %q want LINE_SYNCED (LRCLIB synced)", result.SyncType)
	}
	if len(result.Lines) != 2 {
		t.Errorf("expected 2 lines, got %d", len(result.Lines))
	}
	if result.Lines[0].Words != "LRC line one" {
		t.Errorf("expected LRCLIB content; got Lines[0]=%q", result.Lines[0].Words)
	}
}

func TestFetchLyrics_BothFailReturnsErrNoLyrics(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always return the no-lyrics sentinel
		if strings.Contains(r.URL.Path, "token.get") {
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"t"}}}`))
			return
		}
		_, _ = w.Write(noSyncedLyricsResponse())
	}))
	t.Cleanup(srv.Close)

	lrc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404) // LRCLIB also has nothing
	}))
	t.Cleanup(lrc.Close)

	lp := newTestLyricsProviderForHTTP()
	lp.lpTokenURL = srv.URL + "/token.get"
	lp.lpSubtitleURL = srv.URL + "/macro.subtitles.get"
	lp.lrclibURL = lrc.URL

	_, err := lp.FetchLyrics(context.Background(), "track-nothing", "X", "Y", "", 60_000)
	if err == nil {
		t.Fatal("expected ErrNoLyrics, got nil")
	}
	if err != ErrNoLyrics {
		t.Errorf("got err %q want ErrNoLyrics", err)
	}
}

func TestFetchLyrics_EmptyTrackNameReturnsErrorImmediately(t *testing.T) {
	t.Parallel()

	// LyricsProvider's own guard, no track name = no useful query
	lp := newTestLyricsProviderForHTTP()

	_, err := lp.FetchLyrics(context.Background(), "track-x", "", "Artist", "", 60_000)
	if err == nil {
		t.Error("empty trackName should error, got nil")
	}
}

func TestFetchLyrics_CacheHitSkipsUpstream(t *testing.T) {
	t.Parallel()

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if strings.Contains(r.URL.Path, "token.get") {
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"t"}}}`))
			return
		}
		_, _ = w.Write(happyPrimarySubtitleResponse())
	}))
	t.Cleanup(srv.Close)

	lp := newTestLyricsProviderForHTTP()
	lp.lpTokenURL = srv.URL + "/token.get"
	lp.lpSubtitleURL = srv.URL + "/macro.subtitles.get"
	lp.lrclibURL = "http://localhost:1/nope"

	if _, err := lp.FetchLyrics(context.Background(), "k", "Hi", "X", "", 60_000); err != nil {
		t.Fatalf("first FetchLyrics: %v", err)
	}
	requestsAfterFirst := atomic.LoadInt32(&requests)
	if requestsAfterFirst == 0 {
		t.Fatal("first call should have hit the network")
	}

	// Second call same trackId, should be a pure cache hit.
	if _, err := lp.FetchLyrics(context.Background(), "k", "Hi", "X", "", 60_000); err != nil {
		t.Fatalf("second FetchLyrics: %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != requestsAfterFirst {
		t.Errorf("second call hit network: requests went from %d to %d", requestsAfterFirst, got)
	}
}

// cache eviction

func TestEvictOldestLocked_BoundsCacheSize(t *testing.T) {
	t.Parallel()

	// eviction shrinks the cache enough to be bounded again
	cases := []struct {
		name           string
		startSize      int
		expectedRemain int
	}{
		// Traced: iter1 deletes (0 < 4/2=2). iter2 break (1 >= 3/2=1).
		// One deletion total 3 remain.
		{"size_4_deletes_1", 4, 3},
		// iter1 deletes (0 < 2). iter2 deletes (1 < 4/2=2). iter3 break (2 >= 3/2=1).
		{"size_5_deletes_2", 5, 3},
		// iter1-3 delete. iter4 break (3 >= 7/2=3).
		{"size_10_deletes_3", 10, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lp := newTestLyricsProviderForHTTP()
			for i := 0; i < tc.startSize; i++ {
				lp.cache[string(rune('a'+i))] = &LyricsResult{SyncType: "LINE_SYNCED"}
			}

			lp.mu.Lock()
			lp.evictOldestLocked()
			lp.mu.Unlock()

			if got := len(lp.cache); got != tc.expectedRemain {
				t.Errorf("start=%d: cache size after eviction got %d want %d",
					tc.startSize, got, tc.expectedRemain)
			}
		})
	}
}

func TestEvictOldestLocked_EmptyCacheIsSafe(t *testing.T) {
	t.Parallel()

	// calling eviction on an empty cache must not panic
	lp := newTestLyricsProviderForHTTP()

	lp.mu.Lock()
	lp.evictOldestLocked()
	lp.mu.Unlock()

	if len(lp.cache) != 0 {
		t.Errorf("expected empty cache, got %d entries", len(lp.cache))
	}
}

func TestClearCache_RemovesAllEntries(t *testing.T) {
	t.Parallel()

	lp := newTestLyricsProviderForHTTP()
	for _, k := range []string{"a", "b", "c"} {
		lp.cache[k] = &LyricsResult{SyncType: "UNSYNCED"}
	}

	lp.ClearCache()

	if len(lp.cache) != 0 {
		t.Errorf("ClearCache: expected empty, got %d entries", len(lp.cache))
	}
}

func TestFetchLyrics_ConcurrentCallsForSameTrackDoNotDeadlock(t *testing.T) {
	t.Parallel()

	// race-detector sanity, 10 concurrent FetchLyrics for the same id
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "token.get") {
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"t"}}}`))
			return
		}
		_, _ = w.Write(happyPrimarySubtitleResponse())
	}))
	t.Cleanup(srv.Close)

	lp := newTestLyricsProviderForHTTP()
	lp.lpTokenURL = srv.URL + "/token.get"
	lp.lpSubtitleURL = srv.URL + "/macro.subtitles.get"
	lp.lrclibURL = "http://localhost:1/nope"

	const N = 10
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = lp.FetchLyrics(context.Background(), "concurrent-key", "X", "Y", "", 60_000)
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchLyrics goroutines did not all finish within 5s - possible deadlock")
	}

	// the cache populated despite the race.
	lp.mu.RLock()
	defer lp.mu.RUnlock()
	if _, ok := lp.cache["concurrent-key"]; !ok {
		t.Error("cache entry not populated after concurrent calls")
	}
}

var _ = json.Marshal
