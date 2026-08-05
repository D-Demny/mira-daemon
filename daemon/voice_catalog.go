package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// catalog sync, fetches users catalog thru pathfinder,

const (
	catalogPageSize     = 50
	catalogMaxRetries   = 6
	catalogMaxPlaylists = 200
	catalogIndexVersion = 3
)

var (
	catalogMinInterval  = 350 * time.Millisecond
	catalogJitterMax    = 250 * time.Millisecond
	catalogBackoffStart = 2 * time.Second
	catalogBackoffCap   = 60 * time.Second
)

type catalogFetcher interface {
	likedTracks(ctx context.Context, offset, limit int, force bool) ([]catalogItem, int, error)
	libraryList(ctx context.Context, filter string, offset, limit int, force bool) ([]catalogItem, int, error)
	playlistTracks(ctx context.Context, uri string, offset, limit int, force bool) ([]catalogItem, int, error)
	topArtists(ctx context.Context, offset, limit int, force bool) ([]catalogItem, int, error)
}

type catalogSyncer struct {
	fetch            catalogFetcher
	g2p              *g2p
	cacheDir         string
	log              logger
	publish          func(*routedIndex)
	onPersistedDrift func()
	onProgress       func(catalogProgress)
	last             time.Time
}

type catalogProgress struct {
	Stage   string  `json:"stage"`
	Done    int     `json:"done"`
	Total   int     `json:"total"`
	Percent float64 `json:"percent"`
}

var catalogStageSpans = map[string][2]float64{
	"liked":           {0, 25},
	"playlists":       {25, 5},
	"artists":         {30, 4},
	"albums":          {34, 4},
	"playlist-tracks": {38, 52},
	"top-artists":     {90, 2},
	"finalizing":      {92, 8},
}

// frac < 0 means derive it from done/total
func (s *catalogSyncer) progress(stage string, done, total int, frac float64) {
	if s.onProgress == nil {
		return
	}
	span, ok := catalogStageSpans[stage]
	if !ok {
		return
	}
	if frac < 0 {
		frac = 0
		if total > 0 {
			frac = float64(done) / float64(total)
		}
	}
	if frac > 1 {
		frac = 1
	}
	s.onProgress(catalogProgress{Stage: stage, Done: done, Total: total, Percent: span[0] + span[1]*frac})
}

type logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

func newCatalogSyncer(f catalogFetcher, g *g2p, cacheDir string, log logger, publish func(*routedIndex)) *catalogSyncer {
	return &catalogSyncer{fetch: f, g2p: g, cacheDir: cacheDir, log: log, publish: publish}
}

// resumable so a restart resumes mid sync
type checkpoint struct {
	Version int `json:"version"`

	LikedOffset int           `json:"liked_offset"`
	LikedDone   bool          `json:"liked_done"`
	Liked       []catalogItem `json:"liked"`

	PlaylistsOffset int           `json:"playlists_offset"`
	PlaylistsDone   bool          `json:"playlists_done"`
	Playlists       []catalogItem `json:"playlists"`

	// followed artists
	ArtistsOffset int           `json:"artists_offset"`
	ArtistsDone   bool          `json:"artists_done"`
	Artists       []catalogItem `json:"artists"`

	AlbumsOffset int           `json:"albums_offset"`
	AlbumsDone   bool          `json:"albums_done"`
	Albums       []catalogItem `json:"albums"`

	PlTracksPlIdx  int                      `json:"pltracks_pl_idx"`
	PlTracksOffset int                      `json:"pltracks_offset"`
	PlTracksDone   bool                     `json:"pltracks_done"`
	PlaylistTracks map[string][]catalogItem `json:"playlist_tracks"`

	// TopArtists
	TopArtists     []catalogItem `json:"top_artists"`
	TopArtistsDone bool          `json:"top_artists_done"`

	// Last seen server totalCount per source
	LikedTotal     int            `json:"liked_total"`
	PlaylistsTotal int            `json:"playlists_total"`
	AlbumsTotal    int            `json:"albums_total"`
	ArtistsTotal   int            `json:"artists_total"`
	PlTrackCounts  map[string]int `json:"pltrack_counts"`
}

