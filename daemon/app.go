package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/apresolve"
	"github.com/devgianlu/go-librespot/daemon/bluetooth"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"
	"github.com/devgianlu/go-librespot/session"
	"golang.org/x/exp/rand"
)

type App struct {
	log librespot.Logger
	cfg *Config

	stateStore StateStore

	client *http.Client

	resolver *apresolve.ApResolver

	deviceId    string
	deviceType  devicespb.DeviceType
	clientToken string
	state       *librespot.AppState

	server   ApiServer
	logoutCh chan *AppPlayer

	// auth state for /auth/status
	authMu       sync.RWMutex
	authRequired bool
	authURL      string
	// authKnown distinguishes the state so the frontend can branch correctly
	authKnown    bool

	bt *bluetooth.Manager

	retryNowCh chan struct{}

	// pre-network attempt sits in DNS resolution for 20-30s. parking here until network is up dodges that
	onlineMu sync.Mutex
	isOnline bool
	onlineCh chan struct{}

	closed bool
}

func parseDeviceType(val string) (devicespb.DeviceType, error) {
	valEnum, ok := devicespb.DeviceType_value[strings.ToUpper(val)]
	if !ok {
		return 0, fmt.Errorf("invalid device type: %s", val)
	}

	return devicespb.DeviceType(valEnum), nil
}

func New(opts *Options) (*App, error) {
	if opts == nil {
		return nil, errors.New("daemon: Options is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("daemon: Options.Logger is required")
	}
	if opts.Config == nil {
		return nil, errors.New("daemon: Options.Config is required")
	}
	if opts.StateStore == nil {
		return nil, errors.New("daemon: Options.StateStore is required")
	}

	app := &App{
		log:        opts.Logger,
		cfg:        opts.Config,
		stateStore: opts.StateStore,
		logoutCh:   make(chan *AppPlayer),
		client:     &http.Client{Timeout: 30 * time.Second},
		retryNowCh: make(chan struct{}, 1),
		onlineCh:   make(chan struct{}),
	}

	var err error
	app.deviceType, err = parseDeviceType(app.cfg.DeviceType)
	if err != nil {
		return nil, err
	}

	app.state, err = opts.StateStore.Load()
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}
	if app.state == nil {
		app.state = &librespot.AppState{}
	}

	app.resolver = apresolve.NewApResolver(app.log, app.client)

	if app.cfg.DeviceId != "" {
		app.deviceId = app.cfg.DeviceId
	} else if app.state.DeviceId != "" {
		app.deviceId = app.state.DeviceId
	} else {
		deviceIdBytes := make([]byte, 20)
		_, _ = rand.Read(deviceIdBytes)
		app.deviceId = hex.EncodeToString(deviceIdBytes)
		app.log.Infof("generated new device id: %s", app.deviceId)

		app.state.DeviceId = app.deviceId
		if err := app.persistState(); err != nil {
			return nil, err
		}
	}

	if app.cfg.ClientToken != "" {
		app.clientToken = app.cfg.ClientToken
	}

	if opts.APIServer != nil {
		app.server = opts.APIServer
	} else {
		app.server, _ = NewStubApiServer(app.log)
	}

	// /auth/status can answer before any AppPlayer exists
	app.server.SetAuthHandler(app.GetAuthState)

	// app owns reset because it touches BT manager + state store
	app.server.SetSystemHandler(app)

	// BT manager init, non-fatal on dev/test systems without BlueZ
	emit := func(eventType string, payload any) {
		app.server.Emit(&ApiEvent{
			Type: ApiEventType(eventType),
			Data: payload,
		})
	}
	if bm, err := bluetooth.NewManager(app.log, emit); err != nil {
		app.log.WithError(err).Warn("bluetooth: manager unavailable (continuing without)")
	} else {
		app.bt = bm
		app.server.SetBluetoothHandler(bm)

		// seed lastPanAddress from persisted state so the offline-retry loop can try the known device asap
		if app.state.LastBluetoothPanAddress != "" {
			bm.SeedLastPanAddress(app.state.LastBluetoothPanAddress)
		}

		// persist on change so next reboot has the address
		bm.SetLastPanAddressChangedHandler(func(addr string) {
			app.state.LastBluetoothPanAddress = addr
			if err := app.persistState(); err != nil {
				app.log.WithError(err).Warn("failed to persist last PAN address")
			}
		})
	}

	// network monitor emits network_status events + drives BT discoverability
	onNetTransition := func(online bool) {
		app.setOnlineState(online)

		if online {
			select {
			case app.retryNowCh <- struct{}{}:
			default:
			}
		}

		if app.bt == nil {
			return
		}
		if err := app.bt.SetDiscoverable(!online); err != nil {
			app.log.WithError(err).Debugf("bluetooth: failed to set discoverable=%v on network transition", !online)
		}
		// offline PAN retry loop runs if we're offline
		app.bt.SetOfflineRetry(!online)
	}
	startNetworkMonitor(app.log, app.server, onNetTransition)

	return app, nil
}

