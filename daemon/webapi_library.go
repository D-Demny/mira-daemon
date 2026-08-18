package daemon

import (
	"context"
	"encoding/json"
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
		return false
	}
}

// handleWebApiLocal routes the locally-served Web API paths.
func (p *AppPlayer) handleWebApiLocal(ctx context.Context, d ApiRequestDataWebApi) (any, error) {
	switch d.Path {
	case "me/playlists":
		return p.webApiPlaylists(ctx, d.Query)
	case "me/player/recently-played":
		// The current web-player Pathfinder build exposes no play-history
		// query, so this returns an empty list for now.
		return map[string]any{"items": []any{}, "next": nil}, nil
	default:
		return nil, ErrNotFound
	}
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
