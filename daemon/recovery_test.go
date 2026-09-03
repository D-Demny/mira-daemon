package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// epic 10 ticket10-6 (part B): PiRecovery (reboot orchestration +
// persistent "reboot started by us" flag + boot-timing tolerance).
// Fake SSH: a test-local fake that records, at the moment of the
// `ssh reboot` call, whether the flag file already exists (the ticket's
// hard requirement: the flag must be persisted BEFORE the reboot). Fake
// internet: the injectable CheckInternet (a bool toggle). Fake clock +
// the shared fakeTicker from pi_session_test.go: the 15 s window and the
// 10 min patience window are driven deterministically, no real waiting.
// No t.Parallel anywhere (t.Setenv).

// recoveryRebootFakeSSH records, for every `reboot` call, whether the
// recovery flag file exists AT THE TIME OF THE CALL (one
// "flag=present|absent" line per call into RECOVERY_REBOOT_LOG); any
// other command behaves like the standard key-auth fake (sshkey_test.go).
const recoveryRebootFakeSSH = `#!/bin/sh
key_mode=0
for a in "$@"; do
    [ "$a" = "-i" ] && key_mode=1
done
cmd=""
for a in "$@"; do
    cmd="$a"
done
if [ "$cmd" = "reboot" ]; then
    if [ -f "${RECOVERY_FLAG:-/nonexistent-mira-recovery-flag}" ]; then
        echo "flag=present" >> "${RECOVERY_REBOOT_LOG:-/dev/null}"
    else
        echo "flag=absent" >> "${RECOVERY_REBOOT_LOG:-/dev/null}"
    fi
    [ "${FAKE_SSH_KEY_OK:-0}" = 1 ] || exit 255
    exit 0
fi
if [ "$key_mode" = 1 ] && [ "${FAKE_SSH_KEY_OK:-0}" != 1 ]; then
    printf 'Permission denied (publickey).\n' >&2
    exit 255
fi
exit 0
`

// installRecoveryRebootFakeSSH prepends the reboot-recording fake ssh to
// PATH and points it at the flag file. It returns the reboot log path
// (one line per `ssh reboot` call). t.Setenv => no t.Parallel.
func installRecoveryRebootFakeSSH(t *testing.T, flagPath string) (rebootLog string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(recoveryRebootFakeSSH), 0o755); err != nil {
		t.Fatalf("writing fake ssh: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RECOVERY_FLAG", flagPath)
	rebootLog = filepath.Join(t.TempDir(), "reboot-calls")
	t.Setenv("RECOVERY_REBOOT_LOG", rebootLog)
	return rebootLog
}

func recoveryRebootLogLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading the reboot log: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// recoveryLoopGoroutines counts live PiRecovery.loop goroutines on the
// stacks - exact, unlike a raw NumGoroutine total (which wobbles with
// transient runtime goroutines).
func recoveryLoopGoroutines() int {
	b := make([]byte, 1024*1024)
	n := runtime.Stack(b, true)
	return strings.Count(string(b[:n]), "daemon.(*PiRecovery).loop")
}

// recoveryProfilesBlobJSON builds a v2 UI settings blob (ticket10-5 shape).
func recoveryProfilesBlobJSON(active string, profiles ...PiProfile) []byte {
	blob := map[string]any{"v": 2, "piProfiles": profiles}
	if active != "" {
		blob["activePiId"] = active
	}
	b, err := json.Marshal(blob)
	if err != nil {
		panic(err) // tests only
	}
	return b
}

func recoveryWriteFlag(t *testing.T, path string, started time.Time, profileID string) {
	t.Helper()
	b, err := json.Marshal(recoveryFlag{StartedAt: started.UTC().Format(time.RFC3339), ProfileId: profileID})
	if err != nil {
		t.Fatalf("marshaling the flag: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("writing the flag file: %v", err)
	}
}

func recoveryReadFlag(path string) (*recoveryFlag, error) {
	return (&PiRecovery{flagPath: path}).loadFlag()
}

func recoveryFlagExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// recoveryWaitForTicker waits until the loop goroutine has created the
// (fake) ticker (Start returns before the loop body runs, so the first
// explicit tick must not happen before the ticker exists).
func recoveryWaitForTicker(t *testing.T, cur **fakeTicker, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if *cur != nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the loop to create the ticker")
}

// recoveryWaitForState polls Status until it reports want ("", "rebooting",
// "waiting_after_reboot").
func recoveryWaitForState(t *testing.T, rec *PiRecovery, want string, timeout time.Duration) (string, time.Time) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, started := rec.Status()
		if state == want {
			return state, started
		}
		time.Sleep(5 * time.Millisecond)
	}
	last, _ := rec.Status()
	t.Fatalf("timed out waiting for recovery state %q (last: %q)", want, last)
	return "", time.Time{}
}

