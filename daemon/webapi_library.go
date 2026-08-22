package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Spotify rate-limits tokens issued under our (Android) client_id on the public
// Web API (api.spotify.com) with 429 for essentially every request. The data
// the UI needs is instead served from Pathfinder (Spotify's internal GraphQL
// API), authenticated with the Login5 token that already powers playback.

const (
	playlistCountCacheTTL   = 30 * time.Minute
	playlistCountFailTTL    = 2 * time.Minute
	playlistCountWorkers    = 8
	playlistCountPhaseLimit = 25 * time.Second
)

type playlistCountEntry struct {
	total int
	at    time.Time
	ok    bool
}

type playlistCountTarget struct {
	idx int
	uri string
}

// Web API response shapes (mirrors api.spotify.com so the UI needs no changes)

type webApiImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type webApiOwner struct {
	DisplayName string `json:"display_name"`
	ID          string `json:"id,omitempty"`
	Type        string `json:"type"`
}

type webApiPlaylist struct {
	Collaborative bool        `json:"collaborative"`
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	URI           string      `json:"uri"`
	Owner         webApiOwner `json:"owner"`
	Tracks        struct {
		Total int `json:"total"`
	} `json:"tracks"`
	Images []webApiImage `json:"images,omitempty"`
}

type webApiPlaylistsResponse struct {
	Href     string           `json:"href"`
	Items    []webApiPlaylist `json:"items"`
	Limit    int              `json:"limit"`
	Next     any              `json:"next"`
	Offset   int              `json:"offset"`
	Previous any              `json:"previous"`
	Total    int              `json:"total"`
}

// isLocalWebApiPath reports whether a /web-api/ path is served from Pathfinder
// instead of being proxied to the (rate-limited) public Web API.
func isLocalWebApiPath(path string) bool {
	switch path {
	case "me/playlists", "me/player/recently-played":
		return true
	default:
		return playlistTracksPath(path) != ""
	}
}

// playlistTracksPath returns the playlist id for playlists/<id>/tracks paths
// and "" for anything else.
func playlistTracksPath(path string) string {
	if !strings.HasPrefix(path, "playlists/") || !strings.HasSuffix(path, "/tracks") {
		return ""
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, "playlists/"), "/tracks")
	if id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

// handleWebApiLocal routes the locally-served Web API paths.
func (p *AppPlayer) handleWebApiLocal(ctx context.Context, d ApiRequestDataWebApi) (any, error) {
	switch {
	case d.Path == "me/playlists":
		return p.webApiPlaylists(ctx, d.Query)
	case d.Path == "me/player/recently-played":
		return p.webApiRecentlyPlayed(ctx, d.Query)
	default:
		if id := playlistTracksPath(d.Path); id != "" {
			return p.webApiPlaylistTracks(ctx, id, d.Query)
		}
		return nil, ErrNotFound
	}
}

// webApiRecentlyPlayed serves GET me/player/recently-played from the web
// player's "Recents" list (spotify:list:recents:page), mapped to the Web API
// response shape. Only track entries are kept, like the public endpoint.
func (p *AppPlayer) webApiRecentlyPlayed(ctx context.Context, q url.Values) (any, error) {
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	body := p.pfBody("recents", map[string]any{
		"uris":   []string{"spotify:list:recents:page"},
		"offset": 0,
		"limit":  limit,
	})
	data, err := p.pathfinderQueryEx(ctx, body, false)
	if err != nil {
		p.app.log.Warnf("web-api: me/player/recently-played: %v", err)
		return nil, err
	}
	items, err := mapRecentlyPlayedPage(data)
	if err != nil {
		p.app.log.Warnf("web-api: me/player/recently-played: bad pathfinder payload: %v", err)
		return nil, err
	}
	return map[string]any{
		"items":   items,
		"next":    nil,
		"cursors": map[string]any{},
		"limit":   limit,
		"href":    "https://api.spotify.com/v1/me/player/recently-played",
	}, nil
}

