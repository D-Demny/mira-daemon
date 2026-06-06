package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
	"github.com/devgianlu/go-librespot/dealer"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/devgianlu/go-librespot/session"
)

// MaxStateVolume is the maximum volume value used in Spotify connect state
const MaxStateVolume = 65535

type AppPlayer struct {
	app  *App
	sess *session.Session

	stop   chan struct{}
	logout chan *AppPlayer

	spotConnId string

	prodInfo    *ProductInfo
	countryCode *string

	hasSpotConnId          bool
	hasInitialConnectState bool
	hasCountryCode         bool
	playbackReadyCh        chan struct{}
	playbackReadyOnce      sync.Once

	state *State

	prefetchTimer *time.Timer

	// lyricsProvider handles fetching lyrics from primary + LRCLIB sources
	lyricsProvider *LyricsProvider

	// queueResolver fills in artist/album names for queue entries
	queueResolver   *queueResolver
	queueResolvedCh chan struct{}
}

func (p *AppPlayer) playbackReady() bool {
	select {
	case <-p.playbackReadyCh:
		return true
	default:
		return false
	}
}

func (p *AppPlayer) notifyPlaybackReadyIfNeeded() {
	if !p.hasSpotConnId || !p.hasInitialConnectState || !p.hasCountryCode {
		return
	}

	p.playbackReadyOnce.Do(func() {
		close(p.playbackReadyCh)
		p.app.server.Emit(&ApiEvent{Type: ApiEventTypePlaybackReady})
	})
}

func (p *AppPlayer) handleAccesspointPacket(pktType ap.PacketType, payload []byte) error {
	switch pktType {
	case ap.PacketTypeProductInfo:
		var prod ProductInfo
		if err := xml.Unmarshal(payload, &prod); err != nil {
			return fmt.Errorf("failed umarshalling ProductInfo: %w", err)
		}

		if len(prod.Products) != 1 {
			return fmt.Errorf("invalid ProductInfo")
		}

		p.prodInfo = &prod
		return nil
	case ap.PacketTypeCountryCode:
		*p.countryCode = string(payload)
		p.hasCountryCode = true
		p.notifyPlaybackReadyIfNeeded()
		return nil
	default:
		return nil
	}
}

func (p *AppPlayer) handleDealerMessage(ctx context.Context, msg dealer.Message) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if strings.HasPrefix(msg.Uri, "hm://pusher/v1/connections/") {
		p.spotConnId = msg.Headers["Spotify-Connection-Id"]
		p.hasSpotConnId = p.spotConnId != ""
		p.app.log.Debugf("received connection id: %s...%s", p.spotConnId[:16], p.spotConnId[len(p.spotConnId)-16:])

		if err := p.putConnectState(ctx, connectpb.PutStateReason_NEW_DEVICE); err != nil {
			return fmt.Errorf("failed initial state put: %w", err)
		}

		p.hasInitialConnectState = true
		p.notifyPlaybackReadyIfNeeded()
		return nil
	} else if strings.HasPrefix(msg.Uri, "hm://connect-state/v1/cluster") {
		var clusterUpdate connectpb.ClusterUpdate
		if err := proto.Unmarshal(msg.Payload, &clusterUpdate); err != nil {
			return fmt.Errorf("failed unmarshalling ClusterUpdate: %w", err)
		}

		cluster := clusterUpdate.Cluster
		activeDeviceId := cluster.ActiveDeviceId
		isUs := activeDeviceId == p.app.deviceId

		if !isUs && cluster.PlayerState != nil {
			p.updateRemoteState(ctx, cluster)
		}

		return nil
	}

	p.app.log.Debugf("skipping dealer message, uri: %s", msg.Uri)
	return nil
}

func (p *AppPlayer) handleDealerRequest(ctx context.Context, req dealer.Request) error {
	// Observer mode: reject all player commands
	p.app.log.Debugf("observer mode: rejecting player command %s from %s",
		req.Payload.Command.Endpoint, req.Payload.SentByDeviceId)
	return nil
}

