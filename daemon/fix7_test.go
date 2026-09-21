package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// issue #56 — the primary play of Liked Songs sends the user-specific
// collection uri `spotify:user:<id>:collection:tracks` (the form proven to
// start playback on the device) whenever the account id is resolvable through
// the established chain (cached /v1/me result, bounded /v1/me lookup, JWT sub
// claim, stored credentials username); when nothing resolves, the bare
// `spotify:collection:tracks` pseudo id goes out as a documented legacy
// best-effort fallback. A one-shot escape hatch repairs a liked play that
// starts nothing: a POSITIVE transition (a track is active and/or playback is
// running) disarms the retry, a NEGATIVE clear (the receiver stopped/cleared)
// fires it immediately, and no change for likedPlayRetryDelay lets the timer
// fire — either way the SAME last-sent context goes out once again (a bare
// fallback is re-resolved, so an account id that lands in between upgrades
// the retry to the user-specific uri). Queue expansion of any liked session
// pages library tracks even when the Connect state echoes a playlist-shaped
// id. Spotify's global "Today's Top Hits" playlist is NOT a liked-songs
// context and is never sent here.
// Reuses the existing seams (fakeMeFn + nopStateStore from api_server_test.go)
// plus the sendDeviceCommandFn transport seam.

// sentCommands records the connect commands a stubbed sendDeviceCommand saw
// (single-goroutine: tests drive handleApiRequest/handleLikedPlayRetry
// directly, no Run loop involved).
type sentCommands struct {
	mu   sync.Mutex
	cmds []connectCommand
}

func (r *sentCommands) send(_ context.Context, _ string, _ string, cmd connectCommand) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
	return nil
}

func (r *sentCommands) take() []connectCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]connectCommand, len(r.cmds))
	copy(out, r.cmds)
	return out
}

// newFix7Player wires a minimal AppPlayer for the play path + escape hatch:
// stubbed transport (sendDeviceCommandFn), fake /v1/me (webMeAccountFn), and
// an active target device in the observer state so resolveTargetDevice has
// somewhere to send. The fixture state carries no cached account id, no
// credentials username, and an opaque (non-JWT) access token — every id
// source below the bounded /v1/me lookup is empty by design.
func newFix7Player(t *testing.T, meID string) (*AppPlayer, *fakeMeFn, *sentCommands) {
	t.Helper()
	me := &fakeMeFn{id: meID}
	rec := &sentCommands{}
	state := &librespot.AppState{OAuth: librespot.OAuthState{AccessToken: "opaque-token-no-dots"}}
	p := &AppPlayer{
		app:                 &App{log: &librespot.NullLogger{}, state: state, stateStore: nopStateStore{}},
		state:               &State{remoteState: &RemoteState{DeviceId: "target-dev", DeviceName: "Target Dev"}},
		webMeAccountFn:      me.fn,
		sendDeviceCommandFn: rec.send,
	}
	return p, me, rec
}

// playLikedSongs issues the UI's actual request shape: the bare pseudo id with
// the tapped offset — exactly what the UI sends for a Liked-Songs tap.
func playLikedSongs(t *testing.T, p *AppPlayer) {
	t.Helper()
	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: likedCollectionUri})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (liked songs): %v", err)
	}
}

