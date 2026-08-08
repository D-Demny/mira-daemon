package daemon

import (
	"math/rand"
	"testing"
	"time"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

func TestProjectQueue_EmptyInputReturnsNil(t *testing.T) {
	t.Parallel()

	if got := projectQueue(nil, 10); got != nil {
		t.Errorf("nil input: got %v want nil", got)
	}
	if got := projectQueue([]*connectpb.ProvidedTrack{}, 10); got != nil {
		t.Errorf("empty slice: got %v want nil", got)
	}
}

func TestProjectQueue_SkipsNilAndEmptyUriEntries(t *testing.T) {
	t.Parallel()

	// Spotify sometimes ships "queued but unresolved" rows where the URI hasnt been filled in yet
	tracks := []*connectpb.ProvidedTrack{
		{Uri: "spotify:track:keep_1"},
		nil,
		{Uri: ""},
		{Uri: "spotify:track:keep_2"},
	}

	got := projectQueue(tracks, 10)
	if len(got) != 2 {
		t.Fatalf("expected 2 valid entries, got %d: %+v", len(got), got)
	}
	if got[0].Uri != "spotify:track:keep_1" || got[1].Uri != "spotify:track:keep_2" {
		t.Errorf("wrong entries surfaced: %+v", got)
	}
}

func TestProjectQueue_HonorsLimitAndStopsEarly(t *testing.T) {
	t.Parallel()

	// 15 valid tracks, limit=10,  exactly 10 returne
	tracks := make([]*connectpb.ProvidedTrack, 15)
	for i := range tracks {
		tracks[i] = &connectpb.ProvidedTrack{Uri: "spotify:track:abc"}
	}

	got := projectQueue(tracks, 10)
	if len(got) != 10 {
		t.Errorf("expected %d entries (limit), got %d", 10, len(got))
	}
}

func TestProjectQueue_LimitInteractsWithSkippedEntries(t *testing.T) {
	t.Parallel()

	// the limit check counts valid entries, not loop iterations
	tracks := []*connectpb.ProvidedTrack{
		{Uri: ""}, // skipped
		{Uri: "spotify:track:a"},
		nil, // skipped
		{Uri: "spotify:track:b"},
		{Uri: "spotify:track:c"},
		{Uri: "spotify:track:d"}, // dropped by limit
	}

	got := projectQueue(tracks, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(got), got)
	}
	if got[0].Uri != "spotify:track:a" ||
		got[1].Uri != "spotify:track:b" ||
		got[2].Uri != "spotify:track:c" {
		t.Errorf("unexpected ordering: %+v", got)
	}
}

func TestProjectQueue_ExtractsTrackIdFromThreePartSpotifyUri(t *testing.T) {
	t.Parallel()

	// 3-part Spotify URI, trackId is the third part. non-3-part leaves it empty
	tracks := []*connectpb.ProvidedTrack{
		{Uri: "spotify:track:abc123"},
		{Uri: "spotify:local:something"}, // 3-part but not "track" - still extracts "something"
		{Uri: "spotify:track:abc:extra"}, // 4-part - strings.SplitN(",",3) returns 3 parts where the third has ":extra"
		{Uri: "local-file"},              // 1-part - no trackId
	}

	got := projectQueue(tracks, 10)
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(got))
	}
	if got[0].TrackId != "abc123" {
		t.Errorf("standard URI: trackId got %q want %q", got[0].TrackId, "abc123")
	}
	if got[1].TrackId != "something" {
		t.Errorf("local URI: trackId got %q want %q", got[1].TrackId, "something")
	}
	// SplitN(":", 3) gives 3 parts max, third part keeps remaining colons.
	if got[2].TrackId != "abc:extra" {
		t.Errorf("4-part URI: trackId got %q want %q", got[2].TrackId, "abc:extra")
	}
	if got[3].TrackId != "" {
		t.Errorf("non-Spotify URI: trackId should be empty, got %q", got[3].TrackId)
	}
}

func TestProjectQueue_PopulatesMetadataFieldsAndConvertsImageUrl(t *testing.T) {
	t.Parallel()

	// image_url passes through convertSpotifyImageUrl, rest of fields direct
	tracks := []*connectpb.ProvidedTrack{
		{
			Uri: "spotify:track:song1",
			Metadata: map[string]string{
				"title":       "Test Track",
				"artist_name": "Test Artist",
				"album_title": "Test Album",
				"image_url":   "spotify:image:ab67616d00001e02deadbeef",
			},
		},
	}

	got := projectQueue(tracks, 10)
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	q := got[0]
	if q.Name != "Test Track" {
		t.Errorf("Name: got %q want %q", q.Name, "Test Track")
	}
	if q.Artist != "Test Artist" {
		t.Errorf("Artist: got %q want %q", q.Artist, "Test Artist")
	}
	if q.Album != "Test Album" {
		t.Errorf("Album: got %q want %q", q.Album, "Test Album")
	}
	if q.ImageUrl != "https://i.scdn.co/image/ab67616d00001e02deadbeef" {
		t.Errorf("ImageUrl not converted via convertSpotifyImageUrl: got %q", q.ImageUrl)
	}
}

