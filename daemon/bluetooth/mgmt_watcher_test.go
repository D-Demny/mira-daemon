package bluetooth

import (
	"encoding/binary"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// mgmtEvent builds a raw mgmt packet: header {opcode, index, plen} LE + params
func mgmtEvent(opcode uint16, params []byte) []byte {
	buf := make([]byte, 6+len(params))
	binary.LittleEndian.PutUint16(buf[0:2], opcode)
	binary.LittleEndian.PutUint16(buf[2:4], 0) // controller index
	binary.LittleEndian.PutUint16(buf[4:6], uint16(len(params)))
	copy(buf[6:], params)
	return buf
}

// disconnectParams: bdaddr is wire-order (reversed), type, reason
func disconnectParams(reversedAddr [6]byte, reason byte) []byte {
	p := make([]byte, 8)
	copy(p, reversedAddr[:])
	p[6] = 0x00 // BR/EDR
	p[7] = reason
	return p
}

func TestParseMgmtDisconnect_ValidEvent(t *testing.T) {
	t.Parallel()

	// AA:BB:CC:DD:EE:FF arrives reversed on the wire
	buf := mgmtEvent(mgmtEvDeviceDisconnected,
		disconnectParams([6]byte{0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA}, mgmtReasonRemote))

	addr, reason, ok := parseMgmtDisconnect(buf)
	if !ok {
		t.Fatal("expected ok")
	}
	if addr != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("addr: got %q", addr)
	}
	if reason != mgmtReasonRemote {
		t.Errorf("reason: got %d", reason)
	}
}

func TestParseMgmtDisconnect_RejectsOtherOpcodesAndShortPackets(t *testing.T) {
	t.Parallel()

	cases := map[string][]byte{
		"other opcode":   mgmtEvent(0x000B, disconnectParams([6]byte{}, 1)),
		"short header":   {0x0C, 0x00, 0x00},
		"short params":   mgmtEvent(mgmtEvDeviceDisconnected, []byte{1, 2, 3}),
		"plen oversells": func() []byte { b := mgmtEvent(mgmtEvDeviceDisconnected, disconnectParams([6]byte{}, 1)); return b[:10] }(),
	}
	for name, buf := range cases {
		if _, _, ok := parseMgmtDisconnect(buf); ok {
			t.Errorf("%s: expected !ok", name)
		}
	}
}

func newWatcherTestManager(addr string) *Manager {
	m := newTestManager()
	m.SeedKnownDevices([]librespot.BluetoothKnownDevice{
		{Address: addr, LastConnected: time.Now()},
	})
	return m
}

func TestHandleDisconnectReason_RemoteAfterRealUseMarksManual(t *testing.T) {
	t.Parallel()

	m := newWatcherTestManager("AA:BB:CC:DD:EE:FF")
	m.manualMu.Lock()
	m.connectedSince["AA:BB:CC:DD:EE:FF"] = time.Now().Add(-5 * time.Minute)
	m.manualMu.Unlock()
	m.setPanSessionUp("AA:BB:CC:DD:EE:FF")

	m.handleDisconnectReason("AA:BB:CC:DD:EE:FF", mgmtReasonRemote)

	if !m.isManualDisconnect("AA:BB:CC:DD:EE:FF") {
		t.Fatal("remote disconnect after real use should mark manual")
	}
	if m.panSessionWasUp("AA:BB:CC:DD:EE:FF") {
		t.Fatal("the ACL session ended, the PAN-session flag must reset")
	}
}

func TestHandleDisconnectReason_RemoteWithoutPanSessionKeepsReconnect(t *testing.T) {
	t.Parallel()

	m := newWatcherTestManager("AA:BB:CC:DD:EE:FF")
	m.manualMu.Lock()
	m.connectedSince["AA:BB:CC:DD:EE:FF"] = time.Now().Add(-5 * time.Minute)
	m.manualMu.Unlock()

	m.handleDisconnectReason("AA:BB:CC:DD:EE:FF", mgmtReasonRemote)

	if m.isManualDisconnect("AA:BB:CC:DD:EE:FF") {
		t.Fatal("remote disconnect with no PAN this session must not mark manual")
	}
}

