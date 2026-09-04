package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// epic 10 ticket10-5: the Pi profile model on the daemon side - parsing the
// UI settings blob (v2 piProfiles + legacy piServer fallback), resolving the
// active profile, sanitizing profile ids, the per-profile key storage, the
// profile deletion service and the DELETE /api/pi/profile contract.

func TestParsePiProfiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		blob string
		want []PiProfile
	}{
		{"empty blob", "", nil},
		{"malformed json", `{"nope`, nil},
		{"v2 two profiles (trimmed)", `{"v":2,"piProfiles":[{"id":" pi-1 ","label":"first","ip":" 10.0.0.1 ","user":" u "},{"id":"pi-2","label":"second","ip":"10.0.0.2","user":"v"}]}`,
			[]PiProfile{{ID: "pi-1", Label: "first", Ip: "10.0.0.1", User: "u"}, {ID: "pi-2", Label: "second", Ip: "10.0.0.2", User: "v"}}},
		{"v2 empty list wins over legacy", `{"piProfiles":[],"piServer":{"ip":"10.0.0.9","user":"old"}}`, []PiProfile{}},
		{"legacy shape", `{"piServer":{"ip":"10.0.0.9","user":"old"}}`,
			[]PiProfile{{ID: legacyPiProfileID, Label: legacyPiProfileID, Ip: "10.0.0.9", User: "old"}}},
		{"legacy shape with empty ip", `{"piServer":{"ip":"","user":"old"}}`, nil},
		{"blob without pi info", `{"bluetoothDevices":[]}`, nil},
		{"broken piProfiles array", `{"piProfiles":[{"id":}`, nil},
	}
	for _, tc := range cases {
		got := ParsePiProfiles([]byte(tc.blob))
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %d profiles (%+v), want %d", tc.name, len(got), got, len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: profiles[%d] = %+v, want %+v", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

func TestResolveActiveProfile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		blob string
		want string // "" = nil (no profile)
	}{
		{"active id present", `{"piProfiles":[{"id":"pi-1"},{"id":"pi-2"}],"activePiId":"pi-2"}`, "pi-2"},
		{"stale active id falls back to first", `{"piProfiles":[{"id":"pi-1"},{"id":"pi-2"}],"activePiId":"pi-9"}`, "pi-1"},
		{"no active id falls back to first", `{"piProfiles":[{"id":"pi-1"},{"id":"pi-2"}]}`, "pi-1"},
		{"no profiles", `{}`, ""},
		{"empty blob", "", ""},
		{"legacy blob resolves to implicit legacy", `{"piServer":{"ip":"10.0.0.9","user":"old"}}`, legacyPiProfileID},
	}
	for _, tc := range cases {
		p := ResolveActiveProfile([]byte(tc.blob))
		if tc.want == "" {
			if p != nil {
				t.Errorf("%s: got %+v, want nil", tc.name, p)
			}
			continue
		}
		if p == nil || p.ID != tc.want {
			t.Errorf("%s: got %+v, want profile %q", tc.name, p, tc.want)
		}
	}
}

func TestSanitizeProfileID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in string
		ok bool
	}{
		{"pi-1", true},
		{"legacy", true},
		{"pi-123", true},
		{"  pi-1  ", true}, // trimmed
		{strings.Repeat("a", 64), true},
		{"", false},
		{"   ", false},
		{strings.Repeat("a", 65), false}, // too long
		{"a\"b", false},
		{"a$b", false},
		{"a`b", false},
		{"a b", false},
		{"a/b", false},
		{"a\\b", false},
		{"a\nb", false},
		{"a\r b", false},
		{"a..b", false},
		{".hidden", false},
	}
	for _, tc := range cases {
		got, err := sanitizeProfileID(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("sanitize(%q) unexpected error: %v", tc.in, err)
				continue
			}
			if want := strings.TrimSpace(tc.in); got != want {
				t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, want)
			}
		} else if err == nil {
			t.Errorf("sanitize(%q) accepted (%q), want an error", tc.in, got)
		}
	}
}