// (a) wire contract: a liked-songs play leaves the envelope with the
// user-specific collection uri as the primary context, resolved through the
// established account-id chain; the offset rides along unchanged. The pure
// builder still passes every other context through byte-for-byte.
func TestPlay_LikedSongsSendsUserFormContextAndPassesOffsetThrough(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{
		Uri:    likedCollectionUri,
		Offset: &ApiRequestPlayOffset{Uri: "spotify:track:first", Position: 42},
	})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (liked songs): %v", err)
	}

	cmds := rec.take()
	if len(cmds) != 1 {
		t.Fatalf("sent %d commands, want exactly 1 (the primary play)", len(cmds))
	}
	const wantURI = "spotify:user:me-user:collection:tracks"
	if got := cmds[0].Context.Uri; got != wantURI {
		t.Errorf("primary context: got %q want the user-specific collection uri %q", got, wantURI)
	}
	if got, want := cmds[0].Context.Url, "context://"+wantURI; got != want {
		t.Errorf("context url: got %q want %q", got, want)
	}
	if cmds[0].Options.Offset == nil || cmds[0].Options.Offset.Uri != "spotify:track:first" || cmds[0].Options.Offset.Position != 42 {
		t.Errorf("offset: got %+v, want unchanged {uri:spotify:track:first pos:42}", cmds[0].Options.Offset)
	}
	if got := cmds[0].Options.SkipTo.TrackUri; got != "spotify:track:first" {
		t.Errorf("skip_to: got %q, want the offset track uri (bug29 mirroring)", got)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times on the primary play path, want exactly 1 (the established resolution chain)", n)
	}
	if got := p.app.state.AccountID; got != "me-user" {
		t.Errorf("resolved id must be cached for the next play: got %q want me-user", got)
	}

	// the pure builder still passes every other context through untouched
	for _, uri := range []string{
		likedCollectionUri,
		"spotify:user:u1:collection:tracks",
		"spotify:playlist:abc123",
		"spotify:album:abc123",
	} {
		if got := buildPlayCommand(ApiRequestDataPlay{Uri: uri}).Context.Uri; got != uri {
			t.Errorf("builder passthrough %q: got %q", uri, got)
		}
	}
}

// (b) nothing resolvable — no cached id, /v1/me failing, opaque token, empty
// credentials username: the play is NOT refused, it goes out with the bare
// pseudo id as a documented legacy best-effort fallback, and the session is
// still marked as a liked-songs context (queue expansion pages library
// tracks). A subsequent non-liked play clears that state.
func TestPlay_LikedSongsUnresolvedFallsBackToBarePseudoContext(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "") // chronic-429 shape: every source below /v1/me is empty

	playLikedSongs(t, p) // must not be refused

	cmds := rec.take()
	if len(cmds) != 1 {
		t.Fatalf("sent %d commands, want exactly 1 (the primary play)", len(cmds))
	}
	if got := cmds[0].Context.Uri; got != likedCollectionUri {
		t.Errorf("unresolved fallback: got %q want the bare pseudo id", got)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times, want exactly 1 (the chain was tried before falling back)", n)
	}
	if !p.likedSessionActive.Load() {
		t.Error("a liked-songs play must mark the session so queue expansion pages library tracks")
	}
	// the armed retry keeps the exact payload that went out on the wire —
	// the bare pseudo id, re-resolved only at firing time
	if p.playRetryPending == nil || p.playRetryPending.data.Uri != likedCollectionUri {
		t.Errorf("armed retry source: got %+v, want the bare pseudo id that actually went out", p.playRetryPending)
	}

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: "spotify:playlist:pl"})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (playlist): %v", err)
	}
	if p.playRetryPending != nil || p.likedSessionActive.Load() {
		t.Error("a playlist play must clear all liked-session state and arm nothing")
	}
	if len(rec.take()) != 2 {
		t.Errorf("sent %d commands, want 2 (one per play)", len(rec.take()))
	}
}

// (c) the receiver ignored the command — no state change through the full
// window — so the timer fires and re-sends the SAME last-sent context once
// (it was never bare, so nothing is re-resolved and no /v1/me traffic lands).
func TestPlay_LikedSongsStallRetriesSameContext(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	playLikedSongs(t, p)
	primary := rec.take()[0].Context.Uri

	// 10 s passed with the active state untouched — the armed timer fires
	p.handleLikedPlayRetry(context.Background())

	cmds := rec.take()
	if len(cmds) != 2 {
		t.Fatalf("sent %d commands, want 2 (primary + retry)", len(cmds))
	}
	if got, want := cmds[1].Context.Uri, primary; got != want {
		t.Errorf("retry context: got %q want the same last-sent context %q", got, want)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times, want exactly 1 (no re-resolution of a non-bare context)", n)
	}
	if !p.likedSessionActive.Load() {
		t.Error("the session must stay marked as liked-songs for queue expansion")
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("the retry is one-shot: the armed state must be consumed")
	}

	// a second firing is a no-op even though the active state still has not moved
	p.handleLikedPlayRetry(context.Background())
	if len(rec.take()) != 2 {
		t.Errorf("second firing must not re-send, got %d commands", len(rec.take()))
	}
}

