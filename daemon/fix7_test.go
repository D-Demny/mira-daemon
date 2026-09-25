package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// issue #56 — the Liked-Songs play path (upstream b9a0970). The UI tap carries
// the bare library alias `spotify:collection:tracks`; buildPlayCommand
// rewrites it to the paired account's own collection
// (`spotify:user:<username>:collection`) — the only liked-songs context
// Connect receivers actually start. The username comes from the live session
// (the playUsername seam stands in for tests); a play that arrives before the
// session reports one sends the alias as-is rather than a half-built user form.
// A taken liked-songs play marks the session as liked so queue expansion pages
// library tracks even for a playlist-shaped Connect echo. Spotify's global
// "Today's Top Hits" playlist is NOT a liked-songs context and is never sent
// here. Reuses the nopStateStore fixture plus the sendDeviceCommandFn transport
// seam.

// sentCommands records the connect commands a stubbed sendDeviceCommand saw
// (single-goroutine: tests drive handleApiRequest directly, no Run loop
// involved).
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

// newFix7Player wires a minimal AppPlayer for the play path: stubbed
// transport (sendDeviceCommandFn) and an active target device in the observer
// state so resolveTargetDevice has somewhere to send. No session exists, so
// playUsername() yields "" unless a test sets the playUsernameFn seam — the
// same "" a real player reports before its first welcome packet.
func newFix7Player(t *testing.T) (*AppPlayer, *sentCommands) {
	t.Helper()
	rec := &sentCommands{}
	state := &librespot.AppState{OAuth: librespot.OAuthState{AccessToken: "opaque-token-no-dots"}}
	p := &AppPlayer{
		app:                 &App{log: &librespot.NullLogger{}, state: state, stateStore: nopStateStore{}},
		state:               &State{remoteState: &RemoteState{DeviceId: "target-dev", DeviceName: "Target Dev"}},
		sendDeviceCommandFn: rec.send,
	}
	return p, rec
}

// (a) wire contract: a liked-songs play whose session reports a username
// leaves the envelope with the ACCOUNT COLLECTION as the connect context —
// the bare library alias is rewritten in both Context.Uri and Url — and the
// tapped offset rides along unchanged. The session is marked as liked so queue
// expansion pages library tracks even for a playlist-shaped Connect echo. The
// pure builder still passes every other context through byte-for-byte.
func TestPlay_LikedSongsSendsAccountCollection(t *testing.T) {
	t.Parallel()
	p, rec := newFix7Player(t)
	const username = "u1"
	p.playUsernameFn = func() string { return username }

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{
		Uri:    likedCollectionUri,
		Offset: &ApiRequestPlayOffset{Uri: "spotify:track:first", Position: 42},
	})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (liked songs): %v", err)
	}

	const want = "spotify:user:" + username + ":collection"
	cmds := rec.take()
	if len(cmds) != 1 {
		t.Fatalf("sent %d commands, want exactly 1 (the primary play)", len(cmds))
	}
	if got := cmds[0].Context.Uri; got != want {
		t.Errorf("primary context: got %q want the account collection %q", got, want)
	}
	if got, w := cmds[0].Context.Url, "context://"+want; got != w {
		t.Errorf("context url: got %q want %q", got, w)
	}
	if cmds[0].Options.Offset == nil || cmds[0].Options.Offset.Uri != "spotify:track:first" || cmds[0].Options.Offset.Position != 42 {
		t.Errorf("offset: got %+v, want unchanged {uri:spotify:track:first pos:42}", cmds[0].Options.Offset)
	}
	if got := cmds[0].Options.SkipTo.TrackUri; got != "spotify:track:first" {
		t.Errorf("skip_to: got %q, want the offset track uri (bug29 mirroring)", got)
	}
	if !p.likedSessionActive.Load() {
		t.Error("a taken liked-songs play must mark the session for library-track queue expansion")
	}

	// the pure builder still passes every other context through untouched,
	// and never rewrites anything that is not the bare library alias
	for _, uri := range []string{
		"spotify:playlist:abc123",
		"spotify:album:abc123",
		"spotify:user:u2:collection:tracks",
	} {
		if got := buildPlayCommand(ApiRequestDataPlay{Uri: uri}, username).Context.Uri; got != uri {
			t.Errorf("builder passthrough %q: got %q", uri, got)
		}
	}
}

// (b) the session has not reported a username yet (before the welcome packet):
// the play is NOT refused and the bare library alias goes out AS-IS — no
// half-built user form is ever constructed. A subsequent non-liked play clears
// the liked-session flag again.
func TestPlay_LikedSongsWithoutUsernameSendsAliasUntouched(t *testing.T) {
	t.Parallel()
	p, rec := newFix7Player(t) // no session, no seam → playUsername() == ""

	req, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: likedCollectionUri})
	if _, err := p.handleApiRequest(context.Background(), req); err != nil {
		t.Fatalf("play (liked songs): %v", err)
	}

	cmds := rec.take()
	if len(cmds) != 1 {
		t.Fatalf("sent %d commands, want exactly 1 (the primary play)", len(cmds))
	}
	if got, want := cmds[0].Context.Uri, likedCollectionUri; got != want {
		t.Errorf("context: got %q want the bare alias sent untouched while no username is known", got)
	}
	if got, want := cmds[0].Context.Url, "context://"+likedCollectionUri; got != want {
		t.Errorf("context url: got %q want %q", got, want)
	}
	if !p.likedSessionActive.Load() {
		t.Error("a taken liked-songs play must mark the session for library-track queue expansion")
	}

	req2, _ := NewApiRequest(ApiRequestTypePlay, ApiRequestDataPlay{Uri: "spotify:playlist:pl"})
	if _, err := p.handleApiRequest(context.Background(), req2); err != nil {
		t.Fatalf("play (playlist): %v", err)
	}
	if p.likedSessionActive.Load() {
		t.Error("a playlist play must clear the liked-session flag")
	}
	if got := len(rec.take()); got != 2 {
		t.Errorf("sent %d commands, want 2 (one per play)", got)
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
		{"spotify:user:u1:collection", false, "fetchLibraryTracks"},
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