func TestKeyPathForProfile(t *testing.T) {
	// no t.Parallel(): t.Setenv
	t.Setenv(sshKeyPathEnv, filepath.Join("/tmp/mira-test-ssh", "id_ed25519"))
	if p, err := KeyPathForProfile("legacy"); err != nil || p != "/tmp/mira-test-ssh/id_ed25519" {
		t.Errorf("KeyPathForProfile(legacy) = %q, %v; want the env override path", p, err)
	}
	if p, err := KeyPathForProfile("pi-1"); err != nil || p != "/tmp/mira-test-ssh/pi-1_ed25519" {
		t.Errorf("KeyPathForProfile(pi-1) = %q, %v; want <dir>/pi-1_ed25519", p, err)
	}
	if _, err := KeyPathForProfile("../x"); err == nil {
		t.Error("traversal profile id accepted")
	}
	if _, err := KeyPathForProfile(""); err == nil {
		t.Error("empty profile id accepted")
	}
}

func TestEnsureKeyForProfile(t *testing.T) {
	fakeSSHBinDir(t)
	legacyPath, _ := fakeKeyEnv(t)

	if err := EnsureKeyForProfile(context.Background(), "pi-9"); err != nil {
		t.Fatalf("EnsureKeyForProfile: %v", err)
	}
	p, err := KeyPathForProfile("pi-9")
	if err != nil {
		t.Fatalf("KeyPathForProfile: %v", err)
	}
	priv, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("profile private key missing: %v", err)
	}
	if string(priv) != "FAKE-PRIVATE-KEY\n" {
		t.Errorf("private key content = %q, want the fake key", priv)
	}
	pub, err := os.ReadFile(p + ".pub")
	if err != nil {
		t.Fatalf("profile public key missing: %v", err)
	}
	if strings.TrimSpace(string(pub)) != fakePublicKey {
		t.Errorf("public key = %q, want %q", pub, fakePublicKey)
	}
	// the legacy pair is NOT touched by a non-legacy profile
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy pair created although only pi-9 was ensured")
	}
}

func TestRemoveKeyCommand(t *testing.T) {
	t.Parallel()
	cmd, err := removeKeyCommand(fakePublicKey)
	if err != nil {
		t.Fatalf("removeKeyCommand: %v", err)
	}
	if !strings.Contains(cmd, "MIRA_KEY_REMOVE") {
		t.Errorf("missing the MIRA_KEY_REMOVE marker: %q", cmd)
	}
	if !strings.Contains(cmd, `LINE="`+fakePublicKey+`"`) {
		t.Errorf("public key not embedded: %q", cmd)
	}
	if strings.Contains(cmd, "__MIRA_PUBKEY__") {
		t.Errorf("placeholder survived: %q", cmd)
	}
	for _, bad := range []string{`a"b`, "a$b", "a`b", "a\nb", "", "   "} {
		if _, err := removeKeyCommand(bad); err == nil {
			t.Errorf("unsafe public key %q accepted", bad)
		}
	}
}

// the deletion service (unit level, fake SSH)

func TestPiProfileDelete_RemovesDeviceKeyAndAuthorizedKeys(t *testing.T) {
	fakeSSHBinDir(t)
	_, authKeys := fakeKeyEnv(t)
	if err := EnsureKeyForProfile(context.Background(), "pi-1"); err != nil {
		t.Fatalf("EnsureKeyForProfile: %v", err)
	}
	if err := os.WriteFile(authKeys, []byte(fakePublicKey+"\n"), 0o600); err != nil {
		t.Fatalf("seeding authorized_keys: %v", err)
	}
	fakeKeyOK(t, true)

	svc := NewPiProfileService(&librespot.NullLogger{}, PiProfileServiceConfig{})
	res, err := svc.DeletePiProfile("pi-1", PiProfileDeleteRequest{Ip: "192.168.7.1", User: "u", Password: "p"})
	if err != nil {
		t.Fatalf("DeletePiProfile: %v", err)
	}
	if !res.KeyRemoved || !res.AuthorizedKeysRemoved {
		t.Errorf("result = %+v, want both removals reported", res)
	}
	if res.Error != "" {
		t.Errorf("error = %q, want empty", res.Error)
	}
	p, _ := KeyPathForProfile("pi-1")
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("device key still present after deletion")
	}
	b, err := os.ReadFile(authKeys)
	if err != nil {
		t.Fatalf("reading authorized_keys: %v", err)
	}
	if strings.Contains(string(b), fakePublicKey) {
		t.Errorf("authorized_keys still contains the key: %q", b)
	}
}

