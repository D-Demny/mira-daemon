package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// issue #56 — the Liked-Songs play path. The UI tap carries the bare pseudo id
// `spotify:collection:tracks`; the daemon plays the REAL per-user liked-songs
// playlist uri (`spotify:playlist:<id>`) whenever one has been resolved (the
// background resolver / the opportunistic me/playlists scan persist it), which
// is a context Connect receivers reliably start. Until then the tap falls back
// to STANDALONE TRACK playback — the tapped offset track when the request
// carries one, otherwise the first library track via a bounded fetch — and
// NEVER sends the bare pseudo id or the legacy user-specific collection form
// (Connect receivers reject both and clear playback). A taken liked-songs play
// marks the session as liked (queue expansion pages library tracks even for a
// playlist-shaped Connect echo) and arms NO escape-hatch retry: the real
// context starts reliably on-device. The retained retry machinery
// (arm/observe/handle — wired for the follow-up cleanup pass, not armed from
// the play path anymore) is exercised below by arming it manually; its guard
// sends only a REAL resolved uri — a still-unresolved retry skips the re-send
// entirely instead of emitting a rejected context. Spotify's global "Today's
// Top Hits" playlist is NOT a liked-songs context and is never sent here.
// Reuses the existing seams (fakeMeFn + nopStateStore from api_server_test.go)
// plus the sendDeviceCommandFn transport seam; fallback tests use the
// libraryFirstTrackFn seam instead of touching a session.

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
// credentials username, an opaque (non-JWT) access token — AND no persisted
// liked-songs playlist uri: every resolution source is empty by design until a
// test seeds one. No session exists: any code path that would hit the network
// must go through a seam (webMeAccountFn / libraryFirstTrackFn).
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

// (a) wire contract: a liked-songs play with a RESOLVED context leaves the
// envelope with the real per-user playlist uri as the connect context — no
// account-id traffic on the path — and the tapped offset rides along
// unchanged. The pure builder still passes every other context through
// byte-for-byte.
func TestPlay_LikedSongsResolvedSendsRealPlaylistContext(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")
	const realLiked = "spotify:playlist:liked-real"
	p.app.state.LikedPlaylistURI = realLiked

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
	if got := cmds[0].Context.Uri; got != realLiked {
		t.Errorf("primary context: got %q want the resolved per-user playlist uri %q", got, realLiked)
	}
	if got, want := cmds[0].Context.Url, "context://"+realLiked; got != want {
		t.Errorf("context url: got %q want %q", got, want)
	}
	if cmds[0].Options.Offset == nil || cmds[0].Options.Offset.Uri != "spotify:track:first" || cmds[0].Options.Offset.Position != 42 {
		t.Errorf("offset: got %+v, want unchanged {uri:spotify:track:first pos:42}", cmds[0].Options.Offset)
	}
	if got := cmds[0].Options.SkipTo.TrackUri; got != "spotify:track:first" {
		t.Errorf("skip_to: got %q, want the offset track uri (bug29 mirroring)", got)
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times on a resolved play, want 0 (the real context needs no account id)", n)
	}
	if !p.likedSessionActive.Load() {
		t.Error("a taken liked-songs play must mark the session for library-track queue expansion")
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("a reliable real-playlist context arms no escape-hatch retry")
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

// (b) nothing resolved yet: the play is NOT refused and NOTHING
// collection-shaped goes out — the tapped offset track plays standalone. The
// session is NOT marked as liked (a plain track play) and no retry is armed.
// A subsequent non-liked play leaves all state clean.
func TestPlay_LikedSongsUnresolvedFallsBackToTappedTrack(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "") // fresh install: no persisted playlist uri

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{
		Uri:    likedCollectionUri,
		Offset: &ApiRequestPlayOffset{Uri: "spotify:track:first", Position: 42},
	})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (liked songs): %v", err)
	}

	cmds := rec.take()
	if len(cmds) != 1 {
		t.Fatalf("sent %d commands, want exactly 1 (the fallback track play)", len(cmds))
	}
	if got := cmds[0].Context.Uri; got != "spotify:track:first" {
		t.Errorf("unresolved fallback: got %q want the tapped track played standalone", got)
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times on the fallback path, want 0 (a track play does no account-id work)", n)
	}
	if p.likedSessionActive.Load() {
		t.Error("the standalone fallback is a plain track play and must not mark the session as liked")
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("the standalone fallback arms no escape-hatch retry")
	}

	req2, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: "spotify:playlist:pl"})
	if _, err := p.handleApiRequest(context.Background(), req2); err != nil {
		t.Fatalf("play (playlist): %v", err)
	}
	if p.playRetryPending != nil || p.likedSessionActive.Load() {
		t.Error("a playlist play must leave all liked-session state clear")
	}
	if len(rec.take()) != 2 {
		t.Errorf("sent %d commands, want 2 (one per play)", len(rec.take()))
	}
}