// webApiPlaylistTracks serves GET playlists/<id>/tracks from the fetchPlaylist
// Pathfinder query (the same persisted query the voice catalog pages with),
// mapped to the Web API response shape.
func (p *AppPlayer) webApiPlaylistTracks(ctx context.Context, playlistID string, q url.Values) (any, error) {
	limit, offset := parseWebApiPaging(q)
	body := p.pfBody("fetchPlaylist", map[string]any{
		"uri":                       "spotify:playlist:" + playlistID,
		"offset":                    offset,
		"limit":                     limit,
		"enableWatchFeedEntrypoint": false,
	})
	data, err := p.pathfinderQueryEx(ctx, body, false)
	if err != nil {
		p.app.log.Warnf("web-api: playlists/%s/tracks: %v", playlistID, err)
		return nil, err
	}
	items, total, err := mapPlaylistTracksPage(data)
	if err != nil {
		p.app.log.Warnf("web-api: playlists/%s/tracks: bad pathfinder payload: %v", playlistID, err)
		return nil, err
	}
	next := any(nil)
	if offset+limit < total {
		next = fmt.Sprintf("https://api.spotify.com/v1/playlists/%s/tracks?limit=%d&offset=%d",
			playlistID, limit, offset+limit)
	}
	return map[string]any{
		"href":   fmt.Sprintf("https://api.spotify.com/v1/playlists/%s/tracks", playlistID),
		"items":  items,
		"limit":  limit,
		"offset": offset,
		"next":   next,
		"total":  total,
	}, nil
}

// the "Recents" list payload: data.lists[] of the generic web-player List
// type; every entry carries the entity traits of the played item
type recentsPayload struct {
	Data struct {
		Lists []struct {
			Typename string `json:"__typename"`
			Items    struct {
				TotalCount int `json:"totalCount"`
				Items      []struct {
					AddedAt string `json:"addedAt"`
					Entity  struct {
						URI  string `json:"_uri"`
						Data struct {
							IdentityTrait struct {
								Name         string `json:"name"`
								Contributors []struct {
									URI  string `json:"uri"`
									Name string `json:"name"`
								} `json:"contributors"`
								ContentHierarchyParent *struct {
									URI           string `json:"uri"`
									IdentityTrait struct {
										Name string `json:"name"`
									} `json:"identityTrait"`
								} `json:"contentHierarchyParent"`
							} `json:"identityTrait"`
							VisualIdentityTrait struct {
								Images any `json:"images"` // {sources:[...]} or a bare array
							} `json:"visualIdentityTrait"`
						} `json:"data"`
					} `json:"entity"`
				} `json:"items"`
			} `json:"items"`
		} `json:"lists"`
	} `json:"data"`
}

func mapRecentlyPlayedPage(data []byte) ([]any, error) {
	var r recentsPayload
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	out := make([]any, 0, len(r.Data.Lists))
	for _, list := range r.Data.Lists {
		if list.Typename != "List" {
			continue
		}
		for _, it := range list.Items.Items {
			uri := it.Entity.URI
			if !strings.HasPrefix(uri, "spotify:track:") {
				continue
			}
			id := strings.TrimPrefix(uri, "spotify:track:")
			ident := it.Entity.Data.IdentityTrait
			album := map[string]any{"name": "", "images": []any{}}
			if parent := ident.ContentHierarchyParent; parent != nil {
				album["name"] = parent.IdentityTrait.Name
			}
			if imgs := webApiImagesFromAny(it.Entity.Data.VisualIdentityTrait.Images); imgs != nil {
				album["images"] = imgs
			}
			out = append(out, map[string]any{
				"track": map[string]any{
					"id":      id,
					"name":    ident.Name,
					"uri":     uri,
					"artists": webApiArtistsFromContributors(ident.Contributors),
					"album":   album,
				},
				"played_at": it.AddedAt,
			})
		}
	}
	return out, nil
}

