package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// epic 10 ticket10-6 (part B): reboot orchestration + persistent
// "reboot started by us" flag + boot-timing tolerance, for an ALREADY
// RECOGNIZED Raspberry Pi (an existing device key per ticket10-3/10-5)
// that is reachable but the Mira has NO internet over USB tethering.
//
// Flow (ticket10-6 constraint 2 + 3):
//  1. Decision (idle phase, 5 s ticks): active profile with an existing
//     device key + the Pi is reachable (session connected OR a key-only
//     probe succeeds) + NO internet for a CONTINUOUS 15 s window ->
//     reboot the Pi EXACTLY ONCE per daemon lifetime (episode).
//  2. The flag is persisted BEFORE `ssh reboot` (hard requirement: the Pi
//     reboot very likely restarts the Mira too - shared USB power - so the
//     flag must survive the Mira's own restart).
//  3. Waiting phase (after the reboot, across the Mira restart): patient
//     window of 10 minutes for the FULL Pi boot. No second reboot is ever
//     issued (the flag suppresses it after the Mira restarts). As soon as
//     the Pi is reachable again the re-run-safe tethering script is
//     re-fired (ticket10-6A: the iptables NAT rules are runtime-only and
//     the reboot wiped them; dnsmasq config + sysctl persist, so a re-run
//     restores the complete tethering). Internet OK -> flag cleared.
//     Window expiry -> timeout warning + flag cleared, no reboot again in
//     this lifetime.
//
// Design decisions (worker review, ticket10-6B):
//   - Internet probe (device side): a plain TCP dial to 1.1.1.1:443 with a
//     3 s timeout (no DNS dependency - a fresh DHCP lease may not have DNS
//     yet; the hardcoded IP keeps the probe working; the same target
//     family the tethering script probes on the RPi side, so "internet"
//     means the same thing on both sides; a connected TCP socket proves
//     route + NAT without a TLS handshake). Rate: one probe per 5 s tick
//     while a recognized target exists, none otherwise. Injectable
//     (CheckInternet) for tests - no fake in production.
//   - 15 s reference point: the window starts at the first observation of
//     "Pi reachable AND no internet" - NOT at the Mira boot. While the Pi
//     is still booting (it boots slower than the Mira) it is unreachable,
//     which must not count toward the 15 s (boot-timing tolerance). The
//     clock resets whenever the Pi becomes unreachable again.
//   - Flag storage: a SEPARATE daemon-owned file
//     /etc/mira/pi_reboot_recovery.json (env override MIRA_RECOVERY_FLAG),
//     NOT the UI settings blob. Rationale: the daemon CAN write the blob
//     (PutSettings), but the UI (settings.ts) serializes its TYPED
//     Settings object (coerce() keeps only known fields) - any UI settings
//     write (debounced updateSettings, e.g. the wizard's credential entry)
//     would silently DROP the unknown piRebootRecovery field, exactly
//     during the flows that matter; a read-modify-write on the blob would
//     also race the UI's debounced PUT. The daemon-owned file has exactly
//     one writer (this service), is written atomically (tmp+rename), and
//     persists across Mira reboots like the /etc/mira/ssh keys. Shape:
//     {"started_at": RFC3339 UTC, "profile_id": "<id>"}.
//   - Flag semantics: FRESH (younger than the patience window) -> a reboot
//     was issued in the previous daemon lifetime -> enter the waiting
//     phase WITHOUT rebooting a second time (the "exactly once" guarantee
//     across the Mira restart). STALE (older than the patience window) ->
//     the previous episode already timed out -> warning + clear + no new
//     reboot in this lifetime (a genuinely broken setup must not become a
//     reboot loop across power cycles; the user re-runs the wizard).
//     PROFILE MISMATCH (flag belongs to another/removed profile) ->
//     warning + clear + the episode is consumed for safety (no new reboot
//     in this lifetime - the flagged Pi was rebooted by us and is no
//     longer our target; a fresh episode for the new target becomes
//     possible after the next daemon restart, when no flag exists).
//   - Tethering re-fire: at most once per waiting episode, and only when
//     the Pi was actually down (sawDown) or the reboot command itself
//     failed (rebootFailed) - the failed-reboot case self-heals by
//     re-running the script while the Pi is still up (the NAT restore
//     often fixes the situation without a reboot at all). A busy
//     tethering job (ErrTetheringBusy) is accepted (a run already in
//     progress covers it).
//   - Status shape (GET /api/pi/status, additive, omitempty):
//     recovery: "rebooting" | "waiting_after_reboot" + recovery_started_at
//     (RFC3339 UTC = the flag's started_at). The UI (ticket10-6C) uses it
//     to suppress the first-run "Setup USB Tethering" onboarding card and
//     to show a waiting state during recovery.
//   - Boot-timing tolerance (constraint 3): the recovery service NEVER
//     concludes "no key" / "wrong password" from early failures - it only
//     acts when the key EXISTS, it carries no password at all, and it
//     resets its 15 s clock on unreachability instead of drawing
//     conclusions. The PiSession (pi_session.go) stays in its patient
//     10 s key-only loop while the flag is set: its loop has no password
//     path and treats the key file read-only, so early SSH failures cannot
//     reset any key/password state; the only states that change are the
//     expected connection states (connecting/disconnected/connected).
//   - Shutdown: Stop() (App.Close) cancels the context (aborts an in-
//     flight probe via the tick context) and waits for the single loop
//     goroutine - same pattern as PiSession. No per-probe goroutines.