// (b) no offset either — the fallback pages the first library track through
// the bounded fetch seam and plays it standalone.
func TestPlay_LikedSongsUnresolvedFallsBackToFirstLibraryTrack(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "")
	p.libraryFirstTrackFn = func(context.Context) (string, error) {
		return "spotify:track:first-lib", nil
	}

	playLikedSongs(t, p) // no offset — must not be refused

	cmds := rec.take()
	if len(cmds) != 1 {
		t.Fatalf("sent %d commands, want exactly 1 (the fallback track play)", len(cmds))
	}
	if got := cmds[0].Context.Uri; got != "spotify:track:first-lib" {
		t.Errorf("unresolved fallback without offset: got %q want the first library track", got)
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times, want 0", n)
	}
}

// (b) the first-library-track fetch fails too — the tap is refused with a
// rate-limit-friendly message and NOTHING goes out on the wire.
func TestPlay_LikedSongsUnresolvedFallbackSurfacesFetchFailure(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "")
	p.libraryFirstTrackFn = func(context.Context) (string, error) {
		return "", errors.New("library query rate-limited")
	}

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: likedCollectionUri})
	_, err := p.handleApiRequest(context.Background(), req)
	if err == nil {
		t.Fatal("want an error while no real context and no library track are available")
	}
	if !strings.Contains(err.Error(), "still being set up") {
		t.Errorf("fallback error: got %q, want the retry-later message", err.Error())
	}
	if len(rec.take()) != 0 {
		t.Errorf("sent %d commands on a failed fallback, want 0", len(rec.take()))
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times, want 0", n)
	}
}

// armManualRetrySnapshot wires a positive pre-state and manually arms the
// retained escape hatch with the bare pseudo context — the shape the follow-up
// cleanup pass will restore in production. No command is sent by this helper:
// these tests exercise the machinery itself, not the (now un-armed) play path.
func armManualRetrySnapshot(t *testing.T, p *AppPlayer) {
	t.Helper()
	p.state.remoteState = &RemoteState{
		DeviceId:   "target-dev",
		DeviceName: "Target Dev",
		TrackUri:   "spotify:track:prev",
		ContextUri: "spotify:playlist:pl",
		IsPlaying:  true,
	}
	p.armLikedPlayRetry("target-dev", "Target Dev", ApiRequestDataPlay{Uri: likedCollectionUri})
	if p.playRetryPending == nil || p.playRetryTimerCh == nil {
		t.Fatal("precondition: the manual arm must snapshot and arm")
	}
}

// clearReceiverState moves the active state to the cleared/stopped shape a
// receiver reports when it rejects a context (negative transition).
func clearReceiverState(p *AppPlayer) {
	p.state.remoteState = &RemoteState{DeviceId: "target-dev", DeviceName: "Target Dev"}
}

// markReceiverPlaying moves the active state to a positive shape (a track is
// active and playback runs — the receiver took the command).
func markReceiverPlaying(p *AppPlayer) {
	p.state.remoteState = &RemoteState{
		DeviceId:   "target-dev",
		DeviceName: "Target Dev",
		TrackUri:   "spotify:track:first-liked",
		ContextUri: "spotify:playlist:7EL2aPD9U3qbjggALcJ7IV", // the internal echo a receiver reports
		IsPlaying:  true,
	}
}

// (c) a resolved liked-songs play arms NO escape hatch: the real per-user
// context starts reliably on-device, so there is nothing to re-send. A stale
// invocation of the retained machinery stays a no-op.
func TestPlay_LikedSongsResolvedArmsNoRetry(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")
	const realLiked = "spotify:playlist:liked-real"
	p.app.state.LikedPlaylistURI = realLiked

	playLikedSongs(t, p)
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Fatal("a resolved liked-songs play must not arm the escape-hatch retry")
	}

	p.handleLikedPlayRetry(context.Background()) // stale firing: nothing armed
	p.observeLikedRetryTransition(context.Background())
	if len(rec.take()) != 1 {
		t.Errorf("sent %d commands, want exactly 1 (the primary play)", len(rec.take()))
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times, want 0", n)
	}
}

// (c) the retained escape hatch, manually armed while nothing is resolved: a
// NEGATIVE clear observed on the next state update fires the one-shot retry
// early — but the guard must keep the wire clean. There is no real context to
// send yet and the bare pseudo id must never go out; the armed state is
// consumed either way.
func TestRetry_UnresolvedNegativeClearSkipsResend(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "")

	armManualRetrySnapshot(t, p)
	clearReceiverState(p) // receiver rejected the context
	p.observeLikedRetryTransition(context.Background())

	if len(rec.take()) != 0 {
		t.Errorf("unresolved retry: got %d commands, want 0 (no real context to resend)", len(rec.take()))
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times, want 0 (resolution is a state read, no web traffic)", n)
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("the one-shot retry must be consumed even when the guard skips the re-send")
	}
	if p.likedSessionActive.Load() {
		t.Error("a skipped retry must not mark the session as liked")
	}
}