// updateRemoteState pulls active device state from a cluster + stores it
func clusterToRemoteState(cluster *connectpb.Cluster) *RemoteState {
	if cluster == nil || cluster.PlayerState == nil {
		return nil
	}

	ps := cluster.PlayerState
	track := ps.Track

	activeDeviceId := cluster.ActiveDeviceId
	var deviceName, deviceType string
	var volume uint32
	var volumeDisabled bool
	var volumeSteps int32
	if dev, ok := cluster.Device[activeDeviceId]; ok {
		deviceName = dev.Name
		deviceType = dev.DeviceType.String()
		volume = dev.Volume
		if dev.Capabilities != nil {
			volumeDisabled = dev.Capabilities.DisableVolume
			volumeSteps = dev.Capabilities.VolumeSteps
		}
	}

	trackUri := ""
	trackName := ""
	imageUrl := ""
	contextUri := ""
	var rawMeta map[string]string

	if track != nil {
		trackUri = track.Uri
		if track.Metadata != nil {
			trackName = track.Metadata["title"]
			rawMeta = track.Metadata

			if img, ok := track.Metadata["image_url"]; ok {
				imageUrl = convertSpotifyImageUrl(img)
			}
		}
	}

	if ps.ContextUri != "" {
		contextUri = ps.ContextUri
	}

	var contextName string
	if ps.ContextMetadata != nil {
		contextName = ps.ContextMetadata["context_description"]
	}

	return &RemoteState{
		DeviceId:              activeDeviceId,
		DeviceName:            deviceName,
		DeviceType:            deviceType,
		TrackUri:              trackUri,
		TrackName:             trackName,
		TrackImageUrl:         imageUrl,
		ContextUri:            contextUri,
		ContextName:           contextName,
		Duration:              int64(ps.Duration),
		PositionAsOfTimestamp: ps.PositionAsOfTimestamp,
		Timestamp:             ps.Timestamp,
		IsPlaying:             !ps.IsPaused && ps.IsPlaying,
		IsPaused:              ps.IsPaused,
		PlaybackSpeed:         ps.PlaybackSpeed,
		Volume:                volume,
		VolumeDisabled:        volumeDisabled,
		VolumeSteps:           volumeSteps,
		ShuffleContext:        ps.Options != nil && ps.Options.ShufflingContext,
		RepeatContext:         ps.Options != nil && ps.Options.RepeatingContext,
		RepeatTrack:           ps.Options != nil && ps.Options.RepeatingTrack,
		DisallowSkipPrev: ps.Restrictions != nil && len(ps.Restrictions.DisallowSkippingPrevReasons) > 0,
		DisallowSkipNext: ps.Restrictions != nil && len(ps.Restrictions.DisallowSkippingNextReasons) > 0,
		DisallowSeek:     ps.Restrictions != nil && len(ps.Restrictions.DisallowSeekingReasons) > 0,
		PrevTracks:       projectQueue(ps.PrevTracks, QueueLimit),
		NextTracks:       projectQueue(ps.NextTracks, QueueLimit),
		RawMetadata:      rawMeta,
	}
}

