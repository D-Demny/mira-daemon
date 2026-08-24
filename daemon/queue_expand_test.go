package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// bug32: the Connect cluster payload only carries a short preview of the
// upcoming queue; the full queue is derived from the active context's track
// list, validated against the authoritative preview.

func contextTracks(n int) []QueueTrack {
	list := make([]QueueTrack, 0, n)
	for i := 0; i < n; i++ {
		list = append(list, QueueTrack{
			Uri:     fmt.Sprintf("spotify:track:t%02d", i),
			TrackId: fmt.Sprintf("t%02d", i),
			Name:    fmt.Sprintf("Track %d", i),
			Artist:  "Artist",
		})
	}
	return list
}

// previewFrom derives a Connect-style preview: the first n upcoming tracks of
// the context (optionally prefixed by an echo of the active track, the shape
// single-track payloads ship — bug28)
func previewFrom(list []QueueTrack, activeIdx, n int, echo bool) []QueueTrack {
	preview := make([]QueueTrack, 0, n+1)
	if echo {
		preview = append(preview, QueueTrack{Uri: list[activeIdx].Uri, TrackId: list[activeIdx].TrackId})
	}
	taken := 0
	for i := activeIdx + 1; i < len(list) && taken < n; i++ {
		preview = append(preview, list[i])
		taken++
	}
	return preview
}

func TestComputeQueueExpansion_FullUpcomingQueueFromContextOrder(t *testing.T) {
	t.Parallel()

	list := contextTracks(12)
	activeIdx := 3
	preview := previewFrom(list, activeIdx, 2, false)

	got := computeQueueExpansion(list, list[activeIdx].Uri, list[activeIdx].TrackId, false, preview)
	if got == nil {
		t.Fatal("expected an expansion, got nil")
	}
	if len(got) != 12-activeIdx-1 {
		t.Fatalf("upcoming: got %d entries, want %d", len(got), 12-activeIdx-1)
	}
	for i, tr := range got {
		want := list[activeIdx+1+i]
		if tr.Uri != want.Uri || tr.TrackId != want.TrackId {
			t.Errorf("upcoming[%d]: got %s want %s", i, tr.Uri, want.Uri)
		}
	}
}

func TestComputeQueueExpansion_EchoOfActiveTrackInPreviewIsSkipped(t *testing.T) {
	t.Parallel()

	// single-track payloads ship the current track inside next_tracks (bug28):
	// the echo must be tolerated, not treated as a queue divergence
	list := contextTracks(6)
	activeIdx := 1
	preview := previewFrom(list, activeIdx, 3, true)

	got := computeQueueExpansion(list, list[activeIdx].Uri, list[activeIdx].TrackId, false, preview)
	if got == nil {
		t.Fatal("echo prefix in the preview must not block the expansion")
	}
	if len(got) != 6-activeIdx-1 {
		t.Errorf("upcoming: got %d entries, want %d", len(got), 6-activeIdx-1)
	}
}

func TestComputeQueueExpansion_PreviewDisagreesWithContextOrder(t *testing.T) {
	t.Parallel()

	// a manually queued track breaks the context order — keep the preview
	list := contextTracks(6)
	activeIdx := 1
	preview := previewFrom(list, activeIdx, 3, false)
	preview[1] = QueueTrack{Uri: "spotify:track:manually-queued", TrackId: "manually-queued"}

	if got := computeQueueExpansion(list, list[activeIdx].Uri, list[activeIdx].TrackId, false, preview); got != nil {
		t.Errorf("diverged queue must keep the preview, got an expansion of %d entries", len(got))
	}
}

