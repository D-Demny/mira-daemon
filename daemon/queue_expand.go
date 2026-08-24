package daemon

import (
	"context"
	"strings"
	"time"
)

// bug32: the Spotify Connect cluster payload only ships a short preview of
// the queue — PlayerState.NextTracks carries just a handful of the upcoming
// tracks regardless of how long the real queue is (verified on the device:
// a Liked-Songs session delivered exactly 2 upcoming entries). Nothing in
// the daemon truncates further (QueueLimit=100), so the "3 item" cap the UI
// saw is the payload itself.
//
// For sequential (non-shuffle) playback of a deterministic context the
// upcoming queue IS the context from the active track on, so the preview can
// be expanded into the full queue from the context's track list — the same
// Pathfinder queries the locally-served /web-api/ endpoints already page
// with. The authoritative Connect preview validates the expansion: if it
// disagrees with the context order (manually queued tracks, edits) we keep
// the preview instead of showing a wrong queue.

const (
	queueExpandPageLimit        = 100
	queueExpandMaxEntries       = 500 // safety cap for a context's length
	queueExpandCacheTTL         = 10 * time.Minute
	queueExpandFetchTimeout     = 30 * time.Second
	queueExpandCacheMaxContexts = 32 // bound the cache on long-running sessions
)

// queueExpandCacheEntry is one fetched context track list
type queueExpandCacheEntry struct {
	list      []QueueTrack
	total     int
	fetchedAt time.Time
}

// queueExpandResult is delivered to the run loop when a background fetch lands
type queueExpandResult struct {
	contextUri string
	list       []QueueTrack
	total      int
}

// expandableContextUri reports whether the context's track order can be
// enumerated (the two contexts the daemon already pages from Pathfinder)
func expandableContextUri(uri string) string {
	if strings.HasPrefix(uri, "spotify:playlist:") {
		return uri
	}
	if uri == "spotify:collection:tracks" {
		return uri
	}
	return ""
}

// expandQueue replaces the short Connect preview in rs.NextTracks with the
// full upcoming queue when it can be derived. It never blocks the run loop:
// a cached expansion is applied synchronously (memory only), a cache miss
// starts a background fetch whose result lands on queueExpandedCh, where the
// run loop re-applies the expansion (by then the cache holds the list, so
// expandQueue hits the cache path).
func (p *AppPlayer) expandQueue(rs *RemoteState) {
	if rs == nil || rs.TrackUri == "" || rs.ShuffleContext {
		return
	}
	contextUri := expandableContextUri(rs.ContextUri)
	if contextUri == "" {
		return
	}

	activeId := trackIdFromUri(rs.TrackUri)
	p.queueExpandMu.Lock()
	entry, ok := p.queueExpandCache[contextUri]
	if ok && time.Since(entry.fetchedAt) >= queueExpandCacheTTL {
		ok = false
	}
	inflight := false
	if !ok {
		_, inflight = p.queueExpandInFlight[contextUri]
	}
	p.queueExpandMu.Unlock()

	if ok {
		if expanded := computeQueueExpansion(entry.list, rs.TrackUri, activeId, rs.RepeatContext, rs.NextTracks); expanded != nil {
			rs.NextTracks = expanded
		}
		return
	}
	if inflight {
		return
	}
	p.queueExpandMu.Lock()
	p.queueExpandInFlight[contextUri] = struct{}{}
	p.queueExpandMu.Unlock()
	go p.fetchQueueExpand(contextUri)
}

