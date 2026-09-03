package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// epic 10 ticket10-4 + ticket10-5: after a Mira reboot the key-based SSH
// connection to the ACTIVE Raspberry Pi profile must be restored
// automatically. PiSession is the daemon-side auto-reconnect manager: one
// long-lived `ssh -N` session plus a fixed 10 s retry loop (no backoff),
// bound to the active profile of the UI settings blob (ticket10-5) and
// exposed via /api/pi/status.
//
// Key-only by design: the 10 s loop must never re-prompt for a password
// (ticket dependency, ticket10-3). The first password login stays the
// provisioning wizard; after it installed the device key on the Pi, every
// reconnect is BatchMode key auth. RunKeyFirst (sshkey.go) is deliberately
// NOT used here: its sshpass fallback would require the password, which the
// session manager does not carry.
//
// Design decisions (worker review, ticket10-4 + ticket10-5B):
//   - Connection mechanics: a long-lived `ssh -N` session with
//     ServerAliveInterval/CountMax keepalives ("connected" = the session is
//     up), not a periodic health check. Each (re)connect is two-phase: a
//     fast key-only probe round-trip (same code path as the ticket10-3 key
//     check) decides the attempt, then the long-lived session is started.
//     The probe runs once per boot/reconnect, never periodically.
//   - Retry: a single loop goroutine, one attempt per retry-grid tick
//     (fixed 10 s, no backoff). The first attempt is immediate on Start
//     (boot recovery). No goroutine per attempt => a single in-flight
//     attempt is guaranteed by construction (no stacking).
//   - No key / no config: the loop stays idle WITHOUT attempts (no SSH
//     process, no log lines); config and key are re-checked on every tick,
//     so a key generated later (provisioning wizard finishing in this boot)
//     is picked up automatically.
//   - Config source: the UI settings blob (ticket10-5 profile model:
//     piProfiles[] + activePiId, the legacy piServer shape resolves to the
//     implicit "legacy" profile - see pi_profiles.go), re-read on every
//     attempt so a profile change in the UI takes effect on the next tick.
//     The password in the blob is never read (key-only).
//   - Profile switch (ticket10-5): while a session is alive, every grid
//     tick re-resolves the active profile; a changed target (different
//     profile id, ip, user, or per-profile key path) kills the running
//     session and the loop IMMEDIATELY reconnects to the new target (no
//     extra 10 s wait). The switch is detected through the per-tick config
//     re-read - no separate watcher.
//   - Per-profile key (ticket10-5): the session always uses the key pair
//     of the active profile (KeyPathForProfile); the legacy profile keeps
//     the original id_ed25519.
//   - model/tier: read live from the provisioning service (last successful
//     run of this daemon lifetime, in-memory like the rest of the job
//     state); omitted when unknown.
//   - Bounded state: log ring capped like setupPiLogCap; no per-attempt
//     state beyond a counter, the last attempt timestamp, and the bound
//     profile identity. One log line per attempt at the fixed rhythm (no
//     bursts, no spam).
//   - Status (ticket10-5): /api/pi/status carries the profile the session
//     targets (profile_id) and the per-profile key existence
//     (profiles[].key_installed = device-side key file presence). Conn
//     describes the session of the ACTIVE profile only; the other profiles
//     have no live session state.
//   - Shutdown: Stop() cancels the loop context (killing a running ssh
//     session via exec.CommandContext) and waits for the loop goroutine.

const (
	// piSessionRetryInterval is the fixed reconnect rhythm: exactly one
	// attempt every 10 s, intentionally NO exponential backoff (ticket).
	piSessionRetryInterval = 10 * time.Second

	// piSessionConnectTimeout bounds one probe or session handshake.
	piSessionConnectTimeout = 10

	// piSessionAliveInterval/CountMax: a dead link (cable pulled, Pi off)
	// reports through the ssh keepalives within ~30 s.
	piSessionAliveInterval = 10
	piSessionAliveCountMax = 3

	// piSessionLogCap keeps the in-memory log ring bounded (same pattern as
	// setupPiLogCap): a 24 h outage must not grow unbounded state.
	piSessionLogCap = 100

	// piSessionStopTimeout bounds Stop(): the loop only blocks inside
	// ctx-cancelled ssh machinery, so this is a safety net.
	piSessionStopTimeout = 5 * time.Second
)

