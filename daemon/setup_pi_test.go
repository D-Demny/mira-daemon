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

func TestSetupPi_StartAndStatusTransition(t *testing.T) {
	t.Parallel()

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

func TestSetupPi_FailedJob(t *testing.T) {
	t.Parallel()

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

func TestSetupPi_BusyConflict(t *testing.T) {
	t.Parallel()

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
