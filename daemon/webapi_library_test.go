package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestIsLocalWebApiPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"me/playlists", true},
		{"me/player/recently-played", true},
		{"me/tracks", true},
		{"me", false},
		{"me/tracks/extra", false},
		{"me/top", false},
		{"playlists/abc", false},
		{"playlists/abc123/tracks", true},
		{"playlists/abc123/tracks?limit=50", false},
		{"playlists//tracks", false},
		{"", false},
		{"me/playlists/extra", false},
	}
	for _, c := range cases {
		if got := isLocalWebApiPath(c.path); got != c.want {
			t.Errorf("isLocalWebApiPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestParseWebApiPaging(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		query      url.Values
		wantLimit  int
		wantOffset int
	}{
		{"defaults", url.Values{}, 50, 0},
		{"explicit", url.Values{"limit": {"20"}, "offset": {"40"}}, 20, 40},
		{"zero limit falls back", url.Values{"limit": {"0"}}, 50, 0},
		{"negative limit falls back", url.Values{"limit": {"-5"}}, 50, 0},
		{"limit capped at 100", url.Values{"limit": {"500"}}, 100, 0},
		{"negative offset clamped", url.Values{"offset": {"-1"}}, 50, 0},
		{"offset capped", url.Values{"offset": {"999999"}}, 50, 100000},
		{"garbage values fall back", url.Values{"limit": {"abc"}, "offset": {"xyz"}}, 50, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			limit, offset := parseWebApiPaging(c.query)
			if limit != c.wantLimit || offset != c.wantOffset {
				t.Errorf("parseWebApiPaging = (%d, %d), want (%d, %d)", limit, offset, c.wantLimit, c.wantOffset)
			}
		})
	}
}

const libraryV3Fixture = `{
  "data": {
    "me": {
      "libraryV3": {
        "totalCount": 140,
        "items": [
          {
            "item": {
              "_uri": "spotify:playlist:abc123",
              "data": {
                "__typename": "PlaylistV2",
                "uri": "spotify:playlist:abc123",
                "name": "Workout",
                "count": 0,
                "images": {
                  "items": [
                    {
                      "sources": [
                        {"url": "https://i.scdn.com/small.jpg", "width": 60, "height": 60},
                        {"url": "https://i.scdn.com/large.jpg", "width": 640, "height": 640}
                      ]
                    }
                  ]
                },
                "image": {"sources": []},
                "ownerV2": {"data": {"name": "Max", "id": "owner-id", "username": "max"}}
              }
            }
          },
          {
            "item": {
              "_uri": "spotify:collection:tracks",
              "data": {
                "__typename": "PseudoPlaylist",
                "uri": "spotify:collection:tracks",
                "name": "Liked Songs",
                "count": 250,
                "images": {"items": []},
                "image": {"sources": [{"url": "https://i.scdn.com/liked.jpg", "width": 640, "height": 640}]},
                "ownerV2": {"data": {"name": "", "id": "", "username": ""}}
              }
            }
          },
          {
            "item": {
              "_uri": "",
              "data": {
                "__typename": "PlaylistV2",
                "uri": "",
                "name": "",
                "count": 0,
                "images": {"items": []},
                "image": {"sources": []},
                "ownerV2": {"data": {}}
              }
            }
          }
        ]
      }
    }
  }
}`