const (
	piConnDisconnected = "disconnected"
	piConnConnecting   = "connecting"
	piConnConnected    = "connected"
)

// PiProfileStatus is one entry of the per-profile list in /api/pi/status
// (ticket10-5): which profiles are stored and whether each one's
// device-side key pair exists (the UI renders "SSH-Key installiert" vs
// "Passwort-Login erforderlich" per profile).
type PiProfileStatus struct {
	ID           string `json:"id"`
	KeyInstalled bool   `json:"key_installed"`
}

// PiStatus is the GET /api/pi/status response (epic 10 ticket10-4, profile
// context ticket10-5). Conn is always one of connected|connecting|
// disconnected and describes the session bound to the ACTIVE profile
// (ProfileId); LastAttemptAt is UTC RFC3339; Model/Tier come from the last
// known-good provisioning result and are omitted when unknown; Profiles
// carries the per-profile key existence for all stored profiles (omitted
// when none are stored).
type PiStatus struct {
	Conn          string            `json:"conn"`
	LastAttemptAt string            `json:"last_attempt_at,omitempty"`
	Model         string            `json:"model,omitempty"`
	Tier          string            `json:"tier,omitempty"`
	ProfileId     string            `json:"profile_id,omitempty"`
	Profiles      []PiProfileStatus `json:"profiles,omitempty"`
}

// PiSessionHandler is the cross-service handler for the /api/pi/status
// endpoint, injected via SetPiSessionHandler on the ApiServer.
type PiSessionHandler interface {
	PiStatus() PiStatus
}

// tickerLike is the subset of time.Ticker the loop needs. The production
// default is a realTicker; tests inject a fake to drive the retry rhythm
// deterministically (fake clock).
type tickerLike interface {
	C() <-chan time.Time
	Stop()
}

// realTicker adapts *time.Ticker (whose C is a field, not a method) to
// tickerLike.
type realTicker struct {
	*time.Ticker
}

func (r *realTicker) C() <-chan time.Time { return r.Ticker.C }

func defaultNewTicker(d time.Duration) tickerLike {
	return &realTicker{time.NewTicker(d)}
}

// PiSessionConfig configures the PiSession manager.
type PiSessionConfig struct {
	// LoadConfig returns the stored ACTIVE profile target (id, ip, user)
	// from the UI settings blob (ticket10-5 profile model; the legacy
	// piServer shape resolves to the implicit "legacy" profile). ok=false =
	// no usable config; the loop stays idle without attempts. Re-read on
	// every attempt.
	LoadConfig func() (id, ip, user string, ok bool)

	// LoadProfiles returns all stored profiles (ticket10-5): the status
	// reports the per-profile key existence from it. nil = none reported.
	LoadProfiles func() []PiProfile

	// ModelTier returns the last known-good model/tier (provisioning
	// result); may return empty.
	ModelTier func() (model, tier string)

	// Now is the clock; defaults to time.Now (tests inject a fake clock).
	Now func() time.Time

	// RetryInterval overrides piSessionRetryInterval (tests may shorten it).
	RetryInterval time.Duration

	// NewTicker builds the retry ticker; defaults to time.NewTicker.
	NewTicker func(d time.Duration) tickerLike
}

// piIdentity identifies the profile a session is bound to: a change of any
// part (profile id, ip, user, per-profile key path) means the stored
// configuration moved to a different target (ticket10-5 profile switch).
type piIdentity struct {
	id      string
	ip      string
	user    string
	keyPath string
}

func (p piIdentity) same(o piIdentity) bool {
	return p.id == o.id && p.ip == o.ip && p.user == o.user && p.keyPath == o.keyPath
}

