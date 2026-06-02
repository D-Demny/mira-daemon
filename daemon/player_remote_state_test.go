package daemon

import (
	"testing"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"
)

// clusterToRemoteState protobuf -> wire RemoteState projection tests

func baseCluster() *connectpb.Cluster {
	return &connectpb.Cluster{
		ActiveDeviceId: "device-abc",
		Device: map[string]*connectpb.DeviceInfo{
			"device-abc": {
				Name:       "Test Phone",
				DeviceType: devicespb.DeviceType_SMARTPHONE,
			},
		},
		PlayerState: &connectpb.PlayerState{
			Track: &connectpb.ProvidedTrack{
				Uri: "spotify:track:abc123",
				Metadata: map[string]string{
					"title":       "Test Track",
					"artist_name": "Test Artist",
					"album_title": "Test Album",
					"image_url":   "spotify:image:ab67616d00001e02deadbeef",
				},
			},
			ContextUri:            "spotify:playlist:xyz",
			Duration:              199_160,
			PositionAsOfTimestamp: 61_420,
			Timestamp:             1_700_000_000_000,
			IsPlaying:             true,
			IsPaused:              false,
			PlaybackSpeed:         1.0,
		},
	}
}

func TestClusterToRemoteState_NilInputsReturnNil(t *testing.T) {
	t.Parallel()

	if got := clusterToRemoteState(nil); got != nil {
		t.Errorf("nil cluster: got %+v want nil", got)
	}
	if got := clusterToRemoteState(&connectpb.Cluster{}); got != nil {
		t.Errorf("cluster with nil PlayerState: got %+v want nil", got)
	}
}

func TestClusterToRemoteState_HappyPathActivelyPlaying(t *testing.T) {
	t.Parallel()

	rs := clusterToRemoteState(baseCluster())
	if rs == nil {
		t.Fatal("clusterToRemoteState returned nil for valid input")
	}

	if got, want := rs.DeviceId, "device-abc"; got != want {
		t.Errorf("DeviceId: got %q want %q", got, want)
	}
	if got, want := rs.DeviceName, "Test Phone"; got != want {
		t.Errorf("DeviceName: got %q want %q", got, want)
	}
	if got, want := rs.DeviceType, "SMARTPHONE"; got != want {
		t.Errorf("DeviceType: got %q want %q", got, want)
	}
	if got, want := rs.TrackUri, "spotify:track:abc123"; got != want {
		t.Errorf("TrackUri: got %q want %q", got, want)
	}
	if got, want := rs.TrackName, "Test Track"; got != want {
		t.Errorf("TrackName: got %q want %q", got, want)
	}
	if got, want := rs.ContextUri, "spotify:playlist:xyz"; got != want {
		t.Errorf("ContextUri: got %q want %q", got, want)
	}
	if got, want := rs.Duration, int64(199_160); got != want {
		t.Errorf("Duration: got %d want %d", got, want)
	}
	if got, want := rs.PositionAsOfTimestamp, int64(61_420); got != want {
		t.Errorf("PositionAsOfTimestamp: got %d want %d", got, want)
	}
	// The defensive AND collapses IsPlaying=true && IsPaused=false is true.
	if !rs.IsPlaying {
		t.Errorf("IsPlaying should be true when ps.IsPlaying && !ps.IsPaused")
	}
	if rs.IsPaused {
		t.Errorf("IsPaused should be false on the happy path")
	}
}

func TestClusterToRemoteState_PausedCollapsesIsPlayingFalse(t *testing.T) {
	t.Parallel()

	c := baseCluster()
	c.PlayerState.IsPlaying = false
	c.PlayerState.IsPaused = true

	rs := clusterToRemoteState(c)
	if rs.IsPlaying {
		t.Errorf("IsPlaying should be false when IsPaused=true")
	}
	if !rs.IsPaused {
		t.Errorf("IsPaused should be true on the wire")
	}
}

func TestClusterToRemoteState_ContradictoryPlayPauseFallsToNotPlaying(t *testing.T) {
	t.Parallel()

	c := baseCluster()
	c.PlayerState.IsPlaying = true
	c.PlayerState.IsPaused = true

	rs := clusterToRemoteState(c)
	if rs.IsPlaying {
		t.Errorf("contradictory input: IsPlaying must collapse to false, got true")
	}
	if !rs.IsPaused {
		t.Errorf("IsPaused passes through verbatim, expected true")
	}
}

func TestClusterToRemoteState_EmptyActiveDeviceIdProducesEmptyDeviceFields(t *testing.T) {
	t.Parallel()

	// regression: progress bar keeps moving when Spotify quits
	c := baseCluster()
	c.ActiveDeviceId = ""
	c.Device = nil // no lookup possible anyway

	rs := clusterToRemoteState(c)
	if rs == nil {
		t.Fatal("empty ActiveDeviceId should NOT return nil; should return state with empty device fields")
	}
	if rs.DeviceId != "" || rs.DeviceName != "" || rs.DeviceType != "" {
		t.Errorf("device fields should be empty strings: got Id=%q Name=%q Type=%q",
			rs.DeviceId, rs.DeviceName, rs.DeviceType)
	}
	// track level fields still populate normally
	if rs.TrackUri == "" {
		t.Errorf("TrackUri should still propagate even with no active device")
	}
}