func TestMapLibraryV3Page(t *testing.T) {
	t.Parallel()
	resp, targets, err := mapLibraryV3Page([]byte(libraryV3Fixture), 50, 0)
	if err != nil {
		t.Fatalf("mapLibraryV3Page: %v", err)
	}
	if resp.Total != 140 || resp.Limit != 50 || resp.Offset != 0 {
		t.Errorf("envelope = total %d limit %d offset %d, want 140/50/0", resp.Total, resp.Limit, resp.Offset)
	}
	if resp.Href != "https://api.spotify.com/v1/me/playlists" {
		t.Errorf("href = %q", resp.Href)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items = %d, want 2 (empty item must be skipped)", len(resp.Items))
	}

	// regular playlist
	pl := resp.Items[0]
	if pl.ID != "abc123" || pl.Name != "Workout" || pl.URI != "spotify:playlist:abc123" {
		t.Errorf("playlist[0] = %+v", pl)
	}
	if pl.Owner.DisplayName != "Max" || pl.Owner.ID != "owner-id" || pl.Owner.Type != "user" {
		t.Errorf("owner = %+v", pl.Owner)
	}
	if len(pl.Images) != 2 || pl.Images[0].Width != 640 || pl.Images[1].Width != 60 {
		t.Errorf("images not sorted by width desc: %+v", pl.Images)
	}
	if pl.Tracks.Total != 0 {
		t.Errorf("playlist[0] tracks.total = %d, want 0 (count fetched later)", pl.Tracks.Total)
	}

	// pseudo-playlist (Liked Songs) carries its count inline
	liked := resp.Items[1]
	if liked.ID != "spotify:collection:tracks" || liked.Name != "Liked Songs" {
		t.Errorf("playlist[1] = %+v", liked)
	}
	if liked.Owner.DisplayName != "Spotify" {
		t.Errorf("liked owner = %q, want fallback %q", liked.Owner.DisplayName, "Spotify")
	}
	if liked.Tracks.Total != 250 {
		t.Errorf("liked tracks.total = %d, want 250", liked.Tracks.Total)
	}
	if len(liked.Images) != 1 || liked.Images[0].URL != "https://i.scdn.com/liked.jpg" {
		t.Errorf("liked images = %+v", liked.Images)
	}

	if len(targets) != 1 || targets[0].idx != 0 || targets[0].uri != "spotify:playlist:abc123" {
		t.Errorf("targets = %+v, want only the regular playlist", targets)
	}
}

func TestMapLibraryV3Page_BadJSON(t *testing.T) {
	t.Parallel()
	if _, _, err := mapLibraryV3Page([]byte("not json"), 50, 0); err == nil {
		t.Error("expected error for non-JSON payload")
	}
}

func TestParsePlaylistCount(t *testing.T) {
	t.Parallel()
	total, err := parsePlaylistCount([]byte(`{"data":{"playlistV2":{"content":{"totalCount":13}}}}`))
	if err != nil || total != 13 {
		t.Errorf("parsePlaylistCount = (%d, %v), want (13, nil)", total, err)
	}
	if _, err := parsePlaylistCount([]byte(`{`)); err == nil {
		t.Error("expected error for truncated payload")
	}
}

func TestPlaylistTracksPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want string
	}{
		{"playlists/abc123/tracks", "abc123"},
		{"playlists/abc123", ""},
		{"playlists/", ""},
		{"playlists//tracks", ""},
		{"playlists/a/b/tracks", ""},
		{"playlists/abc123/tracks/", ""},
		{"playlists/abc123/tracks/extra", ""},
		{"tracks", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := playlistTracksPath(c.path); got != c.want {
			t.Errorf("playlistTracksPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

const recentsFixture = `{
  "data": {
    "lists": [
      {
        "__typename": "List",
        "items": {
          "totalCount": 2,
          "items": [
            {
              "addedAt": {"timestamp": 1787306400000, "isoString": "2026-08-21T10:00:00Z"},
              "entity": {
                "_uri": "spotify:track:111111111111111111111111111",
                "data": {
                  "identityTrait": {
                    "name": "Bohemian Rhapsody",
                    "contributors": [
                      {"uri": "spotify:artist:222222", "name": "Queen"},
                      {"uri": "spotify:artist:333333", "name": ""}
                    ],
                    "contentHierarchyParent": {
                      "uri": "spotify:album:444444",
                      "identityTrait": {"name": "A Night at the Opera"}
                    }
                  },
                  "visualIdentityTrait": {
                    "images": {
                      "sources": [
                        {"url": "https://i.scdn.com/640.jpg", "width": 640, "height": 640},
                        {"url": "https://i.scdn.com/60.jpg", "width": 60, "height": 60}
                      ]
                    }
                  }
                }
              }
            },
            {
              "addedAt": {"timestamp": 1787302800000, "isoString": "2026-08-21T09:00:00Z"},
              "entity": {
                "_uri": "spotify:episode:555555",
                "data": {
                  "identityTrait": {"name": "Some Episode", "contributors": []},
                  "visualIdentityTrait": {"images": null}
                }
              }
            }
          ]
        }
      },
      {
        "__typename": "NotAList",
        "items": {"totalCount": 0, "items": []}
      }
    ]
  }
}`

func TestMapRecentlyPlayedPage(t *testing.T) {
	t.Parallel()
	items, err := mapRecentlyPlayedPage([]byte(recentsFixture))
	if err != nil {
		t.Fatalf("mapRecentlyPlayedPage: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (episode and non-List skipped)", len(items))
	}
	entry, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] type = %T, want map", items[0])
	}
	if entry["played_at"] != "2026-08-21T10:00:00Z" {
		t.Errorf("played_at = %v", entry["played_at"])
	}
	tr, ok := entry["track"].(map[string]any)
	if !ok {
		t.Fatalf("track type = %T, want map", entry["track"])
	}
	if tr["id"] != "111111111111111111111111111" || tr["name"] != "Bohemian Rhapsody" || tr["uri"] != "spotify:track:111111111111111111111111111" {
		t.Errorf("track identity = %+v", tr)
	}
	artists, _ := tr["artists"].([]any)
	if len(artists) != 1 {
		t.Errorf("artists = %+v, want exactly one named contributor", artists)
	} else if artists[0].(map[string]any)["name"] != "Queen" {
		t.Errorf("artists[0] = %+v", artists[0])
	}
	album, _ := tr["album"].(map[string]any)
	if album["name"] != "A Night at the Opera" {
		t.Errorf("album.name = %v", album["name"])
	}
	imgs, _ := album["images"].([]webApiImage)
	if len(imgs) != 2 || imgs[0].Width != 640 || imgs[1].Width != 60 {
		t.Errorf("album.images = %+v", album["images"])
	}
}

// bug19: the newer web-player bundle ships contributors as an {items:[...]}
// object with profile-nested names, a playlist (not album) as the hierarchy
// parent, and a bare-string addedAt variant
const recentsFixtureV2 = `{
  "data": {
    "lists": [
      {
        "__typename": "List",
        "items": {
          "totalCount": 3,
          "items": [
            {
              "addedAt": {"timestamp": 1787306400000, "isoString": "2026-08-21T10:00:00Z"},
              "entity": {
                "_uri": "spotify:track:abc123",
                "data": {
                  "identityTrait": {
                    "name": "Numb",
                    "contributors": {
                      "totalCount": 1,
                      "items": [
                        {"uri": "spotify:artist:lp", "profile": {"name": "Linkin Park"}}
                      ]
                    },
                    "contentHierarchyParent": {
                      "uri": "spotify:playlist:pl1",
                      "identityTrait": {"name": "Linkin Park"}
                    }
                  },
                  "visualIdentityTrait": {
                    "images": {
                      "sources": [{"url": "https://i.scdn.com/a.jpg", "width": 640, "height": 640}]
                    }
                  }
                }
              }
            },
            {
              "addedAt": "2026-08-21T09:00:00Z",
              "entity": {
                "_uri": "spotify:track:def456",
                "data": {
                  "identityTrait": {
                    "name": "In the End",
                    "contributors": [{"name": "Linkin Park"}],
                    "contentHierarchyParent": {
                      "uri": "spotify:album:al1",
                      "identityTrait": {"name": "Meteora"}
                    }
                  },
                  "visualIdentityTrait": {
                    "images": [{"url": "https://i.scdn.com/b.jpg", "width": 300, "height": 300}]
                  }
                }
              }
            },
            {
              "addedAt": {"isoString": "2026-08-21T08:00:00Z"},
              "entity": {
                "_uri": "spotify:track:ghi789",
                "data": {
                  "identityTrait": {
                    "name": "One Step Closer",
                    "contributors": [],
                    "contentHierarchyParent": {
                      "uri": "spotify:artist-playlist:xyz",
                      "identityTrait": {"name": "LP Mix"}
                    }
                  }
                }
              }
            }
          ]
        }
      }
    ]
  }
}`

func TestMapRecentlyPlayedPage_NewShapes(t *testing.T) {
	t.Parallel()
	items, err := mapRecentlyPlayedPage([]byte(recentsFixtureV2))
	if err != nil {
		t.Fatalf("mapRecentlyPlayedPage: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}

	// playlist parent: context_uri set, album stays unnamed
	entry0 := items[0].(map[string]any)
	if entry0["context_uri"] != "spotify:playlist:pl1" {
		t.Errorf("context_uri = %v, want spotify:playlist:pl1", entry0["context_uri"])
	}
	tr0 := entry0["track"].(map[string]any)
	artists0 := tr0["artists"].([]any)
	if len(artists0) != 1 || artists0[0].(map[string]any)["name"] != "Linkin Park" {
		t.Errorf("artists = %+v", artists0)
	}
	album0 := tr0["album"].(map[string]any)
	if album0["name"] != "" {
		t.Errorf("album.name = %v, want empty for a playlist parent", album0["name"])
	}
	if imgs := album0["images"].([]webApiImage); len(imgs) != 1 || imgs[0].Width != 640 {
		t.Errorf("album.images = %+v", album0["images"])
	}

	// album parent: names the album AND is the context
	entry1 := items[1].(map[string]any)
	if entry1["context_uri"] != "spotify:album:al1" {
		t.Errorf("context_uri = %v, want spotify:album:al1", entry1["context_uri"])
	}
	album1 := entry1["track"].(map[string]any)["album"].(map[string]any)
	if album1["name"] != "Meteora" {
		t.Errorf("album.name = %v, want Meteora", album1["name"])
	}
	if entry1["played_at"] != "" {
		t.Errorf("played_at = %v, want empty for the bare-string addedAt variant", entry1["played_at"])
	}

	// non-playable parent: no context_uri at all
	entry2 := items[2].(map[string]any)
	if _, has := entry2["context_uri"]; has {
		t.Errorf("entry2 = %+v, want no context_uri for an artist-playlist parent", entry2)
	}
	if entry2["track"].(map[string]any)["name"] != "One Step Closer" {
		t.Errorf("entry2 track = %+v", entry2["track"])
	}
}

func TestIsPlayableContextURI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		uri  string
		want bool
	}{
		{"spotify:album:1", true},
		{"spotify:playlist:2", true},
		{"spotify:collection:tracks", true},
		{"spotify:artist:3", true},
		{"spotify:artist-playlist:4", false},
		{"spotify:track:5", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isPlayableContextURI(c.uri); got != c.want {
			t.Errorf("isPlayableContextURI(%q) = %v, want %v", c.uri, got, c.want)
		}
	}
}

func TestMapRecentlyPlayedPage_BadJSON(t *testing.T) {
	t.Parallel()
	if _, err := mapRecentlyPlayedPage([]byte("not json")); err == nil {
		t.Error("expected error for non-JSON payload")
	}
}

func TestMapRecentlyPlayedPage_Empty(t *testing.T) {
	t.Parallel()
	items, err := mapRecentlyPlayedPage([]byte(`{"data":{"lists":[]}}`))
	if err != nil || len(items) != 0 {
		t.Errorf("items = %#v (err %v), want empty slice", items, err)
	}
}

func TestRecentsPlayedAt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   recentsAddedAt
		want string
	}{
		{"isoString wins", recentsAddedAt{Timestamp: 1787306400000, IsoString: "2026-08-21T10:00:00Z"}, "2026-08-21T10:00:00Z"},
		{"ms timestamp", recentsAddedAt{Timestamp: 1787306400000}, "2026-08-21T10:00:00Z"},
		{"bare seconds", recentsAddedAt{Timestamp: 1787306400}, "2026-08-21T10:00:00Z"},
		{"empty", recentsAddedAt{}, ""},
	}
	for _, c := range cases {
		if got := recentsPlayedAt(c.in); got != c.want {
			t.Errorf("recentsPlayedAt(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

const playlistTracksFixture = `{
  "data": {
    "playlistV2": {
      "content": {
        "totalCount": 3,
        "items": [
          {
            "itemV2": {
              "_uri": "spotify:track:aaaa",
              "data": {
                "name": "Track A",
                "uri": "spotify:track:aaaa",
                "artists": {"items": [{"profile": {"name": "Artist A"}}]},
                "album": {
                  "name": "Album A",
                  "images": [{"url": "https://i.scdn.com/a.jpg", "width": 640, "height": 640}]
                },
                "durationMs": 210000
              }
            }
          },
          {
            "itemV2": {
              "_uri": "spotify:track:bbbb",
              "data": {"name": "", "uri": "spotify:track:bbbb"}
            }
          },
          {
            "itemV2": {
              "_uri": "spotify:track:cccc",
              "data": {
                "name": "Track C",
                "uri": "",
                "artists": {"items": []},
                "album": {"name": "", "images": {"sources": []}}
              }
            }
          }
        ]
      }
    }
  }
}`

func TestMapPlaylistTracksPage(t *testing.T) {
	t.Parallel()
	items, total, err := mapPlaylistTracksPage([]byte(playlistTracksFixture), 0)
	if err != nil {
		t.Fatalf("mapPlaylistTracksPage: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (empty-name item skipped)", len(items))
	}

	a, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] type = %T, want map", items[0])
	}
	if a["is_local"] != false {
		t.Errorf("is_local = %v, want false", a["is_local"])
	}
	ta, _ := a["track"].(map[string]any)
	if ta["id"] != "aaaa" || ta["name"] != "Track A" || ta["uri"] != "spotify:track:aaaa" {
		t.Errorf("track A = %+v", ta)
	}
	if ta["position"] != 0 {
		t.Errorf("track A position = %v, want 0", ta["position"])
	}
	if ta["duration_ms"] != 210000 {
		t.Errorf("duration_ms = %v, want 210000", ta["duration_ms"])
	}
	artists, _ := ta["artists"].([]any)
	if len(artists) != 1 || artists[0].(map[string]any)["name"] != "Artist A" {
		t.Errorf("artists = %+v", artists)
	}
	albumA, _ := ta["album"].(map[string]any)
	imgsA, _ := albumA["images"].([]webApiImage)
	if albumA["name"] != "Album A" || len(imgsA) != 1 || imgsA[0].Width != 640 {
		t.Errorf("album A = %+v", albumA)
	}

	c, ok := items[1].(map[string]any)
	if !ok {
		t.Fatalf("items[1] type = %T, want map", items[1])
	}
	tc, _ := c["track"].(map[string]any)
	if tc["id"] != "cccc" || tc["name"] != "Track C" || tc["uri"] != "spotify:track:cccc" {
		t.Errorf("track C = %+v, want uri/id fallback to _uri", tc)
	}
	// the empty-name item at index 1 is skipped but still occupies a playlist slot,
	// so Track C (payload index 2) reports absolute position 2
	if tc["position"] != 2 {
		t.Errorf("track C position = %v, want 2 (skipped slot still counted)", tc["position"])
	}
	if _, has := tc["duration_ms"]; has {
		t.Errorf("duration_ms = %v, want absent", tc["duration_ms"])
	}
	artistsC, _ := tc["artists"].([]any)
	if len(artistsC) != 0 {
		t.Errorf("artists C = %+v, want empty", artistsC)
	}
}

func TestMapPlaylistTracksPage_BaseOffset(t *testing.T) {
	t.Parallel()
	items, _, err := mapPlaylistTracksPage([]byte(playlistTracksFixture), 50)
	if err != nil {
		t.Fatalf("mapPlaylistTracksPage: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	ta, _ := items[0].(map[string]any)["track"].(map[string]any)
	tc, _ := items[1].(map[string]any)["track"].(map[string]any)
	if ta["position"] != 50 {
		t.Errorf("track A position = %v, want 50", ta["position"])
	}
	if tc["position"] != 52 {
		t.Errorf("track C position = %v, want 52", tc["position"])
	}
}

func TestMapPlaylistTracksPage_BadJSON(t *testing.T) {
	t.Parallel()
	if _, _, err := mapPlaylistTracksPage([]byte("not json"), 0); err == nil {
		t.Error("expected error for non-JSON payload")
	}
}

// the fetchLibraryTracks payload (me.library.tracks): items[].track wrappers
// with profile-nested artists; one item ships no album data at all (the
// fallback then reads the track's own cover), one carries an inline album.
const savedTracksFixture = `{
  "data": {
    "me": {
      "library": {
        "tracks": {
          "totalCount": 3,
          "items": [
            {
              "track": {
                "_uri": "spotify:track:liked-1",
                "data": {
                  "name": "Faded",
                  "uri": "spotify:track:liked-1",
                  "artists": {"items": [{"profile": {"name": "Alan Walker"}}]},
                  "album": {"name": "", "images": []},
                  "visualIdentityTrait": {
                    "images": [{"url": "https://i.scdn.com/faded.jpg", "width": 300, "height": 300}]
                  },
                  "durationMs": 221000
                }
              }
            },
            {
              "track": {
                "_uri": "spotify:track:liked-2",
                "data": {
                  "name": "Another One Bites the Dust",
                  "uri": "spotify:track:liked-2",
                  "artists": {"items": [{"profile": {"name": "Queen"}}]},
                  "album": {
                    "name": "News of the World",
                    "images": [{"url": "https://i.scdn.com/ntw.jpg", "width": 640, "height": 640}]
                  }
                }
              }
            }
          ]
        }
      }
    }
  }
}`

func TestMapSavedTracksPage(t *testing.T) {
	t.Parallel()
	items, total, err := mapSavedTracksPage([]byte(savedTracksFixture), 0)
	if err != nil {
		t.Fatalf("mapSavedTracksPage: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}

	first, _ := items[0].(map[string]any)
	tf, _ := first["track"].(map[string]any)
	if tf["id"] != "liked-1" || tf["name"] != "Faded" || tf["uri"] != "spotify:track:liked-1" {
		t.Errorf("track 1 = %+v", tf)
	}
	if tf["position"] != 0 {
		t.Errorf("track 1 position = %v, want 0", tf["position"])
	}
	if tf["duration_ms"] != 221000 {
		t.Errorf("duration_ms = %v, want 221000", tf["duration_ms"])
	}
	artists, _ := tf["artists"].([]any)
	if len(artists) != 1 || artists[0].(map[string]any)["name"] != "Alan Walker" {
		t.Errorf("artists = %+v", artists)
	}
	// empty album data: the track's own cover art is used instead (bug22/23)
	album, _ := tf["album"].(map[string]any)
	if album["name"] != "" {
		t.Errorf("album name = %v, want empty", album["name"])
	}
	imgs, _ := album["images"].([]webApiImage)
	if len(imgs) != 1 || imgs[0].URL != "https://i.scdn.com/faded.jpg" {
		t.Errorf("album images = %+v, want the track cover", imgs)
	}

	second, _ := items[1].(map[string]any)
	ts, _ := second["track"].(map[string]any)
	if ts["position"] != 1 {
		t.Errorf("track 2 position = %v, want 1", ts["position"])
	}
	albumS, _ := ts["album"].(map[string]any)
	if albumS["name"] != "News of the World" {
		t.Errorf("album name = %v, want News of the World", albumS["name"])
	}
}

func TestMapSavedTracksPage_BaseOffset(t *testing.T) {
	t.Parallel()
	items, _, err := mapSavedTracksPage([]byte(savedTracksFixture), 50)
	if err != nil {
		t.Fatalf("mapSavedTracksPage: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	tf, _ := items[0].(map[string]any)["track"].(map[string]any)
	ts, _ := items[1].(map[string]any)["track"].(map[string]any)
	if tf["position"] != 50 || ts["position"] != 51 {
		t.Errorf("positions = %v, %v, want 50, 51", tf["position"], ts["position"])
	}
}

func TestMapSavedTracksPage_BadJSON(t *testing.T) {
	t.Parallel()
	if _, _, err := mapSavedTracksPage([]byte("not json"), 0); err == nil {
		t.Error("expected error for non-JSON payload")
	}
}

// the shared lenient mapper must also handle the playlist itemV2 shape
// (mapPlaylistTracksPage keeps its strict struct; this pins the wrapper
// detection used by the new code path)
func TestMapPfTrackItems_ItemV2Shape(t *testing.T) {
	t.Parallel()
	raw := []any{
		map[string]any{
			"itemV2": map[string]any{
				"_uri": "spotify:track:cccc",
				"data": map[string]any{
					"name":    "Track C",
					"uri":     "",
					"artists": map[string]any{"items": []any{}},
				},
			},
		},
	}
	items := mapPfTrackItems(raw, 10)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	tm, _ := items[0].(map[string]any)["track"].(map[string]any)
	if tm["id"] != "cccc" || tm["uri"] != "spotify:track:cccc" {
		t.Errorf("track = %+v, want uri fallback to _uri", tm)
	}
	if tm["position"] != 10 {
		t.Errorf("position = %v, want 10", tm["position"])
	}
}

func TestWebApiImagesFromAny(t *testing.T) {
	t.Parallel()
	wrapped := map[string]any{
		"sources": []any{
			map[string]any{"url": "https://x/1.jpg", "width": 640, "height": 640},
		},
	}
	if imgs := webApiImagesFromAny(wrapped); len(imgs) != 1 || imgs[0].URL != "https://x/1.jpg" || imgs[0].Width != 640 {
		t.Errorf("wrapped = %+v", imgs)
	}
	bare := []any{
		map[string]any{"url": "https://x/2.jpg", "width": 300, "height": 300},
	}
	if imgs := webApiImagesFromAny(bare); len(imgs) != 1 || imgs[0].URL != "https://x/2.jpg" {
		t.Errorf("bare = %+v", imgs)
	}
	if imgs := webApiImagesFromAny(nil); imgs != nil {
		t.Errorf("nil = %+v, want nil", imgs)
	}
	if imgs := webApiImagesFromAny("garbage"); imgs != nil {
		t.Errorf("garbage = %+v, want nil", imgs)
	}
}

func TestWebApiArtistsFromAny(t *testing.T) {
	t.Parallel()
	// older bundle shape: a flat array of {uri, name}
	flat := []any{
		map[string]any{"uri": "spotify:artist:1", "name": "A"},
		map[string]any{"name": "B"},
		map[string]any{"uri": "spotify:artist:3", "name": ""},
	}
	out := webApiArtistsFromAny(flat)
	if len(out) != 2 {
		t.Fatalf("artists = %+v, want 2 (empty name skipped)", out)
	}
	if out[0].(map[string]any)["name"] != "A" || out[0].(map[string]any)["uri"] != "spotify:artist:1" {
		t.Errorf("artists[0] = %+v", out[0])
	}
	if _, has := out[1].(map[string]any)["uri"]; has {
		t.Errorf("artists[1] = %+v, want no uri for empty input uri", out[1])
	}

	// newer bundle shape (bug19): an {items:[...]} object with profile-nested names
	obj := map[string]any{
		"totalCount": 2,
		"items": []any{
			map[string]any{"uri": "spotify:artist:9", "profile": map[string]any{"name": "C", "uri": "spotify:artist:9"}},
			map[string]any{"_uri": "spotify:artist:10", "data": map[string]any{
				"identityTrait": map[string]any{"name": "D"},
			}},
		},
	}
	out2 := webApiArtistsFromAny(obj)
	if len(out2) != 2 {
		t.Fatalf("artists (object shape) = %+v, want 2", out2)
	}
	if out2[0].(map[string]any)["name"] != "C" || out2[0].(map[string]any)["uri"] != "spotify:artist:9" {
		t.Errorf("artists[0] = %+v", out2[0])
	}
	if out2[1].(map[string]any)["name"] != "D" || out2[1].(map[string]any)["uri"] != "spotify:artist:10" {
		t.Errorf("artists[1] = %+v", out2[1])
	}

	if out3 := webApiArtistsFromAny(nil); len(out3) != 0 {
		t.Errorf("artists(nil) = %+v, want empty", out3)
	}
}

func TestHandleWebApiLocal_UnknownPath(t *testing.T) {
	t.Parallel()
	p := &AppPlayer{}
	_, err := p.handleWebApiLocal(context.Background(), ApiRequestDataWebApi{
		Method: "GET",
		Path:   "me/top",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPlaylistCountCache(t *testing.T) {
	t.Parallel()
	p := &AppPlayer{}

	if _, ok := p.cachedPlaylistCount("spotify:playlist:x"); ok {
		t.Error("empty cache must miss")
	}

	p.storePlaylistCount("spotify:playlist:x", 13, true)
	if total, ok := p.cachedPlaylistCount("spotify:playlist:x"); !ok || total != 13 {
		t.Errorf("cached = (%d, %v), want (13, true)", total, ok)
	}

	// expired success entry is evicted
	p.playlistCountCache["spotify:playlist:x"] = playlistCountEntry{
		total: 13, at: time.Now().Add(-playlistCountCacheTTL - time.Minute), ok: true,
	}
	if total, ok := p.cachedPlaylistCount("spotify:playlist:x"); ok {
		t.Errorf("expired success entry = (%d, true), want miss", total)
	}
	if _, exists := p.playlistCountCache["spotify:playlist:x"]; exists {
		t.Error("expired entry must be evicted")
	}

	// failed entries use the shorter TTL
	p.storePlaylistCount("spotify:playlist:y", 0, false)
	if _, ok := p.cachedPlaylistCount("spotify:playlist:y"); !ok {
		t.Error("fresh failed entry must hit (suppresses refetch)")
	}
	p.playlistCountCache["spotify:playlist:y"] = playlistCountEntry{
		total: 0, at: time.Now().Add(-playlistCountFailTTL - time.Minute), ok: false,
	}
	if _, ok := p.cachedPlaylistCount("spotify:playlist:y"); ok {
		t.Error("expired failed entry must miss")
	}
}

func TestFillPlaylistCounts_UsesCache(t *testing.T) {
	t.Parallel()
	p := &AppPlayer{playlistCountCache: map[string]playlistCountEntry{
		"spotify:playlist:a": {total: 13, at: time.Now(), ok: true},
		"spotify:playlist:b": {total: 0, at: time.Now(), ok: false},
	}}
	resp := &webApiPlaylistsResponse{Items: []webApiPlaylist{{}, {}}}
	p.fillPlaylistCounts(context.Background(), []playlistCountTarget{
		{idx: 0, uri: "spotify:playlist:a"},
		{idx: 1, uri: "spotify:playlist:b"},
	}, resp)
	if resp.Items[0].Tracks.Total != 13 {
		t.Errorf("items[0].tracks.total = %d, want 13 (from cache)", resp.Items[0].Tracks.Total)
	}
	if resp.Items[1].Tracks.Total != 0 {
		t.Errorf("items[1].tracks.total = %d, want 0 (cached failure keeps zero)", resp.Items[1].Tracks.Total)
	}
}

// HTTP dispatch: the mux must route the two local paths to
// ApiRequestTypeWebApiLocal and everything else to the old proxy type.

func TestWebApiLocalDispatch(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		target   string
		wantType ApiRequestType
		wantPath string
	}{
		{"playlists local", http.MethodGet, "/web-api/me/playlists?limit=5&offset=10", ApiRequestTypeWebApiLocal, "me/playlists"},
		{"recently played local", http.MethodGet, "/web-api/me/player/recently-played?limit=20", ApiRequestTypeWebApiLocal, "me/player/recently-played"},
		{"saved tracks local", http.MethodGet, "/web-api/me/tracks?limit=50&offset=0", ApiRequestTypeWebApiLocal, "me/tracks"},
		{"other path proxied", http.MethodGet, "/web-api/me/top?limit=1", ApiRequestTypeWebApi, "me/top"},
		{"non-GET proxied", http.MethodPost, "/web-api/me/playlists", ApiRequestTypeWebApi, "me/playlists"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, base := newTestApiServer(t)
			srv.SetPlayerReady(true)

			captured := drainOne(t, srv, nil, nil)
			req, err := http.NewRequest(c.method, base+c.target, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := testClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()

			var got ApiRequest
			select {
			case got = <-captured:
			case <-time.After(2 * time.Second):
				t.Fatal("no request captured")
			}
			if got.Type != c.wantType {
				t.Errorf("type = %s, want %s", got.Type, c.wantType)
			}
			d, ok := got.Data.(ApiRequestDataWebApi)
			if !ok {
				t.Fatalf("data type = %T, want ApiRequestDataWebApi", got.Data)
			}
			if d.Method != c.method || d.Path != c.wantPath {
				t.Errorf("method/path = %s %s, want %s %s", d.Method, d.Path, c.method, c.wantPath)
			}
		})
	}
}

func TestWebApiLocalDispatch_QueryPreserved(t *testing.T) {
	srv, base := newTestApiServer(t)
	srv.SetPlayerReady(true)

	captured := drainOne(t, srv, nil, nil)
	resp, err := testClient.Get(base + "/web-api/me/playlists?limit=7&offset=42")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	var got ApiRequest
	select {
	case got = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("no request captured")
	}
	d := got.Data.(ApiRequestDataWebApi)
	if d.Query.Get("limit") != "7" || d.Query.Get("offset") != "42" {
		t.Errorf("query = %v, want limit=7 offset=42", d.Query)
	}
}

// the JSON envelope the UI decodes must round-trip through handleRequest
func TestWebApiPlaylistsResponse_JSONShape(t *testing.T) {
	t.Parallel()
	resp := webApiPlaylistsResponse{
		Href:   "https://api.spotify.com/v1/me/playlists",
		Items:  []webApiPlaylist{{Name: "X", URI: "spotify:playlist:1", ID: "1", Owner: webApiOwner{DisplayName: "o", Type: "user"}}},
		Limit:  50,
		Offset: 0,
		Total:  1,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var ui struct {
		Items []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Owner         struct {
				DisplayName string `json:"display_name"`
			} `json:"owner"`
			Images []struct {
				URL string `json:"url"`
			} `json:"images"`
			Tracks struct {
				Total int `json:"total"`
			} `json:"tracks"`
			Collaborative bool   `json:"collaborative"`
			URI           string `json:"uri"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(b, &ui); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(ui.Items) != 1 || ui.Items[0].ID != "1" || ui.Items[0].Owner.DisplayName != "o" || ui.Total != 1 {
		t.Errorf("round-trip = %+v", ui)
	}
}