func (p *AppPlayer) updateRemoteState(ctx context.Context, cluster *connectpb.Cluster) {
	rs := clusterToRemoteState(cluster)
	if rs == nil {
		return
	}
	track := cluster.PlayerState.Track

	// Fill in any cached artist/album for the queue entries
	if p.queueResolver != nil {
		needNext := p.queueResolver.applyCache(rs.NextTracks)
		needPrev := p.queueResolver.applyCache(rs.PrevTracks)
		p.queueResolver.ResolveAsync(append(needNext, needPrev...))
	}

	// Resolve artist and album from track metadata or spclient
	artistName := ""
	albumName := ""
	if track != nil && track.Metadata != nil {
		artistName = track.Metadata["artist_name"]
		albumName = track.Metadata["album_title"]
	}

	if artistName == "" && rs.TrackUri != "" {
		spotId, err := librespot.SpotifyIdFromUri(rs.TrackUri)
		if err == nil && spotId != nil {
			prevState := p.state.remoteState
			if prevState != nil && prevState.TrackUri == rs.TrackUri && prevState.TrackArtist != "" {
				artistName = prevState.TrackArtist
				albumName = prevState.TrackAlbum
			} else {
				artistName, albumName = p.resolveTrackMetadata(ctx, *spotId)
			}
		}
	}

	rs.TrackArtist = artistName
	rs.TrackAlbum = firstNonEmpty(albumName, rs.RawMetadata["album_title"])

	prevState := p.state.remoteState
	trackChanged := prevState == nil || prevState.TrackUri != rs.TrackUri

	p.state.remoteState = rs

	if trackChanged {
		p.app.log.Debugf("observer: track changed to %q by %s on %s", rs.TrackName, rs.TrackArtist, rs.DeviceName)
		p.app.server.Emit(&ApiEvent{
			Type: ApiEventTypeObserverTrackChanged,
			Data: rs,
		})
	} else {
		p.app.server.Emit(&ApiEvent{
			Type: ApiEventTypeObserverStateChanged,
			Data: rs,
		})
	}
}

// resolveTrackMetadata tries spclient first (fast), falls back to the web API
// when spclient is unavailable
func (p *AppPlayer) resolveTrackMetadata(ctx context.Context, spotId librespot.SpotifyId) (artist, album string) {
	artist, album = p.resolveViaSpclient(ctx, spotId)
	if artist != "" {
		return
	}

	wbArtist, wbAlbum := p.resolveViaWebApi(ctx, spotId)
	if wbArtist != "" {
		return wbArtist, wbAlbum
	}
	return artist, album
}

func (p *AppPlayer) resolveViaSpclient(ctx context.Context, spotId librespot.SpotifyId) (artist, album string) {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var trackMeta metadatapb.Track
	if err := p.sess.Spclient().ExtendedMetadataSimple(reqCtx, spotId,
		extmetadatapb.ExtensionKind_TRACK_V4, &trackMeta); err != nil {
		p.app.log.Debugf("observer: spclient metadata for %s failed: %v", spotId.Uri(), err)
		return "", ""
	}

	if len(trackMeta.Artist) > 0 && trackMeta.Artist[0].Name != nil {
		artist = *trackMeta.Artist[0].Name
	}
	if trackMeta.Album != nil && trackMeta.Album.Name != nil {
		album = *trackMeta.Album.Name
	}

	p.app.log.Debugf("observer: spclient metadata for %s: artist=%q, album=%q",
		spotId.Uri(), artist, album)
	return
}

