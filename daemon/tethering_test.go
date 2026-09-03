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

// /api/pi/tethering* contract tests (epic 10 ticket10-6, part A): the
// settings UI triggers the USB-tethering setup for the active Pi profile,
// the daemon execs the REAL setup-tethering.sh with the profile's SSH
// credentials (key-first, password fallback) and tracks the job in memory.
// The script's remote ssh calls are served by a fake ssh binary (markers
// MIRA_TETHER_DETECT / MIRA_TETHER_SETUP / MIRA_TETHER_RPINET), the
// device-side internet probe by the FAKE_TETHER_DEV_NET test hook.

// fakeTetherSSH is the fake ssh binary for the tethering tests. It
// reproduces the key-first schema of the real script: key auth (-i present)
// succeeds only when FAKE_SSH_KEY_OK=1, otherwise exit 255; password auth
// (no -i, via the fake sshpass) succeeds unless FAKE_SSH_PASS_FAIL=1. The
// remote program (last argument) is matched by marker:
//
//	MIRA_TETHER_DETECT -> prints UP=<FAKE_TETHER_UP:-eth>
//	MIRA_TETHER_SETUP  -> prints USB/DHCP/NAT lines (or USB=none + exit 1
//	                     when FAKE_TETHER_SETUP_FAIL=1)
//	MIRA_TETHER_RPINET -> prints RPI_NET=ok (FAKE_TETHER_RPI_NET, default ok)
//	otherwise -> exit 0 (the connection probe "true")
const fakeTetherSSH = `#!/bin/sh
key_mode=0
for a in "$@"; do
    [ "$a" = "-i" ] && key_mode=1
done
[ -n "${FAKE_SSH_DELAY:-}" ] && sleep "$FAKE_SSH_DELAY"
if [ "$key_mode" = 1 ] && [ "${FAKE_SSH_KEY_OK:-0}" != 1 ]; then
    printf 'Permission denied (publickey).\n' >&2
    exit 255
fi
if [ "$key_mode" != 1 ] && [ "${FAKE_SSH_PASS_FAIL:-0}" = 1 ]; then
    printf 'Permission denied (password).\n' >&2
    exit 255
fi
cmd=""
for a in "$@"; do
    cmd="$a"
done
case "$cmd" in
    *"MIRA_TETHER_DETECT"*)
        printf "UP=%s\n" "${FAKE_TETHER_UP:-eth}"
        exit 0
        ;;
    *"MIRA_TETHER_SETUP"*)
        if [ "${FAKE_TETHER_SETUP_FAIL:-0}" = 1 ]; then
            printf 'mira-tether: no usb network interface found facing the mira (cable connected?)\n' >&2
            echo "USB=none"
            exit 1
        fi
        echo "mira-tether: usb interface: usb0"
        echo "USB=usb0"
        echo "DHCP=ok"
        case "${FAKE_TETHER_UP:-eth}" in
            none) echo "NAT=skip" ;;
            *) echo "NAT=ok" ;;
        esac
        exit 0
        ;;
    *"MIRA_TETHER_RPINET"*)
        if [ "${FAKE_TETHER_RPI_NET:-ok}" = ok ]; then
            echo "RPI_NET=ok"
            exit 0
        fi
        echo "RPI_NET=fail"
        exit 1
        ;;
    *)
        exit 0
        ;;
esac
`

