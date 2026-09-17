package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// issue #56 fix #4 — the background account-id resolver. Reuses the existing
// webMeAccountFn seam (fakeMeFn + recordingStateStore from api_server_test.go)
// plus the per-player pacing knobs so the whole suite stays well under 5s.

// newBGResolverPlayer wires a minimal AppPlayer for
// resolveAccountIDInBackground without a session: fake /v1/me fetch, real
// state + persist path (house fixture shape from api_server_test.go).
func newBGResolverPlayer(t *testing.T) (*AppPlayer, *librespot.AppState, *recordingStateStore, *fakeMeFn) {
	t.Helper()
	store := &recordingStateStore{}
	me := &fakeMeFn{id: ""} // default: the rate-limit shape (lookup fails)
	state := &librespot.AppState{OAuth: librespot.OAuthState{AccessToken: "opaque-token-no-dots"}}
	p := &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state, stateStore: store}, webMeAccountFn: me.fn}
	return p, state, store, me
}

// waitForResolver blocks until the single-flight guard is released again, i.e.
// the loop has exited (success, stop, or the attempt cap).
func waitForResolver(t *testing.T, p *AppPlayer) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for p.accountIDResolverRunning.Load() {
		select {
		case <-deadline:
			t.Fatal("background resolver did not exit within 5s (guard stuck)")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// (a) first success -> cached + persisted, exactly one /v1/me lookup; a later
// run short-circuits on the cache without new traffic.
func TestResolveAccountIDInBackground_PersistsAfterFirstSuccess(t *testing.T) {
	t.Parallel()
	p, state, store, me := newBGResolverPlayer(t)
	me.id = "bg-user"

	done := make(chan struct{})
	go func() { p.resolveAccountIDInBackground(make(chan struct{})); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resolver did not return after a successful fetch")
	}

	if got := p.cachedAccountID(); got != "bg-user" {
		t.Errorf("cached id: got %q want bg-user", got)
	}
	if state.AccountID != "bg-user" {
		t.Errorf("state.AccountID: got %q want bg-user", state.AccountID)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me lookups: got %d want exactly 1", n)
	}
	if n := store.saves.Load(); n != 1 {
		t.Errorf("persist saves: got %d want exactly 1", n)
	}

	// the guard is reset AND the cached id short-circuits a second run — it
	// must return immediately without another /v1/me lookup
	p.resolveAccountIDInBackground(make(chan struct{}))
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me lookups after re-run: got %d want still 1", n)
	}
	if p.accountIDResolverRunning.Load() {
		t.Error("single-flight guard still set after the second run")
	}
}

// (b) the stop channel wakes the retry delay between attempts, cutting the
// loop far short of the cap.
func TestResolveAccountIDInBackground_HonorsStopBetweenAttempts(t *testing.T) {
	t.Parallel()
	p, _, _, me := newBGResolverPlayer(t) // me.id == "" -> every attempt fails
	p.accountIDMaxAttempts = 20
	p.accountIDRetryDelay = 100 * time.Millisecond

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { p.resolveAccountIDInBackground(stop); close(done) }()

	// let it run through a couple of fail + retry-delay cycles, then stop
	time.Sleep(250 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolver ignored the stop channel")
	}

	if n := me.calls.Load(); n == 0 || n >= int32(p.accountIDMaxAttempts) {
		t.Errorf("/v1/me lookups: got %d, want between 1 and %d (stop must cut the retry loop short)",
			n, p.accountIDMaxAttempts)
	}
	if p.accountIDResolverRunning.Load() {
		t.Error("single-flight guard still set after stop — a re-pair trigger would be starved")
	}
}

// (c) gives up at the attempt cap without panicking; nothing is persisted.
func TestResolveAccountIDInBackground_GivesUpAfterAttemptCap(t *testing.T) {
	t.Parallel()
	p, _, store, me := newBGResolverPlayer(t) // me.id == "" -> never succeeds
	p.accountIDMaxAttempts = 3
	p.accountIDRetryDelay = time.Millisecond

	done := make(chan struct{})
	go func() { p.resolveAccountIDInBackground(make(chan struct{})); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolver did not give up at the attempt cap")
	}

	if n := me.calls.Load(); n != 3 {
		t.Errorf("/v1/me lookups: got %d want exactly 3 (the cap)", n)
	}
	if p.cachedAccountID() != "" {
		t.Error("account id must stay empty when every attempt fails")
	}
	if n := store.saves.Load(); n != 0 {
		t.Errorf("persist saves: got %d want 0 (nothing resolved)", n)
	}
	if p.accountIDResolverRunning.Load() {
		t.Error("single-flight guard still set after giving up — a re-pair trigger would be starved")
	}
}

// single flight: app start (Run) and an OAuth re-pair (onOAuthTokenChanged) may
// both fire the resolver, but only one loop runs — the concurrent second call
// is a no-op while the first is in flight.
func TestResolveAccountIDInBackground_SingleFlight(t *testing.T) {
	t.Parallel()
	p, _, _, _ := newBGResolverPlayer(t)

	release := make(chan struct{})
	var inFlight atomic.Int32
	p.webMeAccountFn = func(ctx context.Context) string {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		<-release // hold attempt 1 open so the second trigger races it
		return ""
	}
	p.accountIDMaxAttempts = 5
	p.accountIDRetryDelay = time.Millisecond

	stop := make(chan struct{})
	go p.resolveAccountIDInBackground(stop) // trigger A shape (app start)
	go p.resolveAccountIDInBackground(stop) // trigger B shape (re-pair) — must no-op
	time.Sleep(50 * time.Millisecond)       // attempt 1 is still blocked in the fake fetch

	if n := inFlight.Load(); n != 1 {
		t.Fatalf("concurrent triggers: %d fetch in flight, want exactly 1", n)
	}

	close(release)
	waitForResolver(t, p) // first loop burns its remaining attempts and exits
}