// returns the per playlist track map
func (cp *checkpoint) plTracks() map[string][]catalogItem {
	if cp.PlaylistTracks == nil {
		cp.PlaylistTracks = map[string][]catalogItem{}
	}
	return cp.PlaylistTracks
}

// returns the per playlist server count
func (cp *checkpoint) plTrackCounts() map[string]int {
	if cp.PlTrackCounts == nil {
		cp.PlTrackCounts = map[string]int{}
	}
	return cp.PlTrackCounts
}

func (s *catalogSyncer) checkpointPath() string {
	return filepath.Join(s.cacheDir, "catalog_checkpoint.json")
}
func (s *catalogSyncer) indexPath() string { return filepath.Join(s.cacheDir, "phonetic_index.json") }

// returns the previously built index if present and valid
func (s *catalogSyncer) loadCachedIndex() (*routedIndex, bool) {
	b, err := os.ReadFile(s.indexPath())
	if err != nil {
		return nil, false
	}
	var wrap struct {
		Version int          `json:"version"`
		Index   *routedIndex `json:"index"`
	}
	if json.Unmarshal(b, &wrap) != nil || wrap.Version != catalogIndexVersion || wrap.Index == nil {
		return nil, false
	}
	return wrap.Index, true
}

func (s *catalogSyncer) saveIndex(idx *routedIndex) {
	wrap := struct {
		Version int          `json:"version"`
		BuiltAt string       `json:"built_at"`
		Index   *routedIndex `json:"index"`
	}{Version: catalogIndexVersion, BuiltAt: time.Now().UTC().Format(time.RFC3339), Index: idx}
	b, err := json.Marshal(wrap)
	if err != nil {
		return
	}
	if err := atomicWrite(s.indexPath(), b); err != nil {
		s.log.Warnf("voice catalog: failed to cache index: %v", err)
	}
}

func (s *catalogSyncer) loadCheckpoint() *checkpoint {
	cp := &checkpoint{Version: catalogIndexVersion}
	b, err := os.ReadFile(s.checkpointPath())
	if err != nil {
		return cp
	}
	var loaded checkpoint
	if json.Unmarshal(b, &loaded) == nil && loaded.Version == catalogIndexVersion {
		return &loaded
	}
	return cp
}

func (s *catalogSyncer) saveCheckpoint(cp *checkpoint) {
	b, err := json.Marshal(cp)
	if err != nil {
		return
	}
	if err := atomicWrite(s.checkpointPath(), b); err != nil {
		s.log.Debugf("voice catalog: failed to checkpoint: %v", err)
	}
}

func (s *catalogSyncer) Run(ctx context.Context) (*routedIndex, error) {
	if err := os.MkdirAll(s.cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("catalog cache dir: %w", err)
	}
	cp := s.loadCheckpoint()
	s.log.Infof("voice catalog: sync starting (resume: liked=%d/%v playlists=%d/%v)",
		len(cp.Liked), cp.LikedDone, len(cp.Playlists), cp.PlaylistsDone)

	stages := []struct {
		name string
		run  func(context.Context, *checkpoint) error
	}{
		{"liked", s.syncLiked},
		{"playlists", s.syncPlaylists},
		{"artists", s.syncArtists},
		{"albums", s.syncAlbums},
		{"playlist-tracks", s.syncPlaylistTracks},
		{"top-artists", s.syncTopArtists},
	}
	s.progress("liked", 0, 0, 0)
	for _, st := range stages {
		if ctx.Err() != nil {
			return s.buildIndex(ctx, cp), ctx.Err()
		}
		if err := st.run(ctx, cp); err != nil {
			if ctx.Err() != nil {
				return s.buildIndex(ctx, cp), ctx.Err()
			}
			// Nonfatal
			s.log.Warnf("voice catalog: stage %q stopped: %v (falling through to searchDesktop for it)", st.name, err)
		}
		s.progress(st.name, 0, 0, 1)
		s.saveCheckpoint(cp)
		// publish a live partial index so voice improves as the sync proceeds
		s.publish(s.buildIndex(ctx, cp))
	}

	s.progress("finalizing", 0, 0, 0)
	idx := s.buildIndex(ctx, cp)
	s.saveIndex(idx)
	s.progress("finalizing", 0, 0, 1)
	s.log.Infof("voice catalog: sync complete (tracks=%d artists=%d playlists=%d albums=%d)",
		len(idx.Tracks), len(idx.Artists), len(idx.Playlists), len(idx.Albums))
	return idx, nil
}

