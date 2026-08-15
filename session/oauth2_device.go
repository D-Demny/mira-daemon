package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// OAuth2 device flow
// Device flow gets us a URL we render as a QR code, then we poll the token endpoint
const (
	deviceAuthorizeURL = "https://accounts.spotify.com/oauth2/device/authorize"
	deviceTokenURL     = "https://accounts.spotify.com/api/token"

	deviceFlowScopes = "app-remote-control,playlist-modify,playlist-modify-private,playlist-modify-public,playlist-read,playlist-read-collaborative,playlist-read-private,streaming,ugc-image-upload,user-follow-modify,user-follow-read,user-library-modify,user-library-read,user-modify,user-modify-playback-state,user-modify-private,user-personalized,user-read-birthdate,user-read-currently-playing,user-read-email,user-read-play-history,user-read-playback-position,user-read-playback-state,user-read-private,user-read-recently-played,user-top-read"

	deviceFlowUserAgent = "Spotify/125700463 Win32_x86_64/0 (PC desktop)"
)

type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

type deviceTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

var errDeviceTokenPollTransient = errors.New("transient device token poll failure")

// oauthTokens holds the OAuth access token, refresh token, and expiration time
type oauthTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// runDeviceAuthFlow blocks until user authorizes / code expires / ctx cancel.
// urlCallback fires once with the verification URI so the caller can render
// a QR code while polling. returns the oauthTokens (access + refresh), username
// comes back from the AP via APWelcome
func runDeviceAuthFlow(
	ctx context.Context,
	log librespot.Logger,
	client *http.Client,
	urlCallback func(url string),
) (*oauthTokens, error) {
	auth, err := requestDeviceCode(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("requesting device code: %w", err)
	}

	log.Infof("open %s on your phone to authorize this device", auth.VerificationURIComplete)
	if urlCallback != nil {
		urlCallback(auth.VerificationURIComplete)
	}

	interval := time.Duration(auth.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expires := time.Duration(auth.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = 10 * time.Minute
	}
	deadline := time.Now().Add(expires)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("device code expired before user authorized")
		}

		tok, err := pollDeviceToken(ctx, client, auth.DeviceCode)
		if err != nil {
			if errors.Is(err, errDeviceTokenPollTransient) {
				log.WithError(err).Warn("device auth token poll failed transiently; keeping current QR code")
				continue
			}
			return nil, err
		}
		if tok == nil {
			continue
		}

		return &oauthTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second),
		}, nil
	}
}

func requestDeviceCode(ctx context.Context, client *http.Client) (*deviceAuthResponse, error) {
	body := url.Values{
		"client_id": {librespot.ClientIdHex},
		"creation_point": {
			"https://login.app.spotify.com/?client_id=" + librespot.ClientIdHex +
				"&utm_source=spotify&utm_medium=desktop-win32&utm_campaign=organic",
		},
		"intent": {"login"},
		"scope":  {deviceFlowScopes},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceAuthorizeURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", deviceFlowUserAgent)
	req.Header.Set("Accept-Language", "en-Latn-US,en-US;q=0.9,en-Latn;q=0.8,en;q=0.7")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device authorize: %s", resp.Status)
	}

	var out deviceAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if out.DeviceCode == "" || out.VerificationURIComplete == "" {
		return nil, fmt.Errorf("device authorize response missing required fields")
	}
	return &out, nil
}

// pollDeviceToken: (nil, nil) when user hasn't authorized yet (pending/slow_down),
// non-nil token on success, error otherwise
func pollDeviceToken(ctx context.Context, client *http.Client, deviceCode string) (*deviceTokenResponse, error) {
	body := url.Values{
		"client_id":   {librespot.ClientIdHex},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceTokenURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: token request failed: %v", errDeviceTokenPollTransient, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: token endpoint returned %s", errDeviceTokenPollTransient, resp.Status)
	}

	var out deviceTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding token response: %w", err)
	}

	if resp.StatusCode == http.StatusOK && out.AccessToken != "" {
		return &out, nil
	}

	switch out.Error {
	case "authorization_pending", "slow_down":
		return nil, nil
	case "":
		return nil, fmt.Errorf("token endpoint returned %s with no error field", resp.Status)
	default:
		desc := out.ErrorDescription
		if desc == "" {
			desc = out.Error
		}
		return nil, fmt.Errorf("device authorization failed: %s", desc)
	}
}