// recoveryToggle is a mutex-guarded bool the test flips while the loop
// goroutine reads it (no data race under -race).
type recoveryToggle struct {
	mu  sync.Mutex
	val bool
}

func (x *recoveryToggle) set(v bool) {
	x.mu.Lock()
	x.val = v
	x.mu.Unlock()
}

func (x *recoveryToggle) get() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.val
}

// recoveryCounter counts the RunTethering calls (the re-fire must happen
// EXACTLY once per episode; a sticky bool could not prove the "no second
// call" half).
type recoveryCounter struct {
	mu  sync.Mutex
	val int
}

func (x *recoveryCounter) inc() int {
	x.mu.Lock()
	x.val++
	v := x.val
	x.mu.Unlock()
	return v
}

func (x *recoveryCounter) get() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.val
}

// (a) a recognized Pi (device key exists) is reachable and the Mira has no
// internet for the CONTINUOUS 15 s window -> the Pi is rebooted exactly
// once: the persistent flag is written BEFORE the `ssh reboot` (recorded
// from inside the fake ssh at the moment of the call) and no second
// reboot ever happens in this lifetime.
func TestPiRecovery_RebootsExactlyOnceAfterFifteenSecondsWithoutInternet(t *testing.T) {
	_, _ = fakeKeyEnv(t)
	keyPath, err := KeyPathForProfile("pi-1")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-1): %v", err)
	}
	writeFakeKey(t, keyPath) // the RPi is already recognized (key exists)

	flagPath := filepath.Join(t.TempDir(), "pi_reboot_recovery.json")
	rebootLog := installRecoveryRebootFakeSSH(t, flagPath)

	t0 := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	var nowOffset time.Duration
	var cur *fakeTicker
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: flagPath,
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable: func(ctx context.Context) bool { return true },
		CheckInternet: func(ctx context.Context) (bool, error) {
			return false, nil
		},
		Now: func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			ft := &fakeTicker{ch: make(chan time.Time, 1)}
			cur = ft
			return ft
		},
	})
	rec.Start()
	defer rec.Stop()
	recoveryWaitForTicker(t, &cur, 5*time.Second)

	// t=0 (the immediate tick on Start): the first "reachable + no
	// internet" observation starts the 15 s window - no reboot yet
	time.Sleep(100 * time.Millisecond)
	if recoveryFlagExists(flagPath) {
		t.Fatalf("flag file written on the first observation (the 15 s window is not complete)")
	}
	// t=5, t=10: still inside the 15 s window
	for _, ts := range []time.Duration{5 * time.Second, 10 * time.Second} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	if recoveryFlagExists(flagPath) {
		t.Fatalf("reboot issued after only 10 s, want the full 15 s window")
	}
	// t=15: the window is complete -> flag persisted BEFORE the reboot
	nowOffset = 15 * time.Second
	cur.tick()
	_, started := recoveryWaitForState(t, rec, recoveryStateWaiting, 5*time.Second)
	if want := t0.Add(15 * time.Second).Format(time.RFC3339); started.UTC().Format(time.RFC3339) != want {
		t.Errorf("recovery_started_at = %q, want %q (the flag's started_at)", started, want)
	}
	// the fake ssh recorded the flag state AT THE TIME OF the reboot call
	lines := recoveryRebootLogLines(t, rebootLog)
	if len(lines) != 1 {
		t.Fatalf("reboot calls = %d, want exactly 1: %v", len(lines), lines)
	}
	if lines[0] != "flag=present" {
		t.Fatalf("flag state at the reboot call = %q, want flag=present (the flag must be persisted BEFORE the reboot)", lines[0])
	}
	flag, err := recoveryReadFlag(flagPath)
	if err != nil || flag == nil {
		t.Fatalf("flag file missing after the reboot: %v", err)
	}
	if flag.ProfileId != "pi-1" {
		t.Errorf("flag profile_id = %q, want pi-1", flag.ProfileId)
	}
	// no second reboot in this lifetime, even with the condition persisting
	for _, ts := range []time.Duration{20 * time.Second, 25 * time.Second, 30 * time.Second} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	if lines := recoveryRebootLogLines(t, rebootLog); len(lines) != 1 {
		t.Fatalf("reboot calls after the condition persisted = %d, want still exactly 1 (exactly once per lifetime)", len(lines))
	}
	if st, _ := rec.Status(); st != recoveryStateWaiting {
		t.Errorf("state = %q, want waiting_after_reboot", st)
	}
}

