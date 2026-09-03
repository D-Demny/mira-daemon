package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// epic 10 ticket10-6 (part A): the USB-tethering setup for the ACTIVE
// Raspberry Pi profile. The settings UI (ticket10-6 part C, the onboarding
// wizard) triggers the run: the daemon execs the packaged setup-tethering.sh
// with SSH_HOST/SSH_USER (and SSH_PASS when the stored profile carries a
// password) plus the per-profile device key path (ticket10-5) and tracks the
// job in memory, like the provisioning wizard (setup_pi.go).
//
// Contract differences to /api/setup-pi (documented design decisions):
//   - The run REQUIRES an existing device key for the target profile: the
//     first password login (wizard) installs the key (ticket10-3/10-5). A
//     missing key pair is a synchronous 409 (ErrTetheringNoKey), not an
//     in-job failure.
//   - The request body carries no credentials, only an optional profile_id:
//     host/user/password come from the stored profile of the UI settings
//     blob (the password is only the run_ssh password fallback, like the
//     wizard; key auth is the primary path).
//   - No key installation happens here (key setup stays with the wizard).
//   - The machine-readable result is
//     RESULT uplink="eth|wlan|none" tethering="ok|fail" internet="ok|fail" detail="..."
//     - uplink: the rpi's upstream (ethernet / wifi / none)
//     - tethering: the usb segment on the rpi side is configured
//       (usb interface + 192.168.7.1 + dhcp service)
//     - internet: the DEVICE itself reached the internet over the usb
//       segment (the authoritative check; the rpi-side uplink test only
//       feeds the detail)
//   - Timeout: a full run is a handful of ssh round-trips plus the dnsmasq
//     package install on slow images and the device-side probe retries -
//     10 minutes is generous and bounded (the provisioning wizard keeps its
//     30-minute budget for the bigger tier deploys).
//
// Cross-service handler like /api/setup-pi and /api/pi/status: no player
// session needed.

// tetheringDefaultScriptPath is where the firmware build (stage 20) installs
// the tethering setup script.
const tetheringDefaultScriptPath = "/usr/local/share/mira/setup-tethering.sh"

// tetheringScriptEnv overrides the script location (tests, custom images).
const tetheringScriptEnv = "MIRA_TETHERING_SCRIPT"

// tetheringScriptTimeout bounds a single tethering run (see the file header:
// 10 minutes - ssh round-trips + dnsmasq install + device-side probes).
const tetheringScriptTimeout = 10 * time.Minute

// tetheringLogCap keeps the in-memory log ring bounded (pattern: setupPiLogCap)
const tetheringLogCap = 200

// tetheringLogTailLines is how many of the most recent log lines the status
// endpoint returns
const tetheringLogTailLines = 20

const (
	tetheringStateIdle    = "idle"
	tetheringStateRunning = "running"
	tetheringStateSuccess = "success"
	tetheringStateFailed  = "failed"
)

// ErrTetheringBusy is returned when a tethering run is already in progress.
var ErrTetheringBusy = errors.New("a tethering run is already in progress")

// ErrTetheringNoKey is returned when the device key pair of the target
// profile does not exist (the provisioning wizard must run first). The HTTP
// layer maps it to a synchronous 409.
var ErrTetheringNoKey = errors.New("no ssh key for the target profile: run the provisioning wizard first")

// ErrTetheringProfile is returned when the request names a profile that is
// not usable for a tethering run (unknown id, no active profile, empty
// ip/user). The HTTP layer maps it to a synchronous 400.
var ErrTetheringProfile = errors.New("no usable pi profile for the tethering run")

// TetheringConfig configures the /api/pi/tethering endpoints.
type TetheringConfig struct {
	// ScriptPath of the tethering setup script; empty = default path.
	ScriptPath string

	// LoadSettings returns the UI settings blob (ticket10-5 profile model):
	// the run resolves the target profile from it (the active profile when
	// the body carries no explicit profile_id) and takes the profile's
	// stored password as the ssh fallback.
	LoadSettings func() []byte
}

