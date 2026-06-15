package bluetooth

import (
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

func dev(addr, name string, starred bool, last time.Time) librespot.BluetoothKnownDevice {
	return librespot.BluetoothKnownDevice{Address: addr, Name: name, Starred: starred, LastConnected: last}
}

func TestSortKnownDevices_StarredFirstThenRecency(t *testing.T) {
	t.Parallel()

	now := time.Now()
	devs := []librespot.BluetoothKnownDevice{
		dev("AA", "old", false, now.Add(-2*time.Hour)),
		dev("BB", "newest", false, now),
		dev("CC", "starred-old", true, now.Add(-24*time.Hour)),
	}
	sortKnownDevices(devs)

	if devs[0].Address != "CC" {
		t.Fatalf("starred device should sort first, got %s", devs[0].Address)
	}
	if devs[1].Address != "BB" || devs[2].Address != "AA" {
		t.Fatalf("unstarred devices should sort by recency, got %s, %s", devs[1].Address, devs[2].Address)
	}
}

func TestRecordPanConnected_InsertsAndBumpsRecency(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	var persisted [][]librespot.BluetoothKnownDevice
	m.SetKnownDevicesChangedHandler(func(devs []librespot.BluetoothKnownDevice) {
		persisted = append(persisted, devs)
	})

	m.recordPanConnected("AA:BB:CC:DD:EE:FF", "Phone A")
	m.recordPanConnected("11:22:33:44:55:66", "Phone B")

	if len(persisted) != 2 {
		t.Fatalf("expected 2 persistence callbacks, got %d", len(persisted))
	}
	latest := persisted[1]
	if len(latest) != 2 {
		t.Fatalf("expected 2 known devices, got %d", len(latest))
	}
	// A holds the default first-device star, so it sorts first even though B
	// connected more recently; recency is tracked underneath
	if latest[0].Address != "AA:BB:CC:DD:EE:FF" || !latest[0].Starred {
		t.Fatalf("first-paired device should be starred and first, got %+v", latest[0])
	}
	recency := func(devs []librespot.BluetoothKnownDevice, addr string) time.Time {
		for _, d := range devs {
			if d.Address == addr {
				return d.LastConnected
			}
		}
		t.Fatalf("device %s missing", addr)
		return time.Time{}
	}
	if !recency(latest, "11:22:33:44:55:66").After(recency(latest, "AA:BB:CC:DD:EE:FF")) {
		t.Fatalf("B connected later, its recency should be newer")
	}

	// reconnecting A bumps its recency past B and keeps its name when empty
	m.recordPanConnected("AA:BB:CC:DD:EE:FF", "")
	latest = persisted[2]
	if !recency(latest, "AA:BB:CC:DD:EE:FF").After(recency(latest, "11:22:33:44:55:66")) {
		t.Fatalf("PAN reconnect should bump recency")
	}
	if latest[0].Name != "Phone A" {
		t.Fatalf("upsert should keep prior name on empty, got %+v", latest[0])
	}
	if len(latest) != 2 {
		t.Fatalf("upsert must not duplicate, got %d entries", len(latest))
	}
}

func TestRecordPairedDevice_FirstDeviceStarredByDefault(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	m.recordPairedDevice("AA:BB:CC:DD:EE:FF", "Phone A")
	m.recordPairedDevice("11:22:33:44:55:66", "PC")

	m.knownMu.Lock()
	defer m.knownMu.Unlock()
	if len(m.knownDevices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(m.knownDevices))
	}
	for _, d := range m.knownDevices {
		if d.Address == "AA:BB:CC:DD:EE:FF" && !d.Starred {
			t.Errorf("first paired device should be starred by default")
		}
		if d.Address == "11:22:33:44:55:66" && d.Starred {
			t.Errorf("second paired device must not steal the star")
		}
	}
}

