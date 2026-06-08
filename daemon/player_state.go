package daemon

import (
	"context"
	"strings"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/dealer"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

type State struct {
	active      bool
	activeSince time.Time

	device *connectpb.DeviceInfo
	player *connectpb.PlayerState

	queueID uint64

	lastCommand           *dealer.RequestPayload
	lastTransferTimestamp int64

	// remoteState tracks what is playing on another device when in observer mode.
	remoteState *RemoteState

	// connectDevices is the latest snapshot of selectable Connect devices
	connectDevices []ConnectDevice
	// connectDevSig identical per-second clusters dont re-emit events
	connectDevSig string
}

// RemoteState holds information about the playback state of the currently active remote device.
// This is populated from ClusterUpdate messages
type RemoteState struct {
	// DeviceId of the active device.
	DeviceId string
	// DeviceName of the active device (human readable).
	DeviceName string
	// DeviceType of the active device (e.g. "COMPUTER", "SMARTPHONE").
	DeviceType string

	// TrackUri is the Spotify URI of the current track (e.g. "spotify:track:xxx").
	TrackUri string
	// TrackName from the ProvidedTrack metadata.
	TrackName string
	// TrackArtist from the ProvidedTrack metadata.
	TrackArtist string
	// TrackAlbum from the ProvidedTrack metadata.
	TrackAlbum string
	// TrackImageUrl from the ProvidedTrack metadata.
	TrackImageUrl string

	// ContextUri is what context is playing (playlist, album, etc).
	ContextUri string
	// ContextName playlist title etc, may be empty.
	ContextName string

	// Duration of the current track in milliseconds.
	Duration int64
	// PositionAsOfTimestamp is the playback position at the time of Timestamp.
	PositionAsOfTimestamp int64
	// Timestamp is the server-side unix timestamp (ms) when position was captured.
	Timestamp int64
	// IsPlaying indicates whether the remote device is actively playing.
	IsPlaying bool
	// IsPaused indicates whether the remote device is paused.
	IsPaused bool
	// PlaybackSpeed is the playback speed (0 when paused, 1 when playing).
	PlaybackSpeed float64

	// Volume of the active device, 0..MaxStateVolume (65535). 0 if unknown.
	Volume uint32
	// VolumeDisabled is true when the active device doesn't accept remote volume changes (usually phones with their own speaker)
	VolumeDisabled bool
	// VolumeSteps is the devicess advertised volume granularitys
	VolumeSteps int32

	// ShuffleContext indicates whether shuffle is enabled.
	ShuffleContext bool
	// RepeatContext indicates whether repeat context is enabled.
	RepeatContext bool
	// RepeatTrack indicates whether repeat track is enabled.
	RepeatTrack bool

	// from PlayerState.Restrictions.DisallowSkipping*Reasons
	DisallowSkipPrev bool
	DisallowSkipNext bool
	DisallowSeek     bool

	// surrounding queue capped at QueueLimit per direction, for art/lyrics prefetch
	PrevTracks []QueueTrack
	NextTracks []QueueTrack

	// RawMetadata contains all metadata keys from the ProvidedTrack for debugging
	RawMetadata map[string]string
}

// lightweight projection of ProvidedTrack, just what the UI needs for thumbnails + prefetch
type QueueTrack struct {
	Uri      string `json:"uri"`
	TrackId  string `json:"track_id"`
	Name     string `json:"name"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	ImageUrl string `json:"image_url"`
}

// max entries per direction, UI only prefetches a handful
const QueueLimit = 10

// projects ProvidedTracks to QueueTracks, skips entries with no URI
func projectQueue(tracks []*connectpb.ProvidedTrack, limit int) []QueueTrack {
	if len(tracks) == 0 {
		return nil
	}
	out := make([]QueueTrack, 0, min(len(tracks), limit))
	for _, t := range tracks {
		if t == nil || t.Uri == "" {
			continue
		}
		if len(out) >= limit {
			break
		}
		var name, artist, album, imageUrl string
		if t.Metadata != nil {
			name = t.Metadata["title"]
			artist = t.Metadata["artist_name"]
			album = t.Metadata["album_title"]
			if img, ok := t.Metadata["image_url"]; ok && img != "" {
				imageUrl = convertSpotifyImageUrl(img)
			}
		}
		trackId := ""
		if parts := strings.SplitN(t.Uri, ":", 3); len(parts) == 3 {
			trackId = parts[2]
		}
		out = append(out, QueueTrack{
			Uri:      t.Uri,
			TrackId:  trackId,
			Name:     name,
			Artist:   artist,
			Album:    album,
			ImageUrl: imageUrl,
		})
	}
	return out
}

// Set the IsPaused flag
func (s *State) setPaused(val bool) {
	s.player.IsPaused = val
	if val {
		s.player.PlaybackSpeed = 0
	} else {
		s.player.PlaybackSpeed = 1
	}
}

func (s *State) setActive(val bool) {
	if val {
		if s.active {
			return
		}

		s.active = true
		s.activeSince = time.Now()
	} else {
		s.active = false
		s.activeSince = time.Time{}
	}
}

func (s *State) reset() {
	s.active = false
	s.activeSince = time.Time{}
	s.player = &connectpb.PlayerState{
		IsSystemInitiated: true,
		PlaybackSpeed:     1,
		PlayOrigin:        &connectpb.PlayOrigin{},
		Suppressions:      &connectpb.Suppressions{},
		Options:           &connectpb.ContextPlayerOptions{},
	}
}

func (s *State) trackPosition() int64 {
	// If paused or not actually playing, use raw position value
	if s.player.IsPaused || !s.player.IsPlaying {
		return s.player.PositionAsOfTimestamp
	}

	// Calculate dynamic position only if playback is actually active
	now := time.Now().UnixMilli()
	elapsed := now - s.player.Timestamp

	// Validate timestamp freshness
	const maxReasonableElapsed = 10 * 60 * 1000 // 10 minutes in milliseconds
	if elapsed > maxReasonableElapsed || elapsed < 0 {
		return s.player.PositionAsOfTimestamp
	}

	calculated := s.player.PositionAsOfTimestamp + elapsed
	// clamp negative positions, defensive against clock skew
	if calculated < 0 {
		return s.player.PositionAsOfTimestamp
	}

	return calculated
}

// Update timestamp, and updating the player position timestamp
func (s *State) updateTimestamp() {
	now := time.Now()

	// How many milliseconds the playback has advanced
	advancedTimeMillis := now.UnixMilli() - s.player.Timestamp

	// How far the playback position has advanced during that time
	advancedPositionMillis := int64(float64(advancedTimeMillis) * s.player.PlaybackSpeed)

	// Update the timestamps accordingly.
	s.player.PositionAsOfTimestamp += advancedPositionMillis
	s.player.Timestamp = now.UnixMilli()
}

func (s *State) playOrigin() string {
	return s.player.PlayOrigin.FeatureIdentifier
}

func (p *AppPlayer) initState() {
	canBePlayer := !p.app.cfg.ObserverMode

	p.state = &State{
		lastCommand: nil,
		device: &connectpb.DeviceInfo{
			CanPlay:               canBePlayer,
			Volume:                MaxStateVolume,
			Name:                  p.app.cfg.DeviceName,
			DeviceId:              p.app.deviceId,
			DeviceType:            p.app.deviceType,
			DeviceSoftwareVersion: librespot.VersionString(),
			ClientId:              librespot.ClientIdHex,
			SpircVersion:          "3.2.6",
			Capabilities: &connectpb.Capabilities{
				CanBePlayer:                canBePlayer,
				RestrictToLocal:            false,
				GaiaEqConnectId:            true,
				SupportsLogout:             false,
				IsObservable:               true,
				VolumeSteps:                100,
				SupportedTypes:             []string{"audio/track", "audio/episode"},
				CommandAcks:                true,
				SupportsRename:             false,
				Hidden:                     p.app.cfg.ObserverMode,
				DisableVolume:              p.app.cfg.ObserverMode,
				ConnectDisabled:            false,
				SupportsPlaylistV2:         true,
				IsControllable:             !p.app.cfg.ObserverMode,
				SupportsExternalEpisodes:   false, // TODO: support external episodes
				SupportsSetBackendMetadata: true,
				SupportsTransferCommand:    !p.app.cfg.ObserverMode,
				SupportsCommandRequest:     true,
				IsVoiceEnabled:             false,
				NeedsFullPlayerState:       false,
				SupportsGzipPushes:         true,
				SupportsSetOptionsCommand:  true,
				SupportsHifi:               nil, // TODO: nice to have?
				ConnectCapabilities:        "",
			},
		},
	}
	p.state.reset()
}

// RemotePosition computes the current playback position of the remote device
func (rs *RemoteState) RemotePosition() int64 {
	if rs == nil {
		return 0
	}
	if rs.IsPaused || !rs.IsPlaying {
		return rs.PositionAsOfTimestamp
	}

	now := time.Now().UnixMilli()
	elapsed := now - rs.Timestamp
	if elapsed < 0 || elapsed > 10*60*1000 {
		return rs.PositionAsOfTimestamp
	}

	return rs.PositionAsOfTimestamp + int64(float64(elapsed)*rs.PlaybackSpeed)
}

func (p *AppPlayer) updateState(ctx context.Context) {
	if err := p.putConnectState(ctx, p.spotConnId, connectpb.PutStateReason_PLAYER_STATE_CHANGED); err != nil {
		p.app.log.Errorf("failed put state after update: %v", err)
	}
}

// putConnectState takes connId so it can be called safely from a goroutine
func (p *AppPlayer) putConnectState(ctx context.Context, connId string, reason connectpb.PutStateReason) error {
	if reason == connectpb.PutStateReason_BECAME_INACTIVE {
		return p.sess.Spclient().PutConnectStateInactive(ctx, connId, false)
	}

	putStateReq := &connectpb.PutStateRequest{
		ClientSideTimestamp: uint64(time.Now().UnixMilli()),
		MemberType:          connectpb.MemberType_CONNECT_STATE,
		PutStateReason:      reason,
	}

	if t := p.state.activeSince; !t.IsZero() {
		putStateReq.StartedPlayingAt = uint64(t.UnixMilli())
	}

	putStateReq.IsActive = p.state.active
	putStateReq.Device = &connectpb.Device{
		DeviceInfo:  p.state.device,
		PlayerState: p.state.player,
	}

	if p.state.lastCommand != nil {
		putStateReq.LastCommandMessageId = p.state.lastCommand.MessageId
		putStateReq.LastCommandSentByDeviceId = p.state.lastCommand.SentByDeviceId
	}

	// send the state update
	return p.sess.Spclient().PutConnectState(ctx, connId, putStateReq)
}