// TetheringRequest is the POST /api/pi/tethering body. All fields optional:
// empty profile_id = the active profile of the UI settings blob.
type TetheringRequest struct {
	ProfileId string `json:"profile_id"`
}

// TetheringStatus is the GET /api/pi/tethering/status response.
// Uplink is one of eth|wlan|none (empty before a finished run);
// TetheringOk/InternetOk are always present (false = not achieved yet).
type TetheringStatus struct {
	State       string   `json:"state"`
	JobId       string   `json:"job_id,omitempty"`
	ProfileId   string   `json:"profile_id,omitempty"`
	StartedAt   string   `json:"started_at,omitempty"`
	FinishedAt  string   `json:"finished_at,omitempty"`
	Uplink      string   `json:"uplink,omitempty"`
	TetheringOk bool     `json:"tethering_ok"`
	InternetOk  bool     `json:"internet_ok"`
	Error       string   `json:"error,omitempty"`
	LogTail     []string `json:"log_tail,omitempty"`
}

// TetheringHandler is the cross-service handler for the /api/pi/tethering*
// endpoints, injected via SetTetheringHandler on the ApiServer.
type TetheringHandler interface {
	StartTethering(req TetheringRequest) (jobId string, err error)
	TetheringStatus() TetheringStatus
}

// TetheringService is the minimal in-memory job tracker behind the
// /api/pi/tethering endpoints. No persistence: a daemon restart resets the
// job to idle. Only one run at a time.
type TetheringService struct {
	log          librespot.Logger
	scriptPath   string
	loadSettings func() []byte

	mu          sync.Mutex
	jobId       string
	profileID   string
	state       string
	started     time.Time
	finished    time.Time
	uplink      string
	tetheringOk bool
	internetOk  bool
	errMsg      string
	logLines    []string
}

// NewTetheringService resolves the setup script path: env override wins over
// the configured path, which wins over the default rootfs location.
func NewTetheringService(log librespot.Logger, cfg TetheringConfig) *TetheringService {
	path := os.Getenv(tetheringScriptEnv)
	if path == "" {
		path = cfg.ScriptPath
	}
	if path == "" {
		path = tetheringDefaultScriptPath
	}
	return &TetheringService{log: log, scriptPath: path, loadSettings: cfg.LoadSettings, state: tetheringStateIdle}
}

// resolveTetheringProfile picks the profile the run works on: an explicit
// profile_id from the body (must name a stored profile), otherwise the
// active profile of the UI settings blob. The result must carry ip and user
// (the run needs a connect target). All problems are wrapped in
// ErrTetheringProfile (synchronous 400 at the HTTP layer).
func (s *TetheringService) resolveTetheringProfile(explicit string) (*PiProfile, error) {
	if s.loadSettings == nil {
		return nil, fmt.Errorf("%w: no settings source", ErrTetheringProfile)
	}
	blob := s.loadSettings()
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		profiles := ParsePiProfiles(blob)
		for i := range profiles {
			p := profiles[i]
			if p.ID == explicit {
				if strings.TrimSpace(p.Ip) == "" || strings.TrimSpace(p.User) == "" {
					return nil, fmt.Errorf("%w: profile %q has no ip/user", ErrTetheringProfile, p.ID)
				}
				return &p, nil
			}
		}
		return nil, fmt.Errorf("%w: unknown profile id %q", ErrTetheringProfile, explicit)
	}
	p := ResolveActiveProfile(blob)
	if p == nil {
		return nil, fmt.Errorf("%w: no active pi profile (add one in the settings first)", ErrTetheringProfile)
	}
	if strings.TrimSpace(p.Ip) == "" || strings.TrimSpace(p.User) == "" {
		return nil, fmt.Errorf("%w: profile %q has no ip/user", ErrTetheringProfile, p.ID)
	}
	return p, nil
}

