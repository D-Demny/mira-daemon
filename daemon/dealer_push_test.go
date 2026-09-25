package daemon

import (
	"testing"
)

// Device-anchored ids from the T20 log capture: the id Spotify pushes in the
// hm://playlist/v2/playlist/<id> path for the Liked-Songs feed entry and the
// real per-user liked playlist id (all 22 chars).
const (
	pseudoLikedPushID = "37i9dQZF1F5p3rmiWPIYgZ" // pushed uri-path id
	userLikedID       = "37i9dQZF1DX5wDmLW735Yd" // real per-user liked playlist
)

// (issue #17, PR #18) playlistURIsInText must stop at a clean token boundary:
// protobuf dealer payloads carry binary tail bytes right after the literal
// token (T23 build-152 capture). Only [0-9A-Za-z] runs of 20..25 chars yield
// candidates; everything else is rejected or trimmed away.

func TestPlaylistURIsInText_BinaryTailTrimsToCleanID(t *testing.T) {
	t.Parallel()
	s := "spotify:playlist:" + userLikedID + string([]byte{0x12, 0x18, 'N', 0x00})
	got := playlistURIsInText(s)
	want := []string{"spotify:playlist:" + userLikedID}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("candidates: got %#v want %#v (binary tail must not leak in)", got, want)
	}
}

func TestPlaylistURIsInText_ShortRunYieldsNothing(t *testing.T) {
	t.Parallel()
	s := "spotify:playlist:abcdefghij!" // 10 alnum then non-alnum → run < 20
	if got := playlistURIsInText(s); len(got) != 0 {
		t.Fatalf("candidates: got %#v want none (short run rejected)", got)
	}
}

func TestPlaylistURIsInText_TwoTokensAcrossBinaryGarbageDeduped(t *testing.T) {
	t.Parallel()
	garbage := string([]byte{0x1a, 0x27, 0x00, '\t', '/', 0xff})
	s := "spotify:playlist:" + pseudoLikedPushID + garbage +
		"spotify:playlist:" + userLikedID + garbage +
		"spotify:playlist:" + pseudoLikedPushID
	got := playlistURIsInText(s)
	want := []string{"spotify:playlist:" + pseudoLikedPushID, "spotify:playlist:" + userLikedID}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("candidates: got %#v want %#v (clean tokens, deduped in order)", got, want)
	}
}

func TestPlaylistURIsInText_QuotedJSONUnchanged(t *testing.T) {
	t.Parallel()
	s := `{"data":[{"uri":"spotify:playlist:` + userLikedID + `","name":"x"},{"uri":"spotify:playlist:` + pseudoLikedPushID + `"}]}`
	got := playlistURIsInText(s)
	want := []string{"spotify:playlist:" + userLikedID, "spotify:playlist:" + pseudoLikedPushID}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("candidates: got %#v want %#v (JSON behavior must be unchanged)", got, want)
	}
}
