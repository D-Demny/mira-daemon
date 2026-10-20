package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/devgianlu/go-librespot/ma"
)

// issue #134 (daemon part 2): the Music Assistant observer and control
// mapping, on top of the settings-blob config from ma_config.go.
//
// Design (locked in issue #133/#134): the daemon observes exactly ONE MA
// player - the one selected through POST /ma/player/select - by polling
// players/get once per second while playbackSource == "ma" and a usable
// ma config exists (maModeActive). Every poll is mapped into the SAME
// RemoteState shape the Spotify Connect observer produces, and emits the
// SAME event types (ObserverTrackChanged / ObserverStateChanged /
// ObserverInactive via clearActiveDevice), so /observer/status and the UI
// need no new surface: the existing "observed remote playback" contract
// just changes its source of truth.
//
// Locking: every MA touch point (the poll tick, the /ma/* request cases,
// the control-verb preludes) runs inside the single Run() goroutine - all
// API requests are dispatched through the requests channel - so the MA
// fields on AppPlayer are deliberately lock-free. Keep it that way: never
// read or write them from another goroutine.
//
// The WebSocket event stream is out of scope (v1 is polling only).

const maPollInterval = time.Second

// maPollRequestTimeout bounds one poll request; staying under the client's
// own 5s timeout keeps a hung server from stretching the effective poll
// interval by more than a couple of ticks.
const maPollRequestTimeout = 4 * time.Second

// maControlTimeout bounds control command round-trips (well inside the
// 30s request timeout the API server gives each request).
const maControlTimeout = 10 * time.Second

const maDeviceType = "MUSIC_ASSISTANT"

