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
	case "me/playlists", "me/player/recently-played", "me/tracks":
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
	case d.Path == "me/tracks":
		return p.webApiSavedTracks(ctx, d.Query)
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
// Each item carries the context the track was played from (album/playlist)
// as context_uri when it is a playable context (bug19).
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

// webApiSavedTracks serves GET me/tracks (Liked Songs) from the
// fetchLibraryTracks Pathfinder query (the same persisted query the voice
// catalog pages with), mapped to the Web API response shape (bug22).
func (p *AppPlayer) webApiSavedTracks(ctx context.Context, q url.Values) (any, error) {
	limit, offset := parseWebApiPaging(q)
	body := p.pfBody("fetchLibraryTracks", map[string]any{
		"offset": offset,
		"limit":  limit,
	})
	data, err := p.pathfinderQueryEx(ctx, body, false)
	if err != nil {
		p.app.log.Warnf("web-api: me/tracks: %v", err)
		return nil, err
	}
	items, total, err := mapSavedTracksPage(data, offset)
	if err != nil {
		p.app.log.Warnf("web-api: me/tracks: bad pathfinder payload: %v", err)
		return nil, err
	}
	next := any(nil)
	if offset+limit < total {
		next = fmt.Sprintf("https://api.spotify.com/v1/me/tracks?limit=%d&offset=%d", limit, offset+limit)
	}
	return map[string]any{
		"href":   "https://api.spotify.com/v1/me/tracks",
		"items":  items,
		"limit":  limit,
		"offset": offset,
		"next":   next,
		"total":  total,
	}, nil
}

// playableContextPrefixes are the entity types a connect play call accepts as
// a context uri (bug19: recents cards replay the context the track came from)
var playableContextPrefixes = []string{
	"spotify:album:",
	"spotify:playlist:",
	"spotify:collection:tracks",
	"spotify:artist:",
}

func isPlayableContextURI(uri string) bool {
	for _, pfx := range playableContextPrefixes {
		if strings.HasPrefix(uri, pfx) {
			return true
		}
	}
	return false
}

// ── lenient pathfinder identity helpers ────────────────────────────────────
// The web-player payloads rotate their serialization (e.g. contributors went
// from a flat array to an {items:[...]} object, album refs from inline
// {name,images} to entity wrappers). The mappers below therefore navigate
// with type assertions and accept every shape we have seen, in order.

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// firstString returns the first non-empty string value among the given keys.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := asStr(m[k]); s != "" {
			return s
		}
	}
	return ""
}

// identityCandidates collects the candidate objects that may carry the
// identity fields of an entity: the object itself, its data wrapper, and the
// identityTrait objects (at both nesting levels).
func identityCandidates(v any) []map[string]any {
	m := asMap(v)
	if m == nil {
		return nil
	}
	out := []map[string]any{m}
	if d := asMap(m["data"]); d != nil {
		out = append(out, d)
	}
	if t := asMap(m["identityTrait"]); t != nil {
		out = append(out, t)
	}
	if t := asMap(asMap(m["data"])["identityTrait"]); t != nil {
		out = append(out, t)
	}
	return out
}

// pfIdentity is the merged result of extracting name/uri/images from an
// entity-shaped value, whichever of the known shapes it uses.
type pfIdentity struct {
	Name   string
	URI    string
	Images []webApiImage
}

func pfIdentityFromAny(v any) pfIdentity {
	var out pfIdentity
	for _, c := range identityCandidates(v) {
		if out.Name == "" {
			out.Name = firstString(c, "name")
		}
		if out.URI == "" {
			out.URI = firstString(c, "uri", "_uri")
		}
		if len(out.Images) == 0 {
			if imgs := webApiImagesFromAny(c["images"]); imgs != nil {
				out.Images = imgs
			} else if imgs := webApiImagesFromAny(c["visualIdentityTrait"]); imgs != nil {
				out.Images = imgs
			} else if imgs := webApiImagesFromAny(c["coverArt"]); imgs != nil {
				out.Images = imgs
			}
		}
	}
	return out
}

