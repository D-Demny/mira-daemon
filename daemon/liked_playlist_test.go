package daemon

import (
	"encoding/json"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
)

// issue #56 — unit tests for the opportunistic libraryV3 scan that finds the
// REAL per-user liked-songs playlist uri inside the pseudo "Liked Songs" item
// of a raw Pathfinder payload (scanLikedPlaylistURI + firstPlaylistURInItem,
// webapi_library.go). The scan is pure JSON work with no session involved, so
// these tests pin the identification contract: the explicit collection uri is
// the canonical signal, a PseudoPlaylist named "Liked Songs" is the secondary,
// and same-named user playlists are decoys that must never be picked.

const scanRealLikedURI = "spotify:playlist:37i9dQZF1F5p3rmiWPIYgZ" // observed on-device

func newScanTestPlayer(t *testing.T) *AppPlayer {
	t.Helper()
	return &AppPlayer{app: &App{log: &librespot.NullLogger{}, state: &librespot.AppState{}}}
}

// likedScanPayload wraps raw item objects in the data.me.libraryV3.items
// envelope the scan expects — each entry is {"item": <raw item>}.
func likedScanPayload(t *testing.T, items ...map[string]any) []byte {
	t.Helper()
	wrapped := make([]map[string]any, 0, len(items))
	for _, it := range items {
		wrapped = append(wrapped, map[string]any{"item": it})
	}
	env := map[string]any{"data": map[string]any{"me": map[string]any{
		"libraryV3": map[string]any{"items": wrapped},
	}}}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal scan payload: %v", err)
	}
	return b
}

// a user playlist the owner named "Liked Songs" — its id belongs to THAT
// playlist and must never come back as the liked-songs context.
func decoyUserPlaylist(name, uri string) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"__typename": "Playlist",
			"name":       name,
			"uri":        uri,
		},
	}
}

// (1) the canonical shape: an item carrying the collection uri (in both the
// canonical data.uri slot and the sibling _uri slot) plus the real playlist
// uri nested somewhere inside it. The scan returns that nested uri — and a
// decoy user playlist on the same page is ignored.
func TestScanLikedPlaylistURI_PseudoItemWithNestedRealUri(t *testing.T) {
	t.Parallel()
	p := newScanTestPlayer(t)

	items := []map[string]any{
		decoyUserPlaylist("Liked Songs", "spotify:user:u1:playlist:decoy1"), // decoy FIRST on the page
		{
			"_uri": likedCollectionUri,
			"data": map[string]any{
				"__typename": "PseudoPlaylist",
				"name":       "Liked Songs",
				"uri":        likedCollectionUri,
				"contexts":   map[string]any{"uri": scanRealLikedURI}, // the real uri nested in the item
			},
		},
	}

	if got := p.scanLikedPlaylistURI(likedScanPayload(t, items...)); got != scanRealLikedURI {
		t.Errorf("scan: got %q want the real liked-songs playlist uri %q", got, scanRealLikedURI)
	}
}

// (2) several nested playlist uris in one item: the FIRST in document order
// wins (Go marshals map keys sorted, so document order is deterministic here).
func TestScanLikedPlaylistURI_PicksFirstNestedUriInDocumentOrder(t *testing.T) {
	t.Parallel()
	p := newScanTestPlayer(t)

	items := []map[string]any{{
		"data": map[string]any{
			"uri": likedCollectionUri,
			"contexts": map[string]any{
				"a": map[string]any{"uri": "spotify:playlist:first"},
				"z": map[string]any{"uri": "spotify:playlist:second"},
			},
		},
	}}

	if got := p.scanLikedPlaylistURI(likedScanPayload(t, items...)); got != "spotify:playlist:first" {
		t.Errorf("scan: got %q want the document-first uri spotify:playlist:first", got)
	}
}

// (3) the explicit collection uri wins over any PseudoPlaylist name match —
// even when the named decoy item appears earlier on the page.
func TestScanLikedPlaylistURI_ExplicitPseudoBeatsSameNamedDecoy(t *testing.T) {
	t.Parallel()
	p := newScanTestPlayer(t)

	items := []map[string]any{
		{ // secondary-shaped decoy first: PseudoPlaylist named Liked Songs WITHOUT the collection uri
			"data": map[string]any{
				"__typename": "PseudoPlaylist",
				"name":       "Liked Songs",
				"contexts":   map[string]any{"uri": "spotify:playlist:named-decoy"},
			},
		},
		{ // the canonical item later: explicit collection uri carrying the real one
			"data": map[string]any{
				"__typename": "PseudoPlaylist",
				"name":       "Liked Songs",
				"uri":        likedCollectionUri,
				"contexts":   map[string]any{"uri": scanRealLikedURI},
			},
		},
	}

	if got := p.scanLikedPlaylistURI(likedScanPayload(t, items...)); got != scanRealLikedURI {
		t.Errorf("scan: got %q want the uri carried by the explicit collection item %q", got, scanRealLikedURI)
	}
}

