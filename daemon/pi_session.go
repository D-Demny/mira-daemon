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

// epic 10 ticket10-4: after a Mira reboot the key-based SSH connection to
// the configured Raspberry Pi must be restored automatically. PiSession is
// the daemon-side auto-reconnect manager: one long-lived `ssh -N` session
// plus a fixed 10 s retry loop (no backoff), exposed via /api/pi/status.
//
// Key-only by design: the 10 s loop must never re-prompt for a password
// (ticket dependency, ticket10-3). The first password login stays the
// provisioning wizard; after it installed the device key on the Pi, every
// reconnect is BatchMode key auth. RunKeyFirst (sshkey.go) is deliberately
// NOT used here: its sshpass fallback would require the password, which the
// session manager does not carry.
//
// Design decisions (worker review):
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
//   - Config source: the UI settings blob (piServer.ip / piServer.user),
//     re-read on every attempt so an IP changed in the UI takes effect on
//     the next tick. The password in the blob is never read (key-only).
//   - model/tier: read live from the provisioning service (last successful
//     run of this daemon lifetime, in-memory like the rest of the job
//     state); omitted when unknown.
//   - Bounded state: log ring capped like setupPiLogCap; no per-attempt
//     state beyond a counter and the last attempt timestamp. One log line
//     per attempt at the fixed rhythm (no bursts, no spam).
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

// PiStatus is the GET /api/pi/status response (epic 10 ticket10-4). Conn is
// always one of connected|connecting|disconnected; LastAttemptAt is UTC
// RFC3339; Model/Tier come from the last known-good provisioning result and
// are omitted when unknown.
type PiStatus struct {
	Conn          string `json:"conn"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	Model         string `json:"model,omitempty"`
	Tier          string `json:"tier,omitempty"`
}

// PiSessionHandler is the cross-service handler for the /api/pi/* endpoints,
// injected via SetPiSessionHandler on the ApiServer.
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
	// KeyPath is the device-side private key (ticket10-3); empty = KeyPath().
	KeyPath string

	// LoadConfig returns the stored Pi target (ip, user) from the UI
	// settings blob. ok=false = no usable config; the loop stays idle
	// without attempts. Re-read on every attempt.
	LoadConfig func() (ip, user string, ok bool)

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

// PiSession is the SSH auto-reconnect manager. It runs exactly one
// goroutine for its whole lifetime (Start ... Stop) and never spawns one
// per attempt.
type PiSession struct {
	log librespot.Logger

	keyPath    string
	loadConfig func() (ip, user string, ok bool)
	modelTier  func() (model, tier string)
	now        func() time.Time
	retryEvery time.Duration
	newTicker  func(d time.Duration) tickerLike

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	state       string
	lastAttempt time.Time
	attempts    int
	logLines    []string

	startOnce   sync.Once
	stopOnce    sync.Once
	loopStarted atomic.Bool
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// NewPiSession builds the manager. Start begins the reconnect loop; Stop
// ends it.
func NewPiSession(log librespot.Logger, cfg PiSessionConfig) *PiSession {
	keyPath := cfg.KeyPath
	if keyPath == "" {
		keyPath = KeyPath()
	}
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
		load = func() (string, string, bool) { return "", "", false }
	}
	mt := cfg.ModelTier
	if mt == nil {
		mt = func() (string, string) { return "", "" }
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PiSession{
		log:        log,
		keyPath:    keyPath,
		loadConfig: load,
		modelTier:  mt,
		now:        now,
		retryEvery: interval,
		newTicker:  newTicker,
		ctx:        ctx,
		cancel:     cancel,
		state:      piConnDisconnected,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
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

	// immediate first attempt: boot recovery
	s.attempt()

	for {
		select {
		case <-s.stopCh:
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C():
		}
		s.attempt()
	}
}

// attempt runs one (re)connect cycle: config + key check, a key-only probe,
// and - on success - the long-lived session until it dies. It always
// returns; the loop schedules the next attempt on the retry grid.
func (s *PiSession) attempt() {
	ip, user, ok := s.loadConfig()
	if !ok {
		// no stored config: idle without attempts (the user has not run the
		// wizard yet); the check is re-done on every tick
		s.setState(piConnDisconnected)
		return
	}
	if _, err := os.Stat(s.keyPath); err != nil {
		// no device key yet: key-only reconnect means no attempts at all
		// (and no log spam); the provisioning wizard may create the key
		// later in this boot, the next tick picks it up
		s.setState(piConnDisconnected)
		return
	}

	s.mu.Lock()
	s.lastAttempt = s.now()
	s.attempts++
	s.mu.Unlock()
	s.setState(piConnConnecting)

	// phase 1: a fast key-only round-trip (BatchMode can never prompt for a
	// password; ConnectTimeout bounds a dead host)
	if err := s.probe(ip, user); err != nil {
		msg := oneLine(err.Error())
		s.appendLog(fmt.Sprintf("pi: reconnect probe to %s@%s failed: %s", user, ip, msg))
		s.log.Warnf("pi: reconnect probe to %s@%s failed: %s", user, ip, msg)
		s.setState(piConnDisconnected)
		return
	}

	// phase 2: the long-lived session (ssh -N + keepalives). While the
	// process is alive, conn=connected. It dies on a broken link (or
	// Stop); the loop retries on the next retry-grid tick.
	s.setState(piConnConnected)
	if err := s.runSession(ip, user); err != nil {
		// the process could not be started at all (spawn error)
		s.appendLog("pi: starting session failed: " + oneLine(err.Error()))
		s.log.Warnf("pi: starting session failed: %s", oneLine(err.Error()))
		s.setState(piConnDisconnected)
		return
	}
	s.log.Infof("pi: session to %s@%s lost", user, ip)
	s.setState(piConnDisconnected)
}

// probe runs one key-only ssh round-trip (command "true") and reports the
// failure with the ssh output attached.
func (s *PiSession) probe(host, user string) error {
	out, err := runSshKeyWith(s.ctx, s.keyPath, host, user, "true")
	if err == nil {
		return nil
	}
	return fmt.Errorf("%v (%s)", err, oneLine(out))
}

// runSession starts the long-lived `ssh -N` session and blocks until it
// exits. It returns an error only if the process could not be started at
// all; a dead link reports through the process exit, not the return value.
// The session is key-only and BatchMode: it can never prompt for a
// password.
func (s *PiSession) runSession(host, user string) error {
	args := []string{
		"-N",
		"-i", s.keyPath,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", fmt.Sprintf("ConnectTimeout=%d", piSessionConnectTimeout),
		"-o", fmt.Sprintf("ServerAliveInterval=%d", piSessionAliveInterval),
		"-o", fmt.Sprintf("ServerAliveCountMax=%d", piSessionAliveCountMax),
		"-o", "LogLevel=ERROR",
		user + "@" + host,
	}
	cmd := exec.CommandContext(s.ctx, "ssh", args...)
	cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK=")
	// the session only prints errors (LogLevel=ERROR); funnel them into the
	// bounded log ring
	out := &piSessionLogWriter{sess: s}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return err
	}
	// Wait returns when the link dies or on Stop (ctx kill).
	cmd.Wait()
	return nil
}

// PiStatus returns the current session state (the GET /api/pi/status body).
func (s *PiSession) PiStatus() PiStatus {
	s.mu.Lock()
	st := PiStatus{Conn: s.state}
	if !s.lastAttempt.IsZero() {
		st.LastAttemptAt = s.lastAttempt.UTC().Format(time.RFC3339)
	}
	s.mu.Unlock()
	if s.modelTier != nil {
		st.Model, st.Tier = s.modelTier()
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