// StartTethering validates the request (profile + key) and kicks off the
// tethering script in the background. It returns the job id immediately
// (202 at the HTTP layer). A missing key pair is ErrTetheringNoKey (409).
func (s *TetheringService) StartTethering(req TetheringRequest) (string, error) {
	if explicit := strings.TrimSpace(req.ProfileId); explicit != "" {
		if _, err := sanitizeProfileID(explicit); err != nil {
			return "", fmt.Errorf("%w: %v", ErrTetheringProfile, err)
		}
	}
	profile, err := s.resolveTetheringProfile(req.ProfileId)
	if err != nil {
		return "", err
	}
	keyPath, err := KeyPathForProfile(profile.ID)
	if err != nil {
		// cannot happen after resolveTetheringProfile (stored profile ids
		// are sanitized on creation); defensive
		return "", fmt.Errorf("%w: %v", ErrTetheringProfile, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		// the run is key-first by contract: the provisioning wizard
		// (ticket10-3/10-5) must have installed the key pair first
		return "", fmt.Errorf("%w (profile %q)", ErrTetheringNoKey, profile.ID)
	}

	if _, err := os.Stat(s.scriptPath); err != nil {
		return "", fmt.Errorf("tethering script not found at %s (not packaged in this firmware build)", s.scriptPath)
	}

	s.mu.Lock()
	if s.state == tetheringStateRunning {
		s.mu.Unlock()
		return "", ErrTetheringBusy
	}
	jobId := fmt.Sprintf("teth-%d", time.Now().UnixNano())
	s.jobId = jobId
	s.profileID = profile.ID
	s.state = tetheringStateRunning
	s.started = time.Now()
	s.finished = time.Time{}
	s.uplink = ""
	s.tetheringOk = false
	s.internetOk = false
	s.errMsg = ""
	s.logLines = nil
	s.mu.Unlock()

	// host/user only (never the password) reach the log
	s.log.Infof("tethering: starting usb tethering setup for %s (user %s, profile %s) via %s", profile.Ip, profile.User, profile.ID, s.scriptPath)

	go s.run(profile, jobId)
	return jobId, nil
}

func (s *TetheringService) run(profile *PiProfile, jobId string) {
	ctx, cancel := context.WithTimeout(context.Background(), tetheringScriptTimeout)
	defer cancel()

	keyPath, err := KeyPathForProfile(profile.ID)
	if err != nil {
		// cannot happen after StartTethering; defensive
		s.failJob(jobId, "key path: "+err.Error())
		return
	}
	pubKey := ""
	if pub, err := ReadPublicKey(keyPath); err == nil {
		pubKey = pub
	}

	// exec the script directly (rootfs installs it 0755); the password
	// travels only in the environment (run_ssh password fallback), never on
	// the command line and never in the log
	cmd := exec.CommandContext(ctx, s.scriptPath)
	cmd.Env = append(os.Environ(),
		"SSH_HOST="+profile.Ip,
		"SSH_USER="+profile.User,
		"SSH_PASS="+profile.Password,
		"MIRA_SSH_KEY_PATH="+keyPath,
	)
	if pubKey != "" {
		cmd.Env = append(cmd.Env, "MIRA_SSH_PUBKEY="+pubKey)
	}
	s.appendLog(fmt.Sprintf("exec %s (host %s, user %s)", s.scriptPath, profile.Ip, profile.User))

	// stdout and stderr share the log ring so the UI sees one stream
	out := &tetheringLogWriter{svc: s}
	cmd.Stdout = out
	cmd.Stderr = out

	runErr := cmd.Run()

	errMsg := ""
	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			errMsg = fmt.Sprintf("tethering timed out after %s", tetheringScriptTimeout)
		} else {
			errMsg = runErr.Error()
		}
	}

	// the script prints RESULT uplink="..." tethering="..." internet="..."
	// on both the success and the finished-but-failed path
	uplink, tetheringOk, internetOk, _ := parseTetheringResult(s.snapshotLogLines())

	s.mu.Lock()
	// a newer job has already reset the fields; only the job that owns them writes the outcome
	if s.jobId == jobId {
		if errMsg == "" {
			s.state = tetheringStateSuccess
		} else {
			s.state = tetheringStateFailed
			s.errMsg = errMsg
		}
		s.finished = time.Now()
		if uplink != "" {
			s.uplink = uplink
		}
		s.tetheringOk = tetheringOk
		s.internetOk = internetOk
	}
	s.mu.Unlock()

	if errMsg == "" {
		s.log.Infof("tethering: run finished (uplink %q, tethering %v, internet %v)", uplink, tetheringOk, internetOk)
	} else {
		s.log.Warnf("tethering: run failed: %s (uplink %q, tethering %v, internet %v)", errMsg, uplink, tetheringOk, internetOk)
	}
}