// recoveryDefaultFlagPath is where the persistent "reboot started by us"
// flag lives (the firmware build creates /etc/mira for the ssh keys).
const recoveryDefaultFlagPath = "/etc/mira/pi_reboot_recovery.json"

// recoveryFlagEnv overrides the flag file location (tests, custom images).
const recoveryFlagEnv = "MIRA_RECOVERY_FLAG"

const (
	// recoveryTickDefault is the recovery loop rhythm: 5 s (one internet
	// probe + optionally one reachability probe per tick; the 15 s window
	// and the patience window are wall-clock based, not tick counts).
	recoveryTickDefault = 5 * time.Second

	// recoveryNoInternetWindowDefault is the ticket's 15 s: a CONTINUOUS
	// window of "Pi reachable + no internet" before the reboot decision.
	recoveryNoInternetWindowDefault = 15 * time.Second

	// recoveryPatienceDefault is the window to wait for the FULL Pi boot
	// after a reboot we issued: Pi boot (1-2 min, slow images longer) +
	// dnsmasq/DHCP come-up + the tethering re-run (a couple of minutes) -
	// 10 minutes is generous and bounded.
	recoveryPatienceDefault = 10 * time.Minute

	// recoveryRebootTimeout bounds one tick's probes (the reachability
	// probe's ssh has its own 10 s ConnectTimeout, the internet dial 3 s).
	recoveryRebootTimeout = 15 * time.Second

	// recoveryStopTimeout bounds Stop(): the loop only blocks inside
	// ctx-cancelled probes, so this is a safety net.
	recoveryStopTimeout = 5 * time.Second
)

const (
	recoveryInternetHost = "1.1.1.1"
	recoveryInternetPort = "443"

	// recoveryInternetTimeout bounds one TCP dial (3 s, see the file
	// header: no DNS, no TLS - a connected socket is the proof).
	recoveryInternetTimeout = 3 * time.Second
)

const (
	recoveryStateIdle    = "idle"
	recoveryStateReboot  = "rebooting"
	recoveryStateWaiting = "waiting_after_reboot"
)

// recoveryFlag is the on-disk shape of the persistent flag (see the file
// header for the storage decision).
type recoveryFlag struct {
	StartedAt string `json:"started_at"` // RFC3339 UTC
	ProfileId string `json:"profile_id"`
}

