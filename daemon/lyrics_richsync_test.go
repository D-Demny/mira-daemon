package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rate limit
func TestRichsync_MatcherEmptyArrayBodyTripsCooldown(t *testing.T) {
	var matcherHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "token"):
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"tok"}}}`))
		case strings.Contains(r.URL.Path, "matcher.track.get"):
			atomic.AddInt32(&matcherHits, 1)
			// the throttle response shape
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":[]}}`))
		default:
			http.Error(w, "unexpected path", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	s := newTestSecondaryProviderHTTP()
	s.tokenURL = srv.URL + "/token.get"
	s.subtitleURL = srv.URL + "/macro.subtitles.get"
	s.appID = "x"

	q := lyricsQuery{trackName: "Blinding Lights", artistName: "The Weeknd"}

	if _, err := s.fetchRichsync(context.Background(), q); err == nil {
		t.Fatal("expected an error from the empty-array throttle response, got nil")
	}

	// daemon backs off when cooldown for ratelimit
	s.mu.Lock()
	cooling := time.Now().Before(s.cooldownUntil)
	s.mu.Unlock()
	if !cooling {
		t.Fatal("matcher empty-array throttle did not set the cooldown")
	}

	hitsBefore := atomic.LoadInt32(&matcherHits)
	if _, err := s.fetchRichsync(context.Background(), q); err == nil {
		t.Fatal("expected a cooling-down error on the follow-up call")
	}
	if got := atomic.LoadInt32(&matcherHits) - hitsBefore; got != 0 {
		t.Errorf("follow-up hit the matcher %d times during cooldown; want 0 (gated)", got)
	}
}

func TestRichsync_MatcherNoMatchDoesNotTripCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "token"):
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"user_token":"tok"}}}`))
		case strings.Contains(r.URL.Path, "matcher.track.get"):
			_, _ = w.Write([]byte(`{"message":{"header":{"status_code":200},"body":{"track":{"commontrack_id":0}}}}`))
		default:
			http.Error(w, "unexpected path", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	s := newTestSecondaryProviderHTTP()
	s.tokenURL = srv.URL + "/token.get"
	s.subtitleURL = srv.URL + "/macro.subtitles.get"
	s.appID = "x"

	q := lyricsQuery{trackName: "Some Obscure B-side", artistName: "Nobody"}
	if _, err := s.fetchRichsync(context.Background(), q); err == nil {
		t.Fatal("expected a no-match error, got nil")
	}

	s.mu.Lock()
	cooling := time.Now().Before(s.cooldownUntil)
	s.mu.Unlock()
	if cooling {
		t.Error("a genuine no-match must not set the cooldown")
	}
}