// the fetchPlaylist content payload (playlistV2.content)
type playlistTracksPayload struct {
	Data struct {
		PlaylistV2 struct {
			Content struct {
				TotalCount int `json:"totalCount"`
				Items      []struct {
					ItemV2 struct {
						URI  string `json:"_uri"`
						Data struct {
							Name    string `json:"name"`
							URI     string `json:"uri"`
							Artists struct {
								Items []struct {
									Profile struct {
										Name string `json:"name"`
									} `json:"profile"`
								} `json:"items"`
							} `json:"artists"`
							Album      any `json:"album"` // may be absent in the payload
							DurationMs int `json:"durationMs"`
						} `json:"data"`
					} `json:"itemV2"`
				} `json:"items"`
			} `json:"content"`
		} `json:"playlistV2"`
	} `json:"data"`
}

func mapPlaylistTracksPage(data []byte) ([]any, int, error) {
	var r playlistTracksPayload
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, 0, err
	}
	c := r.Data.PlaylistV2.Content
	out := make([]any, 0, len(c.Items))
	for _, it := range c.Items {
		d := it.ItemV2.Data
		if d.Name == "" {
			continue
		}
		uri := d.URI
		if uri == "" {
			uri = it.ItemV2.URI
		}
		if uri == "" {
			continue
		}
		id := uri[strings.LastIndex(uri, ":")+1:]
		artists := make([]any, 0, len(d.Artists.Items))
		for _, a := range d.Artists.Items {
			if a.Profile.Name == "" {
				continue
			}
			artists = append(artists, map[string]any{"name": a.Profile.Name})
		}
		album := map[string]any{"name": "", "images": []any{}}
		if b, err := json.Marshal(d.Album); err == nil {
			var obj struct {
				Name   string `json:"name"`
				Images any    `json:"images"`
			}
			if json.Unmarshal(b, &obj) == nil && (obj.Name != "" || obj.Images != nil) {
				album["name"] = obj.Name
				if imgs := webApiImagesFromAny(obj.Images); imgs != nil {
					album["images"] = imgs
				}
			}
		}
		track := map[string]any{
			"id":      id,
			"name":    d.Name,
			"uri":     uri,
			"artists": artists,
			"album":   album,
		}
		if d.DurationMs > 0 {
			track["duration_ms"] = d.DurationMs
		}
		out = append(out, map[string]any{
			"is_local": false,
			"track":    track,
		})
	}
	return out, c.TotalCount, nil
}

// webApiImagesFromAny normalizes the image payloads the web-player queries
// ship ({sources:[{url,width,height}]} or a bare array) to webApiImage.
func webApiImagesFromAny(v any) []webApiImage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var wrapped struct {
		Sources []webApiImage `json:"sources"`
	}
	if err := json.Unmarshal(b, &wrapped); err == nil && len(wrapped.Sources) > 0 {
		return wrapped.Sources
	}
	var bare []webApiImage
	if err := json.Unmarshal(b, &bare); err == nil && len(bare) > 0 {
		return bare
	}
	return nil
}

// webApiArtistsFromContributors maps the recents identityTrait contributors
// to the Web API artist objects.
func webApiArtistsFromContributors(cs []struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}) []any {
	out := make([]any, 0, len(cs))
	for _, c := range cs {
		if c.Name == "" {
			continue
		}
		a := map[string]any{"name": c.Name}
		if c.URI != "" {
			a["uri"] = c.URI
		}
		out = append(out, a)
	}
	return out
}