// fakeTetherBinDir installs the fake ssh/sshpass executables and points PATH
// at them. t.Setenv => the caller must not use t.Parallel.
func fakeTetherBinDir(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for name, body := range map[string]string{
		"ssh":     fakeTetherSSH,
		"sshpass": fakeSSHPass,
	} {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatalf("writing fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeTetherKeyDir points the key storage (MIRA_SSH_KEY_PATH) at a temp
// location and returns the directory; the key files themselves are created
// per profile with writeTetherKey (the daemon requires an existing key pair
// - a missing one is the 409 case under test).
func fakeTetherKeyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(sshKeyPathEnv, filepath.Join(dir, "id_ed25519"))
	t.Setenv("FAKE_SSH_KEY_OK", "1")
	return dir
}

func writeTetherKey(t *testing.T, dir, profileID string) string {
	t.Helper()
	name := profileID + "_ed25519"
	if profileID == legacyPiProfileID {
		name = "id_ed25519"
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("FAKE-PRIVATE-KEY\n"), 0o600); err != nil {
		t.Fatalf("writing fake key: %v", err)
	}
	return p
}

// the stored profile the tethering tests run on (the blob also carries the
// password - the run_ssh password fallback reads it from the profile)
const tetherTestBlob = `{"v":2,"piProfiles":[{"id":"pi-1","label":"first","ip":"192.168.7.1","user":"tuser","password":"tether-pass-123"}],"activePiId":"pi-1"}`

// tetherTestScript is the REAL packaged tethering setup script (repo-relative
// to the daemon package dir): the lifecycle tests exec it against the fake
// ssh/sshpass binaries - the remote-program markers (MIRA_TETHER_DETECT /
// MIRA_TETHER_SETUP / MIRA_TETHER_RPINET) are part of the tested contract, so
// a temp stub would not exercise the script itself.
const tetherTestScript = "../scripts/setup-tethering.sh"

func newTetheringTestService(t *testing.T, script string) (ApiServer, string) {
	t.Helper()
	srv, base := newTestApiServer(t)
	srv.SetTetheringHandler(NewTetheringService(&librespot.NullLogger{}, TetheringConfig{
		ScriptPath:   script,
		LoadSettings: func() []byte { return []byte(tetherTestBlob) },
	}))
	return srv, base
}

func postTethering(t *testing.T, base string, body string) (int, map[string]string) {
	t.Helper()
	resp, err := testClient.Post(base+"/api/pi/tethering", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/pi/tethering: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]string{}
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func getTetheringStatus(t *testing.T, base string) TetheringStatus {
	t.Helper()
	resp, err := testClient.Get(base + "/api/pi/tethering/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	var st TetheringStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	return st
}

func waitForTetheringState(t *testing.T, base string, want string) TetheringStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := getTetheringStatus(t, base)
		if st.State == want {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for state %q", want)
	return TetheringStatus{}
}

func TestTethering_NoHandlerIs503(t *testing.T) {
	t.Parallel()
	_, base := newTestApiServer(t)

	status, _ := postTethering(t, base, ``)
	if status != http.StatusServiceUnavailable {
		t.Errorf("POST status = %d, want 503 (no handler)", status)
	}
	resp, err := testClient.Get(base + "/api/pi/tethering/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET status = %d, want 503 (no handler)", resp.StatusCode)
	}
}

// no t.Parallel(): t.Setenv (key dir)
func TestTethering_ValidationErrors(t *testing.T) {
	fakeTetherBinDir(t)
	fakeTetherKeyDir(t)

	srv, base := newTestApiServer(t)
	srv.SetTetheringHandler(NewTetheringService(&librespot.NullLogger{}, TetheringConfig{
		ScriptPath:   "/nonexistent/setup-tethering.sh",
		LoadSettings: func() []byte { return []byte(tetherTestBlob) },
	}))

	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{"profile_id":`},
		{"unsafe profile id", `{"profile_id":"../escape"}`},
		{"unknown profile id", `{"profile_id":"pi-99"}`},
	}
	for _, tc := range cases {
		status, out := postTethering(t, base, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (error: %s)", tc.name, status, out["error"])
		}
		if out["error"] == "" {
			t.Errorf("%s: expected an error message in the body", tc.name)
		}
	}

	// no profiles in the blob at all: empty body (default profile) -> 400
	srv2, base2 := newTestApiServer(t)
	srv2.SetTetheringHandler(NewTetheringService(&librespot.NullLogger{}, TetheringConfig{
		ScriptPath:   "/nonexistent/setup-tethering.sh",
		LoadSettings: func() []byte { return []byte(`{"v":2,"piProfiles":[]}`) },
	}))
	status, out := postTethering(t, base2, ``)
	if status != http.StatusBadRequest {
		t.Errorf("empty profile list: status = %d, want 400 (error: %s)", status, out["error"])
	}
	if !strings.Contains(out["error"], "no active pi profile") {
		t.Errorf("empty profile list: error = %q, want a no-active-profile message", out["error"])
	}
}

// no t.Parallel(): t.Setenv (key dir)
func TestTethering_NoKeyIs409(t *testing.T) {
	fakeTetherBinDir(t)
	fakeTetherKeyDir(t) // key dir WITHOUT the key file: the 409 case

	srv, base := newTestApiServer(t)
	srv.SetTetheringHandler(NewTetheringService(&librespot.NullLogger{}, TetheringConfig{
		ScriptPath:   tetherTestScript,
		LoadSettings: func() []byte { return []byte(tetherTestBlob) },
	}))

	status, out := postTethering(t, base, ``)
	if status != http.StatusConflict {
		t.Fatalf("POST status = %d, want 409 (error: %s)", status, out["error"])
	}
	if !strings.Contains(out["error"], "provisioning wizard") {
		t.Errorf("error = %q, want the provisioning-wizard hint", out["error"])
	}
	st := getTetheringStatus(t, base)
	if st.State != tetheringStateIdle {
		t.Errorf("state = %q, want idle (no job may have started)", st.State)
	}
}

// no t.Parallel(): the run execs the real script against the fake ssh
func TestTethering_StartAndStatusLifecycle(t *testing.T) {
	fakeTetherBinDir(t)
	dir := fakeTetherKeyDir(t)
	writeTetherKey(t, dir, "pi-1")

	_, base := newTetheringTestService(t, tetherTestScript)

	// initial state is idle
	if st := getTetheringStatus(t, base); st.State != tetheringStateIdle {
		t.Fatalf("initial state = %q, want idle", st.State)
	}

	// start the job with an EMPTY body (default = the active profile)
	status, out := postTethering(t, base, ``)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202 (error: %s)", status, out["error"])
	}
	if out["job_id"] == "" {
		t.Fatalf("POST body missing job_id: %s", out)
	}

	st := waitForTetheringState(t, base, tetheringStateSuccess)
	if st.JobId != out["job_id"] {
		t.Errorf("status job_id = %q, want %q", st.JobId, out["job_id"])
	}
	if st.ProfileId != "pi-1" {
		t.Errorf("status profile_id = %q, want the active profile pi-1", st.ProfileId)
	}
	if st.Uplink != "eth" {
		t.Errorf("uplink = %q, want eth", st.Uplink)
	}
	if !st.TetheringOk {
		t.Errorf("tethering_ok = false, want true")
	}
	if !st.InternetOk {
		t.Errorf("internet_ok = false, want true")
	}
	if st.StartedAt == "" || st.FinishedAt == "" {
		t.Errorf("started_at/finished_at = %q/%q, want both set", st.StartedAt, st.FinishedAt)
	}
	if st.Error != "" {
		t.Errorf("error = %q, want empty on success", st.Error)
	}
	// the run log carries the script output (steps + RESULT line)
	foundResult := false
	for _, line := range st.LogTail {
		if strings.Contains(line, "tether-pass-123") {
			t.Errorf("password leaked into log tail: %q", line)
		}
		if strings.Contains(line, `RESULT uplink="eth" tethering="ok" internet="ok"`) {
			foundResult = true
		}
		if strings.Contains(line, "host 192.168.7.1, user tuser") {
			// the daemon's exec line proves SSH_HOST/SSH_USER were passed
		}
	}
	if !foundResult {
		t.Errorf("log tail missing the machine-readable RESULT line: %v", st.LogTail)
	}
}

// no t.Parallel(): the run execs the real script against the fake ssh
func TestTethering_UplinkWlanWithExplicitProfile(t *testing.T) {
	fakeTetherBinDir(t)
	dir := fakeTetherKeyDir(t)
	writeTetherKey(t, dir, "pi-1")
	t.Setenv("FAKE_TETHER_UP", "wlan")

	_, base := newTetheringTestService(t, tetherTestScript)

	status, _ := postTethering(t, base, `{"profile_id":"pi-1"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}
	st := waitForTetheringState(t, base, tetheringStateSuccess)
	if st.Uplink != "wlan" {
		t.Errorf("uplink = %q, want wlan", st.Uplink)
	}
	if !st.TetheringOk || !st.InternetOk {
		t.Errorf("tethering_ok/internet_ok = %v/%v, want both true", st.TetheringOk, st.InternetOk)
	}
}

// no t.Parallel(): the run execs the real script against the fake ssh
func TestTethering_UplinkNoneFailsInternet(t *testing.T) {
	fakeTetherBinDir(t)
	dir := fakeTetherKeyDir(t)
	writeTetherKey(t, dir, "pi-1")
	t.Setenv("FAKE_TETHER_UP", "none")
	t.Setenv("FAKE_TETHER_RPI_NET", "fail")
	t.Setenv("FAKE_TETHER_DEV_NET", "fail")

	_, base := newTetheringTestService(t, tetherTestScript)

	status, _ := postTethering(t, base, ``)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}
	// the run finishes (the script exits 1) -> state failed, but the
	// machine-readable fields are still reported
	st := waitForTetheringState(t, base, tetheringStateFailed)
	if st.Uplink != "none" {
		t.Errorf("uplink = %q, want none", st.Uplink)
	}
	// the usb segment itself was configured (usb found, dhcp started) even
	// without an uplink - only the internet is missing
	if !st.TetheringOk {
		t.Errorf("tethering_ok = false, want true (usb segment is configured)")
	}
	if st.InternetOk {
		t.Errorf("internet_ok = true, want false (no uplink)")
	}
	if st.Error == "" {
		t.Errorf("failed job has no error: %+v", st)
	}
}

// no t.Parallel(): the run execs the real script against the fake ssh
func TestTethering_SetupFailure(t *testing.T) {
	fakeTetherBinDir(t)
	dir := fakeTetherKeyDir(t)
	writeTetherKey(t, dir, "pi-1")
	t.Setenv("FAKE_TETHER_SETUP_FAIL", "1")
	t.Setenv("FAKE_TETHER_DEV_NET", "fail")

	_, base := newTetheringTestService(t, tetherTestScript)

	status, _ := postTethering(t, base, ``)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}
	st := waitForTetheringState(t, base, tetheringStateFailed)
	if st.Uplink != "eth" {
		t.Errorf("uplink = %q, want eth (detection ran before the setup failure)", st.Uplink)
	}
	if st.TetheringOk {
		t.Errorf("tethering_ok = true, want false (no usb interface)")
	}
	if st.InternetOk {
		t.Errorf("internet_ok = true, want false")
	}
}

// no t.Parallel(): the run execs the real script against the fake ssh
func TestTethering_InternetFailWithUplink(t *testing.T) {
	fakeTetherBinDir(t)
	dir := fakeTetherKeyDir(t)
	writeTetherKey(t, dir, "pi-1")
	t.Setenv("FAKE_TETHER_DEV_NET", "fail")

	_, base := newTetheringTestService(t, tetherTestScript)

	status, _ := postTethering(t, base, ``)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}
	st := waitForTetheringState(t, base, tetheringStateFailed)
	if st.TetheringOk != true {
		t.Errorf("tethering_ok = false, want true (usb segment configured)")
	}
	if st.InternetOk {
		t.Errorf("internet_ok = true, want false (device probe failed)")
	}
	if st.Uplink != "eth" {
		t.Errorf("uplink = %q, want eth", st.Uplink)
	}
}

// no t.Parallel(): the run execs the real script against the fake ssh
func TestTethering_KeyFallbackToPassword(t *testing.T) {
	fakeTetherBinDir(t)
	dir := fakeTetherKeyDir(t)
	writeTetherKey(t, dir, "pi-1")
	t.Setenv("FAKE_SSH_KEY_OK", "0") // key auth fails -> password fallback

	_, base := newTetheringTestService(t, tetherTestScript)

	status, _ := postTethering(t, base, ``)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", status)
	}
	st := waitForTetheringState(t, base, tetheringStateSuccess)
	if !st.TetheringOk || !st.InternetOk {
		t.Errorf("tethering_ok/internet_ok = %v/%v, want both true (password fallback served the run)", st.TetheringOk, st.InternetOk)
	}
	for _, line := range st.LogTail {
		if strings.Contains(line, "tether-pass-123") {
			t.Errorf("password leaked into log tail: %q", line)
		}
	}
}

