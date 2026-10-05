package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)
// issue #84: the full-queue backfill behind the standalone-playback
// fallback. The liked-songs tap plays the tapped track immediately and a
// background worker queues the REST of the collection onto the target's
// queue (the same fetchLibraryTracks page source the working UI submenu
// uses, one add_to_queue per track) — until the real per-user playlist uri
// resolves or the backfill is superseded/stopped. Every test here is
// deterministic: the transport is a recording stub, the library pages come
// from an installed seam, and pacing/retry/budget are pinned to tiny values.

// waitForCommands polls the recorded commands until at least want have been
// sent (the backfill runs in a goroutine behind handleApiRequest's return).
func waitForCommands(t *testing.T, rec *sentCommands, want int, timeout time.Duration) []connectCommand {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.cmds)
		rec.mu.Unlock()
		if n >= want {
			return rec.take()
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.mu.Lock()
	n := len(rec.cmds)
	rec.mu.Unlock()
	t.Fatalf("timed out waiting for %d commands, got %d", want, n)
	return nil
}

// waitForStableCount waits until the recorded command count stops growing
// (a quiescent window proves the backfill is done or dead) and returns it.
func waitForStableCount(t *testing.T, rec *sentCommands, quiescence, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	lastChange := time.Now()
	for {
		rec.mu.Lock()
		n := len(rec.cmds)
		rec.mu.Unlock()
		if n != last {
			last, lastChange = n, time.Now()
		} else if time.Since(lastChange) >= quiescence {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("command count never went stable: %d", last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newBackfillTestPlayer(t *testing.T) (*AppPlayer, *sentCommands) {
	t.Helper()
	p, rec := newFix7Player(t)
	p.likedQueueBackfillPace = time.Millisecond
	p.likedQueueBackfillRetry = time.Millisecond
	p.likedQueueBackfillBudget = 5 * time.Second
	return p, rec
}

// (1) the whole collection gets queued after a tapped-track fallback: the
// first command is the standalone play of the tapped track (never the
// pseudo id), then one add_to_queue per remaining liked track — same page
// source and order as me/tracks — on the EXPLICIT play target.
func TestPlay_LikedSongsUnresolvedBackfillsFullQueue(t *testing.T) {
	t.Parallel()
	p, rec := newBackfillTestPlayer(t)

	const total = 123
	p.libraryTracksPageFn = func(_ context.Context, offset, limit int) ([]catalogItem, int, error) {
		end := offset + limit
		if end > total {
			end = total
		}
		items := make([]catalogItem, 0, end-offset)
		for i := offset; i < end; i++ {
			items = append(items, catalogItem{Uri: fmt.Sprintf("spotify:track:q%d", i)})
		}
		return items, total, nil
	}

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{
		Uri:    likedCollectionUri,
		Offset: &ApiRequestPlayOffset{Uri: "spotify:track:tapped", Position: 0},
	})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (liked songs): %v", err)
	}

	wantTotal := total // 1 fallback play + total-1 queued tracks
	cmds := waitForCommands(t, rec, wantTotal, 10*time.Second)
	if len(cmds) != wantTotal {
		t.Fatalf("sent %d commands, want exactly %d", len(cmds), wantTotal)
	}

	if cmds[0].Endpoint != "play" || cmds[0].Context == nil || cmds[0].Context.Uri != "spotify:track:tapped" {
		t.Errorf("first command: got endpoint=%q context=%+v, want the standalone play of the tapped track", cmds[0].Endpoint, cmds[0].Context)
	}
	for i, cmd := range cmds {
		if i == 0 {
			continue
		}
		wantUri := fmt.Sprintf("spotify:track:q%d", i) // offset i-1+1 == i in the liked list
		if cmd.Endpoint != "add_to_queue" || cmd.Track == nil || cmd.Track.Uri != wantUri {
			t.Errorf("command %d: got endpoint=%q uri=%v, want add_to_queue of %s (in order)", i, cmd.Endpoint, cmd.Track, wantUri)
			continue
		}
		if cmd.Track.Provider != "queue" || cmd.Track.Metadata["is_queued"] != "true" {
			t.Errorf("command %d: queue track shape mismatch: provider=%q metadata=%v", i, cmd.Track.Provider, cmd.Track.Metadata)
		}
		if cmd.Context != nil && cmd.Context.Uri == likedCollectionUri {
			t.Errorf("command %d: the pseudo collection uri must never leave the daemon", i)
		}
	}
	if n := waitForStableCount(t, rec, 150*time.Millisecond, 2*time.Second); n != wantTotal {
		t.Errorf("sent %d commands in total, want exactly %d (no double queueing)", n, wantTotal)
	}
}

// (2) any other play supersedes the backfill: the half-built queue stops
// where it is and the new play goes out untouched.
func TestPlay_LikedSongsBackfillSupersededByNewPlay(t *testing.T) {
	t.Parallel()
	p, rec := newBackfillTestPlayer(t)

	gate := make(chan struct{})
	p.libraryTracksPageFn = func(ctx context.Context, offset, _ int) ([]catalogItem, int, error) {
		if offset == 1 {
			return []catalogItem{{Uri: "spotify:track:q1"}}, 500, nil
		}
		// the backfill's next page: block until superseded (ctx done) or
		// the gate closes after the test has already made its assertions
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-gate:
			return nil, 0, errors.New("unblocked after supersede")
		}
	}

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{
		Uri:    likedCollectionUri,
		Offset: &ApiRequestPlayOffset{Uri: "spotify:track:tapped", Position: 0},
	})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (liked songs): %v", err)
	}
	cmds := waitForCommands(t, rec, 2, 5*time.Second) // fallback play + the single queued track
	if cmds[1].Endpoint != "add_to_queue" || cmds[1].Track == nil || cmds[1].Track.Uri != "spotify:track:q1" {
		t.Fatalf("second command: got endpoint=%q uri=%v, want add_to_queue of spotify:track:q1", cmds[1].Endpoint, cmds[1].Track)
	}

	req2, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: "spotify:playlist:normal"})
	if _, err := p.handleApiRequest(context.Background(), req2); err != nil {
		t.Fatalf("play (normal playlist): %v", err)
	}
	close(gate) // release the blocked page; its ctx is already cancelled

	n := waitForStableCount(t, rec, 200*time.Millisecond, 2*time.Second)
	if n != 3 {
		t.Fatalf("after supersede: %d commands recorded, want exactly 3 (fallback play + one queued track + new play)", n)
	}
	cmds = rec.take()
	if cmds[2].Endpoint != "play" || cmds[2].Context == nil || cmds[2].Context.Uri != "spotify:playlist:normal" {
		t.Errorf("third command: got endpoint=%q context=%+v, want the plain playlist play", cmds[2].Endpoint, cmds[2].Context)
	}
	adds := 0
	for _, c := range cmds {
		if c.Endpoint == "add_to_queue" {
			adds++
		}
	}
	if adds != 1 {
		t.Errorf("%d add_to_queue commands after supersede, want exactly 1 (the queue stops where it is)", adds)
	}
}

