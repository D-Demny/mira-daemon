package daemon

import (
	"testing"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

func TestProjectQueue_EmptyInputReturnsNil(t *testing.T) {
	t.Parallel()

	if got := projectQueue(nil, 10); got != nil {
		t.Errorf("nil input: got %v want nil", got)
	}
	if got := projectQueue([]*connectpb.ProvidedTrack{}, 10); got != nil {
		t.Errorf("empty slice: got %v want nil", got)
	}
}

func TestProjectQueue_SkipsNilAndEmptyUriEntries(t *testing.T) {
	t.Parallel()

	// Spotify sometimes ships "queued but unresolved" rows where the URI hasnt been filled in yet
	tracks := []*connectpb.ProvidedTrack{
		{Uri: "spotify:track:keep_1"},
		nil,
		{Uri: ""},
		{Uri: "spotify:track:keep_2"},
	}

	got := projectQueue(tracks, 10)
	if len(got) != 2 {
		t.Fatalf("expected 2 valid entries, got %d: %+v", len(got), got)
	}
	if got[0].Uri != "spotify:track:keep_1" || got[1].Uri != "spotify:track:keep_2" {
		t.Errorf("wrong entries surfaced: %+v", got)
	}
}

func TestProjectQueue_HonorsLimitAndStopsEarly(t *testing.T) {
	t.Parallel()

	// 15 valid tracks, limit=10,  exactly 10 returne
	tracks := make([]*connectpb.ProvidedTrack, 15)
	for i := range tracks {
		tracks[i] = &connectpb.ProvidedTrack{Uri: "spotify:track:abc"}
	}

	got := projectQueue(tracks, 10)
	if len(got) != 10 {
		t.Errorf("expected %d entries (limit), got %d", 10, len(got))
	}
}

func TestProjectQueue_LimitInteractsWithSkippedEntries(t *testing.T) {
	t.Parallel()

	// the limit check counts valid entries, not loop iterations
	tracks := []*connectpb.ProvidedTrack{
		{Uri: ""}, // skipped
		{Uri: "spotify:track:a"},
		nil, // skipped
		{Uri: "spotify:track:b"},
		{Uri: "spotify:track:c"},
		{Uri: "spotify:track:d"}, // dropped by limit
	}

	got := projectQueue(tracks, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(got), got)
	}
	if got[0].Uri != "spotify:track:a" ||
		got[1].Uri != "spotify:track:b" ||
		got[2].Uri != "spotify:track:c" {
		t.Errorf("unexpected ordering: %+v", got)
	}
}

func TestProjectQueue_ExtractsTrackIdFromThreePartSpotifyUri(t *testing.T) {
	t.Parallel()

	// 3-part Spotify URI, trackId is the third part. non-3-part leaves it empty
	tracks := []*connectpb.ProvidedTrack{
		{Uri: "spotify:track:abc123"},
		{Uri: "spotify:local:something"}, // 3-part but not "track" - still extracts "something"
		{Uri: "spotify:track:abc:extra"}, // 4-part - strings.SplitN(",",3) returns 3 parts where the third has ":extra"
		{Uri: "local-file"},              // 1-part - no trackId
	}

	got := projectQueue(tracks, 10)
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(got))
	}
	if got[0].TrackId != "abc123" {
		t.Errorf("standard URI: trackId got %q want %q", got[0].TrackId, "abc123")
	}
	if got[1].TrackId != "something" {
		t.Errorf("local URI: trackId got %q want %q", got[1].TrackId, "something")
	}
	// SplitN(":", 3) gives 3 parts max, third part keeps remaining colons.
	if got[2].TrackId != "abc:extra" {
		t.Errorf("4-part URI: trackId got %q want %q", got[2].TrackId, "abc:extra")
	}
	if got[3].TrackId != "" {
		t.Errorf("non-Spotify URI: trackId should be empty, got %q", got[3].TrackId)
	}
}

func TestProjectQueue_PopulatesMetadataFieldsAndConvertsImageUrl(t *testing.T) {
	t.Parallel()

	// image_url passes through convertSpotifyImageUrl, rest of fields direct
	tracks := []*connectpb.ProvidedTrack{
		{
			Uri: "spotify:track:song1",
			Metadata: map[string]string{
				"title":       "Test Track",
				"artist_name": "Test Artist",
				"album_title": "Test Album",
				"image_url":   "spotify:image:ab67616d00001e02deadbeef",
			},
		},
	}

	got := projectQueue(tracks, 10)
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	q := got[0]
	if q.Name != "Test Track" {
		t.Errorf("Name: got %q want %q", q.Name, "Test Track")
	}
	if q.Artist != "Test Artist" {
		t.Errorf("Artist: got %q want %q", q.Artist, "Test Artist")
	}
	if q.Album != "Test Album" {
		t.Errorf("Album: got %q want %q", q.Album, "Test Album")
	}
	if q.ImageUrl != "https://i.scdn.co/image/ab67616d00001e02deadbeef" {
		t.Errorf("ImageUrl not converted via convertSpotifyImageUrl: got %q", q.ImageUrl)
	}
}

func TestProjectQueue_NilMetadataAndEmptyImageUrlAreSafe(t *testing.T) {
	t.Parallel()

	// nil/empty Metadata or empty image_url -> ImageUrl stays empty (not "https://")
	tracks := []*connectpb.ProvidedTrack{
		{Uri: "spotify:track:a"},                                               // Metadata is nil
		{Uri: "spotify:track:b", Metadata: map[string]string{}},                // empty map
		{Uri: "spotify:track:c", Metadata: map[string]string{"image_url": ""}}, // empty image_url
	}

	got := projectQueue(tracks, 10)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	for i, q := range got {
		if q.ImageUrl != "" {
			t.Errorf("entry[%d] ImageUrl should be empty (no conversion of blank input), got %q",
				i, q.ImageUrl)
		}
		if q.Name != "" {
			t.Errorf("entry[%d] Name: got %q want empty", i, q.Name)
		}
	}
}
