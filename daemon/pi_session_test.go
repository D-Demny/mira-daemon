package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// epic 10 ticket10-4: PiSession (SSH auto-reconnect, boot recovery + fixed
// 10 s retry) and the /api/pi/status contract tests. Fake SSH: the fake
// ssh/sshpass/ssh-keygen binaries from sshkey_test.go (the fake ssh
// additionally emulates the long-lived `ssh -N` session with a fake Pi
// up/down file). Fake clock + fake ticker: the 10 s retry rhythm is driven
// deterministically, no real waiting.

// fakeTicker is a deterministic replacement for time.Ticker in the rhythm
// tests: the test sends a tick (= one retry interval elapsed) on demand.
// Buffer cap 1 mirrors time.Ticker (a tick while the loop is busy is
// dropped, exactly one can be queued).
type fakeTicker struct {
	ch chan time.Time
}

func (f *fakeTicker) C() <-chan time.Time { return f.ch }
func (f *fakeTicker) Stop()               {}

// tick delivers one retry-interval tick (non-blocking, dropped when full).
func (f *fakeTicker) tick() {
	select {
	case f.ch <- time.Time{}:
	default:
	}
}

func (s *PiSession) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *PiSession) logLineCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.logLines)
}

// piSessionLoopGoroutines counts live PiSession.loop goroutines on the
// stacks - exact, unlike a raw NumGoroutine total (which wobbles with
// transient runtime goroutines, e.g. the CommandContext kill-waiters of an
// in-flight fake ssh).
func piSessionLoopGoroutines() int {
	b := make([]byte, 1024*1024)
	n := runtime.Stack(b, true)
	return strings.Count(string(b[:n]), "daemon.(*PiSession).loop")
}

// writeFakeKey creates the device-side key file the manager stats before
// an attempt.
func writeFakeKey(t *testing.T, keyPath string) {
	t.Helper()
	if err := os.WriteFile(keyPath, []byte("FAKE-PRIVATE-KEY\n"), 0o600); err != nil {
		t.Fatalf("writing fake key: %v", err)
	}
}

// waitForPiConn polls PiStatus until conn reaches want.
func waitForPiConn(t *testing.T, s *PiSession, want string, timeout time.Duration) PiStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := s.PiStatus()
		if st.Conn == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for conn=%q (last: %+v)", want, s.PiStatus())
	return PiStatus{}
}

// waitForAttempts polls until at least want attempts have started.
func waitForAttempts(t *testing.T, s *PiSession, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.attemptCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d attempts (have %d)", want, s.attemptCount())
}

// (a) reboot -> auto-reconnect: stored config + key present, fake Pi up,
// key auth works -> the manager reconnects on its own (first attempt
// immediate on Start, no user interaction).
func TestPiSession_BootAutoReconnect(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	fakeKeyOK(t, true)
	if err := os.WriteFile(fakePiUpFile(t), []byte("up\n"), 0o644); err != nil {
		t.Fatalf("creating fake Pi-up file: %v", err)
	}
	writeFakeKey(t, keyPath)

	sess := NewPiSession(&librespot.NullLogger{}, PiSessionConfig{
		LoadConfig: func() (string, string, bool) { return "192.168.7.1", "piuser", true },
		ModelTier:  func() (string, string) { return "Fake Pi 4", "compute" },
	})
	sess.Start()
	defer sess.Stop()

	st := waitForPiConn(t, sess, piConnConnected, 15*time.Second)
	if st.LastAttemptAt == "" {
		t.Errorf("last_attempt_at empty, want the RFC3339 time of the boot attempt")
	}
	if st.Model != "Fake Pi 4" || st.Tier != "compute" {
		t.Errorf("model/tier = %q/%q, want the last known-good state", st.Model, st.Tier)
	}
}

