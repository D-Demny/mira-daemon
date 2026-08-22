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
		{"me", false},
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

func TestWebApiArtistsFromContributors(t *testing.T) {
	t.Parallel()
	type contribs = []struct {
		URI  string `json:"uri"`
		Name string `json:"name"`
	}
	out := webApiArtistsFromContributors(contribs{
		{URI: "spotify:artist:1", Name: "A"},
		{URI: "", Name: "B"},
		{URI: "spotify:artist:3", Name: ""},
	})
	if len(out) != 2 {
		t.Fatalf("artists = %+v, want 2 (empty name skipped)", out)
	}
	if out[0].(map[string]any)["name"] != "A" || out[0].(map[string]any)["uri"] != "spotify:artist:1" {
		t.Errorf("artists[0] = %+v", out[0])
	}
	if _, has := out[1].(map[string]any)["uri"]; has {
		t.Errorf("artists[1] = %+v, want no uri for empty input uri", out[1])
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
