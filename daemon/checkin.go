package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
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

func versionLess(a, b string) bool {
	pa, oka := parseVersionParts(a)
	pb, okb := parseVersionParts(b)
	if !oka || !okb {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseVersionParts(v string) ([3]int, bool) {
	var out [3]int
	if v == "" {
		return out, false
	}
	parts := strings.SplitN(v, ".", 3)
	for i, p := range parts {
		p, _, _ = strings.Cut(p, "-")
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
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

func (app *App) latestVersion() string {
	app.state.Lock()
	defer app.state.Unlock()
	return app.state.LatestVersion
}

func (app *App) updateMandatory() bool {
	app.state.Lock()
	defer app.state.Unlock()
	return app.state.UpdateMandatory
}

func (app *App) latestHighlights() []string {
	app.state.Lock()
	defer app.state.Unlock()
	out := make([]string, len(app.state.LatestHighlights))
	copy(out, app.state.LatestHighlights)
	return out
}

func (app *App) updateAvailable() bool {
	latest := app.latestVersion()
	if latest == "" {
		return false
	}
	return versionLess(normalizeVersion(firmwareVersion()), normalizeVersion(latest))
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
		UtcOffsetMin     *int     `json:"utc_offset_min"`
		LatestVersion    *string  `json:"latest_version"`
		LatestHighlights []string `json:"latest_highlights"`
		UpdateMandatory  *bool    `json:"update_mandatory"`
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
	if out.LatestVersion != nil && *out.LatestVersion != "" && app.state.LatestVersion != *out.LatestVersion {
		app.state.LatestVersion = *out.LatestVersion
		changed = true
	}
	if len(out.LatestHighlights) > 0 &&
		strings.Join(out.LatestHighlights, "\n") != strings.Join(app.state.LatestHighlights, "\n") {
		app.state.LatestHighlights = out.LatestHighlights
		changed = true
	}
	mandatory := out.UpdateMandatory != nil && *out.UpdateMandatory
	if app.state.UpdateMandatory != mandatory {
		app.state.UpdateMandatory = mandatory
		changed = true
	}
	app.state.Unlock()

	if changed {
		if err := app.persistState(); err != nil {
			app.log.WithError(err).Warn("checkin: failed to persist state")
		}
		app.log.Infof("checkin: utc_offset_min=%d latest_version=%s", off, app.latestVersion())
	}
	return nil
}
