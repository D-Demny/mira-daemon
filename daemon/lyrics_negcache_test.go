package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A track that every source reports as having no lyrics should still be cached
func TestFetchLyrics_NoLyricsCachedSkipsRefetch(t *testing.T) {
	t.Parallel()

	var secReqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secReqs, 1)
		if strings.Contains(r.URL.Path, "token.get") {
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"t"}}}`))
			return
		}
		_, _ = w.Write(noSyncedLyricsResponse()) // 200, but no lyrics
	}))
	t.Cleanup(srv.Close)

	var lrcReqs int32
	lrc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&lrcReqs, 1)
		w.WriteHeader(404) // lrclib: definitively nothing
	}))
	t.Cleanup(lrc.Close)

	sec := newTestSecondaryProviderHTTP()
	sec.tokenURL = srv.URL + "/token.get"
	sec.subtitleURL = srv.URL + "/macro.subtitles.get"
	ter := newTestTertiaryProviderHTTP()
	ter.url = lrc.URL
	lp := newTestOrchestrator(sec, ter)

	// first fetch: hits upstream, finds nothing, caches the negative
	if _, err := lp.FetchLyrics(context.Background(), "track-instrumental", "X", "Y", "", 60_000); !errors.Is(err, ErrNoLyrics) {
		t.Fatalf("first fetch: got %v, want ErrNoLyrics", err)
	}
	sec1, lrc1 := atomic.LoadInt32(&secReqs), atomic.LoadInt32(&lrcReqs)
	if sec1 == 0 || lrc1 == 0 {
		t.Fatal("first fetch should have hit upstream")
	}

	// second fetch: served from the negative cache
	if _, err := lp.FetchLyrics(context.Background(), "track-instrumental", "X", "Y", "", 60_000); !errors.Is(err, ErrNoLyrics) {
		t.Fatalf("second fetch: got %v, want ErrNoLyrics", err)
	}
	if got := atomic.LoadInt32(&secReqs); got != sec1 {
		t.Errorf("secondary re-queried on negative cache hit: %d -> %d", sec1, got)
	}
	if got := atomic.LoadInt32(&lrcReqs); got != lrc1 {
		t.Errorf("lrclib re-queried on negative cache hit: %d -> %d", lrc1, got)
	}

	// the cache holds an explicit nil (negative) entry
	lp.mu.RLock()
	v, ok := lp.cache["track-instrumental"]
	lp.mu.RUnlock()
	if !ok || v != nil {
		t.Errorf("expected a nil negative cache entry, got ok=%v v=%v", ok, v)
	}
}

// if a source fails transiently (here, a connection error from
// lrclib), the result must NOT be cached
func TestFetchLyrics_TransientFailureNotCached(t *testing.T) {
	t.Parallel()

	var secReqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secReqs, 1)
		if strings.Contains(r.URL.Path, "token.get") {
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"t"}}}`))
			return
		}
		_, _ = w.Write(noSyncedLyricsResponse()) // secondary: definitive no lyrics
	}))
	t.Cleanup(srv.Close)

	sec := newTestSecondaryProviderHTTP()
	sec.tokenURL = srv.URL + "/token.get"
	sec.subtitleURL = srv.URL + "/macro.subtitles.get"

	// tertiary points at a dead port -> connection error (transient, not ErrNoLyrics)
	ter := newTestTertiaryProviderHTTP()
	ter.url = "http://localhost:1/nope"
	lp := newTestOrchestrator(sec, ter)

	if _, err := lp.FetchLyrics(context.Background(), "track-maybe", "X", "Y", "", 60_000); !errors.Is(err, ErrNoLyrics) {
		t.Fatalf("first fetch: got %v, want ErrNoLyrics", err)
	}
	sec1 := atomic.LoadInt32(&secReqs)

	// a transient failure must leave the cache empty
	lp.mu.RLock()
	_, cached := lp.cache["track-maybe"]
	lp.mu.RUnlock()
	if cached {
		t.Fatal("transient failure was cached as a negative; lyrics could appear on retry")
	}

	// so the second fetch re-queries upstream
	if _, err := lp.FetchLyrics(context.Background(), "track-maybe", "X", "Y", "", 60_000); !errors.Is(err, ErrNoLyrics) {
		t.Fatalf("second fetch: got %v, want ErrNoLyrics", err)
	}
	if got := atomic.LoadInt32(&secReqs); got <= sec1 {
		t.Errorf("expected upstream re-query after a transient failure; secondary reqs stayed at %d", got)
	}
}
