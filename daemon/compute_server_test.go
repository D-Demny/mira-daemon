package daemon

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// End-to-end tests for the Pi compute server (epic 10 follow-up): the REAL
// scripts/compute-server.js is exec'd under node (a temp copy per test) and
// served a fake CDN - an httptest server that counts requests and answers
// with embedded test images (solid red 64x64 JPEG + red PNG). Covered: the
// T2 route contract (capabilities/CORS/OPTIONS), the real 160-resize
// (dimensions via a dep-free SOF byte parse in a node snippet), color
// extraction, the disk cache (hit + mtime eviction), the in-flight dedup,
// the documented degradation modes (no jpeg-js, PNG source) and the error
// paths (400/404/502). Skipped when node (or npm for the codec tests) is
// not installed.

// testRedJPEGB64: solid red 64x64 JPEG (q90, generated with jpeg-js),
// embedded so the tests carry no external assets.
const testRedJPEGB64 = `
/9j/4AAQSkZJRgABAQAAAQABAAD/2wCEAAMCAgMCAgMDAwMEAwMEBQgFBQQEBQoHBwYIDAoM
DAsKCwsNDhIQDQ4RDgsLEBYQERMUFRUVDA8XGBYUGBIUFRQBAwQEBQQFCQUFCRQNCw0UFBQU
FBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFP/AABEIAEAA
QAMBEQACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMF
BQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYn
KCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SV
lpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz
9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncA
AQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3
ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqi
o6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/
2gAMAwEAAhEDEQA/APnSvww/1TCgAoAKACgAoAKACgAoAKACgAoAKACgAoAKACgAoAKACgAo
AKACgAoAKACgAoAKACgAoAKACgAoAKACgAoAKACgAoAKACgAoAKACgAoAKACgAoAKACgAoAK
ACgAoAKACgAoAKACgAoAKACgAoAKAP8A/9k=
`

var testRedJPEG = func() []byte {
	b, err := base64.StdEncoding.DecodeString(testRedJPEGB64)
	if err != nil {
		panic("embedded red JPEG is not valid base64: " + err.Error())
	}
	return b
}()

// redPNGBytes builds a solid red 16x16 PNG with the stdlib encoder (the
// JPEG cannot be produced by the stdlib, the PNG can - no asset needed).
func redPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding the red PNG: %v", err)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------- fake CDN

type fakeCDN struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   map[string]int32
	redJPG []byte
	redPNG []byte
}

func newFakeCDN(t *testing.T, redJPG, redPNG []byte) *fakeCDN {
	t.Helper()
	c := &fakeCDN{redJPG: redJPG, redPNG: redPNG, hits: map[string]int32{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.hits[r.URL.Path]++
		c.mu.Unlock()
		switch r.URL.Path {
		case "/img/fail.jpg":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "upstream down")
			return
		case "/img/slow.jpg":
			time.Sleep(300 * time.Millisecond)
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(c.redJPG)
			return
		case "/img/art.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(c.redPNG)
			return
		default:
			// everything else (incl. the eviction test urls) serves the red JPEG
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(c.redJPG)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeCDN) hit(path string) int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits[path]
}

func (c *fakeCDN) cdnURL(path string) string {
	return c.srv.URL + path
}

// ---------------------------------------------------------------- service start

// srvLog is a mutex-guarded bytes.Buffer (the child process writes to it
// while the test may read it on failure).
type srvLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *srvLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *srvLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type computeService struct {
	cmd  *exec.Cmd
	log  *srvLog
	base string
}

// startComputeServer execs the REAL compute-server.js (prepareComputeDir
// must have copied it into dir) under node with the given extra env vars,
// waits for the capabilities endpoint to answer and kills the process in a
// cleanup. MIRA_PI_PORT and MIRA_CACHE_DIR are always pinned to test
// values (free port, <dir>/cache).
func startComputeServer(t *testing.T, dir string, extraEnv map[string]string) *computeService {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed - skipping the compute-server end-to-end tests")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocating a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	s := &computeService{log: &srvLog{}}
	s.cmd = exec.Command(nodeBin, filepath.Join(dir, "compute-server.js"))
	s.cmd.Dir = dir
	env := append(os.Environ(),
		"MIRA_PI_PORT="+strconv.Itoa(port),
		"MIRA_CACHE_DIR="+filepath.Join(dir, "cache"),
	)
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	s.cmd.Env = env
	s.cmd.Stdout = s.log
	s.cmd.Stderr = s.log
	if err := s.cmd.Start(); err != nil {
		t.Fatalf("starting compute-server: %v", err)
	}
	s.base = "http://127.0.0.1:" + strconv.Itoa(port)

	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(s.base + "/api/v1/capabilities")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			s.stop(t)
			t.Fatalf("compute-server did not come up within 10s:\n%s", s.log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { s.stop(t) })
	return s
}

func (s *computeService) stop(t *testing.T) {
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	if s.cmd != nil {
		s.cmd.Wait()
	}
	if t != nil {
		t.Logf("compute-server log:\n%s", s.log.String())
	}
}

// prepareComputeDir copies the real scripts/compute-server.js into a fresh
// temp dir; withCodec additionally runs `npm install --prefix <dir>
// jpeg-js` (pure JS, no native build step; one install per test dir).
func prepareComputeDir(t *testing.T, withCodec bool) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "scripts", "compute-server.js"))
	if err != nil {
		t.Fatalf("reading the real compute-server.js: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compute-server.js"), src, 0o644); err != nil {
		t.Fatalf("copying compute-server.js: %v", err)
	}
	if withCodec {
		if _, err := exec.LookPath("npm"); err != nil {
			t.Skip("npm is not installed - skipping the codec-dependent test")
		}
		cmd := exec.Command("npm", "install", "--prefix", dir, "jpeg-js", "--no-audit", "--no-fund", "--loglevel=error")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("npm install jpeg-js: %v\n%s", err, out)
		}
		entries, _ := os.ReadDir(filepath.Join(dir, "node_modules"))
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Logf("node_modules in %s: %v", dir, names)
	}
	return dir
}

