package bluetooth

import (
	"encoding/binary"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// The kernels BT mgmt control channel broadcasts DEVICE_DISCONNECTED events
// carrying the HCI disconnect reason, which BlueZ never exposes over D-Bus
const (
	hciChannelControl = 3      // HCI_CHANNEL_CONTROL
	hciDevNone        = 0xffff // HCI_DEV_NONE: events from all controllers

	mgmtEvDeviceDisconnected = 0x000C

	// mgmt-api.txt disconnect reasons
	mgmtReasonUnknown = 0x00
	mgmtReasonTimeout = 0x01 // supervision timeout: out of range / link lost
	mgmtReasonLocal   = 0x02 // terminated by local host (our own Disconnect)
	mgmtReasonRemote  = 0x03 // terminated by remote host (user's choice)
	mgmtReasonAuth    = 0x04
	mgmtReasonSuspend = 0x05 // local host suspending
)

// a remote-initiated drop within this window after connecting is treated as profile-negotiation teardown
const manualDisconnectMinUptime = 45 * time.Second

// a remote disconnect within this window of a bnep0/PAN drop is not deliberate
const networkDropGrace = 15 * time.Second

func (m *Manager) markNetworkDrop() {
	m.networkDropMu.Lock()
	m.lastNetworkDropAt = time.Now()
	m.networkDropMu.Unlock()
}

func (m *Manager) recentNetworkDrop() bool {
	m.networkDropMu.Lock()
	t := m.lastNetworkDropAt
	m.networkDropMu.Unlock()
	return !t.IsZero() && time.Since(t) < networkDropGrace
}

// watchMgmtDisconnects opens the mgmt control channel and feeds disconnect reasons into the manager
func (m *Manager) watchMgmtDisconnects() error {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return fmt.Errorf("mgmt socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: hciDevNone, Channel: hciChannelControl}); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("mgmt bind: %w", err)
	}

	go func() {
		defer func() { _ = unix.Close(fd) }()
		buf := make([]byte, 1024)
		for {
			n, err := unix.Read(fd, buf)
			if err != nil {
				if err == unix.EINTR {
					continue
				}
				m.log.WithError(err).Warn("bluetooth: mgmt event socket died, disconnect reasons unavailable")
				return
			}
			if addr, reason, ok := parseMgmtDisconnect(buf[:n]); ok {
				m.handleDisconnectReason(addr, reason)
			}
		}
	}()

	m.log.Info("bluetooth: mgmt disconnect-reason watcher active")
	return nil
}

// parseMgmtDisconnect decodes one mgmt event packet
func parseMgmtDisconnect(buf []byte) (addr string, reason uint8, ok bool) {
	if len(buf) < 6 {
		return "", 0, false
	}
	opcode := binary.LittleEndian.Uint16(buf[0:2])
	plen := int(binary.LittleEndian.Uint16(buf[4:6]))
	if opcode != mgmtEvDeviceDisconnected || plen < 8 || len(buf) < 6+plen {
		return "", 0, false
	}
	p := buf[6:]
	addr = fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", p[5], p[4], p[3], p[2], p[1], p[0])
	return addr, p[7], true
}

// mgmtReasonName is a human label for the coarse mgmt disconnect reason
// ( 0x13/0x14/0x15 -> remote, 0x08 -> timeout, 0x16 -> local).
func mgmtReasonName(reason uint8) string {
	switch reason {
	case mgmtReasonUnknown:
		return "unknown"
	case mgmtReasonTimeout:
		return "timeout/out-of-range"
	case mgmtReasonLocal:
		return "local-host"
	case mgmtReasonRemote:
		return "remote-terminated"
	case mgmtReasonAuth:
		return "auth-failure"
	case mgmtReasonSuspend:
		return "suspend"
	default:
		return "?"
	}
}

// handleDisconnectReason classifies a disconnect by its HCI reason
func (m *Manager) handleDisconnectReason(address string, reason uint8) {
	// diagnostic
	m.log.Infof("bluetooth: mgmt disconnect %s reason=0x%02x (%s), recentPanDrop=%t",
		address, reason, mgmtReasonName(reason), m.recentNetworkDrop())

	switch reason {
	case mgmtReasonRemote:
		if !m.isKnownDevice(address) {
			return
		}
		// post pair profile teardown
		if uptime, ok := m.connectionUptime(address); ok && uptime < manualDisconnectMinUptime {
			m.clearPanSession(address)
			m.log.Infof("bluetooth: %s disconnected by remote %.0fs after connecting, treating as profile churn (will still auto-reconnect)",
				address, uptime.Seconds())
			return
		}
		// the PAN just dropped:
		if m.recentNetworkDrop() {
			m.clearPanSession(address)
			m.clearManualDisconnect(address)
			m.log.Infof("bluetooth: %s disconnected right after PAN dropped tethering/network change, keeping auto-reconnect", address)
			return
		}
		// PAN never came up this ACL session
		if !m.panSessionWasUp(address) {
			m.log.Infof("bluetooth: %s disconnected by remote but PAN was never up this session, keeping auto-reconnect", address)
			return
		}
		m.clearPanSession(address)
		m.markManualDisconnect(address)
		m.log.Infof("bluetooth: %s disconnected from the phone side, pausing auto-reconnect (reconnect from the phone or the Bluetooth menu)", address)

	case mgmtReasonTimeout:
		// link lost
		m.clearPanSession(address)
		m.clearManualDisconnect(address)
		if m.isKnownDevice(address) {
			m.log.Infof("bluetooth: link to %s lost (timeout), reconnecting when it's back in range", address)
			go func() {
				time.Sleep(3 * time.Second)
				m.tryActiveReconnect([]string{address})
			}()
		}

	default:
		// local/suspend/auth
		m.clearPanSession(address)
	}
}

// connectionUptime reports how long the device had been ACL-connected before the disconnect
func (m *Manager) connectionUptime(address string) (time.Duration, bool) {
	m.manualMu.Lock()
	since, ok := m.connectedSince[address]
	m.manualMu.Unlock()
	if !ok {
		return 0, false
	}
	return time.Since(since), true
}
