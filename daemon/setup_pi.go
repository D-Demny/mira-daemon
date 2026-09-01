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
}

// SetupPiRequest is the POST /api/setup-pi body. The password is only
// handed to the script process (SSH_PASS env) and never logged.
type SetupPiRequest struct {
	Ip       string `json:"ip"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// SetupPiStatus is the GET /api/setup-pi/status response.
type SetupPiStatus struct {
	State      string   `json:"state"`
	JobId      string   `json:"job_id,omitempty"`
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Model      string   `json:"model,omitempty"`
	Tier       string   `json:"tier,omitempty"`
	Error      string   `json:"error,omitempty"`
	LogTail    []string `json:"log_tail,omitempty"`
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
	log        librespot.Logger
	scriptPath string

	mu       sync.Mutex
	jobId    string
	state    string
	started  time.Time
	finished time.Time
	model    string
	tier     string
	errMsg   string
	logLines []string
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
	return &SetupPiService{log: log, scriptPath: path, state: setupPiStateIdle}
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

	s.mu.Lock()
	if s.state == setupPiStateRunning {
		s.mu.Unlock()
		return "", ErrSetupPiBusy
	}
	jobId := fmt.Sprintf("pi-%d", time.Now().UnixNano())
	s.jobId = jobId
	s.state = setupPiStateRunning
	s.started = time.Now()
	s.finished = time.Time{}
	s.model = ""
	s.tier = ""
	s.errMsg = ""
	s.logLines = nil
	s.mu.Unlock()

	// the password deliberately never reaches the log
	s.log.Infof("setup-pi: starting provisioning of %s (user %s) via %s", req.Ip, req.User, s.scriptPath)

	go s.run(req, jobId)
	return jobId, nil
}

func (s *SetupPiService) run(req SetupPiRequest, jobId string) {
	ctx, cancel := context.WithTimeout(context.Background(), setupPiScriptTimeout)
	defer cancel()

	// exec the script directly (rootfs installs it 0755); the password
	// travels only in the environment, never on the command line
	cmd := exec.CommandContext(ctx, s.scriptPath)
	cmd.Env = append(os.Environ(),
		"SSH_HOST="+req.Ip,
		"SSH_USER="+req.User,
		"SSH_PASS="+req.Password,
	)
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
	st := SetupPiStatus{State: s.state, JobId: s.jobId, Model: s.model, Tier: s.tier, Error: s.errMsg}
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