// (c) a NEGATIVE transition — the receiver stopped or cleared the context
// instead of starting playback — fires the retry immediately on the next
// state update, without waiting for the timer (the probe-147 symptom: the
// remote player takes the command and reports stopped). The retry carries the
// same last-sent context.
func TestPlay_LikedSongsNegativeClearFiresRetryImmediately(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	// a session is already playing — this is the pre-snapshot arm time sees
	p.state.remoteState = &RemoteState{
		DeviceId:   "target-dev",
		DeviceName: "Target Dev",
		TrackUri:   "spotify:track:prev",
		ContextUri: "spotify:playlist:pl",
		IsPlaying:  true,
	}
	playLikedSongs(t, p)
	primary := rec.take()[0].Context.Uri

	// the receiver could not honor the context and stopped playback — a
	// negative clear transition observed on the next state update
	p.state.remoteState = &RemoteState{DeviceId: "target-dev", DeviceName: "Target Dev"}
	p.observeLikedRetryTransition(context.Background())

	cmds := rec.take()
	if len(cmds) != 2 {
		t.Fatalf("sent %d commands, want 2 (primary + immediate retry)", len(cmds))
	}
	if got, want := cmds[1].Context.Uri, primary; got != want {
		t.Errorf("retry context: got %q want the same last-sent context %q", got, want)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times, want exactly 1 (no re-resolution of a non-bare context)", n)
	}
	if !p.likedSessionActive.Load() {
		t.Error("the session must stay marked as liked-songs for queue expansion")
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("the retry is one-shot: the armed state must be consumed")
	}
}

// (c) an account id that lands between the primary send and the retry fires
// the retry with the upgraded user-specific uri — the bare fallback is
// re-resolved at firing time, everything else stays put.
func TestPlay_LikedSongsRetryUpgradesBareFallbackToUserForm(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "") // unresolvable at play time

	playLikedSongs(t, p)
	cmds := rec.take()
	if len(cmds) != 1 || cmds[0].Context.Uri != likedCollectionUri {
		t.Fatalf("precondition: want exactly the bare fallback sent first, got %+v", cmds)
	}

	// the account id lands before the retry (the background resolver persists
	// it into the cached state — no /v1/me traffic needed at firing time)
	p.app.state.AccountID = "late-user"
	p.handleLikedPlayRetry(context.Background())

	cmds = rec.take()
	if len(cmds) != 2 {
		t.Fatalf("sent %d commands, want 2 (bare primary + retry)", len(cmds))
	}
	if got, want := cmds[1].Context.Uri, "spotify:user:late-user:collection:tracks"; got != want {
		t.Errorf("retry context: got %q want the upgraded user-specific uri %q", got, want)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times, want exactly 1 (the cached id must win at firing time)", n)
	}
	if !p.likedSessionActive.Load() {
		t.Error("the session must stay marked as liked-songs for queue expansion")
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("the retry is one-shot: the armed state must be consumed")
	}
}

