package spclient

import "testing"

// issue #56 fix #3: the join must yield EXACTLY one leading v1 segment no
// matter how the caller wrote the path — "/v1/me" used to land on
// https://api.spotify.com/v1/v1/me (410 Gone) because the old "already has
// v1" guard missed paths with a leading slash.
func TestWebApiURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/v1/me", "https://api.spotify.com/v1/me"},                 // broken shape on-device (#56)
		{"me", "https://api.spotify.com/v1/me"},                     // UI proxy paths arrive v1-less
		{"/v1/tracks/abc", "https://api.spotify.com/v1/tracks/abc"}, // queue art backfill (#50), same leading-slash shape
		{"tracks/abc", "https://api.spotify.com/v1/tracks/abc"},
		{"v1/me/tracks", "https://api.spotify.com/v1/me/tracks"}, // already prefixed — no double prefix
		{"playlists/xyz/tracks", "https://api.spotify.com/v1/playlists/xyz/tracks"},
		{"", "https://api.spotify.com/"},
	}
	for _, c := range cases {
		if got := webApiURL(c.in).String(); got != c.want {
			t.Errorf("webApiURL(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
