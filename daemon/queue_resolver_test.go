package daemon

import (
	"context"
	"fmt"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
)

// newTestQueueResolver wires a queueResolver with test doubles: the spclient
// batch is swappable via batch (nil keeps the real one, unused here) and the
// web api cover lookup is injected like in production (player.go hands in
// AppPlayer.queueWebArt) (issue #50)
func newTestQueueResolver(batch func(ctx context.Context, uris []string) map[string]resolvedQueueTrack, webArt func(ctx context.Context, uri string) string) *queueResolver {
	r := newQueueResolver(&librespot.NullLogger{}, nil, make(chan struct{}, 1), "large", webArt)
	if batch != nil {
		r.batchFn = batch
	}
	return r
}

func artSet(uris ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(uris))
	for _, u := range uris {
		set[u] = struct{}{}
	}
	return set
}

// an empty image_url is backfilled: the batch carries no cover for uri-b, so
// the web api lookup fills it — while uri-a's batch-provided cover does not
// trigger a second (duplicate) lookup for the same uri
func TestQueueResolver_ArtBackfillViaWebApi(t *testing.T) {
	t.Parallel()

	var webCalls []string
	r := newTestQueueResolver(
		func(_ context.Context, uris []string) map[string]resolvedQueueTrack {
			out := make(map[string]resolvedQueueTrack, len(uris))
			for _, u := range uris {
				e := resolvedQueueTrack{name: "n-" + u, artist: "a-" + u, album: "al-" + u}
				if u == "uri-a" {
					e.imageUrl = "https://example.com/batch-a.jpg"
				}
				out[u] = e
			}
			return out
		},
		func(_ context.Context, uri string) string {
			webCalls = append(webCalls, uri)
			return "https://example.com/web-" + uri + ".jpg"
		},
	)

	tracks := []QueueTrack{
		{Uri: "uri-a", Name: "x", Artist: "a", Album: "b"}, // batch brings the cover
		{Uri: "uri-b", Name: "", Artist: "", Album: ""},    // needs the web api fallback
	}
	needs := r.applyArtBackfill(tracks, true)
	if len(needs) != 2 {
		t.Fatalf("applyArtBackfill: got %v, want both uris", needs)
	}
	r.resolve(needs, artSet(needs...))

	if got := r.cache["uri-a"].imageUrl; got != "https://example.com/batch-a.jpg" {
		t.Errorf("uri-a cover: got %q", got)
	}
	if got := r.cache["uri-b"].imageUrl; got != "https://example.com/web-uri-b.jpg" {
		t.Errorf("uri-b cover: got %q, want the web api result", got)
	}
	if len(webCalls) != 1 || webCalls[0] != "uri-b" {
		t.Errorf("web api calls: got %v, want exactly [uri-b]", webCalls)
	}

	// production applies the landed resolutions on the next state emit —
	// simulate that pass and check both cards end up covered
	if needs = r.applyArtBackfill(tracks, false); len(needs) != 0 {
		t.Fatalf("apply after resolve: got %v, want none (everything settled)", needs)
	}
	if tracks[0].ImageUrl != "https://example.com/batch-a.jpg" {
		t.Errorf("track uri-a image not filled from cache: %q", tracks[0].ImageUrl)
	}
	if tracks[1].ImageUrl != "https://example.com/web-uri-b.jpg" {
		t.Errorf("track uri-b image not filled from cache: %q", tracks[1].ImageUrl)
	}

	r.mu.Lock()
	pending := len(r.pending)
	r.mu.Unlock()
	if pending != 0 {
		t.Errorf("%d pending markers left after resolve, want 0", pending)
	}
}

// a card whose Connect metadata already carried a cover is never scheduled,
// never overwritten by the web art hook, and an earlier web api cover in the
// cache survives a later batch entry that lacks one
func TestQueueResolver_ExistingArtNotOverwritten(t *testing.T) {
	t.Parallel()

	webCalled := false
	r := newTestQueueResolver(
		func(_ context.Context, uris []string) map[string]resolvedQueueTrack {
			out := make(map[string]resolvedQueueTrack, len(uris))
			for _, u := range uris { // text only, no album cover in the batch
				out[u] = resolvedQueueTrack{name: "n", artist: "a", album: "b"}
			}
			return out
		},
		func(_ context.Context, uri string) string {
			webCalled = true
			return "https://example.com/wrong-" + uri
		},
	)

	tracks := []QueueTrack{
		{Uri: "uri-a", ImageUrl: "https://example.com/connect-art.jpg"}, // Connect shipped a cover
	}
	if needs := r.applyArtBackfill(tracks, true); len(needs) != 0 {
		t.Fatalf("art present: scheduled %v, want none", needs)
	}
	if tracks[0].ImageUrl != "https://example.com/connect-art.jpg" {
		t.Errorf("applyArtBackfill touched a Connect-provided cover: %q", tracks[0].ImageUrl)
	}

	// merge level: an image-less batch entry must not wipe an already
	// resolved cover from the cache
	r.cache["uri-c"] = resolvedQueueTrack{artist: "a", imageUrl: "https://example.com/earlier.jpg"}
	r.resolve([]string{"uri-c"}, nil)
	if got := r.cache["uri-c"].imageUrl; got != "https://example.com/earlier.jpg" {
		t.Errorf("merge lost a previously resolved cover: %q", got)
	}
	if webCalled {
		t.Error("web art hook ran although no card needed a cover")
	}
}

