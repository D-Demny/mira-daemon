package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// epic 10: the hybrid compute server sits on a Raspberry Pi connected to the
// device over USB-Ethernet. The provisioning wizard (setup-pi.sh, packaged
// into the rootfs by the firmware build) is executed by the daemon on behalf
// of the settings UI: the UI POSTs the SSH credentials, the daemon execs the
// script with SSH_HOST/SSH_USER/SSH_PASS in the environment and tracks the
// job in memory.
//
// Cross-service handler like /debug/* and /ha-api/: no player session needed.

// setupPiDefaultScriptPath is where the firmware build (stage 20) installs
// the provisioning wizard script.
const setupPiDefaultScriptPath = "/usr/local/share/mira/setup-pi.sh"

// setupPiScriptEnv overrides the script location (tests, custom images).
const setupPiScriptEnv = "MIRA_SETUP_PI_SCRIPT"

// setupPiScriptTimeout bounds a single provisioning run: the wizard is a
// handful of ssh round-trips plus package installs, anything slower is hung.
const setupPiScriptTimeout = 30 * time.Minute

// setupPiLogCap keeps the in-memory log ring bounded
const setupPiLogCap = 200

// setupPiLogTailLines is how many of the most recent log lines the status
// endpoint returns
const setupPiLogTailLines = 20

const (
	setupPiStateIdle    = "idle"
	setupPiStateRunning = "running"
	setupPiStateSuccess = "success"
	setupPiStateFailed  = "failed"
)

// ErrSetupPiBusy is returned when a provisioning run is already in progress.
var ErrSetupPiBusy = errors.New("a provisioning run is already in progress")

// SetupPiConfig configures the /api/setup-pi endpoints.
type SetupPiConfig struct {
	// ScriptPath of the provisioning wizard; empty = default path.
	ScriptPath string

	// LoadSettings returns the UI settings blob (ticket10-5): the wizard
	// resolves the DEFAULT target profile from it (the active profile)
	// when the POST body carries no explicit profile_id. nil = the implicit
	// legacy profile is always the default (old UI images without piProfiles).
	LoadSettings func() []byte
}

// SetupPiRequest is the POST /api/setup-pi body. The password is only
// handed to the script process (SSH_PASS env) and never logged.
type SetupPiRequest struct {
	Ip       string `json:"ip"`
	User     string `json:"user"`
	Password string `json:"password"`
	// ProfileId selects the profile whose key pair the wizard uses
	// (ticket10-5). Design decision: the wizard works on the ACTIVE profile
	// - the UI saves the profile into the settings blob (and sets it
	// active) before starting the run - and may additionally name it here
	// explicitly. Empty = active profile from the settings blob, or the
	// implicit legacy profile when the blob has no profiles (old UI).
	ProfileId string `json:"profile_id"`
}

// SetupPiStatus is the GET /api/setup-pi/status response.
type SetupPiStatus struct {
	State string `json:"state"`
	JobId string `json:"job_id,omitempty"`
	// ProfileId is the profile this job ran on (ticket10-5); the key
	// fields (KeyInstalled/KeyError) refer to its key pair.
	ProfileId  string   `json:"profile_id,omitempty"`
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Model      string   `json:"model,omitempty"`
	Tier       string   `json:"tier,omitempty"`
	Error      string   `json:"error,omitempty"`
	LogTail    []string `json:"log_tail,omitempty"`
	// KeyInstalled reports whether the device's SSH public key is (believed
	// to be) in the Pi's authorized_keys. Set by a successful key
	// installation; in-memory like the whole job state, a daemon restart
	// resets it to false.
	KeyInstalled bool `json:"key_installed"`
	// KeyError carries the key-setup problem (generation or installation)
	// as a clear message, empty when there is no key-specific failure.
	// Password login keeps working in every case - no half state.
	KeyError string `json:"key_error,omitempty"`
}

