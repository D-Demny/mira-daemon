package daemon

import (
	"context"
	"errors"
	"fmt"
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

	// issue #138: upcoming queue for the "Läuft gerade" pane. Fetched only
	// while media is active (idle players have nothing queued) on the same
	// 4s poll budget; on a transient fetch failure applyMaState carries the
	// previous list over instead of blanking the pane.
	var queue *ma.PlayerQueue
	if pl.PlaybackState != "idle" && pl.CurrentMedia != nil {
		q, qerr := client.GetQueue(pctx, p.maActivePlayerId)
		queue = q
		if qerr != nil {
			p.app.log.Debugf("ma: player_queues/get %s failed: %v", p.maActivePlayerId, qerr)
		}
	}

	p.applyMaState(pl, queue)
}

// applyMaState maps one polled MA player into the shared RemoteState -
// same shape, same clock-timing notes, same events as updateRemoteState -
// so /observer/status and the event stream stay source-agnostic.
// issue #134.
func (p *AppPlayer) applyMaState(pl *ma.Player, queue *ma.PlayerQueue) {
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
	// blank the pane: carry the previous lists over while we still own state
	// on this device.
	if queue != nil {
		rs.NextTracks = maQueueItemsToTracks(queue.NextTracks)
		if queue.PreviousTrack != nil {
			rs.PrevTracks = []QueueTrack{maQueueItemToTrack(*queue.PreviousTrack)}
		}
	} else if prevState != nil && p.maOwnsRemoteState && prevState.DeviceId == rs.DeviceId {
		rs.NextTracks = prevState.NextTracks
		rs.PrevTracks = prevState.PrevTracks
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

// maQueueItemToTrack maps one MA queue item into the shared QueueTrack shape
// (issue #138). MA pre-composes music-item Names as "Artist(s) - Title" with
// an optional trailing "(version)" suffix and may simplify the nested artists
// away, so the real title comes from media_item when present and the artist
// is recovered by suffix-matching it against the composed name - a naive
// first-" - " split would corrupt titles that themselves contain " - ".
func maQueueItemToTrack(q ma.QueueItem) QueueTrack {
	t := QueueTrack{TrackId: q.QueueItemID}

	if mi := q.MediaItem; mi != nil {
		t.Uri = mi.URI
		t.Album = mi.Album
		title := mi.Name
		if title == "" {
			title = maStripVersionSuffix(q.Name, mi.Version)
		}
		t.Name = title
		if len(mi.Artists) > 0 {
			names := make([]string, 0, len(mi.Artists))
			for _, a := range mi.Artists {
				if a.Name != "" {
					names = append(names, a.Name)
				}
			}
			t.Artist = strings.Join(names, ", ")
		} else if title != "" {
			base := maStripVersionSuffix(q.Name, mi.Version)
			if strings.HasSuffix(base, title) {
				t.Artist = strings.TrimSuffix(base[:len(base)-len(title)], " - ")
			} else if i := strings.Index(base, " - "); i > 0 {
				t.Artist = base[:i]
			}
		}
		if img := maImageURL(q.Image); img != "" {
			t.ImageUrl = img
		} else if mi.Image != nil {
			t.ImageUrl = maImageURL(mi.Image)
		}
		return t
	}

	// no media_item: best-effort parse of the composed name
	if i := strings.Index(q.Name, " - "); i > 0 {
		t.Artist = q.Name[:i]
		t.Name = strings.TrimSpace(q.Name[i+3:])
	} else {
		t.Name = q.Name
	}
	t.ImageUrl = maImageURL(q.Image)
	return t
}

// maStripVersionSuffix removes a trailing "(version)" from a composed queue
// item name ("A - B (remix)" with version "remix" -> "A - B").
func maStripVersionSuffix(name, version string) string {
	if version == "" {
		return name
	}
	suffix := "(" + version + ")"
	if strings.HasSuffix(name, suffix) {
		return name[:len(name)-len(suffix)]
	}
	return name
}

// maQueueItemsToTracks maps the upcoming queue (issue #138): capped at
// QueueLimit like the Connect projection and dropping entries with no usable
// identity - the UI sanitizer would reject them anyway and the pane keys off
// track_id/uri.
func maQueueItemsToTracks(items []ma.QueueItem) []QueueTrack {
	if len(items) > QueueLimit {
		items = items[:QueueLimit]
	}
	out := make([]QueueTrack, 0, len(items))
	for i := range items {
		t := maQueueItemToTrack(items[i])
		if t.Uri == "" && t.Name == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// MaPlaylist is one entry of GET /ma/playlists (issue #138, pinned contract).
type MaPlaylist struct {
	ItemID   string `json:"item_id"`
	Name     string `json:"name"`
	URI      string `json:"uri"`
	ImageURL string `json:"image_url"`
}

// MaTrack is one entry of GET /ma/playlist-items and GET /ma/recent (issue
// #135, pinned contract): the shared track-card shape.
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
		t := MaTrack{ID: it.ItemID, Name: it.Name, URI: it.URI, Album: it.Album, Artists: names, Position: it.Position}
		if it.Duration > 0 {
			t.DurationMs = int64(it.Duration) * 1000
		}
		t.ImageURL = maImageURL(it.Image)
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