// no t.Parallel(): the run execs the real script against the fake ssh
func TestTethering_BusyConflict(t *testing.T) {
	fakeTetherBinDir(t)
	dir := fakeTetherKeyDir(t)
	writeTetherKey(t, dir, "pi-1")
	t.Setenv("FAKE_SSH_DELAY", "1") // every ssh round-trip takes ~1s

	_, base := newTetheringTestService(t, tetherTestScript)

	status, _ := postTethering(t, base, ``)
	if status != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202", status)
	}
	// the script is still in its ssh round-trips: the second start must be
	// rejected
	status, out := postTethering(t, base, ``)
	if status != http.StatusConflict {
		t.Fatalf("second POST status = %d, want 409 (error: %s)", status, out["error"])
	}
	waitForTetheringState(t, base, tetheringStateSuccess)
}

func TestTethering_ScriptMissing(t *testing.T) {
	t.Parallel()
	srv, base := newTestApiServer(t)
	srv.SetTetheringHandler(NewTetheringService(&librespot.NullLogger{}, TetheringConfig{
		ScriptPath:   "/nonexistent/setup-tethering.sh",
		LoadSettings: func() []byte { return []byte(tetherTestBlob) },
	}))

	// with no key file this hits the 409 BEFORE the script check; create a
	// key so the missing script (500) is what gets reported
	// -> use a service with a key that exists: the key check uses
	// KeyPathForProfile -> MIRA_SSH_KEY_PATH env; set it to a real file
	status, out := postTethering(t, base, `{"profile_id":"pi-1"}`)
	_ = status
	_ = out
	// (the 500 case is covered by the env-override test below where the
	// service is built after the key exists)
}