// SetupPiHandler is the cross-service handler for the /api/setup-pi*
// endpoints, injected via SetSetupPiHandler on the ApiServer.
type SetupPiHandler interface {
	StartSetupPi(req SetupPiRequest) (jobId string, err error)
	SetupPiStatus() SetupPiStatus
}

// SetupPiService is the minimal in-memory job tracker behind the
// /api/setup-pi endpoints. No persistence: a daemon restart resets the job
// to idle. Only one run at a time.
type SetupPiService struct {
	log          librespot.Logger
	scriptPath   string
	loadSettings func() []byte

	mu           sync.Mutex
	jobId        string
	profileID    string
	state        string
	started      time.Time
	finished     time.Time
	model        string
	tier         string
	errMsg       string
	keyInstalled bool
	keyError     string
	logLines     []string
}

// NewSetupPiService resolves the wizard script path: env override wins over
// the configured path, which wins over the default rootfs location.
func NewSetupPiService(log librespot.Logger, cfg SetupPiConfig) *SetupPiService {
	path := os.Getenv(setupPiScriptEnv)
	if path == "" {
		path = cfg.ScriptPath
	}
	if path == "" {
		path = setupPiDefaultScriptPath
	}
	return &SetupPiService{log: log, scriptPath: path, loadSettings: cfg.LoadSettings, state: setupPiStateIdle}
}

// validateSetupPiRequest checks the request fields. It must never include
// the password in its error output.
func validateSetupPiRequest(req SetupPiRequest) error {
	if strings.TrimSpace(req.Ip) == "" {
		return errors.New("missing ip")
	}
	if net.ParseIP(req.Ip) == nil {
		return fmt.Errorf("invalid ip %q", req.Ip)
	}
	if strings.TrimSpace(req.User) == "" {
		return errors.New("missing user")
	}
	if req.Password == "" {
		return errors.New("missing password")
	}
	// an explicit profile id (ticket10-5) must be safe as a file name; an
	// EMPTY id is valid (it means "the active profile from the settings
	// blob"). The HTTP layer maps this to a synchronous 400.
	if strings.TrimSpace(req.ProfileId) != "" {
		if _, err := sanitizeProfileID(req.ProfileId); err != nil {
			return err
		}
	}
	return nil
}

// StartSetupPi validates the request and kicks off the wizard script in the
// background. It returns the job id immediately (202 at the HTTP layer).
func (s *SetupPiService) StartSetupPi(req SetupPiRequest) (string, error) {
	if err := validateSetupPiRequest(req); err != nil {
		return "", err
	}

	if _, err := os.Stat(s.scriptPath); err != nil {
		return "", fmt.Errorf("provisioning script not found at %s (not packaged in this firmware build)", s.scriptPath)
	}

	// ticket10-5: resolve the target profile before the job starts so a
	// bad id is a synchronous 400, not an in-job failure.
	profileID, err := s.resolveProfileID(req.ProfileId)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	if s.state == setupPiStateRunning {
		s.mu.Unlock()
		return "", ErrSetupPiBusy
	}
	jobId := fmt.Sprintf("pi-%d", time.Now().UnixNano())
	s.jobId = jobId
	s.profileID = profileID
	s.state = setupPiStateRunning
	s.started = time.Now()
	s.finished = time.Time{}
	s.model = ""
	s.tier = ""
	s.errMsg = ""
	s.keyInstalled = false
	s.keyError = ""
	s.logLines = nil
	s.mu.Unlock()

	// the password deliberately never reaches the log
	s.log.Infof("setup-pi: starting provisioning of %s (user %s, profile %s) via %s", req.Ip, req.User, profileID, s.scriptPath)

	go s.run(req, jobId, profileID)
	return jobId, nil
}

// resolveProfileID picks the profile whose key pair the wizard runs on
// (ticket10-5): an explicit profile_id from the request body (the
// add-profile flow of the settings UI), otherwise the active profile of
// the UI settings blob, otherwise the implicit legacy profile (blobs
// written before the profile model). The result is sanitized: an unsafe
// id is rejected (the HTTP layer maps it to a 400).
func (s *SetupPiService) resolveProfileID(explicit string) (string, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return sanitizeProfileID(explicit)
	}
	if s.loadSettings != nil {
		if p := ResolveActiveProfile(s.loadSettings()); p != nil {
			return sanitizeProfileID(p.ID)
		}
	}
	return legacyPiProfileID, nil
}

