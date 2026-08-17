package session

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
	"github.com/devgianlu/go-librespot/apresolve"
	"github.com/devgianlu/go-librespot/dealer"
	"github.com/devgianlu/go-librespot/login5"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"
	credentialspb "github.com/devgianlu/go-librespot/proto/spotify/login5/v3/credentials"
	"github.com/devgianlu/go-librespot/spclient"
)

type Session struct {
	deviceType  devicespb.DeviceType
	deviceId    string
	clientToken string

	client *http.Client

	log librespot.Logger

	resolver *apresolve.ApResolver
	login5   *login5.Login5

	ap     *ap.Accesspoint
	sp     *spclient.Spclient
	dealer *dealer.Dealer

	// oauthTokens holds the OAuth access token and refresh token.
	// Used for Web API calls (has user scopes like playlist-read).
	// Unlike the Login5 token (Spotify Connect only, no user scopes), it works with api.spotify.com.
	oauthMu        sync.Mutex
	oauthToken     string
	oauthRefresh   string
	oauthExpiresAt time.Time

	// tokenEndpoint is where OAuth token refreshes go, overridable in tests
	tokenEndpoint string
	// oauthChanged persists the token pair after device flow + every refresh
	oauthChanged func(*librespot.OAuthState)
}

func NewSessionFromOptions(ctx context.Context, opts *Options) (*Session, error) {
	// validate device type
	if opts.DeviceType == devicespb.DeviceType_UNKNOWN {
		return nil, fmt.Errorf("missing device type")
	}

	// validate device id
	if deviceId, err := hex.DecodeString(opts.DeviceId); err != nil {
		return nil, fmt.Errorf("invalid device id: %w", err)
	} else if len(deviceId) != 20 {
		return nil, fmt.Errorf("invalid device id length: %s", opts.DeviceId)
	}

	s := Session{
		deviceType: opts.DeviceType,
		deviceId:   opts.DeviceId,
		client:     opts.Client,
		log:        opts.Log,
		// token refreshes go to the same endpoint the device flow polls
		tokenEndpoint: deviceTokenURL,
		oauthChanged:  opts.OAuthTokenChanged,
	}

	if s.client == nil {
		s.client = &http.Client{Timeout: 30 * time.Second}
	}

	// use provided client token or retrieve a new one
	if len(opts.ClientToken) == 0 {
		var err error
		s.clientToken, err = retrieveClientToken(ctx, s.client, s.deviceId)
		if err != nil {
			return nil, fmt.Errorf("failed obtaining client token: %w", err)
		}

		opts.Log.Debugf("obtained new client token (%d chars)", len(s.clientToken))
	} else {
		s.clientToken = opts.ClientToken
	}

	// use provided resolver or create a new one
	if opts.Resolver != nil {
		s.resolver = opts.Resolver
	} else {
		s.resolver = apresolve.NewApResolver(opts.Log, s.client)
	}

	// create new login5.Login5
	s.login5 = login5.NewLogin5(opts.Log, s.client, s.deviceId, s.clientToken)

	// connect to the accesspoint
	apAddr, err := s.resolver.GetAccesspoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed getting accesspoint from resolver: %w", err)
	}
	s.ap = ap.NewAccesspoint(opts.Log, apAddr, s.deviceId)

	// authenticate with the accesspoint using the proper credentials
	switch creds := opts.Credentials.(type) {
	case StoredCredentials:
		if err := s.ap.ConnectStored(ctx, creds.Username, creds.Data); err != nil {
			return nil, fmt.Errorf("failed authenticating accesspoint with stored credentials: %w", err)
		}
		// restore persisted Web API OAuth tokens so api.spotify.com keeps
		// working across daemon restarts without a new QR login
		if opts.PersistedOAuth != nil && opts.PersistedOAuth.RefreshToken != "" {
			s.oauthMu.Lock()
			s.oauthToken = opts.PersistedOAuth.AccessToken
			s.oauthRefresh = opts.PersistedOAuth.RefreshToken
			if opts.PersistedOAuth.ExpiresAt > 0 {
				s.oauthExpiresAt = time.Unix(opts.PersistedOAuth.ExpiresAt, 0)
			}
			s.oauthMu.Unlock()
			opts.Log.Infof("OAuth: restored persisted tokens (access_token=%d chars, refresh_token=%d chars, expires in %s)",
				len(s.oauthToken), len(s.oauthRefresh), time.Until(s.oauthExpiresAt).Round(time.Second))
		}
	case InteractiveCredentials:
		// device flow, user authorizes via the QR URL we send to AuthURLCallback.
		// username left empty, AP fills it via APWelcome from the token itself
		oauthTokens, err := runDeviceAuthFlow(ctx, opts.Log, s.client, opts.AuthURLCallback)
		if err != nil {
			return nil, fmt.Errorf("device authorization flow failed: %w", err)
		}

		if err := s.ap.ConnectSpotifyToken(ctx, "", oauthTokens.AccessToken); err != nil {
			return nil, fmt.Errorf("failed authenticating accesspoint interactively: %w", err)
		}

		// Store OAuth tokens for Web API calls (has user scopes like playlist-read)
		// instead of Login5 token (Spotify Connect only, no user scopes)
		s.oauthMu.Lock()
		s.oauthToken = oauthTokens.AccessToken
		s.oauthRefresh = oauthTokens.RefreshToken
		s.oauthExpiresAt = oauthTokens.ExpiresAt
		s.oauthMu.Unlock()
		opts.Log.Infof("OAuth: device flow complete (access_token=%d chars, refresh_token=%d chars, expires_in=%ds)",
			len(oauthTokens.AccessToken), len(oauthTokens.RefreshToken),
			int(time.Until(oauthTokens.ExpiresAt).Seconds()))
		s.emitOAuthChanged()
	case SpotifyTokenCredentials:
		if err := s.ap.ConnectSpotifyToken(ctx, creds.Username, creds.Token); err != nil {
			return nil, fmt.Errorf("failed authenticating accesspoint with username and spotify token: %w", err)
		}
		// creds.Token IS the OAuth token — store it for Web API calls
		s.oauthMu.Lock()
		s.oauthToken = creds.Token
		s.oauthExpiresAt = time.Now().Add(30 * time.Minute)
		s.oauthMu.Unlock()
	case BlobCredentials:
		if err := s.ap.ConnectBlob(ctx, creds.Username, creds.Blob); err != nil {
			return nil, fmt.Errorf("failed authenticating accesspoint with blob: %w", err)
		}
	default:
		panic("unknown credentials")
	}

	// authenticate with login5
	if err := s.login5.Login(ctx, &credentialspb.StoredCredential{
		Username: s.ap.Username(),
		Data:     s.ap.StoredCredentials(),
	}); err != nil {
		return nil, fmt.Errorf("failed authenticating with login5: %w", err)
	}

	// initialize spclient
	// Pass OAuth token function for Web API calls (has user scopes)
	// Login5 token is used for Spotify Connect (no user scopes)
	// Use OAuth token function when we have an access token or a persisted
	// refresh token (the access token gets refreshed on first use)
	var oauthFunc librespot.GetLogin5TokenFunc
	if s.oauthToken != "" || s.oauthRefresh != "" {
		oauthFunc = s.oauthTokenFunc()
	}
	if spAddr, err := s.resolver.GetSpclient(ctx); err != nil {
		return nil, fmt.Errorf("failed getting spclient from resolver: %w", err)
	} else if s.sp, err = spclient.NewSpclient(ctx, opts.Log, s.client, spAddr, s.login5.AccessToken(), oauthFunc, s.deviceId, s.clientToken); err != nil {
		return nil, fmt.Errorf("failed initializing spclient: %w", err)
	}

	// initialize dealer
	dealerAddr, err := s.resolver.GetDealer(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed getting dealer from resolver: %w", err)
	}
	s.dealer = dealer.NewDealer(opts.Log, s.client, dealerAddr, s.login5.AccessToken())

	return &s, nil
}

