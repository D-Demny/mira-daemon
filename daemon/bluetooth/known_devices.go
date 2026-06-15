package bluetooth

import (
	"fmt"
	"sort"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// KnownDevice is a known-devices list entry enriched with live BlueZ state
type KnownDevice struct {
	Address       string    `json:"address"`
	Name          string    `json:"name"`
	Starred       bool      `json:"starred"`
	LastConnected time.Time `json:"last_connected"`
	Connected     bool      `json:"connected"`
	Network bool `json:"network"`
}

// sortKnownDevices orders the reconnect list by priority
func sortKnownDevices(devs []librespot.BluetoothKnownDevice) {
	sort.SliceStable(devs, func(i, j int) bool {
		if devs[i].Starred != devs[j].Starred {
			return devs[i].Starred
		}
		return devs[i].LastConnected.After(devs[j].LastConnected)
	})
}

// SeedKnownDevices loads the persisted reconnect list at boot
func (m *Manager) SeedKnownDevices(devs []librespot.BluetoothKnownDevice) {
	m.knownMu.Lock()
	m.knownDevices = append([]librespot.BluetoothKnownDevice(nil), devs...)
	sortKnownDevices(m.knownDevices)
	m.knownMu.Unlock()
	m.log.Debugf("bluetooth: seeded %d known device(s) from persisted state", len(devs))
}

// SetKnownDevicesChangedHandler registers a persistence callback
func (m *Manager) SetKnownDevicesChangedHandler(fn func(devs []librespot.BluetoothKnownDevice)) {
	m.knownMu.Lock()
	m.onKnownDevicesChanged = fn
	m.knownMu.Unlock()
}

// snapshotKnownLocked returns a sorted copy
func (m *Manager) snapshotKnownLocked() []librespot.BluetoothKnownDevice {
	cp := append([]librespot.BluetoothKnownDevice(nil), m.knownDevices...)
	sortKnownDevices(cp)
	return cp
}

// notifyKnownChanged fires the persistence callback
func (m *Manager) notifyKnownChanged(devs []librespot.BluetoothKnownDevice, cb func([]librespot.BluetoothKnownDevice)) {
	if cb != nil {
		cb(devs)
	}
}

// recordPanConnected upserts a device into the reconnect list
func (m *Manager) recordPanConnected(address, name string) {
	m.upsertKnownDevice(address, name, true)
}

// recordPairedDevice adds a freshly paired device to the list right away
func (m *Manager) recordPairedDevice(address, name string) {
	m.upsertKnownDevice(address, name, false)
}

func (m *Manager) upsertKnownDevice(address, name string, bumpRecency bool) {
	m.knownMu.Lock()
	found := false
	for i := range m.knownDevices {
		if m.knownDevices[i].Address != address {
			continue
		}
		if bumpRecency {
			m.knownDevices[i].LastConnected = time.Now()
		}
		if name != "" {
			m.knownDevices[i].Name = name
		}
		found = true
		break
	}
	if !found {
		m.knownDevices = append(m.knownDevices, librespot.BluetoothKnownDevice{
			Address:       address,
			Name:          name,
			LastConnected: time.Now(),
			// the first device the user ever pairs is the priority by default
			Starred: len(m.knownDevices) == 0,
		})
	}
	sortKnownDevices(m.knownDevices)
	devs := m.snapshotKnownLocked()
	cb := m.onKnownDevicesChanged
	m.knownMu.Unlock()

	m.notifyKnownChanged(devs, cb)
}

// StarDevice marks a device as the priority reconnect target
func (m *Manager) StarDevice(address string, starred bool) error {
	m.knownMu.Lock()
	found := false
	for i := range m.knownDevices {
		if m.knownDevices[i].Address == address {
			m.knownDevices[i].Starred = starred
			found = true
		} else if starred {
			m.knownDevices[i].Starred = false
		}
	}
	if !found {
		m.knownMu.Unlock()
		return fmt.Errorf("unknown device %s", address)
	}
	sortKnownDevices(m.knownDevices)
	devs := m.snapshotKnownLocked()
	cb := m.onKnownDevicesChanged
	m.knownMu.Unlock()

	m.log.Infof("bluetooth: device %s starred=%v", address, starred)
	m.notifyKnownChanged(devs, cb)
	return nil
}

// ForgetDevice removes the BlueZ bonding and drops the device 
func (m *Manager) ForgetDevice(address string) error {
	if err := m.RemoveDevice(address); err != nil {
		return err
	}

	m.knownMu.Lock()
	forgotStarred := false
	kept := m.knownDevices[:0]
	for _, d := range m.knownDevices {
		if d.Address == address {
			forgotStarred = d.Starred
			continue
		}
		kept = append(kept, d)
	}
	m.knownDevices = kept
	if forgotStarred && len(m.knownDevices) > 0 {
		oldest := 0
		for i := range m.knownDevices {
			if m.knownDevices[i].LastConnected.Before(m.knownDevices[oldest].LastConnected) {
				oldest = i
			}
		}
		m.knownDevices[oldest].Starred = true
		sortKnownDevices(m.knownDevices)
	}
	devs := m.snapshotKnownLocked()
	cb := m.onKnownDevicesChanged
	m.knownMu.Unlock()

	m.clearManualDisconnect(address)

	m.panMu.Lock()
	if m.lastPanAddress == address {
		m.lastPanAddress = ""
	}
	m.panMu.Unlock()

	m.log.Infof("bluetooth: forgot device %s", address)
	m.notifyKnownChanged(devs, cb)
	return nil
}

// KnownDevices returns the priority-ordered reconnect list
func (m *Manager) KnownDevices() []KnownDevice {
	m.knownMu.Lock()
	devs := m.snapshotKnownLocked()
	m.knownMu.Unlock()

	m.panMu.Lock()
	activePan := m.lastPanAddress
	m.panMu.Unlock()
	panUp := m.NetworkUp()

	out := make([]KnownDevice, 0, len(devs))
	for _, d := range devs {
		kd := KnownDevice{
			Address:       d.Address,
			Name:          d.Name,
			Starred:       d.Starred,
			LastConnected: d.LastConnected,
			Network:       panUp && d.Address == activePan,
		}
		if info, err := m.GetDeviceInfo(d.Address); err == nil {
			kd.Connected = info.Connected
			// prefer the live name
			if info.Alias != "" {
				kd.Name = info.Alias
			} else if info.Name != "" {
				kd.Name = info.Name
			}
		}
		out = append(out, kd)
	}
	return out
}

// reconnectCandidates returns priority-ordered addresses
func (m *Manager) reconnectCandidates() []string {
	m.knownMu.Lock()
	devs := m.snapshotKnownLocked()
	m.knownMu.Unlock()

	var out []string
	for _, d := range devs {
		if m.isManualDisconnect(d.Address) {
			continue
		}
		out = append(out, d.Address)
	}
	return out
}

// topReconnectCandidate returns the highest-priority address or ""
func (m *Manager) topReconnectCandidate() string {
	if c := m.reconnectCandidates(); len(c) > 0 {
		return c[0]
	}
	return ""
}

// manual-disconnect marks
const manualDisconnectTTL = 10 * time.Minute

func (m *Manager) markManualDisconnect(address string) {
	m.manualMu.Lock()
	m.manualDisconnects[address] = time.Now()
	m.manualMu.Unlock()
}

func (m *Manager) clearManualDisconnect(address string) {
	m.manualMu.Lock()
	delete(m.manualDisconnects, address)
	m.manualMu.Unlock()
}

func (m *Manager) isManualDisconnect(address string) bool {
	m.manualMu.Lock()
	defer m.manualMu.Unlock()
	at, ok := m.manualDisconnects[address]
	if !ok {
		return false
	}
	// expired marks self-clear so auto-reconnect resumes
	if time.Since(at) > manualDisconnectTTL {
		delete(m.manualDisconnects, address)
		return false
	}
	return true
}

// ClearManualDisconnects drops all marks
func (m *Manager) ClearManualDisconnects() {
	m.manualMu.Lock()
	cleared := len(m.manualDisconnects)
	m.manualDisconnects = make(map[string]time.Time)
	m.manualMu.Unlock()
	if cleared > 0 {
		m.log.Infof("bluetooth: cleared %d manual-disconnect mark(s)", cleared)
	}
}

func (m *Manager) isKnownDevice(address string) bool {
	m.knownMu.Lock()
	defer m.knownMu.Unlock()
	for _, d := range m.knownDevices {
		if d.Address == address {
			return true
		}
	}
	return false
}