// both lookups fail (spclient error + web api without cover): the track stays
// artless without panicking, gets settled exactly once (no re-fetch on every
// cluster update), and no failure state poisons the cache or pending set
func TestQueueResolver_ArtlessReleaseStaysArtlessWithoutRetry(t *testing.T) {
	t.Parallel()

	webCalls := 0
	r := newTestQueueResolver(
		func(_ context.Context, uris []string) map[string]resolvedQueueTrack { return nil }, // spclient failure
		func(_ context.Context, uri string) string { webCalls++; return "" },                // web api has no cover either
	)

	tracks := []QueueTrack{{Uri: "uri-a"}}
	needs := r.applyArtBackfill(tracks, true)
	if len(needs) != 1 || needs[0] != "uri-a" {
		t.Fatalf("first pass: got %v, want [uri-a]", needs)
	}
	r.resolve(needs, artSet(needs...))

	if _, ok := r.cache["uri-a"]; ok {
		t.Error("failed resolution left a cache entry behind")
	}
	r.mu.Lock()
	_, tried := r.artTried["uri-a"]
	pending := len(r.pending)
	r.mu.Unlock()
	if !tried {
		t.Error("failed art lookup not marked settled (artTried)")
	}
	if pending != 0 {
		t.Errorf("%d pending markers left after resolve, want 0", pending)
	}

	// second cluster update: the settled track must not be scheduled again
	if second := r.applyArtBackfill(tracks, true); len(second) != 0 {
		t.Fatalf("settled artless track rescheduled: %v", second)
	}
	if webCalls != 1 {
		t.Errorf("web api called %d times for a settled track, want 1", webCalls)
	}
	if tracks[0].ImageUrl != "" {
		t.Errorf("artless track picked up an image: %q", tracks[0].ImageUrl)
	}
}

// the backfill bounds to the first queueArtBackfillLimit queued tracks — the
// rest only enter the window as playback advances, on their turn
func TestQueueResolver_ArtBackfillBoundToFirstN(t *testing.T) {
	t.Parallel()

	r := newTestQueueResolver(nil, nil) // scheduling only; nothing is fetched here

	var tracks []QueueTrack
	for i := 0; i < 15; i++ {
		tracks = append(tracks, QueueTrack{Uri: fmt.Sprintf("uri-%d", i)})
	}
	needs := r.applyArtBackfill(tracks, true)
	if len(needs) != queueArtBackfillLimit {
		t.Fatalf("scheduled %d uris, want exactly %d", len(needs), queueArtBackfillLimit)
	}
	for i, u := range needs {
		want := fmt.Sprintf("uri-%d", i)
		if u != want {
			t.Errorf("needs[%d] = %s, want %s (first N in order)", i, u, want)
		}
	}
	r.mu.Lock()
	_, beyondPending := r.pending["uri-14"]
	r.mu.Unlock()
	if beyondPending {
		t.Error("track beyond the bound was marked pending")
	}

	// a later update must not expand the window: tracks 0..N are settled or
	// in flight, so only nothing new is scheduled
	if again := r.applyArtBackfill(tracks, true); len(again) != 0 {
		t.Fatalf("second pass scheduled %v, want none (pending/scheduled already)", again)
	}
}

// a track id that does not parse never reaches the web art hook with garbage —
// the production hook (AppPlayer.queueWebArt) swallows the parse error; this
// pins that its result (empty) behaves like any other empty cover
func TestQueueResolver_WebArtErrorYieldsEmptyCover(t *testing.T) {
	t.Parallel()

	var gotUri string
	r := newTestQueueResolver(
		func(_ context.Context, uris []string) map[string]resolvedQueueTrack { return nil },
		func(_ context.Context, uri string) string { gotUri = uri; return "" },
	)

	tracks := []QueueTrack{{Uri: "spotify:track:deadbeef"}}
	needs := r.applyArtBackfill(tracks, true)
	if len(needs) != 1 {
		t.Fatalf("applyArtBackfill: got %v, want [the uri]", needs)
	}
	r.resolve(needs, artSet(needs...))

	if gotUri != "spotify:track:deadbeef" {
		t.Errorf("web art hook called with %q", gotUri)
	}
	if tracks[0].ImageUrl != "" {
		t.Errorf("cover set despite an empty web api result: %q", tracks[0].ImageUrl)
	}
	r.mu.Lock()
	_, tried := r.artTried["spotify:track:deadbeef"]
	r.mu.Unlock()
	if !tried {
		t.Error("empty web api result not marked settled")
	}
}