// refresh checks each source current count and refetches only if something changed
func (s *catalogSyncer) Refresh(ctx context.Context) (*routedIndex, bool, error) {
	if err := os.MkdirAll(s.cacheDir, 0o755); err != nil {
		return nil, false, fmt.Errorf("catalog cache dir: %w", err)
	}
	cp := s.loadCheckpoint()
	if !cp.LikedDone {
		// no valid prior sync
		s.log.Infof("voice catalog: refresh base missing, running full sync")
		idx, err := s.Run(ctx)
		return idx, true, err
	}

	var what []string
	stop := func(name string, err error) {
		if err != nil && ctx.Err() == nil {
			s.log.Warnf("voice catalog: refresh %s stopped: %v", name, err)
		}
	}

	// liked songs
	if upd, ok, err := s.refreshLiked(ctx, cp); err != nil {
		stop("liked", err)
	} else if ok {
		cp.Liked = upd
		what = append(what, "liked")
	}

	// followed artist/album/playlist
	if ok, err := s.refreshList(ctx, "Artists", &cp.Artists, &cp.ArtistsTotal); err != nil {
		stop("artists", err)
	} else if ok {
		what = append(what, "artists")
	}
	if ok, err := s.refreshList(ctx, "Albums", &cp.Albums, &cp.AlbumsTotal); err != nil {
		stop("albums", err)
	} else if ok {
		what = append(what, "albums")
	}
	if ok, err := s.refreshList(ctx, "Playlists", &cp.Playlists, &cp.PlaylistsTotal); err != nil {
		stop("playlists", err)
	} else if ok {
		what = append(what, "playlists")
	}

	// playlist tracks
	if s.refreshPlaylistTracks(ctx, cp) {
		what = append(what, "playlist-tracks")
	}

	if len(what) == 0 {
		s.log.Infof("voice catalog: refresh, no changes (liked=%d playlists=%d albums=%d artists=%d)",
			len(cp.Liked), len(cp.Playlists), len(cp.Albums), len(cp.Artists))
		return nil, false, nil
	}

	s.saveCheckpoint(cp)
	idx := s.buildIndex(ctx, cp)
	s.saveIndex(idx)
	s.publish(idx)
	s.log.Infof("voice catalog: refresh complete, updated [%s] (tracks=%d artists=%d playlists=%d albums=%d)",
		strings.Join(what, ","), len(idx.Tracks), len(idx.Artists), len(idx.Playlists), len(idx.Albums))
	return idx, true, nil
}