// (b) cable pulled -> attempts at a fixed 10 s rhythm (fake clock): after
// 30 s elapsed, exactly 3 attempts (t=0, 10, 20), the rhythm continues on
// the grid.
func TestPiSession_FixedTenSecondRetryRhythm(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	fakeKeyOK(t, false) // unreachable Pi: every probe fails
	writeFakeKey(t, keyPath)

	t0 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	var nowOffset time.Duration
	var cur *fakeTicker
	sess := NewPiSession(&librespot.NullLogger{}, PiSessionConfig{
		LoadConfig: func() (string, string, bool) { return "192.168.7.1", "piuser", true },
		Now:        func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			if d != piSessionRetryInterval {
				t.Errorf("retry interval = %s, want the fixed %s (no backoff)", d, piSessionRetryInterval)
			}
			ft := &fakeTicker{ch: make(chan time.Time, 1)}
			cur = ft
			return ft
		},
	})
	sess.Start()
	defer sess.Stop()

	// the first attempt is immediate (t=0, boot recovery)
	waitForAttempts(t, sess, 1, 5*time.Second)
	// t=10: attempt 2
	nowOffset = 10 * time.Second
	cur.tick()
	waitForAttempts(t, sess, 2, 5*time.Second)
	// t=20: attempt 3
	nowOffset = 20 * time.Second
	cur.tick()
	waitForAttempts(t, sess, 3, 5*time.Second)
	// 30 s elapsed (the t=30 tick not delivered yet): exactly 3 attempts
	nowOffset = 30 * time.Second
	if got := sess.attemptCount(); got != 3 {
		t.Fatalf("attempts after 30 s = %d, want exactly 3 (fixed 10 s rhythm)", got)
	}
	st := sess.PiStatus()
	wantTS := t0.Add(20 * time.Second).Format(time.RFC3339)
	if st.LastAttemptAt != wantTS {
		t.Errorf("last_attempt_at = %q, want %q", st.LastAttemptAt, wantTS)
	}
	if st.Conn != piConnDisconnected {
		t.Errorf("conn = %q, want disconnected (Pi unreachable)", st.Conn)
	}
	// the rhythm continues on the grid: the t=30 tick starts attempt 4
	cur.tick()
	waitForAttempts(t, sess, 4, 5*time.Second)
}

// (b2) no stacking: ticks arriving while an attempt is in flight do not
// queue extra attempts (single in-flight attempt by construction).
func TestPiSession_NoStackingWhileAttemptInFlight(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	fakeKeyOK(t, false)
	t.Setenv("FAKE_SSH_DELAY", "0.6") // each probe takes >= 600 ms
	writeFakeKey(t, keyPath)

	var cur *fakeTicker
	sess := NewPiSession(&librespot.NullLogger{}, PiSessionConfig{
		LoadConfig: func() (string, string, bool) { return "192.168.7.1", "piuser", true },
		NewTicker:  func(d time.Duration) tickerLike { ft := &fakeTicker{ch: make(chan time.Time, 1)}; cur = ft; return ft },
	})
	sess.Start()
	defer sess.Stop()

	// the first attempt is in flight for >= 600 ms; pump 5 ticks at it
	waitForAttempts(t, sess, 1, 5*time.Second)
	time.Sleep(100 * time.Millisecond) // still inside the first attempt
	for i := 0; i < 5; i++ {
		cur.tick()
	}
	// attempt 1 ends at >= 600 ms; at most ONE queued tick may fire, so
	// once the second attempt is in flight there must be exactly 2
	time.Sleep(700 * time.Millisecond)
	if got := sess.attemptCount(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (ticks during an in-flight attempt must not stack)", got)
	}
}