func TestPiProfileDelete_NoCredentialsDeviceSideOnly(t *testing.T) {
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	if err := EnsureKeyForProfile(context.Background(), "pi-1"); err != nil {
		t.Fatalf("EnsureKeyForProfile: %v", err)
	}
	// the mark file proves the fake ssh was never spawned without credentials
	mark := filepath.Join(t.TempDir(), "ssh-calls")
	t.Setenv("FAKE_SSH_MARK", mark)

	svc := NewPiProfileService(&librespot.NullLogger{}, PiProfileServiceConfig{})
	res, err := svc.DeletePiProfile("pi-1", PiProfileDeleteRequest{})
	if err != nil {
		t.Fatalf("DeletePiProfile: %v", err)
	}
	if !res.KeyRemoved {
		t.Errorf("key_removed = false, want true (device-side removal always happens)")
	}
	if res.AuthorizedKeysRemoved {
		t.Errorf("authorized_keys_removed = true, want false (no credentials given)")
	}
	if res.Error != "" {
		t.Errorf("error = %q, want empty", res.Error)
	}
	if _, err := os.Stat(mark); !os.IsNotExist(err) {
		t.Errorf("ssh was spawned although no credentials were given")
	}
}

func TestPiProfileDelete_InvalidProfileId(t *testing.T) {
	t.Parallel()
	svc := NewPiProfileService(&librespot.NullLogger{}, PiProfileServiceConfig{})
	for _, id := range []string{"", "  ", "../x", "a/b", ".hidden", strings.Repeat("a", 100)} {
		if _, err := svc.DeletePiProfile(id, PiProfileDeleteRequest{}); err == nil {
			t.Errorf("DeletePiProfile(%q) accepted, want an error", id)
		}
	}
}

// ticket10-7 (G-D1): deleting a profile clears the reboot-recovery flag
// that belongs to it (file + in-memory episode, so the /api/pi/status
// recovery fields go idle immediately), keeps the flag of another
// profile, and is a no-op when no flag exists.
func TestPiProfileDelete_ClearsMatchingRecoveryFlag(t *testing.T) {
	// no t.Parallel(): t.Setenv (fakeSSHBinDir/fakeKeyEnv) + MIRA_RECOVERY_FLAG
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	flagPath := filepath.Join(t.TempDir(), "flag.json")
	t.Setenv(recoveryFlagEnv, flagPath) // NewPiRecovery resolves the flag path from the env

	// (a) matching profile with an active waiting episode: the flag file
	// AND the in-memory recovery state are cleared by the deletion
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable:   func(ctx context.Context) bool { return true },
		CheckInternet: func(ctx context.Context) (bool, error) { return false, nil },
		Now:           func() time.Time { return t0 },
		NewTicker:     func(d time.Duration) tickerLike { return &fakeTicker{ch: make(chan time.Time, 1)} },
	})
	recoveryWriteFlag(t, flagPath, t0, "pi-1")
	rec.Start()
	defer rec.Stop()
	if st, _ := rec.Status(); st != "waiting_after_reboot" {
		t.Fatalf("recovery state = %q, want waiting_after_reboot (fresh flag of the active profile)", st)
	}
	if err := EnsureKeyForProfile(context.Background(), "pi-1"); err != nil {
		t.Fatalf("EnsureKeyForProfile: %v", err)
	}
	svc := NewPiProfileService(&librespot.NullLogger{}, PiProfileServiceConfig{
		ClearRecoveryFlag: rec.ClearFlagForProfile,
	})
	res, err := svc.DeletePiProfile("pi-1", PiProfileDeleteRequest{})
	if err != nil {
		t.Fatalf("DeletePiProfile: %v", err)
	}
	if !res.KeyRemoved {
		t.Errorf("key_removed = false, want true")
	}
	if recoveryFlagExists(flagPath) {
		t.Errorf("the flag file of the deleted profile survived the deletion")
	}
	if st, _ := rec.Status(); st != "" {
		t.Errorf("recovery state after the deletion = %q, want idle (no status field for a gone profile)", st)
	}

	// (b) a flag belonging to ANOTHER profile is kept (its episode continues)
	recoveryWriteFlag(t, flagPath, t0, "pi-2")
	if err := EnsureKeyForProfile(context.Background(), "pi-1"); err != nil {
		t.Fatalf("EnsureKeyForProfile: %v", err)
	}
	if _, err := svc.DeletePiProfile("pi-1", PiProfileDeleteRequest{}); err != nil {
		t.Fatalf("DeletePiProfile: %v", err)
	}
	flag, err := recoveryReadFlag(flagPath)
	if err != nil || flag == nil || flag.ProfileId != "pi-2" {
		t.Errorf("a flag of another profile was modified (got %+v, %v), want unchanged pi-2", flag, err)
	}

	// (c) no flag file: the deletion succeeds (missing flag = no-op)
	if err := EnsureKeyForProfile(context.Background(), "pi-1"); err != nil {
		t.Fatalf("EnsureKeyForProfile: %v", err)
	}
	res, err = svc.DeletePiProfile("pi-1", PiProfileDeleteRequest{})
	if err != nil {
		t.Fatalf("DeletePiProfile: %v", err)
	}
	if res.Error != "" {
		t.Errorf("error = %q, want empty (a missing flag is a no-op)", res.Error)
	}
}

