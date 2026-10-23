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

// maPlaylistTTL is the refresh cadence of the background playlist warmup -
// it matches the UI's own 5-minute cache, so a fresh live fetch always wins
// over serving the warmed copy (issue #138 defect B).
const maPlaylistTTL = 5 * time.Minute

// maPlaylistWarmTimeout is the budget of one BACKGROUND browse: deliberately
// much larger than maControlTimeout, because a cold browse can outlive it -
// that mismatch was exactly defect B. Runs off the request path, so being
// slow there cannot hold up /ma/playlists.
const maPlaylistWarmTimeout = 90 * time.Second

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
// command. Unconfigured MA is a client error (ErrBadRequest -> HTTP 400) per
// the pinned contract. When no player is selected yet it falls back to the
// blob's "maDefaultDeviceId" (issue #48): if that id resolves via players/all
// to a known player, the target is RETARGETED there - a PLAIN re-target
// WITHOUT queue transfer (an explicit transfer goes through /ma/player/select
// -> maTransferPlayback) - and persisted as the selection so the next poll
// tick observes it. No default key, an unresolvable default, or a failed
// players/all all keep the legacy "no player selected" 400. Run-loop only.
func (p *AppPlayer) maControlTarget(ctx context.Context) (*ma.Client, string, error) {
	cfg, _ := p.maResolved()
	if cfg == nil {
		return nil, "", ErrBadRequest
	}
	if p.maActivePlayerId == "" {
		// issue #48: no player selected yet - fall back to the UI's MA default
		// device before erroring. Plain re-target only (see header).
		if def := maDefaultDeviceIdFromSettings(p.app.GetSettings()); def != "" {
			client := p.maClientFor(cfg)
			pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
			players, err := client.ListPlayers(pctx)
			cancel()
			if err != nil {
				p.app.log.Debugf("ma: players/all for the default device failed: %v", err)
			} else if resolved := p.maResolvePlayerID(def, players); resolved != "" {
				p.maActivePlayerId = resolved
				p.app.log.Infof("ma: no player selected - retargeting to default device %s", resolved)
				return client, resolved, nil
			}
		}
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
	// issue #138 defect B: keep the playlist cache fresh in the background so
	// /ma/playlists never has to wait on a cold browse (see maListPlaylists).
	p.maMaybeWarmPlaylists(client)
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

	// issue #138 (defect 1): a selection that starts IDLE has no live state
	// to display - fetch the speaker's OWN playlog once (per-queue filter,
	// read-only) so the synthesized paused state can show its last song. The
	// tried-guard keeps a failing lookup from re-hitting MA every second.
	if (pl.PlaybackState == "idle" || pl.CurrentMedia == nil) &&
		p.maIdleLogTriedFor != p.maActivePlayerId && p.maIdleLastTrack(p.maActivePlayerId) == nil {
		p.maFetchIdleFallback(pctx, client, pl)
	}

	// issue #138: upcoming queue for the "Läuft gerade" pane - the LIVE queue
	// tail (maUpcomingTracks enumerates player_queues/items one past the
	// current position, like MA's own webui), fetched only while media is
	// active (idle players have nothing queued) on the same 4s poll budget.
	// It also keeps the last good list on a transient fetch failure.
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
		// issue #138 (defects 1+2): a SELECTED player going idle/stopped is
		// still the user's device. Clearing state dropped the UI to the
		// device picker - "select an idle speaker" and "stop on the speaker"
		// both became dead ends from which no music could start. Keep the
		// selection: synthesize a PAUSED RemoteState carrying the last known
		// track (live observation, else per-queue playlog). New music starts
		// from the Playlists/Zuletzt panes against this same selection; the
		// now-playing play button resumes via players/cmd/play (a clean
		// no-op on a cleared queue). The picker returns only when MA mode or
		// the selection itself goes away - see the maPollTick early return.
		p.applyMaIdleState(pl)
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
	if queue != nil && queue.ElapsedTime != nil && queue.ElapsedTimeLastUpdated != nil {
		// issue #46: for some providers (live-verified 'sendspin') the
		// player-top position pair is stale/zeroed garbage - the QUEUE pair
		// is canonical there: live while playing, frozen at the paused
		// position. Presence of both fields is the only gate: no check on
		// queue.State or pl.PlaybackState (sendspin reports idle while
		// paused yet must keep its position). MA reports seconds + epoch
		// SECONDS; RemoteState uses unix MILLISECONDS throughout.
		rs.PositionAsOfTimestamp = int64(*queue.ElapsedTime * 1000)
		rs.Timestamp = int64(*queue.ElapsedTimeLastUpdated * 1000)
	} else if pl.ElapsedTime != nil && pl.ElapsedTimeLastUpdated != nil {
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
		// issue #138 (defect 3): expose the single queue source as context_uri:
		// the UI's upcoming-card taps then take the OFFSET branch ({uri:
		// source, offset:{uri: tapped}}) and maPlayUri plays it via play_media
		// + start_item - exact-match resolution, no index arithmetic.
		if len(queue.Sources) == 1 {
			rs.ContextUri = queue.Sources[0].URI
		}
	} else if prevState != nil && p.maOwnsRemoteState && prevState.DeviceId == rs.DeviceId {
		// transient player_queues/get failure: carry list AND its context
		rs.NextTracks = prevState.NextTracks
		rs.ContextUri = prevState.ContextUri
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

	// issue #138 (defects 1+2): refresh the idle-survival last-song source;
	// a live observation supersedes any playlog fallback. Defect A tracks the
	// raw MA state so controls can tell IDLE (replay the displayed track)
	// apart from PAUSED (plain resume).
	p.maLastActiveState = rs
	p.maIdleLogFallback = nil
	p.maLastPlaybackState = pl.PlaybackState

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

// maIdleLastTrack returns the last-song display source for an IDLE poll of
// this player (issue #138, defects 1+2): the last LIVE observation on it
// (maLastActiveState), else the lazily fetched per-queue playlog entry
// (maIdleLogFallback), else nil. Run loop only.
func (p *AppPlayer) maIdleLastTrack(id string) *RemoteState {
	if lrs := p.maLastActiveState; lrs != nil && lrs.DeviceId == id {
		return lrs
	}
	if lrs := p.maIdleLogFallback; lrs != nil && lrs.DeviceId == id {
		return lrs
	}
	return nil
}

// maFetchIdleFallback fills maIdleLogFallback with the speaker's own last
// played track from MA's play log (per-queue filter - a player's default
// queue carries its own id). One read-only call per selection: a failure and
// an empty log both set the tried-guard, so the poll tick neither hammers a
// failing server nor re-fetches for nothing; the result may be title-less, in
// which case the idle state still keeps the device addressable. Run loop only.
func (p *AppPlayer) maFetchIdleFallback(pctx context.Context, client *ma.Client, pl *ma.Player) {
	// issue #138 round 4, defect B': fullyPlayedOnly=false - a speaker that
	// was paused/stopped mid-song is "last played" exactly like one whose
	// song finished; tracks only (a playlist row here would mislabel the
	// synthesized now-playing card).
	entries, err := client.RecentlyPlayed(pctx, 1, pl.PlayerID, []string{"track"}, false)
	if err != nil {
		p.app.log.Debugf("ma: recently_played_items for idle player %s failed: %v", pl.PlayerID, err)
		p.maIdleLogTriedFor = pl.PlayerID
		return
	}
	rs := &RemoteState{DeviceId: pl.PlayerID, DeviceName: pl.Name, DeviceType: maDeviceType}
	if len(entries) > 0 {
		e := entries[0]
		names := make([]string, 0, len(e.Artists))
		for _, a := range e.Artists {
			if a.Name != "" {
				names = append(names, a.Name)
			}
		}
		rs.TrackUri = e.URI
		rs.TrackName = e.Name
		rs.TrackArtist = strings.Join(names, ", ")
		rs.TrackAlbum = e.Album.Name()
		rs.TrackImageUrl = maImageURL(e.Cover())
		if e.Duration > 0 {
			rs.Duration = int64(e.Duration) * 1000
		}
	}
	p.maIdleLogFallback = rs
	p.maIdleLogTriedFor = pl.PlayerID
}

// applyMaIdleState maps one IDLE poll of the SELECTED player into a
// synthesized PAUSED RemoteState (issue #138, defects 1+2 - see
// applyMaState): the speaker keeps showing as the current device with its
// last known song, matching what MA's own webui shows after a stop ("the
// song is merely paused"). The position freezes at the last observed value -
// or at the position the STALE current_media object itself carries when it
// reports one (issue #46); otherwise the last live observation is kept.
// NextTracks stay EMPTY on purpose - an idle queue has nothing upcoming and
// new music starts from the content panes. No noteClusterTiming: the
// synthetic timestamps are local, not server clock evidence. Run loop only.
func (p *AppPlayer) applyMaIdleState(pl *ma.Player) {
	rs := &RemoteState{
		DeviceId:    pl.PlayerID,
		DeviceName:  pl.Name,
		DeviceType:  maDeviceType,
		IsPlaying:   false,
		IsPaused:    true,
		Volume:      maVolumeToState(pl.VolumeLevel),
		VolumeSteps: 100,
	}
	now := time.Now()
	rs.ReceivedAt = now
	rs.ReceivedAtWallMs = now.UnixMilli()
	rs.Timestamp = now.UnixMilli()

	if last := p.maIdleLastTrack(pl.PlayerID); last != nil {
		rs.TrackUri = last.TrackUri
		rs.TrackName = last.TrackName
		rs.TrackArtist = last.TrackArtist
		rs.TrackAlbum = last.TrackAlbum
		rs.TrackImageUrl = last.TrackImageUrl
		rs.Duration = last.Duration
		rs.PositionAsOfTimestamp = last.PositionAsOfTimestamp
	}

	if pl.CurrentMedia != nil &&
		pl.CurrentMedia.ElapsedTime != nil &&
		pl.CurrentMedia.ElapsedTimeLastUpdated != nil {
		// issue #46: a provider that pauses by freezing its queue position
		// (sendspin) keeps reporting it inside the STALE current_media object
		// - that is the position to display. Presence of both fields is the
		// only gate; no new MA fetch in this path.
		rs.PositionAsOfTimestamp = int64(*pl.CurrentMedia.ElapsedTime * 1000)
		rs.Timestamp = int64(*pl.CurrentMedia.ElapsedTimeLastUpdated * 1000)
	}

	prevState := p.state.remoteState
	// same changed-track semantics as the active path: a stop mid-playback
	// (same uri, state flipped) re-renders via ObserverStateChanged
	trackChanged := prevState == nil || !p.maOwnsRemoteState || prevState.TrackUri != rs.TrackUri

	p.state.remoteState = rs
	p.maOwnsRemoteState = true
	if rs.DeviceId != "" {
		p.state.lastActiveDeviceId = rs.DeviceId
		p.state.lastActiveDeviceName = rs.DeviceName
	}
	// issue #138 defect A: this poll is the idle synthesis - record it as such.
	p.maLastPlaybackState = "idle"

	if v := p.app.voice; v != nil {
		v.notifyPlayback(false)
	}
	if trackChanged {
		p.app.server.Emit(&ApiEvent{Type: ApiEventTypeObserverTrackChanged, Data: rs})
	} else {
		p.app.server.Emit(&ApiEvent{Type: ApiEventTypeObserverStateChanged, Data: rs})
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
// Spotify Connect"). It delegates the whole job to MA's native
// player_queues/transfer: that command ungroups a grouped/synced TARGET
// first (the WiiM case that killed the old hand-rolled replay with 500s -
// play_media into a grouped queue has no defined behavior), copies the FULL
// queue (items, sources, repeat/shuffle/crossfade/autoplay state) and
// resumes on the target at the saved position only when the source was
// playing. "Nothing playing" (idle / no media) is a plain re-target no-op.
// A failed transfer aborts the selection (the caller returns the error and
// keeps the old target).
func (p *AppPlayer) maTransferPlayback(ctx context.Context, client *ma.Client, oldPlayerID, newPlayerID string) error {
	q, err := client.GetQueue(ctx, oldPlayerID)
	if err != nil {
		p.app.log.Debugf("ma: transfer: queue get %s failed: %v (treating as idle)", oldPlayerID, err)
		return nil // not observable = nothing active: plain re-target
	}
	if q == nil || q.State == "idle" || (q.CurrentItem == nil && q.Items == 0) {
		return nil
	}
	if err := client.TransferQueue(ctx, oldPlayerID, newPlayerID); err != nil {
		p.app.log.Warnf("ma: transfer %s -> %s failed: %v", oldPlayerID, newPlayerID, err)
		return fmt.Errorf("could not move playback to the new device")
	}
	p.app.log.Infof("ma: moved active playback from %s to %s (MA queue transfer)", oldPlayerID, newPlayerID)
	return nil
}

// maUpcomingTracks returns the live queue tail for the "Läuft gerade" pane
// (issue #138): player_queues/items starting one past the current position -
// the SAME enumeration MA's own webui renders. The round-1 reconstruction
// from queue SOURCE + index is gone on purpose: autoplay re-queues diverge
// from their declared source (live evidence on 2.10.7: a 27-item autoplay
// queue whose single 3-track source playlist shares no track with the queue
// tail), so the source list was actively wrong - and shuffled/ended queues
// yielded nothing at all, while real enumeration answers for all of them.
// The fetch is cached per (queue id | current index | item count | current
// uri): the 1/s poll does not re-fetch while one track plays, a track change
// advances the index and an autoplay batch append bumps the count - both
// invalidate. A failed fetch keeps the last good list (blank when there was
// never one). Run loop only.
func (p *AppPlayer) maUpcomingTracks(ctx context.Context, client *ma.Client, pl *ma.Player, queue *ma.PlayerQueue) []QueueTrack {
	if queue == nil || pl.CurrentMedia == nil || queue.CurrentIndex == nil {
		return p.maUpcomingCached // idle / no media / no position: carry over the previous list
	}
	cur := *queue.CurrentIndex
	qid := queue.QueueID
	if qid == "" {
		qid = pl.PlayerID // a player's default queue carries its own id
	}
	key := qid + "|" + strconv.Itoa(cur) + "|" + strconv.Itoa(queue.Items) + "|" + pl.CurrentMedia.URI
	if key == p.maUpcomingKey {
		return p.maUpcomingCached
	}

	items, err := client.QueueItems(ctx, qid, QueueLimit, cur+1)
	if err != nil {
		p.app.log.Debugf("ma: player_queues/items %s failed: %v", qid, err)
		return p.maUpcomingCached // keep the last good list on transient failure; retry next tick
	}
	upcoming := make([]QueueTrack, 0, len(items))
	for i := range items {
		t := maQueueItemToUpcoming(items[i], cur+1+i) // absolute queue position -> play_index target
		if t.Uri == "" && t.Name == "" {
			continue
		}
		upcoming = append(upcoming, t)
	}
	p.maUpcomingKey = key
	p.maUpcomingCached = upcoming
	return upcoming
}

// maQueueItemToUpcoming maps one LIVE queue entry into the shared QueueTrack
// shape of the upcoming pane (issue #138). TrackId is the entry's absolute
// position in the queue - the play_index target for the in-queue skip
// (maPlayUri); maPlayUri re-resolves the position from the live queue at tap
// time anyway, so a stale cache entry can only cost one extra fallback.
// Media metadata comes from the nested media_item when present; non-media
// queue entries fall back to the pre-composed "Artist - Title" name line.
func maQueueItemToUpcoming(it ma.QueueItem, queuePos int) QueueTrack {
	qt := QueueTrack{ImageUrl: maImageURL(it.Image), TrackId: strconv.Itoa(queuePos)}
	if mi := it.MediaItem; mi != nil {
		qt.Uri = mi.URI
		qt.Name = mi.Name
		qt.Album = mi.Album.Name()
		if len(mi.Artists) > 0 {
			names := make([]string, 0, len(mi.Artists))
			for _, a := range mi.Artists {
				if a.Name != "" {
					names = append(names, a.Name)
				}
			}
			qt.Artist = strings.Join(names, ", ")
		}
		if qt.ImageUrl == "" {
			qt.ImageUrl = maImageURL(mi.Cover())
		}
	} else if it.Name != "" {
		qt.Name = it.Name
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
	client, playerID, err := p.maControlTarget(ctx)
	if err != nil {
		return err
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()

	var callErr error
	switch verb {
	case "resume", "play":
		// resume carries Spotify Connect semantics ("keep playing on the
		// target device"); play may also carry a URI from /player/play
		// (tapped item) and routes to the queue commands instead (issue #138):
		// maPlayUri. A NOT-playing MA player (idle OR paused) asks MA for its
		// OWN resume - after a hard stop that continues the parked current_item
		// at the saved resume_pos (NOT from 0), item 0 on an ended queue, first
		// item when there is no current item; while PAUSED it unpause-forwards
		// at the same position (issue #138 round 4, defect A'). Only if the
		// queue is empty does it fall back to replaying the track the UI shows.
		if verb == "play" {
			play, _ := data.(ApiRequestDataPlay)
			if play.Uri != "" {
				callErr = p.maPlayUri(pctx, client, playerID, play)
				break
			}
		}
		if p.maLastPlaybackState == "playing" {
			// already playing: no-op mirroring MA's own "Ignore PLAY request" -
			// the queue resume would reload the current item at the same
			// position (an audible stream restart for a no-op tap).
			break
		}
		callErr = p.maResumeStopped(pctx, client, playerID)
	case "pause":
		// players/cmd/pause redirects to the pause-aware player_queues/pause,
		// which stores queue.resume_pos - keep the legacy command (issue #138).
		callErr = client.Pause(pctx, playerID)
	case "playpause":
		// issue #138 round 4, defect A': the legacy players/cmd/play_pause is
		// NOT pause-aware on this stack - on a PAUSED queue player, cmd/play
		// skips its queue-resume branch and falls through to the generic resume
		// path whose final step is player.play_media(current media): it
		// RESTARTS the track at 0 (verified byte-identical in MA 2.10.7 and
		// main, controllers/players/controller.py cmd_play / _handle_cmd_play).
		// Pause stays on the legacy command (it redirects to the pause-aware
		// queue pause); unpause goes through player_queues/resume, which IS
		// pause-aware: it replays the parked current item at the stored
		// resume_pos.
		if p.maLastPlaybackState == "playing" {
			callErr = client.Pause(pctx, playerID)
		} else {
			// idle OR paused: MA-native resume from the parked position
			callErr = p.maResumeStopped(pctx, client, playerID)
		}
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

// maIsIdle reports whether the last observed MA playback state of the SELECTED
// player is idle (issue #138 defect A). Run loop only - maLastPlaybackState
// is written by the poll's applyMaState / applyMaIdleState and read by
// maSend - both inside the Run() goroutine, so no synchronization is needed
// (same convention as the other ma* fields).
func (p *AppPlayer) maIsIdle() bool {
	return p.maLastPlaybackState == "idle"
}

// maResumeStopped resumes playback on a NOT-playing MA player (idle OR
// paused - issue #138 round 3, defect A'; extended to paused in round 4): it
// asks MA for its OWN queue resume, which after a hard stop continues the
// parked current_item at the saved resume_pos - live-verified on 2.10.7:
// _handle_stop stores corrected_elapsed_time in queue.resume_pos and keeps
// current_item, so play-after-stop resumes mid-song instead of restarting it
// (the old play_index/play_media replay started every song at 0 by
// construction). While PAUSED it forward-resumes at the same position: pause
// stores resume_pos the same way, and _watch_pause auto-stops ~30 s after a
// pause with stop storing resume_pos too - so every paused window is covered
// by the native path. An ended queue restarts item 0 and a queue without a
// current item starts at its first/next entry - both MA-native semantics.
// Only when the native resume fails (empty queue = QueueEmpty, unreachable
// server, ...) does it fall back to replaying the track the UI shows
// (maResumeDisplayed) - keeping the legacy defect-A behavior as a safety net.
func (p *AppPlayer) maResumeStopped(ctx context.Context, client *ma.Client, playerID string) error {
	if err := client.Resume(ctx, playerID); err == nil {
		return nil
	} else {
		p.app.log.Debugf("ma: queue resume for %s failed: %v - replaying the displayed track", playerID, err)
	}
	return p.maResumeDisplayed(ctx, client, playerID)
}

// maResumeDisplayed replays the track the UI is currently SHOWING on an idle
// MA player - the FALLBACK of maResumeStopped for when MA's native resume
// fails (issue #138 defect A / round 3 A'). After a stop the shown track is
// maIdleLastTrack (last live observation or per-queue playlog); the replay
// order:
//  1. shown track is in the LIVE queue -> play_index onto it: the rest of
//     the queue survives a mid-queue stop (mid-list autoplay additions etc.);
//  2. not in the queue (ended/stale) -> play_media with the single shown
//     track replaces the exhausted queue with it;
//  3. everything failed -> plain resume (legacy behavior, never a dead tap).
func (p *AppPlayer) maResumeDisplayed(ctx context.Context, client *ma.Client, playerID string) error {
	rs := p.state.remoteState
	if rs == nil || rs.TrackUri == "" {
		return client.Play(ctx, playerID) // nothing shown: plain resume
	}
	if idx, ok := p.maQueueIndexOf(ctx, client, playerID, rs.TrackUri); ok {
		if err := client.PlayIndex(ctx, playerID, idx); err == nil {
			return nil
		} else {
			p.app.log.Debugf("ma: replay via play_index %d for %s failed: %v - trying play_media", idx, rs.TrackUri, err)
		}
	}
	if err := client.PlayMedia(ctx, playerID, rs.TrackUri, ""); err != nil {
		p.app.log.Warnf("ma: replay of displayed track %s failed (%v); falling back to plain resume", rs.TrackUri, err)
		return client.Play(ctx, playerID)
	}
	return nil
}

// maListDevices backs GET /ma/devices (issue #134, pinned contract):
// 200 {"devices":[...]} from players/all; an EMPTY array - never an error -
// when MA is unconfigured or unreachable.
func (p *AppPlayer) maListDevices(ctx context.Context) (any, error) {
	cfg, _ := p.maResolved()
	if cfg == nil {
		return map[string]any{"devices": []MaDevice{}}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()
	players, err := p.maClientFor(cfg).ListPlayers(pctx)
	if err != nil {
		p.app.log.Warnf("ma: players/all failed: %v", err)
		return map[string]any{"devices": []MaDevice{}}, nil
	}
	return map[string]any{"devices": maMapDevices(players)}, nil
}

// maSelectPlayer backs POST /ma/player/select (issue #134 + #138, pinned
// contract): the id must resolve via players/all - a bare player id, the
// "group:<master>" name of the synthetic native-group entry, or an id that
// is currently synced to a firmware master (resolves to the master) - else
// 400. MA must be configured (else 400). Picking a DIFFERENT target than
// the current selection TRANSFERS the active playback there with MA's own
// player_queues/transfer (see maTransferPlayback; user-approved "Spotify
// Connect" semantics); picking while nothing plays is a plain re-target. On
// success it persists the RESOLVED id the same way the daemon remembers the
// last Spotify device (in-memory per session): the next poll tick observes
// it within ~1s.
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

	// issue #138 (defect 1): last-song sources are per speaker - a changed
	// target re-derives them (fresh per-queue playlog on the first idle poll).
	if p.maActivePlayerId != resolved {
		p.maLastActiveState = nil
		p.maIdleLogFallback = nil
		p.maIdleLogTriedFor = ""
		p.maLastPlaybackState = ""
	}

	p.maActivePlayerId = resolved
	p.app.log.Infof("ma: selected player %s (%s)", resolved, name)
	return map[string]any{"ok": true}, nil
}

// maPlayUri routes a /player/play URI in MA mode (issue #138), mapping the
// Spotify Connect context semantics onto MA queue commands:
//   - uri + offset.uri: the UI's tracklist confirmations send the parent
//     list as context and the tapped item as offset. Defect C: the LIVE
//     queue is authoritative - when the tapped item sits in it, a true
//     in-queue skip (play_index) preserves the rest of the queue (incl.
//     autoplay additions) and works even when the context uri no longer
//     matches the real queue contents. Only for a tap OUTSIDE the observed
//     window does it fall back to starting that source at the tapped item
//     (play_media replace + start_item; verified live against 2.10.7's
//     exact-match resolver), and a failing fallback plays the tapped item
//     alone - a tap should land the song, not raise a toast.
//   - bare uri matching an upcoming entry of the observed queue: in-queue
//     skip via play_index (bug26 semantics - the queue is preserved, the
//     tapped card jumps to now-playing). MA 2.10.7's index schema is
//     int | str and a STRING is resolved as a queue_item_id (uuids only), so
//     the computed queue position must go over the wire as a JSON NUMBER -
//     the previous string encoding 400'd with "Item N not found in queue"
//     (issue #138, defect 3).
//   - anything else: single-item context.
func (p *AppPlayer) maPlayUri(ctx context.Context, client *ma.Client, playerID string, play ApiRequestDataPlay) error {
	if play.Offset != nil && play.Offset.Uri != "" {
		if idx, ok := p.maQueueIndexOf(ctx, client, playerID, play.Offset.Uri); ok {
			return client.PlayIndex(ctx, playerID, idx)
		}
		err := client.PlayMedia(ctx, playerID, play.Uri, play.Offset.Uri)
		if err == nil {
			return nil
		}
		p.app.log.Debugf("ma: play_media %s@%s failed: %v - falling back to single item", play.Uri, play.Offset.Uri, err)
		return client.PlayMedia(ctx, playerID, play.Offset.Uri, "")
	}
	if rs := p.state.remoteState; rs != nil && rs.DeviceId == playerID {
		for _, t := range rs.NextTracks {
			if t.TrackId != "" && t.Uri != "" && t.Uri == play.Uri {
				if idx, perr := strconv.Atoi(t.TrackId); perr == nil {
					return client.PlayIndex(ctx, playerID, idx)
				}
			}
		}
	}
	return client.PlayMedia(ctx, playerID, play.Uri, "")
}

// maQueueIndexOf resolves a URI to its 0-based position in the player's LIVE
// queue (issue #138 defect C): one bounded window around the current position
// instead of the whole queue - the "Läuft gerade" list spans
// [current, current+QueueLimit) and an idle replay can land a few positions
// BEFORE it. The response's per-item index fields are relative to the offset,
// so absolute position = offset + i (verified live against 2.10.7). ok=false
// (queue or fetch failure, or no match) lets callers keep their legacy path.
func (p *AppPlayer) maQueueIndexOf(ctx context.Context, client *ma.Client, playerID, uri string) (int, bool) {
	q, err := client.GetQueue(ctx, playerID)
	if err != nil || q == nil {
		return 0, false
	}
	base := 0
	if q.CurrentIndex != nil {
		base = *q.CurrentIndex
	}
	start := base - 2
	if start < 0 {
		start = 0
	}
	qid := q.QueueID
	if qid == "" {
		qid = playerID // a player's default queue carries its own id
	}
	items, err := client.QueueItems(ctx, qid, QueueLimit+3, start)
	if err != nil {
		return 0, false
	}
	for i := range items {
		if mi := items[i].MediaItem; mi != nil && mi.URI == uri {
			return start + i, true
		}
	}
	return 0, false
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
	// issue #138 round 4, defect B': set only for the "Zuletzt" playlist rows
	// (recent items); absent for tracks and playlist-items - the UI opens a
	// playlist tracklist on the former and plays the latter.
	MediaType string `json:"media_type,omitempty"`
}

// maListPlaylists backs GET /ma/playlists (issue #138, pinned contract):
// 200 {"playlists":[{item_id,name,uri,image_url}]} - the flattened library
// playlist list. music/browse "root" yields one folder per provider whose
// Path ends in "://"; "<path>playlists" holds that provider's playlists, and
// EVERY playlist - built-in and provider-synced alike - is a LIBRARY item
// with uri "library://playlist/<id>" (verified live against 2.10.7). A
// provider without a playlists collection answers its browse with a 500 -
// that folder is skipped, not an error. EMPTY array only when MA is
// unconfigured (issue #138 defect 4: a 200 with an empty list would be
// cached by the UI as authoritative "no playlists").
//
// Defect B ("playlists load only after a few minutes"): the cold browse can
// outlive maControlTimeout - all folders fail and the pane sits on an error
// card while the UI retries every 15s until MA warms up. The browse result
// is therefore WARMED in the background (maPollTick, every maPlaylistTTL)
// with a generous timeout, and a failed live fetch serves the LAST GOOD list
// instead of erroring (stale-while-error). A failed fetch with NO cached
// list still errors, exactly as before - never an authoritative empty.
func (p *AppPlayer) maListPlaylists(ctx context.Context) (any, error) {
	cfg, _ := p.maResolved()
	if cfg == nil {
		return map[string]any{"playlists": []MaPlaylist{}}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()
	playlists, err := p.maFetchPlaylists(pctx, p.maClientFor(cfg))
	if err == nil {
		// the cache stays in canonical BROWSE order (the background warmup
		// stores it the same way) - stale-while-error never depends on the
		// playlog. issue #138 round 4, defect S: the SERVED list is the
		// browse result re-ordered by last-played recency (one lightweight
		// playlog call inside the same budget; a failure degrades to browse
		// order).
		p.maUpdatePlaylistCache(playlists)
		return map[string]any{"playlists": maApplyRecentPlaylistOrder(pctx, p.app.log, p.maClientFor(cfg), playlists)}, nil
	}
	if st := p.maStalePlaylists(); len(st) > 0 {
		p.app.log.Warnf("ma: playlist browse failed (%v); serving the cached list from %s ago",
			err, time.Since(p.maPlaylistCachedAt).Round(time.Second))
		return map[string]any{"playlists": st}, nil
	}
	p.app.log.Warnf("ma: playlist browse failed with no cached list: %v", err)
	return nil, fmt.Errorf("music assistant playlist browse failed: %w", err)
}

// maFetchPlaylists runs the root + per-provider playlist browse on the given
// context. Shared by the request path (maControlTimeout budget) and the
// background warmup (its own budget) - see maListPlaylists for the wire facts.
// The browse itself lives in maBrowsePlaylists (ma_content.go) so the
// pre-session API serve (issue #138 round 3, defect B') runs the exact same
// sequence.
func (p *AppPlayer) maFetchPlaylists(ctx context.Context, client *ma.Client) ([]MaPlaylist, error) {
	return maBrowsePlaylists(ctx, p.app.log, client)
}

// maMaybeWarmPlaylists refreshes the playlist cache in the BACKGROUND when it
// is older than maPlaylistTTL (issue #138 defect B): a single goroutine at a
// time, on its own context so a slow cold browse (which can outlive
// maControlTimeout) neither blocks the 1/s poll nor times out. Touched from
// the run loop; the cache fields are mutex-guarded because the HTTP handler
// and this goroutine share them.
func (p *AppPlayer) maMaybeWarmPlaylists(client *ma.Client) {
	p.maPlaylistMu.Lock()
	if p.maPlaylistWarming || time.Since(p.maPlaylistCachedAt) < maPlaylistTTL {
		p.maPlaylistMu.Unlock()
		return
	}
	p.maPlaylistWarming = true
	p.maPlaylistMu.Unlock()

	wctx, cancel := context.WithTimeout(context.Background(), maPlaylistWarmTimeout)
	go func() {
		defer cancel()
		playlists, err := p.maFetchPlaylists(wctx, client)
		p.maPlaylistMu.Lock()
		defer p.maPlaylistMu.Unlock()
		if err != nil {
			p.app.log.Warnf("ma: playlist warmup failed: %v", err)
		} else {
			p.maPlaylistCache = playlists
			p.maPlaylistCachedAt = time.Now()
		}
		p.maPlaylistWarming = false
	}()
}

// maUpdatePlaylistCache records a successful live fetch (request path).
func (p *AppPlayer) maUpdatePlaylistCache(playlists []MaPlaylist) {
	p.maPlaylistMu.Lock()
	defer p.maPlaylistMu.Unlock()
	p.maPlaylistCache = playlists
	p.maPlaylistCachedAt = time.Now()
}

// maStalePlaylists returns the last good list regardless of age (nil/empty
// when none): serving a stale library beats an error card, but an empty list
// must never masquerade as content (defect 4 contract).
func (p *AppPlayer) maStalePlaylists() []MaPlaylist {
	p.maPlaylistMu.Lock()
	defer p.maPlaylistMu.Unlock()
	if len(p.maPlaylistCache) == 0 {
		return nil
	}
	return p.maPlaylistCache
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
	cfg, _ := p.maResolved()
	if cfg == nil || itemID == "" {
		return map[string]any{"tracks": []MaTrack{}}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()
	items, err := p.maClientFor(cfg).PlaylistTracks(pctx, itemID)
	if err != nil {
		p.app.log.Warnf("ma: playlist_tracks %s failed: %v", itemID, err)
		return map[string]any{"tracks": []MaTrack{}}, nil
	}
	return map[string]any{"tracks": maMapPlaylistItems(items)}, nil
}

// maListRecent backs GET /ma/recent (issue #138, pinned contract): the last
// N played items from MA's play log - tracks AND playlists in recency order
// (issue #138 round 4, defect B'): fullyPlayedOnly=false so partially played
// items surface immediately instead of only after a track plays to the end
// (the MA API default), and the media-type filter admits playlist rows like
// MA's own "Zuletzt gespielt" view. The play-log mapping is lightweight -
// title + artwork only, no artists (MA limitation) - so the UI renders
// title-only cards for this pane in MA mode. Empty list when unconfigured; a
// fetch failure ERRORS (-> HTTP 5xx) so the UI retries instead of caching an
// empty "Zuletzt" (same contract as maListPlaylists, issue #138 defect 4);
// limit is clamped to 1..50 (the UI asks for 20).
func (p *AppPlayer) maListRecent(ctx context.Context, limit int) (any, error) {
	if limit < 1 || limit > 50 {
		limit = 20
	}
	cfg, _ := p.maResolved()
	if cfg == nil {
		return map[string]any{"tracks": []MaTrack{}}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, maControlTimeout)
	defer cancel()
	items, err := p.maClientFor(cfg).RecentlyPlayed(pctx, limit, "", []string{"track", "playlist"}, false)
	if err != nil {
		p.app.log.Warnf("ma: recently_played_items failed: %v", err)
		return nil, fmt.Errorf("music assistant playlog fetch failed: %w", err)
	}
	return map[string]any{"tracks": maMapRecentItems(items)}, nil
}