// webApiImageSource is one entry of an image source list. The web-player
// payloads rotate the size fields between width/height (images/coverArt
// sources) and maxWidth/maxHeight (ImageV2 sources), so both spellings are
// accepted.
type webApiImageSource struct {
	URL       string `json:"url"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	MaxWidth  int    `json:"maxWidth"`
	MaxHeight int    `json:"maxHeight"`
}

func (s webApiImageSource) image() webApiImage {
	w, h := s.Width, s.Height
	if w == 0 {
		w = s.MaxWidth
	}
	if h == 0 {
		h = s.MaxHeight
	}
	return webApiImage{URL: s.URL, Width: w, Height: h}
}

// sourceImagesFromAny normalizes a list of image source objects to
// webApiImage (largest first, like the Web API image arrays), skipping
// entries without a url.
func sourceImagesFromAny(v any) []webApiImage {
	s := asSlice(v)
	if s == nil {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var arr []webApiImageSource
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil
	}
	out := make([]webApiImage, 0, len(arr))
	for _, src := range arr {
		im := src.image()
		if im.URL == "" {
			continue
		}
		out = append(out, im)
	}
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Width > out[j].Width })
	return out
}

// webApiImagesFromAny normalizes the image payloads the web-player queries
// ship to webApiImage. The serialization rotates per web-player build, so
// every shape we have seen is accepted, in order:
//
//	[{url,width,height}]                  bare source array
//	{sources|items|images: [...]}         wrapper objects (images, coverArt)
//	{url,width,height}                    single source object
//	{squareCoverImage|image: {image: {data: {sources:
//	  [{url,maxWidth,maxHeight}]}}}}       ImageV2 (2026 rotation, bug33)
//	{coverArt: {sources: [...]}}          track/album coverArt (2026, bug33)
//	{data: {...}}                         entity data wrapper
func webApiImagesFromAny(v any) []webApiImage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if imgs := sourceImagesFromAny(v); imgs != nil {
		return imgs
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return nil
	}
	var single webApiImageSource
	if err := json.Unmarshal(b, &single); err == nil && single.URL != "" {
		return []webApiImage{single.image()}
	}
	for _, key := range []string{"sources", "items", "images"} {
		if imgs := sourceImagesFromAny(m[key]); imgs != nil {
			return imgs
		}
	}
	for _, key := range []string{"squareCoverImage", "image", "coverArt", "images", "data"} {
		if sub := asMap(m[key]); sub != nil {
			if imgs := webApiImagesFromAny(sub); imgs != nil {
				return imgs
			}
		}
	}
	return nil
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
	items, total, err := mapPlaylistTracksPage(data, offset)
	if err != nil {
		p.app.log.Warnf("web-api: playlists/%s/tracks: bad pathfinder payload: %v", err)
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
// type; every entry carries the entity traits of the played item. The item
// bodies are kept as raw JSON (any) because the web-player serialization of
// the trait fields rotates (bug19: contributors changed shape and broke the
// strict parser with a 500).
type recentsPayload struct {
	Data struct {
		Lists []any `json:"lists"`
	} `json:"data"`
}

// recentsItem is one recents list entry, parsed leniently
type recentsItem struct {
	added   recentsAddedAt
	uri     string
	name    string
	artists []any
	images  []webApiImage
	// the context the track was played from (album/playlist/...), when known
	contextURI  string
	albumName   string
}

// parseRecentsItem extracts the identity fields from one raw list entry.
// It returns ok=false for entries that are not tracks (e.g. episodes).
func parseRecentsItem(raw any) (recentsItem, bool) {
	em := asMap(raw)
	if em == nil {
		return recentsItem{}, false
	}
	var it recentsItem
	if am := asMap(em["addedAt"]); am != nil {
		it.added = recentsAddedAt{Timestamp: asFloat(am["timestamp"]), IsoString: asStr(am["isoString"])}
	}
	ent := asMap(em["entity"])
	dm := asMap(ent["data"])
	it.uri = firstString(ent, "_uri", "uri")
	if it.uri == "" {
		it.uri = firstString(dm, "uri", "_uri")
	}
	if !strings.HasPrefix(it.uri, "spotify:track:") {
		return recentsItem{}, false
	}
	idTrait := asMap(dm["identityTrait"])
	it.name = firstString(idTrait, "name")
	if it.name == "" {
		it.name = firstString(dm, "name")
	}
	it.artists = webApiArtistsFromAny(idTrait["contributors"])
	// the visualIdentityTrait serialized as {images: {...}} in older builds
	// and as {squareCoverImage: {image: {data: {sources}}}} (ImageV2) in the
	// 2026 rotation — webApiImagesFromAny accepts both (bug33)
	if imgs := webApiImagesFromAny(dm["visualIdentityTrait"]); imgs != nil {
		it.images = imgs
	}
	if parent := asMap(idTrait["contentHierarchyParent"]); parent != nil {
		parentURI := firstString(parent, "uri", "_uri")
		parentName := firstString(asMap(parent["identityTrait"]), "name")
		if parentName == "" {
			parentName = firstString(parent, "name")
		}
		if parentName == "" {
			parentName = firstString(asMap(asMap(parent["data"])["identityTrait"]), "name")
		}
		// only the album parent names the album; other parents (playlists,
		// collections) are the playback context instead
		if strings.HasPrefix(parentURI, "spotify:album:") {
			it.albumName = parentName
		}
		if isPlayableContextURI(parentURI) {
			it.contextURI = parentURI
		}
	}
	return it, true
}

// the addedAt field of a recents list item: the web player ships it as an
// object ({timestamp (epoch ms), isoString}); older bundles sent a bare string
type recentsAddedAt struct {
	Timestamp float64
	IsoString string
}

// recentsPlayedAt converts the addedAt object into the ISO-8601 played_at
// string the Web API returns (falls back to the raw timestamp when isoString
// is absent)
func recentsPlayedAt(a recentsAddedAt) string {
	if a.IsoString != "" {
		return a.IsoString
	}
	if a.Timestamp > 0 {
		ms := a.Timestamp
		if ms < 1e12 { // bare seconds instead of milliseconds
			ms *= 1000
		}
		return time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339)
	}
	return ""
}

func mapRecentlyPlayedPage(data []byte) ([]any, error) {
	var r recentsPayload
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	out := make([]any, 0, len(r.Data.Lists))
	for _, listRaw := range r.Data.Lists {
		list := asMap(listRaw)
		if list == nil || asStr(list["__typename"]) != "List" {
			continue
		}
		entries := asSlice(asMap(list["items"])["items"])
		for _, raw := range entries {
			it, ok := parseRecentsItem(raw)
			if !ok {
				continue
			}
			id := strings.TrimPrefix(it.uri, "spotify:track:")
			album := map[string]any{"name": it.albumName, "images": []any{}}
			if len(it.images) > 0 {
				album["images"] = it.images
			}
			entry := map[string]any{
				"track": map[string]any{
					"id":      id,
					"name":    it.name,
					"uri":     it.uri,
					"artists": it.artists,
					"album":   album,
				},
				"played_at": recentsPlayedAt(it.added),
			}
			if it.contextURI != "" {
				entry["context_uri"] = it.contextURI
			}
			out = append(out, entry)
		}
	}
	return out, nil
}

// mapPlaylistTracksPage maps one page of a fetchPlaylist payload
// (playlistV2.content) to the Web API response shape. Items are parsed
// leniently via mapPfTrackItems (bug23): when the payload ships no album data
// for a track, the track's own cover (visualIdentityTrait) provides the
// artwork, so playlist track cards never render a bare black box.
func mapPlaylistTracksPage(data []byte, baseOffset int) ([]any, int, error) {
	var r struct {
		Data struct {
			PlaylistV2 struct {
				Content struct {
					TotalCount int   `json:"totalCount"`
					Items      []any `json:"items"`
				} `json:"content"`
			} `json:"playlistV2"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, 0, err
	}
	c := r.Data.PlaylistV2.Content
	return mapPfTrackItems(c.Items, baseOffset), c.TotalCount, nil
}

