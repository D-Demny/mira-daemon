package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// issue #56 fix #5 — duration-budgeted background account-id resolution
// (24h budget, 30s→5min doubling retry ramp) feeding the liked-songs play
// path's user-specific collection uri (the play-path itself is covered by
// fix7_test.go). Reuses the existing webMeAccountFn seam (fakeMeFn,
// recordingStateStore from api_server_test.go; newBGResolverPlayer from
// fix4_test.go). All injected durations are tiny — no real sleeps of the
// production shape.

// flakyMeFn is a webMeAccountFn seam fake that fails its first `fails`
// lookups and returns id afterwards (the chronic-429 shape: many failures,
// one eventual success). It records the start time of every call so the retry
// ramp can be measured from inter-call spacing. Writes are single-threaded
// (the resolver loop); readers only observe them after the loop has joined.
type flakyMeFn struct {
	id    string
	fails int
	calls atomic.Int32
	at    []time.Time
}

func (f *flakyMeFn) fn(ctx context.Context) string {
	n := f.calls.Add(1)
	f.at = append(f.at, time.Now())
	if n <= int32(f.fails) {
		return ""
	}
	return f.id
}

// pins the fix-#5 production pacing defaults: the 24h budget replaces the old
// 20-attempt hard cap, the attempt deadline stays 20s, the retry ramp starts
// at 30s and is capped at 5min.
func TestResolveAccountIDBackground_DefaultsPinned(t *testing.T) {
	t.Parallel()

	if got := accountIDAttemptTimeout; got != 20*time.Second {
		t.Errorf("accountIDAttemptTimeout: got %s want 20s (unchanged)", got)
	}
	if got := defaultAccountIDBudget; got != 24*time.Hour {
		t.Errorf("defaultAccountIDBudget: got %s want 24h", got)
	}
	if got := defaultAccountIDInitialDelay; got != 30*time.Second {
		t.Errorf("defaultAccountIDInitialDelay: got %s want 30s", got)
	}
	if got := accountIDMaxRetryDelay; got != 5*time.Minute {
		t.Errorf("accountIDMaxRetryDelay: got %s want 5m", got)
	}
}

// (a) chronic-429 shape: the lookup fails repeatedly, then succeeds — the id
// must persist exactly once and the loop must stop (a later trigger
// short-circuits on the cache without new traffic).
func TestResolveAccountIDBackground_EventualSuccessAfterFailuresPersistsAndStops(t *testing.T) {
	t.Parallel()
	p, state, store, _ := newBGResolverPlayer(t)
	me := &flakyMeFn{id: "late-user", fails: 4} // 4 failures, then success
	p.webMeAccountFn = me.fn
	p.accountIDInitialDelay = time.Millisecond
	p.accountIDMaxDelay = 2 * time.Millisecond

	done := make(chan struct{})
	go func() { p.resolveAccountIDInBackground(make(chan struct{})); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resolver did not return after the eventual success")
	}

	if got := p.cachedAccountID(); got != "late-user" {
		t.Errorf("cached id: got %q want late-user", got)
	}
	if state.AccountID != "late-user" {
		t.Errorf("state.AccountID: got %q want late-user", state.AccountID)
	}
	if n := me.calls.Load(); n != 5 {
		t.Errorf("/v1/me lookups: got %d want exactly 5 (4 failures + 1 success)", n)
	}
	if n := store.saves.Load(); n != 1 {
		t.Errorf("persist saves: got %d want exactly 1", n)
	}
	if p.accountIDResolverRunning.Load() {
		t.Error("single-flight guard still set after the loop ended")
	}

	// stopped: a second run (e.g. an OAuth re-pair trigger) short-circuits on
	// the cached id without another /v1/me lookup
	p.resolveAccountIDInBackground(make(chan struct{}))
	if n := me.calls.Load(); n != 5 {
		t.Errorf("/v1/me lookups after re-run: got %d want still 5", n)
	}
}

