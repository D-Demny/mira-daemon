package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	registerGen       atomic.Uint64
	registered        atomic.Bool
	heartbeatInFlight atomic.Bool
	clusterCh         chan *connectpb.Cluster

	prodInfo    *ProductInfo
	countryCode *string

	hasSpotConnId          bool
	hasInitialConnectState bool
	hasCountryCode         bool
	playbackReadyCh        chan struct{}
	playbackReadyOnce      sync.Once

	state *State

	// clockEst learns the local-vs-server clock offset from cluster deliveries
	// so stale snapshots can be aged correctly without trusting wall time.
	clockEst       clockOffsetEstimator
	clockEstSeeded bool
	// lastSampledClusterTs dedupes estimator samples: redelivered snapshots
	// share a Timestamp and must not count as fresh clock evidence
	lastSampledClusterTs int64

	prefetchTimer *time.Timer

	// lyricsProvider handles fetching lyrics from primary + LRCLIB sources
	lyricsProvider *LyricsProvider

	// queueResolver fills in artist/album names for queue entries
	queueResolver   *queueResolver
	queueResolvedCh chan struct{}

	// bug32: the Connect cluster payload only carries a short preview of the
	// upcoming queue; these back expandQueue, which derives the full queue
	// from the active context's track list (validated against the preview)
	queueExpandMu       sync.Mutex
	queueExpandCache    map[string]queueExpandCacheEntry
	queueExpandInFlight map[string]struct{}
	queueExpandedCh     chan queueExpandResult
	// swappable in tests (nil = the real (*AppPlayer).queueExpandPage);
	// asLibrary selects the fetch route (see queueExpandFetchOp)
	queueExpandPageFn func(ctx context.Context, contextUri string, offset, limit int, asLibrary bool) ([]any, int, error)

	// issue #56 fix #2: swappable in tests (nil = the real
	// (*AppPlayer).webApiMeAccountID) — the one-shot /v1/me lookup that
	// derives the account id behind the liked-songs collection
	webMeAccountFn func(ctx context.Context) string

	// issue #56 fix #4: single-flight guard for the background account-id
	// resolver — app start and an OAuth re-pair may both trigger it, but only
	// one loop runs per player; reset when the loop finishes so a later
	// re-pair can start a fresh one
	accountIDResolverRunning atomic.Bool

	// issue #56 fix #4: pacing of the background resolver, swappable in tests
	// (zero values = the production defaults below). Kept for compatibility
	// with the fix-#4 shape; precedence under fix #5: a non-zero
	// accountIDMaxAttempts still hard-caps the attempt count, and
	// accountIDRetryDelay is honored as the initial delay fallback when
	// accountIDInitialDelay is unset.
	accountIDMaxAttempts int
	accountIDRetryDelay  time.Duration

	// issue #56 fix #5: duration-budget pacing of the background resolver,
	// swappable in tests (zero values = the production defaults below). The
	// budget replaces the legacy attempt cap as the primary bound of the loop;
	// the retry delay doubles after every consecutive failure, starting at
	// accountIDInitialDelay and capped by accountIDMaxDelay.
	accountIDBudget       time.Duration
	accountIDInitialDelay time.Duration
	accountIDMaxDelay     time.Duration

	// issue #56: escape hatch for the liked-songs context. The primary play
	// sends the user-specific collection uri when the account id is
	// resolvable, and the bare pseudo id as a documented best-effort fallback
	// otherwise. If nothing actually starts — no POSITIVE active-state
	// transition and no NEGATIVE clear observed, and still none when
	// likedPlayRetryDelay has passed — handleLikedPlayRetry re-sends the same
	// last-sent context once (upgrading a bare fallback to the user-specific
	// uri if the account id becomes resolvable in the meantime). A positive
	// transition disarms the pending retry, a negative clear fires it
	// immediately (both via observeLikedRetryTransition on every state
	// update). All fields are touched only from the run loop, so no locking
	// is needed.
	playRetryPending *playRetryPending
	playRetryTimerCh <-chan time.Time

	// issue #56: set while the current session is a liked-songs context —
	// queue expansion of that session pages library tracks even if the
	// Connect state reports a playlist-shaped context id for it. Cleared on
	// every new play request.
	likedSessionActive atomic.Bool

	// swappable in tests (nil = the real (*AppPlayer).sendDeviceCommand)
	sendDeviceCommandFn func(ctx context.Context, deviceId, deviceName string, cmd connectCommand) error

	// async artist/album resolution
	metaResolvedCh       chan resolvedTrackMeta
	metaResolveInFlight  string
	metaResolveFailedUri string

	// in-memory cache of playlist track counts (uri -> entry), for the
	// locally-served /web-api/me/playlists endpoint
	playlistCountMu    sync.Mutex
	playlistCountCache map[string]playlistCountEntry
}

type resolvedTrackMeta struct {
	uri      string
	name     string
	artist   string
	album    string
	imageUrl string
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

// registerAsync registers this device with the connect cluster
func (p *AppPlayer) registerAsync(connId string, gen uint64) {
	waits := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second}
	const slowRetry = 30 * time.Second

	for attempt := 0; ; attempt++ {
		wait := slowRetry
		if attempt < len(waits) {
			wait = waits[attempt]
		}
		if wait > 0 {
			time.Sleep(wait)
		}

		// superseded by a newer connection-id
		if p.registerGen.Load() != gen || p.app.currentPlayer.Load() != p {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cluster, err := p.putConnectState(ctx, connId, connectpb.PutStateReason_NEW_DEVICE)
		cancel()
		if err == nil {
			if p.registerGen.Load() == gen {
				p.registered.Store(true)
				// the registration response carries the current cluster
				p.injectCluster(cluster)
			}
			if attempt > 0 {
				p.app.log.Debugf("connect state put landed on attempt %d", attempt+1)
			}
			return
		}

		// warn through the fast ladder
		if attempt < len(waits) {
			p.app.log.WithError(err).Warnf("connect state put failed (attempt %d), retrying", attempt+1)
		} else {
			p.app.log.WithError(err).Debugf("connect state put failed (attempt %d), retrying in %s", attempt+1, slowRetry)
		}
	}
}

// connectStateHeartbeatInterval keeps our cluster registration alive
const connectStateHeartbeatInterval = 4 * time.Minute

// heartbeatConnectState re-puts our connect state periodically
func (p *AppPlayer) heartbeatConnectState() {
	if !p.hasSpotConnId || !p.registered.Load() {
		// registerAsync is still retrying, don't pile on
		return
	}
	if !p.heartbeatInFlight.CompareAndSwap(false, true) {
		return
	}

	connId, gen := p.spotConnId, p.registerGen.Load()
	go func() {
		defer p.heartbeatInFlight.Store(false)

		for attempt := 1; attempt <= 2; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			cluster, err := p.putConnectState(ctx, connId, connectpb.PutStateReason_NEW_DEVICE)
			cancel()
			if err == nil {
				p.app.log.Tracef("connect state heartbeat ok")
				// keep the device list fresh even if no dealer push arrived
				if p.registerGen.Load() == gen {
					p.injectCluster(cluster)
				}
				return
			}

			// a newer connection-id took over, its registerAsync owns recovery now
			if p.registerGen.Load() != gen || p.app.currentPlayer.Load() != p {
				return
			}

			p.app.log.WithError(err).Warnf("connect state heartbeat failed (attempt %d)", attempt)
			if attempt == 1 {
				time.Sleep(5 * time.Second)
			}
		}

		p.registered.Store(false)
		p.sess.Dealer().ForceReconnect()
	}()
}

func (p *AppPlayer) handleDealerMessage(ctx context.Context, msg dealer.Message) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if strings.HasPrefix(msg.Uri, "hm://pusher/v1/connections/") {
		p.spotConnId = msg.Headers["Spotify-Connection-Id"]
		p.hasSpotConnId = p.spotConnId != ""
		if len(p.spotConnId) >= 32 {
			p.app.log.Debugf("received connection id: %s...%s", p.spotConnId[:16], p.spotConnId[len(p.spotConnId)-16:])
		} else {
			p.app.log.Debugf("received connection id (%d bytes)", len(p.spotConnId))
		}

		// a fresh connection-id voids any previous registration
		p.app.log.Infof("dealer connection-id received (%d bytes), re-registering with connect cluster", len(p.spotConnId))
		p.registered.Store(false)
		go p.registerAsync(p.spotConnId, p.registerGen.Add(1))

		p.hasInitialConnectState = true
		p.notifyPlaybackReadyIfNeeded()
		return nil
	} else if strings.HasPrefix(msg.Uri, "hm://connect-state/v1/cluster") {
		var clusterUpdate connectpb.ClusterUpdate
		if err := proto.Unmarshal(msg.Payload, &clusterUpdate); err != nil {
			return fmt.Errorf("failed unmarshalling ClusterUpdate: %w", err)
		}

		p.handleCluster(ctx, clusterUpdate.Cluster)
		return nil
	}

	p.app.log.Debugf("skipping dealer message, uri: %s", msg.Uri)
	return nil
}

// handleCluster applies a Cluster snapshot
func (p *AppPlayer) handleCluster(ctx context.Context, cluster *connectpb.Cluster) {
	if cluster == nil {
		return
	}

	activeDeviceId := cluster.ActiveDeviceId

	// snapshot the selectable device list
	p.updateConnectDevices(cluster)

	// "is anything active" signal is ActiveDeviceId
	switch {
	case activeDeviceId == p.app.deviceId:
		// ignore we cannot playback on the car thing
	case activeDeviceId == "":
		// nothing is active anywhere so we go idle.
		p.clearActiveDevice()
	case cluster.PlayerState != nil:
		p.updateRemoteState(ctx, cluster)
	}
}