// realCheckInternet is the production internet probe: a TCP dial to
// 1.1.1.1:443 (see the file header for the target/timeout rationale).
func realCheckInternet(ctx context.Context) (bool, error) {
	d := net.Dialer{Timeout: recoveryInternetTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(recoveryInternetHost, recoveryInternetPort))
	if err != nil {
		return false, err
	}
	_ = conn.Close()
	return true, nil
}

// PiRecoveryConfig configures the PiRecovery manager.
type PiRecoveryConfig struct {
	// FlagPath of the persistent flag file; empty = env override /
	// default rootfs location.
	FlagPath string

	// LoadSettings returns the UI settings blob (ticket10-5 profile
	// model): the decision resolves the ACTIVE profile from it.
	LoadSettings func() []byte

	// PiReachable reports whether the active Pi is up and reachable via
	// key-only SSH (the session connected OR a fast probe succeeds). The
	// context bounds the probe.
	PiReachable func(ctx context.Context) bool

	// CheckInternet reports whether the Mira itself has internet (the
	// authoritative device-side check). The context bounds the probe.
	CheckInternet func(ctx context.Context) (bool, error)

	// RunTethering re-fires the (re-run-safe) tethering setup
	// (StartTethering with the active profile). nil = no re-fire (the
	// waiting phase still works, it just cannot restore the runtime NAT
	// rules on its own).
	RunTethering func() (jobId string, err error)

	// Now is the clock; defaults to time.Now (tests inject a fake clock).
	Now func() time.Time

	// TickInterval overrides recoveryTickDefault (tests may shorten it).
	TickInterval time.Duration

	// NewTicker builds the loop ticker; defaults to time.NewTicker.
	NewTicker func(d time.Duration) tickerLike

	// NoInternetWindow overrides recoveryNoInternetWindowDefault.
	NoInternetWindow time.Duration

	// PatienceWindow overrides recoveryPatienceDefault.
	PatienceWindow time.Duration
}

// PiRecovery is the reboot-orchestration manager. It runs exactly one
// goroutine for its whole lifetime (Start ... Stop) and never spawns one
// per probe.
type PiRecovery struct {
	log           librespot.Logger
	flagPath      string
	loadSettings  func() []byte
	piReachable   func(ctx context.Context) bool
	checkInternet func(ctx context.Context) (bool, error)
	runTethering  func() (string, error)
	now           func() time.Time
	tickEvery     time.Duration
	newTicker     func(d time.Duration) tickerLike
	noInternetFor time.Duration
	patience      time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	mu              sync.Mutex
	state           string
	flagStartedAt   time.Time
	flagProfileID   string
	rebootIssued    bool // exactly once per daemon lifetime (episode)
	tetheringFired  bool // re-fire at most once per waiting episode
	sawDown         bool // the Pi was down at least once during the wait
	rebootFailed    bool // the reboot command itself failed
	noInternetSince time.Time

	startOnce   sync.Once
	stopOnce    sync.Once
	loopStarted atomic.Bool
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// NewPiRecovery builds the manager (the Start...Stop lifecycle is the
// same pattern as PiSession).
func NewPiRecovery(log librespot.Logger, cfg PiRecoveryConfig) *PiRecovery {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	tick := cfg.TickInterval
	if tick <= 0 {
		tick = recoveryTickDefault
	}
	newTicker := cfg.NewTicker
	if newTicker == nil {
		newTicker = defaultNewTicker
	}
	noNet := cfg.NoInternetWindow
	if noNet <= 0 {
		noNet = recoveryNoInternetWindowDefault
	}
	patience := cfg.PatienceWindow
	if patience <= 0 {
		patience = recoveryPatienceDefault
	}
	check := cfg.CheckInternet
	if check == nil {
		check = realCheckInternet
	}
	reachable := cfg.PiReachable
	if reachable == nil {
		reachable = func(context.Context) bool { return false }
	}
	load := cfg.LoadSettings
	if load == nil {
		load = func() []byte { return nil }
	}
	path := os.Getenv(recoveryFlagEnv)
	if path == "" {
		path = cfg.FlagPath
	}
	if path == "" {
		path = recoveryDefaultFlagPath
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PiRecovery{
		log:           log,
		flagPath:      path,
		loadSettings:  load,
		piReachable:   reachable,
		checkInternet: check,
		runTethering:  cfg.RunTethering,
		now:           now,
		tickEvery:     tick,
		newTicker:     newTicker,
		noInternetFor: noNet,
		patience:      patience,
		ctx:           ctx,
		cancel:        cancel,
		state:         recoveryStateIdle,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
}

// Start runs the boot check (a FRESH flag from a previous lifetime puts
// the manager directly into the waiting phase - NO second reboot; a stale
// flag is cleared with a warning) and begins the loop.
func (r *PiRecovery) Start() {
	r.startOnce.Do(func() {
		r.bootCheck()
		go r.loop()
	})
}

// Stop shuts the manager down (App.Close): it cancels the context (which
// aborts an in-flight probe) and waits for the loop goroutine to exit.
// Safe to call twice and after Start was never called.
func (r *PiRecovery) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
		r.cancel()
	})
	if !r.loopStarted.Load() {
		return
	}
	select {
	case <-r.doneCh:
	case <-time.After(recoveryStopTimeout):
		r.log.Warnf("recovery: loop did not exit within %s", recoveryStopTimeout)
	}
}

