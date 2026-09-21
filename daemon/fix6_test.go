package daemon

import (
	"testing"
)

// issue #56 fix #6 — the account-id resolution chain's last-resort tier: while
// the Web API GET /v1/me stays rate-limited and no cached/JWT id exists, the
// paired account's stored credentials username (state.Credentials.Username)
// supplies the id. The chain itself is untouched by the playlist-uri rework —
// but the play path now resolves liked songs through the persisted per-user
// PLAYLIST uri instead of an account id, so the resolve tests below pin that
// no credentials tier ever synthesizes a context: while nothing is resolved,
// resolvePlayContextUri yields "" and the play path owns the standalone-track
// fallback. Reuses the existing seams (fakeMeFn + recordingStateStore from
// api_server_test.go; newBGResolverPlayer from fix4_test.go).

// (a) /v1/me success: the fetched id wins and is cached + persisted exactly
// once — a pre-existing credentials username must not shadow it.
func TestSpotifyAccountId_WebMeSuccessWinsOverCredentials(t *testing.T) {
	t.Parallel()
	p, state, store, me := newBGResolverPlayer(t) // me fails by default
	state.Credentials.Username = "1234567890"

	me.id = "me-user"
	got := p.spotifyAccountId()
	if want := "me-user"; got != want {
		t.Fatalf("account id: got %q want %q (web api /v1/me must win)", got, want)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me lookups: got %d want exactly 1", n)
	}
	if state.AccountID != "me-user" {
		t.Errorf("state.AccountID: got %q want me-user (success is persisted)", state.AccountID)
	}
	if n := store.saves.Load(); n != 1 {
		t.Errorf("persist saves: got %d want exactly 1", n)
	}
}

// (b) /v1/me failing + credentials username present: the playlist-uri rework
// no longer synthesizes a liked-songs context from an account id — resolve
// yields "" (the play path owns the standalone-track fallback), no /v1/me
// traffic happens, and nothing is persisted. Once a playlist uri IS persisted
// it short-circuits everything regardless of credentials.
func TestResolvePlayContextUri_IgnoresCredentialsUsername(t *testing.T) {
	t.Parallel()
	p, state, store, me := newBGResolverPlayer(t) // me.id == "" — chronic-429 shape
	state.Credentials.Username = "1234567890"

	uri := p.resolvePlayContextUri(likedCollectionUri)
	if uri != "" {
		t.Errorf("unresolved liked context: got %q want empty (no user-form synthesis from credentials)", uri)
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me lookups: got %d want 0 (the play path does no account-id work)", n)
	}
	if state.AccountID != "" {
		t.Errorf("state.AccountID: got %q want empty", state.AccountID)
	}
	if n := store.saves.Load(); n != 0 {
		t.Errorf("persist saves: got %d want 0", n)
	}

	// a persisted playlist uri wins outright — no credentials tier in sight
	p.app.state.LikedPlaylistURI = "spotify:playlist:late-real"
	if got := p.resolvePlayContextUri(likedCollectionUri); got != "spotify:playlist:late-real" {
		t.Errorf("resolved liked context: got %q want the persisted playlist uri", got)
	}
}

// (c) nothing persisted and the credentials username unusable (empty or
// email-shaped): resolve yields "" — nothing is refused and no rejected
// context goes out; state.AccountID stays reserved for an authoritative
// resolution.
func TestResolvePlayContextUri_UnresolvedYieldsEmpty(t *testing.T) {
	t.Parallel()

	// (c1) genuinely empty credentials username
	p, state, _, me := newBGResolverPlayer(t) // me.id == "" and Credentials zero-valued
	if state.Credentials.Username != "" {
		t.Fatal("test precondition: credentials username must be empty")
	}
	if uri := p.resolvePlayContextUri(likedCollectionUri); uri != "" {
		t.Errorf("no persisted uri: got %q, want empty (the play path owns the fallback)", uri)
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me lookups: got %d want 0", n)
	}

	// (c2) an email-shaped credentials value never builds a context
	p2, state2, _, me2 := newBGResolverPlayer(t) // me2.id == ""
	state2.Credentials.Username = "user@example.com"
	if uri := p2.resolvePlayContextUri(likedCollectionUri); uri != "" {
		t.Errorf("email-shaped credentials must not build a liked-songs context, got %q", uri)
	} else if me2.calls.Load() != 0 {
		t.Errorf("/v1/me lookups: got %d want 0", me2.calls.Load())
	}
	if state2.AccountID != "" {
		t.Errorf("state.AccountID must stay empty: got %q", state2.AccountID)
	}
}

// (d) priority: a cached/persisted id beats the credentials username — no
// /v1/me traffic, no fallback needed.
func TestSpotifyAccountId_CachedIDBeatsCredentials(t *testing.T) {
	t.Parallel()
	p, _, _, me := newBGResolverPlayer(t) // me would fail anyway; must not be called
	p.app.state.AccountID = "cached-user"
	p.app.state.Credentials.Username = "1234567890"

	if got := p.spotifyAccountId(); got != "cached-user" {
		t.Errorf("account id: got %q want cached-user (the cache wins over the credentials fallback)", got)
	}
	if n := me.calls.Load(); n != 0 {
		t.Errorf("/v1/me lookups with a cached id: got %d want 0", n)
	}
}
