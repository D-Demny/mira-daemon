package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	checkinInterval       = 6 * time.Hour
	checkinRetryInterval  = 2 * time.Minute
	checkinRequestTimeout = 10 * time.Second
	checkinJitter         = 30 * time.Minute
)

func checkinWait() time.Duration {
	return checkinInterval - checkinJitter + rand.N(2*checkinJitter)
}

type checkinTracker struct {
	mu          sync.Mutex
	lastSuccess string
	lastError   string
}

func (t *checkinTracker) noteSuccess(at time.Time) {
	t.mu.Lock()
	t.lastSuccess = at.UTC().Format(time.RFC3339)
	t.mu.Unlock()
}

func (t *checkinTracker) noteError(at time.Time, err error) {
	t.mu.Lock()
	t.lastError = fmt.Sprintf("%s: %v", at.UTC().Format(time.RFC3339), err)
	t.mu.Unlock()
}

func (t *checkinTracker) LastSuccess() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastSuccess
}

func (t *checkinTracker) LastError() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastError
}

func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

func (app *App) utcOffsetMin() *int {
	app.state.Lock()
	defer app.state.Unlock()
	if app.state.UtcOffsetMin == nil {
		return nil
	}
	v := *app.state.UtcOffsetMin
	return &v
}

func (app *App) hasCheckedInEver() bool {
	return app.utcOffsetMin() != nil
}

func (app *App) startCheckin() {
	if !app.cfg.Checkin || app.cfg.CheckinURL == "" {
		app.log.Debug("checkin: disabled by config")
		return
	}
	go app.runCheckinLoop(normalizeVersion(firmwareVersion()))
}

func (app *App) runCheckinLoop(version string) {
	ctx := context.Background()
	for {
		for app.waitOnline(ctx, time.Hour) != nil {
		}

		wait := checkinWait()
		if err := app.doCheckin(ctx, version); err != nil {
			app.checkinStatus.noteError(time.Now(), err)
			app.log.WithError(err).Debug("checkin: request failed")
			if !app.hasCheckedInEver() {
				wait = checkinRetryInterval
			}
		} else {
			app.checkinStatus.noteSuccess(time.Now())
		}

		time.Sleep(wait)
	}
}

func (app *App) doCheckin(ctx context.Context, version string) error {
	q := url.Values{}
	q.Set("version", version)

	ctx, cancel := context.WithTimeout(ctx, checkinRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(app.cfg.CheckinURL, "/")+"/v1/checkin?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := app.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
	if err != nil {
		return err
	}

	var out struct {
		UtcOffsetMin *int `json:"utc_offset_min"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("bad response: %w", err)
	}
	if out.UtcOffsetMin == nil {
		return fmt.Errorf("response missing utc_offset_min")
	}

	app.state.Lock()
	changed := app.state.UtcOffsetMin == nil || *app.state.UtcOffsetMin != *out.UtcOffsetMin
	off := *out.UtcOffsetMin
	app.state.UtcOffsetMin = &off
	app.state.Unlock()

	if changed {
		if err := app.persistState(); err != nil {
			app.log.WithError(err).Warn("checkin: failed to persist state")
		}
		app.log.Infof("checkin: utc_offset_min=%d", off)
	}
	return nil
}