// refreshLiked checks the song count and newest page
func (s *catalogSyncer) refreshLiked(ctx context.Context, cp *checkpoint) ([]catalogItem, bool, error) {
	page, total, err := s.do(ctx, func(force bool) ([]catalogItem, int, error) {
		return s.fetch.likedTracks(ctx, 0, catalogPageSize, force)
	})
	if err != nil {
		return nil, false, err
	}
	old := cp.Liked
	if total == cp.LikedTotal && sameURIHead(page, old) {
		return nil, false, nil // unchanged
	}

	oldIdx := make(map[string]int, len(old))
	for i, it := range old {
		if it.Uri != "" {
			if _, dup := oldIdx[it.Uri]; !dup {
				oldIdx[it.Uri] = i
			}
		}
	}
	var prepended []catalogItem
	overlap := -1
	offset := 0
	cur := page
	for {
		brk := false
		for _, it := range cur {
			if it.Uri != "" {
				if i, ok := oldIdx[it.Uri]; ok {
					overlap = i
					brk = true
					break
				}
			}
			prepended = append(prepended, it)
		}
		if brk || len(cur) < catalogPageSize {
			break
		}
		offset += len(cur)
		if offset > total+catalogPageSize {
			break
		}
		if cur, _, err = s.do(ctx, func(force bool) ([]catalogItem, int, error) {
			return s.fetch.likedTracks(ctx, offset, catalogPageSize, force)
		}); err != nil {
			return nil, false, err
		}
	}
	if overlap >= 0 {
		merged := append(prepended, old[overlap:]...)
		if len(merged) == total {
			cp.LikedTotal = total
			return merged, true, nil
		}
	} else if len(cur) < catalogPageSize && len(prepended) == total {
		cp.LikedTotal = total
		return prepended, true, nil
	}
	full, err := s.fetchAllPaged(ctx, func(off int, force bool) ([]catalogItem, int, error) {
		return s.fetch.likedTracks(ctx, off, catalogPageSize, force)
	})
	if err != nil {
		return nil, false, err
	}
	cp.LikedTotal = total
	return full, true, nil
}

func (s *catalogSyncer) fetchAllPaged(ctx context.Context, fetch func(off int, force bool) ([]catalogItem, int, error)) ([]catalogItem, error) {
	var all []catalogItem
	offset := 0
	for {
		off := offset
		page, total, err := s.do(ctx, func(force bool) ([]catalogItem, int, error) {
			return fetch(off, force)
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		offset += len(page)
		if len(page) < catalogPageSize || (total > 0 && offset >= total) {
			break
		}
	}
	return all, nil
}

func (s *catalogSyncer) refreshList(ctx context.Context, filter string, acc *[]catalogItem, storedTotal *int) (bool, error) {
	_, total, err := s.do(ctx, func(force bool) ([]catalogItem, int, error) {
		return s.fetch.libraryList(ctx, filter, 0, 1, force)
	})
	if err != nil {
		return false, err
	}
	if total == *storedTotal {
		return false, nil
	}
	var all []catalogItem
	off, done, newTotal := 0, false, 0
	if err := s.syncList(ctx, filter, &off, &done, &all, &newTotal); err != nil {
		return false, err
	}
	*acc = all
	*storedTotal = newTotal
	return true, nil
}

func (s *catalogSyncer) refreshPlaylistTracks(ctx context.Context, cp *checkpoint) bool {
	changed := false
	m := cp.plTracks()
	counts := cp.plTrackCounts()
	cur := make(map[string]bool)
	n := len(cp.Playlists)
	if n > catalogMaxPlaylists {
		n = catalogMaxPlaylists
	}
	for i := 0; i < n; i++ {
		pl := cp.Playlists[i]
		if !strings.HasPrefix(pl.Uri, "spotify:playlist:") {
			continue
		}
		cur[pl.Uri] = true
		uri := pl.Uri
		_, total, err := s.do(ctx, func(force bool) ([]catalogItem, int, error) {
			return s.fetch.playlistTracks(ctx, uri, 0, 1, force)
		})
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warnf("voice catalog: refresh playlist %s probe stopped: %v", uri, err)
			}
			continue
		}
		if _, ok := m[uri]; ok && total == counts[uri] {
			continue // unchanged
		}
		tracks, err := s.fetchAllPaged(ctx, func(off int, force bool) ([]catalogItem, int, error) {
			return s.fetch.playlistTracks(ctx, uri, off, catalogPageSize, force)
		})
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warnf("voice catalog: refresh playlist %s stopped: %v", uri, err)
			}
			continue
		}
		m[uri] = tracks
		counts[uri] = total
		changed = true
	}
	for uri := range m {
		if !cur[uri] {
			delete(m, uri)
			delete(counts, uri)
			changed = true
		}
	}
	return changed
}

