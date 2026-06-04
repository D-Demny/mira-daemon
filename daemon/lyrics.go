package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

type LyricsLine struct {
	StartTimeMs string `json:"startTimeMs"`
	Words       string `json:"words"`
}

type LyricsResult struct {
	SyncType string       `json:"syncType"` // LINE_SYNCED or UNSYNCED
	Lines    []LyricsLine `json:"lines"`
}

// LyricsProvider fetches from a primary source then falls back to LRCLIB.
type LyricsProvider struct {
	log    librespot.Logger
	client *http.Client
	// separate client with cookie jar so session cookies persist across calls
	lpClient *http.Client

	// primary source config
	lpTokenURL    string
	lpSubtitleURL string
	lpAppID       string
	lpOrigin      string
	lpReferer     string
	lpSubtitleFmt string
	lrclibURL     string

	mu    sync.RWMutex
	cache map[string]*LyricsResult // keyed by trackId

	lpMu    sync.Mutex
	lpToken string
	lpExp   time.Time
	// skip primary until this time if we get rate limited
	lpCooldownUntil time.Time
}

// how long to skip the primary source after a rate-limit before retrying
const primaryCooldown = 60 * time.Second

func NewLyricsProvider(logger librespot.Logger) *LyricsProvider {
	jar, jarErr := cookiejar.New(nil)
	if jarErr != nil {
		logger.Warnf("lyrics: cookie jar init failed (primary path may degrade): %v", jarErr)
	}
	return &LyricsProvider{
		log: logger,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		lpClient: &http.Client{
			Timeout: 10 * time.Second,
			Jar:     jar,
		},
		lpTokenURL:    os.Getenv("THING_LP_TOKEN_URL"),
		lpSubtitleURL: os.Getenv("THING_LP_SUBTITLE_URL"),
		lpAppID:       os.Getenv("THING_LP_APP_ID"),
		lpOrigin:      os.Getenv("THING_LP_ORIGIN"),
		lpReferer:     os.Getenv("THING_LP_REFERER"),
		lpSubtitleFmt: os.Getenv("THING_LP_SUBTITLE_FORMAT"),
		lrclibURL:     lrclibURL,
		cache:         make(map[string]*LyricsResult),
	}
}

// ErrNoLyrics maps to 404 in the HTTP layer (normal for tracks w no lyrics)
var ErrNoLyrics = errors.New("no lyrics available")

// FetchLyrics tries primary then LRCLIB. trackId is the cache key.
func (lp *LyricsProvider) FetchLyrics(ctx context.Context, trackId, trackName, artistName, albumName string, durationMs int) (*LyricsResult, error) {
	if trackName == "" {
		return nil, fmt.Errorf("track name is required")
	}

	lp.mu.RLock()
	if cached, ok := lp.cache[trackId]; ok {
		lp.mu.RUnlock()
		return cached, nil
	}
	lp.mu.RUnlock()

	result, err := lp.fetchPrimary(ctx, trackName, artistName, albumName, durationMs)
	if err != nil {
		lp.log.Debugf("lyrics: primary failed, trying lrclib: %v", err)
	}

	if result == nil {
		result, err = lp.fetchLRCLIB(ctx, trackName, artistName, durationMs)
		if err != nil {
			lp.log.Debugf("lyrics: lrclib also failed: %v", err)
			return nil, ErrNoLyrics
		}
	}

	if result == nil {
		return nil, ErrNoLyrics
	}

	lp.mu.Lock()
	lp.cache[trackId] = result
	if len(lp.cache) > 200 {
		lp.evictOldestLocked()
	}
	lp.mu.Unlock()

	lp.log.Debugf("lyrics found for %q by %q (%s, %d lines)", trackName, artistName, result.SyncType, len(result.Lines))
	return result, nil
}

func (lp *LyricsProvider) evictOldestLocked() {
	// drop half when over the limit
	count := 0
	for k := range lp.cache {
		if count >= len(lp.cache)/2 {
			break
		}
		delete(lp.cache, k)
		count++
	}
}