// DELETE /api/pi/profile contract (HTTP level)

func deletePiProfile(t *testing.T, base, id, body string) *http.Response {
	t.Helper()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest("DELETE", base+"/api/pi/profile?id="+url.QueryEscape(id), strings.NewReader(body))
	} else {
		req, err = http.NewRequest("DELETE", base+"/api/pi/profile?id="+url.QueryEscape(id), nil)
	}
	if err != nil {
		t.Fatalf("building DELETE request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /api/pi/profile: %v", err)
	}
	return resp
}

func TestPiProfileDeleteEndpoint_NoHandlerIs503(t *testing.T) {
	t.Parallel()
	_, base := newTestApiServer(t)
	resp := deletePiProfile(t, base, "pi-1", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (no handler wired)", resp.StatusCode)
	}
}

func TestPiProfileDeleteEndpoint_MissingIdIs400(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	srv.SetPiProfileHandler(NewPiProfileService(&librespot.NullLogger{}, PiProfileServiceConfig{}))

	// no id in the query string
	req, err := http.NewRequest("DELETE", base+"/api/pi/profile", nil)
	if err != nil {
		t.Fatalf("building DELETE request: %v", err)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /api/pi/profile: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing id)", resp.StatusCode)
	}
}

func TestPiProfileDeleteEndpoint_InvalidIdIs400(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	srv.SetPiProfileHandler(NewPiProfileService(&librespot.NullLogger{}, PiProfileServiceConfig{}))
	resp := deletePiProfile(t, base, "../x", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (unsafe id)", resp.StatusCode)
	}
}

func TestPiProfileDeleteEndpoint_FullFlow(t *testing.T) {
	fakeSSHBinDir(t)
	_, authKeys := fakeKeyEnv(t)
	if err := EnsureKeyForProfile(context.Background(), "pi-1"); err != nil {
		t.Fatalf("EnsureKeyForProfile: %v", err)
	}
	if err := os.WriteFile(authKeys, []byte(fakePublicKey+"\n"), 0o600); err != nil {
		t.Fatalf("seeding authorized_keys: %v", err)
	}
	fakeKeyOK(t, true)

	srv, base := newTestApiServer(t)
	srv.SetPiProfileHandler(NewPiProfileService(&librespot.NullLogger{}, PiProfileServiceConfig{}))

	resp := deletePiProfile(t, base, "pi-1", `{"ip":"192.168.7.1","user":"u","password":"p"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, b)
	}
	var res PiProfileDeleteResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if !res.KeyRemoved || !res.AuthorizedKeysRemoved {
		t.Errorf("result = %+v, want both removals reported", res)
	}
	if res.Error != "" {
		t.Errorf("error = %q, want empty", res.Error)
	}
	p, _ := KeyPathForProfile("pi-1")
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("device key still present after deletion")
	}
	b, err := os.ReadFile(authKeys)
	if err != nil {
		t.Fatalf("reading authorized_keys: %v", err)
	}
	if strings.Contains(string(b), fakePublicKey) {
		t.Errorf("authorized_keys still contains the key: %q", b)
	}
}