func TestProjectQueue_NilMetadataAndEmptyImageUrlAreSafe(t *testing.T) {
	t.Parallel()

	// nil/empty Metadata or empty image_url -> ImageUrl stays empty (not "https://")
	tracks := []*connectpb.ProvidedTrack{
		{Uri: "spotify:track:a"},                                               // Metadata is nil
		{Uri: "spotify:track:b", Metadata: map[string]string{}},                // empty map
		{Uri: "spotify:track:c", Metadata: map[string]string{"image_url": ""}}, // empty image_url
	}

	got := projectQueue(tracks, 10)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	for i, q := range got {
		if q.ImageUrl != "" {
			t.Errorf("entry[%d] ImageUrl should be empty (no conversion of blank input), got %q",
				i, q.ImageUrl)
		}
		if q.Name != "" {
			t.Errorf("entry[%d] Name: got %q want empty", i, q.Name)
		}
	}
}

// RemotePosition must not compare the local clock against the server sidetime
func TestRemotePosition_ClockSkewImmune(t *testing.T) {
	t.Parallel()

	rs := &RemoteState{
		PositionAsOfTimestamp: 10_000,
		Timestamp:             time.Now().Add(5 * time.Hour).UnixMilli(),
		IsPlaying:             true,
		PlaybackSpeed:         1,
		ReceivedAt:            time.Now().Add(-2 * time.Second),
	}
	if got := rs.RemotePosition(); got < 11_900 || got > 13_000 {
		t.Errorf("RemotePosition with skewed server Timestamp: got %d, want ~12000 (must advance from ReceivedAt)", got)
	}

	rs.ReceivedAt = time.Time{}
	if got := rs.RemotePosition(); got != 10_000 {
		t.Errorf("RemotePosition with zero ReceivedAt: got %d, want 10000", got)
	}

	rs.ReceivedAt = time.Now().Add(-2 * time.Second)
	rs.IsPaused = true
	if got := rs.RemotePosition(); got != 10_000 {
		t.Errorf("RemotePosition while paused: got %d, want 10000", got)
	}

	rs.IsPaused = false
	rs.ReceivedAt = time.Now().Add(-11 * time.Minute)
	if got := rs.RemotePosition(); got != 10_000 {
		t.Errorf("RemotePosition with stale snapshot: got %d, want 10000", got)
	}
}

// cluster snapshots can be stale at delivery
func TestRemotePosition_StaleSnapshotAged(t *testing.T) {
	t.Parallel()

	now := time.Now()
	rs := &RemoteState{
		PositionAsOfTimestamp: 5_000,
		Timestamp:             now.UnixMilli() - 100_000,
		ReceivedAt:            now.Add(-1 * time.Second),
		ReceivedAtWallMs:      now.UnixMilli() - 1_000,
		clockOffsetMs:         0,
		offsetKnown:           true,
		IsPlaying:             true,
		PlaybackSpeed:         1,
	}
	if got := rs.RemotePosition(); got < 104_000 || got > 106_500 {
		t.Errorf("stale snapshot: got %d, want ~105000 (age must be added back)", got)
	}
}

// The age correction must cancel clock skew instead of trusting clock
func TestRemotePosition_StaleSnapshotAgedUnderSkew(t *testing.T) {
	t.Parallel()

	now := time.Now()
	const skew = int64(-7_200_000) // local clock 2h behind server
	rs := &RemoteState{
		PositionAsOfTimestamp: 5_000,
		Timestamp:             now.UnixMilli() - skew - 50_000,
		ReceivedAt:            now.Add(-1 * time.Second),
		ReceivedAtWallMs:      now.UnixMilli() - 1_000,
		clockOffsetMs:         skew,
		offsetKnown:           true,
		IsPlaying:             true,
		PlaybackSpeed:         1,
	}
	if got := rs.RemotePosition(); got < 54_000 || got > 56_500 {
		t.Errorf("stale snapshot under skew: got %d, want ~55000", got)
	}
}

func TestClockOffsetEstimator(t *testing.T) {
	t.Parallel()

	var e clockOffsetEstimator
	if _, ok := e.offset(); ok {
		t.Fatal("empty estimator must report unknown")
	}
	e.add(-7_200_000 + 90_000)
	e.add(-7_200_000 + 300)
	e.add(-7_200_000 + 400)
	if off, ok := e.offset(); !ok || off != -7_199_700 {
		t.Errorf("offset: got %d ok=%v, want -7199700", off, ok)
	}
	for i := 0; i < clockOffsetWindow; i++ {
		e.add(1_000)
	}
	if off, _ := e.offset(); off != 1_000 {
		t.Errorf("offset after window slide: got %d, want 1000", off)
	}
}