// ---------------------------------------------------------------- http helpers

func doGetErr(u string) (int, http.Header, []byte, error) {
	resp, err := http.Get(u)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	return resp.StatusCode, resp.Header, b, nil
}

func doGet(t *testing.T, u string) (int, http.Header, []byte) {
	t.Helper()
	code, hdr, b, err := doGetErr(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	return code, hdr, b
}

// imgRequestPath builds a T2 contract path: the CDN URL is
// percent-encoded into the first segment (the service decodes it).
func imgRequestPath(cdnURL, suffix string) string {
	return "/img/" + url.QueryEscape(cdnURL) + "/" + suffix
}

// ---------------------------------------------------------------- SOF parse

// sofParseJS is a dependency-free JPEG SOF scanner: prints "width height"
// from the first SOF marker. Used because the test must verify the real
// dimensions of the produced JPEG without adding a Go image dependency
// (and it doubles as a structural check that the answer is a standalone
// well-formed JPEG, not a passthrough).
const sofParseJS = `
// node -e: extra args start at argv[1] (no script-name slot)
const fs = require("fs");
const b = fs.readFileSync(process.argv[1]);
if (b.length < 4 || b[0] !== 0xFF || b[1] !== 0xD8) { console.error("not a JPEG (missing SOI)"); process.exit(1); }
let i = 2;
while (i + 4 < b.length) {
  if (b[i] !== 0xFF) { console.error("bad marker at " + i); process.exit(1); }
  while (b[i] === 0xFF) i++;
  const m = b[i++];
  if (m >= 0xD0 && m <= 0xD7) continue;
  if (m === 0xD8 || m === 0xD9 || m === 0x01) continue;
  const len = (b[i] << 8) | b[i + 1];
  if ((m >= 0xC0 && m <= 0xC3) || (m >= 0xC5 && m <= 0xC7) || (m >= 0xC9 && m <= 0xCB)) {
    const h = (b[i + 3] << 8) | b[i + 4];
    const w = (b[i + 5] << 8) | b[i + 6];
    console.log(w + " " + h);
    process.exit(0);
  }
  i += len;
}
console.error("no SOF marker found");
process.exit(1);
`

func jpegDimensions(t *testing.T, file string) (int, int) {
	t.Helper()
	out, err := exec.Command("node", "-e", sofParseJS, file).CombinedOutput()
	if err != nil {
		t.Fatalf("SOF parse of %s: %v\n%s", file, err, out)
	}
	var w, h int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &w, &h); err != nil {
		t.Fatalf("parsing SOF output %q: %v", string(out), err)
	}
	return w, h
}

// ---------------------------------------------------------------- tests