// sameURIHead reports whether pages URIs match the head of old
func sameURIHead(page, old []catalogItem) bool {
	if len(old) < len(page) {
		return false
	}
	for i, it := range page {
		if it.Uri != old[i].Uri {
			return false
		}
	}
	return true
}

func (s *catalogSyncer) syncLiked(ctx context.Context, cp *checkpoint) error {
	for !cp.LikedDone {
		items, total, err := s.do(ctx, func(force bool) ([]catalogItem, int, error) {
			return s.fetch.likedTracks(ctx, cp.LikedOffset, catalogPageSize, force)
		})
		if err != nil {
			return err
		}
		cp.Liked = append(cp.Liked, items...)
		cp.LikedOffset += len(items)
		if total > 0 {
			cp.LikedTotal = total
		}
		if len(items) < catalogPageSize || (total > 0 && cp.LikedOffset >= total) {
			cp.LikedDone = true
		}
		s.progress("liked", cp.LikedOffset, cp.LikedTotal, -1)
		s.saveCheckpoint(cp)
	}
	return nil
}

func (s *catalogSyncer) syncList(ctx context.Context, filter string, offset *int, done *bool, acc *[]catalogItem, totalOut *int) error {
	for !*done {
		off := *offset
		items, total, err := s.do(ctx, func(force bool) ([]catalogItem, int, error) {
			return s.fetch.libraryList(ctx, filter, off, catalogPageSize, force)
		})
		if err != nil {
			return err
		}
		*acc = append(*acc, items...)
		*offset += len(items)
		if total > 0 && totalOut != nil {
			*totalOut = total
		}
		if len(items) < catalogPageSize || (total > 0 && *offset >= total) {
			*done = true
		}
		s.progress(strings.ToLower(filter), *offset, total, -1)
	}
	return nil
}

func (s *catalogSyncer) syncPlaylists(ctx context.Context, cp *checkpoint) error {
	return s.syncList(ctx, "Playlists", &cp.PlaylistsOffset, &cp.PlaylistsDone, &cp.Playlists, &cp.PlaylistsTotal)
}
func (s *catalogSyncer) syncArtists(ctx context.Context, cp *checkpoint) error {
	return s.syncList(ctx, "Artists", &cp.ArtistsOffset, &cp.ArtistsDone, &cp.Artists, &cp.ArtistsTotal)
}
func (s *catalogSyncer) syncAlbums(ctx context.Context, cp *checkpoint) error {
	return s.syncList(ctx, "Albums", &cp.AlbumsOffset, &cp.AlbumsDone, &cp.Albums, &cp.AlbumsTotal)
}

func (s *catalogSyncer) syncTopArtists(ctx context.Context, cp *checkpoint) error {
	if cp.TopArtistsDone {
		return nil
	}
	items, _, err := s.do(ctx, func(force bool) ([]catalogItem, int, error) {
		return s.fetch.topArtists(ctx, 0, catalogPageSize, force)
	})
	if err != nil {
		return err
	}
	cp.TopArtists = items // kept separate from followed artists
	cp.TopArtistsDone = true
	return nil
}

