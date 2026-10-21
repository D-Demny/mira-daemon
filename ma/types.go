package ma

import (
	"encoding/json"
	"fmt"
)

// Player is the subset of the MA Player schema the daemon consumes
// (issue #134): the observer needs playback state, position, volume and
// the current media; /ma/devices needs id/name/availability. Everything
// else in the 40-field schema is deliberately ignored so wire growth
// can never break unmarshalling.
type Player struct {
	PlayerID      string `json:"player_id"`
	Name          string `json:"name"`
	Available     bool   `json:"available"`
	PlaybackState string `json:"playback_state"` // "idle" | "playing" | "paused"

	// Playback position in seconds, as of ElapsedTimeLastUpdated (epoch
	// SECONDS). Both are nullable: MA answers null for fields of a player
	// that is unpowered or unavailable (verified live against 2.10.7), so
	// they must not be plain numbers.
	ElapsedTime            *float64 `json:"elapsed_time"`
	ElapsedTimeLastUpdated *float64 `json:"elapsed_time_last_updated"`

	// VolumeLevel is 0..100; nullable like the position fields.
	VolumeLevel *int  `json:"volume_level"`
	VolumeMuted *bool `json:"volume_muted"`

	// CurrentMedia is nil while the player has no media at all (and can be
	// STALE after the player went idle: MA keeps the last item - callers
	// must gate on PlaybackState first, not on this pointer).
	CurrentMedia *CurrentMedia `json:"current_media"`

	// Native multi-room grouping (WiiM firmware level; verified live against
	// 2.10.7's wiim provider): the group MASTER carries every member - itself
	// included - in GroupMembers with SyncedTo nil, each SLAVE has an empty
	// GroupMembers and points to its master via SyncedTo. The daemon uses
	// this to synthesize one "group" device in /ma/devices and to resolve
	// commands for a slave or the group to the firmware master (the master
	// mirrors playback to the slaves natively).
	GroupMembers []string `json:"group_members"`
	SyncedTo     *string  `json:"synced_to"` // master player id, nil when not grouped
}

