package daemon

import (
	"encoding/json"
	"strings"
)

// ticket 9.4 (daemon part 1): the Home Assistant connection config moves
// from the build-time config.yml into the UI settings blob (schema v3).
// The UI (settings.ts, ticket 9.4 task 4) persists
//
//	{v: 3, ha: {url, username, password, token, tokenSource}, ...}
//
// in the same opaque settings blob the daemon mirrors from the UI
// (PUT /settings, mirrored to disk). This file is the daemon-side reader
// for that shape, mirroring the ParsePiProfiles pattern (pi_profiles.go).
//
// Resolution order (per proxy request, getHomeAssistantConfig in
// api_server.go): runtime blob "ha" object -> build-time config.yml
// defaults (SetHomeAssistantConfig). A blob without a usable "ha" object
// (no key, empty url, empty or corrupt blob) resolves to the defaults,
// so existing installations behave exactly as before until the user first
// saves the HA settings in the UI (migration, ticket 9.4 section 5).
//
// The daemon never writes the blob (pi_profiles.go convention): the UI is
// the sole writer, the daemon only reads.
//
// Parsing rules (strict, like ParsePiProfiles):
//   - empty or malformed blob -> nil (defaults)
//   - no "ha" key (v2 blob, old UI) -> nil (defaults)
//   - "url" and "username" are trimmed, "password" and "token" are
//     VERBATIM (a user may legitimately choose a token with leading or
//     trailing whitespace; the UI writes what it stores)
//   - tokenSource is validated against the UI enum 'default' | 'manual' |
//     'login'; any foreign value (corruption, hand-edited blob, future
//     enum growth the daemon does not know yet) normalizes to 'default'.
//     'default' = build-time value from config.yml, no login needed.
//   - a "ha" object whose url trims to empty returns nil: an HA object
//     without a URL is useless for the proxy (it cannot forward anything),
//     so the config.yml defaults apply instead. The UI writes url:'' for
//     "not configured" - that must map to the defaults, not to a broken
//     runtime config.

// HaConfig is the stored Home Assistant connection (ticket 9.4), the same
// shape the UI persists in the settings blob. Username and Password are
// consumed by the WS login endpoint (task 9, later part of this ticket);
// the /ha-api/ proxy only needs URL + Token.
type HaConfig struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Token       string `json:"token"`
	TokenSource string `json:"tokenSource"`
}

// haTokenSources are the valid tokenSource values the UI writes.
var haTokenSources = map[string]bool{
	"default": true,
	"manual":  true,
	"login":   true,
}

// ParseHaConfig extracts the Home Assistant config from a UI settings
// blob. It returns nil when the blob carries no usable "ha" object
// (no key, empty url, empty or malformed blob) - callers treat that as
// "fall back to the config.yml defaults". See the file header for the
// strict parsing rules.
func ParseHaConfig(blob []byte) *HaConfig {
	if len(blob) == 0 {
		return nil
	}
	var raw struct {
		Ha *struct {
			URL         string `json:"url"`
			Username    string `json:"username"`
			Password    string `json:"password"`
			Token       string `json:"token"`
			TokenSource string `json:"tokenSource"`
		} `json:"ha"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil
	}
	if raw.Ha == nil {
		return nil
	}
	url := strings.TrimSpace(raw.Ha.URL)
	if url == "" {
		return nil
	}
	source := raw.Ha.TokenSource
	if !haTokenSources[source] {
		source = "default"
	}
	return &HaConfig{
		URL:         url,
		Username:    strings.TrimSpace(raw.Ha.Username),
		Password:    raw.Ha.Password,
		Token:       raw.Ha.Token,
		TokenSource: source,
	}
}
