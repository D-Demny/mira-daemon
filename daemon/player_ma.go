package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
// loop only. When MA mode is off, it releases state a previous MA session
// left behind so the Spotify cluster path owns remoteState again. When MA
// mode is on but no player is selected, it releases state from ANY
// previous source - the stale-ownership gate below would otherwise keep
// showing a Spotify session observed before the switch, and the UI would
// stay on the now-playing screen with the Spotify device list instead of
// falling back to the idle screen with the MA picker. (issue #134)
func (p *AppPlayer) maPollTick(ctx context.Context) {
	cfg, src := p.maResolved()
	if cfg == nil || src != PlaybackSourceMA || p.maActivePlayerId == "" {
		release := p.maOwnsRemoteState
		// MA mode is the single source of truth: with no player selected
		// there is nothing observable, so release foreign (e.g. Spotify)
		// state even when MA never owned remoteState itself.
		if src == PlaybackSourceMA {
			release = true
		}
		if release {
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

	// issue #138: native group self-heal - if the observed player became a
	// firmware slave since selection (an HA automation formed the WiiM group
	// around playback while we looked at it), retarget to its master: in a
	// NATIVE group the music lives on the master and mirrors to the slaves,
	// so the slave's own state is not the truth. The next tick polls the
	// master; control already resolves through maResolvePlayerID.
	if pl.SyncedTo != nil && *pl.SyncedTo != "" && *pl.SyncedTo != p.maActivePlayerId {
		p.app.log.Infof("ma: player %s is synced to %s - following the group master", pl.PlayerID, *pl.SyncedTo)
		p.maActivePlayerId = *pl.SyncedTo
		return
	}

	// issue #138: upcoming queue for the "Läuft gerade" pane. Fetched only
	// while media is active (idle players have nothing queued) on the same
	// 4s poll budget; the queue response itself carries NO item list, so the
	// list is RECONSTRUCTED from source + index below (maUpcomingTracks),
	// which also keeps the last good list on a transient fetch failure.
	var queue *ma.PlayerQueue
	if pl.PlaybackState != "idle" && pl.CurrentMedia != nil {
		q, qerr := client.GetQueue(pctx, p.maActivePlayerId)
		queue = q
		if qerr != nil {
			p.app.log.Debugf("ma: player_queues/get %s failed: %v", p.maActivePlayerId, qerr)
		}
	}

	p.applyMaState(pl, queue, p.maUpcomingTracks(pctx, client, pl, queue))
}

// applyMaState maps one polled MA player into the shared RemoteState -
// same shape, same clock-timing notes, same events as updateRemoteState -
// so /observer/status and the event stream stay source-agnostic.
// issue #134.
func (p *AppPlayer) applyMaState(pl *ma.Player, queue *ma.PlayerQueue, upcoming []QueueTrack) {
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
	// issue #138: upcoming queue for the "Läuft gerade" pane. A nil queue
	// (transient player_queues/get failure while media is active) must not
	// blank the pane: carry the previous list over while we still own state
	// on this device. A NON-nil queue with an empty upcoming list is real
	// "nothing queued behind" (single-track source, shuffled order, ...) -
	// the pane goes blank on purpose there. MA reports no previous track at
	// all (no previous_track field on the wire, verified against 2.10.7), so
	// PrevTracks stays empty by design.
	if queue != nil {
		rs.NextTracks = upcoming
	} else if prevState != nil && p.maOwnsRemoteState && prevState.DeviceId == rs.DeviceId {
		rs.NextTracks = prevState.NextTracks
	}
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

// maGroupIDPrefix marks the synthetic /ma/devices entry of a native
// (firmware-level) group; selecting it resolves to the group master, whose
// WiiM firmware mirrors playback to every slave natively (issue #138 - the
// "Wiim Wohnzimmer (Gruppe)" the user asked for; deliberately NOT an MA
// virtual group: in a native group the music plays once and syncs at the
// firmware level, which is what the WiiM sync software already does).
const maGroupIDPrefix = "group:"

// maResolvePlayerID canonicalizes a selected MA player id (issue #138):
// a "group:<id>" name (the synthetic group entry) resolves to the master it
// names; a player currently synced to a firmware master resolves to that
// master, because in a native WiiM group only the master can play - slaves
// just mirror. Unknown ids (and dissolved groups) yield "". Run loop only.
func (p *AppPlayer) maResolvePlayerID(id string, players []ma.Player) string {
	if strings.HasPrefix(id, maGroupIDPrefix) {
		id = strings.TrimPrefix(id, maGroupIDPrefix)
	}
	for _, pl := range players {
		if pl.PlayerID != id {
			continue
		}
		if pl.SyncedTo != nil && *pl.SyncedTo != "" {
			return *pl.SyncedTo
		}
		return id
	}
	return ""
}

// maTransferPlayback moves the active playback from oldPlayerID to
// newPlayerID (issue #138, user-approved transfer semantics - "like
// Spotify Connect"): it replays the SAME source on the new player starting
// at the current track, then pauses the old one. The source comes from the
// old queue's sources[] + current item; a queue without a usable source
// (single-item playback) falls back to the bare current track URI. "Nothing
// playing" (idle / no media) is a plain re-target no-op. PlayMedia failure
// aborts the selection (the caller returns the error and keeps the old
// target); Pause failure only logs - the music already moved.
func (p *AppPlayer) maTransferPlayback(ctx context.Context, client *ma.Client, oldPlayerID, newPlayerID string) error {
	q, err := client.GetQueue(ctx, oldPlayerID)
	if err != nil {
		p.app.log.Debugf("ma: transfer: queue get %s failed: %v (treating as idle)", oldPlayerID, err)
		return nil // not observable = nothing active: plain re-target
	}
	if q == nil || q.State == "idle" || q.CurrentItem == nil || q.CurrentItem.MediaItem == nil {
		return nil
	}
	cur := q.CurrentItem.MediaItem
	mediaURI, startItem := cur.URI, ""
	if len(q.Sources) == 1 && q.Sources[0].URI != "" {
		mediaURI = q.Sources[0].URI // replay the full source, not a single track
		startItem = cur.ItemID      // MA's resolver matches item_id or uri exactly (2.10.7)
	}
	if err := client.PlayMedia(ctx, newPlayerID, mediaURI, startItem); err != nil {
		p.app.log.Warnf("ma: transfer to %s failed: %v", newPlayerID, err)
		return fmt.Errorf("could not move playback to the new device")
	}
	if err := client.Pause(ctx, oldPlayerID); err != nil {
		p.app.log.Warnf("ma: transfer: pausing %s failed: %v", oldPlayerID, err)
	}
	p.app.log.Infof("ma: moved playback from %s to %s (%s)", oldPlayerID, newPlayerID, mediaURI)
	return nil
}

// maUpcomingTracks reconstructs the upcoming queue list for the "Läuft
// gerade" pane (issue #138). MA's player_queues/get carries no item list -
// only sources[] + current_index - so a single-source queue is re-expanded
// from its source's ordered track list: the currently playing track is
// located in it and everything AFTER it becomes the upcoming list. 2.10.7
// semantics (music/queue media resolver, verified in source): a playlist or
// album queue holds the FULL ordered track list with at most the prefix
// before the start item dropped, so "everything after the current position"
// is exactly the queue tail - for both full and rotated queues.
// Queues that cannot be reconstructed yield an EMPTY list (not a failure):
// no/multiple sources, single-track sources, shuffle on (the queue order
// then differs from the source order and queue indices are not addressable).
// A failed source fetch keeps the last good list instead of blanking. The
// result is cached per (source, current track) so the 1/s poll does not
// re-fetch playlist tracks while one track plays. Run loop only.
func (p *AppPlayer) maUpcomingTracks(ctx context.Context, client *ma.Client, pl *ma.Player, queue *ma.PlayerQueue) []QueueTrack {
	if queue == nil || pl.CurrentMedia == nil {
		return p.maUpcomingCached // stale-or-empty; the poll carries the previous list over
	}
	if queue.CurrentIndex == nil || len(queue.Sources) != 1 || queue.ShuffleEnabled {
		return []QueueTrack{} // not reconstructable: honest blank, no fetch
	}
	src := queue.Sources[0]

	key := src.URI + "|" + pl.CurrentMedia.URI
	if key == p.maUpcomingKey {
		return p.maUpcomingCached
	}

	var (
		tracks []ma.TrackItem
		err    error
	)
	switch src.MediaType {
	case "playlist":
		tracks, err = client.PlaylistTracks(ctx, src.ItemID)
	case "album":
		tracks, err = client.AlbumTracks(ctx, src.ItemID, src.Provider)
	default:
		return []QueueTrack{} // single track or unknown source type: nothing to expand
	}
	if err != nil {
		p.app.log.Debugf("ma: upcoming reconstruction %s/%s failed: %v", src.MediaType, src.ItemID, err)
		return p.maUpcomingCached // keep the last good list on transient failure; retry next tick
	}

	upcoming := []QueueTrack{}
	if idx := maIndexOfTrack(tracks, pl.CurrentMedia); idx >= 0 {
		rest := tracks[idx+1:]
		if len(rest) > QueueLimit {
			rest = rest[:QueueLimit]
		}
		for j := range rest {
			t := maTrackToUpcoming(rest[j], *queue.CurrentIndex+1+j, true)
			if t.Uri != "" || t.Name != "" {
				upcoming = append(upcoming, t)
			}
		}
	}
	p.maUpcomingKey = key
	p.maUpcomingCached = upcoming
	return upcoming
}

// maIndexOfTrack locates the currently playing track inside an ordered track
// list: exact URI first (stable per provider instance), case-insensitive
// title as fallback. -1 when not found (dynamic playlist reshuffle, manual
// queue edits) - the upcoming pane stays empty then rather than guess.
func maIndexOfTrack(tracks []ma.TrackItem, cur *ma.CurrentMedia) int {
	for i := range tracks {
		if cur.URI != "" && tracks[i].URI == cur.URI {
			return i
		}
	}
	for i := range tracks {
		if tracks[i].Name != "" && strings.EqualFold(tracks[i].Name, cur.Title) {
			return i
		}
	}
	return -1
}

// maTrackToUpcoming maps one source track into the shared QueueTrack shape
// for the upcoming pane (issue #138). queueIndex is the entry's absolute
// position in the QUEUE, not in the source list: it doubles as the play_index
// target for the in-queue skip (maPlayUri), which accepts a queue index.
// TrackId is only set when the queue order equals the source order - i.e.
// shuffle off; shuffled queues get empty TrackIds so in-queue taps fall back
// to single-item playback instead of mis-jumping. Cover: top-level image
// first, metadata.images as fallback (library track lists only carry the
// latter).
func maTrackToUpcoming(t ma.TrackItem, queueIndex int, addressable bool) QueueTrack {
	qt := QueueTrack{
		Uri:      t.URI,
		Name:     t.Name,
		Album:    t.Album.Name(),
		ImageUrl: maImageURL(t.Cover()),
	}
	if addressable {
		qt.TrackId = strconv.Itoa(queueIndex)
	}
	if len(t.Artists) > 0 {
		names := make([]string, 0, len(t.Artists))
		for _, a := range t.Artists {
			if a.Name != "" {
				names = append(names, a.Name)
			}
		}
		qt.Artist = strings.Join(names, ", ")
	}
	return qt
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
		// resume carries Spotify Connect semantics ("keep playing on the
		// target device"); MA's Play is the equivalent - it resumes an idle
		// player and no-ops a playing one (issue #134 mapping).
		// play may also carry a URI from /player/play (tapped item) and must
		// route to the queue commands instead (issue #138): maPlayUri.
		if verb == "play" {
			play, _ := data.(ApiRequestDataPlay)
			if play.Uri != "" {
				callErr = p.maPlayUri(pctx, client, playerID, play)
				break
			}
		}
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
	// issue #138: one synthetic entry per native (firmware-level) group -
	// the MASTER with more than one member. Selecting it resolves to the
	// master whose WiiM firmware mirrors playback to the slaves natively;
	// HA's group-formation automation keeps the group alive around playback
	// exactly like the Spotify flow (deliberately NOT an MA virtual group).
	for _, pl := range players {
		if len(pl.GroupMembers) > 1 {
			devs = append(devs, MaDevice{Id: maGroupIDPrefix + pl.PlayerID, Name: pl.Name + " (Gruppe)", Available: pl.Available})
		}
	}
	return map[string]any{"devices": devs}, nil
}

// maSelectPlayer backs POST /ma/player/select (issue #134 + #138, pinned
// contract): the id must resolve via players/all - a bare player id, the
// "group:<master>" name of the synthetic native-group entry, or an id that
// is currently synced to a firmware master (resolves to the master) - else
// 400. MA must be configured (else 400). Picking a DIFFERENT target than
// the current selection TRANSFERS the active playback there (replay the
// same source from the current track + pause the old player; user-approved
// "Spotify Connect" semantics); picking while nothing plays is a plain
// re-target. On success it persists the RESOLVED id the same way the daemon
// remembers the last Spotify device (in-memory per session): the next poll
// tick observes it within ~1s.
func (p *AppPlayer) maSelectPlayer(ctx context.Context, playerID string) (any, error) {
	cfg, _ := p.maResolved()
	if cfg == nil {
		return nil, ErrBadRequest
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()
	client := p.maClientFor(cfg)
	players, err := client.ListPlayers(pctx)
	if err != nil {
		p.app.log.Warnf("ma: players/all failed during player select: %v", err)
		return nil, fmt.Errorf("music assistant unreachable: %w", err)
	}

	resolved := p.maResolvePlayerID(playerID, players)
	if resolved == "" {
		return nil, ErrBadRequest
	}
	name := playerID
	for _, pl := range players {
		if pl.PlayerID == resolved {
			name = pl.Name
			break
		}
	}

	// issue #138 (user-approved): transfer the active playback to the new
	// target - "like Spotify Connect", the music follows the selection. Only
	// when a DIFFERENT player is currently selected: re-selecting the same
	// resolved target (tapping a slave or its group entry while the master
	// already plays) must not restart the music.
	if old := p.maActivePlayerId; old != "" && old != resolved {
		if err := p.maTransferPlayback(pctx, client, old, resolved); err != nil {
			return nil, err
		}
	}

	p.maActivePlayerId = resolved
	p.app.log.Infof("ma: selected player %s (%s)", resolved, name)
	return map[string]any{"ok": true}, nil
}

// maPlayUri routes a /player/play URI in MA mode (issue #138), mapping the
// Spotify Connect context semantics onto MA queue commands:
//   - uri + offset.uri: the UI's tracklist confirmations send the parent
//     list as context and the tapped item as offset - start that source at
//     the tapped item, queueing the rest behind it (play_media replace +
//     start_item; verified live against 2.10.7's exact-match resolver).
//   - bare uri matching an upcoming entry of the observed queue: in-queue
//     skip via play_index (bug26 semantics - the queue is preserved, the
//     tapped card jumps to now-playing; MA context_uri is empty so the UI
//     always takes this bare-uri branch for "Läuft gerade" cards).
//   - anything else: single-item context.
func (p *AppPlayer) maPlayUri(ctx context.Context, client *ma.Client, playerID string, play ApiRequestDataPlay) error {
	if play.Offset != nil && play.Offset.Uri != "" {
		return client.PlayMedia(ctx, playerID, play.Uri, play.Offset.Uri)
	}
	if rs := p.state.remoteState; rs != nil && rs.DeviceId == playerID {
		for _, t := range rs.NextTracks {
			if t.TrackId != "" && t.Uri != "" && t.Uri == play.Uri {
				return client.PlayIndex(ctx, playerID, t.TrackId)
			}
		}
	}
	return client.PlayMedia(ctx, playerID, play.Uri, "")
}

// maImageURL picks a UI-fetchable url from an MA image entry: only remotely
// accessible absolute http(s) paths qualify. MA serves local files through
// its own media proxy (proxy_id), which the daemon does not route - those
// entries yield "" and the card falls back to its placeholder rather than
// serving a dead link (verified against live 2.10.7 image shapes).
func maImageURL(img *ma.MediaImage) string {
	if img == nil || !img.RemotelyAccessible {
		return ""
	}
	p := img.Path
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	return ""
}

// MaPlaylist is one entry of GET /ma/playlists (issue #138, pinned contract).
type MaPlaylist struct {
	ItemID   string `json:"item_id"`
	Name     string `json:"name"`
	URI      string `json:"uri"`
	ImageURL string `json:"image_url"`
}

// MaTrack is one entry of GET /ma/playlist-items and GET /ma/recent (issue
// #138, pinned contract): the shared track-card shape.
type MaTrack struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	URI        string   `json:"uri"`
	Album      string   `json:"album,omitempty"`
	Artists    []string `json:"artists"`
	DurationMs int64    `json:"duration_ms,omitempty"` // source seconds -> ms
	Position   int      `json:"position"`              // absolute index in the parent playlist
	ImageURL   string   `json:"image_url,omitempty"`
}

// maListPlaylists backs GET /ma/playlists (issue #138, pinned contract):
// 200 {"playlists":[{item_id,name,uri,image_url}]} - the flattened library
// playlist list. music/browse "root" yields one folder per provider whose
// Path ends in "://"; "<path>playlists" holds that provider's playlists, and
// EVERY playlist - built-in and provider-synced alike - is a LIBRARY item
// with uri "library://playlist/<id>" (verified live against 2.10.7). A
// provider without a playlists collection answers its browse with a 500 -
// that folder is skipped, not an error. EMPTY array when MA is unconfigured
// or the root browse fails (maListDevices contract).
func (p *AppPlayer) maListPlaylists(ctx context.Context) (any, error) {
	playlists := []MaPlaylist{}
	cfg, _ := p.maResolved()
	if cfg == nil {
		return map[string]any{"playlists": playlists}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()
	client := p.maClientFor(cfg)

	root, err := client.Browse(pctx, "root")
	if err != nil {
		p.app.log.Warnf("ma: music/browse root failed: %v", err)
		return map[string]any{"playlists": playlists}, nil
	}
	for _, folder := range root {
		if !strings.HasSuffix(folder.Path, "://") {
			continue // not a provider folder (breadcrumbs etc.)
		}
		items, err := client.Browse(pctx, folder.Path+"playlists")
		if err != nil {
			p.app.log.Debugf("ma: music/browse %splaylists failed: %v", folder.Path, err)
			continue
		}
		for _, it := range items {
			if it.MediaType != "playlist" || it.URI == "" {
				continue // back-breadcrumb folders and non-playlists
			}
			pl := MaPlaylist{ItemID: it.ItemID, Name: it.Name, URI: it.URI}
			pl.ImageURL = maPlaylistImage(it)
			playlists = append(playlists, pl)
		}
	}
	return map[string]any{"playlists": playlists}, nil
}

// maPlaylistImage picks the UI-fetchable cover of a playlist browse entry:
// playlist items carry no top-level image - covers live in metadata.images
// (verified live against 2.10.7) and only remotely accessible absolute
// entries qualify (maImageURL).
func maPlaylistImage(it ma.BrowseItem) string {
	if it.Metadata == nil {
		return ""
	}
	for i := range it.Metadata.Images {
		if url := maImageURL(&it.Metadata.Images[i]); url != "" {
			return url
		}
	}
	return ""
}

// maListPlaylistItems backs GET /ma/playlist-items?id=<library playlist id>
// (issue #138, pinned contract): 200 {"tracks":[...]} in the shared track
// card shape, in playlist order (Position is the absolute index). Empty list
// when MA is unconfigured or the lookup fails.
func (p *AppPlayer) maListPlaylistItems(ctx context.Context, itemID string) (any, error) {
	tracks := []MaTrack{}
	cfg, _ := p.maResolved()
	if cfg == nil || itemID == "" {
		return map[string]any{"tracks": tracks}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()
	items, err := p.maClientFor(cfg).PlaylistTracks(pctx, itemID)
	if err != nil {
		p.app.log.Warnf("ma: playlist_tracks %s failed: %v", itemID, err)
		return map[string]any{"tracks": tracks}, nil
	}
	for i := range items {
		it := &items[i]
		if it.URI == "" {
			continue
		}
		names := make([]string, 0, len(it.Artists))
		for _, a := range it.Artists {
			if a.Name != "" {
				names = append(names, a.Name)
			}
		}
		t := MaTrack{ID: it.ItemID, Name: it.Name, URI: it.URI, Album: it.Album.Name(), Artists: names, Position: it.Position}
		if it.Duration > 0 {
			t.DurationMs = int64(it.Duration) * 1000
		}
		t.ImageURL = maImageURL(it.Cover())
		tracks = append(tracks, t)
	}
	return map[string]any{"tracks": tracks}, nil
}

// maListRecent backs GET /ma/recent (issue #138, pinned contract): the last
// N played tracks from MA's play log. The play-log mapping is lightweight -
// title + artwork only, no artists (MA limitation) - so the UI renders
// title-only cards for this pane in MA mode. Empty list when unconfigured or
// unreachable; limit is clamped to 1..50 (the UI asks for 20).
func (p *AppPlayer) maListRecent(ctx context.Context, limit int) (any, error) {
	tracks := []MaTrack{}
	if limit < 1 || limit > 50 {
		limit = 20
	}
	cfg, _ := p.maResolved()
	if cfg == nil {
		return map[string]any{"tracks": tracks}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()
	items, err := p.maClientFor(cfg).RecentlyPlayed(pctx, limit)
	if err != nil {
		p.app.log.Warnf("ma: recently_played_items failed: %v", err)
		return map[string]any{"tracks": tracks}, nil
	}
	for i := range items {
		it := &items[i]
		t := MaTrack{ID: it.ItemID, Name: it.Name, URI: it.URI, Artists: []string{}}
		t.ImageURL = maImageURL(it.Image)
		tracks = append(tracks, t)
	}
	return map[string]any{"tracks": tracks}, nil
}