// failJob books a failed run (used for the defensive pre-check paths).
func (s *TetheringService) failJob(jobId, errMsg string) {
	s.mu.Lock()
	if s.jobId == jobId {
		s.state = tetheringStateFailed
		s.errMsg = errMsg
		s.finished = time.Now()
	}
	s.mu.Unlock()
	s.log.Warnf("tethering: %s", errMsg)
}

func (s *TetheringService) appendLog(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > tetheringLogCap {
		s.logLines = s.logLines[len(s.logLines)-tetheringLogCap:]
	}
}

func (s *TetheringService) snapshotLogLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := make([]string, len(s.logLines))
	copy(lines, s.logLines)
	return lines
}

// TetheringStatus returns the current job state (the GET /api/pi/tethering/status
// body). Times are UTC RFC3339.
func (s *TetheringService) TetheringStatus() TetheringStatus {
	s.mu.Lock()
	st := TetheringStatus{State: s.state, JobId: s.jobId, ProfileId: s.profileID,
		Uplink: s.uplink, TetheringOk: s.tetheringOk, InternetOk: s.internetOk, Error: s.errMsg}
	if !s.started.IsZero() {
		st.StartedAt = s.started.UTC().Format(time.RFC3339)
	}
	if !s.finished.IsZero() {
		st.FinishedAt = s.finished.UTC().Format(time.RFC3339)
	}
	if len(s.logLines) > 0 {
		tail := s.logLines
		if len(tail) > tetheringLogTailLines {
			tail = tail[len(tail)-tetheringLogTailLines:]
		}
		st.LogTail = make([]string, len(tail))
		copy(st.LogTail, tail)
	}
	s.mu.Unlock()
	return st
}

// tetheringLogWriter funnels the script's combined output into the log ring
type tetheringLogWriter struct {
	svc *TetheringService
}

func (w *tetheringLogWriter) Write(p []byte) (int, error) {
	text := string(p)
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line != "" {
			w.svc.appendLog(line)
		}
	}
	return len(p), nil
}

// tetheringResultPrefix marks the machine-readable line the script prints:
// RESULT uplink="eth" tethering="ok" internet="ok" detail="usb=usb0 dhcp=ok nat=ok rpinet=ok"
const tetheringResultPrefix = "RESULT "

// parseTetheringResult extracts the last RESULT line from the run log.
// tetheringOk/internetOk are true exactly when the script reported "ok".
func parseTetheringResult(lines []string) (uplink string, tetheringOk, internetOk bool, detail string) {
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, tetheringResultPrefix) {
			continue
		}
		rest := line[len(tetheringResultPrefix):]
		up := parseSetupPiQuotedField(rest, "uplink")
		teth := parseSetupPiQuotedField(rest, "tethering")
		net := parseSetupPiQuotedField(rest, "internet")
		detail = parseSetupPiQuotedField(rest, "detail")
		return up, teth == "ok", net == "ok", detail
	}
	return "", false, false, ""
}