// webApiPlaylists serves GET me/playlists from the libraryV3 Pathfinder query,
// mapped to the Web API response shape.
func (p *AppPlayer) webApiPlaylists(ctx context.Context, q url.Values) (any, error) {
	limit, offset := parseWebApiPaging(q)

	body := p.pfBody("libraryV3", map[string]any{
		"filters":                      []string{"Playlists"},
		"order":                        nil,
		"textFilter":                   "",
		"features":                     []string{"LIKED_SONGS", "YOUR_EPISODES_V2", "PRERELEASES", "EVENTS"},
		"limit":                        limit,
		"offset":                       offset,
		"flatten":                      true,
		"expandedFolders":              []string{},
		"folderUri":                    nil,
		"includeFoldersWhenFlattening": false,
	})
	data, err := p.pathfinderQueryEx(ctx, body, false)
	if err != nil {
		p.app.log.Warnf("web-api: me/playlists: %v", err)
		return nil, err
	}

	resp, targets, err := mapLibraryV3Page(data, limit, offset)
	if err != nil {
		p.app.log.Warnf("web-api: me/playlists: bad pathfinder payload: %v", err)
		return nil, err
	}

	if len(targets) > 0 {
		p.fillPlaylistCounts(ctx, targets, &resp)
	}
	return resp, nil
}

// parseWebApiPaging clamps the limit/offset query params to Web API bounds.
func parseWebApiPaging(q url.Values) (limit, offset int) {
	limit, _ = strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	offset, _ = strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	if offset > 100000 {
		offset = 100000
	}
	return limit, offset
}

// mapLibraryV3Page maps a raw libraryV3 Pathfinder payload to the Web API
// response shape; playlists without an inline count are returned as targets
// for follow-up count queries.
func mapLibraryV3Page(data []byte, limit, offset int) (webApiPlaylistsResponse, []playlistCountTarget, error) {
	var r struct {
		Data struct {
			Me struct {
				LibraryV3 struct {
					TotalCount int `json:"totalCount"`
					Items      []struct {
						Item struct {
							Data struct {
								Typename string `json:"__typename"`
								Uri      string `json:"uri"`
								Name     string `json:"name"`
								Count    int    `json:"count"`
								Images   struct {
									Items []struct {
										Sources []struct {
											URL    string `json:"url"`
											Width  int    `json:"width"`
											Height int    `json:"height"`
										} `json:"sources"`
									} `json:"items"`
								} `json:"images"`
								Image struct {
									Sources []struct {
										URL    string `json:"url"`
										Width  int    `json:"width"`
										Height int    `json:"height"`
									} `json:"sources"`
								} `json:"image"`
								OwnerV2 struct {
									Data struct {
										Name     string `json:"name"`
										ID       string `json:"id"`
										Username string `json:"username"`
									} `json:"data"`
								} `json:"ownerV2"`
							} `json:"data"`
							URI string `json:"_uri"`
						} `json:"item"`
					} `json:"items"`
				} `json:"libraryV3"`
			} `json:"me"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return webApiPlaylistsResponse{}, nil, err
	}

	lib := r.Data.Me.LibraryV3
	resp := webApiPlaylistsResponse{
		Href:   "https://api.spotify.com/v1/me/playlists",
		Items:  make([]webApiPlaylist, 0, len(lib.Items)),
		Limit:  limit,
		Offset: offset,
		Total:  lib.TotalCount,
	}

	var targets []playlistCountTarget
	for _, it := range lib.Items {
		d := it.Item.Data
		uri := d.Uri
		if uri == "" {
			uri = it.Item.URI
		}
		if d.Name == "" || uri == "" {
			continue
		}
		pl := webApiPlaylist{Name: d.Name, URI: uri, Collaborative: false}
		if strings.HasPrefix(uri, "spotify:playlist:") {
			pl.ID = strings.TrimPrefix(uri, "spotify:playlist:")
		} else {
			pl.ID = uri // e.g. spotify:collection:tracks (Liked Songs)
		}
		ownerName := d.OwnerV2.Data.Name
		ownerID := d.OwnerV2.Data.ID
		if ownerName == "" {
			ownerName = "Spotify"
		}
		pl.Owner = webApiOwner{DisplayName: ownerName, ID: ownerID, Type: "user"}

		var srcs []struct {
			URL    string `json:"url"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		}
		if len(d.Images.Items) > 0 {
			srcs = d.Images.Items[0].Sources
		} else if len(d.Image.Sources) > 0 {
			srcs = d.Image.Sources
		}
		sort.Slice(srcs, func(i, j int) bool { return srcs[i].Width > srcs[j].Width })
		for _, s := range srcs {
			pl.Images = append(pl.Images, webApiImage{URL: s.URL, Width: s.Width, Height: s.Height})
		}

		if d.Count > 0 {
			pl.Tracks.Total = d.Count // pseudo-playlists (Liked Songs) carry their count
		} else if strings.HasPrefix(uri, "spotify:playlist:") {
			targets = append(targets, playlistCountTarget{idx: len(resp.Items), uri: uri})
		}
		resp.Items = append(resp.Items, pl)
	}
	return resp, targets, nil
}