// (4) anti false-positive: a pseudo item that carries NO real playlist uri must
// NOT fall through to a same-named user playlist elsewhere on the page — its
// id belongs to that playlist and is not the liked-songs context.
func TestScanLikedPlaylistURI_PseudoWithoutRealUriDoesNotFallThrough(t *testing.T) {
	t.Parallel()
	p := newScanTestPlayer(t)

	items := []map[string]any{
		{
			"data": map[string]any{
				"__typename": "PseudoPlaylist",
				"name":       "Liked Songs",
				"uri":        likedCollectionUri,
				"id":         "37i9dQZF1F5p3rmiWPIYgZ", // id without the uri prefix does not count
			},
		},
		decoyUserPlaylist("Liked Songs", "spotify:user:u1:playlist:decoy2"),
	}

	if got := p.scanLikedPlaylistURI(likedScanPayload(t, items...)); got != "" {
		t.Errorf("scan: got %q want empty (a pseudo item without a real uri must not be padded from a decoy)", got)
	}
}

// (5) secondary signal only — payload variants that keep the real uri but drop
// the collection uri: a PseudoPlaylist named "Liked Songs" (any case) with a
// nested real uri qualifies.
func TestScanLikedPlaylistURI_SecondaryNameMatchWithoutCollectionUri(t *testing.T) {
	t.Parallel()
	p := newScanTestPlayer(t)

	items := []map[string]any{
		decoyUserPlaylist("liked songs", "spotify:user:u1:playlist:decoy3"), // user playlist, wrong typename
		{
			"data": map[string]any{
				"__typename": "PseudoPlaylist",
				"name":       "liked songs", // case-insensitive match
				"contexts":   map[string]any{"uri": scanRealLikedURI},
			},
		},
	}

	if got := p.scanLikedPlaylistURI(likedScanPayload(t, items...)); got != scanRealLikedURI {
		t.Errorf("scan: got %q want the uri carried by the named PseudoPlaylist %q", got, scanRealLikedURI)
	}
}

// (6) nothing matches: ordinary user playlists with real uris yield "" — their
// ids are user playlist ids, not the liked-songs context. Malformed payloads
// and empty envelopes do too.
func TestScanLikedPlaylistURI_NoMatchYieldsEmpty(t *testing.T) {
	t.Parallel()
	p := newScanTestPlayer(t)

	items := []map[string]any{
		decoyUserPlaylist("Chill", "spotify:user:u1:playlist:a"),
		decoyUserPlaylist("Workout", "spotify:user:u1:playlist:b"),
	}
	if got := p.scanLikedPlaylistURI(likedScanPayload(t, items...)); got != "" {
		t.Errorf("scan: got %q want empty (no pseudo item on the page)", got)
	}

	if got := p.scanLikedPlaylistURI([]byte(`{"data":{"me":{"libraryV3":{"items":`)); got != "" {
		t.Errorf("malformed payload: got %q want empty", got)
	}
	if got := p.scanLikedPlaylistURI([]byte(`not-json`)); got != "" {
		t.Errorf("non-JSON payload: got %q want empty", got)
	}
	if got := p.scanLikedPlaylistURI(likedScanPayload(t)); got != "" {
		t.Errorf("empty items list: got %q want empty", got)
	}
}

// (7) the per-item uri picker itself: first spotify:playlist:* string in
// document order, "" when absent or malformed.
func TestFirstPlaylistURInItem(t *testing.T) {
	t.Parallel()

	raw, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"name": "Liked Songs",
			"nested": map[string]any{
				"a": map[string]any{"uri": "spotify:playlist:first"},
				"z": map[string]any{"uri": "spotify:playlist:second"},
			},
		},
	})
	if got := firstPlaylistURInItem(raw); got != "spotify:playlist:first" {
		t.Errorf("picker: got %q want the document-first uri", got)
	}

	rawNo, _ := json.Marshal(map[string]any{"data": map[string]any{"uri": likedCollectionUri, "id": "37i9dQZF1F5p3rmiWPIYgZ"}})
	if got := firstPlaylistURInItem(rawNo); got != "" {
		t.Errorf("picker: got %q want empty (the collection uri and a bare id are not playlist uris)", got)
	}

	if got := firstPlaylistURInItem([]byte(`{"data": `)); got != "" {
		t.Errorf("picker: malformed raw item: got %q want empty", got)
	}
}
