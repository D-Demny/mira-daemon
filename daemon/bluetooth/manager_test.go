package bluetooth

import (
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// minimal Manager for flow-control tests, no D-Bus wired up
func newTestManager() *Manager {
	return &Manager{
		log:               &librespot.NullLogger{},
		manualDisconnects: make(map[string]time.Time),
		connectedSince:    make(map[string]time.Time),
	}
}

// SetOfflineRetry start/stop lifecycle
func TestOfflineRetry_StartStopLifecycle(t *testing.T) {
	m := newTestManager()
	t.Cleanup(func() { m.SetOfflineRetry(false) })

	if m.offlineRetryStop != nil {
		t.Fatalf("expected initial offlineRetryStop to be nil")
	}

	m.SetOfflineRetry(true)
	if m.offlineRetryStop == nil {
		t.Fatalf("SetOfflineRetry(true) should have allocated stop channel")
	}
	firstStop := m.offlineRetryStop

	// idempotent re-start
	m.SetOfflineRetry(true)
	if m.offlineRetryStop != firstStop {
		t.Fatalf("SetOfflineRetry(true) when already running should be a no-op; got new channel")
	}

	m.SetOfflineRetry(false)
	if m.offlineRetryStop != nil {
		t.Fatalf("SetOfflineRetry(false) should have cleared stop channel")
	}

	m.SetOfflineRetry(true)
	if m.offlineRetryStop == nil {
		t.Fatalf("SetOfflineRetry(true) after stop should restart loop")
	}
	if m.offlineRetryStop == firstStop {
		t.Fatalf("restarted loop should use a fresh stop channel")
	}
}

// stop must not panic on a nil channel
func TestOfflineRetry_DoubleStopSafe(t *testing.T) {
	m := newTestManager()

	m.SetOfflineRetry(true)
	m.SetOfflineRetry(false)

	m.SetOfflineRetry(false)
}

// loop goroutine must observe the close and return
func TestOfflineRetry_GoroutineExitsOnStop(t *testing.T) {
	m := newTestManager()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		m.offlineRetryLoop(stop)
		close(done)
	}()

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("offlineRetryLoop did not exit within 2s of stop close")
	}
}