// (3) a persistently failing page beyond the first: bounded retries, then a
// quiet stop — the tapped track keeps playing, the partial prefix stays
// queued, nothing panics or errors into the request path.
func TestPlay_LikedSongsBackfillStopsOnPageFailure(t *testing.T) {
	t.Parallel()
	p, rec := newBackfillTestPlayer(t)

	p.libraryTracksPageFn = func(_ context.Context, offset, _ int) ([]catalogItem, int, error) {
		if offset == 1 {
			return []catalogItem{{Uri: "spotify:track:q1"}}, 500, nil
		}
		return nil, 0, errors.New("rate limited")
	}

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{
		Uri:    likedCollectionUri,
		Offset: &ApiRequestPlayOffset{Uri: "spotify:track:tapped", Position: 0},
	})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (liked songs): %v", err) // the tap itself must succeed
	}

	n := waitForStableCount(t, rec, 300*time.Millisecond, 5*time.Second)
	if n != 2 {
		t.Fatalf("%d commands, want exactly 2 (fallback play + the one queued track from page 1)", n)
	}
	cmds := rec.take()
	if cmds[0].Context == nil || cmds[0].Context.Uri != "spotify:track:tapped" {
		t.Errorf("first command context: got %v, want the tapped track", cmds[0].Context)
	}
}

// (4) Part B of the #84 fix: an unresolved liked-songs tap RE-ARMS the
// background uri resolver. A run that exhausted its budget has already
// exited (the single-flight flag resets on every exit), so a second
// goroutine launch after that exit must be accepted by the CAS and run —
// otherwise discovery would die permanently in the dead state Spotify's
// current web-player build leaves behind (see the pfLookupChildEntities
// comment: the lookup op is absent from the shipped bundle).
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