// (b) the flag survives the Mira's own restart: a fresh service instance
// finds a FRESH flag from the previous lifetime -> it enters the waiting
// phase WITHOUT a second reboot (the "exactly once" guarantee across the
// Mira restart); while the Pi is still booting no reboot is issued, when
// the Pi is back the re-run-safe tethering setup is re-fired exactly once
// (the iptables NAT rules are runtime-only), and internet OK clears the
// flag.
func TestPiRecovery_FreshFlagSurvivesMiraRestart(t *testing.T) {
	_, _ = fakeKeyEnv(t)
	keyPath, err := KeyPathForProfile("pi-1")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-1): %v", err)
	}
	writeFakeKey(t, keyPath)

	flagPath := filepath.Join(t.TempDir(), "pi_reboot_recovery.json")
	rebootLog := installRecoveryRebootFakeSSH(t, flagPath)

	t0 := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	started := t0.Add(-time.Minute)
	recoveryWriteFlag(t, flagPath, started, "pi-1") // the previous lifetime's reboot

	var nowOffset time.Duration
	var cur *fakeTicker
	reachable := &recoveryToggle{} // the Pi is still booting
	internet := &recoveryToggle{}
	tetherFired := &recoveryCounter{}
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: flagPath,
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable: func(ctx context.Context) bool { return reachable.get() },
		CheckInternet: func(ctx context.Context) (bool, error) {
			return internet.get(), nil
		},
		RunTethering: func() (string, error) {
			tetherFired.inc()
			return "job-1", nil
		},
		Now: func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			ft := &fakeTicker{ch: make(chan time.Time, 1)}
			cur = ft
			return ft
		},
	})
	rec.Start()
	defer rec.Stop()
	recoveryWaitForTicker(t, &cur, 5*time.Second)

	// boot check: fresh flag -> waiting phase, started_at from the flag
	_, gotStarted := recoveryWaitForState(t, rec, recoveryStateWaiting, 5*time.Second)
	if want := started.UTC().Format(time.RFC3339); gotStarted.UTC().Format(time.RFC3339) != want {
		t.Errorf("recovery_started_at = %q, want %q (the flag's started_at)", gotStarted, started)
	}
	// the Pi is still booting (slower than the Mira): down ticks, NO second
	// reboot at any point
	for _, ts := range []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	if lines := recoveryRebootLogLines(t, rebootLog); len(lines) != 0 {
		t.Fatalf("a second reboot was issued after the Mira restart: %v", lines)
	}
	// the Pi is back (still no internet): the tethering setup is re-fired
	// exactly once
	reachable.set(true)
	nowOffset = 25 * time.Second
	cur.tick()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tetherFired.get() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tetherFired.get() != 1 {
		t.Fatalf("tethering re-fire calls = %d, want exactly 1", tetherFired.get())
	}
	// another reachable tick: no second re-fire
	nowOffset = 30 * time.Second
	cur.tick()
	time.Sleep(100 * time.Millisecond)
	if tetherFired.get() != 1 {
		t.Fatalf("tethering re-fire calls = %d, want still exactly 1 (no second re-fire)", tetherFired.get())
	}
	// internet is back -> the flag is cleared, state idle
	internet.set(true)
	nowOffset = 35 * time.Second
	cur.tick()
	recoveryWaitForState(t, rec, "", 5*time.Second)
	if recoveryFlagExists(flagPath) {
		t.Fatalf("the flag was not cleared after the internet came back")
	}
	if lines := recoveryRebootLogLines(t, rebootLog); len(lines) != 0 {
		t.Fatalf("reboot calls across the whole episode = %d, want 0 in this lifetime: %v", len(lines), lines)
	}
}

