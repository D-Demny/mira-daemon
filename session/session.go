package session

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
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

	resolver *apresolve.ApResolver
	login5   *login5.Login5

	ap     *ap.Accesspoint
	sp     *spclient.Spclient
	dealer *dealer.Dealer

	// oauthToken is the OAuth access token from the device flow.
	// It has user scopes (playlist-read, etc.) and is used for Web API calls.
	// Unlike the Login5 token (Spotify Connect only), it works with api.spotify.com.
	oauthToken librespot.GetLogin5TokenFunc
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
	case InteractiveCredentials:
		// device flow, user authorizes via the QR URL we send to AuthURLCallback.
		// username left empty, AP fills it via APWelcome from the token itself
		oauthAccessToken, err := runDeviceAuthFlow(ctx, opts.Log, s.client, opts.AuthURLCallback)
		if err != nil {
			return nil, fmt.Errorf("device authorization flow failed: %w", err)
		}

		if err := s.ap.ConnectSpotifyToken(ctx, "", oauthAccessToken); err != nil {
			return nil, fmt.Errorf("failed authenticating accesspoint interactively: %w", err)
		}

		// Store OAuth token for Web API calls (has user scopes like playlist-read)
		// instead of Login5 token (Spotify Connect only, no user scopes)
		s.oauthToken = func(ctx context.Context, force bool) (string, error) {
			return oauthAccessToken, nil
		}
	case SpotifyTokenCredentials:
		if err := s.ap.ConnectSpotifyToken(ctx, creds.Username, creds.Token); err != nil {
			return nil, fmt.Errorf("failed authenticating accesspoint with username and spotify token: %w", err)
		}
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
	// Pass OAuth token for Web API calls (has user scopes)
	// Login5 token is used for Spotify Connect (no user scopes)
	if spAddr, err := s.resolver.GetSpclient(ctx); err != nil {
		return nil, fmt.Errorf("failed getting spclient from resolver: %w", err)
	} else if s.sp, err = spclient.NewSpclient(ctx, opts.Log, s.client, spAddr, s.login5.AccessToken(), s.oauthToken, s.deviceId, s.clientToken); err != nil {
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