// injectCluster hands a Cluster from a put_state response
func (p *AppPlayer) injectCluster(cluster *connectpb.Cluster) {
	if cluster == nil {
		return
	}
	select {
	case p.clusterCh <- cluster:
	default:
		// drop the previous queued snapshot in favour of this newer one
		select {
		case <-p.clusterCh:
		default:
		}
		select {
		case p.clusterCh <- cluster:
		default:
		}
	}
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
			// all rotated Connect image keys (bug33), same lookup the queue
			// projection uses (bug42)
			imageUrl = firstConnectImage(track.Metadata)
		}
	}

	if ps.ContextUri != "" {
		contextUri = ps.ContextUri
	}

	var contextName string
	if ps.ContextMetadata != nil {
		contextName = ps.ContextMetadata["context_description"]
	}

	rs := &RemoteState{
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
		SmartShuffle:          deriveSmartShuffle(ps.Options),
		RepeatContext:         ps.Options != nil && ps.Options.RepeatingContext,
		RepeatTrack:           ps.Options != nil && ps.Options.RepeatingTrack,
		DisallowSkipPrev:      ps.Restrictions != nil && len(ps.Restrictions.DisallowSkippingPrevReasons) > 0,
		DisallowSkipNext:      ps.Restrictions != nil && len(ps.Restrictions.DisallowSkippingNextReasons) > 0,
		DisallowSeek:          ps.Restrictions != nil && len(ps.Restrictions.DisallowSeekingReasons) > 0,
		PrevTracks:            projectQueue(ps.PrevTracks, QueueLimit),
		NextTracks:            projectQueue(ps.NextTracks, QueueLimit),
		RawMetadata:           rawMeta,
	}
	now := time.Now()
	rs.ReceivedAt = now
	rs.ReceivedAtWallMs = now.UnixMilli()
	rs.Position = rs.RemotePosition()
	return rs
}

// smartShuffleModeKey is the authoritative key inside
// ContextPlayerOptions.Modes carrying the smart-shuffle state. Wire fact from
// issue #39 (verified against a live capture of an active smart-shuffle
// state and the write-side set_options command): while smart shuffle is ON
// the desktop client publishes modes={context_enhancement:"RECOMMENDATION",
// jam:"off"}; with it off the same key reads "NONE".
const smartShuffleModeKey = "context_enhancement"

// deriveSmartShuffle reads the smart-shuffle flag from the Connect state.
// Only options.Modes["context_enhancement"] == "RECOMMENDATION" counts as on;
// anything else — "NONE", another value, an absent key, or no Modes map at
// all — means off.
func deriveSmartShuffle(options *connectpb.ContextPlayerOptions) bool {
	if options == nil {
		return false
	}
	return options.Modes[smartShuffleModeKey] == "RECOMMENDATION"
}

const clockSyncedFlag = "/run/clock_synced"

func (p *AppPlayer) noteClusterTiming(rs *RemoteState) {
	if !p.clockEstSeeded {
		if _, err := os.Stat(clockSyncedFlag); err != nil {
			rs.Position = rs.RemotePosition()
			return
		}
		p.clockEst.add(0)
		p.clockEstSeeded = true
	}
	if rs.Timestamp > 0 && rs.Timestamp != p.lastSampledClusterTs {
		p.clockEst.add(rs.ReceivedAtWallMs - rs.Timestamp)
		p.lastSampledClusterTs = rs.Timestamp
	}
	if off, ok := p.clockEst.offset(); ok {
		rs.clockOffsetMs = off
		rs.offsetKnown = true
	}
	rs.Position = rs.RemotePosition()
}