// SetAuthState records whether auth is required + the OAuth URL
// sets authKnown to true so the frontend can drop its initial loading state
func (app *App) SetAuthState(required bool, url string) {
	app.authMu.Lock()
	app.authRequired = required
	app.authURL = url
	app.authKnown = true
	app.authMu.Unlock()
}

// GetAuthState returns (required, url, known)
func (app *App) GetAuthState() (required bool, url string, known bool) {
	app.authMu.RLock()
	defer app.authMu.RUnlock()
	return app.authRequired, app.authURL, app.authKnown
}

// run starts the observer daemon
func (app *App) Run(ctx context.Context) error {
	// no RTC on the Car Thing
	if err := waitForClock(ctx, app.log); err != nil {
		// only ctx.Err can come back, timeout falls through to warn + continue
		return err
	}

	switch app.cfg.Credentials.Type {
	case "interactive":
		return app.runInteractive(ctx)
	case "spotify_token":
		return app.runSpotifyToken(ctx, app.cfg.Credentials.SpotifyToken.Username, app.cfg.Credentials.SpotifyToken.AccessToken)
	default:
		return fmt.Errorf("unknown or unsupported credentials type for observer mode: %s", app.cfg.Credentials.Type)
	}
}

// max wait for NTP before proceeding anyway, downstream TLS retries handle the rest
const clockMaxWait = 60 * time.Second

func waitForClock(ctx context.Context, log librespot.Logger) error {
	const minYear = 2025
	if time.Now().Year() >= minYear {
		return nil
	}
	log.Infof("waiting for clock to reach >= %d (currently %s, max wait %s)",
		minYear, time.Now().Format(time.RFC3339), clockMaxWait)

	deadline := time.NewTimer(clockMaxWait)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			log.Warnf("clock still bad after %s (now %s), proceeding anyway, TLS will retry until NTP catches up",
				clockMaxWait, time.Now().Format(time.RFC3339))
			return nil
		case <-ticker.C:
			if time.Now().Year() >= minYear {
				log.Infof("clock is sane (%s), proceeding to Spotify session", time.Now().Format(time.RFC3339))
				return nil
			}
		}
	}
}

func (app *App) Close() error {
	if app.closed {
		return nil
	}
	app.closed = true

	if app.server != nil {
		return app.server.Close()
	}
	return nil
}

func (app *App) persistState() error {
	if err := app.stateStore.Save(app.state); err != nil {
		return fmt.Errorf("persisting state: %w", err)
	}
	return nil
}

// PerformReset wipes user data (creds, last PAN address, BT bondings) and reboots
func (app *App) PerformReset() {
	app.log.Warn("system: performing factory reset")

	// BT bondings via BlueZ RemoveDevice (cleaner than rm'ing /var/lib/bluetooth)
	if app.bt != nil {
		devs, err := app.bt.GetDevices()
		if err != nil {
			app.log.WithError(err).Warn("system: failed to enumerate BT devices for removal")
		} else {
			for _, d := range devs {
				if !d.Paired {
					continue
				}
				if err := app.bt.RemoveDevice(d.Address); err != nil {
					app.log.WithError(err).Warnf("system: failed to remove BT device %s", d.Address)
				} else {
					app.log.Infof("system: removed BT bonding for %s", d.Address)
				}
			}
		}
	}

	// clear creds + last PAN address
	app.state.Credentials.Username = ""
	app.state.Credentials.Data = nil
	app.state.LastBluetoothPanAddress = ""
	if err := app.persistState(); err != nil {
		app.log.WithError(err).Warn("system: failed to persist post-reset state")
	}

	// let disk writes flush + BlueZ finish propagating RemoveDevice
	time.Sleep(500 * time.Millisecond)

	app.log.Warn("system: rebooting")
	if err := exec.Command("/sbin/reboot").Run(); err != nil {
		app.log.WithError(err).Warn("system: /sbin/reboot failed, trying busybox reboot")
		if err := exec.Command("reboot").Run(); err != nil {
			app.log.WithError(err).Error("system: reboot command failed; user must power-cycle manually")
		}
	}
}

