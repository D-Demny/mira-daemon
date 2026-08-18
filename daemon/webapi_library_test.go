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

func TestHandleWebApiLocal_RecentlyPlayed(t *testing.T) {
	t.Parallel()
	p := &AppPlayer{}
	data, err := p.handleWebApiLocal(context.Background(), ApiRequestDataWebApi{
		Method: "GET",
		Path:   "me/player/recently-played",
	})
	if err != nil {
		t.Fatalf("handleWebApiLocal: %v", err)
	}
	m, ok := data.(map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want map", data)
	}
	items, ok := m["items"].([]any)
	if !ok || len(items) != 0 {
		t.Errorf("items = %#v, want empty list", m["items"])
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