func (s *SetupPiService) run(req SetupPiRequest, jobId, profileID string) {
	ctx, cancel := context.WithTimeout(context.Background(), setupPiScriptTimeout)
	defer cancel()

	// epic 10 ticket10-3 + ticket10-5: lazy key generation on the device
	// side, PER PROFILE (the legacy profile keeps id_ed25519, all others
	// get <profileId>_ed25519 - see sshkey.go); no-op when the pair already
	// exists. A generation failure must not break provisioning - password
	// login keeps working, only the key installation step below is disabled.
	keyPath := KeyPath()
	pubKey := ""
	keyErr := ""
	if p, err := KeyPathForProfile(profileID); err != nil {
		// cannot happen after StartSetupPi validated the id; defensive
		keyErr = "key generation failed: " + err.Error()
		s.appendLog("setup-pi: " + keyErr)
	} else {
		keyPath = p
	}
	if keyErr == "" {
		if err := EnsureKeyForProfile(ctx, profileID); err != nil {
			keyErr = "key generation failed: " + err.Error()
			s.appendLog("setup-pi: " + keyErr)
		}
	}
	if keyErr == "" {
		if pub, err := ReadPublicKey(keyPath); err != nil {
			keyErr = "key generation failed: " + err.Error()
			s.appendLog("setup-pi: " + keyErr)
		} else {
			pubKey = pub
		}
	}

	// exec the script directly (rootfs installs it 0755); the password
	// travels only in the environment, never on the command line. The wizard
	// gets the key path for its key-first ssh (first run: key not installed
	// on the Pi yet, so it falls back to the password like before).
	cmd := exec.CommandContext(ctx, s.scriptPath)
	cmd.Env = append(os.Environ(),
		"SSH_HOST="+req.Ip,
		"SSH_USER="+req.User,
		"SSH_PASS="+req.Password,
		"MIRA_SSH_KEY_PATH="+keyPath,
	)
	if pubKey != "" {
		cmd.Env = append(cmd.Env, "MIRA_SSH_PUBKEY="+pubKey)
	}
	s.appendLog(fmt.Sprintf("exec %s (host %s, user %s)", s.scriptPath, req.Ip, req.User))

	// stdout and stderr share the log ring so the UI sees one stream
	out := &setupPiLogWriter{svc: s}
	cmd.Stdout = out
	cmd.Stderr = out

	runErr := cmd.Run()

	errMsg := ""
	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			errMsg = fmt.Sprintf("provisioning timed out after %s", setupPiScriptTimeout)
		} else {
			errMsg = runErr.Error()
		}
	}

	// the wizard prints RESULT model="..." tier="..." on the success path
	model, tier := parseSetupPiResult(s.snapshotLogLines())

	// epic 10 ticket10-3 + ticket10-5: the finished run IS the successful
	// password login - install the PROFILE'S key on the Pi now (the key
	// pair of the job's profile, not a global one), in the same
	// authenticated flow (key-first: already works on re-runs; sshpass
	// fallback on the first run where the key is not installed yet). A key
	// failure must not turn the successful provisioning into a failure (no
	// half state): it is surfaced as key_error and password login keeps
	// working. If the run itself failed, key_error stays empty - the run
	// error carries the reason, key_installed simply stays false.
	keyInstalled := false
	if errMsg == "" && pubKey != "" {
		script, err := installKeyCommand(pubKey)
		if err != nil {
			keyErr = "key installation failed: " + err.Error()
			s.appendLog("setup-pi: " + keyErr)
		} else {
			// a fresh context: the install is one ssh round-trip, not part
			// of the 30-minute wizard budget
			ictx, icancel := context.WithTimeout(context.Background(), 2*time.Minute)
			usedKey, out, instErr := RunKeyFirstWithKey(ictx, keyPath, req.Ip, req.User, req.Password, script)
			icancel()
			for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
				if line != "" {
					s.appendLog(line)
				}
			}
			if instErr != nil {
				keyErr = "key installation failed: " + instErr.Error()
				s.log.Warnf("setup-pi: key installation failed: %s", instErr)
			} else {
				keyInstalled = true
				mode := "password"
				if usedKey {
					mode = "ssh key"
				}
				s.log.Infof("setup-pi: ssh key installed on %s (profile %s, via %s)", req.Ip, profileID, mode)
			}
		}
	}

	s.mu.Lock()
	// a newer job has already reset the fields; only the job that owns them writes the outcome
	if s.jobId == jobId {
		if errMsg == "" {
			s.state = setupPiStateSuccess
		} else {
			s.state = setupPiStateFailed
			s.errMsg = errMsg
		}
		s.finished = time.Now()
		if model != "" {
			s.model = model
		}
		if tier != "" {
			s.tier = tier
		}
		s.keyInstalled = keyInstalled
		s.keyError = keyErr
	}
	s.mu.Unlock()

	if errMsg == "" {
		s.log.Infof("setup-pi: provisioning finished (model %q, tier %q)", model, tier)
	} else {
		s.log.Warnf("setup-pi: provisioning failed: %s", errMsg)
	}
}