// (c) a reboot issued in THIS lifetime: the Pi goes down (sawDown), comes
// back -> the tethering setup is re-fired exactly once (not again on the
// next tick), and internet OK clears the flag.
func TestPiRecovery_RebootEpisodeWithTetheringRerun(t *testing.T) {
	_, _ = fakeKeyEnv(t)
	keyPath, err := KeyPathForProfile("pi-1")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-1): %v", err)
	}
	writeFakeKey(t, keyPath)

	flagPath := filepath.Join(t.TempDir(), "pi_reboot_recovery.json")
	rebootLog := installRecoveryRebootFakeSSH(t, flagPath)

	t0 := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	var nowOffset time.Duration
	var cur *fakeTicker
	reachable := &recoveryToggle{}
	internet := &recoveryToggle{}
	tetherFired := &recoveryCounter{}
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: flagPath,
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable: func(ctx context.Context) bool { return reachable.get() },
		CheckInternet: func(ctx context.Context) (bool, error) {
			return internet.get(), nil
		},
		RunTethering: func() (string, error) {
			tetherFired.inc()
			return "job-1", nil
		},
		Now: func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			ft := &fakeTicker{ch: make(chan time.Time, 1)}
			cur = ft
			return ft
		},
	})
	reachable.set(true)
	rec.Start()
	defer rec.Stop()
	recoveryWaitForTicker(t, &cur, 5*time.Second)

	// the 15 s window (t=0..10) then the reboot at t=15
	for _, ts := range []time.Duration{5 * time.Second, 10 * time.Second} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	nowOffset = 15 * time.Second
	cur.tick()
	recoveryWaitForState(t, rec, recoveryStateWaiting, 5*time.Second)
	if lines := recoveryRebootLogLines(t, rebootLog); len(lines) != 1 {
		t.Fatalf("reboot calls = %d, want 1: %v", len(lines), lines)
	}
	// the Pi goes down (the reboot is in flight) and comes back
	reachable.set(false)
	for _, ts := range []time.Duration{20 * time.Second, 25 * time.Second} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	reachable.set(true)
	nowOffset = 30 * time.Second
	cur.tick()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tetherFired.get() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tetherFired.get() != 1 {
		t.Fatalf("tethering re-fire calls = %d, want exactly 1", tetherFired.get())
	}
	// another reachable tick: no second re-fire
	nowOffset = 35 * time.Second
	cur.tick()
	time.Sleep(100 * time.Millisecond)
	if tetherFired.get() != 1 {
		t.Fatalf("tethering re-fire calls = %d, want still exactly 1 (no second re-fire)", tetherFired.get())
	}
	// internet is back -> flag cleared, state idle
	internet.set(true)
	nowOffset = 40 * time.Second
	cur.tick()
	recoveryWaitForState(t, rec, "", 5*time.Second)
	if recoveryFlagExists(flagPath) {
		t.Fatalf("the flag was not cleared after the internet came back")
	}
}

// (d) stale flag: the previous episode already timed out -> warning + flag
// cleared + the episode is consumed (no new reboot in this lifetime, a
// broken setup must not become a reboot loop across power cycles).
func TestPiRecovery_StaleFlagConsumedNoReboot(t *testing.T) {
	_, _ = fakeKeyEnv(t)
	keyPath, err := KeyPathForProfile("pi-1")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-1): %v", err)
	}
	writeFakeKey(t, keyPath)

	flagPath := filepath.Join(t.TempDir(), "pi_reboot_recovery.json")
	rebootLog := installRecoveryRebootFakeSSH(t, flagPath)

	t0 := time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC)
	recoveryWriteFlag(t, flagPath, t0.Add(-11*time.Minute), "pi-1") // older than the 10 min patience

	var nowOffset time.Duration
	var cur *fakeTicker
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: flagPath,
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable: func(ctx context.Context) bool { return true },
		CheckInternet: func(ctx context.Context) (bool, error) {
			return false, nil
		},
		Now: func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			ft := &fakeTicker{ch: make(chan time.Time, 1)}
			cur = ft
			return ft
		},
	})
	rec.Start()
	defer rec.Stop()
	recoveryWaitForTicker(t, &cur, 5*time.Second)

	// the boot check cleared the stale flag; the episode is consumed
	if recoveryFlagExists(flagPath) {
		t.Fatalf("the stale flag was not cleared on boot")
	}
	// even with "reachable + no internet" for the full 15 s window, no
	// reboot in this lifetime
	time.Sleep(100 * time.Millisecond)
	for _, ts := range []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	if lines := recoveryRebootLogLines(t, rebootLog); len(lines) != 0 {
		t.Fatalf("a reboot was issued although the stale flag consumed the episode: %v", lines)
	}
	if recoveryFlagExists(flagPath) {
		t.Fatalf("a new flag was written although the stale flag consumed the episode")
	}
	if st, _ := rec.Status(); st != "" {
		t.Errorf("state = %q, want idle", st)
	}
}

