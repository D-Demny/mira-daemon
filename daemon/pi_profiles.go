package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// epic 10 ticket10-5: multi-Pi profile model (daemon side). The UI
// (settings.ts, blob schema v2, ticket10-5A) persists
//
//	{v: 2, piProfiles: [{id, label, ip, user, password, keyInstalled}], activePiId}
//
// in the same opaque settings blob the daemon mirrors from the UI. This file
// is the daemon-side reader for that shape, the backward-compat handling of
// the legacy flat shape, the profile-id sanitization shared by the key
// storage, and the profile deletion endpoint.
//
// Backward compatibility (design decision, ticket10-5B): a legacy v1 blob
// ({piServer: {ip, user, password}}, written by UI images before
// ticket10-5A) is read as exactly ONE implicit profile with the fixed id
// "legacy" - but only while the blob carries no "piProfiles" key at all.
// Both shapes occur during the transition (old UI image + new daemon); a
// fresh install (new UI) always writes "piProfiles" (possibly empty) -> the
// v2 model wins and an empty list means "no target" (session idle). The
// daemon-internal "legacy" id only ever appears for such old-shape blobs;
// the new UI never writes it (its one-time migration re-shapes the legacy
// entry into a v2 profile with id "pi-1").
//
// Key storage per profile (sshkey.go, ticket10-5): the legacy profile keeps
// the original /etc/mira/ssh/id_ed25519 (ticket10-3, reused without re-
// generation); every other profile gets <profileId>_ed25519. Transition
// note: a profile migrated by the new UI (id "pi-1") gets its OWN fresh
// key - if the Pi only trusts the old id_ed25519 yet, the first wizard run
// after the upgrade installs the new key (idempotent, password fallback);
// the legacy key file is kept, never regenerated or deleted by the daemon.

// PiProfile is one stored Raspberry Pi (ticket10-5), the same shape the UI
// persists in the settings blob.
type PiProfile struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Ip           string `json:"ip"`
	User         string `json:"user"`
	Password     string `json:"password"`
	KeyInstalled bool   `json:"keyInstalled"`
}

// legacyPiProfileID is the fixed id of the implicit profile derived from a
// legacy v1 settings blob (see the file header).
const legacyPiProfileID = "legacy"

// maxProfileIDLen bounds a profile id for its use in a file name.
const maxProfileIDLen = 64