func TestRecordPairedDevice_DoesNotBumpRecencyOrDuplicate(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	m.recordPanConnected("AA", "Phone A")
	m.recordPanConnected("BB", "Phone B") // most recent

	// re-pairing A (or an ACL-only event) must not jump it over B
	m.recordPairedDevice("AA", "Phone A renamed")

	m.knownMu.Lock()
	defer m.knownMu.Unlock()
	if len(m.knownDevices) != 2 {
		t.Fatalf("upsert duplicated: %d entries", len(m.knownDevices))
	}
	// A is starred (first device) so it sorts first regardless; verify recency
	// directly instead of via order
	var a, b time.Time
	for _, d := range m.knownDevices {
		if d.Address == "AA" {
			a = d.LastConnected
			if d.Name != "Phone A renamed" {
				t.Errorf("paired upsert should refresh the name, got %q", d.Name)
			}
		}
		if d.Address == "BB" {
			b = d.LastConnected
		}
	}
	if a.After(b) {
		t.Errorf("recordPairedDevice must not bump recency past a later PAN connect")
	}
}

func TestStarDevice_SingleStarEnforced(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	m.SeedKnownDevices([]librespot.BluetoothKnownDevice{
		dev("AA", "a", true, time.Now()),
		dev("BB", "b", false, time.Now().Add(-time.Hour)),
	})

	if err := m.StarDevice("BB", true); err != nil {
		t.Fatalf("StarDevice: %v", err)
	}

	c := m.reconnectCandidates()
	if c[0] != "BB" {
		t.Fatalf("newly starred device should be top priority, got %s", c[0])
	}

	m.knownMu.Lock()
	for _, d := range m.knownDevices {
		if d.Address == "AA" && d.Starred {
			t.Errorf("starring BB should have unstarred AA")
		}
	}
	m.knownMu.Unlock()

	if err := m.StarDevice("ZZ", true); err == nil {
		t.Fatalf("starring an unknown device should error")
	}
}

func TestReconnectCandidates_ExcludesManualDisconnects(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	m.SeedKnownDevices([]librespot.BluetoothKnownDevice{
		dev("AA", "a", false, time.Now()),
		dev("BB", "b", false, time.Now().Add(-time.Hour)),
	})

	m.markManualDisconnect("AA")
	c := m.reconnectCandidates()
	if len(c) != 1 || c[0] != "BB" {
		t.Fatalf("manually disconnected device must be excluded, got %v", c)
	}

	// device coming back clears the mark via the connect path
	m.clearManualDisconnect("AA")
	if got := m.reconnectCandidates(); len(got) != 2 {
		t.Fatalf("cleared mark should restore candidate, got %v", got)
	}

	m.markManualDisconnect("AA")
	m.markManualDisconnect("BB")
	if got := m.reconnectCandidates(); len(got) != 0 {
		t.Fatalf("all marked -> no candidates, got %v", got)
	}
	m.ClearManualDisconnects()
	if got := m.reconnectCandidates(); len(got) != 2 {
		t.Fatalf("ClearManualDisconnects should restore all, got %v", got)
	}
}

func TestManualDisconnect_ExpiresAfterTTL(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	m.SeedKnownDevices([]librespot.BluetoothKnownDevice{
		dev("AA", "a", false, time.Now()),
	})

	m.markManualDisconnect("AA")
	if !m.isManualDisconnect("AA") {
		t.Fatal("fresh mark should suppress reconnect")
	}
	if got := m.reconnectCandidates(); len(got) != 0 {
		t.Fatalf("fresh mark -> no candidates, got %v", got)
	}

	// age the mark past the TTL
	m.manualMu.Lock()
	m.manualDisconnects["AA"] = time.Now().Add(-manualDisconnectTTL - time.Minute)
	m.manualMu.Unlock()

	if m.isManualDisconnect("AA") {
		t.Fatal("mark older than the TTL should self-clear")
	}
	if got := m.reconnectCandidates(); len(got) != 1 || got[0] != "AA" {
		t.Fatalf("expired mark should restore the candidate, got %v", got)
	}
	// the read should have deleted the stale entry
	m.manualMu.Lock()
	_, present := m.manualDisconnects["AA"]
	m.manualMu.Unlock()
	if present {
		t.Fatal("expired mark should be deleted from the map")
	}
}

func TestTopReconnectCandidate_EmptyAndPriority(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	if got := m.topReconnectCandidate(); got != "" {
		t.Fatalf("empty list should yield empty candidate, got %q", got)
	}

	m.SeedKnownDevices([]librespot.BluetoothKnownDevice{
		dev("AA", "a", false, time.Now()),
		dev("BB", "b", true, time.Now().Add(-time.Hour)),
	})
	if got := m.topReconnectCandidate(); got != "BB" {
		t.Fatalf("starred device should be top candidate, got %q", got)
	}
}