// computeQueueExpansion returns the upcoming queue of the active track
// (Spotify queue order, the active track being position 0) for a
// deterministic context played in order, or nil when the expansion cannot be
// trusted (active track missing from the context, or the authoritative
// Connect preview disagrees with the context order).
func computeQueueExpansion(
	list []QueueTrack,
	activeUri, activeId string,
	repeatContext bool,
	preview []QueueTrack,
) []QueueTrack {
	if len(list) == 0 {
		return nil
	}

	// locate the active track in the context (id first, uri as fallback)
	idx := -1
	if activeId != "" {
		for i := range list {
			if list[i].TrackId == activeId {
				idx = i
				break
			}
		}
	}
	if idx < 0 && activeUri != "" {
		for i := range list {
			if list[i].Uri == activeUri {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return nil
	}

	upcoming := make([]QueueTrack, 0, len(list))
	for i := idx + 1; i < len(list); i++ {
		upcoming = append(upcoming, list[i])
	}
	if repeatContext {
		// with context repeat the queue wraps: after the last track the
		// context (and the active track) comes around again
		for i := 0; i <= idx; i++ {
			upcoming = append(upcoming, list[i])
		}
	}

	// the Connect preview is authoritative: it must match the derived order.
	// A leading entry echoing the active track (single-track payloads ship the
	// current track inside next_tracks, bug28) is stripped, not an error.
	compared := preview
	if len(preview) > 0 {
		first := preview[0]
		if first.Uri == activeUri || (first.TrackId != "" && first.TrackId == activeId) {
			compared = preview[1:]
		}
	}
	if len(compared) > len(upcoming) {
		// the preview is longer than the derived queue — the queue diverged
		// from the plain context order (manually queued tracks, edits) —
		// keep the preview
		return nil
	}
	for i := range compared {
		if compared[i].Uri == "" || compared[i].Uri != upcoming[i].Uri {
			return nil
		}
	}

	if len(upcoming) > QueueLimit {
		upcoming = upcoming[:QueueLimit]
	}
	return upcoming
}

// pruneQueueExpandCache drops expired entries and, if the cache is still
// over the cap, the oldest ones — bounds memory on long-running sessions
// with many different contexts
func pruneQueueExpandCache(cache map[string]queueExpandCacheEntry, now time.Time) {
	for uri, entry := range cache {
		if now.Sub(entry.fetchedAt) >= queueExpandCacheTTL {
			delete(cache, uri)
		}
	}
	for len(cache) > queueExpandCacheMaxContexts {
		var oldest string
		var oldestAt time.Time
		for uri, entry := range cache {
			if oldest == "" || entry.fetchedAt.Before(oldestAt) {
				oldest = uri
				oldestAt = entry.fetchedAt
			}
		}
		if oldest == "" {
			return
		}
		delete(cache, oldest)
	}
}

// trackIdFromUri extracts the id part of a three-part Spotify uri
func trackIdFromUri(uri string) string {
	if parts := strings.SplitN(uri, ":", 3); len(parts) == 3 {
		return parts[2]
	}
	return ""
}

// fetchQueueExpand pages the full track list of an expandable context in the
// background and hands it to the run loop via queueExpandedCh
func (p *AppPlayer) fetchQueueExpand(contextUri string) {
	defer func() {
		p.queueExpandMu.Lock()
		delete(p.queueExpandInFlight, contextUri)
		p.queueExpandMu.Unlock()
	}()

	fctx, cancel := context.WithTimeout(context.Background(), queueExpandFetchTimeout)
	defer cancel()

	page := p.queueExpandPage
	if p.queueExpandPageFn != nil {
		page = p.queueExpandPageFn
	}

	var list []QueueTrack
	total := 0
	for offset := 0; offset < queueExpandMaxEntries; {
		limit := queueExpandPageLimit
		if offset+limit > queueExpandMaxEntries {
			limit = queueExpandMaxEntries - offset
		}
		items, pageTotal, err := page(fctx, contextUri, offset, limit)
		if err != nil {
			p.app.log.Warnf("queue expand: %s page %d: %v", contextUri, offset, err)
			return
		}
		list = append(list, queueTracksFromPfItems(items)...)
		total = pageTotal
		offset += limit
		if total > 0 && offset >= total {
			break
		}
	}
	if len(list) == 0 {
		return
	}

	now := time.Now()
	p.queueExpandMu.Lock()
	p.queueExpandCache[contextUri] = queueExpandCacheEntry{list: list, total: total, fetchedAt: now}
	if len(p.queueExpandCache) > queueExpandCacheMaxContexts {
		pruneQueueExpandCache(p.queueExpandCache, now)
	}
	p.queueExpandMu.Unlock()
	p.app.log.Debugf("queue expand: %s -> %d tracks (total %d)", contextUri, len(list), total)

	select {
	case p.queueExpandedCh <- queueExpandResult{contextUri: contextUri, list: list, total: total}:
	default:
		// the run loop is busy; the next state update re-applies the cache
	}
}

// queueExpandPage fetches one page of the context's tracks with the same
// lenient mappers the local /web-api/ endpoints use
func (p *AppPlayer) queueExpandPage(ctx context.Context, contextUri string, offset, limit int) ([]any, int, error) {
	var body []byte
	if strings.HasPrefix(contextUri, "spotify:playlist:") {
		body = p.pfBody("fetchPlaylist", map[string]any{
			"uri":                       contextUri,
			"offset":                    offset,
			"limit":                     limit,
			"enableWatchFeedEntrypoint": false,
		})
	} else {
		body = p.pfBody("fetchLibraryTracks", map[string]any{
			"offset": offset,
			"limit":  limit,
		})
	}
	data, err := p.pathfinderQueryEx(ctx, body, false)
	if err != nil {
		return nil, 0, err
	}
	if strings.HasPrefix(contextUri, "spotify:playlist:") {
		return mapPlaylistTracksPage(data, offset)
	}
	return mapSavedTracksPage(data, offset)
}

// queueTracksFromPfItems maps the lenient /web-api/ track items to QueueTracks
func queueTracksFromPfItems(items []any) []QueueTrack {
	out := make([]QueueTrack, 0, len(items))
	for _, raw := range items {
		tr := asMap(asMap(raw)["track"])
		if tr == nil {
			continue
		}
		uri := asStr(tr["uri"])
		if uri == "" {
			continue
		}
		id := asStr(tr["id"])
		if id == "" {
			id = trackIdFromUri(uri)
		}
		var artist string
		if arts := asSlice(tr["artists"]); len(arts) > 0 {
			artist = firstString(asMap(arts[0]), "name")
		}
		var album string
		if alb := asMap(tr["album"]); alb != nil {
			album = firstString(alb, "name")
		}
		imageUrl := ""
		if alb := asMap(tr["album"]); alb != nil {
			imageUrl = convertSpotifyImageUrl(pfImageUrl(alb["images"]))
		}
		out = append(out, QueueTrack{
			Uri:      uri,
			TrackId:  id,
			Name:     asStr(tr["name"]),
			Artist:   artist,
			Album:    album,
			ImageUrl: imageUrl,
		})
	}
	return out
}

// pfImageUrl picks the queue card artwork from a pathfinder image payload:
// the ~300px variant when available (the card art is 170px, a full-res 640px
// decode is a main-thread cost — bug8.2), otherwise the first usable url
func pfImageUrl(v any) string {
	first := ""
	for _, im := range asSlice(v) {
		var url string
		var width int
		switch t := im.(type) {
		case webApiImage:
			url, width = t.URL, t.Width
		case map[string]any:
			url = firstString(t, "url", "URL")
			width = int(asFloat(t["width"]))
		}
		if url == "" {
			continue
		}
		if first == "" {
			first = url
		}
		if width >= 250 && width <= 400 {
			return url
		}
	}
	return first
}