func (app *App) newAppPlayer(ctx context.Context, creds any) (_ *AppPlayer, err error) {
	appPlayer := &AppPlayer{
		app:             app,
		stop:            make(chan struct{}, 1),
		logout:          app.logoutCh,
		countryCode:     new(string),
		playbackReadyCh: make(chan struct{}),
		queueResolvedCh: make(chan struct{}, 1),
	}

	appPlayer.prefetchTimer = time.NewTimer(math.MaxInt64)
	appPlayer.prefetchTimer.Stop()

	if appPlayer.sess, err = session.NewSessionFromOptions(ctx, &session.Options{
		Log:         app.log,
		DeviceType:  app.deviceType,
		DeviceId:    app.deviceId,
		ClientToken: app.clientToken,
		Resolver:    app.resolver,
		Client:      app.client,
		AppState:    app.state,
		Credentials: creds,
		AuthURLCallback: func(url string) {
			app.SetAuthState(true, url)
		},
	}); err != nil {
		return nil, err
	}

	app.SetAuthState(false, "")
	appPlayer.initState()

	// observer mode

	return appPlayer, nil
}

func (app *App) runSpotifyToken(ctx context.Context, username, token string) error {
	return app.withCredentials(ctx, session.SpotifyTokenCredentials{Username: username, Token: token})
}

func (app *App) runInteractive(ctx context.Context) error {
	return app.withCredentials(ctx, session.InteractiveCredentials{})
}

// setOnlineState flips the waitOnline barrier
func (app *App) setOnlineState(online bool) {
	app.onlineMu.Lock()
	defer app.onlineMu.Unlock()

	if app.isOnline == online {
		return
	}
	app.isOnline = online
	if online {
		close(app.onlineCh)
		app.onlineCh = make(chan struct{})
	}
}

// waitOnline blocks until online, timeout, or ctx cancel
func (app *App) waitOnline(ctx context.Context, timeout time.Duration) error {
	app.onlineMu.Lock()
	if app.isOnline {
		app.onlineMu.Unlock()
		return nil
	}
	ch := app.onlineCh
	app.onlineMu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("waitOnline timed out")
	case <-ch:
		return nil
	}
}

// sessionRetryBackoff: 2s, 4s, 8s, 16s, 30s, 30s, ... capped at 30s
func sessionRetryBackoff(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * time.Second
	const cap = 30 * time.Second
	if d > cap {
		return cap
	}
	return d
}

// newAppPlayerWithRetry retries session creation with exponential backoff
func (app *App) newAppPlayerWithRetry(ctx context.Context, creds any) (*AppPlayer, error) {
	for attempt := 0; ; attempt++ {
		// park here until network is reachable so we don't burn a 30s DNS timeout
		if err := app.waitOnline(ctx, 60*time.Second); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			app.log.WithError(err).Debug("session retry: waitOnline gave up, attempting anyway")
		}

		appPlayer, err := app.newAppPlayer(ctx, creds)
		if err == nil {
			return appPlayer, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		backoff := sessionRetryBackoff(attempt)
		app.log.WithError(err).Warnf("session attempt %d failed; retrying in %s", attempt+1, backoff)

		select {
		case <-app.retryNowCh:
		default:
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-app.retryNowCh:
			app.log.Infof("session retry: network came online, retrying immediately (skipping %s backoff)", backoff)
		case <-time.After(backoff):
		}
	}
}

func (app *App) withCredentials(ctx context.Context, creds any) (err error) {
	if len(app.state.Credentials.Data) > 0 {
		appPlayer, err := app.newAppPlayerWithRetry(ctx, session.StoredCredentials{
			Username: app.state.Credentials.Username,
			Data:     app.state.Credentials.Data,
		})
		if err != nil {
			return err
		}

		appPlayer.Run(ctx, app.server.Receive())
		return nil
	}

	appPlayer, err := app.newAppPlayerWithRetry(ctx, creds)
	if err != nil {
		return err
	}

	app.state.Credentials.Username = appPlayer.sess.Username()
	app.state.Credentials.Data = appPlayer.sess.StoredCredentials()

	if err = app.persistState(); err != nil {
		return err
	}

	app.log.Debugf("stored credentials for %s", librespot.ObfuscateUsername(appPlayer.sess.Username()))
	appPlayer.Run(ctx, app.server.Receive())
	return nil
}