// fillPlaylistCounts resolves track counts for the given playlists with bounded
// concurrency and an overall time budget; misses keep the cached value or 0.
func (p *AppPlayer) fillPlaylistCounts(ctx context.Context, targets []playlistCountTarget, resp *webApiPlaylistsResponse) {
	cctx, cancel := context.WithTimeout(ctx, playlistCountPhaseLimit)
	defer cancel()

	type result struct {
		idx   int
		total int
		ok    bool
	}
	resCh := make(chan result, len(targets))
	sem := make(chan struct{}, playlistCountWorkers)
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t playlistCountTarget) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-cctx.Done():
				resCh <- result{idx: t.idx}
				return
			}
			defer func() { <-sem }()
			if cctx.Err() != nil {
				resCh <- result{idx: t.idx}
				return
			}
			total, ok := p.fetchPlaylistCount(cctx, t.uri)
			resCh <- result{idx: t.idx, total: total, ok: ok}
		}(t)
	}
	wg.Wait()
	close(resCh)
	for r := range resCh {
		if r.ok {
			resp.Items[r.idx].Tracks.Total = r.total
		}
	}
}

func (p *AppPlayer) cachedPlaylistCount(uri string) (int, bool) {
	p.playlistCountMu.Lock()
	defer p.playlistCountMu.Unlock()
	e, ok := p.playlistCountCache[uri]
	if !ok {
		return 0, false
	}
	ttl := playlistCountCacheTTL
	if !e.ok {
		ttl = playlistCountFailTTL
	}
	if time.Since(e.at) > ttl {
		delete(p.playlistCountCache, uri)
		return 0, false
	}
	return e.total, ok
}

func (p *AppPlayer) storePlaylistCount(uri string, total int, ok bool) {
	p.playlistCountMu.Lock()
	defer p.playlistCountMu.Unlock()
	if p.playlistCountCache == nil {
		p.playlistCountCache = make(map[string]playlistCountEntry)
	}
	p.playlistCountCache[uri] = playlistCountEntry{total: total, at: time.Now(), ok: ok}
}

// fetchPlaylistCount asks Pathfinder for one page (limit 1) of the playlist to
// read its content.totalCount, using the in-memory count cache.
func (p *AppPlayer) fetchPlaylistCount(ctx context.Context, uri string) (int, bool) {
	if total, ok := p.cachedPlaylistCount(uri); ok {
		return total, ok
	}
	body := p.pfBody("fetchPlaylist", map[string]any{
		"uri":                       uri,
		"offset":                    0,
		"limit":                     1,
		"enableWatchFeedEntrypoint": false,
	})
	data, err := p.pathfinderQueryEx(ctx, body, false)
	if err != nil {
		p.storePlaylistCount(uri, 0, false)
		return 0, false
	}
	total, err := parsePlaylistCount(data)
	if err != nil {
		p.storePlaylistCount(uri, 0, false)
		return 0, false
	}
	p.storePlaylistCount(uri, total, true)
	return total, true
}

// parsePlaylistCount reads content.totalCount from a fetchPlaylist payload.
func parsePlaylistCount(data []byte) (int, error) {
	var r struct {
		Data struct {
			PlaylistV2 struct {
				Content struct {
					TotalCount int `json:"totalCount"`
				} `json:"content"`
			} `json:"playlistV2"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return 0, err
	}
	return r.Data.PlaylistV2.Content.TotalCount, nil
}