// (b) the retry delay doubles per consecutive failure and plateaus at the cap:
// with a 20ms start / 80ms cap and 6 capped attempts the nominal sleep
// sequence is [20, 40, 80, 80, 80]ms = 300ms. Without the cap it would be
// [20, 40, 80, 160, 320]ms = 620ms — measuring the inter-call spacing of the
// failing fetches discriminates the two.
func TestResolveAccountIDBackground_RetryDelayDoublesToCap(t *testing.T) {
	t.Parallel()
	const (
		initial = 20 * time.Millisecond
		maxD    = 80 * time.Millisecond
		capped  = initial + 2*initial + 3*maxD                             // 300ms nominal with the cap
		uncap   = initial + 2*initial + 4*initial + 8*initial + 16*initial // 620ms without
	)
	p, _, store, _ := newBGResolverPlayer(t) // base wiring (state + persist seam)
	f := &flakyMeFn{id: ""}                  // every attempt fails; records call times
	p.webMeAccountFn = f.fn
	p.accountIDInitialDelay = initial
	p.accountIDMaxDelay = maxD
	p.accountIDMaxAttempts = 6 // legacy cap bounds the run; budget stays default (24h)

	done := make(chan struct{})
	go func() { p.resolveAccountIDInBackground(make(chan struct{})); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resolver did not give up at the attempt cap")
	}

	if n := f.calls.Load(); n != 6 {
		t.Fatalf("/v1/me lookups: got %d want exactly 6 (the legacy attempt cap)", n)
	}
	if len(f.at) != 6 {
		t.Fatalf("recorded call times: got %d want 6", len(f.at))
	}
	deltas := make([]time.Duration, len(f.at)-1)
	for i := 1; i < len(f.at); i++ {
		deltas[i-1] = f.at[i].Sub(f.at[i-1])
	}

	if d := deltas[0]; d < initial {
		t.Errorf("first retry delay: got %s want >= %s (the injected start)", d, initial)
	}
	if d := deltas[1]; d < 2*initial {
		t.Errorf("second retry delay: got %s want >= %s (doubled after the first failure)", d, 2*initial)
	}
	total := f.at[len(f.at)-1].Sub(f.at[0])
	if total >= uncap {
		t.Errorf("total wait: got %s — the doubling ramp ran past the cap (uncapped nominal would be %s)", total, uncap)
	}
	if total < capped {
		t.Errorf("total wait: got %s want >= %s (time.After never fires early)", total, capped)
	}
	if n := store.saves.Load(); n != 0 {
		t.Errorf("persist saves: got %d want 0 (nothing resolved)", n)
	}
}

// (c) the duration budget bounds the attempt count: with a tiny injected
// budget and no legacy attempt cap set, the loop must stop on its own when the
// budget is exhausted — several attempts happen, but not an unbounded run.
func TestResolveAccountIDBackground_BudgetBoundsAttempts(t *testing.T) {
	t.Parallel()
	p, _, store, me := newBGResolverPlayer(t) // default fake: every attempt fails
	p.accountIDBudget = 80 * time.Millisecond
	p.accountIDInitialDelay = 5 * time.Millisecond
	p.accountIDMaxDelay = 10 * time.Millisecond
	// accountIDMaxAttempts intentionally unset (0) — the budget is the only bound

	done := make(chan struct{})
	go func() { p.resolveAccountIDInBackground(make(chan struct{})); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resolver did not stop when the budget was exhausted")
	}

	if n := me.calls.Load(); n < 3 {
		t.Errorf("/v1/me lookups: got %d, want >= 3 (the budget must allow several attempts)", n)
	}
	if n := me.calls.Load(); n > 100 {
		t.Errorf("/v1/me lookups: got %d, want <= 100 (the loop must stop at the budget)", n)
	}
	if p.cachedAccountID() != "" {
		t.Error("account id must stay empty when every attempt fails")
	}
	if n := store.saves.Load(); n != 0 {
		t.Errorf("persist saves: got %d want 0 (nothing resolved)", n)
	}
	if p.accountIDResolverRunning.Load() {
		t.Error("single-flight guard still set after the budget gave up")
	}
}

// (d) the stop channel exits the loop even with the default 24h budget —
// shutdown must not wait out the budget.
func TestResolveAccountIDBackground_StopChannelExits(t *testing.T) {
	t.Parallel()
	p, _, _, me := newBGResolverPlayer(t) // default fake: every attempt fails
	p.accountIDInitialDelay = 5 * time.Millisecond
	p.accountIDMaxDelay = 10 * time.Millisecond
	// budget + attempt cap left at defaults (24h / unset) — only stop ends this run

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { p.resolveAccountIDInBackground(stop); close(done) }()

	time.Sleep(30 * time.Millisecond) // a few fail + delay cycles, then tear down
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("resolver ignored the stop channel")
	}

	if n := me.calls.Load(); n == 0 || n > 20 {
		t.Errorf("/v1/me lookups: got %d, want between 1 and 20 (stop must cut the loop short)", n)
	}
	if p.accountIDResolverRunning.Load() {
		t.Error("single-flight guard still set after stop — a re-pair trigger would be starved")
	}
}
