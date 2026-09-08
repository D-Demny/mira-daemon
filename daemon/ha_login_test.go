package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// ticket 9.4 (daemon part 2): contract tests for POST /api/ha/login (fake
// HA websocket server speaking the real auth protocol) and POST /api/ha/test
// (fake HA REST root).
//
// The login tests are deliberately NOT t.Parallel: TestHaLogin_SlowServer
// dials the package-level haLogin*Timeout vars down (shared process state,
// restored in a cleanup) while the handler re-reads them per request. The
// /api/ha/test tests are parallel: they touch no shared state.

// ---------------------------------------------------------------------------
// fake HA websocket server

// fakeHaWs records the client frames of a websocket session so the tests
// can assert the exact request sequence the daemon sends.
type fakeHaWs struct {
	mu     sync.Mutex
	frames [][]byte
	path   string
}

func (f *fakeHaWs) recordFrame(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make([]byte, len(b))
	copy(c, b)
	f.frames = append(f.frames, c)
}

func (f *fakeHaWs) frameCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames)
}

func (f *fakeHaWs) frame(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.frames) {
		return ""
	}
	return string(f.frames[i])
}

// waitFrames blocks until the behavior recorded at least n client frames
// (it records them as the daemon sends them, so this is a join point, not a
// race).
func (f *fakeHaWs) waitFrames(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.frameCount() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d client frames, got %d", n, f.frameCount())
}

// newFakeHaWs stands up an httptest server speaking the HA websocket auth
// protocol at /api/websocket (coder/websocket Accept - the same library the
// client dials with). behavior runs per connection after the handshake.
func newFakeHaWs(t *testing.T, behavior func(ctx context.Context, conn *websocket.Conn, f *fakeHaWs)) (*fakeHaWs, string) {
	t.Helper()
	f := &fakeHaWs{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/websocket" {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		f.mu.Lock()
		f.path = r.URL.Path
		f.mu.Unlock()
		behavior(context.Background(), conn, f)
	}))
	t.Cleanup(ts.Close)
	return f, ts.URL
}

// readFrame reads the next client frame and records it.
func readFrame(ctx context.Context, conn *websocket.Conn, f *fakeHaWs) bool {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return false
	}
	f.recordFrame(data)
	return true
}

// haWsBehaviorSuccess: auth request -> auth_ok, token request -> token
// result with the access_token field (the documented HA shapes, ticket 9.4
// section 1).
func haWsBehaviorSuccess(ctx context.Context, conn *websocket.Conn, f *fakeHaWs) {
	if !readFrame(ctx, conn, f) {
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_ok","ha_version":"2026.9.1"}`)); err != nil {
		return
	}
	if !readFrame(ctx, conn, f) {
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"id":2,"type":"auth/long_lived_access_token/result","access_token":"fake-jwt-123"}`))
}

// haWsBehaviorAuthInvalid: HA rejects the credentials.
func haWsBehaviorAuthInvalid(ctx context.Context, conn *websocket.Conn, f *fakeHaWs) {
	if !readFrame(ctx, conn, f) {
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_invalid"}`))
}

// haWsBehaviorMfa: MFA-enabled account, the auth request gets a reprompt
// instead of auth_ok (the documented shape with the mfa_setup_followup
// field).
func haWsBehaviorMfa(ctx context.Context, conn *websocket.Conn, f *fakeHaWs) {
	if !readFrame(ctx, conn, f) {
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth","mfa_setup":false,"mfa_setup_followup":"abc"}`))
}

// haWsBehaviorSlow: accepts the handshake but never answers - the client's
// (test-dialled-down) overall timeout must fire first.
func haWsBehaviorSlow(ctx context.Context, conn *websocket.Conn, f *fakeHaWs) {
	time.Sleep(3 * time.Second)
}

// ---------------------------------------------------------------------------
// helpers

// withHaLoginTimeouts dials the package-level login timeouts down for the
// duration of a test (restored in a cleanup). Callers must not be
// t.Parallel (shared process state).
func withHaLoginTimeouts(t *testing.T, connect, overall time.Duration) {
	t.Helper()
	oldConnect, oldOverall := haLoginConnectTimeout, haLoginOverallTimeout
	haLoginConnectTimeout = connect
	haLoginOverallTimeout = overall
	t.Cleanup(func() {
		haLoginConnectTimeout, haLoginOverallTimeout = oldConnect, oldOverall
	})
}

func hostPortOf(u string) string { return strings.TrimPrefix(u, "http://") }

func postHaLogin(t *testing.T, base, body string) *http.Response {
	t.Helper()
	resp, err := testClient.Post(base+"/api/ha/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/ha/login: %v", err)
	}
	return resp
}

