package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// issue #56 fix #7/#8 — the primary play of Liked Songs sends Spotify's
// canonical Liked-Songs playlist id (the bare `spotify:collection:tracks`
// pseudo id is NOT resolvable on a Connect receiver), plus a one-shot escape
// hatch driven by how the active state moves after the command: a POSITIVE
// transition (a track is active and/or playback is running) suppresses the
// retry, a NEGATIVE clear (the receiver stopped/cleared) fires it
// immediately, and no change for likedPlayRetryDelay lets the timer fire —
// either way the play is re-sent once with the resolved user-specific
// collection uri, and queue expansion of that session pages library tracks.
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
// somewhere to send.
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

func playLikedSongs(t *testing.T, p *AppPlayer) {
	t.Helper()
	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: likedCollectionUri})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (bare liked songs): %v", err)
	}
}

// (a) wire contract, issue #56 fix #8: a liked-songs play leaves the
// envelope with Spotify's canonical Liked-Songs playlist id as the primary
// context (the bare pseudo id is NOT resolvable on a Connect receiver); the
// offset rides along unchanged. The pure builder itself still passes every
// other context through byte-for-byte.
func TestPlay_LikedSongsSendsCanonicalContextAndPassesOffsetThrough(t *testing.T) {
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
	cmd := cmds[0]
	if got := cmd.Context.Uri; got != canonicalLikedPlaylistUri {
		t.Errorf("primary context: got %q want the canonical Liked-Songs playlist id", got)
	}
	if got, want := cmd.Context.Url, "context://"+canonicalLikedPlaylistUri; got != want {
		t.Errorf("context url: got %q want %q", got, want)
	}
	if cmd.Options.Offset == nil || cmd.Options.Offset.Uri != "spotify:track:first" || cmd.Options.Offset.Position != 42 {
		t.Errorf("offset: got %+v, want unchanged {uri:spotify:track:first pos:42}", cmd.Options.Offset)
	}
	if got := cmd.Options.SkipTo.TrackUri; got != "spotify:track:first" {
		t.Errorf("skip_to: got %q, want the offset track uri (bug29 mirroring)", got)
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times on the primary play path, want 0 (no up-front resolution)", n)
	}

	// the pure builder still passes every other context through untouched
	for _, uri := range []string{
		likedCollectionUri,
		"spotify:user:u1:collection:tracks",
		canonicalLikedPlaylistUri,
		"spotify:album:abc123",
	} {
		if got := buildPlayCommand(ApiRequestDataPlay{Uri: uri}).Context.Uri; got != uri {
			t.Errorf("builder passthrough %q: got %q", uri, got)
		}
	}
}

// (b) the play path issues exactly the canonical Liked-Songs playlist id —
// no /v1/me on the wire path, no refusal — and arms the one-shot retry; a
// non-liked play arms nothing and clears the session state.
func TestPlay_LikedSongsIssuesCanonicalContextAndArmsEscapeHatch(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	playLikedSongs(t, p)

	cmds := rec.take()
	if len(cmds) != 1 {
		t.Fatalf("sent %d commands, want exactly 1 (the primary play)", len(cmds))
	}
	if got := cmds[0].Context.Uri; got != canonicalLikedPlaylistUri {
		t.Errorf("primary play context: got %q want the canonical Liked-Songs playlist id", got)
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times on the primary play path, want 0 (no up-front resolution)", n)
	}
	if p.playRetryPending == nil || p.likedViaUserForm.Load() {
		t.Errorf("escape hatch: want armed pending + flag clear, got pending=%v flag=%v",
			p.playRetryPending != nil, p.likedViaUserForm.Load())
	}
	// the armed retry keeps the bare pseudo id as its source — the user-form
	// uri is derived only at firing time (resolvePlayContextUri), so an
	// unresolved account id still skips cleanly (see the skip test below)
	if p.playRetryPending.data.Uri != likedCollectionUri {
		t.Errorf("armed retry source: got %q want the bare pseudo id", p.playRetryPending.data.Uri)
	}

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: "spotify:playlist:pl"})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (playlist): %v", err)
	}
	if p.playRetryPending != nil || p.likedViaUserForm.Load() {
		t.Error("a playlist play must not arm the retry or set the flag")
	}
	if len(rec.take()) != 2 {
		t.Errorf("sent %d commands, want 2 (one per play)", len(rec.take()))
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

	if p.playRetryPending != nil || p.playRetryTimerCh != nil || p.likedViaUserForm.Load() {
		t.Error("a superseding play must clear all escape-hatch state")
	}
	p.handleLikedPlayRetry(context.Background()) // stale firing: must be a no-op
	if len(rec.take()) != 2 {
		t.Errorf("sent %d commands, want exactly 2 (no stale retry)", len(rec.take()))
	}
}

// (c) issue #56 fix #8: a NEGATIVE transition — the receiver stopped or
// cleared the context instead of starting playback — fires the user-form
// retry immediately on the next state update, without waiting for the timer
// (the probe-147 symptom: the remote player takes the command and reports
// stopped).
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

	// the receiver could not honor the context and stopped playback — a
	// negative clear transition observed on the next state update
	p.state.remoteState = &RemoteState{DeviceId: "target-dev", DeviceName: "Target Dev"}
	p.observeLikedRetryTransition(context.Background())

	cmds := rec.take()
	if len(cmds) != 2 {
		t.Fatalf("sent %d commands, want 2 (primary + immediate retry)", len(cmds))
	}
	if got, want := cmds[1].Context.Uri, "spotify:user:me-user:collection:tracks"; got != want {
		t.Errorf("retry context: got %q want %q", got, want)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times for the retry, want exactly 1", n)
	}
	if !p.likedViaUserForm.Load() {
		t.Error("the flag must be set so queue expansion of this session pages library tracks")
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("the retry is one-shot: the armed state must be consumed")
	}
}