// (d2) patience window timeout: a fresh flag, the Pi never becomes
// reachable -> after the 10 min window the flag is cleared (timeout
// warning), state idle, and no new reboot follows.
func TestPiRecovery_WaitingWindowTimeoutClearsFlag(t *testing.T) {
	_, _ = fakeKeyEnv(t)
	keyPath, err := KeyPathForProfile("pi-1")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-1): %v", err)
	}
	writeFakeKey(t, keyPath)

	flagPath := filepath.Join(t.TempDir(), "pi_reboot_recovery.json")
	rebootLog := installRecoveryRebootFakeSSH(t, flagPath)

	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	recoveryWriteFlag(t, flagPath, t0.Add(-time.Minute), "pi-1") // fresh: waiting phase

	var nowOffset time.Duration
	var cur *fakeTicker
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: flagPath,
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable: func(ctx context.Context) bool { return false }, // the Pi never comes back
		CheckInternet: func(ctx context.Context) (bool, error) {
			return false, nil
		},
		Now: func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			ft := &fakeTicker{ch: make(chan time.Time, 1)}
			cur = ft
			return ft
		},
	})
	rec.Start()
	defer rec.Stop()
	recoveryWaitForTicker(t, &cur, 5*time.Second)

	recoveryWaitForState(t, rec, recoveryStateWaiting, 5*time.Second)
	// inside the patience window: still waiting
	for _, ts := range []time.Duration{1 * time.Minute, 2 * time.Minute, 5 * time.Minute} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	if st, _ := rec.Status(); st != recoveryStateWaiting {
		t.Fatalf("state after 5 min = %q, want waiting_after_reboot (inside the patience window)", st)
	}
	// past the 10 min patience window: timeout -> flag cleared, state idle
	nowOffset = 11 * time.Minute
	cur.tick()
	recoveryWaitForState(t, rec, "", 5*time.Second)
	if recoveryFlagExists(flagPath) {
		t.Fatalf("the flag was not cleared on the patience window timeout")
	}
	// no new reboot follows (the lifetime episode is consumed)
	nowOffset = 12 * time.Minute
	cur.tick()
	time.Sleep(100 * time.Millisecond)
	if lines := recoveryRebootLogLines(t, rebootLog); len(lines) != 0 {
		t.Fatalf("a reboot was issued after the timeout: %v", lines)
	}
}

// (d3) profile mismatch: the flag belongs to another profile -> warning +
// clear + the episode is consumed (no new reboot in this lifetime).
func TestPiRecovery_ProfileMismatchClearsFlag(t *testing.T) {
	_, _ = fakeKeyEnv(t)
	keyPath, err := KeyPathForProfile("pi-1")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-1): %v", err)
	}
	writeFakeKey(t, keyPath)

	flagPath := filepath.Join(t.TempDir(), "pi_reboot_recovery.json")
	rebootLog := installRecoveryRebootFakeSSH(t, flagPath)

	t0 := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	recoveryWriteFlag(t, flagPath, t0.Add(-time.Minute), "pi-2") // someone else's flag

	var nowOffset time.Duration
	var cur *fakeTicker
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: flagPath,
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable: func(ctx context.Context) bool { return true },
		CheckInternet: func(ctx context.Context) (bool, error) {
			return false, nil
		},
		Now: func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			ft := &fakeTicker{ch: make(chan time.Time, 1)}
			cur = ft
			return ft
		},
	})
	rec.Start()
	defer rec.Stop()
	recoveryWaitForTicker(t, &cur, 5*time.Second)

	if recoveryFlagExists(flagPath) {
		t.Fatalf("the mismatched flag was not cleared on boot")
	}
	// the consumed episode suppresses any new reboot in this lifetime
	time.Sleep(100 * time.Millisecond)
	for _, ts := range []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	if lines := recoveryRebootLogLines(t, rebootLog); len(lines) != 0 {
		t.Fatalf("a reboot was issued although the flag belonged to another profile: %v", lines)
	}
	if recoveryFlagExists(flagPath) {
		t.Fatalf("a new flag was written although the episode is consumed")
	}
}

