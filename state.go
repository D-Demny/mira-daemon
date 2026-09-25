package go_librespot

import (
	"encoding/json"
	"sync"
	"time"
)

// BluetoothKnownDevice is one entry in the prioritized reconnect list
// Starred devices are tried first
type BluetoothKnownDevice struct {
	Address       string    `json:"address"`
	Name          string    `json:"name,omitempty"`
	Starred       bool      `json:"starred,omitempty"`
	LastConnected time.Time `json:"last_connected,omitempty"`
}

// OAuthState persists the OAuth token pair used for Web API calls so the
// refresh token survives daemon restarts (StoredCredentials restarts would
// otherwise lose it and the Web API falls back to the Login5 token).
type OAuthState struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // unix seconds
}

type AppState struct {
	sync.Mutex

	DeviceId     string          `json:"device_id"`
	EventManager json.RawMessage `json:"event_manager"`
	Credentials  struct {
		Username string `json:"username"`
		Data     []byte `json:"data"`
	} `json:"credentials"`
	LastVolume *uint32 `json:"last_volume"`

	// LastBluetoothPanAddress is the MAC of the most recently PAN paired device
	LastBluetoothPanAddress string `json:"last_bluetooth_pan_address,omitempty"`

	// KnownBluetoothDevices is the prioritized reconnect list managed by the bluetooth manager
	KnownBluetoothDevices []BluetoothKnownDevice `json:"known_bluetooth_devices,omitempty"`

	// Settings is the user-facing preference blob edited from the ui
	Settings json.RawMessage `json:"settings,omitempty"`

	// OAuth holds the persisted Web API OAuth tokens (see OAuthState)
	OAuth OAuthState `json:"oauth,omitempty"`

	// last offset from the check in service
	UtcOffsetMin *int `json:"utc_offset_min,omitempty"`

	// newest release
	LatestVersion    string   `json:"latest_version,omitempty"`
	LatestHighlights []string `json:"latest_highlights,omitempty"`
	UpdateMandatory  bool     `json:"update_mandatory,omitempty"`
}