// Status reports the recovery state for the /api/pi/status extension:
// "rebooting" or "waiting_after_reboot" (empty = idle) plus the flag's
// started_at (zero when idle).
func (r *PiRecovery) Status() (string, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state {
	case recoveryStateReboot:
		return "rebooting", r.flagStartedAt
	case recoveryStateWaiting:
		return recoveryStateWaiting, r.flagStartedAt
	default:
		return "", time.Time{}
	}
}

func (r *PiRecovery) loop() {
	r.loopStarted.Store(true)
	defer close(r.doneCh)

	ticker := r.newTicker(r.tickEvery)
	defer ticker.Stop()

	for {
		r.tickOnce()
		select {
		case <-r.stopCh:
			return
		case <-r.ctx.Done():
			return
		case <-ticker.C():
		}
	}
}

// bootCheck handles the flag that survived the Mira's own restart (the
// core of the "exactly once" guarantee, see the file header).
func (r *PiRecovery) bootCheck() {
	flag, err := r.loadFlag()
	if err != nil {
		r.log.Warnf("recovery: reading the flag file: %v (starting without it)", err)
		return
	}
	if flag == nil {
		return
	}
	started, perr := time.Parse(time.RFC3339, flag.StartedAt)
	if perr != nil {
		r.log.Warnf("recovery: flag file has an invalid timestamp %q - clearing it", flag.StartedAt)
		r.clearFlag()
		return
	}
	if age := r.now().Sub(started); age > r.patience {
		// stale: the previous episode already timed out -> warn + clear +
		// consume the episode (no new reboot in this lifetime)
		r.log.Warnf("recovery: stale flag (reboot issued %s, age %s > patience %s) - the previous episode timed out, no new reboot",
			started.UTC().Format(time.RFC3339), age.Round(time.Second), r.patience)
		r.clearFlag()
		r.mu.Lock()
		r.rebootIssued = true
		r.mu.Unlock()
		return
	}
	p := ResolveActiveProfile(r.loadSettings())
	activeID := ""
	if p != nil {
		activeID = p.ID
	}
	if activeID == "" || activeID != flag.ProfileId {
		// the flag belongs to another (or no longer existing) profile:
		// irrelevant - but the episode is consumed for safety (the flagged
		// Pi was rebooted by us and is no longer our target)
		r.log.Warnf("recovery: flag is for profile %q but the active profile is %q - clearing it", flag.ProfileId, activeID)
		r.clearFlag()
		r.mu.Lock()
		r.rebootIssued = true
		r.mu.Unlock()
		return
	}
	// FRESH flag: a reboot was issued in the previous daemon lifetime ->
	// wait for the full Pi boot, NEVER reboot a second time
	r.mu.Lock()
	r.state = recoveryStateWaiting
	r.flagStartedAt = started
	r.flagProfileID = flag.ProfileId
	r.rebootIssued = true
	r.mu.Unlock()
	r.log.Infof("recovery: resuming after our own rpi reboot (flag from %s, profile %s) - waiting for the full rpi boot",
		started.UTC().Format(time.RFC3339), flag.ProfileId)
}