func (p *AppPlayer) updateRemoteState(ctx context.Context, cluster *connectpb.Cluster) {
	rs := clusterToRemoteState(cluster)
	if rs == nil {
		return
	}
	// bug32: the Connect cluster payload only carries a short preview of the
	// upcoming queue; expand it from the active context's track list when the
	// queue is derivable (non-shuffle playlist / Liked Songs)
	p.expandQueue(rs)
	p.noteClusterTiming(rs)
	if dev, ok := cluster.Device[rs.DeviceId]; ok {
		rs.DeviceName = p.deviceDisplayName(rs.DeviceId, dev)
		// issue #39: Modes map says smart shuffle but the active device
		// doesn't advertise the capability — note it for the on-device spike
		if rs.SmartShuffle && dev.Capabilities != nil && !dev.Capabilities.SupportsSmartShuffleMode {
			p.app.log.Debugf("cluster: smart shuffle active on %q but device does not advertise supports_smart_shuffle_mode", rs.DeviceId)
		}
		// issue #39 spike instrumentation: log the state echo only on
		// transitions (never per poll) so the on-device test can confirm
		// whether Spotify accepts/reverts the smart flag.
		if prev := p.state.remoteState; prev != nil && prev.SmartShuffle != rs.SmartShuffle {
			p.app.log.Debugf("cluster: smart shuffle state echo on %q: %v -> %v", rs.DeviceId, prev.SmartShuffle, rs.SmartShuffle)
		}
	}
	track := cluster.PlayerState.Track
	if prev := p.state.remoteState; prev != nil && prev.TrackUri == rs.TrackUri {
		delta := rs.RemotePosition() - prev.RemotePosition()
		staleMs := rs.ReceivedAtWallMs - rs.Timestamp - rs.clockOffsetMs
		if delta < -1500 && staleMs > 5000 {
			// a backward jump sourced from a STALE snapshot is the rewind
			// bug; a fresh-timestamp regression is just the user seeking back
			p.app.log.Warnf("cluster: position regressed %dms on %q (posAsOf %d->%d ts %d->%d stale %dms playing=%v paused=%v)",
				delta, rs.TrackUri, prev.PositionAsOfTimestamp, rs.PositionAsOfTimestamp,
				prev.Timestamp, rs.Timestamp, staleMs, rs.IsPlaying, rs.IsPaused)
		} else {
			p.app.log.Debugf("cluster: apply %q pos %dms (delta %+dms posAsOf %d ts %d)",
				rs.TrackUri, rs.RemotePosition(), delta, rs.PositionAsOfTimestamp, rs.Timestamp)
		}
	} else {
		p.app.log.Debugf("cluster: apply new track %q pos %dms (posAsOf %d ts %d playing=%v)",
			rs.TrackUri, rs.RemotePosition(), rs.PositionAsOfTimestamp, rs.Timestamp, rs.IsPlaying)
	}

	// Fill in any cached artist/album for the queue entries (issue #50:
	// cover art for the first-N cards too — see applyArtBackfill). The art
	// pass runs first so a card missing both text and cover lands in
	// artNext: it gets batched for text AND gets the web api fallback when
	// the batch carries no image (Connect sparse metadata is exactly that).
	if p.queueResolver != nil {
		artNext := p.queueResolver.applyArtBackfill(rs.NextTracks, true)
		needNext := p.queueResolver.applyCache(rs.NextTracks)
		needPrev := p.queueResolver.applyCache(rs.PrevTracks)
		fetch := append(append(append([]string{}, needNext...), needPrev...), artNext...)
		if len(fetch) > 0 {
			p.queueResolver.ResolveAsync(fetch, artNext)
		}
	}

	// Resolve artist and album from track metadata or spclient.
	// Unofficial connect devices often send a bare URI with empty metadata
	artistName := ""
	albumName := ""
	if track != nil && track.Metadata != nil {
		artistName = track.Metadata["artist_name"]
		albumName = track.Metadata["album_title"]
	}

	if rs.TrackUri != "" && (artistName == "" || rs.TrackName == "" || rs.TrackImageUrl == "") {
		spotId, err := librespot.SpotifyIdFromUri(rs.TrackUri)
		if err == nil && spotId != nil {
			// carry anything already resolved for this same track
			if prevState := p.state.remoteState; prevState != nil && prevState.TrackUri == rs.TrackUri {
				if artistName == "" && prevState.TrackArtist != "" {
					artistName = prevState.TrackArtist
					if albumName == "" {
						albumName = prevState.TrackAlbum
					}
				}
				if rs.TrackName == "" {
					rs.TrackName = prevState.TrackName
				}
				if rs.TrackImageUrl == "" {
					rs.TrackImageUrl = prevState.TrackImageUrl
				}
			}
			if artistName == "" && p.queueResolver != nil {
				if a, alb, ok := p.queueResolver.lookup(rs.TrackUri); ok {
					artistName, albumName = a, alb
				}
			}
			if artistName == "" || rs.TrackName == "" || rs.TrackImageUrl == "" {
				// resolve via spclient WITHOUT blocking the run loop
				p.resolveCurrentTrackMetaAsync(rs.TrackUri, *spotId)
			}
		}
	}

	rs.TrackArtist = artistName
	rs.TrackAlbum = firstNonEmpty(albumName, rs.RawMetadata["album_title"])

	prevState := p.state.remoteState
	trackChanged := prevState == nil || prevState.TrackUri != rs.TrackUri

	p.state.remoteState = rs
	// remember the active device
	if rs.DeviceId != "" {
		p.state.lastActiveDeviceId = rs.DeviceId
		p.state.lastActiveDeviceName = rs.DeviceName
	}

	// issue #56 fix #8: an armed liked-songs escape hatch observes every
	// active-state update — a positive transition disarms the pending retry,
	// a negative clear fires it immediately.
	p.observeLikedRetryTransition(ctx)

	if v := p.app.voice; v != nil {
		v.notifyPlayback(rs.IsPlaying && !rs.IsPaused)
	}

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

// ConnectDevice is a selectable Spotify Connect device
type ConnectDevice struct {
	Id             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Volume         uint32 `json:"volume"`
	VolumeSteps    int32  `json:"volume_steps"`
	VolumeDisabled bool   `json:"volume_disabled"`
	IsActive       bool   `json:"is_active"`
	IsOffline      bool   `json:"is_offline"`
	CanTransfer    bool   `json:"can_transfer"`
}

// defaultDeviceIdFromSettings reads the "default_device_id" field from the
// UI settings blob. Returns empty string when unset or invalid.
func defaultDeviceIdFromSettings(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s struct {
		DefaultDeviceId *string `json:"default_device_id"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	if s.DefaultDeviceId == nil || *s.DefaultDeviceId == "" {
		return ""
	}
	return *s.DefaultDeviceId
}

// resolveTargetDevice determines the target device for player commands.
// It returns (deviceId, deviceName, isOffline) where:
//   - if default_device_id is set and available: returns that device
//   - if default_device_id is set but offline: returns it with isOffline=true
//   - if default_device_id is unset or not in list: falls back to active device
func (p *AppPlayer) resolveTargetDevice() (deviceId, deviceName string, isOffline bool) {
	defaultId := defaultDeviceIdFromSettings(p.app.state.Settings)
	if defaultId == "" {
		// no default configured — keep existing behaviour (active device)
		rs := p.state.remoteState
		if rs != nil && rs.DeviceId != "" {
			return rs.DeviceId, rs.DeviceName, false
		}
		return "", "", false
	}

	// default is set — look it up in the connect device list
	for _, d := range p.state.connectDevices {
		if d.Id == defaultId {
			return d.Id, d.Name, d.IsOffline
		}
	}

	// default device not in current list — fall back to active device
	rs := p.state.remoteState
	if rs != nil && rs.DeviceId != "" {
		return rs.DeviceId, rs.DeviceName, false
	}
	return "", "", false
}

// transferIfNeeded transfers playback to the default device if it differs
// from the currently active device. Returns true if a transfer was performed.
func (p *AppPlayer) transferIfNeeded(ctx context.Context) bool {
	defaultId := defaultDeviceIdFromSettings(p.app.state.Settings)
	if defaultId == "" {
		return false
	}
	rs := p.state.remoteState
	if rs == nil || rs.DeviceId == "" {
		return false
	}
	if rs.DeviceId == defaultId {
		return false
	}
	p.app.log.Infof("transfer: active=%s default=%s — transferring playback", rs.DeviceId, defaultId)
	if err := p.sendTransfer(ctx, defaultId); err != nil {
		p.app.log.Warnf("transfer to default device %s failed: %v", defaultId, err)
		return false
	}
	return true
}

func looksLikeDeviceId(s string) bool {
	if len(s) < 16 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// the best human readable name a DeviceInfo offers
func friendlyDeviceName(d *connectpb.DeviceInfo) string {
	if alias, ok := d.DeviceAliases[d.SelectedAliasId]; ok && alias.GetDisplayName() != "" {
		return alias.GetDisplayName()
	}
	var bestId uint32
	best := ""
	for id, alias := range d.DeviceAliases {
		if alias.GetDisplayName() != "" && (best == "" || id < bestId) {
			best, bestId = alias.GetDisplayName(), id
		}
	}
	if best != "" {
		return best
	}
	if d.Name != "" && !looksLikeDeviceId(d.Name) {
		return d.Name
	}
	if s := strings.TrimSpace(d.Brand + " " + d.Model); s != "" && !looksLikeDeviceId(s) {
		return s
	}
	return ""
}

// resolves a display name remembering the last good one per device id
func (p *AppPlayer) deviceDisplayName(id string, d *connectpb.DeviceInfo) string {
	if name := friendlyDeviceName(d); name != "" {
		if p.state.connectDeviceNames == nil {
			p.state.connectDeviceNames = map[string]string{}
		}
		p.state.connectDeviceNames[id] = name
		return name
	}
	if cached := p.state.connectDeviceNames[id]; cached != "" {
		return cached
	}
	return d.Name
}

// snapshots the selectable connect devices from a cluster
func (p *AppPlayer) updateConnectDevices(cluster *connectpb.Cluster) {
	activeDeviceId := cluster.ActiveDeviceId
	devs := make([]ConnectDevice, 0, len(cluster.Device))
	for id, d := range cluster.Device {
		if id == p.app.deviceId {
			continue
		}
		// ghost/stale cluster entries aren't selectable, don't list them
		if d.IsOffline && id != activeDeviceId {
			continue
		}
		cd := ConnectDevice{
			Id:          id,
			Name:        p.deviceDisplayName(id, d),
			Type:        d.DeviceType.String(),
			Volume:      d.Volume,
			IsActive:    id == activeDeviceId,
			IsOffline:   d.IsOffline,
			CanTransfer: len(d.DisallowTransferReasons) == 0,
		}
		if d.Capabilities != nil {
			cd.VolumeSteps = d.Capabilities.VolumeSteps
			cd.VolumeDisabled = d.Capabilities.DisableVolume
		}
		devs = append(devs, cd)
	}
	// active device first, then alphabetical
	sort.Slice(devs, func(i, j int) bool {
		if devs[i].IsActive != devs[j].IsActive {
			return devs[i].IsActive
		}
		return devs[i].Name < devs[j].Name
	})

	p.state.connectDevices = devs

	// only emit when the meaningful shape changes
	sig := connectDevicesSignature(devs)
	if sig == p.state.connectDevSig {
		return
	}
	p.state.connectDevSig = sig
	p.app.server.Emit(&ApiEvent{Type: ApiEventTypeConnectDevices, Data: devs})
}

func connectDevicesSignature(devs []ConnectDevice) string {
	var sb strings.Builder
	for _, d := range devs {
		fmt.Fprintf(&sb, "%s:%s:%t:%t;", d.Id, d.Name, d.IsActive, d.IsOffline)
	}
	return sb.String()
}

// returns the device snapshot, never nil
func (p *AppPlayer) connectDevicesOrEmpty() []ConnectDevice {
	if p.state.connectDevices == nil {
		return []ConnectDevice{}
	}
	return p.state.connectDevices
}

// drops the observed remote state when no device is active
func (p *AppPlayer) clearActiveDevice() {
	if p.state.remoteState == nil {
		return
	}
	p.state.remoteState = nil
	if v := p.app.voice; v != nil {
		v.notifyPlayback(false)
	}
	p.app.server.Emit(&ApiEvent{Type: ApiEventTypeObserverInactive})
}

// resolveTrackMetadata tries spclient first (fast), falls back to the web API
// when spclient is unavailable
func (p *AppPlayer) resolveTrackMetadata(ctx context.Context, spotId librespot.SpotifyId) resolvedTrackMeta {
	meta := p.resolveViaSpclient(ctx, spotId)
	if meta.artist != "" {
		return meta
	}

	if wb := p.resolveViaWebApi(ctx, spotId); wb.artist != "" {
		return wb
	}
	return meta
}

// fetches metadata for the current track off the run loop
func (p *AppPlayer) resolveCurrentTrackMetaAsync(uri string, spotId librespot.SpotifyId) {
	if p.metaResolveInFlight == uri || p.metaResolveFailedUri == uri {
		return
	}
	p.metaResolveInFlight = uri
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var meta resolvedTrackMeta
		if strings.HasPrefix(uri, "spotify:episode:") {
			// podcasts dont carry an artist name
			meta = p.resolveEpisodeMetadata(ctx, spotId)
		} else {
			meta = p.resolveTrackMetadata(ctx, spotId)
		}
		meta.uri = uri
		select {
		case p.metaResolvedCh <- meta:
		default:
			// drop
		}
	}()
}

// picks the closest cover file id and turns it into an image CDN url
func coverImageUrl(images []*metadatapb.Image, size string) string {
	fileId := getBestImageIdForSize(images, size)
	if fileId == nil {
		return ""
	}
	return "https://i.scdn.co/image/" + hex.EncodeToString(fileId)
}

// resolveEpisodeMetadata resolves a podcast episode's show name
func (p *AppPlayer) resolveEpisodeMetadata(ctx context.Context, spotId librespot.SpotifyId) resolvedTrackMeta {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var ep metadatapb.Episode
	if err := p.sess.Spclient().ExtendedMetadataSimple(reqCtx, spotId,
		extmetadatapb.ExtensionKind_EPISODE_V4, &ep); err != nil {
		p.app.log.Debugf("observer: spclient episode metadata for %s failed: %v", spotId.Uri(), err)
		return resolvedTrackMeta{}
	}

	var meta resolvedTrackMeta
	meta.name = ep.GetName()
	if ep.Show != nil && ep.Show.Name != nil {
		meta.artist = *ep.Show.Name
		meta.album = *ep.Show.Name
	}
	if ep.CoverImage != nil {
		meta.imageUrl = coverImageUrl(ep.CoverImage.Image, p.app.cfg.ImageSize)
	}
	p.app.log.Debugf("observer: spclient episode metadata for %s: show=%q", spotId.Uri(), meta.artist)
	return meta
}

func (p *AppPlayer) resolveViaSpclient(ctx context.Context, spotId librespot.SpotifyId) resolvedTrackMeta {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var trackMeta metadatapb.Track
	if err := p.sess.Spclient().ExtendedMetadataSimple(reqCtx, spotId,
		extmetadatapb.ExtensionKind_TRACK_V4, &trackMeta); err != nil {
		p.app.log.Debugf("observer: spclient metadata for %s failed: %v", spotId.Uri(), err)
		return resolvedTrackMeta{}
	}

	var meta resolvedTrackMeta
	if trackMeta.Name != nil {
		meta.name = *trackMeta.Name
	}
	if len(trackMeta.Artist) > 0 && trackMeta.Artist[0].Name != nil {
		meta.artist = *trackMeta.Artist[0].Name
	}
	if trackMeta.Album != nil {
		if trackMeta.Album.Name != nil {
			meta.album = *trackMeta.Album.Name
		}
		meta.imageUrl = coverImageUrl(trackMeta.Album.Cover, p.app.cfg.ImageSize)
		if meta.imageUrl == "" && trackMeta.Album.CoverGroup != nil {
			meta.imageUrl = coverImageUrl(trackMeta.Album.CoverGroup.Image, p.app.cfg.ImageSize)
		}
	}

	p.app.log.Debugf("observer: spclient metadata for %s: name=%q artist=%q, album=%q",
		spotId.Uri(), meta.name, meta.artist, meta.album)
	return meta
}

func (p *AppPlayer) resolveViaWebApi(ctx context.Context, spotId librespot.SpotifyId) resolvedTrackMeta {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := p.sess.WebApi(reqCtx, "GET", "/v1/tracks/"+spotId.Base62(), nil, nil, nil)
	if err != nil {
		p.app.log.Debugf("observer: web api metadata for %s failed: %v", spotId.Uri(), err)
		return resolvedTrackMeta{}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.app.log.Debugf("observer: web api metadata for %s returned status %d", spotId.Uri(), resp.StatusCode)
		return resolvedTrackMeta{}
	}

	var data struct {
		Name    string `json:"name"`
		Artists []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Album struct {
			Name   string `json:"name"`
			Images []struct {
				Url string `json:"url"`
			} `json:"images"`
		} `json:"album"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		p.app.log.Debugf("observer: web api metadata for %s decode failed: %v", spotId.Uri(), err)
		return resolvedTrackMeta{}
	}

	var meta resolvedTrackMeta
	meta.name = data.Name
	if len(data.Artists) > 0 {
		meta.artist = data.Artists[0].Name
	}
	meta.album = data.Album.Name
	if len(data.Album.Images) > 0 {
		meta.imageUrl = data.Album.Images[0].Url
	}

	p.app.log.Debugf("observer: web api metadata for %s: name=%q artist=%q, album=%q",
		spotId.Uri(), meta.name, meta.artist, meta.album)
	return meta
}

// issue #50: the queue backfill only needs the album cover, not full track
// metadata — this is the webArt hook handed to the queueResolver. It reuses
// resolveViaWebApi (the same /v1/tracks path the active track falls back
// to), so no new endpoint or token handling is involved. A uri that does not
// parse as a track id yields no cover, same as a web api failure: empty stays
// empty, no error propagation into the background pass.
func (p *AppPlayer) queueWebArt(ctx context.Context, uri string) string {
	spotId, err := librespot.SpotifyIdFromUri(uri)
	if err != nil || spotId == nil {
		return ""
	}
	return p.resolveViaWebApi(ctx, *spotId).imageUrl
}

const (
	pfAddToLibraryHash         = "7c5a69420e2bfae3da5cc4e14cbc8bb3f6090f80afc00ffc179177f19be3f33d"
	pfApplyCurationsHash       = "05b739a3a73091c213385233b9d3ed8a857c2ca29d2eebadb3d04ed12e288697"
	pfAreEntitiesInLibraryHash = "134337999233cc6fdd6b1e6dbf94841409f04a946c5c7b744b09ba0dfe5a85ed"
	// play history of the current user (web player "Recents" list)
	pfRecentsHash = "698be5892a3cc95331deebeff463d05dfdd5febf5254bea30b895b5a93dfb584"
)

// returns the current hash for an operation
func (p *AppPlayer) hashOf(op string) string {
	return p.app.hashes.hash(op)
}

// fires re-scrape when a pathfinder call reports a rotated hash
func (p *AppPlayer) onPersistedDrift() {
	if v := p.app.voice; v != nil {
		v.triggerHashRotate()
	}
}

// reports whether a graphQL error signals a rotated hash
func isPersistedQueryErr(e error) bool {
	s := e.Error()
	return strings.Contains(s, "PersistedQueryNotFound") || strings.Contains(s, "PersistedQueryNotSupported")
}

func pathfinderGraphQLError(body []byte) error {
	var r struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &r) == nil && len(r.Errors) > 0 {
		return fmt.Errorf("pathfinder: %s", r.Errors[0].Message)
	}
	return nil
}

