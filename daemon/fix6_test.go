package daemon

import (
	"testing"

	librespot "github.com/devgianlu/go-librespot"
)

// issue #56 — the play path resolves liked songs through the persisted
// per-user PLAYLIST uri instead of an account id, and the account-id
// resolution tiers are gone. While nothing is resolved, resolvePlayContextUri
// yields "" and the play path owns the standalone-track fallback. The tests
// below pin that no credentials tier ever synthesizes a context: the stored
// credentials username (state.Credentials.Username) is never consulted by the
// resolution path, and the reserved state.AccountID field is never populated
// from it.

// (b) credentials username present but no persisted playlist uri: resolve
// yields "" (the play path owns the standalone-track fallback). Once a
// playlist uri IS persisted it short-circuits everything regardless of
// credentials.
func TestResolvePlayContextUri_IgnoresCredentialsUsername(t *testing.T) {
	t.Parallel()
	state := &librespot.AppState{}
	state.Credentials.Username = "1234567890"
	p := &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state, stateStore: nopStateStore{}}}

	if uri := p.resolvePlayContextUri(likedCollectionUri); uri != "" {
		t.Errorf("unresolved liked context: got %q want empty (no user-form synthesis from credentials)", uri)
	}
	if state.AccountID != "" {
		t.Errorf("state.AccountID: got %q want empty", state.AccountID)
	}

	// a persisted playlist uri wins outright — no credentials tier in sight
	state.LikedPlaylistURI = "spotify:playlist:late-real"
	if got := p.resolvePlayContextUri(likedCollectionUri); got != "spotify:playlist:late-real" {
		t.Errorf("resolved liked context: got %q want the persisted playlist uri", got)
	}
}

// (c) nothing persisted and the credentials username unusable (empty or
// email-shaped): resolve yields "" — nothing is refused and no rejected
// context goes out; state.AccountID stays reserved and is never populated by
// the play path.
func TestResolvePlayContextUri_UnresolvedYieldsEmpty(t *testing.T) {
	t.Parallel()

	// (c1) genuinely empty credentials username
	state := &librespot.AppState{}
	if state.Credentials.Username != "" {
		t.Fatal("test precondition: credentials username must be empty")
	}
	p := &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state, stateStore: nopStateStore{}}}
	if uri := p.resolvePlayContextUri(likedCollectionUri); uri != "" {
		t.Errorf("no persisted uri: got %q, want empty (the play path owns the fallback)", uri)
	}

	// (c2) an email-shaped credentials value never builds a context
	state2 := &librespot.AppState{}
	state2.Credentials.Username = "user@example.com"
	p2 := &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: state2, stateStore: nopStateStore{}}}
	if uri := p2.resolvePlayContextUri(likedCollectionUri); uri != "" {
		t.Errorf("email-shaped credentials must not build a liked-songs context, got %q", uri)
	}
	if state2.AccountID != "" {
		t.Errorf("state.AccountID must stay empty: got %q", state2.AccountID)
	}
}
