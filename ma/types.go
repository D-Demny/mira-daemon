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
