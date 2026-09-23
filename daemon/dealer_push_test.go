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
// the raw push URI. Candidates are merged in priority order: (1) the bare id of
// an hm://playlist/v2/playlist/<id> push uri — the push is ABOUT that playlist,
// (2) literal spotify:playlist: tokens in the payload bytes — Spotify's
// protobuf payloads carry the token verbatim where the JSON item walk finds
// nothing (T20 device capture), (3) the JSON item walk. Each candidate must
// pass BOTH the count query (total > 0) AND playlistV2.format == "liked-songs"
// — a missing/unreadable format rejects it (fail closed) — via the
// likedPlaylistCountFn / likedPlaylistFormatFn seams before being persisted,
// only when state.LikedPlaylistURI is still empty.

// device-anchored ids from the T20 log capture: the id Spotify pushes in the
// hm://playlist/v2/playlist/<id> path for the Liked-Songs feed entry, the real
// per-user liked playlist id, and a normal user playlist id (all 22 chars).
const (
	pseudoLikedPushID = "37i9dQZF1F5p3rmiWPIYgZ" // pushed uri-path id
	userLikedID       = "37i9dQZF1DX5wDmLW735Yd" // real per-user liked playlist
	normalListID      = "4oOZJEq1TBUti6PSouTo5M" // a normal user playlist
)

func newDealerPushTestPlayer(t *testing.T, state *librespot.AppState) *AppPlayer {
	t.Helper()
	return &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state, stateStore: nopStateStore{}}}
}

// validationExpectation is the mocked (total, format) result for one candidate
// uri; a uri absent from the table fails the count query. A "" format models a
// present-but-empty/absent playlistV2.format — which the liked-songs
// discriminator must reject.
type validationExpectation struct {
	total  int
	format string
}

// mockLikedValidation wires both validation seams from one expectation table
// and records every candidate handed to the count seam (in order).
func mockLikedValidation(t *testing.T, p *AppPlayer, table map[string]validationExpectation, validated *[]string) {
	t.Helper()
	p.likedPlaylistCountFn = func(_ context.Context, uri string) (int, bool) {
		*validated = append(*validated, uri)
		v, ok := table[uri]
		if !ok {
			return 0, false
		}
		return v.total, true
	}
	p.likedPlaylistFormatFn = func(_ context.Context, uri string) (string, bool) {
		v, ok := table[uri]
		if !ok {
			return "", false
		}
		return v.format, true
	}
}

// protoPushPayload builds protobuf-LOOKING bytes: length/tag bytes around
// verbatim tokens, no JSON structure — exactly the shape the T20 device log
// captured in hm://playlist/v2/playlist/<id> pushes.
func protoPushPayload(tokens ...string) []byte {
	var b []byte
	for _, tk := range tokens {
		b = append(b, 0x1a, byte(len(tk)))
		b = append(b, tk...)
	}
	return b
}

// (i) the live production shape: push uri hm://playlist/v2/playlist/<id> whose
// payload bytes carry the literal token; count > 0 AND format "liked-songs" →
// the uri is persisted. The uri-path candidate and the payload token
// de-duplicate to ONE validated candidate, and the uri-path candidate is tried
// first.
func TestDealerPush_ProdShapeResolvesWithLikedSongsFormat(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	p := newDealerPushTestPlayer(t, state)

	var validated []string
	mockLikedValidation(t, p, map[string]validationExpectation{
		"spotify:playlist:" + pseudoLikedPushID: {total: 503, format: "liked-songs"},
	}, &validated)

	msg := dealer.Message{
		Uri:     "hm://playlist/v2/playlist/" + pseudoLikedPushID,
		Payload: protoPushPayload("spotify:playlist:" + pseudoLikedPushID),
	}
	if err := p.handleDealerMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleDealerMessage: %v", err)
	}
	want := "spotify:playlist:" + pseudoLikedPushID
	if state.LikedPlaylistURI != want {
		t.Errorf("state.LikedPlaylistURI: got %q want %s", state.LikedPlaylistURI, want)
	}
	if len(validated) != 1 || validated[0] != want {
		t.Errorf("validated candidates: got %v want [%s] (uri-path + payload token de-duped)", validated, want)
	}
}

