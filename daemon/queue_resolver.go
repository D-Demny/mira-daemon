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

// queueResolver lazily fills artist/album on queue tracks that ship with only uri + image
type queueResolver struct {
	log      librespot.Logger
	spclient *spclient.Spclient
	notify   chan struct{}

	mu      sync.Mutex
	cache   map[string]resolvedQueueTrack
	pending map[string]struct{}
}

type resolvedQueueTrack struct {
	name   string
	artist string
	album  string
}

func newQueueResolver(log librespot.Logger, sp *spclient.Spclient, notify chan struct{}) *queueResolver {
	return &queueResolver{
		log:      log,
		spclient: sp,
		notify:   notify,
		cache:    make(map[string]resolvedQueueTrack),
		pending:  make(map[string]struct{}),
	}
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

// ResolveAsync batch-fetches metadata + populates cache, signals on completion
func (r *queueResolver) ResolveAsync(uris []string) {
	if len(uris) == 0 {
		return
	}
	go r.resolve(uris)
}

func (r *queueResolver) resolve(uris []string) {
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
	defer cancel()

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
		return
	}

	resolved := 0
	r.mu.Lock()
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
			if entry.name == "" && entry.artist == "" && entry.album == "" {
				continue
			}
			r.cache[ext.EntityUri] = entry
			resolved++
		}
	}
	// bound the cache
	if len(r.cache) > 500 {
		n := len(r.cache) / 2
		for k := range r.cache {
			if n == 0 {
				break
			}
			delete(r.cache, k)
			n--
		}
	}
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
