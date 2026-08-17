package session

import (
	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/apresolve"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"
	"net/http"
)

type Options struct {
	// Log is the base logger entry to use.
	Log librespot.Logger

	// DeviceType is the Spotify showed device type, required.
	DeviceType devicespb.DeviceType
	// DeviceId is the Spotify device ID, required.
	DeviceId string
	// Credentials is the credentials to be used for authentication, required.
	Credentials any

	// ClientToken is the Spotify client token, leave empty to let the server generate one.
	ClientToken string
	// Resolver is an instance of apresolve.ApResolver, leave nil to use the default one.
	Resolver *apresolve.ApResolver

	// Client is the HTTP client to use for the session, leave empty for a new one.
	Client *http.Client

	// AppState is the app state to use.
	AppState *librespot.AppState

	// PersistedOAuth restores the Web API OAuth tokens from app state on
	// restart (StoredCredentials path). nil or empty refresh token = no restore.
	PersistedOAuth *librespot.OAuthState

	// fired with the device-flow verification URL when auth blocks for the user
	AuthURLCallback func(url string)

	// fired after the device flow completes and after every successful token
	// refresh so the caller can persist the tokens
	OAuthTokenChanged func(*librespot.OAuthState)
}

// InteractiveCredentials picks the OAuth2 device flow (RFC 8628), no fields needed
type InteractiveCredentials struct{}

type SpotifyTokenCredentials struct {
	Username string
	Token    string
}

type StoredCredentials struct {
	Username string
	Data     []byte
}

type BlobCredentials struct {
	Username string
	Blob     []byte
}