// (b2) issue #56 fix #8: a POSITIVE transition — a track is active and/or
// playback is running — disarms the armed retry; a stale timer firing
// afterwards is a no-op, so the retry never double-issues.
func TestPlay_LikedSongsPositiveTransitionDisarmsRetry(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	playLikedSongs(t, p)

	// the receiver started playback of the (canonical) context
	p.state.remoteState = &RemoteState{
		DeviceId:   "target-dev",
		DeviceName: "Target Dev",
		TrackUri:   "spotify:track:first-liked",
		ContextUri: canonicalLikedPlaylistUri,
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
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times after a taken command, want 0", n)
	}
	if p.likedViaUserForm.Load() {
		t.Error("the flag must stay clear when the primary context was honored")
	}
}

// (d) the receiver ignored the command — no state change — so the retry
// re-sends with the resolved user-specific collection and sets the flag.
func TestPlay_LikedSongsEscapeHatchRetriesWithUserForm(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	playLikedSongs(t, p)
	p.handleLikedPlayRetry(context.Background())

	cmds := rec.take()
	if len(cmds) != 2 {
		t.Fatalf("sent %d commands, want 2 (primary + retry)", len(cmds))
	}
	if got, want := cmds[1].Context.Uri, "spotify:user:me-user:collection:tracks"; got != want {
		t.Errorf("retry context: got %q want %q", got, want)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times for the retry, want exactly 1", n)
	}
	if !p.likedViaUserForm.Load() {
		t.Error("the flag must be set so queue expansion of this session pages library tracks")
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("the retry is one-shot: the armed state must be consumed")
	}

	// a second firing (or any later firing) is a no-op even though the active
	// state still has not moved
	p.handleLikedPlayRetry(context.Background())
	if len(rec.take()) != 2 {
		t.Errorf("second firing must not re-send, got %d commands", len(rec.take()))
	}
}

// (e) the receiver took the command — the active state moved — so the retry
// cancels itself: no second command, no /v1/me traffic.
func TestPlay_LikedSongsEscapeHatchCancelsWhenStateMoved(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	playLikedSongs(t, p)
	p.state.remoteState = &RemoteState{
		DeviceId:   "target-dev",
		DeviceName: "Target Dev",
		TrackUri:   "spotify:track:first-liked",
		ContextUri: likedCollectionUri,
		IsPlaying:  true,
	}
	p.handleLikedPlayRetry(context.Background())

	if len(rec.take()) != 1 {
		t.Errorf("state moved: got %d commands, want exactly 1 (no retry)", len(rec.take()))
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times after a taken command, want 0", n)
	}
	if p.likedViaUserForm.Load() {
		t.Error("the flag must stay clear when the primary (bare) context was honored")
	}
}

// (f) nothing resolvable at retry time: skip the retry instead of re-sending
// a dead context (the background resolver keeps retrying; a later play will
// carry the real uri once it lands).
func TestPlay_LikedSongsEscapeHatchSkipsWhenUnresolved(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "") // chronic-429 shape: /v1/me keeps failing

	playLikedSongs(t, p)
	p.handleLikedPlayRetry(context.Background())

	if len(rec.take()) != 1 {
		t.Errorf("unresolved: got %d commands, want exactly 1 (no retry with a dead context)", len(rec.take()))
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me looked up %d times before skipping, want exactly 1", n)
	}
	if p.likedViaUserForm.Load() {
		t.Error("the flag must stay clear when the retry was skipped")
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
		{canonicalLikedPlaylistUri, false, "fetchPlaylist"},
		{canonicalLikedPlaylistUri, true, "fetchLibraryTracks"},
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

// (h) a session started through the user-specific collection: when the
// Connect state reports a playlist-shaped id for it — including the canonical
// Liked-Songs playlist id itself — expansion pages library tracks (not the
// canonical playlist).
func TestQueueExpand_LikedViaUserFormForcesLibraryRoute(t *testing.T) {
	t.Parallel()

	p := newTestQueueExpandPlayer(t)
	var asLibraries []bool
	p.queueExpandPageFn = func(_ context.Context, _ string, _, _ int, asLibrary bool) ([]any, int, error) {
		asLibraries = append(asLibraries, asLibrary) // ordered before the channel send
		items, total := likedTestPage()
		return items, total, nil
	}
	p.likedViaUserForm.Store(true)

	rs := &RemoteState{
		TrackUri:   "spotify:track:t00",
		ContextUri: canonicalLikedPlaylistUri,
		NextTracks: []QueueTrack{{Uri: "spotify:track:t01", TrackId: "t01"}},
	}
	p.expandQueue(rs)

	select {
	case res := <-p.queueExpandedCh:
		if len(asLibraries) != 1 || !asLibraries[0] {
			t.Errorf("page called with asLibrary=%v, want [true] for the playlist-shaped liked id", asLibraries)
		}
		if res.contextUri != canonicalLikedPlaylistUri || len(res.list) != 2 || res.total != 2 {
			t.Errorf("result: got %+v, want the 2-track library page for the playlist-shaped liked id", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no queue expansion result for the playlist-shaped liked id")
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
// the flag boundary — re-fetch instead.
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
	p.likedViaUserForm.Store(true)

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