// (c) 24 h offline -> stable: no crash, the goroutine count is constant
// (no leak), the log ring stays bounded, exactly one attempt per 10 s.
func TestPiSession_24hOutageStaysStable(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	fakeKeyOK(t, false) // the Pi never comes back
	writeFakeKey(t, keyPath)

	time.Sleep(100 * time.Millisecond) // let the runtime settle
	before := runtime.NumGoroutine()
	if g := piSessionLoopGoroutines(); g != 0 {
		t.Fatalf("PiSession.loop goroutine present before Start: %d", g)
	}

	t0 := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	var nowOffset time.Duration
	var cur *fakeTicker
	// 24 h in 10 s steps = 8640 grid ticks. The fake ticker gets a big
	// buffer (a real time.Ticker would never drop a tick here: one attempt
	// takes milliseconds, the grid is 10 s apart) so the test can deliver
	// all ticks without pacing the loop tick by tick.
	const ticks24h = 24 * 60 * 6
	sess := NewPiSession(&librespot.NullLogger{}, PiSessionConfig{
		LoadConfig: func() (string, string, bool) { return "192.168.7.1", "piuser", true },
		Now:        func() time.Time { return t0.Add(nowOffset) },
		NewTicker: func(d time.Duration) tickerLike {
			ft := &fakeTicker{ch: make(chan time.Time, ticks24h+1)}
			cur = ft
			return ft
		},
	})
	sess.Start()
	// the boot attempt (t=0) also guarantees the ticker is already created
	waitForAttempts(t, sess, 1, 5*time.Second)

	for i := 1; i <= ticks24h; i++ {
		nowOffset = time.Duration(i) * piSessionRetryInterval
		cur.tick()
	}
	// half of the 24 h outage elapsed: exactly one loop goroutine, no
	// total growth (transient CommandContext kill-waiters get +1..2 slack)
	waitForAttempts(t, sess, 1+ticks24h/2, 120*time.Second)
	if g := piSessionLoopGoroutines(); g != 1 {
		t.Fatalf("PiSession.loop goroutines mid-outage = %d, want exactly 1 (no per-attempt goroutines)", g)
	}
	if mid := runtime.NumGoroutine(); mid > before+3 {
		t.Fatalf("goroutine growth during outage: %d mid vs %d before start", mid, before)
	}
	// the rest of the outage
	waitForAttempts(t, sess, 1+ticks24h, 120*time.Second)
	// 1 (boot attempt) + 8640 (one per 10 s grid tick over 24 h)
	wantAttempts := 1 + ticks24h
	if got := sess.attemptCount(); got != wantAttempts {
		t.Fatalf("attempts over 24 h = %d, want %d (exactly one per 10 s)", got, wantAttempts)
	}
	if got := sess.logLineCount(); got != piSessionLogCap {
		t.Fatalf("log ring = %d lines, want exactly %d (bounded, full after 8641 appends)", got, piSessionLogCap)
	}
	if st := sess.PiStatus(); st.Conn != piConnDisconnected {
		t.Fatalf("conn = %q, want disconnected", st.Conn)
	}
	wantLast := t0.Add(time.Duration(ticks24h) * piSessionRetryInterval).Format(time.RFC3339)
	if st := sess.PiStatus(); st.LastAttemptAt != wantLast {
		t.Errorf("last_attempt_at = %q, want %q (the 24 h mark)", st.LastAttemptAt, wantLast)
	}

	sess.Stop()
	time.Sleep(100 * time.Millisecond)
	if g := piSessionLoopGoroutines(); g != 0 {
		t.Fatalf("PiSession.loop still running after Stop: %d goroutines", g)
	}
	after := runtime.NumGoroutine()
	if after > before+3 {
		t.Fatalf("goroutine leak: %d after stop, %d before start", after, before)
	}
}

// (d) Pi returns -> auto-reconnect WITHOUT a daemon restart (and a second
// drop is detected again by the same manager).
func TestPiSession_PiReturnsWithoutRestart(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	fakeKeyOK(t, false)
	writeFakeKey(t, keyPath)
	piUp := fakePiUpFile(t)

	sess := NewPiSession(&librespot.NullLogger{}, PiSessionConfig{
		LoadConfig:    func() (string, string, bool) { return "192.168.7.1", "piuser", true },
		RetryInterval: 100 * time.Millisecond, // real ticker, short rhythm
	})
	sess.Start()
	defer sess.Stop()

	// unreachable Pi: a couple of failed attempts
	waitForAttempts(t, sess, 2, 5*time.Second)

	// the Pi comes back (key auth now works, the fake Pi is up)
	fakeKeyOK(t, true)
	if err := os.WriteFile(piUp, []byte("up\n"), 0o644); err != nil {
		t.Fatalf("bringing the fake Pi up: %v", err)
	}
	waitForPiConn(t, sess, piConnConnected, 5*time.Second)

	// cable pulled again: the session dies on its own, the state returns to
	// disconnected - all without a daemon restart
	if err := os.Remove(piUp); err != nil {
		t.Fatalf("pulling the fake cable: %v", err)
	}
	waitForPiConn(t, sess, piConnDisconnected, 5*time.Second)

	// and back once more: the 10 s loop reconnects on its own
	if err := os.WriteFile(piUp, []byte("up\n"), 0o644); err != nil {
		t.Fatalf("re-plugging the fake cable: %v", err)
	}
	waitForPiConn(t, sess, piConnConnected, 5*time.Second)
}