// ParsePiProfiles extracts the stored Pi profiles from a UI settings blob.
// It returns nil when the blob carries no profile information (fresh
// install with an empty v2 list, legacy blob with empty ip/user, empty or
// malformed blob) - callers treat that as "no target". The v2 model wins
// over the legacy shape whenever the "piProfiles" key is present, even as
// an empty array.
func ParsePiProfiles(blob []byte) []PiProfile {
	if len(blob) == 0 {
		return nil
	}
	var raw struct {
		PiProfiles json.RawMessage `json:"piProfiles"`
		PiServer   *struct {
			Ip   string `json:"ip"`
			User string `json:"user"`
		} `json:"piServer"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil
	}
	if len(raw.PiProfiles) > 0 {
		var profiles []PiProfile
		if err := json.Unmarshal(raw.PiProfiles, &profiles); err != nil {
			return nil
		}
		for i := range profiles {
			profiles[i].ID = strings.TrimSpace(profiles[i].ID)
			profiles[i].Ip = strings.TrimSpace(profiles[i].Ip)
			profiles[i].User = strings.TrimSpace(profiles[i].User)
		}
		return profiles
	}
	if raw.PiServer != nil {
		ip := strings.TrimSpace(raw.PiServer.Ip)
		user := strings.TrimSpace(raw.PiServer.User)
		if ip != "" && user != "" {
			return []PiProfile{{ID: legacyPiProfileID, Label: legacyPiProfileID, Ip: ip, User: user}}
		}
	}
	return nil
}

// ResolveActiveProfile returns the active profile of a UI settings blob:
// the profile activePiId points at, or the FIRST profile when activePiId is
// missing/stale (the same fallback the UI's activePiProfile uses). It
// returns nil when the blob has no profiles.
func ResolveActiveProfile(blob []byte) *PiProfile {
	profiles := ParsePiProfiles(blob)
	if len(profiles) == 0 {
		return nil
	}
	var raw struct {
		ActivePiId string `json:"activePiId"`
	}
	if len(blob) > 0 {
		_ = json.Unmarshal(blob, &raw)
	}
	if raw.ActivePiId != "" {
		for i := range profiles {
			if profiles[i].ID == raw.ActivePiId {
				return &profiles[i]
			}
		}
	}
	return &profiles[0]
}

// sanitizeProfileID validates a profile id for its use in a file name
// (<id>_ed25519 under the key directory next to id_ed25519). The denylist
// mirrors installKeyCommand (no quote/dollar/backtick/whitespace/newline -
// ids also end up in shell-rendered paths) plus the filesystem guards a
// path component needs: no separators, no ".." sequence, no leading dot
// (dotfile/traversal), bounded length. The UI only ever produces pi-<n>
// ids; anything else is refused, not mangled.
func sanitizeProfileID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("profile id is empty")
	}
	if len(id) > maxProfileIDLen {
		return "", fmt.Errorf("profile id too long (%d chars, max %d)", len(id), maxProfileIDLen)
	}
	for _, bad := range []string{`"`, "$", "`", "\n", "\r", "\t", " ", "\\", "/"} {
		if strings.Contains(id, bad) {
			return "", fmt.Errorf("profile id contains unsupported character %q", bad)
		}
	}
	if strings.Contains(id, "..") || strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("profile id %q is not a safe file name", id)
	}
	return id, nil
}

// PiProfileDeleteRequest is the DELETE /api/pi/profile body: the SSH
// credentials of the profile's Pi (the UI still knows them right before it
// removes the profile from the store). All fields optional: without
// ip/user only the device-side key files are removed.
type PiProfileDeleteRequest struct {
	Ip       string `json:"ip"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// PiProfileDeleteResult is the DELETE /api/pi/profile response.
type PiProfileDeleteResult struct {
	// KeyRemoved reports whether device-side key files were removed.
	KeyRemoved bool `json:"key_removed"`
	// AuthorizedKeysRemoved reports whether the public key was removed
	// from the Pi's authorized_keys.
	AuthorizedKeysRemoved bool `json:"authorized_keys_removed"`
	// Error carries a Pi-side cleanup problem, empty when none.
	Error string `json:"error,omitempty"`
}

// PiProfileHandler is the cross-service handler for the profile deletion
// endpoint (ticket10-5), injected via SetPiProfileHandler on the ApiServer.
type PiProfileHandler interface {
	DeletePiProfile(profileID string, req PiProfileDeleteRequest) (PiProfileDeleteResult, error)
}

// PiProfileService implements PiProfileHandler.
type PiProfileService struct {
	log librespot.Logger
}

// NewPiProfileService builds the profile deletion handler.
func NewPiProfileService(log librespot.Logger) *PiProfileService {
	return &PiProfileService{log: log}
}

// DeletePiProfile removes a profile's device-side key pair and - when the
// request carries the profile's SSH credentials - its public key from the
// Pi's authorized_keys (key-first with the profile's own key, password
// fallback, like the install in ticket10-3).
//
// Design decision (ticket10-5B): the endpoint is
// DELETE /api/pi/profile?id=<profileId> with an optional JSON body
// {ip,user,password}. The device-side key removal always happens (the
// profile is gone either way); the Pi-side cleanup is best effort and
// reported in the result - it is the UI's decision to surface the error.
// The daemon does NOT touch the settings blob: the UI removes the profile
// from the store itself (and updates activePiId). A session bound to the
// deleted profile reacts through the normal config re-read (the profile is
// gone from the blob on the next tick).
func (s *PiProfileService) DeletePiProfile(profileID string, req PiProfileDeleteRequest) (PiProfileDeleteResult, error) {
	id, err := sanitizeProfileID(profileID)
	if err != nil {
		return PiProfileDeleteResult{}, err
	}
	keyPath, err := KeyPathForProfile(id)
	if err != nil {
		return PiProfileDeleteResult{}, err
	}

	// read the public key BEFORE the device-side removal (the Pi-side
	// cleanup needs its content)
	pub, pubErr := ReadPublicKey(keyPath)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	result := PiProfileDeleteResult{}
	for _, p := range []string{keyPath, keyPath + ".pub"} {
		err := os.Remove(p)
		if err == nil {
			result.KeyRemoved = true
		} else if !os.IsNotExist(err) {
			return PiProfileDeleteResult{}, fmt.Errorf("removing %s: %w", p, err)
		}
	}

	ip := strings.TrimSpace(req.Ip)
	user := strings.TrimSpace(req.User)
	if ip == "" || user == "" {
		return result, nil // no credentials: device-side cleanup only
	}
	if pubErr != nil {
		if os.IsNotExist(pubErr) {
			return result, nil // no key file: nothing to remove on the Pi
		}
		result.Error = "reading public key: " + pubErr.Error()
		return result, nil
	}
	script, err := removeKeyCommand(pub)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	// the device key was just removed: a real ssh -i <gone> fails with
	// exit 255 and RunKeyFirstWithKey falls back to the password
	usedKey, out, runErr := RunKeyFirstWithKey(ctx, keyPath, ip, user, req.Password, script)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line != "" {
			s.log.Infof("pi-profile: %s", line)
		}
	}
	if runErr != nil {
		result.Error = runErr.Error()
		return result, nil
	}
	mode := "password"
	if usedKey {
		mode = "ssh key"
	}
	s.log.Infof("pi-profile: key of profile %q removed from %s (via %s)", id, ip, mode)
	result.AuthorizedKeysRemoved = true
	return result, nil
}