// (c) nothing resolvable at retry time either — the bare fallback goes out a
// second time (same last-sent context; the background resolver keeps
// retrying, and a later play picks up the real uri once it lands).
func TestPlay_LikedSongsRetryStillUnresolvedResendsBareFallback(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "") // every source stays empty

	playLikedSongs(t, p)
	p.handleLikedPlayRetry(context.Background())

	cmds := rec.take()
	if len(cmds) != 2 {
		t.Fatalf("sent %d commands, want 2 (bare primary + bare retry)", len(cmds))
	}
	if got := cmds[1].Context.Uri; got != likedCollectionUri {
		t.Errorf("retry context: got %q want the bare pseudo id re-sent", got)
	}
	if n := me.calls.Load(); n != 2 {
		t.Errorf("/v1/me looked up %d times, want 2 (once per resolution attempt)", n)
	}
	if !p.likedSessionActive.Load() {
		t.Error("the session must stay marked as liked-songs for queue expansion")
	}
}

// (c) defense in depth: the active state moved to a positive shape without an
// observer pass in between, and only then does the timer fire — the retry
// classifies the current state first and suppresses itself.
func TestPlay_LikedSongsRetrySuppressedWhenStateMovedBeforeFiring(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	playLikedSongs(t, p)
	// playback started, but the observer hook did not run before the timer
	// value arrived in the select
	p.state.remoteState = &RemoteState{
		DeviceId:   "target-dev",
		DeviceName: "Target Dev",
		TrackUri:   "spotify:track:first-liked",
		ContextUri: "spotify:playlist:7EL2aPD9U3qbjggALcJ7IV", // the internal echo a receiver reports
		IsPlaying:  true,
	}
	p.handleLikedPlayRetry(context.Background())

	if len(rec.take()) != 1 {
		t.Errorf("state moved: got %d commands, want exactly 1 (no retry)", len(rec.take()))
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times after a taken command, want exactly 1 (the primary resolve only)", n)
	}
	if !p.likedSessionActive.Load() {
		t.Error("the session must stay marked as liked-songs even when the retry is suppressed")
	}
}

// (c) a new play supersedes an armed retry: even if the stale timer fires
// afterwards, there is nothing to re-send.
func TestPlay_NewPlaySupersedesArmedRetry(t *testing.T) {
	t.Parallel()
	p, _, rec := newFix7Player(t, "me-user")

	playLikedSongs(t, p)
	if p.playRetryPending == nil {
		t.Fatal("precondition: the armed retry must be set")
	}
	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: "spotify:playlist:pl"})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (playlist): %v", err)
	}

	if p.playRetryPending != nil || p.playRetryTimerCh != nil || p.likedSessionActive.Load() {
		t.Error("a superseding play must clear all escape-hatch state")
	}
	p.handleLikedPlayRetry(context.Background()) // stale firing: must be a no-op
	if len(rec.take()) != 2 {
		t.Errorf("sent %d commands, want exactly 2 (no stale retry)", len(rec.take()))
	}
}

// (d) a POSITIVE transition — a track is active and/or playback is running —
// disarms the armed retry; a stale timer firing afterwards is a no-op, so the
// retry never double-issues. The session stays marked as liked-songs: its
// Connect echo may be playlist-shaped, so queue expansion must keep paging
// library tracks for it.
func TestPlay_LikedSongsPositiveTransitionDisarmsRetry(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	playLikedSongs(t, p)

	// the receiver started playback of the context (the internal playlist
	// echo a receiver reports for it)
	p.state.remoteState = &RemoteState{
		DeviceId:   "target-dev",
		DeviceName: "Target Dev",
		TrackUri:   "spotify:track:first-liked",
		ContextUri: "spotify:playlist:7EL2aPD9U3qbjggALcJ7IV",
		IsPlaying:  true,
	}
	p.observeLikedRetryTransition(context.Background())

	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("a positive transition must disarm the armed retry and its timer")
	}
	p.handleLikedPlayRetry(context.Background()) // stale timer firing: no-op
	if len(rec.take()) != 1 {
		t.Errorf("positive transition: got %d commands, want exactly 1 (no retry)", len(rec.take()))
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times after a taken command, want exactly 1 (the primary resolve only)", n)
	}
	if !p.likedSessionActive.Load() {
		t.Error("a taken liked-songs play must keep the session marked for library-track queue expansion")
	}
}

