package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
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

// the live fetchPlaylist payload ships empty album objects for its tracks
// ("album":{"images":[],"name":""}); the cover must then come from the track's
// own visualIdentityTrait (bug23).
const playlistTracksAlbumFallbackFixture = `{
  "data": {
    "playlistV2": {
      "content": {
        "totalCount": 1,
        "items": [
          {
            "itemV2": {
              "_uri": "spotify:track:llp-1",
              "data": {
                "name": "In The End",
                "uri": "spotify:track:llp-1",
                "artists": {"items": [{"profile": {"name": "Linkin Park"}}]},
                "album": {"name": "", "images": []},
                "visualIdentityTrait": {
                  "images": [{"url": "https://i.scdn.com/hybrid.jpg", "width": 300, "height": 300}]
                }
              }
            }
          }
        ]
      }
    }
  }
}`

func TestMapPlaylistTracksPage_AlbumFallback(t *testing.T) {
	t.Parallel()
	items, _, err := mapPlaylistTracksPage([]byte(playlistTracksAlbumFallbackFixture), 0)
	if err != nil {
		t.Fatalf("mapPlaylistTracksPage: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	tm, _ := items[0].(map[string]any)["track"].(map[string]any)
	if tm["name"] != "In The End" || tm["uri"] != "spotify:track:llp-1" {
		t.Errorf("track = %+v", tm)
	}
	album, _ := tm["album"].(map[string]any)
	if album["name"] != "" {
		t.Errorf("album name = %v, want empty", album["name"])
	}
	imgs, _ := album["images"].([]webApiImage)
	if len(imgs) != 1 || imgs[0].URL != "https://i.scdn.com/hybrid.jpg" {
		t.Errorf("album images = %+v, want the track's own cover (bug23)", imgs)
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

// ── bug33: 2026 web-player image payload rotation ───────────────────────────
// The 2026 rotation ships recents artwork as ImageV2
// (visualIdentityTrait.squareCoverImage.image.data.sources with
// maxWidth/maxHeight instead of width/height) and album refs as
// albumOfTrack {name,uri,coverArt:{sources}} instead of an inline album. The
// fixtures below are trimmed captures of the live payloads served by the
// device (Build #82 era); the old normaliser only understood
// [{url,width,height}] and {sources|items:[...]}, so it dropped every cover
// and the UI rendered grey placeholders.

const recentsRotated2026Fixture = `{
  "data": {
    "lists": [
      {
        "__typename": "List",
        "formatListAttributes": [
          {
            "key": "revision_hash",
            "value": "0000000000000000ae5181bdee9ddd0a86b9d9fce566bc8e"
          },
          {
            "key": "filters",
            "value": "CgYIARICAQI="
          }
        ],
        "items": {
          "items": [
            {
              "addedAt": {
                "day": 24,
                "month": 8,
                "year": 2026
              },
              "entity": {
                "__typename": "EntityResponseWrapper",
                "_uri": "spotify:album:5AOu8nTJSHWiIDxPvcIDRF",
                "data": {
                  "__typename": "Entity",
                  "consumptionExperienceTrait": {
                    "__typename": "ConsumptionExperienceTrait",
                    "contentRatings": [
                      "CONTENT_RATING_EXPLICIT"
                    ],
                    "duration": {
                      "nanoSeconds": 0,
                      "seconds": 3305
                    }
                  },
                  "entityTypeTrait": {
                    "__typename": "EntityTypeTrait",
                    "type": "ENTITY_TYPE_ALBUM"
                  },
                  "identityTrait": {
                    "__typename": "IdentityTrait",
                    "contentHierarchyParent": null,
                    "contributors": {
                      "items": [
                        {
                          "name": "System Of A Down",
                          "uri": "spotify:artist:5eAWCfyUhZtHHtBdNk56l1"
                        }
                      ],
                      "totalCount": 1
                    },
                    "description": "",
                    "name": "Toxicity",
                    "type": "Album"
                  },
                  "typedEntity": {
                    "__typename": "AlbumResponseWrapper"
                  },
                  "uri": "spotify:album:5AOu8nTJSHWiIDxPvcIDRF",
                  "visualIdentityTrait": {
                    "__typename": "VisualIdentityTrait",
                    "squareCoverImage": {
                      "image": {
                        "data": {
                          "__typename": "ImageV2",
                          "sources": [
                            {
                              "imageFormat": "WEBP",
                              "maxHeight": 640,
                              "maxWidth": 640,
                              "url": "https://image-cdn-ak.spotifycdn.com/image/ab67616d000075a028afd76e21eed9788506c40e"
                            },
                            {
                              "imageFormat": "WEBP",
                              "maxHeight": 64,
                              "maxWidth": 64,
                              "url": "https://image-cdn-ak.spotifycdn.com/image/ab67616d000090d528afd76e21eed9788506c40e"
                            },
                            {
                              "imageFormat": "WEBP",
                              "maxHeight": 300,
                              "maxWidth": 300,
                              "url": "https://image-cdn-ak.spotifycdn.com/image/ab67616d0000ab8728afd76e21eed9788506c40e"
                            }
                          ]
                        }
                      },
                      "originalInstances": [
                        {
                          "flatFile": {
                            "cdnUrl": "https://i.scdn.co/image/ab67616d0000485128afd76e21eed9788506c40e"
                          },
                          "size": "IMAGE_SIZE_SMALL"
                        },
                        {
                          "flatFile": {
                            "cdnUrl": "https://i.scdn.co/image/ab67616d00001e0228afd76e21eed9788506c40e"
                          },
                          "size": "IMAGE_SIZE_DEFAULT"
                        },
                        {
                          "flatFile": {
                            "cdnUrl": "https://i.scdn.co/image/ab67616d0000b27328afd76e21eed9788506c40e"
                          },
                          "size": "IMAGE_SIZE_LARGE"
                        }
                      ]
                    }
                  }
                }
              },
              "formatListAttributes": [
                {
                  "key": "group_id_0",
                  "value": ""
                },
                {
                  "key": "children_group_id",
                  "value": "1"
                },
                {
                  "key": "group_metadata",
                  "value": "CAEiCQoFbXVzaWMQAQ=="
                },
                {
                  "key": "recent_type_played",
                  "value": ""
                },
                {
                  "key": "content_type_music",
                  "value": ""
                }
              ]
            },
            {
              "addedAt": {
                "day": 24,
                "month": 8,
                "year": 2026
              },
              "entity": {
                "__typename": "EntityResponseWrapper",
                "_uri": "spotify:track:4M9Fud77ZcrWt1vYDKIFwD",
                "data": {
                  "__typename": "Entity",
                  "consumptionExperienceTrait": {
                    "__typename": "ConsumptionExperienceTrait",
                    "contentRatings": [],
                    "duration": {
                      "nanoSeconds": 0,
                      "seconds": 176
                    }
                  },
                  "entityTypeTrait": {
                    "__typename": "EntityTypeTrait",
                    "type": "ENTITY_TYPE_TRACK"
                  },
                  "identityTrait": {
                    "__typename": "IdentityTrait",
                    "contentHierarchyParent": {
                      "__typename": "Entity",
                      "identityTrait": {
                        "__typename": "IdentityTrait",
                        "name": "Toxicity"
                      },
                      "uri": "spotify:album:5AOu8nTJSHWiIDxPvcIDRF"
                    },
                    "contributors": {
                      "items": [
                        {
                          "name": "System Of A Down",
                          "uri": "spotify:artist:5eAWCfyUhZtHHtBdNk56l1"
                        }
                      ],
                      "totalCount": 1
                    },
                    "description": "",
                    "name": "ATWA",
                    "type": "Song"
                  },
                  "typedEntity": {
                    "__typename": "TrackResponseWrapper"
                  },
                  "uri": "spotify:track:4M9Fud77ZcrWt1vYDKIFwD",
                  "visualIdentityTrait": {
                    "__typename": "VisualIdentityTrait",
                    "squareCoverImage": {
                      "image": {
                        "data": {
                          "__typename": "ImageV2",
                          "sources": [
                            {
                              "imageFormat": "WEBP",
                              "maxHeight": 640,
                              "maxWidth": 640,
                              "url": "https://image-cdn-ak.spotifycdn.com/image/ab67616d000075a028afd76e21eed9788506c40e"
                            },
                            {
                              "imageFormat": "WEBP",
                              "maxHeight": 64,
                              "maxWidth": 64,
                              "url": "https://image-cdn-ak.spotifycdn.com/image/ab67616d000090d528afd76e21eed9788506c40e"
                            },
                            {
                              "imageFormat": "WEBP",
                              "maxHeight": 300,
                              "maxWidth": 300,
                              "url": "https://image-cdn-ak.spotifycdn.com/image/ab67616d0000ab8728afd76e21eed9788506c40e"
                            }
                          ]
                        }
                      },
                      "originalInstances": [
                        {
                          "flatFile": {
                            "cdnUrl": "https://i.scdn.co/image/ab67616d0000485128afd76e21eed9788506c40e"
                          },
                          "size": "IMAGE_SIZE_SMALL"
                        },
                        {
                          "flatFile": {
                            "cdnUrl": "https://i.scdn.co/image/ab67616d00001e0228afd76e21eed9788506c40e"
                          },
                          "size": "IMAGE_SIZE_DEFAULT"
                        },
                        {
                          "flatFile": {
                            "cdnUrl": "https://i.scdn.co/image/ab67616d0000b27328afd76e21eed9788506c40e"
                          },
                          "size": "IMAGE_SIZE_LARGE"
                        }
                      ]
                    }
                  }
                }
              },
              "formatListAttributes": [
                {
                  "key": "group_id_1",
                  "value": ""
                },
                {
                  "key": "recent_type_played",
                  "value": ""
                },
                {
                  "key": "content_type_music",
                  "value": ""
                }
              ]
            }
          ],
          "pagingInfo": {
            "limit": 5,
            "nextOffset": 5,
            "offset": 0
          },
          "totalCount": 3152
        },
        "name": "Recents",
        "uri": "spotify:list:recents:page"
      }
    ]
  }
}`

func TestMapRecentlyPlayedPage_RotatedImageV2(t *testing.T) {
	t.Parallel()
	items, err := mapRecentlyPlayedPage([]byte(recentsRotated2026Fixture))
	if err != nil {
		t.Fatalf("mapRecentlyPlayedPage: %v", err)
	}
	// the 2026 list interleaves an album section entry before the first
	// track; only the spotify:track entry survives
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (album section entry skipped)", len(items))
	}
	entry, _ := items[0].(map[string]any)
	tr, _ := entry["track"].(map[string]any)
	if tr["name"] != "ATWA" || tr["uri"] != "spotify:track:4M9Fud77ZcrWt1vYDKIFwD" {
		t.Errorf("track = %+v", tr)
	}
	album, _ := tr["album"].(map[string]any)
	if album["name"] != "Toxicity" {
		t.Errorf("album.name = %v, want Toxicity", album["name"])
	}
	imgs, _ := album["images"].([]webApiImage)
	if len(imgs) != 3 {
		t.Fatalf("album.images = %+v, want 3 (ImageV2 sources)", imgs)
	}
	// sorted largest first, sizes from maxWidth/maxHeight
	if imgs[0].Width != 640 || imgs[1].Width != 300 || imgs[2].Width != 64 {
		t.Errorf("image order = %+v, want 640/300/64", imgs)
	}
	if !strings.HasPrefix(imgs[0].URL, "https://") {
		t.Errorf("first url = %q, want an https url", imgs[0].URL)
	}
}

const playlistTracksRotated2026Fixture = `{
  "data": {
    "playlistV2": {
      "__typename": "Playlist",
      "content": {
        "__typename": "PlaylistItemsPage",
        "items": [
          {
            "addedAt": {
              "isoString": "2026-05-21T08:22:05Z"
            },
            "addedBy": {
              "data": {
                "__typename": "User",
                "avatar": null,
                "name": "Daniel D",
                "uri": "spotify:user:1121858487",
                "username": "1121858487"
              }
            },
            "attributes": [],
            "itemV2": {
              "__typename": "TrackResponseWrapper",
              "data": {
                "__typename": "Track",
                "albumOfTrack": {
                  "artists": {
                    "items": [
                      {
                        "profile": {
                          "name": "Linkin Park"
                        },
                        "uri": "spotify:artist:6XyY86QOPPrYVGvF9ch6wz"
                      }
                    ]
                  },
                  "coverArt": {
                    "sources": [
                      {
                        "height": 300,
                        "url": "https://i.scdn.co/image/ab67616d00001e0259211e56a493ac4509457bab",
                        "width": 300
                      },
                      {
                        "height": 64,
                        "url": "https://i.scdn.co/image/ab67616d0000485159211e56a493ac4509457bab",
                        "width": 64
                      },
                      {
                        "height": 640,
                        "url": "https://i.scdn.co/image/ab67616d0000b27359211e56a493ac4509457bab",
                        "width": 640
                      }
                    ]
                  },
                  "name": "A Thousand Suns",
                  "uri": "spotify:album:5uvXx5ZQswNRFCdHR521YZ"
                },
                "artists": {
                  "items": [
                    {
                      "profile": {
                        "name": "Linkin Park"
                      },
                      "uri": "spotify:artist:6XyY86QOPPrYVGvF9ch6wz"
                    }
                  ]
                },
                "associationsV3": {
                  "audioAssociations": {
                    "__typename": "TrackAudioAssociationPage",
                    "items": []
                  },
                  "videoAssociations": {
                    "totalCount": 0
                  }
                },
                "contentRating": {
                  "label": "NONE"
                },
                "discNumber": 1,
                "trackDuration": {
                  "totalMilliseconds": 296560
                },
                "mediaType": "AUDIO",
                "name": "Iridescent",
                "playability": {
                  "playable": true,
                  "reason": "PLAYABLE"
                },
                "playcount": "168237903",
                "trackNumber": 12,
                "uri": "spotify:track:69ZEgPX0hxWXJIqkTlYz41"
              }
            },
            "itemV3": {
              "__typename": "EntityResponseWrapper",
              "data": {
                "__typename": "Entity",
                "consumptionExperienceTrait": {
                  "__typename": "ConsumptionExperienceTrait",
                  "contentRatings": [],
                  "duration": {
                    "nanoSeconds": 0,
                    "seconds": 296
                  }
                },
                "identityTrait": {
                  "__typename": "IdentityTrait",
                  "contentHierarchyParent": {
                    "__typename": "Entity",
                    "identityTrait": {
                      "__typename": "IdentityTrait",
                      "name": "A Thousand Suns"
                    },
                    "uri": "spotify:album:5uvXx5ZQswNRFCdHR521YZ"
                  },
                  "contributors": {
                    "items": [
                      {
                        "name": "Linkin Park",
                        "uri": "spotify:artist:6XyY86QOPPrYVGvF9ch6wz"
                      }
                    ],
                    "totalCount": 1
                  },
                  "description": "",
                  "name": "Iridescent",
                  "type": "Song"
                },
                "uri": "spotify:track:69ZEgPX0hxWXJIqkTlYz41",
                "visualIdentityTrait": {
                  "__typename": "VisualIdentityTrait",
                  "squareCoverImage": {
                    "image": {
                      "data": {
                        "__typename": "ImageV2",
                        "sources": [
                          {
                            "imageFormat": "WEBP",
                            "maxHeight": 640,
                            "maxWidth": 640,
                            "url": "https://image-cdn-fa.spotifycdn.com/image/ab67616d000075a059211e56a493ac4509457bab"
                          },
                          {
                            "imageFormat": "WEBP",
                            "maxHeight": 64,
                            "maxWidth": 64,
                            "url": "https://image-cdn-fa.spotifycdn.com/image/ab67616d000090d559211e56a493ac4509457bab"
                          },
                          {
                            "imageFormat": "WEBP",
                            "maxHeight": 300,
                            "maxWidth": 300,
                            "url": "https://image-cdn-fa.spotifycdn.com/image/ab67616d0000ab8759211e56a493ac4509457bab"
                          }
                        ]
                      }
                    }
                  }
                }
              }
            },
            "uid": "0e3acaf97803be1a"
          }
        ],
        "pagingInfo": {
          "limit": 3,
          "offset": 0
        },
        "totalCount": 13
      },
      "attributes": [],
      "basePermission": "VIEWER",
      "currentUserCapabilities": {
        "canAdministratePermissions": true,
        "canCancelMembership": false,
        "canEditItems": true,
        "canView": true
      },
      "description": "",
      "followers": 0,
      "following": true,
      "format": "",
      "images": {
        "items": [
          {
            "sources": [
              {
                "height": 640,
                "url": "https://mosaic.scdn.co/640/ab67616d00001e0259211e56a493ac4509457babab67616d00001e026e996745f2c7b8036abef213ab67616d00001e028b5b6fa1326d996181e71dd7ab67616d00001e02987fb4c5ec8790e9f637a4a4",
                "width": 640
              },
              {
                "height": 300,
                "url": "https://mosaic.scdn.co/300/ab67616d00001e0259211e56a493ac4509457babab67616d00001e026e996745f2c7b8036abef213ab67616d00001e028b5b6fa1326d996181e71dd7ab67616d00001e02987fb4c5ec8790e9f637a4a4",
                "width": 300
              },
              {
                "height": 60,
                "url": "https://mosaic.scdn.co/60/ab67616d00001e0259211e56a493ac4509457babab67616d00001e026e996745f2c7b8036abef213ab67616d00001e028b5b6fa1326d996181e71dd7ab67616d00001e02987fb4c5ec8790e9f637a4a4",
                "width": 60
              }
            ]
          }
        ]
      },
      "members": {
        "items": [
          {
            "isOwner": true,
            "permissionLevel": "CONTRIBUTOR",
            "user": {
              "data": {
                "__typename": "User",
                "avatar": null,
                "name": "Daniel D",
                "uri": "spotify:user:1121858487",
                "username": "1121858487"
              }
            }
          }
        ],
        "totalCount": 1
      },
      "name": "Linkin Park",
      "ownerV2": {
        "data": {
          "__typename": "User",
          "avatar": null,
          "name": "Daniel D",
          "uri": "spotify:user:1121858487",
          "username": "1121858487"
        }
      },
      "revisionId": "AAAAD0BvW40warchjREnKerxKidNWppZ",
      "sharingInfo": {
        "shareId": "c0Y1TNNHS_aHsucl9eJduw",
        "shareUrl": "https://open.spotify.com/playlist/0Qb9Spsm26hjTg8FjJgCuN?si=c0Y1TNNHS_aHsucl9eJduw"
      },
      "uri": "spotify:playlist:0Qb9Spsm26hjTg8FjJgCuN",
      "visualIdentity": {
        "squareCoverImage": {
          "__typename": "VisualIdentityImage",
          "extractedColorSet": {
            "encoreBaseSetTextColor": {
              "alpha": 255,
              "blue": 187,
              "green": 187,
              "red": 187
            },
            "highContrast": {
              "backgroundBase": {
                "alpha": 255,
                "blue": 83,
                "green": 83,
                "red": 83
              },
              "backgroundTintedBase": {
                "alpha": 255,
                "blue": 51,
                "green": 51,
                "red": 51
              },
              "textBase": {
                "alpha": 255,
                "blue": 255,
                "green": 255,
                "red": 255
              },
              "textBrightAccent": {
                "alpha": 255,
                "blue": 255,
                "green": 255,
                "red": 255
              },
              "textSubdued": {
                "alpha": 255,
                "blue": 205,
                "green": 205,
                "red": 205
              }
            },
            "higherContrast": {
              "backgroundBase": {
                "alpha": 255,
                "blue": 53,
                "green": 53,
                "red": 53
              },
              "backgroundTintedBase": {
                "alpha": 255,
                "blue": 86,
                "green": 86,
                "red": 86
              },
              "textBase": {
                "alpha": 255,
                "blue": 255,
                "green": 255,
                "red": 255
              },
              "textBrightAccent": {
                "alpha": 255,
                "blue": 96,
                "green": 215,
                "red": 30
              },
              "textSubdued": {
                "alpha": 255,
                "blue": 205,
                "green": 205,
                "red": 205
              }
            },
            "minContrast": {
              "backgroundBase": {
                "alpha": 255,
                "blue": 83,
                "green": 83,
                "red": 83
              },
              "backgroundTintedBase": {
                "alpha": 255,
                "blue": 51,
                "green": 51,
                "red": 51
              },
              "textBase": {
                "alpha": 255,
                "blue": 255,
                "green": 255,
                "red": 255
              },
              "textBrightAccent": {
                "alpha": 255,
                "blue": 255,
                "green": 255,
                "red": 255
              },
              "textSubdued": {
                "alpha": 255,
                "blue": 255,
                "green": 255,
                "red": 255
              }
            }
          }
        }
      }
    }
  }
}`

func TestMapPlaylistTracksPage_RotatedAlbumOfTrack(t *testing.T) {
	t.Parallel()
	items, total, err := mapPlaylistTracksPage([]byte(playlistTracksRotated2026Fixture), 0)
	if err != nil {
		t.Fatalf("mapPlaylistTracksPage: %v", err)
	}
	if total != 13 {
		t.Errorf("total = %d, want 13", total)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	entry, _ := items[0].(map[string]any)
	tr, _ := entry["track"].(map[string]any)
	if tr["name"] != "Iridescent" || tr["uri"] != "spotify:track:69ZEgPX0hxWXJIqkTlYz41" {
		t.Errorf("track = %+v", tr)
	}
	album, _ := tr["album"].(map[string]any)
	// the 2026 payload ships no inline album; name + cover come from albumOfTrack
	if album["name"] != "A Thousand Suns" {
		t.Errorf("album.name = %v, want A Thousand Suns (from albumOfTrack)", album["name"])
	}
	imgs, _ := album["images"].([]webApiImage)
	if len(imgs) != 3 {
		t.Fatalf("album.images = %+v, want 3 (albumOfTrack.coverArt.sources)", imgs)
	}
	if imgs[0].Width != 640 || imgs[1].Width != 300 || imgs[2].Width != 64 {
		t.Errorf("image order = %+v, want 640/300/64", imgs)
	}
}

const savedTracksRotated2026Fixture = `{
  "data": {
    "me": {
      "library": {
        "tracks": {
          "__typename": "UserLibraryTrackPage",
          "items": [
            {
              "__typename": "UserLibraryTrackResponse",
              "addedAt": {
                "isoString": "2026-08-12T13:16:30Z"
              },
              "track": {
                "_uri": "spotify:track:0QepvU0N2fC2B5uIPafO1q",
                "data": {
                  "__typename": "Track",
                  "albumOfTrack": {
                    "artists": {
                      "items": [
                        {
                          "profile": {
                            "name": "Limp Bizkit"
                          },
                          "uri": "spotify:artist:165ZgPlLkK7bf5bDoFc6Sb"
                        }
                      ]
                    },
                    "coverArt": {
                      "sources": [
                        {
                          "height": 300,
                          "url": "https://i.scdn.co/image/ab67616d00001e024a31b146c7cf07705d912efe",
                          "width": 300
                        },
                        {
                          "height": 64,
                          "url": "https://i.scdn.co/image/ab67616d000048514a31b146c7cf07705d912efe",
                          "width": 64
                        },
                        {
                          "height": 640,
                          "url": "https://i.scdn.co/image/ab67616d0000b2734a31b146c7cf07705d912efe",
                          "width": 640
                        }
                      ]
                    },
                    "name": "Chocolate Starfish And The Hot Dog Flavored Water",
                    "uri": "spotify:album:5mi7FKaWE5CtcOjdyxScA7"
                  },
                  "artists": {
                    "items": [
                      {
                        "profile": {
                          "name": "Limp Bizkit"
                        },
                        "uri": "spotify:artist:165ZgPlLkK7bf5bDoFc6Sb"
                      }
                    ]
                  },
                  "associationsV3": {
                    "audioAssociations": {
                      "totalCount": 0
                    },
                    "videoAssociations": {
                      "totalCount": 0
                    }
                  },
                  "contentRating": {
                    "label": "EXPLICIT"
                  },
                  "discNumber": 1,
                  "duration": {
                    "totalMilliseconds": 264040
                  },
                  "mediaType": "AUDIO",
                  "name": "Livin' It Up",
                  "playability": {
                    "playable": true
                  },
                  "trackNumber": 7
                }
              }
            }
          ],
          "pagingInfo": {
            "limit": 3,
            "offset": 0
          },
          "totalCount": 501
        }
      }
    }
  }
}`

func TestMapSavedTracksPage_RotatedAlbumOfTrack(t *testing.T) {
	t.Parallel()
	items, total, err := mapSavedTracksPage([]byte(savedTracksRotated2026Fixture), 0)
	if err != nil {
		t.Fatalf("mapSavedTracksPage: %v", err)
	}
	if total != 501 {
		t.Errorf("total = %d, want 501", total)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	entry, _ := items[0].(map[string]any)
	tr, _ := entry["track"].(map[string]any)
	if tr["name"] != "Livin' It Up" {
		t.Errorf("track = %+v", tr)
	}
	album, _ := tr["album"].(map[string]any)
	if album["name"] != "Chocolate Starfish And The Hot Dog Flavored Water" {
		t.Errorf("album.name = %v, want the albumOfTrack name", album["name"])
	}
	imgs, _ := album["images"].([]webApiImage)
	if len(imgs) != 3 || imgs[0].Width != 640 {
		t.Errorf("album.images = %+v, want 3 images, 640 first", imgs)
	}
}

func TestWebApiImagesFromAny_RotatedShapes(t *testing.T) {
	t.Parallel()
	// ImageV2 (recents, 2026): squareCoverImage.image.data.sources with
	// maxWidth/maxHeight, unsorted in the payload
	imageV2 := map[string]any{
		"squareCoverImage": map[string]any{
			"image": map[string]any{
				"data": map[string]any{
					"sources": []any{
						map[string]any{"url": "https://x/640.jpg", "maxWidth": 640, "maxHeight": 640},
						map[string]any{"url": "https://x/64.jpg", "maxWidth": 64, "maxHeight": 64},
						map[string]any{"url": "https://x/300.jpg", "maxWidth": 300, "maxHeight": 300},
					},
				},
			},
		},
	}
	imgs := webApiImagesFromAny(imageV2)
	if len(imgs) != 3 || imgs[0].Width != 640 || imgs[1].Width != 300 || imgs[2].Width != 64 {
		t.Errorf("ImageV2 = %+v, want 3 images largest-first", imgs)
	}
	if imgs[0].Height != 640 {
		t.Errorf("ImageV2 height = %d, want maxHeight mapping", imgs[0].Height)
	}

	// coverArt wrapper (albumOfTrack, 2026)
	coverArt := map[string]any{
		"coverArt": map[string]any{
			"sources": []any{
				map[string]any{"url": "https://x/c300.jpg", "width": 300, "height": 300},
				map[string]any{"url": "https://x/c640.jpg", "width": 640, "height": 640},
			},
		},
	}
	imgs2 := webApiImagesFromAny(coverArt)
	if len(imgs2) != 2 || imgs2[0].Width != 640 || imgs2[1].Width != 300 {
		t.Errorf("coverArt = %+v, want 2 images largest-first", imgs2)
	}

	// bare array using maxWidth spelling
	bareMax := []any{
		map[string]any{"url": "https://x/m300.jpg", "maxWidth": 300, "maxHeight": 300},
		map[string]any{"url": "https://x/m640.jpg", "maxWidth": 640, "maxHeight": 640},
	}
	imgs3 := webApiImagesFromAny(bareMax)
	if len(imgs3) != 2 || imgs3[0].Width != 640 {
		t.Errorf("bare maxWidth array = %+v, want 2 images 640 first", imgs3)
	}

	// single source object (no wrapper at all)
	single := map[string]any{"url": "https://x/s.jpg", "maxWidth": 300, "maxHeight": 300}
	imgs4 := webApiImagesFromAny(single)
	if len(imgs4) != 1 || imgs4[0].Width != 300 {
		t.Errorf("single source = %+v, want 1 image", imgs4)
	}

	// entries without a url are skipped
	noURL := map[string]any{
		"sources": []any{
			map[string]any{"maxWidth": 640, "maxHeight": 640},
			map[string]any{"url": "https://x/kept.jpg", "width": 300, "height": 300},
		},
	}
	imgs5 := webApiImagesFromAny(noURL)
	if len(imgs5) != 1 || imgs5[0].URL != "https://x/kept.jpg" {
		t.Errorf("url-less entry = %+v, want only the url-bearing source", imgs5)
	}

	// backwards compat: the pre-2026 shapes keep working
	oldWrapped := map[string]any{
		"sources": []any{
			map[string]any{"url": "https://x/old640.jpg", "width": 640, "height": 640},
		},
	}
	imgs6 := webApiImagesFromAny(oldWrapped)
	if len(imgs6) != 1 || imgs6[0].Width != 640 {
		t.Errorf("old {sources} shape = %+v, want unchanged", imgs6)
	}
	oldBare := []any{map[string]any{"url": "https://x/old.jpg", "width": 60, "height": 60}}
	imgs7 := webApiImagesFromAny(oldBare)
	if len(imgs7) != 1 || imgs7[0].Width != 60 {
		t.Errorf("old bare shape = %+v, want unchanged", imgs7)
	}

	// degenerate inputs
	if imgs := webApiImagesFromAny(map[string]any{"sources": []any{}}); imgs != nil {
		t.Errorf("empty sources = %+v, want nil", imgs)
	}
	if imgs := webApiImagesFromAny(map[string]any{"squareCoverImage": map[string]any{}}); imgs != nil {
		t.Errorf("empty ImageV2 = %+v, want nil", imgs)
	}
}

func TestWebApiTrackAlbum_RotatedShapes(t *testing.T) {
	t.Parallel()
	// 2026 playlist/library track: no inline album, only albumOfTrack
	item2026 := map[string]any{
		"albumOfTrack": map[string]any{
			"name": "A Thousand Suns",
			"uri":  "spotify:album:5uvXx5ZQswNRFCdHR521YZ",
			"coverArt": map[string]any{
				"sources": []any{
					map[string]any{"url": "https://x/p300.jpg", "width": 300, "height": 300},
					map[string]any{"url": "https://x/p640.jpg", "width": 640, "height": 640},
				},
			},
		},
	}
	alb := webApiTrackAlbum(item2026)
	if alb["name"] != "A Thousand Suns" {
		t.Errorf("album.name = %v, want the albumOfTrack name", alb["name"])
	}
	imgs, _ := alb["images"].([]webApiImage)
	if len(imgs) != 2 || imgs[0].Width != 640 {
		t.Errorf("album.images = %+v, want 2 images 640 first", imgs)
	}

	// inline album without images falls back to albumOfTrack's cover
	itemMixed := map[string]any{
		"album":        map[string]any{"name": "Inline Album", "images": []any{}},
		"albumOfTrack": item2026["albumOfTrack"],
	}
	alb2 := webApiTrackAlbum(itemMixed)
	if alb2["name"] != "Inline Album" {
		t.Errorf("album.name = %v, want the inline album name to win", alb2["name"])
	}
	imgs2, _ := alb2["images"].([]webApiImage)
	if len(imgs2) != 2 || imgs2[0].Width != 640 {
		t.Errorf("album.images = %+v, want the albumOfTrack cover", imgs2)
	}

	// nothing but an ImageV2 visualIdentityTrait (recents-style fallback)
	itemTrait := map[string]any{
		"visualIdentityTrait": map[string]any{
			"squareCoverImage": map[string]any{
				"image": map[string]any{
					"data": map[string]any{
						"sources": []any{
							map[string]any{"url": "https://x/t640.jpg", "maxWidth": 640, "maxHeight": 640},
						},
					},
				},
			},
		},
	}
	alb3 := webApiTrackAlbum(itemTrait)
	if alb3["name"] != "" {
		t.Errorf("album.name = %v, want empty", alb3["name"])
	}
	imgs3, _ := alb3["images"].([]webApiImage)
	if len(imgs3) != 1 || imgs3[0].Width != 640 {
		t.Errorf("album.images = %+v, want the ImageV2 cover", imgs3)
	}
}