// (e) no key -> the session stays idle WITHOUT attempts (no 10 s loop of
// SSH calls, no log spam); a key that appears later (provisioning wizard
// finishing in this boot) is picked up automatically on the next tick.
func TestPiSession_NoKeyNoAttempts(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	// the key file must NOT exist; the mark file proves the fake ssh was
	// never spawned
	mark := filepath.Join(t.TempDir(), "ssh-calls")
	t.Setenv("FAKE_SSH_MARK", mark)

	sess := NewPiSession(&librespot.NullLogger{}, PiSessionConfig{
		LoadConfig:    func() (string, string, bool) { return "192.168.7.1", "piuser", true },
		RetryInterval: 50 * time.Millisecond,
	})
	sess.Start()
	defer sess.Stop()

	time.Sleep(300 * time.Millisecond) // several ticks, still no key
	if got := sess.attemptCount(); got != 0 {
		t.Fatalf("attempts without a key = %d, want 0 (key-only: no loop of attempts without a key)", got)
	}
	if st := sess.PiStatus(); st.Conn != piConnDisconnected {
		t.Fatalf("conn without a key = %q, want disconnected", st.Conn)
	}
	if st := sess.PiStatus(); st.LastAttemptAt != "" {
		t.Fatalf("last_attempt_at = %q, want empty (no attempt happened)", st.LastAttemptAt)
	}
	if _, err := os.Stat(mark); !os.IsNotExist(err) {
		t.Fatalf("the fake ssh was spawned although no key exists (mark file present)")
	}

	// the key appears (e.g. the provisioning wizard just finished) -> the
	// next tick starts the reconnect on its own
	fakeKeyOK(t, true)
	writeFakeKey(t, keyPath)
	if err := os.WriteFile(fakePiUpFile(t), []byte("up\n"), 0o644); err != nil {
		t.Fatalf("bringing the fake Pi up: %v", err)
	}
	waitForPiConn(t, sess, piConnConnected, 5*time.Second)
}

// shutdown: Stop() kills a running session and leaves no hanging
// goroutine; calling it twice is a no-op.
func TestPiSession_StopCleansUp(t *testing.T) {
	fakeSSHBinDir(t)
	keyPath, _ := fakeKeyEnv(t)
	fakeKeyOK(t, true)
	writeFakeKey(t, keyPath)
	if err := os.WriteFile(fakePiUpFile(t), []byte("up\n"), 0o644); err != nil {
		t.Fatalf("creating fake Pi-up file: %v", err)
	}

	time.Sleep(100 * time.Millisecond) // let the runtime settle
	before := runtime.NumGoroutine()

	sess := NewPiSession(&librespot.NullLogger{}, PiSessionConfig{
		LoadConfig: func() (string, string, bool) { return "192.168.7.1", "piuser", true },
	})
	sess.Start()
	waitForPiConn(t, sess, piConnConnected, 15*time.Second)

	sess.Stop()
	time.Sleep(100 * time.Millisecond)
	if g := piSessionLoopGoroutines(); g != 0 {
		t.Fatalf("PiSession.loop still running after Stop: %d goroutines", g)
	}
	after := runtime.NumGoroutine()
	if after > before+3 {
		t.Fatalf("goroutine leak: %d after stop, %d before start", after, before)
	}
	sess.Stop() // must be a no-op
}

// /api/pi/status: 503 when no handler is wired (old daemon build / stub)
func TestPiStatus_NoHandlerIs503(t *testing.T) {
	t.Parallel()
	_, base := newTestApiServer(t)
	resp, err := testClient.Get(base + "/api/pi/status")
	if err != nil {
		t.Fatalf("GET /api/pi/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (handler not wired)", resp.StatusCode)
	}
}

// /api/pi/status: response shape {conn, last_attempt_at?, model?, tier?}
func TestPiStatus_EndpointShape(t *testing.T) {
	srv, base := newTestApiServer(t)
	sess := NewPiSession(&librespot.NullLogger{}, PiSessionConfig{
		LoadConfig: func() (string, string, bool) { return "", "", false },
		ModelTier:  func() (string, string) { return "Fake Pi 4", "compute" },
	})
	srv.SetPiSessionHandler(sess)

	resp, err := testClient.Get(base + "/api/pi/status")
	if err != nil {
		t.Fatalf("GET /api/pi/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatalf("decoding body: %v (%s)", err, b)
	}
	if body["conn"] != piConnDisconnected {
		t.Errorf("conn = %v, want %q", body["conn"], piConnDisconnected)
	}
	if _, ok := body["last_attempt_at"]; ok {
		t.Errorf("last_attempt_at present although no attempt happened: %s", b)
	}
	if body["model"] != "Fake Pi 4" || body["tier"] != "compute" {
		t.Errorf("model/tier = %v/%v, want the last known-good state", body["model"], body["tier"])
	}
}