// (e) boot-timing tolerance + status shape: while the recovery window is
// open (the RPi reboots slower than the Mira) the session keeps its
// patient key-only loop and draws no false conclusions - the
// /api/pi/status recovery fields stay set and constant through the SSH
// failure wave and are omitted again once the recovery is idle.
func TestPiRecovery_StatusConsistentDuringSSHFailureWave(t *testing.T) {
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	keyPath, err := KeyPathForProfile("pi-1")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-1): %v", err)
	}
	writeFakeKey(t, keyPath)
	fakeKeyOK(t, false) // the SSH failure wave: key auth fails (Pi still booting)

	flagPath := filepath.Join(t.TempDir(), "pi_reboot_recovery.json")
	t0 := time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)
	started := t0.Add(-time.Minute)
	recoveryWriteFlag(t, flagPath, started, "pi-1")

	var nowOffset time.Duration
	var cur *fakeTicker
	internet := &recoveryToggle{}
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: flagPath,
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable: func(ctx context.Context) bool { return false }, // still booting
		CheckInternet: func(ctx context.Context) (bool, error) {
			return internet.get(), nil
		},
		Now: func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			ft := &fakeTicker{ch: make(chan time.Time, 1)}
			cur = ft
			return ft
		},
	})
	rec.Start()
	defer rec.Stop()
	recoveryWaitForTicker(t, &cur, 5*time.Second)

	sess := NewPiSession(&librespot.NullLogger{}, PiSessionConfig{
		LoadConfig:    func() (string, string, string, bool) { return "pi-1", "192.168.7.1", "piuser", true },
		RetryInterval: 20 * time.Millisecond, // the patient loop, fast in the test
		RecoveryStatus: func() (string, time.Time) {
			return rec.Status()
		},
	})
	sess.Start()
	defer sess.Stop()

	// through the whole SSH failure wave the recovery fields stay set and
	// constant (no state reset), conn only ever takes the expected states
	wantStarted := started.UTC().Format(time.RFC3339)
	for i := 0; i < 10; i++ {
		st := sess.PiStatus()
		if st.Recovery != recoveryStateWaiting {
			t.Fatalf("sample %d: recovery = %q, want waiting_after_reboot (the SSH failure wave must not reset it)", i, st.Recovery)
		}
		if st.RecoveryStartedAt != wantStarted {
			t.Fatalf("sample %d: recovery_started_at = %q, want %q (constant over the wave)", i, st.RecoveryStartedAt, wantStarted)
		}
		if c := st.Conn; c != piConnDisconnected && c != piConnConnecting && c != piConnConnected {
			t.Fatalf("sample %d: conn = %q, want one of the expected connection states", i, c)
		}
		time.Sleep(30 * time.Millisecond)
	}

	// the fields are PRESENT in the JSON (omitempty, not empty strings)
	b, err := json.Marshal(sess.PiStatus())
	if err != nil {
		t.Fatalf("marshaling the status: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decoding the status: %v", err)
	}
	if _, ok := m["recovery"]; !ok {
		t.Errorf("recovery missing from the status JSON: %s", b)
	}
	if _, ok := m["recovery_started_at"]; !ok {
		t.Errorf("recovery_started_at missing from the status JSON: %s", b)
	}

	// internet is back -> the flag is cleared -> the fields are omitted
	// again (the UI suppresses the onboarding card only while set)
	internet.set(true)
	cur.tick()
	recoveryWaitForState(t, rec, "", 5*time.Second)
	b, _ = json.Marshal(sess.PiStatus())
	var m2 map[string]any // a fresh map: Unmarshal merges into an existing one
	if err := json.Unmarshal(b, &m2); err != nil {
		t.Fatalf("decoding the status: %v", err)
	}
	if _, ok := m2["recovery"]; ok {
		t.Errorf("recovery must be omitted when idle: %s", b)
	}
	if _, ok := m2["recovery_started_at"]; ok {
		t.Errorf("recovery_started_at must be omitted when idle: %s", b)
	}
}