// (ii) same push shape but the uri-path candidate's format is NOT "liked-songs"
// (the pushed id is a catalog/pseudo playlist, not the user's liked list) →
// rejected fail-closed, and the SECOND payload token that passes BOTH checks
// wins.
func TestDealerPush_NonLikedFormatRejectedThenPayloadTokenWins(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	p := newDealerPushTestPlayer(t, state)

	var validated []string
	mockLikedValidation(t, p, map[string]validationExpectation{
		"spotify:playlist:" + pseudoLikedPushID: {total: 503, format: "playlist"}, // count ok, format is not liked-songs
		"spotify:playlist:" + userLikedID:       {total: 501, format: "liked-songs"},
	}, &validated)

	msg := dealer.Message{
		Uri:     "hm://playlist/v2/playlist/" + pseudoLikedPushID,
		Payload: protoPushPayload("spotify:playlist:" + userLikedID),
	}
	if err := p.handleDealerMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleDealerMessage: %v", err)
	}
	want := "spotify:playlist:" + userLikedID
	if state.LikedPlaylistURI != want {
		t.Errorf("state.LikedPlaylistURI: got %q want %s (second candidate wins)", state.LikedPlaylistURI, want)
	}
	wantOrder := []string{"spotify:playlist:" + pseudoLikedPushID, "spotify:playlist:" + userLikedID}
	if len(validated) != 2 || validated[0] != wantOrder[0] || validated[1] != wantOrder[1] {
		t.Errorf("validated candidates: got %v want %v (uri-path first, then payload tokens)", validated, wantOrder)
	}
}

// (iii) a push for a NORMAL list: uri-path id + track-token-only payload, and
// the fetched format is not "liked-songs" → nothing is persisted, no error.
func TestDealerPush_NormalListPushSetsNothing(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	p := newDealerPushTestPlayer(t, state)

	var validated []string
	mockLikedValidation(t, p, map[string]validationExpectation{
		"spotify:playlist:" + normalListID: {total: 13, format: "playlist"},
	}, &validated)

	msg := dealer.Message{
		Uri:     "hm://playlist/v2/playlist/" + normalListID,
		Payload: []byte(`{"tracks":[{"uri":"spotify:track:0vHfY43h0GV1lRv5wPpbJR"}]}`),
	}
	if err := p.handleDealerMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleDealerMessage: %v", err)
	}
	if state.LikedPlaylistURI != "" {
		t.Errorf("state.LikedPlaylistURI: got %q want empty (normal list push)", state.LikedPlaylistURI)
	}
	if len(validated) != 1 || validated[0] != "spotify:playlist:"+normalListID {
		t.Errorf("validated candidates: got %v want [spotify:playlist:%s]", validated, normalListID)
	}
}

// (iv) the same push while a uri is already persisted → no overwrite and no
// validation call at all (count OR format seam).
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
	p.likedPlaylistFormatFn = func(_ context.Context, _ string) (string, bool) {
		called = true
		return "liked-songs", true
	}

	msg := dealer.Message{
		Uri:     "hm://playlist/v2/playlist/" + pseudoLikedPushID,
		Payload: protoPushPayload("spotify:playlist:" + pseudoLikedPushID),
	}
	if err := p.handleDealerMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleDealerMessage: %v", err)
	}
	if state.LikedPlaylistURI != "spotify:playlist:existing" {
		t.Errorf("state.LikedPlaylistURI: got %q want spotify:playlist:existing (no overwrite)", state.LikedPlaylistURI)
	}
	if called {
		t.Error("validation ran although a uri was already persisted")
	}
}

// (v) a push carrying NO spotify:playlist token and a non-qualifying uri shape
// (hm://playlist/v2/list/... yields no uri-path candidate) → nothing happens,
// no validation call.
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

// (vi) a failing first candidate (count query error) falls through to the next
// one, which is persisted once it passes BOTH checks.
func TestDealerPush_CountFailureFallsThrough(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	p := newDealerPushTestPlayer(t, state)

	var validated []string
	mockLikedValidation(t, p, map[string]validationExpectation{
		"spotify:playlist:" + userLikedID: {total: 17, format: "liked-songs"},
	}, &validated) // the uri-path candidate (normal list id) is absent → count query fails

	msg := dealer.Message{
		Uri:     "hm://playlist/v2/playlist/" + normalListID,
		Payload: protoPushPayload("spotify:playlist:" + userLikedID),
	}
	if err := p.handleDealerMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleDealerMessage: %v", err)
	}
	want := "spotify:playlist:" + userLikedID
	if state.LikedPlaylistURI != want {
		t.Errorf("state.LikedPlaylistURI: got %q want %s (fall-through past the failing candidate)", state.LikedPlaylistURI, want)
	}
	wantOrder := []string{"spotify:playlist:" + normalListID, "spotify:playlist:" + userLikedID}
	if len(validated) != 2 || validated[0] != wantOrder[0] || validated[1] != wantOrder[1] {
		t.Errorf("validated candidates: got %v want %v (order matters)", validated, wantOrder)
	}
}