func TestClusterToRemoteState_ShufflePassThrough(t *testing.T) {
	t.Parallel()

	// regression: shuffle toggle from another device must pass through verbatim
	c := baseCluster()
	c.PlayerState.Options = &connectpb.ContextPlayerOptions{
		ShufflingContext: true,
	}

	rs := clusterToRemoteState(c)
	if !rs.ShuffleContext {
		t.Errorf("ShuffleContext: external shuffle=true must reflect on output")
	}

	// and toggling off propagates too
	c.PlayerState.Options.ShufflingContext = false
	rs = clusterToRemoteState(c)
	if rs.ShuffleContext {
		t.Errorf("ShuffleContext: external shuffle=false must reflect on output")
	}

	// nil Options doesn't crash
	c.PlayerState.Options = nil
	rs = clusterToRemoteState(c)
	if rs.ShuffleContext {
		t.Errorf("ShuffleContext: nil Options should yield false, got true")
	}
}

func TestClusterToRemoteState_RepeatFlagsPassThroughIndependently(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		repeatingContext bool
		repeatingTrack   bool
		wantContext      bool
		wantTrack        bool
	}{
		{"both_off", false, false, false, false},
		{"context_only", true, false, true, false},
		{"track_only", false, true, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := baseCluster()
			c.PlayerState.Options = &connectpb.ContextPlayerOptions{
				RepeatingContext: tt.repeatingContext,
				RepeatingTrack:   tt.repeatingTrack,
			}

			rs := clusterToRemoteState(c)
			if rs.RepeatContext != tt.wantContext {
				t.Errorf("RepeatContext: got %v want %v", rs.RepeatContext, tt.wantContext)
			}
			if rs.RepeatTrack != tt.wantTrack {
				t.Errorf("RepeatTrack: got %v want %v", rs.RepeatTrack, tt.wantTrack)
			}
		})
	}
}

func TestClusterToRemoteState_RestrictionsArraysDeriveDisallowBooleans(t *testing.T) {
	t.Parallel()

	// Case 1: nil Restrictions
	c := baseCluster()
	c.PlayerState.Restrictions = nil
	rs := clusterToRemoteState(c)
	if rs.DisallowSkipPrev || rs.DisallowSkipNext || rs.DisallowSeek {
		t.Errorf("nil Restrictions should yield all-false disallow flags: %+v", rs)
	}

	// Case 2: empty arrays
	c.PlayerState.Restrictions = &connectpb.Restrictions{}
	rs = clusterToRemoteState(c)
	if rs.DisallowSkipPrev || rs.DisallowSkipNext || rs.DisallowSeek {
		t.Errorf("empty Restrictions arrays should yield all-false flags: %+v", rs)
	}

	// Case 3: non-empty arrays and corresponding flag true
	c.PlayerState.Restrictions = &connectpb.Restrictions{
		DisallowSkippingPrevReasons: []string{"no_prev_track"},
		DisallowSkippingNextReasons: []string{},
		DisallowSeekingReasons:      []string{"endless_context"},
	}
	rs = clusterToRemoteState(c)
	if !rs.DisallowSkipPrev {
		t.Errorf("DisallowSkipPrev: should be true for non-empty reasons array")
	}
	if rs.DisallowSkipNext {
		t.Errorf("DisallowSkipNext: should be false for empty array")
	}
	if !rs.DisallowSeek {
		t.Errorf("DisallowSeek: should be true for non-empty reasons array")
	}
}

func TestClusterToRemoteState_MissingTrackMetadataLeavesTextFieldsEmpty(t *testing.T) {
	t.Parallel()

	// nil Metadata map after device handshake, must not panic
	c := baseCluster()
	c.PlayerState.Track = &connectpb.ProvidedTrack{
		Uri: "spotify:track:metaless",
		// Metadata: nil
	}

	rs := clusterToRemoteState(c)
	if got, want := rs.TrackUri, "spotify:track:metaless"; got != want {
		t.Errorf("TrackUri should still propagate: got %q want %q", got, want)
	}
	if rs.TrackName != "" || rs.TrackImageUrl != "" {
		t.Errorf("text fields should be empty when Metadata is nil: name=%q image=%q",
			rs.TrackName, rs.TrackImageUrl)
	}
	if rs.RawMetadata != nil {
		t.Errorf("RawMetadata should stay nil when Metadata was nil, got %v", rs.RawMetadata)
	}
}

func TestClusterToRemoteState_ImageUrlPassesThroughConvertHelper(t *testing.T) {
	t.Parallel()

	// image_url wired through convertSpotifyImageUrl
	c := baseCluster()
	c.PlayerState.Track.Metadata["image_url"] = "spotify:image:fe571a85"

	rs := clusterToRemoteState(c)
	if got, want := rs.TrackImageUrl, "https://i.scdn.co/image/fe571a85"; got != want {
		t.Errorf("TrackImageUrl should pass through convertSpotifyImageUrl: got %q want %q", got, want)
	}
}

func TestClusterToRemoteState_QueueTracksProjectedViaProjectQueue(t *testing.T) {
	t.Parallel()

	// prev/next tracks wired through projectQueue with QueueLimit cap
	c := baseCluster()
	c.PlayerState.NextTracks = make([]*connectpb.ProvidedTrack, 30)
	for i := range c.PlayerState.NextTracks {
		c.PlayerState.NextTracks[i] = &connectpb.ProvidedTrack{
			Uri: "spotify:track:next",
		}
	}

	rs := clusterToRemoteState(c)
	if len(rs.NextTracks) > QueueLimit {
		t.Errorf("NextTracks: got %d entries, must be capped at QueueLimit (%d)",
			len(rs.NextTracks), QueueLimit)
	}
	// PrevTracks is empty in baseCluster, should round-trip as nil/empty.
	if len(rs.PrevTracks) != 0 {
		t.Errorf("PrevTracks: expected empty, got %d entries", len(rs.PrevTracks))
	}
}