// PiSession is the SSH auto-reconnect manager. It runs exactly one
// goroutine for its whole lifetime (Start ... Stop) and never spawns one
// per attempt.
type PiSession struct {
	log librespot.Logger

	loadConfig   func() (id, ip, user string, ok bool)
	loadProfiles func() []PiProfile
	modelTier    func() (model, tier string)
	now          func() time.Time
	retryEvery   time.Duration
	newTicker    func(d time.Duration) tickerLike

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	state       string
	targetID    string // last resolved active profile (status: profile_id)
	lastBadID   string // throttled warning for unusable stored profile ids
	lastAttempt time.Time
	attempts    int
	logLines    []string

	// the running long-lived session (nil = none)
	sess       *exec.Cmd
	sessDone   <-chan struct{}
	sessCancel context.CancelFunc
	bound      piIdentity

	startOnce   sync.Once
	stopOnce    sync.Once
	loopStarted atomic.Bool
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// NewPiSession builds the manager. Start begins the reconnect loop; Stop
// ends it.
func NewPiSession(log librespot.Logger, cfg PiSessionConfig) *PiSession {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	interval := cfg.RetryInterval
	if interval <= 0 {
		interval = piSessionRetryInterval
	}
	newTicker := cfg.NewTicker
	if newTicker == nil {
		newTicker = defaultNewTicker
	}
	load := cfg.LoadConfig
	if load == nil {
		load = func() (string, string, string, bool) { return "", "", "", false }
	}
	profiles := cfg.LoadProfiles
	if profiles == nil {
		profiles = func() []PiProfile { return nil }
	}
	mt := cfg.ModelTier
	if mt == nil {
		mt = func() (string, string) { return "", "" }
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PiSession{
		log:          log,
		loadConfig:   load,
		loadProfiles: profiles,
		modelTier:    mt,
		now:          now,
		retryEvery:   interval,
		newTicker:    newTicker,
		ctx:          ctx,
		cancel:       cancel,
		state:        piConnDisconnected,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
}

// Start begins the reconnect loop. It returns immediately; the first attempt
// happens right away (boot recovery), then one attempt per retry interval.
// A missing key or config keeps the session idle without attempts; both are
// re-checked on every tick.
func (s *PiSession) Start() {
	s.startOnce.Do(func() { go s.loop() })
}

// Stop shuts the manager down: it ends the loop and kills a running ssh
// session (context cancel), then waits for the loop goroutine to exit.
// Safe to call twice and after Start was never called.
func (s *PiSession) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.cancel()
	})
	if !s.loopStarted.Load() {
		return
	}
	select {
	case <-s.doneCh:
	case <-time.After(piSessionStopTimeout):
		s.log.Warnf("pi: session loop did not exit within %s", piSessionStopTimeout)
	}
}

func (s *PiSession) loop() {
	s.loopStarted.Store(true)
	defer close(s.doneCh)

	ticker := s.newTicker(s.retryEvery)
	defer ticker.Stop()

	for {
		if s.attempt() {
			switched := s.watchSession(ticker)
			s.finishSession(switched)
			if !switched {
				// the link died on its own: the next attempt is on the grid
				select {
				case <-s.stopCh:
					return
				case <-s.ctx.Done():
					return
				case <-ticker.C():
				}
			}
			// profile switch (ticket10-5): fall through to the next
			// iteration = an immediate attempt for the new target
			continue
		}
		select {
		case <-s.stopCh:
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C():
		}
	}
}

// attempt runs one (re)connect cycle for the currently stored active
// profile (ticket10-5). It returns true when a long-lived session is now
// running (the loop must watch it) and false when the session is idle
// after the attempt.
func (s *PiSession) attempt() bool {
	identity, ok := s.resolveTarget()
	if !ok {
		// no stored config (fresh install, profile removed): any running
		// session must die; the check is re-done on every tick
		s.killSession()
		s.setState(piConnDisconnected)
		return false
	}

	s.mu.Lock()
	running := s.sess != nil
	bound := s.bound
	s.mu.Unlock()

	if running {
		if !bound.same(identity) {
			// profile switch (ticket10-5): the stored configuration moved
			// to another target - disconnect the old session first
			s.log.Infof("pi: active profile changed (%q -> %q), disconnecting session", bound.id, identity.id)
			s.killSession()
			s.setState(piConnDisconnected)
		} else {
			// already bound and connected: nothing to do (defensive - the
			// loop only calls attempt with no session running)
			return true
		}
	}

	if _, err := os.Stat(identity.keyPath); err != nil {
		// no device key for this profile yet: key-only reconnect means no
		// attempts (and no log spam); the provisioning wizard may create
		// the key later in this boot, the next tick picks it up
		s.setState(piConnDisconnected)
		return false
	}

	s.mu.Lock()
	s.lastAttempt = s.now()
	s.attempts++
	s.mu.Unlock()
	s.setState(piConnConnecting)

	// phase 1: a fast key-only round-trip with the profile's own key
	// (BatchMode can never prompt for a password; ConnectTimeout bounds a
	// dead host)
	if err := s.probe(identity); err != nil {
		msg := oneLine(err.Error())
		s.appendLog(fmt.Sprintf("pi: reconnect probe to %s@%s (profile %s) failed: %s", identity.user, identity.ip, identity.id, msg))
		s.log.Warnf("pi: reconnect probe to %s@%s (profile %s) failed: %s", identity.user, identity.ip, identity.id, msg)
		s.setState(piConnDisconnected)
		return false
	}

	// phase 2: the long-lived session (ssh -N + keepalives). While the
	// process is alive, conn=connected. The loop watches it: it dies on a
	// broken link (retry on the next grid tick) or is killed on a profile
	// switch (immediate reconnect to the new target).
	s.setState(piConnConnected)
	cmd, done, cancel, err := s.startSession(identity)
	if err != nil {
		// the process could not be started at all (spawn error)
		s.appendLog("pi: starting session failed: " + oneLine(err.Error()))
		s.log.Warnf("pi: starting session failed: %s", oneLine(err.Error()))
		s.setState(piConnDisconnected)
		return false
	}
	s.mu.Lock()
	s.sess = cmd
	s.sessDone = done
	s.sessCancel = cancel
	s.bound = identity
	s.mu.Unlock()
	return true
}