func (r *PiRecovery) tickOnce() {
	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	if state == recoveryStateWaiting || state == recoveryStateReboot {
		r.waitTick()
		return
	}
	r.idleTick()
}

// idleTick runs the reboot decision (see the file header, flow step 1).
func (r *PiRecovery) idleTick() {
	r.mu.Lock()
	issued := r.rebootIssued
	r.mu.Unlock()
	if issued {
		// exactly once per daemon lifetime (episode)
		return
	}
	p := ResolveActiveProfile(r.loadSettings())
	if p == nil {
		return
	}
	keyPath, err := KeyPathForProfile(p.ID)
	if err != nil {
		return
	}
	if _, err := os.Stat(keyPath); err != nil {
		// NOT recognized (no device key): the onboarding wizard's
		// territory (ticket10-6 part C) - never reboot an unknown Pi
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, recoveryRebootTimeout)
	defer cancel()
	if !r.piReachable(ctx) {
		// the rpi is not (yet) reachable - boot-timing tolerance: it boots
		// slower than the mira, which is expected. The 15 s window (re)
		// starts only once the rpi is actually reachable; no conclusions
		// are drawn from the early failures
		r.mu.Lock()
		r.noInternetSince = time.Time{}
		r.mu.Unlock()
		return
	}
	if netOK, _ := r.checkInternet(ctx); netOK {
		r.mu.Lock()
		r.noInternetSince = time.Time{}
		r.mu.Unlock()
		return
	}
	now := r.now()
	r.mu.Lock()
	since := r.noInternetSince
	r.mu.Unlock()
	if since.IsZero() {
		// first "reachable + no internet" observation: start the 15 s
		// window (NOT at the mira boot, see the file header)
		r.mu.Lock()
		r.noInternetSince = now
		r.mu.Unlock()
		return
	}
	if now.Sub(since) < r.noInternetFor {
		return
	}
	r.issueReboot(p, keyPath, now)
}

// issueReboot executes the exactly-once reboot: the persistent flag is
// written FIRST (before the reboot command - hard requirement), then the
// key-only `ssh reboot` is issued (the remote command returns before the
// machine goes down), then the waiting phase starts.
func (r *PiRecovery) issueReboot(p *PiProfile, keyPath string, now time.Time) {
	// 1) persist the flag BEFORE the reboot: the rpi reboot almost
	// certainly restarts the mira too (shared usb power) - the flag must
	// survive that restart or the mira would reboot the rpi a second time
	if err := r.saveFlag(now, p.ID); err != nil {
		r.log.Warnf("recovery: persisting the reboot flag failed: %v - NOT rebooting (retrying on the next tick)", err)
		return
	}
	// 2) the reboot (key-only, BatchMode: no password is ever involved)
	r.mu.Lock()
	r.state = recoveryStateReboot
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.ctx, recoveryRebootTimeout)
	out, err := runSshKeyWith(ctx, keyPath, p.Ip, p.User, "reboot")
	cancel()
	if err != nil {
		// the command could not be delivered (or the link dropped early):
		// the flag is persisted, so the waiting phase still covers this
		// (the tethering re-fire below may repair the situation without a
		// reboot at all)
		r.mu.Lock()
		r.rebootFailed = true
		r.mu.Unlock()
		r.log.Warnf("recovery: reboot command to %s@%s (profile %s) failed: %s", p.User, p.Ip, p.ID, oneLine(err.Error()+" | "+out))
	} else {
		r.log.Infof("recovery: rpi reboot issued for profile %s (flag persisted at %s)", p.ID, now.UTC().Format(time.RFC3339))
	}
	// 3) wait for the full rpi boot (patience window)
	r.mu.Lock()
	r.state = recoveryStateWaiting
	r.flagStartedAt = now
	r.flagProfileID = p.ID
	r.rebootIssued = true
	r.tetheringFired = false
	r.sawDown = false
	r.noInternetSince = time.Time{}
	r.mu.Unlock()
}

