package daemon

import (
	"context"
	"errors"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
)

// issue #15 — unit tests for the SECONDARY liked-playlist discovery probe
// (probeLookupChildEntities, player.go), issued when the libraryV3
// PseudoPlaylist scan yields no nested playlist uri. The mocking pattern is
// the exact dealer-push one: newDealerPushTestPlayer + fn fields on the
// AppPlayer — lookupChildEntitiesFn stands in for the pathfinder transport,
// likedPlaylistCountFn for candidate validation — so no session and no
// network are involved.

// (1) libraryV3 empty + the probe returns one spotify:playlist uri + count > 0
// → the candidate is validated through the count seam and persisted.
func TestLikedProbe_PayloadUriResolvesWhenEmpty(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	p := newDealerPushTestPlayer(t, state)

	var probed bool
	p.lookupChildEntitiesFn = func(_ context.Context) ([]byte, error) {
		probed = true
		return []byte(`{"data":{"child":{"entities":[{"uri":"spotify:playlist:5oQ4wV0tYBnJc9xK3mZaR2"},{"name":"Liked Songs"}]}}}`), nil
	}
	var validated []string
	p.likedPlaylistCountFn = func(_ context.Context, uri string) (int, bool) {
		validated = append(validated, uri)
		return 7, true
	}

	if got := p.probeLookupChildEntities(context.Background()); got != "spotify:playlist:5oQ4wV0tYBnJc9xK3mZaR2" {
		t.Fatalf("probe return: got %q want spotify:playlist:5oQ4wV0tYBnJc9xK3mZaR2", got)
	}
	if !probed {
		t.Error("probe transport was not invoked")
	}
	if state.LikedPlaylistURI != "spotify:playlist:5oQ4wV0tYBnJc9xK3mZaR2" {
		t.Errorf("state.LikedPlaylistURI: got %q want spotify:playlist:5oQ4wV0tYBnJc9xK3mZaR2", state.LikedPlaylistURI)
	}
	if len(validated) != 1 || validated[0] != "spotify:playlist:5oQ4wV0tYBnJc9xK3mZaR2" {
		t.Errorf("validated candidates: got %v want [spotify:playlist:5oQ4wV0tYBnJc9xK3mZaR2]", validated)
	}
}

// (2) probe response containing NO spotify:playlist uri → stays empty, no
// validation call, no panic.
func TestLikedProbe_NoPlaylistUriYieldsNothing(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	p := newDealerPushTestPlayer(t, state)

	p.lookupChildEntitiesFn = func(_ context.Context) ([]byte, error) {
		return []byte(`{"data":{"child":{"entities":[{"name":"Liked Songs","id":"abc123"}]}}}`), nil
	}
	called := false
	p.likedPlaylistCountFn = func(_ context.Context, _ string) (int, bool) {
		called = true
		return 3, true
	}

	if got := p.probeLookupChildEntities(context.Background()); got != "" {
		t.Errorf("probe return: got %q want empty", got)
	}
	if state.LikedPlaylistURI != "" {
		t.Errorf("state.LikedPlaylistURI: got %q want empty (no tokens in the response)", state.LikedPlaylistURI)
	}
	if called {
		t.Error("count validation ran although no candidate was found")
	}
}

// (3) probe errors — 5xx, PersistedQuery rotation, plain transport error —
// are handled gracefully: empty result, state untouched, no panic.
func TestLikedProbe_ErrorsAreGraceful(t *testing.T) {
	t.Parallel()
	errs := map[string]error{
		"5xx":       &pathfinderError{Status: 503, msg: "pathfinder status 503: upstream unavailable"},
		"persisted": &pathfinderError{Status: 200, PersistedQuery: true, msg: "pathfinder: PersistedQueryNotFound"},
		"plain":     errors.New("pathfinder request failed: dial tcp: i/o timeout"),
	}
	for name, err := range errs {
		t.Run(name, func(t *testing.T) {
			state := &librespot.AppState{}
			p := newDealerPushTestPlayer(t, state)

			p.lookupChildEntitiesFn = func(_ context.Context) ([]byte, error) {
				return nil, err
			}
			called := false
			p.likedPlaylistCountFn = func(_ context.Context, _ string) (int, bool) {
				called = true
				return 3, true
			}

			if got := p.probeLookupChildEntities(context.Background()); got != "" {
				t.Errorf("probe return: got %q want empty", got)
			}
			if state.LikedPlaylistURI != "" {
				t.Errorf("state.LikedPlaylistURI: got %q want empty", state.LikedPlaylistURI)
			}
			if called {
				t.Error("count validation ran although the probe errored")
			}
		})
	}
}

// (4) state already resolved before the attempt → the probe is NOT issued at
// all (no transport call, no validation), existing value untouched.
func TestLikedProbe_SkippedWhenAlreadyResolved(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	state.LikedPlaylistURI = "spotify:playlist:existing"
	p := newDealerPushTestPlayer(t, state)

	probed := false
	p.lookupChildEntitiesFn = func(_ context.Context) ([]byte, error) {
		probed = true
		return []byte(`{"data":{"child":{"entities":[{"uri":"spotify:playlist:other"}]}}}`), nil
	}
	called := false
	p.likedPlaylistCountFn = func(_ context.Context, _ string) (int, bool) {
		called = true
		return 9, true
	}

	if got := p.probeLookupChildEntities(context.Background()); got != "" {
		t.Errorf("probe return: got %q want empty", got)
	}
	if probed {
		t.Error("probe transport was invoked although a uri was already persisted")
	}
	if called {
		t.Error("count validation ran although a uri was already persisted")
	}
	if state.LikedPlaylistURI != "spotify:playlist:existing" {
		t.Errorf("state.LikedPlaylistURI: got %q want spotify:playlist:existing (no overwrite)", state.LikedPlaylistURI)
	}
}