// expands each saved playlists tracks
func (s *catalogSyncer) syncPlaylistTracks(ctx context.Context, cp *checkpoint) error {
	if cp.PlTracksDone {
		return nil
	}
	n := len(cp.Playlists)
	if n > catalogMaxPlaylists {
		n = catalogMaxPlaylists
	}
	for cp.PlTracksPlIdx < n {
		pl := cp.Playlists[cp.PlTracksPlIdx]
		if !strings.HasPrefix(pl.Uri, "spotify:playlist:") {
			cp.PlTracksPlIdx++
			cp.PlTracksOffset = 0
			continue
		}
		off := cp.PlTracksOffset
		items, total, err := s.do(ctx, func(force bool) ([]catalogItem, int, error) {
			return s.fetch.playlistTracks(ctx, pl.Uri, off, catalogPageSize, force)
		})
		if err != nil {
			return err
		}
		cp.plTracks()[pl.Uri] = append(cp.plTracks()[pl.Uri], items...)
		cp.PlTracksOffset += len(items)
		if total > 0 {
			cp.plTrackCounts()[pl.Uri] = total
		}
		if len(items) < catalogPageSize || (total > 0 && cp.PlTracksOffset >= total) {
			cp.PlTracksPlIdx++
			cp.PlTracksOffset = 0
		}
		within := 0.0
		if total > 0 && cp.PlTracksOffset > 0 {
			within = float64(cp.PlTracksOffset) / float64(total)
		}
		s.progress("playlist-tracks", cp.PlTracksPlIdx, n, (float64(cp.PlTracksPlIdx)+within)/float64(n))
		s.saveCheckpoint(cp)
	}
	cp.PlTracksDone = true
	return nil
}

func (s *catalogSyncer) do(ctx context.Context, fetch func(force bool) ([]catalogItem, int, error)) ([]catalogItem, int, error) {
	backoff := catalogBackoffStart
	usedForceRetry := false
	for attempt := 0; ; attempt++ {
		if err := s.pace(ctx); err != nil {
			return nil, 0, err
		}
		items, total, err := fetch(false)
		if err == nil {
			return items, total, nil
		}

		var pe *pathfinderError
		if errors.As(err, &pe) {
			switch {
			case pe.PersistedQuery:
				if s.onPersistedDrift != nil {
					s.onPersistedDrift()
				}
				return nil, 0, fmt.Errorf("persisted-query hash rotated (rescrape triggered): %w", err)
			case pe.Status == 400:
				if s.onPersistedDrift != nil {
					s.onPersistedDrift()
				}
				return nil, 0, fmt.Errorf("bad request (likely a rotated hash rescrape triggered): %w", err)
			case (pe.Status == 401 || pe.Status == 403) && !usedForceRetry:
				usedForceRetry = true
				s.log.Warnf("voice catalog: %d, reminting tokens, retrying once", pe.Status)
				if err := s.pace(ctx); err != nil {
					return nil, 0, err
				}
				if items, total, ferr := fetch(true); ferr == nil {
					return items, total, nil
				} else {
					err = ferr // fall through to backoff
				}
			case pe.Status == 429 || pe.Status >= 500:
				// throttled/server error
				wait := pe.RetryAfter
				if wait <= 0 {
					wait = backoff
				}
				if attempt >= catalogMaxRetries {
					return nil, 0, fmt.Errorf("gave up after %d retries: %w", attempt, err)
				}
				s.log.Warnf("voice catalog: %d, backing off %s (attempt %d)", pe.Status, wait, attempt+1)
				if err := sleepCtx(ctx, wait); err != nil {
					return nil, 0, err
				}
				backoff = nextBackoff(backoff)
				continue
			}
		}

		// network/unknown error
		if attempt >= catalogMaxRetries {
			return nil, 0, fmt.Errorf("gave up after %d retries: %w", attempt, err)
		}
		s.log.Warnf("voice catalog: transient error, backing off %s (attempt %d): %v", backoff, attempt+1, err)
		if err := sleepCtx(ctx, backoff); err != nil {
			return nil, 0, err
		}
		backoff = nextBackoff(backoff)
	}
}

// enforces the minimum request interval
func (s *catalogSyncer) pace(ctx context.Context) error {
	jitter := time.Duration(rand.Int63n(int64(catalogJitterMax) + 1))
	target := catalogMinInterval + jitter
	if !s.last.IsZero() {
		if elapsed := time.Since(s.last); elapsed < target {
			if err := sleepCtx(ctx, target-elapsed); err != nil {
				return err
			}
		}
	}
	s.last = time.Now()
	return nil
}