func (s *Session) Close() {
	s.dealer.Close()
	s.ap.Close()
}

// oauthTokenFunc returns a function that provides the OAuth access token.
// It handles token refresh using the refresh token when the access token expires.
func (s *Session) oauthTokenFunc() librespot.GetLogin5TokenFunc {
	return func(ctx context.Context, force bool) (string, error) {
		// snapshot the token state under the lock, never hold it across the
		// network refresh (the persist callback it triggers takes other locks)
		s.oauthMu.Lock()
		token := s.oauthToken
		hasRefresh := s.oauthRefresh != ""
		expiresAt := s.oauthExpiresAt
		s.oauthMu.Unlock()

		if !force && time.Now().Before(expiresAt) {
			return token, nil
		}

		remaining := time.Until(expiresAt)
		s.log.Infof("OAuth: token %s (refresh token available: %v)",
			func() string { if force { return "force refresh" } else { return "expired" } }(), hasRefresh)

		if !hasRefresh {
			if token != "" {
				s.log.Warnf("OAuth: no refresh token, using stale token (expires in %v)", remaining)
				return token, nil
			}
			return "", fmt.Errorf("OAuth: no refresh token and no valid access token available, re-authentication required")
		}
		if err := s.refreshOAuthToken(); err != nil {
			s.log.Errorf("OAuth: refresh failed: %v", err)
			if token != "" {
				s.log.Warnf("OAuth: falling back to stale token (expires in %v)", remaining)
				return token, nil
			}
			return "", fmt.Errorf("OAuth: refresh failed and no stale token available: %w", err)
		}
		s.oauthMu.Lock()
		token = s.oauthToken
		s.oauthMu.Unlock()
		return token, nil
	}
}

