package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// /api/setup-pi* contract tests (epic 10): the settings UI triggers the Pi
// provisioning wizard from the device, the daemon execs the script with
// SSH_HOST/SSH_USER/SSH_PASS in the environment and tracks the job in memory.

// writes an executable fake wizard script to a temp dir
func writeFakeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-setup-pi.sh")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("writing fake script: %v", err)
	}
	return p
}

func postSetupPi(t *testing.T, base string, body string) (int, map[string]string) {
	t.Helper()
	resp, err := testClient.Post(base+"/api/setup-pi", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/setup-pi: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]string{}
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// polls the status endpoint until the job reaches want
func waitForSetupPiState(t *testing.T, base string, want string) SetupPiStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := testClient.Get(base + "/api/setup-pi/status")
		if err != nil {
			t.Fatalf("GET status: %v", err)
		}
		var st SetupPiStatus
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatalf("decoding status: %v", err)
		}
		resp.Body.Close()
		if st.State == want {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for state %q", want)
	return SetupPiStatus{}
}

func TestSetupPi_ValidationErrors(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: writeFakeScript(t, "#!/bin/sh\nexit 0\n")}))

	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"missing ip", `{"user":"u","password":"p"}`},
		{"invalid ip", `{"ip":"999.999.1.1","user":"u","password":"p"}`},
		{"missing user", `{"ip":"192.168.7.1","password":"p"}`},
		{"missing password", `{"ip":"192.168.7.1","user":"u"}`},
		{"malformed json", `{"ip":`},
	}
	for _, tc := range cases {
		status, out := postSetupPi(t, base, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body: %s)", tc.name, status, out["error"])
		}
		if out["error"] == "" {
			t.Errorf("%s: expected an error message in the body", tc.name)
		}
	}
}

// no t.Parallel(): the run execs ssh-keygen/ssh/sshpass, so the fake
// binaries and their env vars must be installed (t.Setenv)
func TestSetupPi_StartAndStatusTransition(t *testing.T) {
	fakeSSHBinDir(t)
	fakeKeyEnv(t)

	script := writeFakeScript(t, `#!/bin/sh
echo "wizard start host=$SSH_HOST user=$SSH_USER passlen=${#SSH_PASS}"
echo "detecting model via device tree"
printf 'RESULT model="Fake Pi Zero W" tier="lightweight"\n'
`)
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: script}))

	// initial state is idle
	resp, err := testClient.Get(base + "/api/setup-pi/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	var st SetupPiStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	resp.Body.Close()
	if st.State != setupPiStateIdle {
		t.Fatalf("initial state = %q, want idle", st.State)
	}

	// start the job: 202 + job id
	status, out := postSetupPi(t, base, `{"ip":"192.168.7.1","user":"fakeuser","password":"s3cret-pw-XYZ"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202 (error: %s)", status, out["error"])
	}
	if out["job_id"] == "" {
		t.Fatalf("POST body missing job_id: %s", out)
	}

	// the script should already be running or have finished (it is fast);
	// poll for the terminal state
	st = waitForSetupPiState(t, base, setupPiStateSuccess)
	if st.JobId != out["job_id"] {
		t.Errorf("status job_id = %q, want %q", st.JobId, out["job_id"])
	}
	if st.Model != "Fake Pi Zero W" {
		t.Errorf("model = %q, want 'Fake Pi Zero W'", st.Model)
	}
	if st.Tier != "lightweight" {
		t.Errorf("tier = %q, want 'lightweight'", st.Tier)
	}
	if st.StartedAt == "" || st.FinishedAt == "" {
		t.Errorf("started_at/finished_at = %q/%q, want both set", st.StartedAt, st.FinishedAt)
	}

	// the script received the credentials via env (host/user verbatim, the
	// password only as a length - it must never appear in the log)
	foundEnvLine := false
	for _, line := range st.LogTail {
		if strings.Contains(line, "s3cret-pw-XYZ") {
			t.Errorf("password leaked into log tail: %q", line)
		}
		if strings.Contains(line, "host=192.168.7.1 user=fakeuser passlen=13") {
			foundEnvLine = true
		}
	}
	if !foundEnvLine {
		t.Errorf("log tail missing the env line (SSH_HOST/SSH_USER/SSH_PASS not passed): %v", st.LogTail)
	}
	if strings.Contains(st.Error, "s3cret-pw-XYZ") {
		t.Errorf("password leaked into error: %q", st.Error)
	}
}

// no t.Parallel(): the run execs ssh-keygen (t.Setenv fake binaries)
func TestSetupPi_FailedJob(t *testing.T) {
	fakeSSHBinDir(t)
	fakeKeyEnv(t)

	script := writeFakeScript(t, `#!/bin/sh
echo "wizard start"
echo "boom: cannot reach pi" >&2
exit 3
`)
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: script}))

	status, _ := postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"p"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}

	st := waitForSetupPiState(t, base, setupPiStateFailed)
	if st.Error == "" {
		t.Fatalf("failed job has no error: %+v", st)
	}
	if !strings.Contains(st.Error, "exit status 3") {
		t.Errorf("error = %q, want exit status 3", st.Error)
	}
	// the stderr line must be in the log tail
	foundErrLine := false
	for _, line := range st.LogTail {
		if strings.Contains(line, "boom: cannot reach pi") {
			foundErrLine = true
		}
	}
	if !foundErrLine {
		t.Errorf("log tail missing stderr line: %v", st.LogTail)
	}
}

// no t.Parallel(): the runs exec ssh-keygen (t.Setenv fake binaries)
func TestSetupPi_BusyConflict(t *testing.T) {
	fakeSSHBinDir(t)
	fakeKeyEnv(t)

	script := writeFakeScript(t, "#!/bin/sh\nsleep 2\necho done\n")
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: script}))

	status, out := postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"p"}`)
	if status != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202 (error: %s)", status, out["error"])
	}
	// the script sleeps 2s, the second start must be rejected while it runs
	status, out = postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"p"}`)
	if status != http.StatusConflict {
		t.Fatalf("second POST status = %d, want 409 (error: %s)", status, out["error"])
	}

	// and the first job still finishes normally
	waitForSetupPiState(t, base, setupPiStateSuccess)
}

func TestSetupPi_ScriptMissing(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: "/nonexistent/setup-pi.sh"}))

	status, out := postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"p"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("POST status = %d, want 500 (error: %s)", status, out["error"])
	}
	if !strings.Contains(out["error"], "not found") {
		t.Errorf("error = %q, want a not-found hint", out["error"])
	}
}

func TestSetupPi_NoHandlerIs503(t *testing.T) {
	t.Parallel()
	// a fresh server has no setup-pi handler wired
	_, base := newTestApiServer(t)

	status, _ := postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"p"}`)
	if status != http.StatusServiceUnavailable {
		t.Errorf("POST status = %d, want 503 (no handler)", status)
	}
	resp, err := testClient.Get(base + "/api/setup-pi/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET status = %d, want 503 (no handler)", resp.StatusCode)
	}
}