// (g) routing decision: ordinary playlists page as playlists; every liked
// form pages library tracks; the flag forces the library route for a
// playlist-shaped id.
func TestQueueExpandFetchOp_Routing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		uri       string
		asLibrary bool
		want      string
	}{
		{"spotify:playlist:abc", false, "fetchPlaylist"},
		{"spotify:playlist:abc", true, "fetchLibraryTracks"},
		{likedCollectionUri, false, "fetchLibraryTracks"},
		{likedCollectionUri, true, "fetchLibraryTracks"},
		{"spotify:user:u1:collection:tracks", false, "fetchLibraryTracks"},
	} {
		if got := queueExpandFetchOp(tc.uri, tc.asLibrary); got != tc.want {
			t.Errorf("op(%q, %v): got %q want %q", tc.uri, tc.asLibrary, got, tc.want)
		}
	}
}

// likedTestPage is a real two-track page: fetchQueueExpand only writes the
// cache and sends on queueExpandedCh when the paged list is non-empty, so a
// real page gives the tests a channel receive to synchronize on before they
// may read the stub's bookkeeping (the established pattern in this package).
func likedTestPage() ([]any, int) {
	items := []any{
		map[string]any{"is_local": false, "track": map[string]any{"id": "t00", "name": "A", "uri": "spotify:track:t00"}},
		map[string]any{"is_local": false, "track": map[string]any{"id": "t01", "name": "B", "uri": "spotify:track:t01"}},
	}
	return items, 2
}

