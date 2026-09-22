package daemon

import (
	"context"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/dealer"
)

// issue #15 — unit tests for the opportunistic liked-playlist resolution from
// dealer hm://playlist/ pushes (handleDealerMessage +
// resolveLikedPlaylistFromDealerPush, player.go). The receiver hands over a
// fully decoded payload (base64 + gzip already stripped by dealer/recv.go) plus
// the raw push URI; literal spotify:playlist: tokens are pulled from BOTH and
// each candidate is validated with the pathfinder count query (total > 0) via
// the likedPlaylistCountFn seam before being persisted — only when
// state.LikedPlaylistURI is still empty.

func newDealerPushTestPlayer(t *testing.T, state *librespot.AppState) *AppPlayer {
	t.Helper()
	return &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state, stateStore: nopStateStore{}}}
}

// (1) state empty + a push whose payload carries a spotify:playlist uri → the
// candidate is validated through the count seam and persisted.
func TestDealerPush_PlaylistUriInPayloadResolvesWhenEmpty(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	p := newDealerPushTestPlayer(t, state)

	var validated []string
	p.likedPlaylistCountFn = func(_ context.Context, uri string) (int, bool) {
		validated = append(validated, uri)
		return 3, true
	}

	msg := dealer.Message{
		Uri:     "hm://playlist/v2/list/liked-songs",
		Payload: []byte(`{"data":{"contexts":[{"uri":"spotify:playlist:frompayload"}]}}`),
	}
	if err := p.handleDealerMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleDealerMessage: %v", err)
	}
	if state.LikedPlaylistURI != "spotify:playlist:frompayload" {
		t.Errorf("state.LikedPlaylistURI: got %q want spotify:playlist:frompayload", state.LikedPlaylistURI)
	}
	if len(validated) != 1 || validated[0] != "spotify:playlist:frompayload" {
		t.Errorf("validated candidates: got %v want [spotify:playlist:frompayload]", validated)
	}
}

// (2) the same push while a uri is already persisted → no overwrite and no
// validation call at all.
func TestDealerPush_AlreadyResolvedIsNotOverwritten(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	state.LikedPlaylistURI = "spotify:playlist:existing"
	p := newDealerPushTestPlayer(t, state)

	called := false
	p.likedPlaylistCountFn = func(_ context.Context, _ string) (int, bool) {
		called = true
		return 9, true
	}

	msg := dealer.Message{
		Uri:     "hm://playlist/v2/list/liked-songs",
		Payload: []byte(`{"data":{"contexts":[{"uri":"spotify:playlist:frompayload"}]}}`),
	}
	if err := p.handleDealerMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleDealerMessage: %v", err)
	}
	if state.LikedPlaylistURI != "spotify:playlist:existing" {
		t.Errorf("state.LikedPlaylistURI: got %q want spotify:playlist:existing (no overwrite)", state.LikedPlaylistURI)
	}
	if called {
		t.Error("count validation ran although a uri was already persisted")
	}
}

// (3) a push carrying NO spotify:playlist token (payload nor URI path) →
// nothing happens, no validation call.
func TestDealerPush_NoPlaylistUriDoesNothing(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	p := newDealerPushTestPlayer(t, state)

	called := false
	p.likedPlaylistCountFn = func(_ context.Context, _ string) (int, bool) {
		called = true
		return 9, true
	}

	msg := dealer.Message{
		Uri:     "hm://playlist/v2/list/liked-songs",
		Payload: []byte(`{"data":{"name":"Liked Songs","id":"abc123"}}`),
	}
	if err := p.handleDealerMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleDealerMessage: %v", err)
	}
	if state.LikedPlaylistURI != "" {
		t.Errorf("state.LikedPlaylistURI: got %q want empty (no tokens in the push)", state.LikedPlaylistURI)
	}
	if called {
		t.Error("count validation ran although no candidate was found")
	}
}

// (4) tokens from the push URI path itself are candidates too (T13 observation
// hm://playlist/v2/list/liked-songs-artist/...), and a failing first candidate
// falls through to the next one, which is persisted once it passes validation.
func TestDealerPush_UriPathTokensAndValidationFallthrough(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	p := newDealerPushTestPlayer(t, state)

	var validated []string
	p.likedPlaylistCountFn = func(_ context.Context, uri string) (int, bool) {
		validated = append(validated, uri)
		if uri == "spotify:playlist:bad" {
			return 0, true // count query succeeds but the playlist is empty
		}
		return 5, true
	}

	msg := dealer.Message{
		Uri:     "hm://playlist/v2/list/liked-songs-artist/spotify:playlist:bad/fallback/spotify:playlist:good",
		Payload: []byte(`{}`),
	}
	if err := p.handleDealerMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleDealerMessage: %v", err)
	}
	if state.LikedPlaylistURI != "spotify:playlist:good" {
		t.Errorf("state.LikedPlaylistURI: got %q want spotify:playlist:good (fall-through past the failing candidate)", state.LikedPlaylistURI)
	}
	want := []string{"spotify:playlist:bad", "spotify:playlist:good"}
	if len(validated) != 2 || validated[0] != want[0] || validated[1] != want[1] {
		t.Errorf("validated candidates: got %v want %v (order matters)", validated, want)
	}
}