func TestComputeQueueExpansion_PreviewLongerThanDerivedQueue(t *testing.T) {
	t.Parallel()

	// more preview entries than the context has upcoming — queue diverged
	list := contextTracks(3)
	activeIdx := 1
	preview := []QueueTrack{
		{Uri: list[2].Uri, TrackId: list[2].TrackId},
		{Uri: "spotify:track:extra-a", TrackId: "extra-a"},
		{Uri: "spotify:track:extra-b", TrackId: "extra-b"},
	}

	if got := computeQueueExpansion(list, list[activeIdx].Uri, list[activeIdx].TrackId, false, preview); got != nil {
		t.Errorf("longer preview must keep the preview, got an expansion of %d entries", len(got))
	}
}

func TestComputeQueueExpansion_ActiveTrackMissingFromContext(t *testing.T) {
	t.Parallel()

	list := contextTracks(5)
	if got := computeQueueExpansion(list, "spotify:track:unknown", "unknown", false, nil); got != nil {
		t.Errorf("active track absent from the context: expected nil, got %d entries", len(got))
	}
	if got := computeQueueExpansion(nil, "spotify:track:t00", "t00", false, nil); got != nil {
		t.Errorf("empty context list: expected nil, got %d entries", len(got))
	}
}

func TestComputeQueueExpansion_ActiveFoundByUriWhenIdMissing(t *testing.T) {
	t.Parallel()

	list := contextTracks(4)
	activeIdx := 2
	// no track id available, uri-only matching must still locate the track
	got := computeQueueExpansion(list, list[activeIdx].Uri, "", false, nil)
	if got == nil {
		t.Fatal("uri-only matching failed")
	}
	if len(got) != 1 || got[0].Uri != list[3].Uri {
		t.Errorf("upcoming: got %+v, want exactly [%s]", got, list[3].Uri)
	}
}

func TestComputeQueueExpansion_RepeatContextWrapsAround(t *testing.T) {
	t.Parallel()

	// with context repeat the queue wraps: after the last track the context
	// (and the active track) comes around again
	list := contextTracks(5)
	activeIdx := 3
	preview := previewFrom(list, activeIdx, 1, false)

	got := computeQueueExpansion(list, list[activeIdx].Uri, list[activeIdx].TrackId, true, preview)
	if got == nil {
		t.Fatal("expected an expansion with context repeat, got nil")
	}
	if len(got) != len(list) {
		t.Fatalf("wrap: got %d entries, want the full context length %d", len(got), len(list))
	}
	for i, tr := range got {
		want := list[(activeIdx+1+i)%len(list)]
		if tr.Uri != want.Uri {
			t.Errorf("wrap[%d]: got %s want %s", i, tr.Uri, want.Uri)
		}
	}
}

func TestComputeQueueExpansion_CapsAtQueueLimit(t *testing.T) {
	t.Parallel()

	list := contextTracks(QueueLimit + 40)
	activeIdx := 0
	preview := previewFrom(list, activeIdx, 2, false)

	got := computeQueueExpansion(list, list[activeIdx].Uri, list[activeIdx].TrackId, false, preview)
	if got == nil {
		t.Fatal("expected an expansion, got nil")
	}
	if len(got) != QueueLimit {
		t.Errorf("cap: got %d entries, want exactly the QueueLimit (%d)", len(got), QueueLimit)
	}
}