// no t.Parallel(): t.Setenv is not allowed in parallel tests
func TestSetupPi_EnvOverrideScriptPath(t *testing.T) {
	p := writeFakeScript(t, "#!/bin/sh\nexit 0\n")
	t.Setenv(setupPiScriptEnv, p)

	// env override wins over the configured path
	svc := NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: "/configured/path.sh"})
	if svc.scriptPath != p {
		t.Errorf("scriptPath = %q, want env override %q", svc.scriptPath, p)
	}

	// without the env var the configured path wins
	t.Setenv(setupPiScriptEnv, "")
	svc = NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: "/configured/path.sh"})
	if svc.scriptPath != "/configured/path.sh" {
		t.Errorf("scriptPath = %q, want configured path", svc.scriptPath)
	}

	// and with neither, the default rootfs location
	svc = NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{})
	if svc.scriptPath != setupPiDefaultScriptPath {
		t.Errorf("scriptPath = %q, want default %q", svc.scriptPath, setupPiDefaultScriptPath)
	}
}

func TestParseSetupPiResult(t *testing.T) {
	t.Parallel()
	cases := []struct {
		lines     []string
		wantModel string
		wantTier  string
	}{
		{[]string{"noise", `RESULT model="Raspberry Pi 4 Model B Rev 1.5" tier="compute"`}, "Raspberry Pi 4 Model B Rev 1.5", "compute"},
		{[]string{`RESULT model="Old" tier="lightweight"`, `RESULT model="New" tier="compute"`}, "New", "compute"},
		{[]string{"no result here", "RESULT model=x tier=y"}, "x", "y"},
		{[]string{"nothing"}, "", ""},
		{nil, "", ""},
		{[]string{`RESULT model="A B" tier="lightweight" extra=1`}, "A B", "lightweight"},
	}
	for i, tc := range cases {
		model, tier := parseSetupPiResult(tc.lines)
		if model != tc.wantModel || tier != tc.wantTier {
			t.Errorf("case %d: got (%q, %q), want (%q, %q)", i, model, tier, tc.wantModel, tc.wantTier)
		}
	}
}