// watchSession blocks while the long-lived session is alive. On every grid
// tick it re-reads the stored configuration (ticket10-5): when the active
// profile changed (different profile, ip, user, or per-profile key path),
// the session is killed so the loop can immediately reconnect to the new
// target. It returns true when the session ended because of a profile
// switch.
func (s *PiSession) watchSession(ticker tickerLike) bool {
	for {
		s.mu.Lock()
		done := s.sessDone
		bound := s.bound
		s.mu.Unlock()
		if done == nil {
			return false
		}

		select {
		case <-s.stopCh:
			s.killSession()
			return false
		case <-s.ctx.Done():
			return false
		case <-done:
			return false
		case <-ticker.C():
			identity, ok := s.resolveTarget()
			if !ok || !identity.same(bound) {
				s.log.Infof("pi: active profile changed while connected (was %s@%s, profile %q) - switching", bound.user, bound.ip, bound.id)
				s.killSession()
				return true
			}
		}
	}
}

// finishSession books the end of a long-lived session: it clears the
// session state, logs the transition, and drops the state to disconnected.
func (s *PiSession) finishSession(switched bool) {
	s.mu.Lock()
	bound := s.bound
	s.sess = nil
	s.sessDone = nil
	if s.sessCancel != nil {
		s.sessCancel()
		s.sessCancel = nil
	}
	s.bound = piIdentity{}
	s.mu.Unlock()
	if switched {
		s.log.Infof("pi: session to %s@%s (profile %q) disconnected for the profile switch", bound.user, bound.ip, bound.id)
	} else {
		s.log.Infof("pi: session to %s@%s (profile %q) lost", bound.user, bound.ip, bound.id)
	}
	s.setState(piConnDisconnected)
}

// killSession ends a running long-lived session (Stop, config gone, or
// profile switch) and waits for the process to exit.
func (s *PiSession) killSession() {
	s.mu.Lock()
	if s.sess == nil {
		s.mu.Unlock()
		return
	}
	cancel := s.sessCancel
	done := s.sessDone
	s.sess = nil
	s.sessDone = nil
	s.sessCancel = nil
	s.bound = piIdentity{}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(piSessionStopTimeout):
			s.log.Warnf("pi: session did not exit within %s", piSessionStopTimeout)
		}
	}
}

// resolveTarget reads the stored configuration for this attempt (fresh on
// every tick) and resolves it to a connect target: the active profile's
// identity plus its per-profile key path. ok=false = no usable target: no
// profiles (fresh install), an empty ip/user, or a stored profile id that
// cannot be turned into a safe key path (stays idle; a throttled warning
// is logged).
func (s *PiSession) resolveTarget() (piIdentity, bool) {
	id, ip, user, found := s.loadConfig()
	if !found || ip == "" || user == "" {
		s.setTargetID("")
		return piIdentity{}, false
	}
	keyPath, err := KeyPathForProfile(id)
	if err != nil {
		s.setTargetID("")
		s.mu.Lock()
		if s.lastBadID != id {
			s.lastBadID = id
			s.mu.Unlock()
			s.log.Warnf("pi: stored profile id %q is not usable as a key path: %v - session idle", id, err)
			s.mu.Lock()
		} else {
			s.mu.Unlock()
		}
		return piIdentity{}, false
	}
	s.setTargetID(id)
	return piIdentity{id: id, ip: ip, user: user, keyPath: keyPath}, true
}

