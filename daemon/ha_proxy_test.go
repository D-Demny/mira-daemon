package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// HA proxy (/ha-api/) contract tests: the browser cannot reach HA
// cross-origin, so the daemon must forward method/path/query/body and
// inject the token.

type haStub struct {
	lastMethod  string
	lastPath    string
	lastAuth    string
	lastBody    string
	status      int
	body        string
	contentType string
}

func newHaStub(t *testing.T, status int, body, contentType string) (*haStub, *httptest.Server) {
	t.Helper()
	st := &haStub{status: status, body: body, contentType: contentType}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.lastMethod = r.Method
		st.lastPath = r.URL.Path
		if r.URL.RawQuery != "" {
			st.lastPath += "?" + r.URL.RawQuery
		}
		st.lastAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		st.lastBody = string(b)
		w.Header().Set("Content-Type", st.contentType)
		w.WriteHeader(st.status)
		_, _ = w.Write([]byte(st.body))
	}))
	t.Cleanup(ts.Close)
	return st, ts
}

func TestHaApiProxy_ForwardsGetWithToken(t *testing.T) {
	t.Parallel()
	stub, ts := newHaStub(t, http.StatusOK, `{"entity_id":"light.x","state":"on"}`, "application/json")

	srv, base := newTestApiServer(t)
	srv.SetHomeAssistantConfig(HomeAssistantConfig{URL: ts.URL, Token: "secret-token"})

	resp, err := testClient.Get(base + "/ha-api/states/light.x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != stub.body {
		t.Errorf("body = %q, want passthrough %q", b, stub.body)
	}
	if stub.lastMethod != http.MethodGet || stub.lastPath != "/api/states/light.x" {
		t.Errorf("upstream = %s %s, want GET /api/states/light.x", stub.lastMethod, stub.lastPath)
	}
	if stub.lastAuth != "Bearer secret-token" {
		t.Errorf("upstream auth = %q, want Bearer secret-token", stub.lastAuth)
	}
}

func TestHaApiProxy_ForwardsPostBody(t *testing.T) {
	t.Parallel()
	stub, ts := newHaStub(t, http.StatusOK, `[{"entity_id":"light.x","state":"off"}]`, "application/json")

	srv, base := newTestApiServer(t)
	srv.SetHomeAssistantConfig(HomeAssistantConfig{URL: ts.URL, Token: "tok"})

	payload := `{"entity_id":"light.x"}`
	resp, err := testClient.Post(base+"/ha-api/services/light/toggle", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(b) != stub.body {
		t.Errorf("body = %q", b)
	}
	if stub.lastMethod != http.MethodPost || stub.lastPath != "/api/services/light/toggle" {
		t.Errorf("upstream = %s %s, want POST /api/services/light/toggle", stub.lastMethod, stub.lastPath)
	}
	if stub.lastBody != payload {
		t.Errorf("upstream body = %q, want %q", stub.lastBody, payload)
	}
	if stub.lastAuth != "Bearer tok" {
		t.Errorf("upstream auth = %q", stub.lastAuth)
	}
}

func TestHaApiProxy_PassThroughErrorStatus(t *testing.T) {
	t.Parallel()
	_, ts := newHaStub(t, http.StatusNotFound, `{"message":"not found"}`, "application/json")

	srv, base := newTestApiServer(t)
	srv.SetHomeAssistantConfig(HomeAssistantConfig{URL: ts.URL, Token: "tok"})

	resp, err := testClient.Get(base + "/ha-api/states/light.missing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 passthrough", resp.StatusCode)
	}
}

func TestHaApiProxy_NotConfigured(t *testing.T) {
	t.Parallel()
	_, base := newTestApiServer(t)

	resp, err := testClient.Get(base + "/ha-api/states/light.x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (proxy not configured)", resp.StatusCode)
	}
}

func TestHaApiProxy_UpstreamDown(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	// nothing listens on this port
	srv.SetHomeAssistantConfig(HomeAssistantConfig{URL: "http://127.0.0.1:1", Token: "tok"})

	resp, err := testClient.Get(base + "/ha-api/states/light.x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (upstream unreachable)", resp.StatusCode)
	}
}

// issue #28: the settings blob carries the URL verbatim as typed in the UI.
// A bare "host:port" without scheme used to break the proxy's request
// building (url.Parse rejects it -> HTTP 500 for every /ha-api/ call);
// ParseHaConfig now canonicalizes it, so the proxy must forward with a
// scheme-bearing target and inject the blob's token.
func TestHaApiProxy_BareURLFromBlob(t *testing.T) {
	t.Parallel()
	stub, ts := newHaStub(t, http.StatusOK, `{"entity_id":"light.x","state":"on"}`, "application/json")

	srv, base := newTestApiServer(t)
	// deliberately stale defaults: the (bare-URL) blob must win over them
	srv.SetHomeAssistantConfig(HomeAssistantConfig{URL: "http://127.0.0.1:1", Token: "stale-token"})
	// strip the scheme from the stub URL to reproduce the stored bare form
	host := strings.TrimPrefix(ts.URL, "http://")
	srv.SetSettingsHandler(&testSettings{blob: []byte(`{"v":3,"ha":{"url":` + jsonString(host) + `,"token":"blob-token","tokenSource":"login"}}`)})

	resp, err := testClient.Get(base + "/ha-api/states/light.x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (bare blob url must be canonicalized)", resp.StatusCode)
	}
	if stub.lastMethod != http.MethodGet || stub.lastPath != "/api/states/light.x" {
		t.Errorf("upstream = %s %s, want GET /api/states/light.x", stub.lastMethod, stub.lastPath)
	}
	if stub.lastAuth != "Bearer blob-token" {
		t.Errorf("upstream auth = %q, want Bearer blob-token", stub.lastAuth)
	}
}

// Fresh-flash regression (device incident behind build 176): the CI-baked
// config.yml carries HA_URL as a bare "host:port" without scheme. With no
// settings blob (a fresh flash wipes the browser localStorage) the proxy
// fell back to those defaults and died in url.Parse ("first path segment in
// URL cannot contain colon") -> HTTP 500 for every /ha-api/ call. The blob
// path is covered by TestHaApiProxy_BareURLFromBlob (issue #28); this pins
// the config-defaults path: a scheme-less default must be canonicalized at
// load and forwarded with a scheme-bearing target, 200 not 500.
func TestHaApiProxy_BareURLFromConfigDefaults(t *testing.T) {
	t.Parallel()
	stub, ts := newHaStub(t, http.StatusOK, `{"entity_id":"light.x","state":"on"}`, "application/json")

	srv, base := newTestApiServer(t)
	// no settings handler registered: the config defaults are the ONLY source
	// (a fresh flash has no blob)
	host := strings.TrimPrefix(ts.URL, "http://")
	srv.SetHomeAssistantConfig(HomeAssistantConfig{URL: host, Token: "cfg-token"})

	resp, err := testClient.Get(base + "/ha-api/states/light.x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (bare config-default url must be canonicalized)", resp.StatusCode)
	}
	if stub.lastMethod != http.MethodGet || stub.lastPath != "/api/states/light.x" {
		t.Errorf("upstream = %s %s, want GET /api/states/light.x", stub.lastMethod, stub.lastPath)
	}
	if stub.lastAuth != "Bearer cfg-token" {
		t.Errorf("upstream auth = %q, want Bearer cfg-token", stub.lastAuth)
	}
}
