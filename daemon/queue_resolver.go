package daemon

import (
	"context"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/devgianlu/go-librespot/spclient"
)

// queueResolver lazily fills artist/album (and, issue #50, missing cover art)
// on queue tracks that ship with only sparse metadata
type queueResolver struct {
	log       librespot.Logger
	spclient  *spclient.Spclient
	notify    chan struct{}
	imageSize string
	// webArt resolves a single uri's album cover via the web api — the same
	// /v1/tracks path the active track falls back to (resolveViaWebApi), so
	// no new endpoint or token is involved (issue #50)
	webArt func(ctx context.Context, uri string) string
	// batchFn resolves name/artist/album + album cover for the given uris in
	// one call; swappable in tests (default: the real spclient batch)
	batchFn func(ctx context.Context, uris []string) map[string]resolvedQueueTrack

	mu      sync.Mutex
	cache   map[string]resolvedQueueTrack
	pending map[string]struct{}
	// artTried remembers uris whose cover was settled once (found or
	// confirmed absent); keeps an artless track from being re-fetched on
	// every cluster update (issue #50)
	artTried map[string]struct{}
}

type resolvedQueueTrack struct {
	name     string
	artist   string
	album    string
	imageUrl string
}

// queueArtBackfillLimit bounds the cover backfill to the first N queued
// tracks: that is what the 'Läuft gerade' carousel actually shows, and as
// playback advances every later track slides into the window and gets
// covered on its turn (issue #50)
const queueArtBackfillLimit = 10

// queueArtWebApiTimeout caps a single web api cover lookup in the backfill
// pass — opportunistic background work, not the active track's 5s
const queueArtWebApiTimeout = 3 * time.Second

// queueResolverCacheMax bounds the metadata cache on long-running sessions
const queueResolverCacheMax = 500

func newQueueResolver(log librespot.Logger, sp *spclient.Spclient, notify chan struct{}, imageSize string, webArt func(ctx context.Context, uri string) string) *queueResolver {
	r := &queueResolver{
		log:       log,
		spclient:  sp,
		notify:    notify,
		imageSize: imageSize,
		webArt:    webArt,
		cache:     make(map[string]resolvedQueueTrack),
		pending:   make(map[string]struct{}),
		artTried:  make(map[string]struct{}),
	}
	r.batchFn = r.spclientBatch
	return r
}

// applyCache fills missing fields from cache (mutates in place) and returns the URIs that still need resolving
func (r *queueResolver) applyCache(tracks []QueueTrack) (needsResolve []string) {
	if len(tracks) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range tracks {
		complete := tracks[i].Name != "" && tracks[i].Artist != "" && tracks[i].Album != ""
		if complete {
			continue
		}
		if v, ok := r.cache[tracks[i].Uri]; ok {
			if tracks[i].Name == "" {
				tracks[i].Name = v.name
			}
			if tracks[i].Artist == "" {
				tracks[i].Artist = v.artist
			}
			if tracks[i].Album == "" {
				tracks[i].Album = v.album
			}
		}
		// still missing after cache hit, schedule a fetch + skip in-flight URIs
		if tracks[i].Name == "" || tracks[i].Artist == "" || tracks[i].Album == "" {
			if _, p := r.pending[tracks[i].Uri]; !p {
				r.pending[tracks[i].Uri] = struct{}{}
				needsResolve = append(needsResolve, tracks[i].Uri)
			}
		}
	}
	return needsResolve
}

// lookup returns cached artist/album for a single uri
func (r *queueResolver) lookup(uri string) (artist, album string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, found := r.cache[uri]
	if !found || v.artist == "" {
		return "", "", false
	}
	return v.artist, v.album, true
}

// ResolveAsync batch-fetches metadata (+ cover art for the given uris) and
// populates the cache, signals on completion
func (r *queueResolver) ResolveAsync(uris []string, artUris []string) {
	if len(uris) == 0 {
		return
	}
	art := make(map[string]struct{}, len(artUris))
	for _, u := range artUris {
		art[u] = struct{}{}
	}
	go r.resolve(uris, art)
}