// (f) the 15 s window is CONTINUOUS: an unreachability (the RPi is still
// booting, or a link blip) resets the clock - no reboot until a full
// window completes again from the first "reachable + no internet"
// observation (boot-timing tolerance: early failures draw no conclusions).
func TestPiRecovery_NoInternetWindowResetsOnUnreachability(t *testing.T) {
	_, _ = fakeKeyEnv(t)
	keyPath, err := KeyPathForProfile("pi-1")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-1): %v", err)
	}
	writeFakeKey(t, keyPath)

	flagPath := filepath.Join(t.TempDir(), "pi_reboot_recovery.json")
	rebootLog := installRecoveryRebootFakeSSH(t, flagPath)

	t0 := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	var nowOffset time.Duration
	var cur *fakeTicker
	reachable := &recoveryToggle{}
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: flagPath,
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable: func(ctx context.Context) bool { return reachable.get() },
		CheckInternet: func(ctx context.Context) (bool, error) {
			return false, nil
		},
		Now: func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			ft := &fakeTicker{ch: make(chan time.Time, 1)}
			cur = ft
			return ft
		},
	})
	reachable.set(true)
	rec.Start()
	defer rec.Stop()
	recoveryWaitForTicker(t, &cur, 5*time.Second)

	// t=0: window starts; t=5, t=10: inside the window
	time.Sleep(100 * time.Millisecond)
	for _, ts := range []time.Duration{5 * time.Second, 10 * time.Second} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	// t=15, t=20: the Pi is unreachable (still booting / blip) -> the clock
	// resets
	reachable.set(false)
	for _, ts := range []time.Duration{15 * time.Second, 20 * time.Second} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	// t=25: reachable again -> the window restarts; t=30, t=35: 10 s in -
	// 35 s total elapsed, but NO reboot yet (the window never ran 15 s
	// continuously)
	reachable.set(true)
	for _, ts := range []time.Duration{25 * time.Second, 30 * time.Second, 35 * time.Second} {
		nowOffset = ts
		cur.tick()
		time.Sleep(100 * time.Millisecond)
	}
	if recoveryFlagExists(flagPath) {
		t.Fatalf("reboot issued although the 15 s window was interrupted by an unreachability")
	}
	if lines := recoveryRebootLogLines(t, rebootLog); len(lines) != 0 {
		t.Fatalf("reboot calls = %d, want 0: %v", len(lines), lines)
	}
	// t=40: a full window completes (25 s -> 40 s) -> reboot now
	nowOffset = 40 * time.Second
	cur.tick()
	recoveryWaitForState(t, rec, recoveryStateWaiting, 5*time.Second)
	if lines := recoveryRebootLogLines(t, rebootLog); len(lines) != 1 {
		t.Fatalf("reboot calls = %d, want 1 after the uninterrupted window: %v", len(lines), lines)
	}
}

// (g) one loop goroutine for the whole lifetime, none per probe; Stop is
// clean and idempotent (safe before Start too); a slow probe does not hold
// Stop hostage (the ctx cancel unblocks the loop).
func TestPiRecovery_LeakCheckAndCleanStop(t *testing.T) {
	time.Sleep(100 * time.Millisecond) // let the runtime settle
	before := runtime.NumGoroutine()
	if g := recoveryLoopGoroutines(); g != 0 {
		t.Fatalf("PiRecovery.loop goroutine present before Start: %d", g)
	}

	// idle instance (no stored profiles): the loop idles
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath:     filepath.Join(t.TempDir(), "flag.json"),
		LoadSettings: func() []byte { return nil },
	})
	rec.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && recoveryLoopGoroutines() != 1 {
		time.Sleep(2 * time.Millisecond) // the loop goroutine schedules in
	}
	if g := recoveryLoopGoroutines(); g != 1 {
		t.Fatalf("PiRecovery.loop goroutines after Start = %d, want exactly 1", g)
	}
	rec.Stop()
	time.Sleep(100 * time.Millisecond)
	if g := recoveryLoopGoroutines(); g != 0 {
		t.Fatalf("PiRecovery.loop still running after Stop: %d goroutines", g)
	}
	after := runtime.NumGoroutine()
	if after > before+3 {
		t.Fatalf("goroutine leak: %d after stop, %d before start", after, before)
	}
	rec.Stop() // must be a no-op

	// Stop before Start is safe
	other := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: filepath.Join(t.TempDir(), "flag.json"),
	})
	other.Stop()
	other.Stop()

	// a slow probe: Stop must return as soon as the probe yields
	_, _ = fakeKeyEnv(t)
	keyPath, err := KeyPathForProfile("pi-1")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-1): %v", err)
	}
	writeFakeKey(t, keyPath)
	slow := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: filepath.Join(t.TempDir(), "flag.json"),
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable: func(ctx context.Context) bool {
			select {
			case <-time.After(300 * time.Millisecond):
			case <-ctx.Done():
			}
			return false
		},
		CheckInternet: func(ctx context.Context) (bool, error) { return false, nil },
	})
	slow.Start()
	done := make(chan struct{})
	go func() {
		slow.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("Stop blocked on a slow probe (the ctx cancel must unblock the loop)")
	}
	time.Sleep(100 * time.Millisecond)
	if g := recoveryLoopGoroutines(); g != 0 {
		t.Fatalf("PiRecovery.loop still running after Stop: %d goroutines", g)
	}
}