// on a synced clock the new position formula must produce identical results to what we had before
func TestRemotePosition_MatchesLegacyOnSyncedClock(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(20260720))
	for i := 0; i < 25_000; i++ {
		age := rng.Int63n(8 * 60 * 1000)
		sinceRecv := rng.Int63n(90 * 1000)
		base := rng.Int63n(300_000)

		now := time.Now()
		recvAt := now.Add(-time.Duration(sinceRecv) * time.Millisecond)
		serverTs := recvAt.UnixMilli() - age

		rs := &RemoteState{
			PositionAsOfTimestamp: base,
			Timestamp:             serverTs,
			ReceivedAt:            recvAt,
			ReceivedAtWallMs:      recvAt.UnixMilli(),
			clockOffsetMs:         0,
			offsetKnown:           true,
			IsPlaying:             true,
			PlaybackSpeed:         1,
		}

		legacy := base + (time.Now().UnixMilli() - serverTs)
		got := rs.RemotePosition()
		if d := got - legacy; d < -25 || d > 25 {
			t.Fatalf("case %d (age=%dms sinceRecv=%dms base=%d): new=%d legacy=%d diff=%d",
				i, age, sinceRecv, base, got, legacy, d)
		}
	}
}

func TestClockOffsetEstimator_FlushesOnConfirmedClockJump(t *testing.T) {
	t.Parallel()

	var e clockOffsetEstimator
	e.add(-4 * 3600 * 1000)
	e.add(250)
	e.add(300)
	if off, ok := e.offset(); !ok || off != 250 {
		t.Errorf("after confirmed forward jump: got %d ok=%v, want 250", off, ok)
	}

	e2 := clockOffsetEstimator{}
	e2.add(500)
	e2.add(-3_600_000)
	e2.add(-3_599_000)
	if off, _ := e2.offset(); off != -3_600_000 {
		t.Errorf("after confirmed backward jump: got %d, want -3600000", off)
	}
}

func TestClockOffsetEstimator_IgnoresLoneStaleSnapshot(t *testing.T) {
	t.Parallel()

	var e clockOffsetEstimator
	e.add(0)
	e.add(120)
	e.add(91_000) // stale snapshot, not a clock step
	if off, ok := e.offset(); !ok || off != 0 {
		t.Errorf("after lone stale sample: got %d ok=%v, want 0 (outlier parked)", off, ok)
	}
	e.add(80) // next fresh sample clears the pending outlier
	if off, _ := e.offset(); off != 0 {
		t.Errorf("after recovery: got %d, want 0", off)
	}
	e.add(95_000)
	e.add(94_500) // a second agreeing outlier IS a resync
	if off, _ := e.offset(); off != 94_500 {
		t.Errorf("after confirmed resync: got %d, want 94500 (min of the pair)", off)
	}
}

func TestNoteClusterTiming_RedeliveredSnapshotSampledOnce(t *testing.T) {
	t.Parallel()

	// the cloud redelivers the SAME snapshot as it ages; two redeliveries
	// <60s apart would otherwise "agree" and fake a confirmed clock resync
	// (the round-2 rewind). One estimator sample per snapshot timestamp.
	p := &AppPlayer{}
	p.clockEst.add(0)
	p.clockEstSeeded = true

	base := int64(1_785_990_000_000)
	mk := func(recv, ts int64) *RemoteState {
		return &RemoteState{Timestamp: ts, ReceivedAtWallMs: recv, ReceivedAt: time.Now()}
	}
	// fresh snapshot, sampled
	p.noteClusterTiming(mk(base+100, base))
	// the same snapshot redelivered at +70s and +110s: both stale, both
	// share ts — must NOT confirm a resync
	p.noteClusterTiming(mk(base+70_000, base))
	p.noteClusterTiming(mk(base+110_000, base))
	if off, _ := p.clockEst.offset(); off != 0 {
		t.Errorf("redelivered snapshot poisoned the offset: got %d, want 0", off)
	}
	// a genuinely new snapshot samples normally
	p.noteClusterTiming(mk(base+120_050, base+120_000))
	if off, _ := p.clockEst.offset(); off != 0 {
		t.Errorf("after fresh snapshot: got %d, want 0", off)
	}
}

func TestClockOffsetEstimator_KeepsNormalJitter(t *testing.T) {
	t.Parallel()

	var e clockOffsetEstimator
	for _, s := range []int64{400, 2_500, 130, 9_000, 700} {
		e.add(s)
	}
	if off, _ := e.offset(); off != 130 {
		t.Errorf("jittery-but-sane window: got %d, want min 130", off)
	}
}
