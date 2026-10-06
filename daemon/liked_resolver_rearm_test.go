package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// issue #84: an unresolved liked-songs tap RE-ARMS the background uri resolver.
// A run that exhausted its budget has already exited (the single-flight flag
// resets on every exit), so a second goroutine launch after that exit must be
// accepted by the CAS and run — otherwise discovery would die permanently in
// the dead state Spotify's current web-player build leaves behind (see the
// pfLookupChildEntities comment: the lookup op is absent from the shipped
// bundle).

func TestResolveLikedPlaylistURI_ReArmAfterBudgetExhaustion(t *testing.T) {
	t.Parallel()
	p, _ := newFix7Player(t)

	stop := make(chan struct{})
	defer close(stop)
	var attempts atomic.Int32
	p.fetchAndValidateLikedPlaylistURIFn = func(context.Context) string {
		attempts.Add(1)
		return "" // the current build's probe never resolves — the dead state
	}
	p.likedPlaylistBudget = 200 * time.Millisecond
	p.likedPlaylistInitialDelay = 5 * time.Millisecond

	waitFlag := func(cond func() bool, what string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// first run: starts, runs to its (tiny) budget and exits, flag released
	go p.resolveLikedPlaylistURIInBackground(stop)
	waitFlag(p.likedPlaylistURIRunning.Load, "the first resolver run to start")
	waitFlag(func() bool { return !p.likedPlaylistURIRunning.Load() }, "the first resolver run to exit after its budget")
	first := attempts.Load()
	if first == 0 {
		t.Fatal("resolver loop never made a single attempt within its budget")
	}

	// re-arm: the same launch expression the play path uses on an unresolved
	// tap — accepted iff the flag really is free after budget exhaustion. A
	// refused CAS exits the goroutine at once, so attempts would not advance.
	go p.resolveLikedPlaylistURIInBackground(stop)
	waitFlag(func() bool { return attempts.Load() > first }, "the re-armed resolver run to make an attempt")
	waitFlag(func() bool { return !p.likedPlaylistURIRunning.Load() && attempts.Load() > first }, "the second resolver run to exit after its budget")
}