// mapSavedTracksPage maps a fetchLibraryTracks payload (data.me.library.tracks)
// to the Web API me/tracks item shape (bug22: the Liked Songs sub-menu).
func mapSavedTracksPage(data []byte, baseOffset int) ([]any, int, error) {
	var r struct {
		Data struct {
			Me struct {
				Library struct {
					Tracks struct {
						TotalCount int `json:"totalCount"`
						Items      []any `json:"items"`
					} `json:"tracks"`
				} `json:"library"`
			} `json:"me"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, 0, err
	}
	t := r.Data.Me.Library.Tracks
	return mapPfTrackItems(t.Items, baseOffset), t.TotalCount, nil
}

// mapPfTrackItems maps one page of raw pathfinder track items to the Web API
// playlist-track item shape ({is_local, track}). The item serialization
// rotates between queries (the library pages ship items[].track, the playlist
// pages items[].itemV2), so the fields are extracted leniently (same pattern
// as the recents mapper, bug19).
func mapPfTrackItems(rawItems []any, baseOffset int) []any {
	out := make([]any, 0, len(rawItems))
	for i, raw := range rawItems {
		em := asMap(raw)
		if em == nil {
			continue
		}
		tm := em
		if w := firstTrackWrapper(em); w != nil {
			tm = w
		}
		dm := asMap(tm["data"])
		if dm == nil {
			dm = tm
		}
		name := firstString(dm, "name")
		if name == "" {
			continue
		}
		uri := firstString(dm, "uri")
		if uri == "" {
			uri = firstString(tm, "_uri", "uri")
		}
		if uri == "" {
			continue
		}
		id := uri[strings.LastIndex(uri, ":")+1:]
		track := map[string]any{
			"id":       id,
			"name":     name,
			"uri":      uri,
			"artists":  webApiArtistsFromAny(dm["artists"]),
			"album":    webApiTrackAlbum(dm),
			"position": baseOffset + i,
		}
		if dur := asFloat(dm["durationMs"]); dur > 0 {
			track["duration_ms"] = int(dur)
		}
		out = append(out, map[string]any{
			"is_local": false,
			"track":    track,
		})
	}
	return out
}

// firstTrackWrapper returns the nested track entity wrapper of a raw page
// item (items[].track for the library, items[].itemV2 for playlists) or nil
// when the item is the track entity itself.
func firstTrackWrapper(em map[string]any) map[string]any {
	for _, key := range []string{"track", "itemV2"} {
		if m := asMap(em[key]); m != nil {
			return m
		}
	}
	return nil
}

// webApiTrackAlbum extracts the album name + images of a track item from the
// known pathfinder shapes: the item's album ref (inline {name,images}, an
// entity wrapper, or albumOfTrack {name,uri,coverArt} — the 2026 rotation,
// bug33) or, when the payload ships no album data at all, the track's own
// cover art (visualIdentityTrait) — the same artwork.
func webApiTrackAlbum(itemData map[string]any) map[string]any {
	album := map[string]any{"name": "", "images": []any{}}
	id := pfIdentityFromAny(itemData["album"])
	if a := asMap(itemData["albumOfTrack"]); a != nil {
		if aid := pfIdentityFromAny(a); id.Name == "" || len(id.Images) == 0 {
			if id.Name == "" {
				id.Name = aid.Name
			}
			if len(id.Images) == 0 {
				id.Images = aid.Images
			}
		}
	}
	if id.Name != "" {
		album["name"] = id.Name
	}
	if len(id.Images) > 0 {
		album["images"] = id.Images
	}
	if len(asSlice(album["images"])) == 0 {
		if imgs := webApiImagesFromAny(itemData["visualIdentityTrait"]); imgs != nil {
			album["images"] = imgs
		}
	}
	return album
}

// webApiArtistsFromAny maps the recents/track identityTrait contributors
// (bug19: shipped as a flat array of {uri,name} in older bundles and as an
// {items:[...]} object with profile-nested names in newer ones) to the Web
// API artist objects.
func webApiArtistsFromAny(v any) []any {
	entries := asSlice(v)
	if entries == nil {
		if items := asSlice(asMap(v)["items"]); items != nil {
			entries = items
		}
	}
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		em := asMap(e)
		if em == nil {
			continue
		}
		name := firstString(em, "name")
		if name == "" {
			name = firstString(asMap(em["profile"]), "name")
		}
		if name == "" {
			name = firstString(asMap(em["identityTrait"]), "name")
		}
		if name == "" {
			name = firstString(asMap(asMap(em["data"])["identityTrait"]), "name")
		}
		if name == "" {
			continue
		}
		a := map[string]any{"name": name}
		if uri := firstString(em, "uri", "_uri"); uri != "" {
			a["uri"] = uri
		} else if uri := firstString(asMap(em["profile"]), "uri", "_uri"); uri != "" {
			a["uri"] = uri
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