func (r *queueResolver) resolve(uris []string, artUris map[string]struct{}) {
	// drop pending markers regardless of success, else failures block retries
	defer func() {
		r.mu.Lock()
		for _, u := range uris {
			delete(r.pending, u)
		}
		r.mu.Unlock()
	}()

	// short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	fetched := r.batchFn(ctx, uris)
	cancel()

	resolved := 0
	r.mu.Lock()
	for uri, entry := range fetched {
		if entry.name == "" && entry.artist == "" && entry.album == "" && entry.imageUrl == "" {
			continue
		}
		// a previously resolved cover survives an overwrite from the batch
		if entry.imageUrl == "" {
			entry.imageUrl = r.cache[uri].imageUrl
		}
		r.cache[uri] = entry
		resolved++
	}
	// collect the art uris still without a cover (read-only)
	var artStill []string
	for uri := range artUris {
		if r.cache[uri].imageUrl == "" {
			artStill = append(artStill, uri)
		}
	}
	r.mu.Unlock()

	// issue #50: covers the batch did not carry fall back to the web api, one
	// lookup per uri — OFF the lock, these calls do network I/O and must never
	// hold the cache mutex while the run loop applies it on every update
	if r.webArt != nil {
		for _, uri := range artStill {
			wctx, wc := context.WithTimeout(context.Background(), queueArtWebApiTimeout)
			url := r.webArt(wctx, uri)
			wc()
			if url == "" {
				continue
			}
			r.mu.Lock()
			entry := r.cache[uri]
			entry.imageUrl = url
			r.cache[uri] = entry
			resolved++
			r.mu.Unlock()
		}
	}

	r.mu.Lock()
	// settled (found or confirmed absent): no re-fetch on later updates
	for uri := range artUris {
		r.artTried[uri] = struct{}{}
	}
	r.evictLocked()
	r.mu.Unlock()

	r.log.Debugf("queue resolver: resolved %d/%d tracks", resolved, len(uris))

	if resolved == 0 {
		return
	}
	// non-blocking signal, the daemon will re-emit observer state on receipt
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// spclientBatch is the production batchFn: one batched spclient call for
// name/artist/album + album cover. A track that came back without an album
// cover still yields (an image-less) entry so the art path knows the lookup
// happened (issue #50).
func (r *queueResolver) spclientBatch(ctx context.Context, uris []string) map[string]resolvedQueueTrack {
	req := &extmetadatapb.BatchedEntityRequest{}
	for _, uri := range uris {
		req.EntityRequest = append(req.EntityRequest, &extmetadatapb.EntityRequest{
			EntityUri: uri,
			Query: []*extmetadatapb.ExtensionQuery{{
				ExtensionKind: extmetadatapb.ExtensionKind_TRACK_V4,
			}},
		})
	}

	resp, err := r.spclient.ExtendedMetadata(ctx, req)
	if err != nil {
		r.log.Debugf("queue resolver: batched metadata failed: %v", err)
		return nil
	}

	out := make(map[string]resolvedQueueTrack, len(uris))
	for _, item := range resp.ExtendedMetadata {
		for _, ext := range item.ExtensionData {
			if ext.Header == nil || ext.Header.StatusCode != 200 {
				continue
			}
			var track metadatapb.Track
			if err := ext.ExtensionData.UnmarshalTo(&track); err != nil {
				continue
			}
			var entry resolvedQueueTrack
			if track.Name != nil {
				entry.name = *track.Name
			}
			if len(track.Artist) > 0 && track.Artist[0].Name != nil {
				entry.artist = *track.Artist[0].Name
			}
			if track.Album != nil && track.Album.Name != nil {
				entry.album = *track.Album.Name
			}
			// issue #50: the same album-cover extraction the active track uses
			// (resolveViaSpclient) — queue cards get it too
			if track.Album != nil {
				entry.imageUrl = coverImageUrl(track.Album.Cover, r.imageSize)
				if entry.imageUrl == "" && track.Album.CoverGroup != nil {
					entry.imageUrl = coverImageUrl(track.Album.CoverGroup.Image, r.imageSize)
				}
			}
			out[ext.EntityUri] = entry
		}
	}
	return out
}

// issue #50 (M1): a queued card's cover is whatever its Connect metadata
// carries — when no image key is present, ImageUrl stays empty and the UI
// keeps its placeholder forever, unlike the active track which resolves its
// cover via spclient/web api. applyArtBackfill settles the first-N cards:
// it fills them from the cache once a resolution has landed, and (schedule)
// marks the unsettled ones for the next ResolveAsync pass. A track whose
// cover was settled before (artTried) is never scheduled again: an artless
// release stays artless and must not re-fetch on every cluster update.
func (r *queueResolver) applyArtBackfill(tracks []QueueTrack, schedule bool) (needsResolve []string) {
	n := len(tracks)
	if n > queueArtBackfillLimit {
		n = queueArtBackfillLimit
	}
	for i := 0; i < n; i++ {
		t := &tracks[i]
		if t.Uri == "" || t.ImageUrl != "" {
			continue
		}
		r.mu.Lock()
		if v, ok := r.cache[t.Uri]; ok && v.imageUrl != "" {
			// a resolution already found the cover: apply it, nothing to fetch
			t.ImageUrl = v.imageUrl
			r.mu.Unlock()
			continue
		}
		settled := false
		if _, tried := r.artTried[t.Uri]; tried {
			settled = true
		}
		needs := false
		if schedule && !settled {
			if _, p := r.pending[t.Uri]; !p {
				r.pending[t.Uri] = struct{}{}
				needs = true
			}
		}
		r.mu.Unlock()
		if needs {
			needsResolve = append(needsResolve, t.Uri)
		}
	}
	return needsResolve
}

// evictLocked halves the cache when it overflows, pruning artTried in step
// so an evicted track may be settled again later (caller holds r.mu)
func (r *queueResolver) evictLocked() {
	if len(r.cache) <= queueResolverCacheMax {
		return
	}
	n := len(r.cache) / 2
	for k := range r.cache {
		if n == 0 {
			break
		}
		delete(r.cache, k)
		delete(r.artTried, k)
		n--
	}
}