func TestValidateSetupPiRequest(t *testing.T) {
	t.Parallel()
	valid := SetupPiRequest{Ip: "192.168.7.1", User: "root", Password: "pw"}
	if err := validateSetupPiRequest(valid); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
	valid.Ip = "::1" // ipv6 must also parse
	if err := validateSetupPiRequest(valid); err != nil {
		t.Errorf("ipv6 request rejected: %v", err)
	}

	bad := valid
	bad.Ip = ""
	if err := validateSetupPiRequest(bad); err == nil {
		t.Error("empty ip accepted")
	}
	bad = valid
	bad.Ip = "not-an-ip"
	if err := validateSetupPiRequest(bad); err == nil {
		t.Error("garbage ip accepted")
	}
	bad = valid
	bad.User = " "
	if err := validateSetupPiRequest(bad); err == nil {
		t.Error("blank user accepted")
	}
	bad = valid
	bad.Password = ""
	if err := validateSetupPiRequest(bad); err == nil {
		t.Error("empty password accepted")
	}
	// error strings must never contain the password
	for _, tc := range []SetupPiRequest{
		{User: "root", Password: "topsecret"},
		{Ip: "192.168.7.1", Password: "topsecret"},
	} {
		err := validateSetupPiRequest(tc)
		if err != nil && strings.Contains(err.Error(), "topsecret") {
			t.Errorf("validation error leaks password: %v", err)
		}
	}
}

// epic 10 ticket10-3: key installation after the successful password login.
// All three tests are no-t.Parallel (t.Setenv fake SSH binaries) and use the
// fake wizard script + the fake remote authorized_keys file.

func TestSetupPi_KeyInstalledOnSuccess(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, authKeys := fakeKeyEnv(t)
	fakeKeyOK(t, false) // first run: key not on the Pi yet -> password fallback

	script := writeFakeScript(t, `#!/bin/sh
printf 'RESULT model="Fake Pi 4" tier="compute"\n'
`)
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: script}))

	status, _ := postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"p"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}
	st := waitForSetupPiState(t, base, setupPiStateSuccess)
	if !st.KeyInstalled {
		t.Errorf("key_installed = false, want true after a successful run")
	}
	if st.KeyError != "" {
		t.Errorf("key_error = %q, want empty", st.KeyError)
	}

	// the key pair was generated on the device side
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("device key missing: %v", err)
	}
	// and the public key landed in the (fake) authorized_keys
	b, err := os.ReadFile(authKeys)
	if err != nil {
		t.Fatalf("fake authorized_keys missing: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 || lines[0] != fakePublicKey {
		t.Errorf("authorized_keys = %q, want exactly one line %q", lines, fakePublicKey)
	}
}

func TestSetupPi_KeyInstallFailureSurfacesError(t *testing.T) {
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	fakeKeyOK(t, false)
	t.Setenv("FAKE_SSH_PASS_FAIL", "1") // both attempts fail

	// the wizard itself still succeeds (its own ssh is the fake script, no
	// real auth) - only the key install step must fail, without turning the
	// run into a failure (no half state: password login keeps working)
	script := writeFakeScript(t, `#!/bin/sh
printf 'RESULT model="Fake Pi 4" tier="compute"\n'
`)
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: script}))

	status, _ := postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"s3cret-pw"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}
	st := waitForSetupPiState(t, base, setupPiStateSuccess)
	if st.State != setupPiStateSuccess {
		t.Fatalf("state = %q, want success (key failure must not fail the run)", st.State)
	}
	if st.KeyInstalled {
		t.Errorf("key_installed = true, want false")
	}
	if !strings.Contains(st.KeyError, "key installation failed") {
		t.Errorf("key_error = %q, want a key installation failure message", st.KeyError)
	}
	// the password must not leak into the key error
	if strings.Contains(st.KeyError, "s3cret-pw") {
		t.Errorf("password leaked into key_error: %q", st.KeyError)
	}
}

func TestSetupPi_KeyInstallIdempotentSecondRun(t *testing.T) {
	fakeSSHBinDir(t)
	_, authKeys := fakeKeyEnv(t)
	fakeKeyOK(t, false) // first run: password fallback installs the key

	script := writeFakeScript(t, `#!/bin/sh
printf 'RESULT model="Fake Pi 4" tier="compute"\n'
`)
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: script}))

	status, _ := postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"p"}`)
	if status != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202", status)
	}
	st := waitForSetupPiState(t, base, setupPiStateSuccess)
	if !st.KeyInstalled {
		t.Fatalf("first run: key_installed = false, want true")
	}

	// second run: the key is on the Pi now -> key auth serves the install,
	// the append must be skipped (no duplicate line)
	fakeKeyOK(t, true)
	status, _ = postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"p"}`)
	if status != http.StatusAccepted {
		t.Fatalf("second POST status = %d, want 202", status)
	}
	st = waitForSetupPiState(t, base, setupPiStateSuccess)
	if !st.KeyInstalled {
		t.Errorf("second run: key_installed = false, want true")
	}
	if st.KeyError != "" {
		t.Errorf("second run: key_error = %q, want empty", st.KeyError)
	}
	b, err := os.ReadFile(authKeys)
	if err != nil {
		t.Fatalf("fake authorized_keys missing: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("authorized_keys has %d lines after the second run, want exactly 1 (idempotent): %q", len(lines), lines)
	}
	if lines[0] != fakePublicKey {
		t.Errorf("authorized_keys line = %q, want %q", lines[0], fakePublicKey)
	}
}