func (lp *LyricsProvider) ClearCache() {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	lp.cache = make(map[string]*LyricsResult)
}

// Primary lyrics source

func (lp *LyricsProvider) addPrimaryHeaders(req *http.Request) {
	// browser-mimicking headers, origin/referer from env so they don't leak in source
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if lp.lpOrigin != "" {
		req.Header.Set("Origin", lp.lpOrigin)
	}
	if lp.lpReferer != "" {
		req.Header.Set("Referer", lp.lpReferer)
	}
}

func (lp *LyricsProvider) getPrimaryToken(ctx context.Context) (string, error) {
	lp.lpMu.Lock()
	defer lp.lpMu.Unlock()

	// tokens last ~10 min, refresh at 8
	if lp.lpToken != "" && time.Now().Before(lp.lpExp) {
		return lp.lpToken, nil
	}

	params := url.Values{
		"app_id": {lp.lpAppID},
	}
	reqURL := lp.lpTokenURL + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating token request: %w", err)
	}
	lp.addPrimaryHeaders(req)

	resp, err := lp.lpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading token response: %w", err)
	}

	var tokenResp struct {
		Message struct {
			Header struct {
				StatusCode int `json:"status_code"`
			} `json:"header"`
			Body struct {
				UserToken string `json:"user_token"`
			} `json:"body"`
		} `json:"message"`
	}

	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}

	if tokenResp.Message.Header.StatusCode != 200 || tokenResp.Message.Body.UserToken == "" {
		return "", fmt.Errorf("primary returned status %d or empty token", tokenResp.Message.Header.StatusCode)
	}

	lp.lpToken = tokenResp.Message.Body.UserToken
	lp.lpExp = time.Now().Add(8 * time.Minute)

	lp.log.Debugf("lyrics: acquired new primary token")
	return lp.lpToken, nil
}

func (lp *LyricsProvider) invalidatePrimaryToken() {
	lp.lpMu.Lock()
	defer lp.lpMu.Unlock()
	lp.lpToken = ""
	lp.lpExp = time.Time{}
	// back off so we don't hammer Musixmatch / its token endpoint while rate-limited
	lp.lpCooldownUntil = time.Now().Add(primaryCooldown)
}

func (lp *LyricsProvider) fetchPrimary(ctx context.Context, trackName, artistName, albumName string, durationMs int) (*LyricsResult, error) {
	if lp.lpTokenURL == "" || lp.lpSubtitleURL == "" {
		return nil, errors.New("primary disabled (no env config)")
	}

	lp.lpMu.Lock()
	cooling := time.Now().Before(lp.lpCooldownUntil)
	lp.lpMu.Unlock()
	if cooling {
		return nil, errors.New("primary cooling down after auth/rate-limit error")
	}

	token, err := lp.getPrimaryToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting primary token: %w", err)
	}

	params := url.Values{
		"format":            {"json"},
		"namespace":         {"lyrics_richsynched"},
		"subtitle_format":   {lp.lpSubtitleFmt},
		"app_id":            {lp.lpAppID},
		"usertoken":         {token},
		"q_track":           {trackName},
		"q_artist":          {artistName},
		"q_artists":         {artistName},
		"track_spotify_id":  {""},
		"f_subtitle_length": {""},
	}
	if albumName != "" {
		params.Set("q_album", albumName)
	}
	if durationMs > 0 {
		params.Set("q_duration", strconv.Itoa(durationMs/1000))
	}

	reqURL := lp.lpSubtitleURL + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating subtitle request: %w", err)
	}
	lp.addPrimaryHeaders(req)

	resp, err := lp.lpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("subtitle request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading subtitle response: %w", err)
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		lp.invalidatePrimaryToken()
		return nil, fmt.Errorf("primary auth error (status %d), token invalidated", resp.StatusCode)
	}

	return lp.parsePrimaryResponse(body)
}