func nextBackoff(b time.Duration) time.Duration {
	b *= 2
	if b > catalogBackoffCap {
		b = catalogBackoffCap
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// returns map keys in sorted order
func sortedKeys(m map[string][]catalogItem) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// concatenates followed + top artists
func mergeArtists(followed, top []catalogItem) []catalogItem {
	out := make([]catalogItem, 0, len(followed)+len(top))
	seen := make(map[string]bool, len(followed)+len(top))
	add := func(a catalogItem) {
		if a.Uri != "" {
			if seen[a.Uri] {
				return
			}
			seen[a.Uri] = true
		}
		out = append(out, a)
	}
	for _, a := range followed {
		add(a)
	}
	for _, a := range top {
		add(a)
	}
	return out
}

func (s *catalogSyncer) buildIndex(ctx context.Context, cp *checkpoint) *routedIndex {
	idx := &routedIndex{}

	var allNames []string
	for _, it := range cp.Liked {
		allNames = append(allNames, it.Name, it.Artist)
	}
	for _, tracks := range cp.PlaylistTracks {
		for _, it := range tracks {
			allNames = append(allNames, it.Name, it.Artist)
		}
	}
	for _, it := range cp.Artists {
		allNames = append(allNames, it.Name)
	}
	for _, it := range cp.TopArtists {
		allNames = append(allNames, it.Name)
	}
	for _, it := range cp.Playlists {
		allNames = append(allNames, it.Name)
	}
	for _, it := range cp.Albums {
		allNames = append(allNames, it.Name)
	}
	s.g2p.ipaMany(ctx, allNames)

	// tracks = liked + playlist tracks
	trackSeen := make(map[string]bool)
	addTrack := func(it catalogItem) {
		if it.Name == "" {
			return
		}
		key := it.Uri
		if key == "" {
			key = strings.ToLower(it.Name + "|" + it.Artist)
		}
		if trackSeen[key] {
			return
		}
		trackSeen[key] = true
		idx.Tracks = append(idx.Tracks, indexEntry{
			Name: it.Name, Artist: it.Artist, Uri: it.Uri,
			Ipa:       s.g2p.ipa(ctx, it.Name),
			ArtistIpa: s.g2p.ipa(ctx, it.Artist),
		})
	}
	for _, it := range cp.Liked {
		addTrack(it)
	}
	for _, uri := range sortedKeys(cp.PlaylistTracks) {
		for _, it := range cp.PlaylistTracks[uri] {
			addTrack(it)
		}
	}

	// liked only songs for random track command
	likedSeen := make(map[string]bool)
	for _, it := range cp.Liked {
		if it.Uri == "" || it.Name == "" || likedSeen[it.Uri] {
			continue
		}
		likedSeen[it.Uri] = true
		idx.Liked = append(idx.Liked, indexEntry{Name: it.Name, Artist: it.Artist, Uri: it.Uri})
	}

	idx.Artists = s.phonemizeNames(ctx, mergeArtists(cp.Artists, cp.TopArtists), false, false)
	idx.Playlists = s.phonemizeNames(ctx, cp.Playlists, false, false)
	idx.Albums = s.phonemizeNames(ctx, cp.Albums, true, true)
	return idx
}

func (s *catalogSyncer) phonemizeNames(ctx context.Context, items []catalogItem, withArtist, sortByName bool) []indexEntry {
	seen := make(map[string]bool)
	out := make([]indexEntry, 0, len(items))
	for _, it := range items {
		if it.Name == "" {
			continue
		}
		key := it.Uri
		if key == "" {
			if withArtist {
				key = strings.ToLower(it.Name + "|" + it.Artist)
			} else {
				key = strings.ToLower(it.Name)
			}
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		e := indexEntry{Name: it.Name, Uri: it.Uri, Ipa: s.g2p.ipa(ctx, it.Name)}
		if withArtist {
			e.Artist = it.Artist
		}
		out = append(out, e)
	}
	if sortByName {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	}
	return out
}

func atomicWrite(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
