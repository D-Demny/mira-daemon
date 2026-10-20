package ma

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
	ItemID    string       `json:"item_id"`
	Provider  string       `json:"provider"`
	Name      string       `json:"name"`
	URI       string       `json:"uri"`
	MediaType string       `json:"media_type"`
	Album     string       `json:"album"`
	Version   string       `json:"version"`
	Artists   []ArtistName `json:"artists"`
	Duration  int          `json:"duration"` // seconds
	Position  int          `json:"position"` // absolute index in the parent playlist (playlist_tracks only)
	Image     *MediaImage  `json:"image"`
}

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

// QueueItem is the subset of MA's QueueItem schema (player_queues/get). For
// music items Name is pre-composed as "Artist(s) - Title" with an optional
// trailing "(version)" suffix, and MediaItem carries the real metadata when
// present (its artists list can be simplified away, hence the fallback
// parsing in the daemon's queue mapping).
type QueueItem struct {
	QueueID     string      `json:"queue_id"`
	QueueItemID string      `json:"queue_item_id"`
	Name        string      `json:"name"`
	Duration    *float64    `json:"duration"` // seconds, nil when unknown
	MediaItem   *TrackItem  `json:"media_item"`
	Image       *MediaImage `json:"image"`
}

// PlayerQueue is the subset of player_queues/get the observer maps into
// RemoteState: the upcoming queue (NextTracks) feeds the "Läuft gerade"
// pane; PreviousTrack maps to PrevTracks for parity with the Connect
// projection.
type PlayerQueue struct {
	QueueID       string      `json:"queue_id"`
	CurrentTrack  *QueueItem  `json:"current_track"`
	PreviousTrack *QueueItem  `json:"previous_track"`
	NextTracks    []QueueItem `json:"next_tracks"`
}