func TestComputeServer_CapabilitiesShapeCORS(t *testing.T) {
	s := startComputeServer(t, prepareComputeDir(t, false), map[string]string{"MIRA_PI_MODEL": "Pi 4 Model B"})

	code, hdr, body := doGet(t, s.base+"/api/v1/capabilities")
	if code != http.StatusOK {
		t.Fatalf("capabilities status = %d, want 200 (body %s)", code, body)
	}
	if got := hdr.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("GET Access-Control-Allow-Origin = %q, want *", got)
	}
	var caps struct {
		Tier         string `json:"tier"`
		DiskCache    bool   `json:"disk_cache"`
		RemoteColors bool   `json:"remote_colors"`
		RemoteBlur   bool   `json:"remote_blur"`
		Host         string `json:"host"`
		Model        string `json:"model"`
	}
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatalf("capabilities body is not JSON: %v\n%s", err, body)
	}
	if caps.Tier != "compute" {
		t.Errorf("tier = %q, want compute (default tier)", caps.Tier)
	}
	if !caps.DiskCache {
		t.Error("disk_cache = false, want true")
	}
	if !caps.RemoteColors || !caps.RemoteBlur {
		t.Errorf("remote_colors/remote_blur = %v/%v, want true/true (degradation keeps them true)", caps.RemoteColors, caps.RemoteBlur)
	}
	if caps.Model != "Pi 4 Model B" {
		t.Errorf("model = %q, want the MIRA_PI_MODEL env value", caps.Model)
	}

	// OPTIONS preflight: 204 + the full CORS header set
	req, err := http.NewRequest(http.MethodOptions, s.base+"/api/v1/capabilities", nil)
	if err != nil {
		t.Fatalf("building the OPTIONS request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("OPTIONS Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); got != "GET, OPTIONS" {
		t.Errorf("OPTIONS Access-Control-Allow-Methods = %q, want GET, OPTIONS", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "*" {
		t.Errorf("OPTIONS Access-Control-Allow-Headers = %q, want *", got)
	}
}

func TestComputeServer_160Resize(t *testing.T) {
	cdn := newFakeCDN(t, testRedJPEG, redPNGBytes(t))
	s := startComputeServer(t, prepareComputeDir(t, true), nil)

	code, hdr, body := doGet(t, s.base+imgRequestPath(cdn.cdnURL("/img/red.jpg"), "160.jpg"))
	if code != http.StatusOK {
		t.Fatalf("160.jpg status = %d, want 200 (body %s)", code, body)
	}
	if got := hdr.Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", got)
	}
	if len(body) < 4 || body[0] != 0xFF || body[1] != 0xD8 {
		t.Fatalf("response is not a JPEG (SOI missing), %d bytes", len(body))
	}
	tmp := filepath.Join(t.TempDir(), "160.jpg")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		t.Fatalf("writing the response JPEG: %v", err)
	}
	w, h := jpegDimensions(t, tmp)
	if w != 160 || h != 160 {
		t.Errorf("response dimensions = %dx%d, want 160x160 (real cover-crop resize)", w, h)
	}
	if n := cdn.hit("/img/red.jpg"); n != 1 {
		t.Errorf("upstream hits = %d, want 1", n)
	}
}