// (c) a real per-user playlist uri that lands between arming and the early
// fire upgrades the retry: it re-sends the REAL context and re-marks the
// session as liked for library-track queue expansion — no /v1/me traffic on
// the way (the persisted uri is read straight from state).
func TestRetry_UpgradesToRealContextWhenItLands(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "")

	armManualRetrySnapshot(t, p)
	const lateLiked = "spotify:playlist:late-real"
	p.app.state.LikedPlaylistURI = lateLiked // the background resolver lands before the fire
	clearReceiverState(p)
	p.observeLikedRetryTransition(context.Background())

	cmds := rec.take()
	if len(cmds) != 1 {
		t.Fatalf("upgraded retry: got %d commands, want 1 (the re-send)", len(cmds))
	}
	if got, want := cmds[0].Context.Uri, lateLiked; got != want {
		t.Errorf("retry context: got %q want the late-resolved per-user playlist uri %q", got, want)
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times, want 0 (the persisted uri needs no account lookup)", n)
	}
	if !p.likedSessionActive.Load() {
		t.Error("a fired retry must re-mark the session as liked-songs for queue expansion")
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("the retry is one-shot: the armed state must be consumed")
	}
}

// (c) the timer path with everything still unresolved — no state change
// through the full window, nothing resolvable at firing time either: the
// retry is skipped, the wire stays clean, and the armed state is consumed.
func TestRetry_TimerPathUnresolvedSkips(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "") // every source stays empty

	armManualRetrySnapshot(t, p)
	p.handleLikedPlayRetry(context.Background()) // the armed timer fires after the full window

	if len(rec.take()) != 0 {
		t.Errorf("unresolved timer retry: got %d commands, want 0", len(rec.take()))
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times, want 0", n)
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("the one-shot retry must be consumed even when the guard skips")
	}
}

// (c) defense in depth: the active state moved to a positive shape without an
// observer pass in between, and only then does the timer value arrive — the
// retry classifies the current state first and suppresses itself.
func TestRetry_SuppressedWhenStatePositiveBeforeFiring(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	armManualRetrySnapshot(t, p)
	markReceiverPlaying(p) // playback started; the observer hook did not run first
	p.handleLikedPlayRetry(context.Background())

	if len(rec.take()) != 0 {
		t.Errorf("state moved: got %d commands, want 0 (no retry after a positive transition)", len(rec.take()))
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times, want 0", n)
	}
	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("the suppressed retry must still consume the armed state")
	}
	if p.likedSessionActive.Load() {
		t.Error("a suppressed retry must not mark the session as liked")
	}
}

// (c) a new play supersedes an armed retry: even if the stale timer fires
// afterwards, there is nothing to re-send and all escape-hatch state is gone.
func TestPlay_NewPlaySupersedesArmedRetry(t *testing.T) {
	t.Parallel()
	p, _, rec := newFix7Player(t, "me-user")

	armManualRetrySnapshot(t, p)
	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: "spotify:playlist:pl"})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (playlist): %v", err)
	}

	if p.playRetryPending != nil || p.playRetryTimerCh != nil || p.likedSessionActive.Load() {
		t.Error("a superseding play must clear all escape-hatch state")
	}
	p.handleLikedPlayRetry(context.Background()) // stale firing: must be a no-op
	if len(rec.take()) != 1 {
		t.Errorf("sent %d commands, want exactly 1 (the new play, no stale retry)", len(rec.take()))
	}
}

// (d) a POSITIVE transition observed on the state update path — a track is
// active and playback is running — disarms the armed retry without firing; a
// stale timer value afterwards is a no-op, so the retry never double-issues.
func TestRetry_PositiveTransitionDisarmsArmedRetry(t *testing.T) {
	t.Parallel()
	p, me, rec := newFix7Player(t, "me-user")

	armManualRetrySnapshot(t, p)
	markReceiverPlaying(p) // the receiver started playback of the context
	p.observeLikedRetryTransition(context.Background())

	if p.playRetryPending != nil || p.playRetryTimerCh != nil {
		t.Error("a positive transition must disarm the armed retry and its timer")
	}
	p.handleLikedPlayRetry(context.Background()) // stale timer firing: no-op
	if len(rec.take()) != 0 {
		t.Errorf("positive transition: got %d commands, want 0 (no retry after the receiver took the command)", len(rec.take()))
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me looked up %d times, want 0", n)
	}
	if p.likedSessionActive.Load() {
		t.Error("a disarmed retry must not mark the session as liked")
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