func (p *AppPlayer) resolveViaWebApi(ctx context.Context, spotId librespot.SpotifyId) (artist, album string) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := p.sess.WebApi(reqCtx, "GET", "/v1/tracks/"+spotId.Base62(), nil, nil, nil)
	if err != nil {
		p.app.log.Debugf("observer: web api metadata for %s failed: %v", spotId.Uri(), err)
		return "", ""
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.app.log.Debugf("observer: web api metadata for %s returned status %d", spotId.Uri(), resp.StatusCode)
		return "", ""
	}

	var data struct {
		Artists []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Album struct {
			Name string `json:"name"`
		} `json:"album"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		p.app.log.Debugf("observer: web api metadata for %s decode failed: %v", spotId.Uri(), err)
		return "", ""
	}

	if len(data.Artists) > 0 {
		artist = data.Artists[0].Name
	}
	album = data.Album.Name

	p.app.log.Debugf("observer: web api metadata for %s: artist=%q, album=%q",
		spotId.Uri(), artist, album)
	return
}

func (p *AppPlayer) handleApiRequest(ctx context.Context, req ApiRequest) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch req.Type {
	case ApiRequestTypeRoot:
		return &ApiResponseRoot{PlaybackReady: p.playbackReady()}, nil

	case ApiRequestTypeWebApi:
		data := req.Data.(ApiRequestDataWebApi)
		resp, err := p.sess.WebApi(ctx, data.Method, data.Path, data.Query, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to send web api request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		switch resp.StatusCode {
		case 400:
			return nil, ErrBadRequest
		case 403:
			return nil, ErrForbidden
		case 404:
			return nil, ErrNotFound
		case 405:
			return nil, ErrMethodNotAllowed
		case 429:
			return nil, ErrTooManyRequests
		}

		if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, fmt.Errorf("failed to read response body: %w", err)
			}
			return respBody, nil
		}

		var respJson any
		if err = json.NewDecoder(resp.Body).Decode(&respJson); err != nil {
			return nil, fmt.Errorf("failed to decode response body: %w", err)
		}
		return respJson, nil

	case ApiRequestTypeStatus:
		resp := &ApiResponseStatus{
			Username:   p.sess.Username(),
			DeviceId:   p.app.deviceId,
			DeviceType: p.app.deviceType.String(),
			DeviceName: p.app.cfg.DeviceName,
			Stopped:    true,
			Paused:     false,
		}
		return resp, nil

	case ApiRequestTypeToken:
		accessToken, err := p.sess.Spclient().GetAccessToken(ctx, true)
		if err != nil {
			return nil, fmt.Errorf("failed getting access token: %w", err)
		}
		return &ApiResponseToken{Token: accessToken}, nil

	case ApiRequestTypeObserverStatus:
		if p.state.remoteState == nil {
			return map[string]any{
				"active":  false,
				"message": "no remote device is currently playing",
			}, nil
		}

		rs := p.state.remoteState
		trackId := ""
		if parts := strings.SplitN(rs.TrackUri, ":", 3); len(parts) == 3 {
			trackId = parts[2]
		}
		lyricsUrl := ""
		if trackId != "" {
			lyricsUrl = fmt.Sprintf("/lyrics/%s", trackId)
		}

		return map[string]any{
			"active":         true,
			"device_id":      rs.DeviceId,
			"device_name":    rs.DeviceName,
			"device_type":    rs.DeviceType,
			"track_id":       trackId,
			"track_uri":      rs.TrackUri,
			"track_name":     rs.TrackName,
			"track_artist":   rs.TrackArtist,
			"track_album":    rs.TrackAlbum,
			"track_image":    rs.TrackImageUrl,
			"context_uri":    rs.ContextUri,
			"context_name":   rs.ContextName,
			"duration":       rs.Duration,
			"position":       rs.RemotePosition(),
			"is_playing":     rs.IsPlaying,
			"is_paused":      rs.IsPaused,
			"volume":          rs.Volume,
			"volume_max":      MaxStateVolume,
			"volume_disabled": rs.VolumeDisabled,
			"volume_steps":    rs.VolumeSteps,
			"shuffle":        rs.ShuffleContext,
			"repeat_context": rs.RepeatContext,
			"repeat_track":   rs.RepeatTrack,
			"disallow_prev":  rs.DisallowSkipPrev,
			"disallow_next":  rs.DisallowSkipNext,
			"disallow_seek":  rs.DisallowSeek,
			"prev_tracks":    rs.PrevTracks,
			"next_tracks":    rs.NextTracks,
			"lyrics_url":     lyricsUrl,
			"raw_metadata":   rs.RawMetadata,
		}, nil

	case ApiRequestTypeLyrics:
		data := req.Data.(ApiRequestDataLyrics)
		trackName := data.TrackName
		artistName := data.ArtistName
		albumName := data.AlbumName
		durationMs := data.DurationMs

		if trackName == "" || artistName == "" {
			rs := p.state.remoteState
			if rs != nil {
				if trackName == "" {
					trackName = rs.TrackName
				}
				if artistName == "" {
					artistName = rs.TrackArtist
				}
				if albumName == "" {
					albumName = rs.TrackAlbum
				}
				if durationMs == 0 {
					durationMs = int(rs.Duration)
				}
			}
		}

		if trackName == "" {
			return nil, fmt.Errorf("track name required (provide ?track= param or wait for observer state)")
		}

		result, err := p.lyricsProvider.FetchLyrics(ctx, data.TrackId, trackName, artistName, albumName, durationMs)
		if err != nil {
			if errors.Is(err, ErrNoLyrics) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("lyrics fetch failed: %w", err)
		}
		return result, nil

	case ApiRequestTypeResume:
		return nil, p.sendActiveDeviceCommand(ctx, connectCommand{Endpoint: "resume"})
	case ApiRequestTypePause:
		return nil, p.sendActiveDeviceCommand(ctx, connectCommand{Endpoint: "pause"})
	case ApiRequestTypePlayPause:
		// pick the endpoint from the last known playback state
		endpoint := "resume"
		if rs := p.state.remoteState; rs != nil && rs.IsPlaying && !rs.IsPaused {
			endpoint = "pause"
		}
		return nil, p.sendActiveDeviceCommand(ctx, connectCommand{Endpoint: endpoint})
	case ApiRequestTypeNext:
		return nil, p.sendActiveDeviceCommand(ctx, connectCommand{Endpoint: "skip_next"})
	case ApiRequestTypePrev:
		return nil, p.sendActiveDeviceCommand(ctx, connectCommand{Endpoint: "skip_prev"})
	case ApiRequestTypeSeek:
		data, _ := req.Data.(ApiRequestDataSeek)
		if data.Relative {
			return nil, fmt.Errorf("relative seek not supported in observer mode")
		}
		return nil, p.sendActiveDeviceCommand(ctx, connectCommand{Endpoint: "seek_to", Value: data.Position})
	case ApiRequestTypeSetShufflingContext:
		val, _ := req.Data.(bool)
		return nil, p.sendActiveDeviceCommand(ctx, connectCommand{Endpoint: "set_shuffling_context", Value: val})
	case ApiRequestTypeSetRepeatingContext:
		val, _ := req.Data.(bool)
		return nil, p.sendActiveDeviceCommand(ctx, connectCommand{Endpoint: "set_repeating_context", Value: val})
	case ApiRequestTypeSetRepeatingTrack:
		val, _ := req.Data.(bool)
		return nil, p.sendActiveDeviceCommand(ctx, connectCommand{Endpoint: "set_repeating_track", Value: val})

	case ApiRequestTypeGetVolume:
		rs := p.state.remoteState
		if rs == nil || rs.DeviceId == "" {
			return nil, fmt.Errorf("no active remote device known yet")
		}
		return &ApiResponseVolume{Value: rs.Volume, Max: MaxStateVolume}, nil

	case ApiRequestTypeSetVolume:
		data, _ := req.Data.(ApiRequestDataVolume)
		rs := p.state.remoteState
		if rs == nil || rs.DeviceId == "" {
			return nil, fmt.Errorf("no active remote device known yet")
		}
		target := int64(data.Volume)
		if data.Relative {
			target = int64(rs.Volume) + int64(data.Volume)
		}
		if target < 0 {
			target = 0
		}
		if target > MaxStateVolume {
			target = MaxStateVolume
		}
		// TEMP diagnostic logging while calibrating the volume knob.
		p.app.log.Infof("set_volume: req={vol:%d rel:%v} deviceVol:%d -> target:%d (%s)",
			data.Volume, data.Relative, rs.Volume, target, rs.DeviceName)
		return nil, p.sendActiveDeviceVolume(ctx, target)

	case ApiRequestTypePlay:
		// tell the active device to start a context
		data, _ := req.Data.(ApiRequestDataPlay)
		if data.Uri == "" {
			return nil, fmt.Errorf("play requires a context uri")
		}
		cmd := connectCommand{
			Endpoint: "play",
			Context: &connectContext{
				Uri: data.Uri,
				Url: "context://" + data.Uri,
			},
			Options: &connectOptions{License: "tft"},
			PlayOrigin: &connectOrigin{
				FeatureIdentifier:  "your_library",
				FeatureVersion:     "go-librespot",
				ReferrerIdentifier: "your_library",
			},
			LoggingParams: &connectLogging{
				PageInstanceIds: []string{},
				InteractionIds:  []string{},
				CommandId:       randomCommandId(),
			},
		}
		if data.SkipToUri != "" {
			cmd.Options.SkipTo = connectSkipTo{TrackUri: data.SkipToUri}
		}
		// TEMP diagnostic logging while verifying the play envelope on hardware.
		p.app.log.Infof("play: context=%s skipTo=%q -> active device", data.Uri, data.SkipToUri)
		return nil, p.sendActiveDeviceCommand(ctx, cmd)

	case ApiRequestTypeAddToQueue:
		return nil, fmt.Errorf("not yet available in observer mode")

	default:
		return nil, fmt.Errorf("unknown request type: %s", req.Type)
	}
}

// connectCommand is the JSON shape of a single Spotify Connect remote-control command
type connectCommand struct {
	Endpoint      string          `json:"endpoint"`
	Value         any             `json:"value,omitempty"`
	Context       *connectContext `json:"context,omitempty"`
	Options       *connectOptions `json:"options,omitempty"`
	PlayOrigin    *connectOrigin  `json:"play_origin,omitempty"`
	LoggingParams *connectLogging `json:"logging_params,omitempty"`
}

// connectContext/connectOptions/connectOrigin/connectLogging are the play-command sub-objects
type connectContext struct {
	Uri      string   `json:"uri"`
	Url      string   `json:"url,omitempty"`
	Metadata struct{} `json:"metadata"`
}

type connectOptions struct {
	License               string        `json:"license,omitempty"`
	SkipTo                connectSkipTo `json:"skip_to"`
	PlayerOptionsOverride struct{}      `json:"player_options_override"`
}

type connectSkipTo struct {
	TrackUri string `json:"track_uri,omitempty"`
}

type connectOrigin struct {
	FeatureIdentifier  string `json:"feature_identifier"`
	FeatureVersion     string `json:"feature_version,omitempty"`
	ReferrerIdentifier string `json:"referrer_identifier,omitempty"`
}

type connectLogging struct {
	PageInstanceIds []string `json:"page_instance_ids"`
	InteractionIds  []string `json:"interaction_ids"`
	CommandId       string   `json:"command_id"`
}

// randomCommandId returns a 32-char hex id
func randomCommandId() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

type connectCommandEnvelope struct {
	Command connectCommand `json:"command"`
}

// sendActiveDeviceCommand sends to the active device in the user's cluster
func (p *AppPlayer) sendActiveDeviceCommand(ctx context.Context, cmd connectCommand) error {
	rs := p.state.remoteState
	if rs == nil || rs.DeviceId == "" {
		return fmt.Errorf("no active remote device known yet")
	}
	if rs.DeviceId == p.app.deviceId {
		return fmt.Errorf("active device is us; cannot remote-control self")
	}
	if !p.hasSpotConnId {
		return fmt.Errorf("dealer not connected (no spotify-connection-id)")
	}

	body, err := json.Marshal(connectCommandEnvelope{Command: cmd})
	if err != nil {
		return fmt.Errorf("marshal connect command: %w", err)
	}

	if err := p.sess.Spclient().SendPlayerCommand(ctx, p.app.deviceId, rs.DeviceId, p.spotConnId, body); err != nil {
		return fmt.Errorf("send %s to %s: %w", cmd.Endpoint, rs.DeviceId, err)
	}
	p.app.log.Debugf("observer: sent %s to %s (%s)", cmd.Endpoint, rs.DeviceId, rs.DeviceName)
	return nil
}

// sendActiveDeviceVolume sets the active device's volume using a connect-state endpoint
func (p *AppPlayer) sendActiveDeviceVolume(ctx context.Context, volume int64) error {
	rs := p.state.remoteState
	if rs == nil || rs.DeviceId == "" {
		return fmt.Errorf("no active remote device known yet")
	}
	if rs.DeviceId == p.app.deviceId {
		return fmt.Errorf("active device is us; cannot remote-control self")
	}
	if !p.hasSpotConnId {
		return fmt.Errorf("dealer not connected (no spotify-connection-id)")
	}

	body, err := json.Marshal(map[string]int64{"volume": volume})
	if err != nil {
		return fmt.Errorf("marshal volume: %w", err)
	}

	if err := p.sess.Spclient().SetConnectVolume(ctx, p.app.deviceId, rs.DeviceId, p.spotConnId, body); err != nil {
		return fmt.Errorf("set volume on %s: %w", rs.DeviceId, err)
	}
	p.app.log.Debugf("observer: set volume %d on %s (%s)", volume, rs.DeviceId, rs.DeviceName)
	return nil
}

func convertSpotifyImageUrl(s string) string {
	if strings.HasPrefix(s, "spotify:image:") {
		return "https://i.scdn.co/image/" + strings.TrimPrefix(s, "spotify:image:")
	}
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		return s
	}
	return "https://" + s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (p *AppPlayer) Close() {
	p.stop <- struct{}{}
	p.sess.Close()
}

func (p *AppPlayer) Run(ctx context.Context, apiRecv <-chan ApiRequest) {
	err := p.sess.Dealer().Connect(ctx)
	if err != nil {
		p.app.log.Errorf("failed connecting to dealer: %v", err)
		p.Close()
		return
	}

	// Signal the API server that we are now consuming the request channel
	p.app.server.SetPlayerReady(true)
	defer p.app.server.SetPlayerReady(false)

	p.lyricsProvider = NewLyricsProvider(p.app.log, func(ctx context.Context, force bool) (string, error) {
		return p.sess.Spclient().GetAccessToken(ctx, force)
	})
	p.app.log.Infof("lyrics provider initialized")

	p.queueResolver = newQueueResolver(p.app.log, p.sess.Spclient(), p.queueResolvedCh)

	apRecv := p.sess.Accesspoint().Receive(ap.PacketTypeProductInfo, ap.PacketTypeCountryCode)
	msgRecv := p.sess.Dealer().ReceiveMessage("hm://pusher/v1/connections/", "hm://connect-state/v1/")
	reqRecv := p.sess.Dealer().ReceiveRequest("hm://connect-state/v1/player/command")

	for {
		select {
		case <-p.stop:
			return
		case pkt, ok := <-apRecv:
			if !ok {
				continue
			}
			if err := p.handleAccesspointPacket(pkt.Type, pkt.Payload); err != nil {
				p.app.log.Warnf("failed handling accesspoint packet: %v", err)
			}
		case msg, ok := <-msgRecv:
			if !ok {
				continue
			}
			if err := p.handleDealerMessage(ctx, msg); err != nil {
				p.app.log.Warnf("failed handling dealer message: %v", err)
			}
		case req, ok := <-reqRecv:
			if !ok {
				continue
			}
			if err := p.handleDealerRequest(ctx, req); err != nil {
				p.app.log.Warnf("failed handling dealer request: %v", err)
				req.Reply(false)
			} else {
				req.Reply(true)
			}
		case req, ok := <-apiRecv:
			if !ok {
				continue
			}
			data, err := p.handleApiRequest(ctx, req)
			req.Reply(data, err)
		case <-p.queueResolvedCh:
			// Background queue-metadata resolution landed
			if rs := p.state.remoteState; rs != nil && p.queueResolver != nil {
				p.queueResolver.applyCache(rs.NextTracks)
				p.queueResolver.applyCache(rs.PrevTracks)
				p.app.server.Emit(&ApiEvent{
					Type: ApiEventTypeObserverStateChanged,
					Data: rs,
				})
			}
		}
	}
}