func TestComputeServer_Colors(t *testing.T) {
	cdn := newFakeCDN(t, testRedJPEG, redPNGBytes(t))
	s := startComputeServer(t, prepareComputeDir(t, true), nil)

	code, hdr, body := doGet(t, s.base+imgRequestPath(cdn.cdnURL("/img/red.jpg"), "colors"))
	if code != http.StatusOK {
		t.Fatalf("colors status = %d, want 200 (body %s)", code, body)
	}
	if got := hdr.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var out struct {
		Dominant []int `json:"dominant"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("colors body is not JSON: %v\n%s", err, body)
	}
	if len(out.Dominant) != 3 {
		t.Fatalf("dominant = %v, want exactly 3 components", out.Dominant)
	}
	for i, v := range out.Dominant {
		if v < 0 || v > 255 {
			t.Errorf("dominant[%d] = %d, want 0-255", i, v)
		}
	}
	// solid red test image: the dominant must be red (high R, low G/B)
	if out.Dominant[0] < 180 {
		t.Errorf("dominant R = %d, want >= 180 (solid red source)", out.Dominant[0])
	}
	if out.Dominant[1] > 60 || out.Dominant[2] > 60 {
		t.Errorf("dominant G/B = %d/%d, want <= 60 each (solid red source)", out.Dominant[1], out.Dominant[2])
	}
}

func TestComputeServer_DiskCacheHit(t *testing.T) {
	cdn := newFakeCDN(t, testRedJPEG, redPNGBytes(t))
	dir := prepareComputeDir(t, true)
	s := startComputeServer(t, dir, nil)
	cdnURL := cdn.cdnURL("/img/red.jpg")

	code1, _, body1 := doGet(t, s.base+imgRequestPath(cdnURL, "160.jpg"))
	if code1 != http.StatusOK {
		t.Fatalf("first 160.jpg status = %d, want 200", code1)
	}
	hitsAfterFirst := cdn.hit("/img/red.jpg")

	code2, _, body2 := doGet(t, s.base+imgRequestPath(cdnURL, "160.jpg"))
	if code2 != http.StatusOK {
		t.Fatalf("second 160.jpg status = %d, want 200 (cache hit)", code2)
	}
	if hits := cdn.hit("/img/red.jpg"); hits != hitsAfterFirst {
		t.Errorf("upstream hits = %d after the cache hit, want %d (unchanged)", hits, hitsAfterFirst)
	}
	if !bytes.Equal(body1, body2) {
		t.Error("second response differs from the first (the cache must serve the stored file)")
	}

	sum := sha1.Sum([]byte(cdnURL))
	cacheFile := filepath.Join(dir, "cache", hex.EncodeToString(sum[:]), "160.jpg")
	if st, err := os.Stat(cacheFile); err != nil {
		t.Errorf("cache file %s missing: %v", cacheFile, err)
	} else if st.Size() == 0 {
		t.Error("cache file exists but is empty")
	}
}

func TestComputeServer_CacheEviction(t *testing.T) {
	cdn := newFakeCDN(t, testRedJPEG, redPNGBytes(t))
	dir := prepareComputeDir(t, true)
	s := startComputeServer(t, dir, map[string]string{"MIRA_CACHE_MAX_FILES": "3"})

	urls := make([]string, 4)
	for i := 0; i < 4; i++ {
		u := cdn.cdnURL("/evict-" + strconv.Itoa(i+1) + ".jpg")
		urls[i] = u
		code, _, body := doGet(t, s.base+imgRequestPath(u, "160.jpg"))
		if code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i+1, code)
		}
		if len(body) == 0 {
			t.Fatalf("request %d: empty body", i+1)
		}
		time.Sleep(20 * time.Millisecond) // distinct directory mtimes
	}

	entries, err := os.ReadDir(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("reading the cache dir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("cache dirs = %d, want 3 (cap)", len(entries))
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	oldest := sha1.Sum([]byte(urls[0]))
	if names[hex.EncodeToString(oldest[:])] {
		t.Errorf("the oldest entry (%s) was not evicted", urls[0])
	}
	for i := 1; i < 4; i++ {
		sum := sha1.Sum([]byte(urls[i]))
		if !names[hex.EncodeToString(sum[:])] {
			t.Errorf("entry %d (%s) missing, want kept", i+1, urls[i])
		}
	}
}

func TestComputeServer_InFlightDedup(t *testing.T) {
	cdn := newFakeCDN(t, testRedJPEG, redPNGBytes(t))
	s := startComputeServer(t, prepareComputeDir(t, true), nil)
	cdnURL := cdn.cdnURL("/img/slow.jpg") // upstream sleeps 300ms per hit

	var wg sync.WaitGroup
	codes := make([]int, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			codes[n], _, _, errs[n] = doGetErr(s.base + imgRequestPath(cdnURL, "160.jpg"))
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if errs[i] != nil {
			t.Fatalf("concurrent request %d: %v", i+1, errs[i])
		}
		if codes[i] != http.StatusOK {
			t.Errorf("concurrent request %d status = %d, want 200", i+1, codes[i])
		}
	}
	if hits := cdn.hit("/img/slow.jpg"); hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (concurrent requests must share one in-flight fetch)", hits)
	}
}

func TestComputeServer_DegradationWithoutJpeg(t *testing.T) {
	// no node_modules: the service must keep running in the documented
	// degraded mode - byte-identical passthrough + grey dominant colors
	cdn := newFakeCDN(t, testRedJPEG, redPNGBytes(t))
	s := startComputeServer(t, prepareComputeDir(t, false), nil)
	cdnURL := cdn.cdnURL("/img/red.jpg")

	code, hdr, body := doGet(t, s.base+imgRequestPath(cdnURL, "160.jpg"))
	if code != http.StatusOK {
		t.Fatalf("160.jpg status = %d, want 200 (passthrough)", code)
	}
	if !bytes.Equal(body, testRedJPEG) {
		t.Errorf("passthrough body differs from the upstream bytes (%d vs %d bytes)", len(body), len(testRedJPEG))
	}
	if got := hdr.Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("passthrough Content-Type = %q, want the upstream image/jpeg", got)
	}

	code, _, cbody := doGet(t, s.base+imgRequestPath(cdnURL, "colors"))
	if code != http.StatusOK {
		t.Fatalf("colors status = %d, want 200", code)
	}
	var out struct {
		Dominant []int `json:"dominant"`
	}
	if err := json.Unmarshal(cbody, &out); err != nil {
		t.Fatalf("colors body is not JSON: %v\n%s", err, cbody)
	}
	if out.Dominant[0] != 128 || out.Dominant[1] != 128 || out.Dominant[2] != 128 {
		t.Errorf("dominant = %v, want [128,128,128] (grey fallback without jpeg-js)", out.Dominant)
	}
}

func TestComputeServer_PNGSourcePassthrough(t *testing.T) {
	png := redPNGBytes(t)
	cdn := newFakeCDN(t, testRedJPEG, png)
	s := startComputeServer(t, prepareComputeDir(t, true), nil) // codec present: decode is tried and fails on the PNG
	cdnURL := cdn.cdnURL("/img/art.png")

	code, hdr, body := doGet(t, s.base+imgRequestPath(cdnURL, "160.jpg"))
	if code != http.StatusOK {
		t.Fatalf("160.jpg status = %d, want 200", code)
	}
	if got := hdr.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want the upstream image/png", got)
	}
	if !bytes.Equal(body, png) {
		t.Error("a PNG source must pass through byte-identically (jpeg-js cannot re-encode it)")
	}

	code, _, cbody := doGet(t, s.base+imgRequestPath(cdnURL, "colors"))
	if code != http.StatusOK {
		t.Fatalf("colors status = %d, want 200", code)
	}
	var out struct {
		Dominant []int `json:"dominant"`
	}
	if err := json.Unmarshal(cbody, &out); err != nil {
		t.Fatalf("colors body is not JSON: %v\n%s", err, cbody)
	}
	if out.Dominant[0] != 128 || out.Dominant[1] != 128 || out.Dominant[2] != 128 {
		t.Errorf("dominant = %v, want [128,128,128] (PNG cannot be decoded)", out.Dominant)
	}
}

func TestComputeServer_UpstreamErrorIs502AndAlive(t *testing.T) {
	cdn := newFakeCDN(t, testRedJPEG, redPNGBytes(t))
	s := startComputeServer(t, prepareComputeDir(t, false), nil)
	cdnURL := cdn.cdnURL("/img/fail.jpg") // upstream answers 500

	if code, _, body := doGet(t, s.base+imgRequestPath(cdnURL, "160.jpg")); code != http.StatusBadGateway {
		t.Errorf("160.jpg status = %d, want 502 (body %s)", code, body)
	}
	if code, _, body := doGet(t, s.base+imgRequestPath(cdnURL, "colors")); code != http.StatusBadGateway {
		t.Errorf("colors status = %d, want 502 (body %s)", code, body)
	}
	// the service must still be alive (an upstream failure must not crash it)
	code, _, _ := doGet(t, s.base+"/api/v1/capabilities")
	if code != http.StatusOK {
		t.Errorf("capabilities status after the upstream error = %d, want 200 (service alive)", code)
	}
}

func TestComputeServer_LightweightTier(t *testing.T) {
	s := startComputeServer(t, prepareComputeDir(t, false), map[string]string{"MIRA_PI_TIER": "lightweight"})
	code, _, body := doGet(t, s.base+"/api/v1/capabilities")
	if code != http.StatusOK {
		t.Fatalf("capabilities status = %d, want 200", code)
	}
	var caps struct {
		Tier string `json:"tier"`
	}
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatalf("capabilities body is not JSON: %v\n%s", err, body)
	}
	if caps.Tier != "cache" {
		t.Errorf("tier = %q, want cache (lightweight)", caps.Tier)
	}
}

func TestComputeServer_BadURL400AndUnknown404(t *testing.T) {
	s := startComputeServer(t, prepareComputeDir(t, false), nil)

	if code, _, body := doGet(t, s.base+"/img/"+url.QueryEscape("file:///tmp/x.txt")+"/160.jpg"); code != http.StatusBadRequest {
		t.Errorf("non-http(s) URL status = %d, want 400 (body %s)", code, body)
	}
	if code, _, body := doGet(t, s.base+"/nope"); code != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404 (body %s)", code, body)
	}
	if code, _, body := doGet(t, s.base+"/img/"+url.QueryEscape("http://cdn.example/a.jpg")); code != http.StatusNotFound {
		t.Errorf("missing suffix segment status = %d, want 404 (body %s)", code, body)
	}
}