// MaDevice is one entry of GET /ma/devices - the pinned contract shape:
// id/name/availability from MA's players/all (issue #134).
type MaDevice struct {
	Id        string `json:"id"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
}

// maResolved returns the parsed MA config and playbackSource for the
// currently mirrored settings blob.
func (p *AppPlayer) maResolved() (*MaConfig, string) {
	blob := p.app.GetSettings()
	return ParseMaConfig(blob), playbackSourceFromSettings(blob)
}

// maModeActive is true when the blob selects the Music Assistant backend
// AND carries a usable ma config - i.e. when the daemon observes MA
// instead of the Spotify Connect cluster (issue #134).
func (p *AppPlayer) maModeActive() bool {
	cfg, src := p.maResolved()
	return cfg != nil && src == PlaybackSourceMA
}

// maClientFor returns a *ma.Client for cfg, cached while url+token stay
// unchanged so the 1/s poll doesn't rebuild a client (and its transport)
// every tick. Run-loop only (see file header).
func (p *AppPlayer) maClientFor(cfg *MaConfig) *ma.Client {
	if p.maClient == nil || p.maClientURL != cfg.URL || p.maClientToken != cfg.Token {
		p.maClient = ma.NewClient(cfg.URL, cfg.Token)
		p.maClientURL = cfg.URL
		p.maClientToken = cfg.Token
	}
	return p.maClient
}

// maControlTarget resolves the client + selected player for a control
// command. Unconfigured MA and "no player selected yet" are client errors
// (ErrBadRequest -> HTTP 400) per the pinned contract.
func (p *AppPlayer) maControlTarget() (*ma.Client, string, error) {
	cfg, _ := p.maResolved()
	if cfg == nil {
		return nil, "", ErrBadRequest
	}
	if p.maActivePlayerId == "" {
		return nil, "", ErrBadRequest
	}
	return p.maClientFor(cfg), p.maActivePlayerId, nil
}

// maPollTick is the 1/s Music Assistant observer poll (issue #134). Run
// loop only. When MA mode is off or no player is selected, it releases
// any state a previous MA session left behind so the Spotify cluster path
// (or idle) owns remoteState again.
func (p *AppPlayer) maPollTick(ctx context.Context) {
	cfg, src := p.maResolved()
	if cfg == nil || src != PlaybackSourceMA || p.maActivePlayerId == "" {
		if p.maOwnsRemoteState {
			p.maOwnsRemoteState = false
			p.clearActiveDevice()
		}
		return
	}

	client := p.maClientFor(cfg)
	pctx, cancel := context.WithTimeout(ctx, maPollRequestTimeout)
	defer cancel()

	pl, err := client.GetPlayer(pctx, p.maActivePlayerId)
	if err != nil {
		// log the first failure at warning level, then stay quiet (Debug)
		// until recovery - a bad token or unreachable server would
		// otherwise spam once per second
		if p.maPollFailing {
			p.app.log.Debugf("ma: players/get %s still failing: %v", p.maActivePlayerId, err)
		} else {
			p.app.log.Warnf("ma: players/get %s failed: %v", p.maActivePlayerId, err)
		}
		p.maPollFailing = true
		return
	}
	if p.maPollFailing {
		p.app.log.Infof("ma: player %q reachable again", p.maActivePlayerId)
	}
	p.maPollFailing = false

	p.applyMaState(pl)
}

// applyMaState maps one polled MA player into the shared RemoteState -
// same shape, same clock-timing notes, same events as updateRemoteState -
// so /observer/status and the event stream stay source-agnostic.
// issue #134.
func (p *AppPlayer) applyMaState(pl *ma.Player) {
	// Idle, or no media at all, is "nothing playing on this speaker" - the
	// MA equivalent of the Spotify cluster's empty ActiveDeviceId: clear +
	// observer_inactive. Gate on PlaybackState FIRST: live players keep a
	// STALE non-null current_media after going idle (verified against
	// 2.10.7), so the media pointer alone is not an idle signal.
	if pl.PlaybackState == "idle" || pl.CurrentMedia == nil {
		p.maOwnsRemoteState = false
		p.clearActiveDevice()
		return
	}

	media := pl.CurrentMedia
	rs := &RemoteState{
		DeviceId:      pl.PlayerID,
		DeviceName:    pl.Name,
		DeviceType:    maDeviceType,
		TrackUri:      media.URI,
		TrackName:     media.Title,
		TrackArtist:   media.Artist,
		TrackAlbum:    media.Album,
		TrackImageUrl: media.ImageURL,
		Duration:      int64(media.Duration) * 1000, // MA seconds -> RemoteState ms
		IsPlaying:     pl.PlaybackState == "playing",
		IsPaused:      pl.PlaybackState == "paused",
		Volume:        maVolumeToState(pl.VolumeLevel),
		VolumeSteps:   100,
	}
	if pl.PlaybackState == "playing" {
		rs.PlaybackSpeed = 1
	}
	if pl.ElapsedTime != nil && pl.ElapsedTimeLastUpdated != nil {
		// MA reports position in seconds and the observation instant in
		// epoch SECONDS; RemoteState uses unix MILLISECONDS throughout.
		rs.PositionAsOfTimestamp = int64(*pl.ElapsedTime * 1000)
		rs.Timestamp = int64(*pl.ElapsedTimeLastUpdated * 1000)
	}
	now := time.Now()
	rs.ReceivedAt = now
	rs.ReceivedAtWallMs = now.UnixMilli()

	prevState := p.state.remoteState
	// a state handed over from the other backend (flag unset) counts as a
	// new track: the UI must re-render title/art/position on the switch
	trackChanged := prevState == nil || !p.maOwnsRemoteState || prevState.TrackUri != rs.TrackUri

	// same clock-offset evidence as the cluster path: local wall ms minus
	// server timestamp, deduped by Timestamp (noteClusterTiming)
	p.noteClusterTiming(rs)

	p.state.remoteState = rs
	p.maOwnsRemoteState = true
	if rs.DeviceId != "" {
		p.state.lastActiveDeviceId = rs.DeviceId
		p.state.lastActiveDeviceName = rs.DeviceName
	}

	if v := p.app.voice; v != nil {
		v.notifyPlayback(rs.IsPlaying && !rs.IsPaused)
	}

	if trackChanged {
		p.app.log.Debugf("ma observer: track changed to %q by %s on %s", rs.TrackName, rs.TrackArtist, rs.DeviceName)
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

// maVolumeToState maps MA's 0..100 level into the shared 0..MaxStateVolume
// scale used by RemoteState.Volume (nil/negative -> 0, clamped to 100).
func maVolumeToState(level *int) uint32 {
	if level == nil || *level < 0 {
		return 0
	}
	v := *level
	if v > 100 {
		v = 100
	}
	return uint32(v * MaxStateVolume / 100)
}

// maTargetToLevel maps a shared-scale (0..MaxStateVolume) volume target back
// to MA's 0..100 for players/cmd/volume_set.
func maTargetToLevel(target int64) int {
	return int(target * 100 / MaxStateVolume)
}

// maSend routes one control verb to the selected MA player (issue #134).
// The verbs mirror the existing /player/* request types; see the switch.
func (p *AppPlayer) maSend(ctx context.Context, verb string, data any) error {
	client, playerID, err := p.maControlTarget()
	if err != nil {
		return err
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()

	var callErr error
	switch verb {
	case "resume", "play":
		// resume/resume_last carry Spotify Connect semantics ("keep playing
		// on the target device"); MA's Play is the equivalent - it resumes
		// an idle player and no-ops a playing one (issue #134 mapping).
		callErr = client.Play(pctx, playerID)
	case "pause":
		callErr = client.Pause(pctx, playerID)
	case "playpause":
		callErr = client.PlayPause(pctx, playerID)
	case "next":
		callErr = client.Next(pctx, playerID)
	case "prev":
		callErr = client.Previous(pctx, playerID)
	case "seek":
		seek, _ := data.(ApiRequestDataSeek)
		if seek.Relative {
			return fmt.Errorf("relative seek not supported in observer mode")
		}
		callErr = client.Seek(pctx, playerID, int(seek.Position/1000)) // ms -> s
	case "volume":
		vol, _ := data.(ApiRequestDataVolume)
		target := int64(vol.Volume)
		if vol.Relative {
			// same base as the Spotify path: last known state volume (in MA
			// mode that is the mapped MA level from the last poll)
			base := int64(0)
			if rs := p.state.remoteState; rs != nil {
				base = int64(rs.Volume)
			}
			target = base + int64(vol.Volume)
		}
		if target < 0 {
			target = 0
		}
		if target > MaxStateVolume {
			target = MaxStateVolume
		}
		callErr = client.VolumeSet(pctx, playerID, maTargetToLevel(target))
	case "shuffle":
		shuf, _ := data.(ApiRequestDataShuffle)
		// smart shuffle has no MA equivalent; only the plain flag applies
		// in v1 (issue #134 mapping).
		callErr = client.Shuffle(pctx, playerID, shuf.Shuffle)
	case "repeat_context":
		val, _ := data.(bool)
		mode := ma.RepeatOff
		if val {
			mode = ma.RepeatAll
		}
		callErr = client.Repeat(pctx, playerID, mode)
	case "repeat_track":
		val, _ := data.(bool)
		mode := ma.RepeatOff
		if val {
			mode = ma.RepeatOne
		}
		callErr = client.Repeat(pctx, playerID, mode)
	default:
		return fmt.Errorf("ma: unsupported control verb %q", verb)
	}

	if callErr != nil && !errors.Is(callErr, context.Canceled) {
		p.app.log.Warnf("ma: %s for player %q failed: %v", verb, playerID, callErr)
	}
	return callErr
}

// maListDevices backs GET /ma/devices (issue #134, pinned contract):
// 200 {"devices":[...]} from players/all; an EMPTY array - never an error -
// when MA is unconfigured or unreachable.
func (p *AppPlayer) maListDevices(ctx context.Context) (any, error) {
	devs := []MaDevice{}
	cfg, _ := p.maResolved()
	if cfg == nil {
		return map[string]any{"devices": devs}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()
	players, err := p.maClientFor(cfg).ListPlayers(pctx)
	if err != nil {
		p.app.log.Warnf("ma: players/all failed: %v", err)
		return map[string]any{"devices": devs}, nil
	}
	for _, pl := range players {
		devs = append(devs, MaDevice{Id: pl.PlayerID, Name: pl.Name, Available: pl.Available})
	}
	return map[string]any{"devices": devs}, nil
}

// maSelectPlayer backs POST /ma/player/select (issue #134, pinned contract):
// the player must exist in players/all (else 400), MA must be configured
// (else 400). On success it persists the selection the same way the daemon
// remembers the last Spotify device (in-memory per session) and starts
// observing: the next poll tick applies the MA state within ~1s.
func (p *AppPlayer) maSelectPlayer(ctx context.Context, playerID string) (any, error) {
	cfg, _ := p.maResolved()
	if cfg == nil {
		return nil, ErrBadRequest
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()
	players, err := p.maClientFor(cfg).ListPlayers(pctx)
	if err != nil {
		p.app.log.Warnf("ma: players/all failed during player select: %v", err)
		return nil, fmt.Errorf("music assistant unreachable: %w", err)
	}

	var name string
	found := false
	for _, pl := range players {
		if pl.PlayerID == playerID {
			found = true
			name = pl.Name
			break
		}
	}
	if !found {
		return nil, ErrBadRequest
	}

	p.maActivePlayerId = playerID
	p.app.log.Infof("ma: selected player %s (%s)", playerID, name)
	return map[string]any{"ok": true}, nil
}