func mustReadJSON(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode response %q: %v", b, err)
	}
	return m
}

// ---------------------------------------------------------------------------
// POST /api/ha/login

func TestHaLogin_Success(t *testing.T) {
	f, fakeURL := newFakeHaWs(t, haWsBehaviorSuccess)

	srv, base := newTestApiServer(t)
	_ = srv

	// url without scheme + trailing slash: exercises the normalization
	// (http:// default, slash stripped) end to end.
	resp := postHaLogin(t, base, `{"url":"`+hostPortOf(fakeURL)+`/","username":" user1 ","password":"secret-pass"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["token"] != "fake-jwt-123" {
		t.Errorf("token = %v, want fake-jwt-123", body["token"])
	}
	if _, has := body["error"]; has {
		t.Errorf("unexpected error field: %v", body)
	}

	// the exact request sequence the daemon sent
	f.waitFrames(t, 2)
	const wantAuth = `{"id":1,"type":"auth","username":"user1","password":"secret-pass"}`
	if got := f.frame(0); got != wantAuth {
		t.Errorf("auth request = %s, want %s", got, wantAuth)
	}
	const wantToken = `{"id":2,"type":"auth/long_lived_access_token","client_name":"Mira Thing"}`
	if got := f.frame(1); got != wantToken {
		t.Errorf("token request = %s, want %s", got, wantToken)
	}
	if f.path != "/api/websocket" {
		t.Errorf("handshake path = %q, want /api/websocket", f.path)
	}
}

func TestHaLogin_AuthInvalid(t *testing.T) {
	f, fakeURL := newFakeHaWs(t, haWsBehaviorAuthInvalid)

	srv, base := newTestApiServer(t)
	_ = srv

	resp := postHaLogin(t, base, `{"url":"`+hostPortOf(fakeURL)+`","username":"u","password":"p"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != false || body["error"] != "invalid_credentials" {
		t.Errorf("body = %v, want ok:false error:invalid_credentials", body)
	}

	f.waitFrames(t, 1)
	const wantAuth = `{"id":1,"type":"auth","username":"u","password":"p"}`
	if got := f.frame(0); got != wantAuth {
		t.Errorf("auth request = %s, want %s", got, wantAuth)
	}
}

func TestHaLogin_MfaReprompt(t *testing.T) {
	f, fakeURL := newFakeHaWs(t, haWsBehaviorMfa)

	srv, base := newTestApiServer(t)
	_ = srv

	resp := postHaLogin(t, base, `{"url":"`+hostPortOf(fakeURL)+`","username":"u","password":"p"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != false || body["error"] != "mfa" {
		t.Errorf("body = %v, want ok:false error:mfa", body)
	}
	f.waitFrames(t, 1)
}

func TestHaLogin_UnreachableClosedPort(t *testing.T) {
	srv, base := newTestApiServer(t)
	_ = srv

	// nothing listens on this port
	resp := postHaLogin(t, base, `{"url":"http://127.0.0.1:1","username":"u","password":"p"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != false || body["error"] != "unreachable" {
		t.Errorf("body = %v, want ok:false error:unreachable", body)
	}
}

func TestHaLogin_SlowServerTimesOut(t *testing.T) {
	withHaLoginTimeouts(t, 200*time.Millisecond, 300*time.Millisecond)
	_, fakeURL := newFakeHaWs(t, haWsBehaviorSlow)

	srv, base := newTestApiServer(t)
	_ = srv

	start := time.Now()
	resp := postHaLogin(t, base, `{"url":"`+hostPortOf(fakeURL)+`","username":"u","password":"p"}`)
	elapsed := time.Since(start)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != false || body["error"] != "unreachable" {
		t.Errorf("body = %v, want ok:false error:unreachable", body)
	}
	// the dialled-down 300ms overall timeout must have bounded the request
	// (a missing ctx wiring would hang until testClient's 5s timeout)
	if elapsed < 250*time.Millisecond {
		t.Errorf("returned after %v; the 300ms overall timeout did not bound the request", elapsed)
	}
}

func TestHaLogin_BadRequest(t *testing.T) {
	srv, base := newTestApiServer(t)
	_ = srv

	cases := []struct {
		name string
		body string
	}{
		{"missing password", `{"url":"http://h:8123","username":"u"}`},
		{"missing username", `{"url":"http://h:8123","password":"p"}`},
		{"missing url", `{"username":"u","password":"p"}`},
		{"whitespace-only url", `{"url":"   ","username":"u","password":"p"}`},
		{"unknown scheme", `{"url":"ftp://h:8123","username":"u","password":"p"}`},
		{"invalid json", `{"url":`},
		{"empty body", ``},
	}
	for _, tc := range cases {
		resp := postHaLogin(t, base, tc.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, resp.StatusCode)
		}
		body := mustReadJSON(t, resp.Body)
		if body["ok"] != false || body["error"] != "bad_request" {
			t.Errorf("%s: body = %v, want ok:false error:bad_request", tc.name, body)
		}
		resp.Body.Close()
	}
}

// ---------------------------------------------------------------------------
// URL normalization (pure functions)

func TestNormalizeHaURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"10.10.1.104:8123", "http://10.10.1.104:8123"},
		{"10.10.1.104", "http://10.10.1.104"},
		{"http://10.10.1.104:8123", "http://10.10.1.104:8123"},
		{"http://10.10.1.104:8123/", "http://10.10.1.104:8123"},
		{"  http://10.10.1.104:8123/  ", "http://10.10.1.104:8123"},
		{"HTTP://10.10.1.104:8123", "http://10.10.1.104:8123"},
		{"https://ha.example.com:8123", "https://ha.example.com:8123"},
		{"ws://10.10.1.104:8123", "http://10.10.1.104:8123"},
		{"wss://ha.example.com", "https://ha.example.com"},
		{"ftp://host:8123", ""},
	}
	for _, tc := range cases {
		if got := normalizeHaURL(tc.in); got != tc.want {
			t.Errorf("normalizeHaURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHaWebSocketURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"http://10.10.1.104:8123", "ws://10.10.1.104:8123/api/websocket"},
		{"https://ha.example.com", "wss://ha.example.com/api/websocket"},
	}
	for _, tc := range cases {
		if got := haWebSocketURL(tc.in); got != tc.want {
			t.Errorf("haWebSocketURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// POST /api/ha/test

type haProbeStub struct {
	mu       sync.Mutex
	lastAuth string
	lastPath string
	status   int
}

// newHaProbeStub stands up a fake HA REST root answering /api/ with a fixed
// status.
func newHaProbeStub(t *testing.T, status int) (*haProbeStub, *httptest.Server) {
	t.Helper()
	st := &haProbeStub{status: status}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.lastAuth = r.Header.Get("Authorization")
		st.lastPath = r.URL.Path
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st.status)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	return st, ts
}

func postHaTest(t *testing.T, base, body string) *http.Response {
	t.Helper()
	resp, err := testClient.Post(base+"/api/ha/test", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/ha/test: %v", err)
	}
	return resp
}

func TestHaTest_Authenticated(t *testing.T) {
	t.Parallel()
	st, ts := newHaProbeStub(t, http.StatusOK)

	srv, base := newTestApiServer(t)
	srv.SetHomeAssistantConfig(HomeAssistantConfig{URL: "http://default:8123", Token: "default-token"})

	resp := postHaTest(t, base, `{"url":"`+ts.URL+`","token":"ui-token"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := mustReadJSON(t, resp.Body)
	if body["reachable"] != true || body["authenticated"] != true {
		t.Errorf("body = %v, want reachable:true authenticated:true", body)
	}
	defaults, _ := body["defaults"].(map[string]any)
	if defaults == nil || defaults["url"] != "http://default:8123" {
		t.Errorf("defaults = %v, want url http://default:8123", body["defaults"])
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lastAuth != "Bearer ui-token" {
		t.Errorf("upstream auth = %q, want Bearer ui-token", st.lastAuth)
	}
	if st.lastPath != "/api/" {
		t.Errorf("upstream path = %q, want /api/", st.lastPath)
	}
}

func TestHaTest_RejectedStatuses(t *testing.T) {
	t.Parallel()
	// 401/403 = reachable but unauthenticated; any other status counts the
	// same (HA answered the probe, /api/ just did not accept it) - see
	// ha_login.go.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTeapot} {
		_, ts := newHaProbeStub(t, status)

		srv, base := newTestApiServer(t)
		_ = srv

		resp := postHaTest(t, base, `{"url":"`+ts.URL+`","token":"ui-token"}`)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status %d: probe status = %d, want 200", status, resp.StatusCode)
		}
		body := mustReadJSON(t, resp.Body)
		if body["reachable"] != true || body["authenticated"] != false {
			t.Errorf("status %d: body = %v, want reachable:true authenticated:false", status, body)
		}
		resp.Body.Close()
	}
}

func TestHaTest_NoTokenSendsNoAuthHeader(t *testing.T) {
	t.Parallel()
	st, ts := newHaProbeStub(t, http.StatusUnauthorized)

	srv, base := newTestApiServer(t)
	_ = srv

	resp := postHaTest(t, base, `{"url":"`+ts.URL+`"}`)
	defer resp.Body.Close()
	body := mustReadJSON(t, resp.Body)
	if body["reachable"] != true || body["authenticated"] != false {
		t.Errorf("body = %v, want reachable:true authenticated:false", body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lastAuth != "" {
		t.Errorf("upstream auth = %q, want empty (no token given)", st.lastAuth)
	}
}

func TestHaTest_Unreachable(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	_ = srv

	// nothing listens on this port
	resp := postHaTest(t, base, `{"url":"http://127.0.0.1:1","token":"t"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := mustReadJSON(t, resp.Body)
	if body["reachable"] != false || body["authenticated"] != false {
		t.Errorf("body = %v, want reachable:false authenticated:false", body)
	}
}

func TestHaTest_DefaultsFromRuntimeBlob(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	c := srv.(*ConcreteApiServer)
	c.SetHomeAssistantConfig(HomeAssistantConfig{URL: "http://default:8123"})
	c.SetSettingsHandler(&testSettings{blob: []byte(`{"v":3,"ha":{"url":"http://runtime:8123","token":"rt"}}`)})

	resp := postHaTest(t, base, `{"url":"http://127.0.0.1:1"}`)
	defer resp.Body.Close()
	body := mustReadJSON(t, resp.Body)
	defaults, _ := body["defaults"].(map[string]any)
	// the runtime blob wins over the config.yml default (same resolution as
	// the /ha-api/ proxy)
	if defaults == nil || defaults["url"] != "http://runtime:8123" {
		t.Errorf("defaults = %v, want url http://runtime:8123", body["defaults"])
	}
}

func TestHaTest_DefaultsEmpty(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	_ = srv
	// no HA config at all: the default URL is the empty string (the UI
	// pre-fills with whatever it gets)

	resp := postHaTest(t, base, `{"url":"http://127.0.0.1:1"}`)
	defer resp.Body.Close()
	body := mustReadJSON(t, resp.Body)
	defaults, _ := body["defaults"].(map[string]any)
	if defaults == nil || defaults["url"] != "" {
		t.Errorf("defaults = %v, want url \"\"", body["defaults"])
	}
}

func TestHaTest_BadRequest(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	_ = srv

	// Note: an empty/missing url is NO LONGER bad_request (ticket 9.4,
	// task 7) - see TestHaTest_EmptyURLSkipsProbe. Only non-empty urls
	// that do not normalize are rejected here.
	cases := []struct {
		name string
		body string
	}{
		{"unknown scheme", `{"url":"ftp://h:8123"}`},
		{"invalid json", `{"url":`},
	}
	for _, tc := range cases {
		resp := postHaTest(t, base, tc.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, resp.StatusCode)
		}
		body := mustReadJSON(t, resp.Body)
		if body["ok"] != false || body["error"] != "bad_request" {
			t.Errorf("%s: body = %v, want ok:false error:bad_request", tc.name, body)
		}
		resp.Body.Close()
	}
}

// TestHaTest_EmptyURLSkipsProbe: an empty or missing url is legal (ticket
// 9.4, task 7) - the settings UI fetches defaults.url in the unconfigured
// state (no ha entry saved) - so the probe is skipped and the response
// carries the defaults only.
func TestHaTest_EmptyURLSkipsProbe(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	srv.SetHomeAssistantConfig(HomeAssistantConfig{URL: "http://default:8123"})

	// The stub counts probe hits: an empty url must never dial it.
	var mu sync.Mutex
	probeHits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		probeHits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	cases := []struct {
		name string
		body string
	}{
		{"empty url", `{"url":""}`},
		{"whitespace url", `{"url":"   "}`},
		{"missing url field", `{"token":"t"}`},
	}
	for _, tc := range cases {
		resp := postHaTest(t, base, tc.body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", tc.name, resp.StatusCode)
		}
		body := mustReadJSON(t, resp.Body)
		if body["reachable"] != false || body["authenticated"] != false {
			t.Errorf("%s: body = %v, want reachable:false authenticated:false", tc.name, body)
		}
		defaults, _ := body["defaults"].(map[string]any)
		if defaults == nil || defaults["url"] != "http://default:8123" {
			t.Errorf("%s: defaults = %v, want url http://default:8123", tc.name, body["defaults"])
		}
		resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if probeHits != 0 {
		t.Errorf("probe hits = %d, want 0 (an empty url must skip the probe)", probeHits)
	}
}