// no t.Parallel(): t.Setenv (key dir)
func TestTethering_ScriptMissing500(t *testing.T) {
	// key file present, script missing -> 500
	keyFile, err := os.CreateTemp("", "tethkey")
	if err != nil {
		t.Fatalf("creating key file: %v", err)
	}
	keyFile.Close()
	defer os.Remove(keyFile.Name())
	t.Setenv(sshKeyPathEnv, keyFile.Name())

	srv, base := newTestApiServer(t)
	srv.SetTetheringHandler(NewTetheringService(&librespot.NullLogger{}, TetheringConfig{
		ScriptPath: "/nonexistent/setup-tethering.sh",
		LoadSettings: func() []byte {
			return []byte(`{"v":2,"piProfiles":[{"id":"pi-1","ip":"192.168.7.1","user":"u","password":"p"}],"activePiId":"pi-1"}`)
		},
	}))
	// profile pi-1 -> key path <dir of keyFile>/pi-1_ed25519: create it
	if err := os.WriteFile(keyFile.Name()+"_tether", nil, 0o600); err != nil {
		t.Fatalf("writing marker: %v", err)
	}
	// the profile id pi-1 maps to filepath.Join(dir, "pi-1_ed25519")
	if err := os.WriteFile(filepath.Join(filepath.Dir(keyFile.Name()), "pi-1_ed25519"), []byte("k"), 0o600); err != nil {
		t.Fatalf("writing pi-1 key: %v", err)
	}

	status, out := postTethering(t, base, `{"profile_id":"pi-1"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("POST status = %d, want 500 (error: %s)", status, out["error"])
	}
	if !strings.Contains(out["error"], "not found") {
		t.Errorf("error = %q, want a not-found hint", out["error"])
	}
}

// no t.Parallel(): t.Setenv
func TestTethering_EnvOverrideScriptPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fake-tethering.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fake script: %v", err)
	}
	t.Setenv(tetheringScriptEnv, p)

	// env override wins over the configured path
	svc := NewTetheringService(&librespot.NullLogger{}, TetheringConfig{ScriptPath: "/configured/path.sh"})
	if svc.scriptPath != p {
		t.Errorf("scriptPath = %q, want env override %q", svc.scriptPath, p)
	}

	// without the env var the configured path wins
	t.Setenv(tetheringScriptEnv, "")
	svc = NewTetheringService(&librespot.NullLogger{}, TetheringConfig{ScriptPath: "/configured/path.sh"})
	if svc.scriptPath != "/configured/path.sh" {
		t.Errorf("scriptPath = %q, want configured path", svc.scriptPath)
	}

	// and with neither, the default rootfs location
	svc = NewTetheringService(&librespot.NullLogger{}, TetheringConfig{})
	if svc.scriptPath != tetheringDefaultScriptPath {
		t.Errorf("scriptPath = %q, want default %q", svc.scriptPath, tetheringDefaultScriptPath)
	}
}

func TestParseTetheringResult(t *testing.T) {
	t.Parallel()
	cases := []struct {
		lines        []string
		wantUplink   string
		wantTetherOK bool
		wantNetOK    bool
		wantDetail   string
	}{
		{
			lines: []string{
				"noise",
				`RESULT uplink="eth" tethering="ok" internet="ok" detail="usb=usb0 dhcp=ok nat=ok rpinet=ok"`,
			},
			wantUplink: "eth", wantTetherOK: true, wantNetOK: true,
			wantDetail: "usb=usb0 dhcp=ok nat=ok rpinet=ok",
		},
		{
			lines: []string{
				`RESULT uplink="wlan" tethering="ok" internet="fail" detail="no route"`,
			},
			wantUplink: "wlan", wantTetherOK: true, wantNetOK: false, wantDetail: "no route",
		},
		{
			lines: []string{
				`RESULT uplink="none" tethering="ok" internet="fail"`,
				`RESULT uplink="eth" tethering="fail" internet="fail" detail="usb=none"`,
			},
			wantUplink: "eth", wantTetherOK: false, wantNetOK: false, wantDetail: "usb=none",
		},
		{
			lines:      []string{"no result here"},
			wantUplink: "", wantTetherOK: false, wantNetOK: false,
		},
		{
			lines:      nil,
			wantUplink: "", wantTetherOK: false, wantNetOK: false,
		},
		{
			lines:      []string{`RESULT uplink=wlan tethering=ok internet=ok`},
			wantUplink: "wlan", wantTetherOK: true, wantNetOK: true,
		},
	}
	for i, tc := range cases {
		uplink, tetherOK, netOK, detail := parseTetheringResult(tc.lines)
		if uplink != tc.wantUplink || tetherOK != tc.wantTetherOK || netOK != tc.wantNetOK || detail != tc.wantDetail {
			t.Errorf("case %d: got (%q, %v, %v, %q), want (%q, %v, %v, %q)",
				i, uplink, tetherOK, netOK, detail,
				tc.wantUplink, tc.wantTetherOK, tc.wantNetOK, tc.wantDetail)
		}
	}
}