// (h) flag file contract: the env override wins over the configured path;
// the on-disk shape is {"started_at": RFC3339 UTC, "profile_id"}; a
// corrupt flag file is tolerated (warning + continue, never a crash).
func TestPiRecovery_FlagFileShapeAndCorruption(t *testing.T) {
	fakeSSHBinDir(t)
	_, _ = fakeKeyEnv(t)
	fakeKeyOK(t, true)
	keyPath, err := KeyPathForProfile("pi-1")
	if err != nil {
		t.Fatalf("KeyPathForProfile(pi-1): %v", err)
	}
	writeFakeKey(t, keyPath)

	// env override wins over the configured path
	envPath := filepath.Join(t.TempDir(), "env-flag.json")
	cfgPath := filepath.Join(t.TempDir(), "cfg-flag.json")
	t.Setenv(recoveryFlagEnv, envPath)

	t0 := time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC)
	var nowOffset time.Duration
	var cur *fakeTicker
	rec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{
		FlagPath: cfgPath, // must be ignored (the env override wins)
		LoadSettings: func() []byte {
			return recoveryProfilesBlobJSON("pi-1", PiProfile{ID: "pi-1", Ip: "192.168.7.1", User: "piuser"})
		},
		PiReachable:      func(ctx context.Context) bool { return true },
		CheckInternet:    func(ctx context.Context) (bool, error) { return false, nil },
		NoInternetWindow: 1 * time.Second,
		Now:              func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			ft := &fakeTicker{ch: make(chan time.Time, 1)}
			cur = ft
			return ft
		},
	})
	rec.Start()
	defer rec.Stop()
	recoveryWaitForTicker(t, &cur, 5*time.Second)
	time.Sleep(100 * time.Millisecond) // t=0: the 1 s window starts
	nowOffset = time.Second
	cur.tick()
	recoveryWaitForState(t, rec, recoveryStateWaiting, 5*time.Second)

	if !recoveryFlagExists(envPath) {
		t.Fatalf("the flag file is missing at the env-override path %s", envPath)
	}
	if recoveryFlagExists(cfgPath) {
		t.Fatalf("the flag file was written to the configured path although the env override is set")
	}
	flag, err := recoveryReadFlag(envPath)
	if err != nil || flag == nil {
		t.Fatalf("reading the flag file: %v", err)
	}
	if flag.ProfileId != "pi-1" {
		t.Errorf("flag profile_id = %q, want pi-1", flag.ProfileId)
	}
	if _, perr := time.Parse(time.RFC3339, flag.StartedAt); perr != nil {
		t.Errorf("flag started_at = %q, want RFC3339: %v", flag.StartedAt, perr)
	}
	t.Setenv(recoveryFlagEnv, "") // back to the configured path

	// corrupt flag files are tolerated (warning + continue, no crash)
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("writing the corrupt flag: %v", err)
	}
	badRec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{FlagPath: bad})
	badRec.bootCheck() // synchronous: no loop needed
	if st, _ := badRec.Status(); st != "" {
		t.Errorf("state after a corrupt flag = %q, want idle", st)
	}

	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte(`{"started_at":"","profile_id":""}`), 0o644); err != nil {
		t.Fatalf("writing the empty flag: %v", err)
	}
	emptyRec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{FlagPath: empty})
	emptyRec.bootCheck()
	if st, _ := emptyRec.Status(); st != "" {
		t.Errorf("state after an empty flag = %q, want idle", st)
	}
	if !recoveryFlagExists(empty) {
		t.Errorf("the empty flag file was modified (an absent flag must not be touched)")
	}

	badt := filepath.Join(t.TempDir(), "badt.json")
	if err := os.WriteFile(badt, []byte(`{"started_at":"not-a-time","profile_id":"pi-1"}`), 0o644); err != nil {
		t.Fatalf("writing the bad-timestamp flag: %v", err)
	}
	badtRec := NewPiRecovery(&librespot.NullLogger{}, PiRecoveryConfig{FlagPath: badt})
	badtRec.bootCheck()
	if recoveryFlagExists(badt) {
		t.Errorf("the flag with an invalid timestamp was not cleared")
	}
}