// CurrentMedia is the subset of MA's PlayerMedia schema the observer maps
// into RemoteState (issue #134). Duration is in SECONDS (live check: a
// track whose elapsed_time froze at its end reported duration 184 for the
// same 184s position). Nullable string fields decode to "".
type CurrentMedia struct {
	URI         string `json:"uri"`
	MediaType   string `json:"media_type"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Album       string `json:"album"`
	AlbumArtist string `json:"album_artist"`
	ImageURL    string `json:"image_url"`
	Duration    int    `json:"duration"` // seconds
}

// MediaImage is MA's image object {type, path, provider}: for music items
// Path is the direct media URL (absolute or server-relative; the daemon
// resolves it against the configured base url before serving it to the UI).
type MediaImage struct {
	Type     string `json:"type"`
	Path     string `json:"path"`
	Provider string `json:"provider"`
	// RemotelyAccessible marks absolute provider urls (e.g. the Spotify CDN)
	// the UI can fetch directly. Entries marked false are server-local files
	// served through MA's media proxy, which the daemon does not route - they
	// are skipped by maImageURL (card gets no art rather than a dead link).
	RemotelyAccessible bool `json:"remotely_accessible"`
}

// ArtistName is the subset of MA's Artist object track/queue metadata carries
// (name only). Queue items may omit it entirely - their top-level Name is
// pre-composed instead (see QueueItem).
type ArtistName struct {
	Name string `json:"name"`
}

// TrackItem is the shared subset of MA's media-item schema: playlist tracks
// (music/playlists/playlist_tracks), recently-played mappings
// (music/recently_played_items) and the nested media_item of queue items all
// decode into it. Fields a command omits stay zero-valued - play-log mappings
// in particular carry no album/artists/position (MA 2.10.7 limitation).
type TrackItem struct {
	ItemID    string         `json:"item_id"`
	Provider  string         `json:"provider"`
	Name      string         `json:"name"`
	URI       string         `json:"uri"`
	MediaType string         `json:"media_type"`
	Album     AlbumRef       `json:"album"`
	Version   string         `json:"version"`
	Artists   []ArtistName   `json:"artists"`
	Duration  int            `json:"duration"` // seconds
	Position  int            `json:"position"` // absolute index in the parent playlist (playlist_tracks only)
	Image     *MediaImage    `json:"image"`
	Metadata  *MediaMetadata `json:"metadata"`
}

// MediaMetadata is the subset of a media item's metadata blob the daemon
// consumes (issue #138): images. Queue items carry their image top-level,
// but LIBRARY track lists (playlist_tracks / album_tracks) nest the cover
// under metadata.images only (verified live against 2.10.7), so upcoming
// cards and playlist track cards read it from here via TrackItem.Cover.
type MediaMetadata struct {
	Images []MediaImage `json:"images"`
}

// Cover returns the item's image entry: top-level first, then the first
// metadata image; nil when neither is present.
func (t TrackItem) Cover() *MediaImage {
	if t.Image != nil {
		return t.Image
	}
	if t.Metadata != nil && len(t.Metadata.Images) > 0 {
		return &t.Metadata.Images[0]
	}
	return nil
}

// AlbumRef is MA's media-item album field, which arrives as a plain NAME
// string in some responses but as a FULL album object in playlist-track and
// queue-media payloads (verified live against 2.10.7 - the object shape
// breaks plain string unmarshalling of the whole TrackItem, issue #138).
// Both shapes decode to the album name; Name() yields "" when absent.
type AlbumRef struct {
	name string
}

// UnmarshalJSON implements json.Unmarshaler: a JSON string or an object
// carrying a "name" field.
func (a *AlbumRef) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		a.name = s
		return nil
	}
	var o struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return fmt.Errorf("ma: unknown album field shape %q", b)
	}
	a.name = o.Name
	return nil
}

// Name returns the decoded album name.
func (a AlbumRef) Name() string { return a.name }

// BrowseItem is the union subset of music/browse results used for playlist
// discovery: provider FOLDERS carry Path (and ItemID "back" as breadcrumb,
// which callers skip); media ITEMS carry URI. Every playlist - built-in and
// provider-synced alike - surfaces as a library item with URI
// "library://playlist/<id>" (verified live against 2.10.7).
type BrowseItem struct {
	ItemID    string      `json:"item_id"`
	Provider  string      `json:"provider"`
	Name      string      `json:"name"`
	URI       string      `json:"uri"`
	Path      string      `json:"path"`
	MediaType string      `json:"media_type"`
	Image     *MediaImage `json:"image"`
	// Metadata carries the richer item blob. Playlist browse entries have NO
	// top-level image - their covers live in Metadata.Images (verified live
	// against 2.10.7); the daemon only serves entries marked
	// remotely accessible (absolute provider URLs fetchable by the UI).
	Metadata *ItemMetadata `json:"metadata"`
}

// ItemMetadata is the subset of the media-item metadata blob the content
// handlers consume (image lists; everything else stays unparsed).
type ItemMetadata struct {
	Images []MediaImage `json:"images"`
}

// QueueItem is the subset of MA's QueueItem schema (player_queues/items, and
// the current/next item snapshots of player_queues/get). For music items
// Name is pre-composed as "Artist(s) - Title" with an optional trailing
// "(version)" suffix; MediaItem carries the real metadata when present. The
// daemon's queue mapping falls back to the pre-composed name line for
// entries without a MediaItem (see maQueueItemToUpcoming).
type QueueItem struct {
	QueueID     string      `json:"queue_id"`
	QueueItemID string      `json:"queue_item_id"`
	Name        string      `json:"name"`
	Duration    *float64    `json:"duration"` // seconds, nil when unknown
	MediaItem   *TrackItem  `json:"media_item"`
	Image       *MediaImage `json:"image"`
}

// QueueSource is the subset of one queue SOURCE media item (player_queues/get
// `sources[]`): a single-source queue is identified by it - the daemon uses
// it as the playback CONTEXT uri and the idle-replay source (see
// daemon/player_ma.go). The upcoming list does NOT come from here.
type QueueSource struct {
	ItemID    string `json:"item_id"`
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	URI       string `json:"uri"`
	MediaType string `json:"media_type"` // "playlist" | "album" | ...
}

// PlayerQueue is the subset of player_queues/get the observer consumes (live
// verified against MA 2.10.7): the response carries NO item list - only the
// item count, the current index, the current/next queue items and the source
// items. The "Läuft gerade" upcoming pane is the daemon's own LIVE
// enumeration of player_queues/items on top of this position (see
// maUpcomingTracks in player_ma.go).
type PlayerQueue struct {
	QueueID        string        `json:"queue_id"`
	Items          int           `json:"items"`         // total queued item count
	CurrentIndex   *int          `json:"current_index"` // 0-based position of CurrentItem
	State          string        `json:"state"`         // "playing" | "paused" | "stopped" ...
	Ended          bool          `json:"ended"`
	ShuffleEnabled bool          `json:"shuffle_enabled"`
	CurrentItem    *QueueItem    `json:"current_item"`
	NextItem       *QueueItem    `json:"next_item"` // nil at the end of the queue
	Sources        []QueueSource `json:"sources"`
}