func (p *AppPlayer) pathfinderQuery(ctx context.Context, body []byte) ([]byte, error) {
	resp, err := p.sess.PartnerApi(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("pathfinder request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(resp.Body)

	// Log the failure body
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(data)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		p.app.log.Warnf("pathfinder: status=%d ct=%q body=%q", resp.StatusCode, resp.Header.Get("Content-Type"), snippet)
	}

	switch resp.StatusCode {
	case 401, 403:
		return nil, ErrForbidden
	case 429:
		return nil, ErrTooManyRequests
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("pathfinder returned status %d", resp.StatusCode)
	}

	if e := pathfinderGraphQLError(data); e != nil {
		if isPersistedQueryErr(e) {
			p.onPersistedDrift()
		}
		return nil, e
	}
	return data, nil
}

type pathfinderError struct {
	Status         int
	RetryAfter     time.Duration
	PersistedQuery bool
	msg            string
}

func (e *pathfinderError) Error() string { return e.msg }

// for the catalog sync
func (p *AppPlayer) pathfinderQueryEx(ctx context.Context, body []byte, force bool) ([]byte, error) {
	resp, err := p.sess.PartnerApiEx(ctx, body, force)
	if err != nil {
		return nil, fmt.Errorf("pathfinder request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(resp.Body)
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(data)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, &pathfinderError{
			Status:     resp.StatusCode,
			RetryAfter: retryAfter,
			msg:        fmt.Sprintf("pathfinder status %d: %s", resp.StatusCode, snippet),
		}
	}

	if msg := pathfinderGraphQLErrorMsg(data); msg != "" {
		pq := strings.Contains(msg, "PersistedQueryNotFound") || strings.Contains(msg, "PersistedQueryNotSupported")
		return nil, &pathfinderError{Status: 200, PersistedQuery: pq, msg: "pathfinder: " + msg}
	}
	return data, nil
}

func pathfinderGraphQLErrorMsg(body []byte) string {
	var r struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &r) == nil && len(r.Errors) > 0 {
		return r.Errors[0].Message
	}
	return ""
}

func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// report whether the URI is in liked songs
func (p *AppPlayer) checkTrackSaved(ctx context.Context, uri string) (bool, error) {
	if !strings.HasPrefix(uri, "spotify:") {
		return false, ErrBadRequest
	}

	body, _ := json.Marshal(map[string]any{
		"operationName": "areEntitiesInLibrary",
		"variables":     map[string]any{"uris": []string{uri}},
		"extensions": map[string]any{
			"persistedQuery": map[string]any{"version": 1, "sha256Hash": p.hashOf("areEntitiesInLibrary")},
		},
	})

	data, err := p.pathfinderQuery(ctx, body)
	if err != nil {
		return false, err
	}

	var r struct {
		Data struct {
			Lookup []struct {
				Saved *bool `json:"saved"`
				Data  struct {
					Saved *bool `json:"saved"`
				} `json:"data"`
			} `json:"lookup"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &r) == nil && len(r.Data.Lookup) > 0 {
		e := r.Data.Lookup[0]
		if e.Data.Saved != nil {
			return *e.Data.Saved, nil
		}
		if e.Saved != nil {
			return *e.Saved, nil
		}
	}
	return false, nil
}

// adds or removes the track or local file from liked songs
func (p *AppPlayer) setTrackSaved(ctx context.Context, uri string, saved bool) error {
	if !strings.HasPrefix(uri, "spotify:") {
		return ErrBadRequest
	}

	var body []byte
	switch {
	case saved && strings.HasPrefix(uri, "spotify:local:"):
		body = p.applyCurationsBody(uri, "CURATE")
	case saved:
		body, _ = json.Marshal(map[string]any{
			"operationName": "addToLibrary",
			"variables":     map[string]any{"libraryItemUris": []string{uri}},
			"extensions": map[string]any{
				"persistedQuery": map[string]any{"version": 1, "sha256Hash": p.hashOf("addToLibrary")},
			},
		})
	default:
		body = p.applyCurationsBody(uri, "UNCURATE")
	}

	if _, err := p.pathfinderQuery(ctx, body); err != nil {
		return err
	}
	p.app.log.Infof("liked songs: %s %s", map[bool]string{true: "added", false: "removed"}[saved], uri)
	return nil
}

func (p *AppPlayer) applyCurationsBody(uri, curationType string) []byte {
	body, _ := json.Marshal(map[string]any{
		"operationName": "applyCurations",
		"variables": map[string]any{
			"input": map[string]any{
				"curations": []any{
					map[string]any{"contextUri": "spotify:collection:tracks", "curationType": curationType},
				},
				"itemUris": []string{uri},
			},
		},
		"extensions": map[string]any{
			"persistedQuery": map[string]any{"version": 1, "sha256Hash": p.hashOf("applyCurations")},
		},
	})
	return body
}

func (p *AppPlayer) handleApiRequest(ctx context.Context, req ApiRequest) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch req.Type {
	case ApiRequestTypeRoot:
		return &ApiResponseRoot{PlaybackReady: p.playbackReady()}, nil

	case ApiRequestTypeWebApi:
		data := req.Data.(ApiRequestDataWebApi)
		p.app.log.Debugf("web-api: %s %s", data.Method, data.Path)
		resp, err := p.sess.WebApi(ctx, data.Method, data.Path, data.Query, nil, nil)
		if err != nil {
			p.app.log.Errorf("web-api: %s %s failed: %v", data.Method, data.Path, err)
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
			p.app.log.Debugf("web-api: %s %s returned non-JSON (%d bytes)", data.Method, data.Path, len(respBody))
			return respBody, nil
		}

		var respJson any
		if err = json.NewDecoder(resp.Body).Decode(&respJson); err != nil {
			p.app.log.Errorf("web-api: %s %s decode error: %v", data.Method, data.Path, err)
			return nil, fmt.Errorf("failed to decode response body: %w", err)
		}
		p.app.log.Debugf("web-api: %s %s success (status %d)", data.Method, data.Path, resp.StatusCode)
		return respJson, nil

	case ApiRequestTypeWebApiLocal:
		data := req.Data.(ApiRequestDataWebApi)
		p.app.log.Debugf("web-api (local): %s %s", data.Method, data.Path)
		return p.handleWebApiLocal(ctx, data)

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
		settingUp := false
		var setupProgress *catalogProgress
		if v := p.app.voice; v != nil && v.firstSyncInProgress.Load() {
			settingUp = true
			setupProgress = v.syncProgressSnapshot()
		}
		if p.state.remoteState == nil {
			message := "no remote device is currently playing"
			if settingUp {
				message = "setting things up"
			}
			resp := map[string]any{
				"active":            false,
				"message":           message,
				"setting_up":        settingUp,
				"devices":           p.connectDevicesOrEmpty(),
				"utc_offset_min":    p.app.utcOffsetMin(),
				"latest_version":    p.app.latestVersion(),
				"latest_highlights": p.app.latestHighlights(),
				"update_available":  p.app.updateAvailable(),
				"update_mandatory":  p.app.updateMandatory(),
			}
			if setupProgress != nil {
				resp["setting_up_progress"] = setupProgress
			}
			return resp, nil
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

		resp := map[string]any{
			"active":            true,
			"device_id":         rs.DeviceId,
			"device_name":       rs.DeviceName,
			"device_type":       rs.DeviceType,
			"track_id":          trackId,
			"track_uri":         rs.TrackUri,
			"track_name":        rs.TrackName,
			"track_artist":      rs.TrackArtist,
			"track_album":       rs.TrackAlbum,
			"track_image":       rs.TrackImageUrl,
			"context_uri":       rs.ContextUri,
			"context_name":      rs.ContextName,
			"duration":          rs.Duration,
			"position":          rs.RemotePosition(),
			"is_playing":        rs.IsPlaying,
			"is_paused":         rs.IsPaused,
			"volume":            rs.Volume,
			"volume_max":        MaxStateVolume,
			"volume_disabled":   rs.VolumeDisabled,
			"volume_steps":      rs.VolumeSteps,
			"shuffle":           rs.ShuffleContext,
			"smart_shuffle":     rs.SmartShuffle,
			"repeat_context":    rs.RepeatContext,
			"repeat_track":      rs.RepeatTrack,
			"disallow_prev":     rs.DisallowSkipPrev,
			"disallow_next":     rs.DisallowSkipNext,
			"disallow_seek":     rs.DisallowSeek,
			"prev_tracks":       rs.PrevTracks,
			"next_tracks":       rs.NextTracks,
			"lyrics_url":        lyricsUrl,
			"raw_metadata":      rs.RawMetadata,
			"setting_up":        settingUp,
			"devices":           p.connectDevicesOrEmpty(),
			"utc_offset_min":    p.app.utcOffsetMin(),
			"latest_version":    p.app.latestVersion(),
			"latest_highlights": p.app.latestHighlights(),
			"update_available":  p.app.updateAvailable(),
			"update_mandatory":  p.app.updateMandatory(),
		}
		if setupProgress != nil {
			resp["setting_up_progress"] = setupProgress
		}
		return resp, nil

	case ApiRequestTypeConnectDevices:
		return map[string]any{"devices": p.connectDevicesOrEmpty()}, nil

	case ApiRequestTypeTransfer:
		data, _ := req.Data.(ApiRequestDataTransfer)
		return nil, p.sendTransfer(ctx, data.DeviceId)

	case ApiRequestTypeResume:
		targetId, targetName, _ := p.resolveTargetDevice()
		if targetId == "" {
			return nil, fmt.Errorf("no target device for resume")
		}
		return nil, p.sendDeviceCommand(ctx, targetId, targetName, connectCommand{Endpoint: "resume"})
	case ApiRequestTypeResumeLast:
		p.transferIfNeeded(ctx)
		return nil, p.resumeLastDevice(ctx)
	case ApiRequestTypePause:
		targetId, targetName, _ := p.resolveTargetDevice()
		if targetId == "" {
			return nil, fmt.Errorf("no target device for pause")
		}
		return nil, p.sendDeviceCommand(ctx, targetId, targetName, connectCommand{Endpoint: "pause"})
	case ApiRequestTypePlayPause:
		// pick the endpoint from the last known playback state
		endpoint := "resume"
		if rs := p.state.remoteState; rs != nil && rs.IsPlaying && !rs.IsPaused {
			endpoint = "pause"
		}
		targetId, targetName, _ := p.resolveTargetDevice()
		if targetId == "" {
			return nil, fmt.Errorf("no target device for playpause")
		}
		return nil, p.sendDeviceCommand(ctx, targetId, targetName, connectCommand{Endpoint: endpoint})
	case ApiRequestTypeNext:
		targetId, targetName, _ := p.resolveTargetDevice()
		if targetId == "" {
			return nil, fmt.Errorf("no target device for next")
		}
		return nil, p.sendDeviceCommand(ctx, targetId, targetName, connectCommand{Endpoint: "skip_next"})
	case ApiRequestTypePrev:
		targetId, targetName, _ := p.resolveTargetDevice()
		if targetId == "" {
			return nil, fmt.Errorf("no target device for prev")
		}
		return nil, p.sendDeviceCommand(ctx, targetId, targetName, connectCommand{Endpoint: "skip_prev"})
	case ApiRequestTypeSeek:
		data, _ := req.Data.(ApiRequestDataSeek)
		if data.Relative {
			return nil, fmt.Errorf("relative seek not supported in observer mode")
		}
		targetId, targetName, _ := p.resolveTargetDevice()
		if targetId == "" {
			return nil, fmt.Errorf("no target device for seek")
		}
		return nil, p.sendDeviceCommand(ctx, targetId, targetName, connectCommand{Endpoint: "seek_to", Value: data.Position})
	case ApiRequestTypeSetShufflingContext:
		data, _ := req.Data.(ApiRequestDataShuffle)
		targetId, targetName, _ := p.resolveTargetDevice()
		if targetId == "" {
			return nil, fmt.Errorf("no target device for shuffle")
		}
		return nil, p.sendDeviceCommand(ctx, targetId, targetName, buildShuffleCommand(data))
	case ApiRequestTypeSetRepeatingContext:
		val, _ := req.Data.(bool)
		targetId, targetName, _ := p.resolveTargetDevice()
		if targetId == "" {
			return nil, fmt.Errorf("no target device for repeat context")
		}
		return nil, p.sendDeviceCommand(ctx, targetId, targetName, connectCommand{Endpoint: "set_repeating_context", Value: val})
	case ApiRequestTypeSetRepeatingTrack:
		val, _ := req.Data.(bool)
		targetId, targetName, _ := p.resolveTargetDevice()
		if targetId == "" {
			return nil, fmt.Errorf("no target device for repeat track")
		}
		return nil, p.sendDeviceCommand(ctx, targetId, targetName, connectCommand{Endpoint: "set_repeating_track", Value: val})

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
		if rs.VolumeDisabled {
			// route volume controls to phone directly
			if data.Relative && rs.DeviceType == "SMARTPHONE" && p.app.bt != nil && p.app.bt.SendPhoneVolumeSteps(int(data.Volume)) {
				p.app.log.Debugf("set_volume: routed %+d phone-volume step(s) (%s)", data.Volume, rs.DeviceName)
				return nil, nil
			}
			return nil, fmt.Errorf("active device does not allow volume control")
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
		// tell the target device to start a context. issue #56: the primary
		// liked-songs play sends the user-specific collection uri (the form
		// proven to start playback on the device); when no account id source
		// resolves, the bare pseudo id is sent as a documented legacy
		// best-effort fallback. offset/skipTo pass through unchanged, and the
		// escape hatch below re-sends the same last-sent context once if the
		// receiver does not actually start playback.
		data, _ := req.Data.(ApiRequestDataPlay)
		if data.Uri == "" {
			return nil, fmt.Errorf("play requires a context uri")
		}
		p.transferIfNeeded(ctx)
		targetId, targetName, _ := p.resolveTargetDevice()
		if targetId == "" {
			return nil, fmt.Errorf("no target device for play")
		}
		isLiked := data.Uri == likedCollectionUri
		sendData := data
		if isLiked {
			sendData.Uri = p.resolvePlayContextUri(data.Uri)
			if sendData.Uri == likedCollectionUri {
				p.app.log.Warnf("play: liked-songs account id unresolved — sending the bare pseudo context as a best-effort fallback")
			}
		}
		cmd := buildPlayCommand(sendData)
		shuf := "inherit"
		if data.Shuffle != nil {
			shuf = fmt.Sprintf("%v", *data.Shuffle)
		}
		// TEMP diagnostic logging while verifying the play envelope on hardware.
		offsetStr := ""
		if data.Offset != nil {
			offsetStr = fmt.Sprintf(" offset={uri:%q pos:%d}", data.Offset.Uri, data.Offset.Position)
		}
		reqStr := ""
		if isLiked {
			reqStr = fmt.Sprintf(" (requested %s)", likedCollectionUri)
		}
		p.app.log.Infof("play: context=%s%s skipTo=%q shuffle=%s%s -> %s", sendData.Uri, reqStr, data.SkipToUri, shuf, offsetStr, targetName)
		if err := p.sendDeviceCommand(ctx, targetId, targetName, cmd); err != nil {
			return nil, err
		}
		// issue #56: every new play supersedes the previous session's
		// escape-hatch state. A liked-songs play marks the current session as
		// a liked-songs context (queue expansion pages library tracks even
		// for a playlist-shaped Connect echo) and arms the one-shot retry with
		// the exact context that just went out on the wire.
		p.resetLikedSession()
		if isLiked {
			p.likedSessionActive.Store(true)
			p.armLikedPlayRetry(targetId, targetName, sendData)
		}
		return nil, nil

	case ApiRequestTypeSearch:
		data, _ := req.Data.(ApiRequestDataSearch)
		if data.TopN {
			return p.searchTracks(ctx, data.Query)
		}
		return p.searchTrack(ctx, data.Query)

	case ApiRequestTypeCatalogPage:
		data, _ := req.Data.(ApiRequestDataCatalogPage)
		return p.catalogPage(ctx, data)

	case ApiRequestTypeAddToQueue:
		uri, _ := req.Data.(string)
		if uri == "" {
			return nil, fmt.Errorf("add_to_queue requires a uri")
		}
		return nil, p.sendActiveDeviceCommand(ctx, connectCommand{
			Endpoint: "add_to_queue",
			Track: &connectQueueTrack{
				Uri:      uri,
				Provider: "queue",
				Metadata: map[string]string{"is_queued": "true"},
			},
		})

	case ApiRequestTypeGetSaved:
		data, _ := req.Data.(ApiRequestDataSaved)
		saved, err := p.checkTrackSaved(ctx, data.Uri)
		if err != nil {
			return nil, err
		}
		return map[string]any{"saved": saved}, nil

	case ApiRequestTypeSetSaved:
		data, _ := req.Data.(ApiRequestDataSaved)
		if err := p.setTrackSaved(ctx, data.Uri, data.Saved); err != nil {
			return nil, err
		}
		return map[string]any{"saved": data.Saved}, nil

	default:
		return nil, fmt.Errorf("unknown request type: %s", req.Type)
	}
}

// connectCommand is the JSON shape of a single Spotify Connect remote-control command
type connectCommand struct {
	Endpoint      string             `json:"endpoint"`
	Value         any                `json:"value,omitempty"`
	Context       *connectContext    `json:"context,omitempty"`
	Options       *connectOptions    `json:"options,omitempty"`
	PlayOrigin    *connectOrigin     `json:"play_origin,omitempty"`
	LoggingParams *connectLogging    `json:"logging_params,omitempty"`
	Track         *connectQueueTrack `json:"track,omitempty"`
	// set_options fields (issue #39, verified wire protocol): siblings of
	// "endpoint" inside the command object, cf. SetOptionsRequest proto
	// (shuffling_context=3, modes=7 map<string,string>).
	ShufflingContext *bool             `json:"shuffling_context,omitempty"`
	Modes            map[string]string `json:"modes,omitempty"`
}

type connectQueueTrack struct {
	Uri      string            `json:"uri"`
	Provider string            `json:"provider,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// connectContext/connectOptions/connectOrigin/connectLogging are the play-command sub-objects
type connectContext struct {
	Uri      string   `json:"uri"`
	Url      string   `json:"url,omitempty"`
	Metadata struct{} `json:"metadata"`
}

type connectOptions struct {
	License               string                       `json:"license,omitempty"`
	SkipTo                connectSkipTo                `json:"skip_to"`
	Offset                *connectOffset               `json:"offset,omitempty"`
	PlayerOptionsOverride connectPlayerOptionsOverride `json:"player_options_override"`
}

// connectOffset is the play command's start position inside the context
// (absolute index and/or the track uri to resolve within the context)
type connectOffset struct {
	Uri      string `json:"uri,omitempty"`
	Position int    `json:"position,omitempty"`
}

type connectPlayerOptionsOverride struct {
	ShufflingContext *bool `json:"shuffling_context,omitempty"`
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

// buildPlayCommand assembles the connect play command envelope for starting a
// context. Pure function of the request data so the wire contract stays
// unit-testable without a live cluster.
func buildPlayCommand(data ApiRequestDataPlay) connectCommand {
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
	} else if data.Offset != nil && data.Offset.Uri != "" {
		// bug29: connect receivers (go-librespot, Spotify web player) ignore
		// `options.offset` and only honor `options.skip_to.track_uri` to start
		// a context at a given track. Mirror the offset's track uri into
		// skip_to so the playlist actually starts at the requested track.
		cmd.Options.SkipTo = connectSkipTo{TrackUri: data.Offset.Uri}
	}
	if data.Offset != nil {
		cmd.Options.Offset = &connectOffset{Uri: data.Offset.Uri, Position: data.Offset.Position}
	}
	if data.Shuffle != nil {
		cmd.Options.PlayerOptionsOverride.ShufflingContext = data.Shuffle
	}
	return cmd
}

// issue #56: the bare "Liked Songs" pseudo context id is not reliably
// resolvable on a Connect receiver, while each account's liked songs are
// addressed by the user-specific public collection
// `spotify:user:<userId>:collection:tracks`. The play path uses that
// user-specific form whenever spotifyAccountId can resolve <id> (cached /v1/me
// result, bounded /v1/me lookup, JWT sub claim, or stored credentials username)
// and falls back to the bare pseudo id only when no source does — a documented
// legacy best effort. Spotify's global "Today's Top Hits" playlist is NOT an
// account-scoped liked-songs context and must not be sent here.

// resolvePlayContextUri replaces the bare liked-songs pseudo context with the
// resolvable user-specific collection uri right before the play command
// envelope is built. Every other context passes through unchanged; a pseudo id
// without a resolvable account id comes back as the bare pseudo uri (the
// documented best-effort fallback).
func (p *AppPlayer) resolvePlayContextUri(uri string) string {
	if uri != likedCollectionUri {
		return uri
	}
	id := p.spotifyAccountId()
	if id == "" {
		p.app.log.Debugf("play: liked-songs context unresolved (no account id via web api /v1/me, the OAuth token, or the stored credentials)")
		return uri
	}
	return "spotify:user:" + id + ":collection:tracks"
}

// likedPlayRetryDelay is how long the escape hatch waits for the Connect
// receiver to acknowledge a liked-songs play before re-sending the same
// last-sent context (issue #56). Cluster pushes for a freshly started context
// land well inside this window on a healthy session; a dead context leaves the
// active state untouched.
const likedPlayRetryDelay = 10 * time.Second

// playRetryPending is the armed escape-hatch state of one issued liked-songs
// play: the command's target + exact payload that went out on the wire, plus a
// snapshot of the active playback state at arm time.
// classifyLikedRetryTransition decides what each state movement means: a
// POSITIVE transition disarms the retry, a NEGATIVE clear fires it
// immediately, and an unchanged state defers the decision to the timer.
type playRetryPending struct {
	targetId      string
	targetName    string
	data          ApiRequestDataPlay
	preTrackUri   string
	preContextUri string
	preIsPlaying  bool
}

func activePlaybackState(p *AppPlayer) (trackUri, contextUri string, playing bool) {
	rs := p.state.remoteState
	if rs == nil {
		return "", "", false
	}
	return rs.TrackUri, rs.ContextUri, rs.IsPlaying && !rs.IsPaused
}

// resetLikedSession clears escape-hatch state before a new play request: a
// fresh play supersedes any armed retry (its timeout must not fire against the
// new session's pre-snapshot) and likedSessionActive describes the current
// session only. Run-loop only.
func (p *AppPlayer) resetLikedSession() {
	p.playRetryPending = nil
	p.playRetryTimerCh = nil
	p.likedSessionActive.Store(false)
}

// armLikedPlayRetry snapshots the current active state and arms the one-shot
// escape-hatch retry for an issued liked-songs play. Called from the run loop
// (handleApiRequest); the timer value lands on playRetryTimerCh in the same
// select, so no locking is involved.
func (p *AppPlayer) armLikedPlayRetry(targetId, targetName string, data ApiRequestDataPlay) {
	preTrack, preCtx, prePlaying := activePlaybackState(p)
	p.playRetryPending = &playRetryPending{
		targetId:      targetId,
		targetName:    targetName,
		data:          data,
		preTrackUri:   preTrack,
		preContextUri: preCtx,
		preIsPlaying:  prePlaying,
	}
	p.playRetryTimerCh = time.After(likedPlayRetryDelay)
}

// likedRetryOutcome classifies how the active playback state moved since the
// liked-songs play command was issued (issue #56 fix #8).
type likedRetryOutcome int

const (
	// likedRetryNoChange: the state is identical to the pre-snapshot — the
	// receiver has acknowledged nothing so far; the armed timer decides.
	likedRetryNoChange likedRetryOutcome = iota
	// likedRetryNegative: the state moved into a cleared/stopped shape (empty
	// track uri and not playing) — the receiver rejected the context; the
	// one-shot retry fires immediately.
	likedRetryNegative
	// likedRetryPositive: the state moved into any other shape (a track is
	// active, playback started or resumed, the context switched) — the
	// receiver took the command; the retry must not double-issue.
	likedRetryPositive
)

// classifyLikedRetryTransition compares the current active state with the
// pre-snapshot taken at arm time (issue #56 fix #8). Only a POSITIVE
// transition (a non-empty track uri and/or playback running) suppresses the
// escape-hatch retry; a NEGATIVE clear counts as "the receiver did not take
// the command" and triggers it immediately, while an unchanged state defers
// the decision to the timer.
func classifyLikedRetryTransition(pd *playRetryPending, trackUri, contextUri string, playing bool) likedRetryOutcome {
	if trackUri == pd.preTrackUri && contextUri == pd.preContextUri && playing == pd.preIsPlaying {
		return likedRetryNoChange
	}
	if trackUri == "" && !playing {
		return likedRetryNegative
	}
	return likedRetryPositive
}

// observeLikedRetryTransition feeds one active-state update into the armed
// escape hatch (issue #56): a POSITIVE transition disarms the pending retry
// (the receiver took the command), a NEGATIVE clear fires the one-shot retry
// immediately instead of waiting for the timer, and an unchanged state leaves
// the armed session untouched. Called from updateRemoteState — same run loop
// as arm/handle, so no locking is needed.
func (p *AppPlayer) observeLikedRetryTransition(ctx context.Context) {
	pd := p.playRetryPending
	if pd == nil {
		return
	}
	trackUri, contextUri, playing := activePlaybackState(p)
	switch classifyLikedRetryTransition(pd, trackUri, contextUri, playing) {
	case likedRetryPositive:
		p.playRetryPending = nil
		p.playRetryTimerCh = nil
		p.app.log.Debugf("play: liked-songs retry suppressed — playback started after the command")
	case likedRetryNegative:
		p.app.log.Debugf("play: liked-songs retry firing early — the receiver stopped or cleared the context")
		p.handleLikedPlayRetry(ctx)
	}
}

// handleLikedPlayRetry re-sends the same last-sent liked-songs context once
// (issue #56). It runs either on the armed timer's firing or directly from
// observeLikedRetryTransition when a negative clear transition is observed.
// Either way it first classifies the current state: if a POSITIVE transition
// happened in between (the receiver took the command after all), the retry is
// suppressed so it never double-issues. A bare-fallback context is re-resolved
// here, so an account id that lands between the primary send and the retry
// upgrades the retry to the user-specific uri; otherwise the exact last-sent
// context goes out again. One shot: the armed state is consumed by this call,
// so a second firing — or a firing after a superseding play — is a no-op.
func (p *AppPlayer) handleLikedPlayRetry(ctx context.Context) {
	p.playRetryTimerCh = nil
	pd := p.playRetryPending
	if pd == nil {
		return
	}
	p.playRetryPending = nil

	trackUri, contextUri, playing := activePlaybackState(p)
	if classifyLikedRetryTransition(pd, trackUri, contextUri, playing) == likedRetryPositive {
		p.app.log.Debugf("play: liked-songs retry suppressed — the active state shows playback started")
		return
	}
	// NoChange (timer path: nothing moved for the full window) or Negative
	// (the receiver stopped/cleared): the context did not take.

	data := pd.data
	if data.Uri == likedCollectionUri {
		data.Uri = p.resolvePlayContextUri(data.Uri)
		if data.Uri == likedCollectionUri {
			p.app.log.Warnf("play: liked-songs retry still unresolved — re-sending the bare pseudo context as a best-effort fallback")
		}
	}

	// This is still a liked-songs session: queue expansion must page library
	// tracks even if the Connect state reports a playlist-shaped id for it.
	p.likedSessionActive.Store(true)
	p.app.log.Infof("play: liked-songs retry with %s -> %s", data.Uri, pd.targetName)
	if err := p.sendDeviceCommand(ctx, pd.targetId, pd.targetName, buildPlayCommand(data)); err != nil {
		p.app.log.Warnf("play: liked-songs retry failed: %v", err)
	}
}

// likedSongsMeTimeout caps the one-shot /v1/me account-id lookup (issue #56
// fix #2) — opportunistic work on the play path, same bound as the queue art
// backfill's web api lookups.
const likedSongsMeTimeout = 3 * time.Second

// spotifyAccountId resolves the Spotify account id of the paired user.
// Resolution order (issue #56 fix #2, last-resort tier added by fix #6):
//  1. the cached/persisted account id (set below once, survives restarts)
//  2. one bounded Web API GET /v1/me with the same token the library and
//     cover-art lookups already use successfully — the device-flow token is
//     opaque so nothing can be decoded from it locally; on success the id is
//     cached + persisted
//  3. the JWT `sub` claim of the access token (secondary fallback for tokens
//     that are still JWTs)
//  4. the paired account's stored credentials username (last resort while
//     /v1/me stays rate-limited; see credentialsAccountID)
func (p *AppPlayer) spotifyAccountId() string {
	if id := p.cachedAccountID(); id != "" {
		return id
	}

	fetch := p.webMeAccountFn
	if fetch == nil {
		fetch = p.webApiMeAccountID
	}
	ctx, cancel := context.WithTimeout(context.Background(), likedSongsMeTimeout)
	id := fetch(ctx)
	cancel()
	if id != "" {
		p.setAccountID(id) // cache + persist for the next play and restart
		return id
	}

	// secondary fallback: tokens that are still JWTs (older auth paths)
	p.app.state.Lock()
	token := p.app.state.OAuth.AccessToken
	p.app.state.Unlock()
	if id := jwtSubClaim(token); id != "" {
		return id
	}

	// last resort (issue #56 fix #6): the paired account's stored credentials
	// username, valid while /v1/me stays rate-limited and no other source
	// produced an id
	if id := p.credentialsAccountID(); id != "" {
		p.app.log.Debugf("play: using credentials username %s as the liked-songs account id (web api /v1/me unresolved)", id)
		return id
	}

	return ""
}

// credentialsAccountID returns the paired account's username as stored at
// pairing time (app.go withCredentials copies sess.Username() into
// state.Credentials). That value is the accesspoint's CanonicalUsername —
// filled in server-side by Spotify's APWelcome packet for every token-based
// flow this daemon supports (device-flow QR, stored-credential replay), never
// typed by the user — and on the paired devices it IS the Spotify account id
// that spotify:user:<id>:collection accepts. We therefore treat it as a
// valid id unless it is empty or email-shaped ("@" present, cf.
// ObfuscateUsername's own heuristic). It is deliberately NOT persisted into
// state.AccountID: that field marks an authoritative /v1/me resolution and
// also stops the background resolver's retries, which should keep running so
// a later successful lookup still lands.
func (p *AppPlayer) credentialsAccountID() string {
	p.app.state.Lock()
	user := strings.TrimSpace(p.app.state.Credentials.Username)
	p.app.state.Unlock()
	if user == "" || strings.Contains(user, "@") {
		return ""
	}
	return user
}

// cachedAccountID returns the persisted account id or "" when it has not
// been resolved yet (fresh install, or state written before fix #2)
func (p *AppPlayer) cachedAccountID() string {
	p.app.state.Lock()
	id := p.app.state.AccountID
	p.app.state.Unlock()
	return id
}

// setAccountID caches + persists the resolved account id so the /v1/me
// lookup happens at most once per paired account. House pattern: mutate
// under the state lock, persist outside it (cf. onOAuthTokenChanged).
func (p *AppPlayer) setAccountID(id string) {
	p.app.state.Lock()
	p.app.state.AccountID = id
	p.app.state.Unlock()
	if err := p.app.persistState(); err != nil {
		p.app.log.Warnf("play: failed to persist the liked-songs account id: %v", err)
	}
}

// webApiMeAccountID performs one Web API GET /v1/me with the session's OAuth
// token (the same opaque device-flow token the library and cover-art lookups
// already succeed with) and returns the account id from the response. No new
// endpoint or token is involved: only our own session token is sent, and only
// the `id` of our own account is ever read.
func (p *AppPlayer) webApiMeAccountID(ctx context.Context) string {
	if p.sess == nil {
		return ""
	}
	p.app.state.Lock()
	token := p.app.state.OAuth.AccessToken
	p.app.state.Unlock()
	if token == "" {
		return ""
	}

	resp, err := p.sess.WebApi(ctx, "GET", "/v1/me", nil, nil, nil)
	if err != nil {
		p.app.log.Debugf("play: web api /v1/me failed: %v", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.app.log.Debugf("play: web api /v1/me returned status %d", resp.StatusCode)
		return ""
	}

	var me struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		p.app.log.Debugf("play: web api /v1/me decode failed: %v", err)
		return ""
	}
	return me.ID
}

// issue #56 fix #4 + fix #5: pacing of the background account-id resolver.
// During a Spotify rate-limit window the play path's ~3s /v1/me call
// deterministically times out (spclient's 429 backoffs are 37-40s each, 5
// retries max, a ~7s burst — always longer than the deadline), so the id never
// persists and every liked-songs play is skipped (fix #5) until an attempt
// lands after the window closes. The resolver below moves that work out of the
// play path: one generous deadline per attempt (it covers a full retry burst),
// a retry delay that doubles per consecutive failure (30s start, 5min cap),
// and — because chronic rate-limiting can outlast any fixed attempt count —
// a wall-clock budget instead of the old 20-attempt hard cap (fix #5).
const (
	accountIDAttemptTimeout      = 20 * time.Second // > spclient's ~7s worst-case retry burst
	defaultAccountIDBudget       = 24 * time.Hour   // fix #5: wall-clock budget per resolver run (replaces the fix-#4 20-attempt cap)
	defaultAccountIDInitialDelay = 30 * time.Second // fix #5: first retry delay, doubles per consecutive failure
	accountIDMaxRetryDelay       = 5 * time.Minute  // fix #5: cap on the doubling retry delay
)

// resolveAccountIDInBackground fetches the Spotify account id so the play path
// hits its cache (issue #56 fix #4, budgeted by fix #5). Each attempt gets a
// 20s deadline — long enough for spclient's worst-case ~7s 429 retry burst to
// finish. Failed attempts are retried until the id is persisted: the retry
// delay doubles per consecutive failure (30s start, 5min cap) inside a
// wall-clock budget (default 24h); a non-zero legacy attempt cap
// (accountIDMaxAttempts) still stops the loop as well, whichever bound comes
// first. Single-flight: app start (Run) and an OAuth re-pair
// (onOAuthTokenChanged) may both trigger it, but the guard lets only one loop
// run; it resets when the loop ends so a later trigger can start again. stop
// wakes the retry delay at shutdown / player teardown; the loop also exits on
// the first successful fetch and whenever the id is already cached.
func (p *AppPlayer) resolveAccountIDInBackground(stop <-chan struct{}) {
	if !p.accountIDResolverRunning.CompareAndSwap(false, true) {
		return // another loop is already running
	}
	defer p.accountIDResolverRunning.Store(false)

	fetch := p.webMeAccountFn
	if fetch == nil {
		fetch = p.webApiMeAccountID
	}

	// fix #5: the budget is the primary bound; the legacy attempt cap (fix #4
	// shape, set only by tests/callers) still applies when non-zero.
	budget := p.accountIDBudget
	if budget <= 0 {
		budget = defaultAccountIDBudget
	}
	maxAttempts := p.accountIDMaxAttempts

	initialDelay := p.accountIDInitialDelay
	if initialDelay <= 0 {
		initialDelay = p.accountIDRetryDelay // fix #4 fallback
	}
	if initialDelay <= 0 {
		initialDelay = defaultAccountIDInitialDelay
	}
	maxDelay := p.accountIDMaxDelay
	if maxDelay <= 0 {
		maxDelay = accountIDMaxRetryDelay
	}

	start := time.Now()
	delay := initialDelay
	attempt := 0
	for {
		gaveUp := ""
		switch {
		case maxAttempts > 0 && attempt >= maxAttempts:
			gaveUp = "attempt cap"
		case !time.Now().Before(start.Add(budget)):
			gaveUp = "budget exhausted"
		}
		if gaveUp != "" {
			p.app.log.Warnf("play: background account-id resolution gave up after %d attempts (%s elapsed of the %s budget, %s) (rate limited?)",
				attempt, time.Since(start).Round(time.Second), budget, gaveUp)
			return
		}
		attempt++

		if id := p.cachedAccountID(); id != "" {
			return // resolved in the meantime (play path or an earlier run)
		}

		ctx, cancel := context.WithTimeout(context.Background(), accountIDAttemptTimeout)
		id := fetch(ctx)
		cancel()
		if id != "" {
			p.setAccountID(id)
			p.app.log.Infof("play: persisted account id %s from web api /v1/me (background)", id)
			return
		}

		if attempt%10 == 0 {
			p.app.log.Infof("play: background account-id attempt %d failed, %s elapsed, retrying in %s",
				attempt, time.Since(start).Round(time.Second), delay)
		} else {
			p.app.log.Debugf("play: background account-id attempt %d failed, retrying in %s", attempt, delay)
		}

		select {
		case <-stop:
			return
		case <-time.After(delay):
		}

		// ramp the delay per consecutive failure, capped (fix #5)
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// jwtSubClaim extracts the `sub` claim from a three-segment JWT payload.
// Returns "" for anything that is not a decodable JWT with a string sub.
func jwtSubClaim(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

// buildShuffleCommand assembles the connect set_options command for the
// shuffle state (issue #39, verified wire protocol from the official Linux
// client bundle + production spotiplay — NOT the legacy set_shuffling_context
// endpoint, which 400s). Both fields are ALWAYS present so the receiver never
// has to guess which half of the state the sender is talking about:
//
//	state        shuffling_context  modes.context_enhancement
//	off          false              "NONE"
//	plain on     true               "NONE"
//	smart on     true               "RECOMMENDATION"
//
// Pure function of the request data so the wire contract stays unit-testable
// without a live cluster.
func buildShuffleCommand(data ApiRequestDataShuffle) connectCommand {
	shuffling := false
	enhancement := "NONE"
	if data.Shuffle {
		shuffling = true
		if data.Smart != nil && *data.Smart {
			enhancement = "RECOMMENDATION"
		}
	}
	return connectCommand{
		Endpoint:         "set_options",
		ShufflingContext: &shuffling,
		Modes:            map[string]string{smartShuffleModeKey: enhancement},
	}
}

// sendActiveDeviceCommand sends to the active device in the user's cluster
func (p *AppPlayer) sendActiveDeviceCommand(ctx context.Context, cmd connectCommand) error {
	rs := p.state.remoteState
	if rs == nil || rs.DeviceId == "" {
		return fmt.Errorf("no active remote device known yet")
	}
	// always target the current active device
	return p.sendDeviceCommand(ctx, rs.DeviceId, rs.DeviceName, cmd)
}

// sendDeviceCommand sends a connect player command to an explicit device id.
func (p *AppPlayer) sendDeviceCommand(ctx context.Context, deviceId, deviceName string, cmd connectCommand) error {
	if p.sendDeviceCommandFn != nil {
		return p.sendDeviceCommandFn(ctx, deviceId, deviceName, cmd)
	}
	if deviceId == "" {
		return fmt.Errorf("no target device")
	}
	if deviceId == p.app.deviceId {
		return fmt.Errorf("target device is us; cannot remote-control self")
	}
	if !p.hasSpotConnId {
		return fmt.Errorf("dealer not connected (no spotify-connection-id)")
	}

	body, err := json.Marshal(connectCommandEnvelope{Command: cmd})
	if err != nil {
		return fmt.Errorf("marshal connect command: %w", err)
	}

	if err := p.sess.Spclient().SendPlayerCommand(ctx, p.app.deviceId, deviceId, p.spotConnId, body); err != nil {
		return fmt.Errorf("send %s to %s: %w", cmd.Endpoint, deviceId, err)
	}
	p.app.log.Debugf("observer: sent %s to %s (%s)", cmd.Endpoint, deviceId, deviceName)
	if cmd.Endpoint == "set_options" {
		// issue #39 instrumentation: log the full outgoing shuffle envelope
		// (command-level, not per-poll) for on-device correlation.
		p.app.log.Debugf("observer: set_options shuffle envelope to %s (%s): %s", deviceId, deviceName, string(body))
	}
	return nil
}

// resumeLastDevice resumes playback on the most-recent active device
func (p *AppPlayer) resumeLastDevice(ctx context.Context) error {
	targetId := p.state.lastActiveDeviceId
	if targetId == "" {
		return ErrNotFound
	}

	present := false
	for _, d := range p.state.connectDevices {
		if d.Id == targetId {
			present = true
			break
		}
	}
	if !present {
		return ErrNotFound
	}

	return p.sendDeviceCommand(ctx, targetId, p.state.lastActiveDeviceName, connectCommand{Endpoint: "resume"})
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

type transferOptions struct {
	RestorePaused string `json:"restore_paused"`
}

type transferBody struct {
	TransferOptions transferOptions `json:"transfer_options"`
	InteractionId   string          `json:"interaction_id"`
	CommandId       string          `json:"command_id"`
}

// moves the current playback session to targetDeviceId
func (p *AppPlayer) sendTransfer(ctx context.Context, targetDeviceId string) error {
	if targetDeviceId == "" {
		return fmt.Errorf("transfer requires a target device id")
	}
	if targetDeviceId == p.app.deviceId {
		return fmt.Errorf("cannot transfer to ourselves (observer cannot play)")
	}
	if !p.hasSpotConnId {
		return fmt.Errorf("dealer not connected (no spotify-connection-id)")
	}

	body, err := json.Marshal(transferBody{
		TransferOptions: transferOptions{RestorePaused: "restore"},
		InteractionId:   randomCommandId(),
		CommandId:       randomCommandId(),
	})
	if err != nil {
		return fmt.Errorf("marshal transfer: %w", err)
	}

	if err := p.sess.Spclient().TransferConnect(ctx, p.app.deviceId, targetDeviceId, p.spotConnId, body); err != nil {
		return fmt.Errorf("transfer to %s: %w", targetDeviceId, err)
	}
	p.app.log.Debugf("observer: transferred playback to %s", targetDeviceId)
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
	select {
	case p.stop <- struct{}{}:
	default:
	}
	p.sess.Close()
}

// handleLyricsAsync resolves the track metadata synchronously
// keeps slow lyrics HTTP off the player loop so it never stalls the dealer keepalive/messages
func (p *AppPlayer) handleLyricsAsync(ctx context.Context, req ApiRequest) {
	data := req.Data.(ApiRequestDataLyrics)

	// fetch the synced transcript by episode id
	if data.Episode {
		lp := p.lyricsProvider
		go func() {
			result, err := lp.FetchEpisodeText(ctx, data.TrackId)
			if err != nil {
				if errors.Is(err, ErrNoLyrics) {
					req.Reply(nil, ErrNotFound)
					return
				}
				req.Reply(nil, fmt.Errorf("episode text fetch failed: %w", err))
				return
			}
			req.Reply(result, nil)
		}()
		return
	}

	trackName := data.TrackName
	artistName := data.ArtistName
	albumName := data.AlbumName
	durationMs := data.DurationMs

	if trackName == "" || artistName == "" {
		if rs := p.state.remoteState; rs != nil {
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
		req.Reply(nil, fmt.Errorf("track name required (provide ?track= param or wait for observer state)"))
		return
	}

	lp := p.lyricsProvider
	go func() {
		result, err := lp.FetchLyrics(ctx, data.TrackId, trackName, artistName, albumName, durationMs, data.Richsync)
		if err != nil {
			if errors.Is(err, ErrNoLyrics) {
				req.Reply(nil, ErrNotFound)
				return
			}
			req.Reply(nil, fmt.Errorf("lyrics fetch failed: %w", err))
			return
		}
		req.Reply(result, nil)
	}()
}

func (p *AppPlayer) Run(ctx context.Context, apiRecv <-chan ApiRequest) {
	// Signal the API server that we are now consuming the request channel
	p.app.server.SetPlayerReady(true)
	defer p.app.server.SetPlayerReady(false)

	// expose ourselves to app level actions
	p.app.currentPlayer.Store(p)
	defer p.app.currentPlayer.CompareAndSwap(p, nil)

	// issue #56 fix #4: under Spotify rate limiting the play path's ~3s /v1/me
	// call times out before spclient's 429 backoff can finish, so fetch the
	// account id in the background — liked-songs plays then hit the cache.
	if p.cachedAccountID() == "" {
		go p.resolveAccountIDInBackground(ctx.Done())
	}

	p.lyricsProvider = NewLyricsProvider(p.app.log, func(ctx context.Context, force bool) (string, error) {
		return p.sess.Spclient().GetAccessToken(ctx, force)
	})
	p.app.log.Infof("lyrics provider initialized")

	p.queueResolver = newQueueResolver(p.app.log, p.sess.Spclient(), p.queueResolvedCh, p.app.cfg.ImageSize, p.queueWebArt)
	p.metaResolvedCh = make(chan resolvedTrackMeta, 4)

	apRecv := p.sess.Accesspoint().Receive(ap.PacketTypeProductInfo, ap.PacketTypeCountryCode)
	msgRecv := p.sess.Dealer().ReceiveMessage("hm://pusher/v1/connections/", "hm://connect-state/v1/")
	reqRecv := p.sess.Dealer().ReceiveRequest("hm://connect-state/v1/player/command")

	heartbeat := time.NewTicker(connectStateHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			p.sess.Close()
			return
		case <-p.stop:
			return
		case <-heartbeat.C:
			p.heartbeatConnectState()
		case pkt, ok := <-apRecv:
			if !ok {
				p.app.log.Warnf("accesspoint receiver closed")
				apRecv = nil
				continue
			}
			if err := p.handleAccesspointPacket(pkt.Type, pkt.Payload); err != nil {
				p.app.log.Warnf("failed handling accesspoint packet: %v", err)
			}
		case msg, ok := <-msgRecv:
			if !ok {
				p.app.log.Warnf("dealer message receiver closed")
				msgRecv = nil
				continue
			}
			if err := p.handleDealerMessage(ctx, msg); err != nil {
				p.app.log.Warnf("failed handling dealer message: %v", err)
			}
		case cluster := <-p.clusterCh:
			// cluster snapshot from a put_state response
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			p.handleCluster(cctx, cluster)
			cancel()
		case req, ok := <-reqRecv:
			if !ok {
				p.app.log.Warnf("dealer request receiver closed")
				reqRecv = nil
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
				apiRecv = nil
				continue
			}
			// fetching lyrics sometimes block the HTTP connection between the ui
			// for seconds at a time in cases where fetching lyrics take too long (no lyrics for several songs in succession)
			// having it inlione stalls this loop that keeps the dealer keepalive/messages
			if req.Type == ApiRequestTypeLyrics {
				p.handleLyricsAsync(ctx, req)
				continue
			}
			data, err := p.handleApiRequest(ctx, req)
			req.Reply(data, err)
		case <-p.playRetryTimerCh:
			// issue #56 fix #7: the armed escape-hatch retry for an issued
			// bare liked-songs play is due (no active state change since the
			// command — re-send with the resolved user-specific collection uri)
			p.handleLikedPlayRetry(ctx)
		case meta := <-p.metaResolvedCh:
			// background current track metadata
			if p.metaResolveInFlight == meta.uri {
				p.metaResolveInFlight = ""
			}
			if meta.artist == "" && meta.name == "" && meta.imageUrl == "" {
				p.metaResolveFailedUri = meta.uri
			}
			if rs := p.state.remoteState; rs != nil && rs.TrackUri == meta.uri {
				updated := *rs
				changed := false
				if meta.artist != "" && updated.TrackArtist == "" {
					updated.TrackArtist = meta.artist
					changed = true
				}
				if updated.TrackAlbum == "" {
					if alb := firstNonEmpty(meta.album, updated.RawMetadata["album_title"]); alb != "" {
						updated.TrackAlbum = alb
						changed = true
					}
				}
				if meta.name != "" && updated.TrackName == "" {
					updated.TrackName = meta.name
					changed = true
				}
				if meta.imageUrl != "" && updated.TrackImageUrl == "" {
					updated.TrackImageUrl = meta.imageUrl
					changed = true
				}
				if changed {
					updated.Position = updated.RemotePosition()
					p.state.remoteState = &updated
					p.app.server.Emit(&ApiEvent{
						Type: ApiEventTypeObserverStateChanged,
						Data: &updated,
					})
				}
			}
		case <-p.queueResolvedCh:
			// Background queue-metadata resolution landed
			if rs := p.state.remoteState; rs != nil && p.queueResolver != nil {
				updated := *rs
				updated.NextTracks = append([]QueueTrack(nil), rs.NextTracks...)
				updated.PrevTracks = append([]QueueTrack(nil), rs.PrevTracks...)
				p.queueResolver.applyCache(updated.NextTracks)
				p.queueResolver.applyCache(updated.PrevTracks)
				// issue #50: newly resolved covers land in the cache too — apply
				// them to the first-N cards (schedule=false: everything here was
				// already scheduled in updateRemoteState, re-scheduling would
				// just leak stale pending markers)
				p.queueResolver.applyArtBackfill(updated.NextTracks, false)
				updated.Position = updated.RemotePosition()
				p.state.remoteState = &updated
				p.app.server.Emit(&ApiEvent{
					Type: ApiEventTypeObserverStateChanged,
					Data: &updated,
				})
			}
		case res := <-p.queueExpandedCh:
			// bug32: a background fetch of the active context's track list
			// landed — the cache now holds it, so re-derive the full upcoming
			// queue for the current remote state (expandQueue hits the cache
			// path; no new fetch) and re-emit so the UI picks it up without
			// waiting for the next cluster push
			p.app.log.Debugf("queue expand: result for %s received (%d tracks)", res.contextUri, res.total)
			if rs := p.state.remoteState; rs != nil {
				before := len(rs.NextTracks)
				p.expandQueue(rs)
				if len(rs.NextTracks) != before {
					updated := *rs
					updated.NextTracks = append([]QueueTrack(nil), rs.NextTracks...)
					updated.Position = updated.RemotePosition()
					p.state.remoteState = &updated
					p.app.server.Emit(&ApiEvent{
						Type: ApiEventTypeObserverStateChanged,
						Data: &updated,
					})
				}
			}
		}
	}
}
