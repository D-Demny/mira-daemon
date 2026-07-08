package bluetooth

import (
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

func TestPanBackoff_EscalatesCapsAndClears(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	if m.inPanBackoff("AA") {
		t.Fatal("no backoff bumped yet")
	}

	if d := m.bumpPanBackoff("AA"); d != panBackoffMin {
		t.Fatalf("first bump = %s, want %s", d, panBackoffMin)
	}
	if !m.inPanBackoff("AA") {
		t.Fatal("bump must pause the address")
	}
	if d := m.bumpPanBackoff("AA"); d != 2*panBackoffMin {
		t.Fatalf("second bump = %s, want %s", d, 2*panBackoffMin)
	}
	for i := 0; i < 10; i++ {
		m.bumpPanBackoff("AA")
	}
	if d := m.bumpPanBackoff("AA"); d != panBackoffMax {
		t.Fatalf("escalation must cap at %s, got %s", panBackoffMax, d)
	}

	// other addresses are unaffected
	if m.inPanBackoff("BB") {
		t.Fatal("backoff must be per-address")
	}

	m.clearPanBackoff("AA")
	if m.inPanBackoff("AA") {
		t.Fatal("clear must lift the pause")
	}
	if d := m.bumpPanBackoff("AA"); d != panBackoffMin {
		t.Fatalf("clear must reset the escalation, got %s", d)
	}
}

func TestPanBackoffPauseClearKeepsEscalation(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	m.bumpPanBackoff("AA")
	m.clearPanBackoffPause("AA")
	if m.inPanBackoff("AA") {
		t.Fatal("pause-clear must lift the pause")
	}
	if d := m.bumpPanBackoff("AA"); d != 2*panBackoffMin {
		t.Fatalf("escalation memory must survive a PAN-up, got %s", d)
	}

	m.panBackoffMu.Lock()
	m.panBackoffBumped["AA"] = time.Now().Add(-panBackoffDecayAfter - time.Minute)
	m.panBackoffMu.Unlock()
	if d := m.bumpPanBackoff("AA"); d != panBackoffMin {
		t.Fatalf("stale escalation must decay to the minimum, got %s", d)
	}
}

func TestReconnectCandidates_SkipsBackoff(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	m.SeedKnownDevices([]librespot.BluetoothKnownDevice{
		{Address: "AA", LastConnected: time.Now()},
		{Address: "BB", LastConnected: time.Now().Add(-time.Hour)},
	})

	m.bumpPanBackoff("AA")
	got := m.reconnectCandidates()
	if len(got) != 1 || got[0] != "BB" {
		t.Fatalf("backing-off address must be skipped, got %v", got)
	}

	m.clearPanBackoff("AA")
	if got := m.reconnectCandidates(); len(got) != 2 {
		t.Fatalf("cleared address must be a candidate again, got %v", got)
	}
}

func TestConsumeSelfTeardown(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	if m.consumeSelfTeardown() {
		t.Fatal("nothing torn down yet")
	}
	m.noteSelfTeardown()
	if !m.consumeSelfTeardown() {
		t.Fatal("a just-noted teardown must be consumed")
	}
	// one teardown = one DELLINK: a second drop inside the window is REAL
	if m.consumeSelfTeardown() {
		t.Fatal("the note must be one-shot")
	}

	// stale note no longer suppresses drops
	m.noteSelfTeardown()
	m.selfTeardownMu.Lock()
	m.selfTeardownAt = time.Now().Add(-time.Minute)
	m.selfTeardownMu.Unlock()
	if m.consumeSelfTeardown() {
		t.Fatal("a minute-old teardown is not recent")
	}
}