// waitTick runs the patient waiting phase after a reboot we issued (see
// the file header, flow step 3): timeout check, internet check, and the
// one-time tethering re-fire once the Pi is back.
func (r *PiRecovery) waitTick() {
	r.mu.Lock()
	startedAt := r.flagStartedAt
	r.mu.Unlock()
	now := r.now()
	if now.Sub(startedAt) > r.patience {
		r.log.Warnf("recovery: timed out after %s waiting for the rpi boot + tethering internet (flag from %s) - giving up, flag cleared",
			r.patience, startedAt.UTC().Format(time.RFC3339))
		r.clearFlag()
		r.mu.Lock()
		r.state = recoveryStateIdle
		r.flagStartedAt = time.Time{}
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, recoveryRebootTimeout)
	defer cancel()
	if netOK, _ := r.checkInternet(ctx); netOK {
		r.log.Infof("recovery: internet restored after the rpi boot (profile %s) - clearing the reboot flag", r.flagProfileID)
		r.clearFlag()
		r.mu.Lock()
		r.state = recoveryStateIdle
		r.flagStartedAt = time.Time{}
		r.mu.Unlock()
		return
	}
	reachable := r.piReachable(ctx)
	r.mu.Lock()
	if !reachable {
		// still booting: keep waiting (the pi is slower than the mira)
		r.sawDown = true
	}
	fired := r.tetheringFired
	fail := r.rebootFailed
	down := r.sawDown
	r.mu.Unlock()
	if !fired && reachable && (down || fail) {
		// the rpi is back (or never went down because the reboot command
		// failed): its iptables NAT rules are runtime-only (ticket10-6A)
		// - re-fire the re-run-safe tethering setup to restore them
		r.mu.Lock()
		r.tetheringFired = true
		r.mu.Unlock()
		r.log.Infof("recovery: rpi reachable again (profile %s) - re-running the usb tethering setup (nat restore)", r.flagProfileID)
		if r.runTethering == nil {
			return
		}
		if jobId, err := r.runTethering(); err != nil {
			if errors.Is(err, ErrTetheringBusy) {
				r.log.Infof("recovery: a tethering run is already in progress (job %s) - waiting for its result", jobId)
			} else {
				r.log.Warnf("recovery: re-running the tethering setup failed: %v (the internet check keeps running)", err)
			}
		}
	}
}

// loadFlag reads the persistent flag; nil = absent (or empty/corrupt in a
// way that leaves no usable fields).
func (r *PiRecovery) loadFlag() (*recoveryFlag, error) {
	b, err := os.ReadFile(r.flagPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f recoveryFlag
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if strings.TrimSpace(f.StartedAt) == "" || strings.TrimSpace(f.ProfileId) == "" {
		return nil, nil
	}
	return &f, nil
}

// saveFlag atomically persists the flag (tmp file + rename in the same
// directory).
func (r *PiRecovery) saveFlag(startedAt time.Time, profileID string) error {
	if err := os.MkdirAll(filepath.Dir(r.flagPath), 0o700); err != nil {
		return fmt.Errorf("creating the flag directory: %w", err)
	}
	b, err := json.Marshal(recoveryFlag{
		StartedAt: startedAt.UTC().Format(time.RFC3339),
		ProfileId: profileID,
	})
	if err != nil {
		return err
	}
	tmp := r.flagPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("writing the flag file: %w", err)
	}
	if err := os.Rename(tmp, r.flagPath); err != nil {
		return fmt.Errorf("committing the flag file: %w", err)
	}
	return nil
}

// clearFlag removes the persistent flag (best effort).
func (r *PiRecovery) clearFlag() {
	if err := os.Remove(r.flagPath); err != nil && !os.IsNotExist(err) {
		r.log.Warnf("recovery: removing the flag file: %v", err)
	}
}