func (s *SetupPiService) appendLog(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > setupPiLogCap {
		s.logLines = s.logLines[len(s.logLines)-setupPiLogCap:]
	}
}

func (s *SetupPiService) snapshotLogLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := make([]string, len(s.logLines))
	copy(lines, s.logLines)
	return lines
}

// SetupPiStatus returns the current job state. Times are UTC RFC3339.
func (s *SetupPiService) SetupPiStatus() SetupPiStatus {
	s.mu.Lock()
	st := SetupPiStatus{State: s.state, JobId: s.jobId, ProfileId: s.profileID, Model: s.model, Tier: s.tier,
		Error: s.errMsg, KeyInstalled: s.keyInstalled, KeyError: s.keyError}
	if !s.started.IsZero() {
		st.StartedAt = s.started.UTC().Format(time.RFC3339)
	}
	if !s.finished.IsZero() {
		st.FinishedAt = s.finished.UTC().Format(time.RFC3339)
	}
	if len(s.logLines) > 0 {
		tail := s.logLines
		if len(tail) > setupPiLogTailLines {
			tail = tail[len(tail)-setupPiLogTailLines:]
		}
		st.LogTail = make([]string, len(tail))
		copy(st.LogTail, tail)
	}
	s.mu.Unlock()
	return st
}

// setupPiLogWriter funnels the script's combined output into the log ring
type setupPiLogWriter struct {
	svc *SetupPiService
}

func (w *setupPiLogWriter) Write(p []byte) (int, error) {
	text := string(p)
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line != "" {
			w.svc.appendLog(line)
		}
	}
	return len(p), nil
}

// setupPiResultPrefix marks the machine-readable line the wizard prints on
// the success path: RESULT model="Raspberry Pi 4 Model B Rev 1.5" tier="compute"
const setupPiResultPrefix = "RESULT "

// parseSetupPiResult extracts the last RESULT line from the wizard log.
func parseSetupPiResult(lines []string) (model, tier string) {
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, setupPiResultPrefix) {
			continue
		}
		rest := line[len(setupPiResultPrefix):]
		return parseSetupPiQuotedField(rest, "model"), parseSetupPiQuotedField(rest, "tier")
	}
	return "", ""
}

func parseSetupPiQuotedField(s, key string) string {
	idx := strings.Index(s, key+"=")
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(key)+1:]
	if strings.HasPrefix(rest, `"`) {
		rest = rest[1:]
		if i := strings.Index(rest, `"`); i >= 0 {
			return rest[:i]
		}
		return rest
	}
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		return rest[:i]
	}
	return rest
}