func TestHandleDisconnectReason_RemoteRightAfterConnectIsProfileChurn(t *testing.T) {
	t.Parallel()

	m := newWatcherTestManager("AA:BB:CC:DD:EE:FF")
	m.manualMu.Lock()
	m.connectedSince["AA:BB:CC:DD:EE:FF"] = time.Now().Add(-2 * time.Second)
	m.manualMu.Unlock()

	m.handleDisconnectReason("AA:BB:CC:DD:EE:FF", mgmtReasonRemote)

	if m.isManualDisconnect("AA:BB:CC:DD:EE:FF") {
		t.Fatal("remote disconnect seconds after connect must not mark manual (Samsung post-pair teardown)")
	}
}

func TestHandleDisconnectReason_RemoteWithoutConnectTimestampMarksManual(t *testing.T) {
	t.Parallel()

	// device connected before the daemon started
	m := newWatcherTestManager("AA:BB:CC:DD:EE:FF")
	m.setPanSessionUp("AA:BB:CC:DD:EE:FF")
	m.handleDisconnectReason("AA:BB:CC:DD:EE:FF", mgmtReasonRemote)

	if !m.isManualDisconnect("AA:BB:CC:DD:EE:FF") {
		t.Fatal("remote disconnect with unknown uptime but PAN up should mark manual")
	}
}

func TestHandleDisconnectReason_RemoteAfterPanDropKeepsReconnecting(t *testing.T) {
	t.Parallel()

	m := newWatcherTestManager("AA:BB:CC:DD:EE:FF")
	// tethering-off: the PAN (bnep0) drops, then the phone drops the ACL right
	// after. that's a network change, not a deliberate "stop reconnecting".
	m.markNetworkDrop()
	m.handleDisconnectReason("AA:BB:CC:DD:EE:FF", mgmtReasonRemote)

	if m.isManualDisconnect("AA:BB:CC:DD:EE:FF") {
		t.Fatal("remote disconnect right after a PAN drop must not suppress auto-reconnect")
	}
}

func TestHandleDisconnectReason_RemoteUnknownDeviceIgnored(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	m.handleDisconnectReason("AA:BB:CC:DD:EE:FF", mgmtReasonRemote)

	if m.isManualDisconnect("AA:BB:CC:DD:EE:FF") {
		t.Fatal("devices outside the known list must not be marked")
	}
}

func TestHandleDisconnectReason_TimeoutClearsManualMark(t *testing.T) {
	t.Parallel()

	// not in the known list so the early-page goroutine (which needs D-Bus)
	// doesn't spawn; the clear happens before the known-device gate
	m := newTestManager()
	m.markManualDisconnect("AA:BB:CC:DD:EE:FF")

	m.handleDisconnectReason("AA:BB:CC:DD:EE:FF", mgmtReasonTimeout)

	if m.isManualDisconnect("AA:BB:CC:DD:EE:FF") {
		t.Fatal("timeout (out of range) must clear a manual mark so reconnect resumes")
	}
}

func TestHandleDisconnectReason_LocalReasonsNoPolicyChange(t *testing.T) {
	t.Parallel()

	m := newWatcherTestManager("AA:BB:CC:DD:EE:FF")
	for _, reason := range []uint8{mgmtReasonUnknown, mgmtReasonLocal, mgmtReasonAuth, mgmtReasonSuspend} {
		m.handleDisconnectReason("AA:BB:CC:DD:EE:FF", reason)
		if m.isManualDisconnect("AA:BB:CC:DD:EE:FF") {
			t.Fatalf("reason %d must not mark manual", reason)
		}
	}
}