func (lp *LyricsProvider) parsePrimaryResponse(body []byte) (*LyricsResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	msgRaw, ok := raw["message"]
	if !ok {
		return nil, fmt.Errorf("no message in response")
	}

	var msg struct {
		Header struct {
			StatusCode int `json:"status_code"`
		} `json:"header"`
		Body struct {
			MacroCalls map[string]json.RawMessage `json:"macro_calls"`
		} `json:"body"`
	}
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return nil, fmt.Errorf("parsing message: %w", err)
	}

	if msg.Header.StatusCode != 200 {
		// Musixmatch signals a dead/rate-limited usertoken via the in-body
		// status_code (HTTP stays 200), so the HTTP-level 401 check above never
		// fires. Invalidate here too, or we reuse the dead token forever.
		if msg.Header.StatusCode == 401 || msg.Header.StatusCode == 403 {
			lp.invalidatePrimaryToken()
		}
		return nil, fmt.Errorf("primary status %d", msg.Header.StatusCode)
	}

	// try synced first
	if subtitlesRaw, ok := msg.Body.MacroCalls["track.subtitles.get"]; ok {
		result, err := lp.parsePrimarySubtitles(subtitlesRaw)
		if err == nil && result != nil {
			return result, nil
		}
		lp.log.Debugf("lyrics: synced subtitles parse failed, trying plain lyrics: %v", err)
	}

	if lyricsRaw, ok := msg.Body.MacroCalls["track.lyrics.get"]; ok {
		result, err := lp.parsePrimaryPlain(lyricsRaw)
		if err == nil && result != nil {
			return result, nil
		}
	}

	return nil, fmt.Errorf("no lyrics data in primary response")
}

func (lp *LyricsProvider) parsePrimarySubtitles(raw json.RawMessage) (*LyricsResult, error) {
	// body comes as either an object or an empty array
	var envelope struct {
		Message struct {
			Header struct {
				StatusCode int `json:"status_code"`
			} `json:"header"`
			Body json.RawMessage `json:"body"`
		} `json:"message"`
	}

	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("parsing subtitle envelope: %w", err)
	}

	if envelope.Message.Header.StatusCode != 200 {
		return nil, fmt.Errorf("subtitle status %d", envelope.Message.Header.StatusCode)
	}

	body := bytes.TrimSpace(envelope.Message.Body)
	if len(body) == 0 || body[0] != '{' {
		// "no synced lyrics" sentinel, fall through to plain or LRCLIB
		return nil, fmt.Errorf("no synced subtitles")
	}

	var subBody struct {
		SubtitleList []struct {
			Subtitle struct {
				SubtitleBody string `json:"subtitle_body"`
			} `json:"subtitle"`
		} `json:"subtitle_list"`
	}
	if err := json.Unmarshal(envelope.Message.Body, &subBody); err != nil {
		return nil, fmt.Errorf("parsing subtitle body envelope: %w", err)
	}

	if len(subBody.SubtitleList) == 0 {
		return nil, fmt.Errorf("empty subtitle list")
	}

	subtitleBody := subBody.SubtitleList[0].Subtitle.SubtitleBody
	if subtitleBody == "" {
		return nil, fmt.Errorf("empty subtitle body")
	}

	var lpLines []struct {
		Text string `json:"text"`
		Time struct {
			Total float64 `json:"total"`
		} `json:"time"`
	}

	if err := json.Unmarshal([]byte(subtitleBody), &lpLines); err != nil {
		return nil, fmt.Errorf("parsing subtitle body: %w", err)
	}

	if len(lpLines) == 0 {
		return nil, fmt.Errorf("no lines in subtitle body")
	}

	lines := make([]LyricsLine, 0, len(lpLines))
	for _, ml := range lpLines {
		ms := int(ml.Time.Total * 1000)
		lines = append(lines, LyricsLine{
			StartTimeMs: strconv.Itoa(ms),
			Words:       ml.Text,
		})
	}

	return &LyricsResult{
		SyncType: "LINE_SYNCED",
		Lines:    lines,
	}, nil
}