// setTargetID books the last resolved active profile for the status.
func (s *PiSession) setTargetID(id string) {
	s.mu.Lock()
	s.targetID = id
	if id != "" {
		s.lastBadID = ""
	}
	s.mu.Unlock()
}

// probe runs one key-only ssh round-trip (command "true") with the
// target's per-profile key and reports the failure with the ssh output
// attached.
func (s *PiSession) probe(identity piIdentity) error {
	out, err := runSshKeyWith(s.ctx, identity.keyPath, identity.ip, identity.user, "true")
	if err == nil {
		return nil
	}
	return fmt.Errorf("%v (%s)", err, oneLine(out))
}

// startSession starts the long-lived `ssh -N` session for the target and
// returns the process, a done channel (closed when the process exits) and
// the context cancel. It returns an error only if the process could not be
// started at all; a dead link reports through the process exit, not the
// return value. The session is key-only (the per-profile key) and BatchMode:
// it can never prompt for a password.
func (s *PiSession) startSession(identity piIdentity) (*exec.Cmd, <-chan struct{}, context.CancelFunc, error) {
	args := []string{
		"-N",
		"-i", identity.keyPath,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", fmt.Sprintf("ConnectTimeout=%d", piSessionConnectTimeout),
		"-o", fmt.Sprintf("ServerAliveInterval=%d", piSessionAliveInterval),
		"-o", fmt.Sprintf("ServerAliveCountMax=%d", piSessionAliveCountMax),
		"-o", "LogLevel=ERROR",
		identity.user + "@" + identity.ip,
	}
	ctx, cancel := context.WithCancel(s.ctx)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK=")
	// the session only prints errors (LogLevel=ERROR); funnel them into the
	// bounded log ring
	out := &piSessionLogWriter{sess: s}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	return cmd, done, cancel, nil
}

// PiStatus returns the current session state (the GET /api/pi/status body).
// Conn describes the session of the ACTIVE profile (ProfileId); the other
// profiles have no live session state, only the per-profile key existence
// (Profiles, ticket10-5).
func (s *PiSession) PiStatus() PiStatus {
	s.mu.Lock()
	st := PiStatus{Conn: s.state, ProfileId: s.targetID}
	if !s.lastAttempt.IsZero() {
		st.LastAttemptAt = s.lastAttempt.UTC().Format(time.RFC3339)
	}
	s.mu.Unlock()
	if s.modelTier != nil {
		st.Model, st.Tier = s.modelTier()
	}
	for _, p := range s.loadProfiles() {
		if strings.TrimSpace(p.ID) == "" {
			continue
		}
		path, err := KeyPathForProfile(p.ID)
		if err != nil {
			continue
		}
		installed := false
		if _, err := os.Stat(path); err == nil {
			installed = true
		}
		st.Profiles = append(st.Profiles, PiProfileStatus{ID: p.ID, KeyInstalled: installed})
	}
	return st
}

// setState flips the state, logging the transition. Repeats are silent so a
// long outage does not spam the log.
func (s *PiSession) setState(state string) {
	s.mu.Lock()
	if s.state == state {
		s.mu.Unlock()
		return
	}
	s.state = state
	s.mu.Unlock()
	s.log.Infof("pi: session state: %s", state)
}

// appendLog appends to the bounded in-memory log ring (pattern: setup_pi).
func (s *PiSession) appendLog(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > piSessionLogCap {
		s.logLines = s.logLines[len(s.logLines)-piSessionLogCap:]
	}
}

// oneLine flattens multi-line errors/outputs into a single log line.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " | ")
}

// piSessionLogWriter funnels the session's stderr into the bounded ring
type piSessionLogWriter struct {
	sess *PiSession
}

func (w *piSessionLogWriter) Write(p []byte) (int, error) {
	text := string(p)
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line != "" {
			w.sess.appendLog(line)
		}
	}
	return len(p), nil
}
