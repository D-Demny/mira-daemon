package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// the persisted-query sha256 hashes rotate per web-player build, so any
// hardcoded hashes eventually stop working
// so instead of each hash needing some new firmware build we just scrape the current hashes

const (
	hashScrapeHome   = "https://open.spotify.com/"
	hashScrapeUA     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
	hashCacheVersion = 1

	maxHomeBytes     = 256 << 10 // 256 KiB
	maxBundleBytes   = 16 << 20  // 16 MiB
	maxChunkBytes    = 4 << 20   // 4 MiB
	maxChunkFetch    = 10        // route chunks fetched per scrape
	hashScrapeBudget = 24 << 20  // 24 MiB total bytes per run
)

var (
	reOpDef      = regexp.MustCompile(`"([A-Za-z][A-Za-z0-9_]*)","(?:query|mutation|subscription)","([0-9a-f]{64})"`)
	reMainBundle = regexp.MustCompile(`https?://[^\s"'<>]+/web-player\.[0-9a-f]+\.js`)
	rePublicPath = regexp.MustCompile(`\.p\s*=\s*"(https://[^"]+/)"`)
	reChunkHash  = regexp.MustCompile(`(\d+):"([0-9a-f]{8,40})"`)
	reChunkName  = regexp.MustCompile(`(\d+):"(xpui[\w-]+)"`)
	reHex64      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func is64Hex(s string) bool { return reHex64.MatchString(s) }

func countHex(m map[string]string) int {
	n := 0
	for _, v := range m {
		if is64Hex(v) {
			n++
		}
	}
	return n
}

var routeChunkKeywords = []string{"search", "profile", "user", "library", "playlist", "browse", "artist", "album", "track"}

type hashScraper struct {
	client *http.Client
	log    logger
	home   string
}

func newHashScraper(log logger) *hashScraper {
	return &hashScraper{
		// dedicated cookieless client
		client: &http.Client{Timeout: 15 * time.Second},
		log:    log,
		home:   hashScrapeHome,
	}
}

// get fetches url with a byte cap and the desktop user agent
func (sc *hashScraper) get(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", hashScrapeUA)
	resp, err := sc.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

// returns the current hashes for our target
func (sc *hashScraper) scrape(ctx context.Context, cachedBundle string) (found map[string]string, bundle string, err error) {
	home, err := sc.get(ctx, sc.home, maxHomeBytes)
	if err != nil {
		return nil, "", fmt.Errorf("homepage: %w", err)
	}
	mb := reMainBundle.Find(home)
	if mb == nil {
		return nil, "", errors.New("main bundle URL not found in homepage")
	}
	bundleURL := string(mb)
	bundle = bundleURL[strings.LastIndex(bundleURL, "/")+1:]
	if cachedBundle != "" && bundle == cachedBundle {
		return nil, bundle, nil // unchanged
	}

	main, err := sc.get(ctx, bundleURL, maxBundleBytes)
	if err != nil {
		return nil, "", fmt.Errorf("main bundle: %w", err)
	}
	found = extractTargetOps(main)
	if haveAllTargets(found) {
		return found, bundle, nil
	}

	// searchDesktop, userTopContent live in lazy route chunks
	pp := publicPathFrom(main, bundleURL)
	urls := routeChunkURLs(pp, main)
	spent := int64(len(home) + len(main))
	for i, u := range urls {
		if i >= maxChunkFetch || haveAllTargets(found) || spent >= hashScrapeBudget {
			break
		}
		body, gerr := sc.get(ctx, u, maxChunkBytes)
		if gerr != nil {
			continue
		}
		spent += int64(len(body))
		for op, h := range extractTargetOps(body) {
			if found[op] == "" {
				found[op] = h
			}
		}
	}
	return found, bundle, nil
}

// runs the triple regex and keeps ONLY what we care about
func extractTargetOps(js []byte) map[string]string {
	out := map[string]string{}
	for _, m := range reOpDef.FindAllSubmatch(js, -1) {
		op := string(m[1])
		if _, want := opHashDefaults[op]; !want {
			continue
		}
		if _, seen := out[op]; seen {
			continue
		}
		out[op] = string(m[2])
	}
	return out
}

func haveAllTargets(found map[string]string) bool {
	for op := range opHashDefaults {
		if !is64Hex(found[op]) {
			return false
		}
	}
	return true
}

func publicPathFrom(main []byte, bundleURL string) string {
	if m := rePublicPath.FindSubmatch(main); m != nil {
		return string(m[1])
	}
	return bundleURL[:strings.LastIndex(bundleURL, "/")+1]
}

func routeChunkURLs(publicPath string, main []byte) []string {
	names := map[string]string{}
	for _, m := range reChunkName.FindAllSubmatch(main, -1) {
		names[string(m[1])] = string(m[2])
	}
	hashes := map[string]string{}
	for _, m := range reChunkHash.FindAllSubmatch(main, -1) {
		id := string(m[1])
		if _, ok := hashes[id]; !ok {
			hashes[id] = string(m[2])
		}
	}
	type cand struct {
		url  string
		rank int
	}
	var cands []cand
	for id, name := range names {
		h := hashes[id]
		if h == "" {
			continue
		}
		rank := -1
		lname := strings.ToLower(name)
		for i, kw := range routeChunkKeywords {
			if strings.Contains(lname, kw) {
				rank = i
				break
			}
		}
		if rank < 0 {
			continue
		}
		cands = append(cands, cand{url: publicPath + name + "." + h + ".js", rank: rank})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank < cands[j].rank })
	urls := make([]string, len(cands))
	for i, c := range cands {
		urls[i] = c.url
	}
	return urls
}

type hashCacheFile struct {
	Version   int               `json:"version"`
	FetchedAt int64             `json:"fetched_at"`
	Bundle    string            `json:"bundle"`
	Current   map[string]string `json:"current"`
	Previous  map[string]string `json:"previous"`
}

func hashCachePath(dir string) string { return filepath.Join(dir, "pathfinder_hashes.json") }

func loadHashCacheFile(dir string, store *hashStore) (bundle string, fetchedAt int64) {
	b, err := os.ReadFile(hashCachePath(dir))
	if err != nil {
		return "", 0
	}
	var f hashCacheFile
	if json.Unmarshal(b, &f) != nil || f.Version != hashCacheVersion {
		return "", 0
	}
	store.seed(f.Current, f.Previous)
	return f.Bundle, f.FetchedAt
}

func saveHashCacheFile(dir, bundle string, fetchedAt int64, store *hashStore) error {
	cur, prev := store.snapshot()
	b, err := json.MarshalIndent(hashCacheFile{
		Version:   hashCacheVersion,
		FetchedAt: fetchedAt,
		Bundle:    bundle,
		Current:   cur,
		Previous:  prev,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return atomicWrite(hashCachePath(dir), b)
}

func (v *voiceService) loadHashCache() {
	bundle, fetchedAt := loadHashCacheFile(v.cacheDir(), v.app.hashes)
	v.hashMu.Lock()
	v.hashBundle, v.hashFetchedAt = bundle, fetchedAt
	v.hashMu.Unlock()
}

func (v *voiceService) runHashRotate() {
	if err := v.app.waitOnline(v.ctx, 30*time.Minute); err != nil {
		return
	}
	select {
	case <-time.After(15 * time.Second):
	case <-v.ctx.Done():
		return
	}

	v.hashMu.Lock()
	fetchedAt := v.hashFetchedAt
	v.hashMu.Unlock()
	if fetchedAt != 0 && time.Since(time.Unix(fetchedAt, 0)) < 24*time.Hour && v.app.hashes.hasAllTargets() {
		v.app.log.Debugf("voice: pathfinder hashes fresh (scraped <24h ago), skipping startup scrape")
	} else {
		v.rotateOnce()
	}

	t := time.NewTicker(7 * 24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-v.ctx.Done():
			return
		case <-t.C:
			v.rotateOnce()
		}
	}
}

func (v *voiceService) triggerHashRotate() {
	if t := v.lastRotate.Load(); t != 0 && time.Since(time.Unix(0, t)) < 5*time.Minute {
		return
	}
	go v.rotateOnce()
}

func (v *voiceService) rotateOnce() {
	if !v.hashRotateGate.CompareAndSwap(false, true) {
		return
	}
	defer v.hashRotateGate.Store(false)
	v.lastRotate.Store(time.Now().UnixNano())

	v.hashMu.Lock()
	cachedBundle := v.hashBundle
	v.hashMu.Unlock()

	ctx, cancel := context.WithTimeout(v.ctx, 60*time.Second)
	defer cancel()

	found, bundle, err := newHashScraper(v.app.log).scrape(ctx, cachedBundle)
	if err != nil {
		v.app.log.Warnf("voice: pathfinder hash scrape failed: %v", err)
		return
	}
	if found == nil {
		return // bundle unchanged since last scrape
	}
	if !haveAllTargets(found) {
		v.app.log.Warnf("voice: pathfinder hash scrape incomplete (%d/%d ops), keeping current table",
			countHex(found), len(opHashDefaults))
		return
	}

	changed := v.app.hashes.adopt(found)
	now := time.Now()
	v.hashMu.Lock()
	v.hashBundle, v.hashFetchedAt = bundle, now.Unix()
	v.hashMu.Unlock()
	if err := saveHashCacheFile(v.cacheDir(), bundle, now.Unix(), v.app.hashes); err != nil {
		v.app.log.Warnf("voice: failed to persist pathfinder hashes: %v", err)
	}
	if changed > 0 {
		v.app.log.Infof("voice: pathfinder hashes rotated (%d changed of %d, build %s)",
			changed, len(opHashDefaults), bundle)
	} else {
		v.app.log.Debugf("voice: pathfinder hashes checked (build %s), none changed", bundle)
	}
}
