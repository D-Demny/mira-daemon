package daemon

import (
	"testing"
)

// issue #56 fix #6 — the play path's last-resort account-id tier: while the
// Web API GET /v1/me stays rate-limited and no cached/JWT id exists, the
// paired account's stored credentials username (state.Credentials.Username,
// the Spotify-issued APWelcome canonical username captured at pairing) builds
// the user-specific liked-songs collection uri; when even that is unusable
// the play falls back to the bare pseudo context. Reuses the existing seams
// (fakeMeFn + recordingStateStore from api_server_test.go;
// newBGResolverPlayer from fix4_test.go).

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

// (b) /v1/me failure + credentials username present: the fallback id is used,
// the liked-songs context builds, and nothing is persisted — state.AccountID
// stays reserved for an authoritative /v1/me resolution (the background
// resolver must keep retrying).
func TestResolvePlayContextUri_WebMeFailureFallsBackToCredentials(t *testing.T) {
	t.Parallel()
	p, state, store, me := newBGResolverPlayer(t) // me.id == "" — chronic-429 shape
	state.Credentials.Username = "1234567890"

	uri := p.resolvePlayContextUri(likedCollectionUri)
	if want := "spotify:user:1234567890:collection:tracks"; uri != want {
		t.Errorf("liked-songs context: got %q want %q", uri, want)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me lookups: got %d want exactly 1 (the fallback is tried only after a failed lookup)", n)
	}
	if state.AccountID != "" {
		t.Errorf("state.AccountID: got %q want empty (the credentials fallback must not be persisted)", state.AccountID)
	}
	if n := store.saves.Load(); n != 0 {
		t.Errorf("persist saves: got %d want 0", n)
	}

	// the fallback is stable across plays on the same player (every play
	// re-tries the bounded /v1/me lookup — the credentials fallback is
	// deliberately not cached)
	if uri2 := p.resolvePlayContextUri(likedCollectionUri); uri2 != uri {
		t.Errorf("repeat play: got %q, want %q", uri2, uri)
	}
	if n := me.calls.Load(); n != 2 {
		t.Errorf("/v1/me lookups over two resolves: got %d want exactly 2 (no caching of the credentials fallback)", n)
	}
}

// (c) both /v1/me and the credentials username unavailable (or email-shaped):
// nothing is refused — the play falls back to the bare pseudo context as a
// documented legacy best effort, and state.AccountID stays reserved for an
// authoritative resolution.
func TestResolvePlayContextUri_NoUsableIdKeepsBareFallback(t *testing.T) {
	t.Parallel()

	// (c1) genuinely empty credentials username
	p, state, _, me := newBGResolverPlayer(t) // me.id == "" and Credentials zero-valued
	if state.Credentials.Username != "" {
		t.Fatal("test precondition: credentials username must be empty")
	}
	uri := p.resolvePlayContextUri(likedCollectionUri)
	if uri != likedCollectionUri {
		t.Errorf("no usable id: got %q, want the bare pseudo context as best-effort fallback", uri)
	}
	if n := me.calls.Load(); n != 1 {
		t.Errorf("/v1/me lookups: got %d want exactly 1 (still tried before falling back)", n)
	}

	// (c2) an email-shaped credentials value is not a usable account id
	p2, state2, _, me2 := newBGResolverPlayer(t) // me2.id == ""
	state2.Credentials.Username = "user@example.com"
	if uri := p2.resolvePlayContextUri(likedCollectionUri); uri != likedCollectionUri {
		t.Errorf("email-shaped credentials username must not build a user-form context, got %q", uri)
	} else if me2.calls.Load() != 1 {
		t.Errorf("/v1/me lookups: got %d want exactly 1", me2.calls.Load())
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