func (s *Session) refreshOAuthToken() error {
	s.oauthMu.Lock()
	refreshToken := s.oauthRefresh
	s.oauthMu.Unlock()
	if refreshToken == "" {
		return fmt.Errorf("OAuth: no refresh token available")
	}

	body := url.Values{
		"grant_type": {"refresh_token"},
		// Spotify answers 400 invalid_client when client_id is missing
		"client_id":     {librespot.ClientIdHex},
		"refresh_token": {refreshToken},
	}.Encode()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		s.tokenEndpoint, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed building OAuth refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	s.log.Infof("OAuth: refreshing access token")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("OAuth refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		s.log.Errorf("OAuth: refresh returned status %d: %s", resp.StatusCode, string(bodyBytes))
		return fmt.Errorf("OAuth refresh returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return fmt.Errorf("OAuth refresh decode failed: %w", err)
	}

	s.oauthMu.Lock()
	s.oauthToken = tok.AccessToken
	if tok.RefreshToken != "" {
		s.oauthRefresh = tok.RefreshToken
	}
	s.oauthExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	s.oauthMu.Unlock()
	s.log.Infof("OAuth: token refreshed successfully (expires in %ds)", tok.ExpiresIn)
	s.emitOAuthChanged()
	return nil
}

// emitOAuthChanged hands a snapshot of the current token pair to the
// persistence callback (nil-safe). It fires after the device flow completes
// and after every successful refresh so the pair can be written to app state.
func (s *Session) emitOAuthChanged() {
	if s.oauthChanged == nil {
		return
	}
	s.oauthMu.Lock()
	state := &librespot.OAuthState{
		AccessToken:  s.oauthToken,
		RefreshToken: s.oauthRefresh,
		ExpiresAt:    s.oauthExpiresAt.Unix(),
	}
	s.oauthMu.Unlock()
	s.oauthChanged(state)
}
