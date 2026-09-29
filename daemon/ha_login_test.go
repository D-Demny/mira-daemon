package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// ticket 9.4 (daemon part 2) + issue #4: contract tests for POST /api/ha/login
// (fake HA websocket server speaking the real auth protocol) and POST
// /api/ha/test (fake HA REST root).
//
// Login protocol under test (issue #4): HA 2026.9 removed username/password
// from the WS auth schema - the daemon sends exactly one frame,
// {"type":"auth","access_token":T} (no "id" field, which HA rejects), and
// accepts only auth_ok/auth_invalid as answers; anything else is a protocol
// failure -> unreachable.
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
		// Real HA always sends auth_required as the first server->client frame;
		// the fake must model it so the tests exercise the real protocol shape
		// (issue #23: the daemon had skipped this frame in production).
		_ = conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"auth_required","ha_version":"2026.9.1"}`))
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

// haWsBehaviorSuccess: one auth request -> auth_ok (the documented HA 2026.9
// shape; issue #4: there is no second exchange, the token is not re-issued).
func haWsBehaviorSuccess(ctx context.Context, conn *websocket.Conn, f *fakeHaWs) {
	if !readFrame(ctx, conn, f) {
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_ok","ha_version":"2026.9.1"}`))
}

// haWsBehaviorAuthInvalid: HA rejects the token.
func haWsBehaviorAuthInvalid(ctx context.Context, conn *websocket.Conn, f *fakeHaWs) {
	if !readFrame(ctx, conn, f) {
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_invalid"}`))
}

// haWsBehaviorMfaReprompt: the pre-2026.9 MFA reprompt answer for a username/
// password auth request (the documented shape with the mfa_setup_followup
// field). With token auth the daemon never triggers it, but if an HA build
// ever answered with this shape it is a protocol failure -> unreachable.
func haWsBehaviorMfaReprompt(ctx context.Context, conn *websocket.Conn, f *fakeHaWs) {
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
	// (http:// default, slash stripped) end to end. Token with leading and
	// trailing whitespace on purpose: it must be sent VERBATIM (same rule as
	// ParseHaConfig) and echoed back unchanged.
	const rawToken = " tok-abc "
	resp := postHaLogin(t, base, `{"url":"`+hostPortOf(fakeURL)+`/","token":"`+rawToken+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["token"] != rawToken {
		t.Errorf("token = %v, want the verbatim token %q echoed", body["token"], rawToken)
	}
	if _, has := body["error"]; has {
		t.Errorf("unexpected error field: %v", body)
	}

	// the exact request sequence the daemon sent: exactly one frame (issue
	// #4 removed the token re-issue exchange)
	f.waitFrames(t, 1)
	const wantAuth = `{"type":"auth","access_token":" tok-abc "}`
	if got := f.frame(0); got != wantAuth {
		t.Errorf("auth request = %s, want %s", got, wantAuth)
	}
	if f.path != "/api/websocket" {
		t.Errorf("handshake path = %q, want /api/websocket", f.path)
	}
}

func TestHaLogin_AuthInvalid(t *testing.T) {
	f, fakeURL := newFakeHaWs(t, haWsBehaviorAuthInvalid)

	srv, base := newTestApiServer(t)
	_ = srv

	resp := postHaLogin(t, base, `{"url":"`+hostPortOf(fakeURL)+`","token":"tok"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != false || body["error"] != "invalid_credentials" {
		t.Errorf("body = %v, want ok:false error:invalid_credentials", body)
	}

	f.waitFrames(t, 1)
	const wantAuth = `{"type":"auth","access_token":"tok"}`
	if got := f.frame(0); got != wantAuth {
		t.Errorf("auth request = %s, want %s", got, wantAuth)
	}
}

// TestHaLogin_MfaReprompt pins the behavior on a legacy MFA reprompt answer:
// with token auth (issue #4) it is not its own error class but a protocol
// failure -> unreachable (502), never 401 (the token itself may be fine).
func TestHaLogin_MfaReprompt(t *testing.T) {
	f, fakeURL := newFakeHaWs(t, haWsBehaviorMfaReprompt)

	srv, base := newTestApiServer(t)
	_ = srv

	resp := postHaLogin(t, base, `{"url":"`+hostPortOf(fakeURL)+`","token":"tok"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != false || body["error"] != "unreachable" {
		t.Errorf("body = %v, want ok:false error:unreachable", body)
	}
	f.waitFrames(t, 1)
}

// TestHaLogin_BadOpeningFrame pins the strict handling of the server's first
// frame (issue #23): real HA always sends auth_required; anything else on
// that slot is a protocol failure -> unreachable (502), never 401.
func TestHaLogin_BadOpeningFrame(t *testing.T) {
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
		ctx := context.Background()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello"}`))
		// keep the connection open so the client's read completes
		time.Sleep(2 * time.Second)
	}))
	t.Cleanup(ts.Close)

	srv, base := newTestApiServer(t)
	_ = srv

	resp := postHaLogin(t, base, `{"url":"`+hostPortOf(ts.URL)+`","token":"tok"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != false || body["error"] != "unreachable" {
		t.Errorf("body = %v, want ok:false error:unreachable", body)
	}
}

func TestHaLogin_UnreachableClosedPort(t *testing.T) {
	srv, base := newTestApiServer(t)
	_ = srv

	// nothing listens on this port
	resp := postHaLogin(t, base, `{"url":"http://127.0.0.1:1","token":"tok"}`)
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
	resp := postHaLogin(t, base, `{"url":"`+hostPortOf(fakeURL)+`","token":"tok"}`)
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
		{"missing token", `{"url":"http://h:8123"}`},
		{"empty token", `{"url":"http://h:8123","token":""}`},
		{"missing url", `{"token":"tok"}`},
		{"whitespace-only url", `{"url":"   ","token":"tok"}`},
		{"unknown scheme", `{"url":"ftp://h:8123","token":"tok"}`},
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
// POST /api/ha/login - credential mode (issue #25)
//
// The fake below serves the whole HA 2026.9 bootstrap protocol on one
// httptest server: the REST login flow (/auth/login_flow,
// /auth/login_flow/{flow_id}, /auth/token) and the WS LLA mint endpoint
// (/api/websocket). It records every HTTP request (method, path, headers,
// body) and every WS client frame so the tests assert the exact wire
// sequence the daemon sends.

// Wire constants the fake answers with; the tests assert the daemon's
// requests against them (controlled values only, no secret dumps).
const (
	fakeAuthCode   = "test-auth-code"
	fakeShortToken = "short-lived-jwt"
	fakeMintedLLA  = "minted-ll-jwt"
)

type haBootstrapRec struct {
	Method string
	Path   string
	Body   string
	Header http.Header
}

// fakeHaBootstrap is a fake HA 2026.9 instance speaking the credential-mode
// bootstrap protocol (issue #25). Knobs: invalidAuth makes the credential
// step answer HTTP 200 + errors.base="invalid_auth"; wsDown makes
// /api/websocket refuse the handshake (the REST steps stay up); mintAnswers
// scripts the WS answers to the mint command(s) in order; listAnswer scripts
// the auth/refresh_tokens answer verbatim, existingLLAs builds its default
// success frame.
type fakeHaBootstrap struct {
	mu           sync.Mutex
	httpReqs     []haBootstrapRec
	wsFrames     [][]byte
	mintAnswers  []string
	nextMintAns  int
	invalidAuth  bool
	wsDown       bool
	listAnswer   string
	existingLLAs []string
}

func (f *fakeHaBootstrap) httpRequest(i int) haBootstrapRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.httpReqs) {
		return haBootstrapRec{}
	}
	return f.httpReqs[i]
}

func (f *fakeHaBootstrap) httpCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.httpReqs)
}

func (f *fakeHaBootstrap) wsFrame(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.wsFrames) {
		return ""
	}
	return string(f.wsFrames[i])
}

func (f *fakeHaBootstrap) wsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.wsFrames)
}

// nextListAnswer returns the scripted auth/refresh_tokens answer
// (listAnswer verbatim) or builds the default HA success frame from
// existingLLAs.
func (f *fakeHaBootstrap) nextListAnswer() string {
	if f.listAnswer != "" {
		return f.listAnswer
	}
	entries := make([]string, 0, len(f.existingLLAs))
	for i, name := range f.existingLLAs {
		entries = append(entries, fmt.Sprintf(`{"id":"tok-%d","client_name":%q,"type":"long_lived_access_token"}`, i+1, name))
	}
	return `{"id":1,"type":"result","success":true,"result":[` + strings.Join(entries, ",") + `]}`
}

// uniqueLLATimestamp validates that name is a fallback client_name in the
// "Mira <UTC YYYYMMDD-HHMMSS>[-<random tail>]" form (issue #25) and returns
// the parsed timestamp part.
func uniqueLLATimestamp(t *testing.T, name string) time.Time {
	t.Helper()
	if !strings.HasPrefix(name, haLLAName+" ") {
		t.Fatalf("unique LLA name %q is not prefixed with %q", name, haLLAName+" ")
	}
	suffix := strings.TrimPrefix(name, haLLAName+" ")
	if i := strings.LastIndex(suffix, "-"); i >= 0 {
		suffix = suffix[:i] // drop the random collision-avoidance tail
	}
	ts, err := time.ParseInLocation("20060102-150405", suffix, time.UTC)
	if err != nil {
		t.Fatalf("unique LLA name %q: timestamp part %q is not a UTC YYYYMMDD-HHMMSS value", name, suffix)
	}
	return ts
}

// newFakeHaBootstrap stands up the fake HA instance; base is the canonical
// http:// URL of it (the client_id/redirect_uri assertions are built from
// it).
func newFakeHaBootstrap(t *testing.T, cfg func(f *fakeHaBootstrap)) (*fakeHaBootstrap, string) {
	t.Helper()
	f := &fakeHaBootstrap{}
	if cfg != nil {
		cfg(f)
	}
	if len(f.mintAnswers) == 0 {
		// default HA behavior: the mint command succeeds on the first try.
		f.mintAnswers = []string{`{"id":2,"type":"result","success":true,"result":"` + fakeMintedLLA + `"}`}
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.httpReqs = append(f.httpReqs, haBootstrapRec{Method: r.Method, Path: r.URL.Path, Body: string(body), Header: r.Header.Clone()})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/auth/login_flow":
			// step 1: the local provider asks username+password together in
			// one "init" step (HA 2026.9 shape, issue #25).
			fmt.Fprint(w, `{"type":"form","flow_id":"flow-1234","handler":["homeassistant",null],`+
				`"data_schema":[{"type":"string","name":"username","required":true},{"type":"string","name":"password","required":true}],`+
				`"errors":{},"description_placeholders":null,"last_step":null,"preview":null,"step_id":"init"}`)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/auth/login_flow/"):
			if f.invalidAuth {
				// wrong credentials: HA answers 200 with errors.base set -
				// the daemon must inspect the body, not the status.
				fmt.Fprint(w, `{"type":"form","flow_id":"flow-1234","handler":["homeassistant",null],`+
					`"data_schema":[],"errors":{"base":"invalid_auth"},`+
					`"description_placeholders":null,"last_step":null,"preview":null,"step_id":"init"}`)
				return
			}
			fmt.Fprintf(w, `{"type":"create_entry","flow_id":"flow-1234","handler":["homeassistant",null],"result":"%s"}`, fakeAuthCode)
		case r.Method == http.MethodPost && r.URL.Path == "/auth/token":
			fmt.Fprintf(w, `{"access_token":"%s","token_type":"Bearer","refresh_token":"refresh-id","expires_in":1800,"ha_auth_provider":"homeassistant"}`, fakeShortToken)
		case r.URL.Path == "/api/websocket":
			if f.wsDown {
				http.NotFound(w, r)
				return
			}
			f.serveWs(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return f, ts.URL
}

// serveWs speaks the WS half of the bootstrap on one accepted connection:
// opening auth_required frame (issue #23), auth_ok after the access-token
// auth request, then the scripted answers to the token-list command and the
// mint command(s).
func (f *fakeHaBootstrap) serveWs(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx := context.Background()
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_required","ha_version":"2026.9.1"}`))
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		f.mu.Lock()
		cp := make([]byte, len(data))
		copy(cp, data)
		f.wsFrames = append(f.wsFrames, cp)
		var msg struct {
			Type string `json:"type"`
			ID   *int   `json:"id"`
		}
		ans := ""
		if err := json.Unmarshal(data, &msg); err == nil && msg.ID != nil {
			switch msg.Type {
			case "auth/refresh_tokens":
				ans = f.nextListAnswer()
			default:
				// the mint command (or its unique-name retry)
				if f.nextMintAns < len(f.mintAnswers) {
					ans = f.mintAnswers[f.nextMintAns]
					f.nextMintAns++
				}
			}
		}
		f.mu.Unlock()
		switch msg.Type {
		case "auth":
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth_ok","ha_version":"2026.9.1"}`))
		case "": // malformed frame: stop answering
			return
		}
		if ans != "" {
			_ = conn.Write(ctx, websocket.MessageText, []byte(ans))
		}
	}
}

func mustParseJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return m
}

func TestHaLogin_CredentialsSuccess(t *testing.T) {
	f, fakeURL := newFakeHaBootstrap(t, nil)
	base := hostPortOf(fakeURL)

	srv, apiBase := newTestApiServer(t)
	_ = srv

	resp := postHaLogin(t, apiBase, `{"url":"`+base+`","username":"probeuser","password":"rightpass"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != true || body["token"] != fakeMintedLLA {
		t.Errorf("body = %v, want ok:true with the minted LLA echoed", body)
	}
	if _, has := body["error"]; has {
		t.Errorf("unexpected error field: %v", body)
	}

	// --- REST wire sequence: order, method, path, key fields (no secret
	// dumps)
	wantPaths := []string{"/auth/login_flow", "/auth/login_flow/flow-1234", "/auth/token"}
	for i, wantPath := range wantPaths {
		got := f.httpRequest(i)
		if got.Method != http.MethodPost || got.Path != wantPath {
			t.Errorf("request %d = %s %s, want POST %s", i, got.Method, got.Path, wantPath)
		}
	}
	if f.httpCount() != len(wantPaths)+1 {
		t.Errorf("http requests = %d, want %d (three REST steps + the WS upgrade)", f.httpCount(), len(wantPaths)+1)
	}
	if last := f.httpRequest(len(wantPaths)); last.Method != http.MethodGet || last.Path != "/api/websocket" {
		t.Errorf("request %d = %s %s, want GET /api/websocket (the mint handshake)", len(wantPaths), last.Method, last.Path)
	}

	// step 1: deterministic client pair + the two-element handler
	start := mustParseJSON(t, f.httpRequest(0).Body)
	wantClientID := "http://" + base + "/mira"
	if start["client_id"] != wantClientID {
		t.Errorf("step 1 client_id = %v, want %q", start["client_id"], wantClientID)
	}
	if start["redirect_uri"] != wantClientID+"/callback" {
		t.Errorf("step 1 redirect_uri = %v, want the same-netloc callback", start["redirect_uri"])
	}
	handler, ok := start["handler"].([]any)
	if !ok || len(handler) != 2 || handler[0] != "homeassistant" || handler[1] != nil {
		t.Errorf("step 1 handler = %v, want [\"homeassistant\",null]", start["handler"])
	}
	if ct := f.httpRequest(0).Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("step 1 content type = %q, want application/json", ct)
	}

	// step 2: the same client_id plus the credentials
	sub := mustParseJSON(t, f.httpRequest(1).Body)
	if sub["client_id"] != wantClientID {
		t.Errorf("step 2 client_id = %v, want %q", sub["client_id"], wantClientID)
	}
	if sub["username"] != "probeuser" || sub["password"] != "rightpass" {
		t.Errorf("step 2 fields = %v, want the submitted username+password", sub)
	}

	// step 3: form-urlencoded code exchange with the identical client_id
	if ct := f.httpRequest(2).Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("step 3 content type = %q, want application/x-www-form-urlencoded", ct)
	}
	form, err := url.ParseQuery(f.httpRequest(2).Body)
	if err != nil {
		t.Fatalf("step 3 body is not form-urlencoded: %v", err)
	}
	if form.Get("grant_type") != "authorization_code" || form.Get("client_id") != wantClientID || form.Get("code") != fakeAuthCode {
		t.Errorf("step 3 form = %q, want grant_type=authorization_code&client_id=%s&code=%s", f.httpRequest(2).Body, wantClientID, fakeAuthCode)
	}

	// --- WS wire sequence: auth (no "id" field), the token-list check,
	// then the mint command under the stable name (free in this fake)
	if got := f.wsFrame(0); got != `{"type":"auth","access_token":"`+fakeShortToken+`"}` {
		t.Errorf("ws auth frame = %s, want the short-lived token with no id field", got)
	}
	list := mustParseJSON(t, f.wsFrame(1))
	if list["id"] != float64(1) || list["type"] != "auth/refresh_tokens" {
		t.Errorf("ws list frame = %v, want id:1 type:auth/refresh_tokens", list)
	}
	mint := mustParseJSON(t, f.wsFrame(2))
	if mint["id"] != float64(2) || mint["type"] != "auth/long_lived_access_token" || mint["client_name"] != "Mira" || mint["lifespan"] != float64(3650) {
		t.Errorf("ws mint frame = %v, want id:2 type:auth/long_lived_access_token client_name:Mira lifespan:3650", mint)
	}
	if f.wsCount() != 3 {
		t.Errorf("ws frames = %d, want exactly 3 (auth + token list + mint)", f.wsCount())
	}
}

func TestHaLogin_CredentialsInvalidAuth(t *testing.T) {
	f, fakeURL := newFakeHaBootstrap(t, func(f *fakeHaBootstrap) { f.invalidAuth = true })
	base := hostPortOf(fakeURL)

	srv, apiBase := newTestApiServer(t)
	_ = srv

	resp := postHaLogin(t, apiBase, `{"url":"`+base+`","username":"probeuser","password":"wrongpass"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != false || body["error"] != "invalid_credentials" {
		t.Errorf("body = %v, want ok:false error:invalid_credentials", body)
	}
	if body["message"] != "invalid_auth" {
		t.Errorf("message = %v, want the HA errors.base value surfaced", body["message"])
	}
	// the bootstrap stops at step 2: no token exchange, no WS connection
	if f.httpCount() != 2 {
		t.Errorf("http requests = %d, want 2 (flow start + credential submit)", f.httpCount())
	}
	if f.wsCount() != 0 {
		t.Errorf("ws frames = %d, want 0", f.wsCount())
	}
}

func TestHaLogin_CredentialsUnreachable(t *testing.T) {
	srv, apiBase := newTestApiServer(t)
	_ = srv

	// (1) dial failure in a REST step: nothing listens on this port.
	resp := postHaLogin(t, apiBase, `{"url":"http://127.0.0.1:1","username":"u","password":"p"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("closed port: status = %d, want 502 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != false || body["error"] != "unreachable" {
		t.Errorf("closed port: body = %v, want ok:false error:unreachable", body)
	}

	// (2) dial failure in the WS step: all three REST steps succeed, then
	// the websocket handshake is refused.
	f, fakeURL := newFakeHaBootstrap(t, func(f *fakeHaBootstrap) { f.wsDown = true })
	resp2 := postHaLogin(t, apiBase, `{"url":"`+hostPortOf(fakeURL)+`","username":"u","password":"p"}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("ws down: status = %d, want 502 (body: %s)", resp2.StatusCode, mustReadJSON(t, resp2.Body))
	}
	body2 := mustReadJSON(t, resp2.Body)
	if body2["ok"] != false || body2["error"] != "unreachable" {
		t.Errorf("ws down: body = %v, want ok:false error:unreachable", body2)
	}
	if f.httpCount() != 4 {
		t.Errorf("http requests = %d, want 4 (three REST steps + the refused WS handshake)", f.httpCount())
	}
	last := f.httpRequest(3)
	if last.Method != http.MethodGet || last.Path != "/api/websocket" {
		t.Errorf("request 3 = %s %s, want GET /api/websocket (the refused handshake)", last.Method, last.Path)
	}
}

func TestHaLogin_CredentialsLLANameTaken(t *testing.T) {
	f, fakeURL := newFakeHaBootstrap(t, func(f *fakeHaBootstrap) {
		f.existingLLAs = []string{"Mira"}
	})
	base := hostPortOf(fakeURL)

	srv, apiBase := newTestApiServer(t)
	_ = srv

	resp := postHaLogin(t, apiBase, `{"url":"`+base+`","username":"probeuser","password":"rightpass"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != true || body["token"] != fakeMintedLLA {
		t.Errorf("body = %v, want ok:true with the minted LLA echoed", body)
	}

	// The daemon must not even try the taken stable name: after auth and
	// the token-list check it mints directly under a unique name.
	list := mustParseJSON(t, f.wsFrame(1))
	if list["id"] != float64(1) || list["type"] != "auth/refresh_tokens" {
		t.Errorf("ws list frame = %v, want id:1 type:auth/refresh_tokens", list)
	}
	mint := mustParseJSON(t, f.wsFrame(2))
	name, _ := mint["client_name"].(string)
	if !strings.HasPrefix(name, haLLAName+" ") {
		t.Errorf("mint client_name = %v, want the unique \"Mira <UTC YYYYMMDD-HHMMSS>[-<rand>]\" form (the stable one is taken)", mint["client_name"])
	} else {
		uniqueLLATimestamp(t, name)
	}
	if f.wsCount() != 3 {
		t.Errorf("ws frames = %d, want 3 (auth + token list + unique-name mint)", f.wsCount())
	}
}

// A failing token-list check is non-fatal: the daemon falls back to the
// stable name and succeeds if HA accepts it.
func TestHaLogin_CredentialsLLAListCheckFailure(t *testing.T) {
	f, fakeURL := newFakeHaBootstrap(t, func(f *fakeHaBootstrap) {
		f.listAnswer = `{"id":1,"type":"result","success":false,"error":{"code":"unknown_error","message":"Unknown error"}}`
	})
	base := hostPortOf(fakeURL)
	srv, apiBase := newTestApiServer(t)
	_ = srv

	resp := postHaLogin(t, apiBase, `{"url":"`+base+`","username":"probeuser","password":"rightpass"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != true || body["token"] != fakeMintedLLA {
		t.Errorf("body = %v, want ok:true (a failed list check is non-fatal)", body)
	}
	mint := mustParseJSON(t, f.wsFrame(2))
	if mint["client_name"] != "Mira" {
		t.Errorf("mint client_name = %v, want the stable name after a failed list check", mint["client_name"])
	}
	if f.wsCount() != 3 {
		t.Errorf("ws frames = %d, want 3 (auth + token list + stable-name mint)", f.wsCount())
	}
}

// TOCTOU fallback: the list says "Mira" is free, but the mint still fails
// (e.g. another client took the name in between) - the daemon must retry
// once under a fresh unique name.
func TestHaLogin_CredentialsLLAMintFailureRetry(t *testing.T) {
	f, fakeURL := newFakeHaBootstrap(t, func(f *fakeHaBootstrap) {
		f.mintAnswers = []string{
			`{"id":2,"type":"result","success":false,"error":{"code":"unknown_error","message":"Unknown error"}}`,
			`{"id":3,"type":"result","success":true,"result":"` + fakeMintedLLA + `"}`,
		}
	})
	base := hostPortOf(fakeURL)
	srv, apiBase := newTestApiServer(t)
	_ = srv

	resp := postHaLogin(t, apiBase, `{"url":"`+base+`","username":"probeuser","password":"rightpass"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != true || body["token"] != fakeMintedLLA {
		t.Errorf("body = %v, want ok:true after the unique-name retry", body)
	}
	first := mustParseJSON(t, f.wsFrame(2))
	if first["client_name"] != "Mira" {
		t.Errorf("first mint client_name = %v, want the stable name (the list said it was free)", first["client_name"])
	}
	retry := mustParseJSON(t, f.wsFrame(3))
	name, _ := retry["client_name"].(string)
	if retry["id"] != float64(3) {
		t.Errorf("retry mint id = %v, want 3 (HA requires strictly increasing ids per connection)", retry["id"])
	}
	if !strings.HasPrefix(name, haLLAName+" ") {
		t.Errorf("retry mint client_name = %v, want the unique \"Mira <UTC YYYYMMDD-HHMMSS>[-<rand>]\" form", retry["client_name"])
	} else {
		uniqueLLATimestamp(t, name)
	}
	if f.wsCount() != 4 {
		t.Errorf("ws frames = %d, want 4 (auth + token list + mint + unique-name retry)", f.wsCount())
	}
}

func TestHaLogin_CredentialsValidation(t *testing.T) {
	srv, apiBase := newTestApiServer(t)
	_ = srv

	// mixed token+password: token mode wins (a plain token-mode fake HA
	// answers; credential mode would never reach it and end in 502).
	_, fakeURL := newFakeHaWs(t, haWsBehaviorSuccess)
	resp := postHaLogin(t, apiBase, `{"url":"`+hostPortOf(fakeURL)+`","token":"tok","username":"probeuser","password":"rightpass"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mixed: status = %d, want 200 (body: %s)", resp.StatusCode, mustReadJSON(t, resp.Body))
	}
	body := mustReadJSON(t, resp.Body)
	if body["ok"] != true || body["token"] != "tok" {
		t.Errorf("mixed: body = %v, want token mode (200 + the token echoed)", body)
	}

	// password without username - and username without password: 400.
	for _, tc := range []string{
		`{"url":"http://127.0.0.1:1","username":"probeuser"}`,
		`{"url":"http://127.0.0.1:1","password":"rightpass"}`,
	} {
		resp := postHaLogin(t, apiBase, tc)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc, resp.StatusCode)
		}
		body := mustReadJSON(t, resp.Body)
		if body["ok"] != false || body["error"] != "bad_request" {
			t.Errorf("%s: body = %v, want ok:false error:bad_request", tc, body)
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