// (h) a liked-songs session — primary user-form or bare fallback — may be
// reported back by the Connect state with a playlist-shaped id (build #145
// observed exactly this echo). While the session is marked as liked,
// expansion pages library tracks instead of paging whatever internal
// playlist the receiver chose to echo.
func TestQueueExpand_LikedSessionForcesLibraryRoute(t *testing.T) {
	t.Parallel()

	p := newTestQueueExpandPlayer(t)
	var asLibraries []bool
	p.queueExpandPageFn = func(_ context.Context, _ string, _, _ int, asLibrary bool) ([]any, int, error) {
		asLibraries = append(asLibraries, asLibrary) // ordered before the channel send
		items, total := likedTestPage()
		return items, total, nil
	}
	p.likedSessionActive.Store(true)

	rs := &RemoteState{
		TrackUri:   "spotify:track:t00",
		ContextUri: "spotify:playlist:7EL2aPD9U3qbjggALcJ7IV", // the internal echo observed on-device
		NextTracks: []QueueTrack{{Uri: "spotify:track:t01", TrackId: "t01"}},
	}
	p.expandQueue(rs)

	select {
	case res := <-p.queueExpandedCh:
		if len(asLibraries) != 1 || !asLibraries[0] {
			t.Errorf("page called with asLibrary=%v, want [true] for the playlist-shaped liked echo", asLibraries)
		}
		if res.contextUri != "spotify:playlist:7EL2aPD9U3qbjggALcJ7IV" || len(res.list) != 2 || res.total != 2 {
			t.Errorf("result: got %+v, want the 2-track library page for the playlist-shaped liked echo", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no queue expansion result for the playlist-shaped liked echo")
	}
	if len(rs.NextTracks) != 1 {
		t.Errorf("the preview must be kept until the run loop re-applies the fetch, got %d entries", len(rs.NextTracks))
	}
}

// (i) without the flag nothing changes: ordinary playlists page as
// playlists; the bare liked form already pages library tracks.
func TestQueueExpand_OrdinaryContextsUnaffectedByFlag(t *testing.T) {
	t.Parallel()

	p := newTestQueueExpandPlayer(t)
	var libs []bool
	p.queueExpandPageFn = func(_ context.Context, _ string, _, _ int, asLibrary bool) ([]any, int, error) {
		libs = append(libs, asLibrary) // ordered before the channel send
		items, total := likedTestPage()
		return items, total, nil
	}
	rs := &RemoteState{
		TrackUri:   "spotify:track:t00",
		ContextUri: "spotify:playlist:pl",
		NextTracks: []QueueTrack{{Uri: "spotify:track:t01", TrackId: "t01"}},
	}
	p.expandQueue(rs)

	select {
	case res := <-p.queueExpandedCh:
		if len(libs) != 1 || libs[0] {
			t.Errorf("unflagged playlist: asLibrary=%v, want [false]", libs)
		}
		if res.contextUri != "spotify:playlist:pl" || len(res.list) != 2 {
			t.Errorf("result: got %+v, want the playlist page for the unflagged context", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no queue expansion result for the unflagged playlist")
	}

	// bare liked form: the flag is irrelevant — the uri already routes to
	// library tracks (isLikedCollectionUri branch of the fetch op)
	p2 := newTestQueueExpandPlayer(t)
	var libs2 []bool
	p2.queueExpandPageFn = func(_ context.Context, _ string, _, _ int, asLibrary bool) ([]any, int, error) {
		libs2 = append(libs2, asLibrary) // ordered before the channel send
		items, total := likedTestPage()
		return items, total, nil
	}
	rs2 := &RemoteState{
		TrackUri:   "spotify:track:t00",
		ContextUri: likedCollectionUri,
		NextTracks: []QueueTrack{{Uri: "spotify:track:t01", TrackId: "t01"}},
	}
	p2.expandQueue(rs2)

	select {
	case res := <-p2.queueExpandedCh:
		if len(libs2) != 1 || libs2[0] {
			t.Errorf("bare liked form: asLibrary=%v, want [false] (the uri already routes to library tracks)", libs2)
		}
		if res.contextUri != likedCollectionUri || len(res.list) != 2 {
			t.Errorf("result: got %+v, want the library page for the bare liked form", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no queue expansion result for the bare liked form")
	}
}

// (j) a fresh cache entry fetched via the other route is not served across
// the route boundary — re-fetch instead.
func TestQueueExpand_AsLibraryMismatchRefetches(t *testing.T) {
	t.Parallel()

	p := newTestQueueExpandPlayer(t)
	list := contextTracks(3)
	p.queueExpandCache["spotify:playlist:pl"] = queueExpandCacheEntry{
		list:      list,
		total:     3,
		fetchedAt: time.Now(),
		asLibrary: false, // fetched earlier for the ordinary playlist route
	}
	var pages int
	p.queueExpandPageFn = func(_ context.Context, _ string, _, _ int, asLibrary bool) ([]any, int, error) {
		pages++ // ordered before the channel send
		if !asLibrary {
			t.Error("the mismatched re-fetch used the playlist route")
		}
		items, total := likedTestPage()
		return items, total, nil
	}
	p.likedSessionActive.Store(true)

	rs := &RemoteState{
		TrackUri:   list[0].Uri,
		ContextUri: "spotify:playlist:pl",
		NextTracks: []QueueTrack{{Uri: list[1].Uri, TrackId: list[1].TrackId}},
	}
	p.expandQueue(rs)

	select {
	case res := <-p.queueExpandedCh:
		if pages != 1 {
			t.Errorf("page calls: got %d want 1 (a route-mismatched cache entry must not be served)", pages)
		}
		if res.contextUri != "spotify:playlist:pl" || len(res.list) != 2 {
			t.Errorf("result: got %+v, want the re-fetched library page", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no queue expansion result for the mismatched cache entry")
	}
	if len(rs.NextTracks) != 1 {
		t.Errorf("the preview must be kept while the re-fetch is in flight, got %d entries", len(rs.NextTracks))
	}

	p.queueExpandMu.Lock()
	entry, ok := p.queueExpandCache["spotify:playlist:pl"]
	p.queueExpandMu.Unlock()
	if !ok || !entry.asLibrary {
		t.Errorf("cache entry after re-fetch: asLibrary=%v (present=%v), want the route flipped to library tracks", entry.asLibrary, ok)
	}
}