// epic 10 ticket10-5: the wizard works PER PROFILE - explicit profile_id,
// the active blob profile as default, the implicit legacy profile for old
// blobs, and a synchronous 400 for unsafe ids.

// no t.Parallel(): the run execs ssh-keygen/ssh/sshpass (t.Setenv fakes)
func TestSetupPi_PerProfileKeyWithExplicitId(t *testing.T) {
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	fakeKeyOK(t, false)

	script := writeFakeScript(t, `#!/bin/sh
printf 'RESULT model="Fake Pi 4" tier="compute"\n'
`)
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: script}))

	status, _ := postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"p","profile_id":"pi-2"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}
	st := waitForSetupPiState(t, base, setupPiStateSuccess)
	if st.ProfileId != "pi-2" {
		t.Errorf("status profile_id = %q, want pi-2", st.ProfileId)
	}
	if !st.KeyInstalled {
		t.Errorf("key_installed = false, want true")
	}
	p2, err := KeyPathForProfile("pi-2")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-2): %v", err)
	}
	if _, err := os.Stat(p2); err != nil {
		t.Errorf("device key for pi-2 missing: %v", err)
	}
	// the legacy pair must NOT be created by a pi-2 run
	if _, err := os.Stat(KeyPath()); !os.IsNotExist(err) {
		t.Errorf("legacy key pair created although the job ran on pi-2")
	}
}

// no t.Parallel(): the run execs ssh-keygen (t.Setenv fakes)
func TestSetupPi_DefaultProfileFromSettingsBlob(t *testing.T) {
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	fakeKeyOK(t, false)

	blob := `{"v":2,"piProfiles":[{"id":"pi-1","label":"first","ip":"192.168.7.1","user":"u1"},{"id":"pi-3","label":"third","ip":"192.168.7.3","user":"u3"}],"activePiId":"pi-3"}`
	script := writeFakeScript(t, `#!/bin/sh
printf 'RESULT model="Fake Pi 4" tier="compute"\n'
`)
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{
		ScriptPath:   script,
		LoadSettings: func() []byte { return []byte(blob) },
	}))

	status, _ := postSetupPi(t, base, `{"ip":"192.168.7.3","user":"u3","password":"p"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}
	st := waitForSetupPiState(t, base, setupPiStateSuccess)
	if st.ProfileId != "pi-3" {
		t.Errorf("status profile_id = %q, want the active blob profile pi-3", st.ProfileId)
	}
	if !st.KeyInstalled {
		t.Errorf("key_installed = false, want true")
	}
	p3, err := KeyPathForProfile("pi-3")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-3): %v", err)
	}
	if _, err := os.Stat(p3); err != nil {
		t.Errorf("device key for pi-3 missing: %v", err)
	}
}

// no t.Parallel(): the run execs ssh-keygen (t.Setenv fakes)
func TestSetupPi_LegacyBlobUsesLegacyProfile(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	fakeKeyOK(t, false)

	blob := `{"piServer":{"ip":"192.168.7.9","user":"legacyuser","password":"x"}}`
	script := writeFakeScript(t, `#!/bin/sh
printf 'RESULT model="Fake Pi 4" tier="compute"\n'
`)
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{
		ScriptPath:   script,
		LoadSettings: func() []byte { return []byte(blob) },
	}))

	status, _ := postSetupPi(t, base, `{"ip":"192.168.7.9","user":"legacyuser","password":"p"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}
	st := waitForSetupPiState(t, base, setupPiStateSuccess)
	if st.ProfileId != legacyPiProfileID {
		t.Errorf("status profile_id = %q, want the implicit legacy profile", st.ProfileId)
	}
	if !st.KeyInstalled {
		t.Errorf("key_installed = false, want true")
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("legacy device key missing: %v", err)
	}
}

// an unsafe explicit profile_id is a synchronous 400 (validated before the
// job starts, so it is never an in-job failure)
func TestSetupPi_InvalidProfileIdIs400(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	srv.SetSetupPiHandler(NewSetupPiService(&librespot.NullLogger{}, SetupPiConfig{ScriptPath: writeFakeScript(t, "#!/bin/sh\nexit 0\n")}))

	status, out := postSetupPi(t, base, `{"ip":"192.168.7.1","user":"u","password":"p","profile_id":"../escape"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("POST status = %d, want 400 (error: %s)", status, out["error"])
	}
	if out["error"] == "" {
		t.Errorf("expected an error message in the body")
	}
}