func (lp *LyricsProvider) parsePrimaryPlain(raw json.RawMessage) (*LyricsResult, error) {
	var lyr struct {
		Message struct {
			Header struct {
				StatusCode int `json:"status_code"`
			} `json:"header"`
			Body struct {
				Lyrics struct {
					LyricsBody string `json:"lyrics_body"`
				} `json:"lyrics"`
			} `json:"body"`
		} `json:"message"`
	}

	if err := json.Unmarshal(raw, &lyr); err != nil {
		return nil, fmt.Errorf("parsing lyrics: %w", err)
	}

	if lyr.Message.Header.StatusCode != 200 || lyr.Message.Body.Lyrics.LyricsBody == "" {
		return nil, fmt.Errorf("no plain lyrics")
	}

	return plainTextToResult(lyr.Message.Body.Lyrics.LyricsBody), nil
}

// LRCLIB

const lrclibURL = "https://lrclib.net/api/get"

func (lp *LyricsProvider) fetchLRCLIB(ctx context.Context, trackName, artistName string, durationMs int) (*LyricsResult, error) {
	params := url.Values{
		"track_name":  {trackName},
		"artist_name": {artistName},
	}
	if durationMs > 0 {
		params.Set("duration", strconv.Itoa(durationMs/1000))
	}

	reqURL := lp.lrclibURL + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating lrclib request: %w", err)
	}
	req.Header.Set("User-Agent", "go-librespot-observer/1.0")

	resp, err := lp.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lrclib request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("lrclib: no lyrics found")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading lrclib response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("lrclib status %d: %s", resp.StatusCode, string(body))
	}

	var lrcResp struct {
		Instrumental bool   `json:"instrumental"`
		SyncedLyrics string `json:"syncedLyrics"`
		PlainLyrics  string `json:"plainLyrics"`
	}

	if err := json.Unmarshal(body, &lrcResp); err != nil {
		return nil, fmt.Errorf("parsing lrclib response: %w", err)
	}

	if lrcResp.Instrumental {
		return &LyricsResult{
			SyncType: "UNSYNCED",
			Lines:    []LyricsLine{{StartTimeMs: "0", Words: "♪ Instrumental ♪"}},
		}, nil
	}

	if lrcResp.SyncedLyrics != "" {
		result, err := parseLRC(lrcResp.SyncedLyrics)
		if err == nil && len(result.Lines) > 0 {

			return result, nil
		}
	}

	if lrcResp.PlainLyrics != "" {
		return plainTextToResult(lrcResp.PlainLyrics), nil
	}

	return nil, fmt.Errorf("lrclib: empty lyrics")
}

// lyric parser

// matches lines like [03:20.31] some text
var lrcLineRegex = regexp.MustCompile(`^\[(\d{2}):(\d{2})\.(\d{2,3})\]\s?(.*)$`)

func parseLRC(lrc string) (*LyricsResult, error) {
	rawLines := strings.Split(lrc, "\n")
	lines := make([]LyricsLine, 0, len(rawLines))

	for _, raw := range rawLines {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		matches := lrcLineRegex.FindStringSubmatch(raw)
		if matches == nil {
			continue
		}

		minutes, _ := strconv.Atoi(matches[1])
		seconds, _ := strconv.Atoi(matches[2])

		// support both 2-digit and 3-digit fractions
		fracStr := matches[3]
		frac, _ := strconv.Atoi(fracStr)
		if len(fracStr) == 2 {
			frac *= 10
		}

		ms := minutes*60*1000 + seconds*1000 + frac
		text := matches[4]

		lines = append(lines, LyricsLine{
			StartTimeMs: strconv.Itoa(ms),
			Words:       text,
		})
	}

	if len(lines) == 0 {
		return nil, fmt.Errorf("no valid LRC lines found")
	}

	return &LyricsResult{
		SyncType: "LINE_SYNCED",
		Lines:    lines,
	}, nil
}

// Helpers

func plainTextToResult(text string) *LyricsResult {
	rawLines := strings.Split(text, "\n")
	lines := make([]LyricsLine, 0, len(rawLines))

	for _, line := range rawLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, LyricsLine{
			StartTimeMs: "0",
			Words:       line,
		})
	}

	if len(lines) == 0 {
		return nil
	}

	return &LyricsResult{
		SyncType: "UNSYNCED",
		Lines:    lines,
	}
}
