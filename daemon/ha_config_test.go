package daemon

import (
	"encoding/json"
	"testing"
)

// ticket 9.4 (daemon part 1): parsing the "ha" object out of the UI
// settings blob (schema v3, ha_config.go) and the runtime config
// resolution in getHomeAssistantConfig (blob wins over config.yml
// defaults).

// testSettings is a SettingsHandler stub for tests: a mutable in-memory
// blob the tests can swap between requests.
type testSettings struct {
	blob []byte
}

func (t *testSettings) GetSettings() []byte { return t.blob }
func (t *testSettings) PutSettings(body []byte) error {
	t.blob = body
	return nil
}

func TestParseHaConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		blob string
		want *HaConfig
	}{
		{"empty blob", "", nil},
		{"malformed json", `{"ha":{"url":`, nil},
		{"v2 blob without ha key", `{"v":2,"piProfiles":[]}`, nil},
		{"blob with unrelated keys only", `{"bluetoothDevices":[],"v":3}`, nil},
		{"ha null", `{"ha":null}`, nil},
		{"ha empty object", `{"ha":{}}`, nil},
		{"ha with empty url", `{"ha":{"url":"","username":"u","token":"tok"}}`, nil},
		{"ha with whitespace-only url", `{"ha":{"url":"   "}}`, nil},
		{"full ha object (url+username trimmed, password/token verbatim)",
			`{"v":3,"ha":{"url":" http://10.10.1.104:8123 ","username":" mira ","password":" pass \n","token":" tok123 ","tokenSource":"login"}}`,
			&HaConfig{URL: "http://10.10.1.104:8123", Username: "mira", Password: " pass \n", Token: " tok123 ", TokenSource: "login"}},
		{"tokenSource manual kept", `{"ha":{"url":"http://h:8123","tokenSource":"manual"}}`,
			&HaConfig{URL: "http://h:8123", TokenSource: "manual"}},
		{"tokenSource default kept", `{"ha":{"url":"http://h:8123","tokenSource":"default"}}`,
			&HaConfig{URL: "http://h:8123", TokenSource: "default"}},
		{"tokenSource missing -> default", `{"ha":{"url":"http://h:8123","token":"t"}}`,
			&HaConfig{URL: "http://h:8123", Token: "t", TokenSource: "default"}},
		{"tokenSource foreign value -> default", `{"ha":{"url":"http://h:8123","tokenSource":"qr-code"}}`,
			&HaConfig{URL: "http://h:8123", TokenSource: "default"}},
		{"tokenSource empty string -> default", `{"ha":{"url":"http://h:8123","tokenSource":""}}`,
			&HaConfig{URL: "http://h:8123", TokenSource: "default"}},
		{"ha with only url (rest empty)", `{"ha":{"url":"http://h:8123"}}`,
			&HaConfig{URL: "http://h:8123", TokenSource: "default"}},
		{"broken ha object (wrong type)", `{"ha":"not-an-object"}`, nil},
		{"broken ha url type", `{"ha":{"url":42}}`, nil},
	}
	for _, tc := range cases {
		got := ParseHaConfig([]byte(tc.blob))
		if (got == nil) != (tc.want == nil) {
			t.Errorf("%s: got %+v, want nil=%v", tc.name, got, tc.want == nil)
			continue
		}
		if got != nil && *got != *tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, *got, *tc.want)
		}
	}
}

// getHomeAssistantConfig resolution: runtime blob beats config.yml
// defaults, and every unusable blob falls back to the defaults.

func TestGetHomeAssistantConfig_RuntimeBlobWins(t *testing.T) {
	t.Parallel()
	srv, _ := newTestApiServer(t)
	c := srv.(*ConcreteApiServer)
	c.SetHomeAssistantConfig(HomeAssistantConfig{URL: "http://default:8123", Token: "default-token"})
	st := &testSettings{blob: []byte(`{"v":3,"ha":{"url":" http://runtime:8123 ","username":"u","password":"p","token":"runtime-token","tokenSource":"login"}}`)}
	c.SetSettingsHandler(st)

	got := c.getHomeAssistantConfig()
	if got.URL != "http://runtime:8123" || got.Token != "runtime-token" {
		t.Errorf("got %+v, want runtime blob URL+Token", got)
	}
}

func TestGetHomeAssistantConfig_NoHandlerUsesDefaults(t *testing.T) {
	t.Parallel()
	srv, _ := newTestApiServer(t)
	c := srv.(*ConcreteApiServer)
	c.SetHomeAssistantConfig(HomeAssistantConfig{URL: "http://default:8123", Token: "default-token"})
	// no SettingsHandler set (the nil-safe getSettingsHandler returns nil)

	got := c.getHomeAssistantConfig()
	if got.URL != "http://default:8123" || got.Token != "default-token" {
		t.Errorf("got %+v, want config.yml defaults", got)
	}
}

func TestGetHomeAssistantConfig_Fallbacks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		blob []byte
	}{
		{"no ha key (v2 blob)", []byte(`{"v":2,"piProfiles":[]}`)},
		{"empty blob", nil},
		{"corrupt blob", []byte(`{"ha":{"url":`)},
		{"ha with empty url", []byte(`{"ha":{"url":"  ","token":"t"}}`)},
	}
	for _, tc := range cases {
		srv, _ := newTestApiServer(t)
		c := srv.(*ConcreteApiServer)
		c.SetHomeAssistantConfig(HomeAssistantConfig{URL: "http://default:8123", Token: "default-token"})
		c.SetSettingsHandler(&testSettings{blob: tc.blob})

		got := c.getHomeAssistantConfig()
		if got.URL != "http://default:8123" || got.Token != "default-token" {
			t.Errorf("%s: got %+v, want config.yml defaults", tc.name, got)
		}
	}
}

// the proxy contract end to end: a runtime blob re-points /ha-api/ without
// a restart, and the 503 "not configured" path still works when neither
// blob nor config.yml carry a URL.

func TestGetHomeAssistantConfig_ProxyHonorsRuntimeBlob(t *testing.T) {
	t.Parallel()
	stub, ts := newHaStub(t, 200, `{"ok":true}`, "application/json")

	srv, base := newTestApiServer(t)
	c := srv.(*ConcreteApiServer)
	c.SetHomeAssistantConfig(HomeAssistantConfig{URL: "http://127.0.0.1:1", Token: "stale-token"}) // wrong target
	c.SetSettingsHandler(&testSettings{blob: []byte(`{"v":3,"ha":{"url":` + jsonString(ts.URL) + `,"token":"blob-token"}}`)})

	resp, err := testClient.Get(base + "/ha-api/states/light.x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if stub.lastAuth != "Bearer blob-token" {
		t.Errorf("upstream auth = %q, want Bearer blob-token", stub.lastAuth)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