func TestExpandableContextUri(t *testing.T) {
	t.Parallel()

	tests := []struct {
		uri  string
		want string
	}{
		{"spotify:playlist:abc123", "spotify:playlist:abc123"},
		{"spotify:collection:tracks", "spotify:collection:tracks"},
		{"spotify:album:abc", ""},
		{"spotify:track:abc", ""},
		{"spotify:artist:abc", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := expandableContextUri(tt.uri); got != tt.want {
			t.Errorf("expandableContextUri(%q): got %q want %q", tt.uri, got, tt.want)
		}
	}
}

func TestTrackIdFromUri(t *testing.T) {
	t.Parallel()

	tests := []struct {
		uri  string
		want string
	}{
		{"spotify:track:abc123", "abc123"},
		{"spotify:track:abc:def", "abc:def"},
		{"spotify:track", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := trackIdFromUri(tt.uri); got != tt.want {
			t.Errorf("trackIdFromUri(%q): got %q want %q", tt.uri, got, tt.want)
		}
	}
}

func TestQueueTracksFromPfItems(t *testing.T) {
	t.Parallel()

	items := []any{
		map[string]any{
			"is_local": false,
			"track": map[string]any{
				"id":   "id-1",
				"name": "First",
				"uri":  "spotify:track:id-1",
				"artists": []any{
					map[string]any{"name": "Artist One", "uri": "spotify:artist:a1"},
					map[string]any{"name": "Artist Two"},
				},
				"album": map[string]any{
					"name": "Album One",
					"images": []any{
						webApiImage{URL: "spotify:image:big", Width: 640, Height: 640},
						webApiImage{URL: "spotify:image:mid", Width: 300, Height: 300},
					},
				},
			},
		},
		// no explicit id: derived from the uri
		map[string]any{
			"is_local": false,
			"track":    map[string]any{"name": "Second", "uri": "spotify:track:id-2"},
		},
		// not a track item: skipped
		map[string]any{"is_local": true},
		// no uri: skipped
		map[string]any{"track": map[string]any{"name": "No Uri"}},
	}

	got := queueTracksFromPfItems(items)
	if len(got) != 2 {
		t.Fatalf("got %d queue tracks, want 2: %+v", len(got), got)
	}
	first, second := got[0], got[1]
	if first.Uri != "spotify:track:id-1" || first.TrackId != "id-1" || first.Name != "First" {
		t.Errorf("first: got %+v", first)
	}
	if first.Artist != "Artist One" || first.Album != "Album One" {
		t.Errorf("first artist/album: got %q/%q", first.Artist, first.Album)
	}
	if first.ImageUrl != "https://i.scdn.co/image/mid" {
		t.Errorf("first image: got %q, want the ~300px variant converted", first.ImageUrl)
	}
	if second.Uri != "spotify:track:id-2" || second.TrackId != "id-2" {
		t.Errorf("second: got %+v", second)
	}
	if second.Artist != "" || second.Album != "" {
		t.Errorf("second artist/album must stay empty: got %q/%q", second.Artist, second.Album)
	}
}

func TestPfImageUrl(t *testing.T) {
	t.Parallel()

	// prefers the ~300px card variant over the full-res one
	variants := []any{
		webApiImage{URL: "spotify:image:big", Width: 640, Height: 640},
		webApiImage{URL: "spotify:image:mid", Width: 300, Height: 300},
	}
	if got := pfImageUrl(variants); got != "spotify:image:mid" {
		t.Errorf("got %q, want the 300px variant", got)
	}
	// no mid-size variant: first usable url wins
	if got := pfImageUrl([]any{webApiImage{URL: "spotify:image:big", Width: 640}}); got != "spotify:image:big" {
		t.Errorf("got %q, want the fallback url", got)
	}
	// map-shaped images (lenient payloads) work too
	if got := pfImageUrl([]any{map[string]any{"url": "spotify:image:x", "width": 300}}); got != "spotify:image:x" {
		t.Errorf("got %q, want the map-shaped url", got)
	}
	// empty / junk: empty result
	if got := pfImageUrl(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := pfImageUrl([]any{map[string]any{}}); got != "" {
		t.Errorf("got %q, want empty for url-less entries", got)
	}
}

func TestPruneQueueExpandCache(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// 40 contexts: 5 expired + 35 fresh (fresh-0 newest … fresh-34 oldest).
	// The expired entries go first, then the oldest fresh ones until the cap
	// (32) is reached: 35 - 3 evicted = 32
	cache := make(map[string]queueExpandCacheEntry, 40)
	for i := 0; i < 5; i++ {
		cache[fmt.Sprintf("expired-%d", i)] = queueExpandCacheEntry{fetchedAt: now.Add(-20 * time.Minute)}
	}
	for i := 0; i < 35; i++ {
		cache[fmt.Sprintf("fresh-%d", i)] = queueExpandCacheEntry{fetchedAt: now.Add(-time.Second * time.Duration(i))}
	}
	pruneQueueExpandCache(cache, now)
	if len(cache) != queueExpandCacheMaxContexts {
		t.Errorf("prune: got %d entries, want the cap (%d)", len(cache), queueExpandCacheMaxContexts)
	}
	for i := 0; i < 5; i++ {
		if _, ok := cache[fmt.Sprintf("expired-%d", i)]; ok {
			t.Errorf("prune: expired entry %d survived", i)
		}
	}
	// the freshest entries survive the oldest-first eviction
	if _, ok := cache["fresh-0"]; !ok {
		t.Errorf("prune: the newest entry was evicted")
	}
	for i := 32; i < 35; i++ {
		if _, ok := cache[fmt.Sprintf("fresh-%d", i)]; ok {
			t.Errorf("prune: the oldest fresh entry %d survived", i)
		}
	}
}

func newTestQueueExpandPlayer(t *testing.T) *AppPlayer {
	t.Helper()
	return &AppPlayer{
		app:                 &App{log: &librespot.NullLogger{}},
		queueExpandCache:    make(map[string]queueExpandCacheEntry),
		queueExpandInFlight: make(map[string]struct{}),
		queueExpandedCh:     make(chan queueExpandResult, 1),
	}
}

func TestExpandQueue_GuardsLeaveThePreviewUntouched(t *testing.T) {
	t.Parallel()

	p := newTestQueueExpandPlayer(t)
	preview := []QueueTrack{{Uri: "spotify:track:next", TrackId: "next"}}

	mk := func(rs *RemoteState) *RemoteState {
		if rs == nil {
			rs = &RemoteState{}
		}
		rs.NextTracks = preview
		return rs
	}

	// shuffle playback: the order is not derivable
	rs := mk(&RemoteState{TrackUri: "spotify:track:t00", ContextUri: "spotify:playlist:pl", ShuffleContext: true})
	p.expandQueue(rs)
	if len(rs.NextTracks) != 1 {
		t.Errorf("shuffle: the preview must be kept, got %d entries", len(rs.NextTracks))
	}
	// non-expandable context (album): the order is not enumerable
	rs = mk(&RemoteState{TrackUri: "spotify:track:t00", ContextUri: "spotify:album:ab"})
	p.expandQueue(rs)
	if len(rs.NextTracks) != 1 {
		t.Errorf("album context: the preview must be kept, got %d entries", len(rs.NextTracks))
	}
	// no active track: nothing to anchor the expansion
	rs = mk(&RemoteState{ContextUri: "spotify:playlist:pl"})
	p.expandQueue(rs)
	if len(rs.NextTracks) != 1 {
		t.Errorf("no active track: the preview must be kept, got %d entries", len(rs.NextTracks))
	}
}

func TestExpandQueue_CacheHitReplacesTheShortPreview(t *testing.T) {
	t.Parallel()

	p := newTestQueueExpandPlayer(t)
	list := contextTracks(12)
	activeIdx := 3
	// the cache holds the full context track list (fetched earlier)
	p.queueExpandCache["spotify:playlist:pl"] = queueExpandCacheEntry{
		list:      list,
		total:     len(list),
		fetchedAt: time.Now(),
	}

	// the cluster payload only ships a short preview
	preview := previewFrom(list, activeIdx, 2, false)
	rs := &RemoteState{
		TrackUri:   list[activeIdx].Uri,
		ContextUri: "spotify:playlist:pl",
		NextTracks: preview,
	}
	p.expandQueue(rs)

	if len(rs.NextTracks) != 12-activeIdx-1 {
		t.Fatalf("cache hit: got %d upcoming entries, want %d", len(rs.NextTracks), 12-activeIdx-1)
	}
	for i, tr := range rs.NextTracks {
		want := list[activeIdx+1+i]
		if tr.Uri != want.Uri {
			t.Errorf("upcoming[%d]: got %s want %s", i, tr.Uri, want.Uri)
			break
		}
	}
}

func TestExpandQueue_StaleCacheIsIgnored(t *testing.T) {
	t.Parallel()

	p := newTestQueueExpandPlayer(t)
	p.queueExpandCache["spotify:playlist:pl"] = queueExpandCacheEntry{
		list:      contextTracks(12),
		total:     12,
		fetchedAt: time.Now().Add(-queueExpandCacheTTL - time.Minute),
	}

	// a stale entry must not be applied; the cache-miss path starts a
	// background fetch (stubbed here to an empty page, which writes no cache)
	p.queueExpandPageFn = func(_ context.Context, _ string, _, _ int) ([]any, int, error) {
		return nil, 0, nil
	}
	preview := []QueueTrack{{Uri: "spotify:track:next", TrackId: "next"}}
	rs := &RemoteState{
		TrackUri:   "spotify:track:t00",
		ContextUri: "spotify:playlist:pl",
		NextTracks: preview,
	}
	p.expandQueue(rs)
	if len(rs.NextTracks) != 1 {
		t.Errorf("stale cache: the preview must be kept, got %d entries", len(rs.NextTracks))
	}
	select {
	case res := <-p.queueExpandedCh:
		t.Errorf("empty fetch must not deliver a result, got %+v", res)
	default:
	}
}

func TestExpandQueue_CacheMissFetchesAndApplies(t *testing.T) {
	t.Parallel()

	const total = 150 // spans two pages (100 + 50)
	const activeIdx = 7
	var all []any
	for i := 0; i < total; i++ {
		all = append(all, map[string]any{
			"is_local": false,
			"track": map[string]any{
				"id":   fmt.Sprintf("t%03d", i),
				"name": fmt.Sprintf("Track %d", i),
				"uri":  fmt.Sprintf("spotify:track:t%03d", i),
			},
		})
	}
	var pages int
	p := newTestQueueExpandPlayer(t)
	p.queueExpandPageFn = func(_ context.Context, _ string, offset, limit int) ([]any, int, error) {
		pages++
		end := offset + limit
		if end > total {
			end = total
		}
		if offset >= total {
			return nil, total, nil
		}
		return all[offset:end], total, nil
	}

	rs := &RemoteState{
		TrackUri:   fmt.Sprintf("spotify:track:t%03d", activeIdx),
		ContextUri: "spotify:playlist:pl",
		NextTracks: []QueueTrack{{Uri: "spotify:track:t008", TrackId: "t008"}},
	}
	p.expandQueue(rs)
	// the cache miss started a background fetch; it lands on the run-loop channel
	select {
	case res := <-p.queueExpandedCh:
		if res.total != total {
			t.Errorf("result total: got %d want %d", res.total, total)
		}
		if len(res.list) != total {
			t.Errorf("result list: got %d tracks want %d (paging across pages)", len(res.list), total)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no queue expansion result on the run-loop channel")
	}
	if pages != 2 {
		t.Errorf("paged %d times, want 2 (100 + 50)", pages)
	}
	// the in-flight marker is released after the fetch
	p.queueExpandMu.Lock()
	_, inflight := p.queueExpandInFlight["spotify:playlist:pl"]
	p.queueExpandMu.Unlock()
	if inflight {
		t.Errorf("in-flight marker must be released after the fetch")
	}
	// the run loop re-applies the landed result through the cache (bug32)
	before := len(rs.NextTracks)
	p.expandQueue(rs)
	if len(rs.NextTracks) == before {
		t.Fatalf("run-loop re-apply: queue unchanged (%d entries)", before)
	}
	// the derived 142 entries are capped at the QueueLimit payload guard
	if len(rs.NextTracks) != QueueLimit {
		t.Errorf("re-apply: got %d upcoming entries, want the QueueLimit cap (%d)", len(rs.NextTracks), QueueLimit)
	}
}
