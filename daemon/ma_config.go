package daemon

import "encoding/json"

// issue #134 (daemon part 1): the Music Assistant backend config lives in
// the UI settings blob, next to the "ha" object - the same opaque blob the
// UI persists and the daemon mirrors (PUT /settings). The blob gains two
// ADDITIVE keys only (no schema bump):
//
//	{ ..., "ma": { "url": "...", "token": "..." }, "playbackSource": "spotify" | "ma" }
//
// This file is the daemon-side reader for that shape and mirrors the
// strict parsing of ParseHaConfig (ha_config.go) and ParsePiProfiles
// (pi_profiles.go):
//   - empty or malformed blob  -> nil (an old UI never sent "ma" at all)
//   - missing "ma" key         -> nil
//   - "url" is canonicalized with normalizeHaURL (ha_login.go) - the same
//     single normalizer the HA path uses: trim, default scheme http,
//     ws(s)://->http(s)://, strip trailing slash. A bare "host:port" typed
//     into the UI field must still work, and a non-canonical stored value
//     is repaired at parse time rather than failing the request. Empty or
//     unusable url -> nil (MA backend disabled).
//   - "token" is taken VERBATIM: it is an opaque bearer token, may contain
//     anything, and must never be trimmed, normalized or logged.
//
// playbackSource is read separately by playbackSourceFromSettings: a
// missing key (every old blob) or any other value means "spotify", so
// pre-#134 blobs keep byte-identical behavior (issue #134 contract).
//
// The daemon NEVER writes the blob (the pi_profiles.go convention); the
// UI is the only writer and the daemon re-parses on every read, so a
// config change from the UI applies without a restart.

// MaConfig is the stored Music Assistant connection (issue #134).
type MaConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// PlaybackSourceMA marks the settings-blob value that selects the Music
// Assistant backend over the Spotify Connect observer.
const PlaybackSourceMA = "ma"

// ParseMaConfig extracts the "ma" object from the UI settings blob, or
// returns nil when the blob is empty/malformed, has no usable "ma" key,
// or its url normalizes to nothing (see file header). The returned config
// always carries a canonical http(s) base URL without trailing slash.
func ParseMaConfig(blob []byte) *MaConfig {
	if len(blob) == 0 {
		return nil
	}

	var raw struct {
		Ma *struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		} `json:"ma"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil || raw.Ma == nil {
		return nil
	}

	// same single canonicalizer as the HA path (issue #134): trim, default
	// http scheme, ws(s)->http(s), strip trailing slash
	url := normalizeHaURL(raw.Ma.URL)
	if url == "" {
		return nil
	}

	return &MaConfig{URL: url, Token: raw.Ma.Token}
}

// playbackSourceFromSettings returns the blob's "playbackSource" value
// ("spotify" or "ma"), or "" when absent/malformed. Callers treat every
// non-"ma" value as spotify (issue #134).
func playbackSourceFromSettings(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	var raw struct {
		PlaybackSource string `json:"playbackSource"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return ""
	}
	return raw.PlaybackSource
}
