package bluetooth

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/vishvananda/netlink"

	librespot "github.com/devgianlu/go-librespot"
)

// upper bound on any single D-Bus call
const dbusCallTimeout = 5 * time.Second

type Manager struct {
	log                librespot.Logger
	conn               *dbus.Conn
	adapter            dbus.ObjectPath
	agent              *agent
	emit               Emitter
	mu                 sync.Mutex
	pendingDisconnects sync.Map

	// most recent PAN-connected device
	panMu          sync.Mutex
	lastPanAddress string

	// serialize PAN connect/reconnect attempts without blocking
	networkMu sync.Mutex

	// when non-nil, signals an active background loop retrying PAN while offline
	offlineRetryMu   sync.Mutex
	offlineRetryStop chan struct{}

	// assume the user intentionally turned tethering off
	panRetryMu      sync.Mutex
	panRetryHistory []time.Time

	// daemon can persist it across reboots
	onLastPanAddressChanged func(addr string)

	// prevents overlapping Device1.Connect calls
	activeReconnectMu       sync.Mutex
	activeReconnectInFlight bool
}

const (
	panRetryThreshold = 3
	panRetryWindow    = 30 * time.Second
)

func NewManager(log librespot.Logger, emit Emitter) (*Manager, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("connect to system bus: %w", err)
	}
	log.Info("bluetooth: connected to system bus")

	// daemon may race ahead of BlueZ on cold boot
	var adapter dbus.ObjectPath
	for attempt := 1; attempt <= 10; attempt++ {
		adapter, err = findDefaultAdapter(conn)
		if err == nil {
			break
		}
		log.Debugf("bluetooth: adapter not ready (attempt %d/10): %v", attempt, err)
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("find bluetooth adapter: %w", err)
	}
	log.Infof("bluetooth: using adapter %s", adapter)

	m := &Manager{
		log:     log,
		conn:    conn,
		adapter: adapter,
		emit:    emit,
	}

	a, err := newAgent(log, conn, m)
	if err != nil {
		return nil, fmt.Errorf("register bluetooth agent: %w", err)
	}
	m.agent = a

	if err := m.setPower(true); err != nil {
		return nil, fmt.Errorf("power on adapter: %w", err)
	}

	m.monitorDisconnects()
	m.monitorNetworkInterfaces()

	// open for pairing on boot, network monitor takes over after
	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := m.SetDiscoverable(true); err != nil {
			m.log.WithError(err).Warn("bluetooth: failed to enable initial discoverable")
		}
	}()

	return m, nil
}

func findDefaultAdapter(conn *dbus.Conn) (dbus.ObjectPath, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbusCallTimeout)
	defer cancel()

	var owner string
	obj := conn.Object("org.freedesktop.DBus", "/org/freedesktop/DBus")
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, "org.bluez").Store(&owner); err != nil {
		return "", fmt.Errorf("get bluez owner: %w", err)
	}

	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	obj = conn.Object("org.bluez", "/")
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objects); err != nil {
		return "", fmt.Errorf("get managed objects: %w", err)
	}

	for path, interfaces := range objects {
		if _, hasAdapter := interfaces[bluezAdapterInterface]; hasAdapter {
			return path, nil
		}
	}

	return "", fmt.Errorf("no bluetooth adapter found")
}

func (m *Manager) monitorDisconnects() {
	if err := m.conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchPathNamespace("/org/bluez"),
	); err != nil {
		m.log.WithError(err).Error("bluetooth: failed to subscribe to property changes")
		return
	}

	signals := make(chan *dbus.Signal, 10)
	m.conn.Signal(signals)

	go func() {
		for signal := range signals {
			if signal.Name != "org.freedesktop.DBus.Properties.PropertiesChanged" {
				continue
			}
			if len(signal.Body) < 3 {
				continue
			}

			iface, _ := signal.Body[0].(string)
			if iface != bluezDeviceInterface {
				continue
			}

			changes, _ := signal.Body[1].(map[string]dbus.Variant)

			devicePath := string(signal.Path)
			address := strings.TrimPrefix(devicePath, string(m.adapter)+"/dev_")
			address = strings.ReplaceAll(address, "_", ":")

			if pairedV, ok := changes["Paired"]; ok {
				if paired, _ := pairedV.Value().(bool); paired {
					m.handleDevicePaired(devicePath, address)
				}
			}

			if connectedV, ok := changes["Connected"]; ok {
				connected, _ := connectedV.Value().(bool)
				if connected {
					m.handleDeviceConnected(devicePath, address)
				} else {
					m.handleDeviceDisconnected(devicePath, address)
				}
			}
		}
	}()
}

// emits EventPaired once per pair
func (m *Manager) handleDevicePaired(devicePath, address string) {
	m.log.Infof("bluetooth: device paired: %s", devicePath)

	// clear pending pair so a stale Cancel() doesn't fire later
	if m.agent != nil {
		m.agent.clearCurrentIfDevice(devicePath)
	}

	info, err := m.GetDeviceInfo(address)
	if err != nil {
		m.log.WithError(err).Debugf("bluetooth: failed to enrich paired event for %s", address)
		info = &DeviceInfo{Address: address, Paired: true}
	}

	if m.emit != nil {
		m.emit(EventPaired, DevicePairedPayload{Device: info})
	}
}

// fires on Connected:true
func (m *Manager) handleDeviceConnected(devicePath, address string) {
	m.log.Infof("bluetooth: device connected: %s", devicePath)

	info, err := m.GetDeviceInfo(address)
	if err != nil {
		m.log.WithError(err).Debugf("bluetooth: failed to enrich connect event for %s", address)
		if m.emit != nil {
			m.emit(EventConnect, DeviceConnectedPayload{Address: address})
		}
	} else if m.emit != nil {
		m.emit(EventConnect, DeviceConnectedPayload{Device: info})
	}

	// only chase PAN for paired devices, random discovering peripherals shouldn't trigger Connect
	if info == nil || !info.Paired {
		return
	}

	// gate auto-PAN on the peer actually advertising NAP
	// iPhone doesn't publish NAP UUID until Personal Hotspot is on so we wait 20s with one Device1.Connect nudge
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		available, err := m.waitForNAPService(ctx, address)
		if err != nil {
			m.log.WithError(err).Debugf("bluetooth: NAP wait aborted for %s", address)
			return
		}
		if !available {
			m.log.Warnf("bluetooth: %s never advertised PAN-NAP, peer likely has tethering disabled", address)
			if m.emit != nil {
				m.emit(EventNAPUnavailable, NetworkConnectedPayload{Address: address})
			}
			return
		}

		if err := m.ConnectNetwork(address); err != nil {
			m.log.WithError(err).Warnf("bluetooth: auto-PAN failed for %s after NAP advertised", address)
			return
		}
		m.log.Debugf("bluetooth: auto-PAN succeeded for %s", address)
	}()
}

// polls Device1.UUIDs for the NAP UUID
func (m *Manager) waitForNAPService(ctx context.Context, address string) (bool, error) {
	devicePath := formatDevicePath(m.adapter, address)
	obj := m.conn.Object(bluezBusName, devicePath)

	check := func() (hasNAP bool, resolved bool, connected bool, err error) {
		props := make(map[string]dbus.Variant)
		if err := m.dbusCall(obj, "org.freedesktop.DBus.Properties.GetAll", bluezDeviceInterface).Store(&props); err != nil {
			return false, false, false, err
		}
		if v, ok := props["Connected"]; ok {
			connected, _ = v.Value().(bool)
		}
		if v, ok := props["ServicesResolved"]; ok {
			resolved, _ = v.Value().(bool)
		}
		if v, ok := props["UUIDs"]; ok {
			if uuids, ok := v.Value().([]string); ok {
				for _, u := range uuids {
					if strings.EqualFold(u, panNAPUUID) {
						hasNAP = true
						break
					}
				}
			}
		}
		return hasNAP, resolved, connected, nil
	}

	nudged := false
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		hasNAP, resolved, connected, err := check()
		if err != nil {
			return false, err
		}
		if !connected {
			return false, fmt.Errorf("device %s disconnected while waiting for NAP", address)
		}
		if hasNAP {
			return true, nil
		}
		if resolved && !nudged {
			nudged = true
			m.log.Debugf("bluetooth: %s ServicesResolved=true but no NAP yet, nudging via Device1.Connect", address)
			if err := m.dbusCall(obj, bluezDeviceInterface+".Connect").Err; err != nil {
				m.log.WithError(err).Debugf("bluetooth: Device1.Connect nudge failed for %s (continuing to poll)", address)
			}
		}

		select {
		case <-ctx.Done():
			return false, nil
		case <-ticker.C:
		}
	}
}

func (m *Manager) handleDeviceDisconnected(devicePath, address string) {
	if _, pending := m.pendingDisconnects.LoadAndDelete(address); !pending {
		if m.emit != nil {
			m.emit(EventDisconnect, DeviceDisconnectedPayload{Address: address})
		}
	}

	m.log.Infof("bluetooth: device disconnected: %s", devicePath)

	if m.agent != nil {
		m.agent.clearCurrentIfDevice(devicePath)
	}
}

func (m *Manager) monitorNetworkInterfaces() {
	linkUpdates := make(chan netlink.LinkUpdate)
	done := make(chan struct{})

	if err := netlink.LinkSubscribe(linkUpdates, done); err != nil {
		m.log.WithError(err).Error("bluetooth: failed to subscribe to netlink updates")
		return
	}

	go func() {
		for update := range linkUpdates {
			if update.Header.Type == syscall.RTM_DELLINK && update.Link.Attrs().Name == panInterface {
				m.log.Info("bluetooth: bnep0 interface removed")
				if m.emit != nil {
					m.emit(EventNetworkDisconnect, nil)
				}
				m.tryRecoverPan()
			}
		}
	}()
}

// tries to re-establish PAN if the BT link is still up
func (m *Manager) tryRecoverPan() {
	m.panMu.Lock()
	addr := m.lastPanAddress
	m.panMu.Unlock()
	if addr == "" {
		return
	}

	allowed, recentDrops := m.shouldRetryPan(time.Now())
	if !allowed {
		m.log.Warnf("bluetooth: PAN dropped %d times in %s, backing off auto-recover (assuming intentional disconnect)",
			recentDrops+1, panRetryWindow)
		return
	}

	go func() {
		time.Sleep(1 * time.Second)
		info, err := m.GetDeviceInfo(addr)
		if err != nil {
			m.log.WithError(err).Debugf("bluetooth: PAN auto-recover skipped for %s (no device info)", addr)
			return
		}
		if !info.Connected {
			m.log.Debugf("bluetooth: PAN auto-recover skipped for %s (BT link is down too)", addr)
			return
		}
		m.log.Infof("bluetooth: bnep0 dropped while %s still BT-connected, retrying PAN", addr)
		if err := m.ConnectNetwork(addr); err != nil {
			m.log.WithError(err).Warn("bluetooth: PAN auto-recover failed")
		}
	}()
}

func (m *Manager) shouldRetryPan(now time.Time) (allowed bool, recentDrops int) {
	m.panRetryMu.Lock()
	defer m.panRetryMu.Unlock()

	// prune via slice[:0] trick, no alloc per call
	cutoff := now.Add(-panRetryWindow)
	fresh := m.panRetryHistory[:0]
	for _, t := range m.panRetryHistory {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	m.panRetryHistory = fresh

	if len(m.panRetryHistory) >= panRetryThreshold {
		return false, len(m.panRetryHistory)
	}
	m.panRetryHistory = append(m.panRetryHistory, now)
	return true, len(m.panRetryHistory) - 1
}

const (
	offlineRetryInterval    = 15 * time.Second
	offlineRetryMaxAttempts = 20
)

func (m *Manager) SetOfflineRetry(active bool) {
	m.offlineRetryMu.Lock()
	defer m.offlineRetryMu.Unlock()

	if active {
		if m.offlineRetryStop != nil {
			return // already running
		}
		stop := make(chan struct{})
		m.offlineRetryStop = stop
		go m.offlineRetryLoop(stop)
		m.log.Debug("bluetooth: offline retry loop started")
		return
	}

	if m.offlineRetryStop != nil {
		close(m.offlineRetryStop)
		m.offlineRetryStop = nil
		m.log.Debug("bluetooth: offline retry loop stopped")
	}
}

func (m *Manager) offlineRetryLoop(stop <-chan struct{}) {
	// first attempt immediately, don't wait a full 15s on cold boot
	m.runOfflineRetryAttempt(1)

	ticker := time.NewTicker(offlineRetryInterval)
	defer ticker.Stop()

	attempts := 1
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			attempts++
			if attempts > offlineRetryMaxAttempts {
				m.log.Infof("bluetooth: offline retry gave up after %d attempts", offlineRetryMaxAttempts)
				return
			}
			m.runOfflineRetryAttempt(attempts)
		}
	}
}

func (m *Manager) runOfflineRetryAttempt(attempt int) {
	m.panMu.Lock()
	addr := m.lastPanAddress
	m.panMu.Unlock()
	if addr == "" {
		m.log.Tracef("bluetooth: offline retry skipped, no prior PAN address")
		return
	}

	info, err := m.GetDeviceInfo(addr)
	if err != nil {
		m.log.WithError(err).Tracef("bluetooth: offline retry skipped for %s (no device info)", addr)
		return
	}
	if !info.Connected {
		m.log.Debugf("bluetooth: offline retry attempt %d, %s not BT-connected, actively reconnecting", attempt, addr)
		m.tryActiveReconnect(addr)
		return
	}

	// ACL up but offline = BNEP zombie state
	m.log.Infof("bluetooth: offline retry attempt %d, force-reconnecting PAN to %s", attempt, addr)
	if err := m.ConnectNetworkForced(addr); err != nil {
		m.log.WithError(err).Debugf("bluetooth: offline retry %d failed", attempt)
	}
}

// pages a bonded-but-disconnected peer via Device1.Connect. async because
// bluez scans can block 20-30s
func (m *Manager) tryActiveReconnect(addr string) {
	m.activeReconnectMu.Lock()
	if m.activeReconnectInFlight {
		m.activeReconnectMu.Unlock()
		m.log.Tracef("bluetooth: active reconnect to %s skipped, previous attempt still in flight", addr)
		return
	}
	m.activeReconnectInFlight = true
	m.activeReconnectMu.Unlock()

	go func() {
		defer func() {
			m.activeReconnectMu.Lock()
			m.activeReconnectInFlight = false
			m.activeReconnectMu.Unlock()
		}()

		// 30s for bluez to finish page scan
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		devicePath := formatDevicePath(m.adapter, addr)
		obj := m.conn.Object(bluezBusName, devicePath)

		m.log.Infof("bluetooth: actively paging %s for reconnect", addr)
		if err := obj.CallWithContext(ctx, bluezDeviceInterface+".Connect", 0).Err; err != nil {
			m.log.WithError(err).Debugf("bluetooth: active reconnect to %s failed (peer likely out of range or BT off)", addr)
			return
		}
		m.log.Infof("bluetooth: active reconnect to %s succeeded, PAN follows via signal handler", addr)
	}()
}

// invokes a D-Bus method with dbusCallTimeout
func (m *Manager) dbusCall(obj dbus.BusObject, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), dbusCallTimeout)
	defer cancel()

	m.log.Tracef("dbus call: %s on %s", method, obj.Path())
	c := obj.CallWithContext(ctx, method, 0, args...)
	// log failures at trace
	if c.Err != nil {
		m.log.WithError(c.Err).Tracef("dbus call FAILED: %s on %s", method, obj.Path())
	} else {
		m.log.Tracef("dbus call OK: %s on %s", method, obj.Path())
	}
	return c
}

func (m *Manager) setPower(enable bool) error {
	obj := m.conn.Object(bluezBusName, m.adapter)
	return m.dbusCall(obj, "org.freedesktop.DBus.Properties.Set",
		bluezAdapterInterface, "Powered", dbus.MakeVariant(enable)).Err
}

func (m *Manager) SetDiscoverable(enable bool) error {
	m.log.Debugf("bluetooth: SetDiscoverable(%v) entry, acquiring mutex", enable)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.log.Debugf("bluetooth: SetDiscoverable(%v) mutex acquired", enable)

	obj := m.conn.Object(bluezBusName, m.adapter)

	// disable auto-off when enabling, set bluetooth default (180s) when disabling
	var timeout uint32 = 180
	if enable {
		timeout = 0
	}
	if err := m.dbusCall(obj, "org.freedesktop.DBus.Properties.Set",
		bluezAdapterInterface, "DiscoverableTimeout", dbus.MakeVariant(timeout)).Err; err != nil {
		m.log.WithError(err).Debug("bluetooth: failed to set DiscoverableTimeout")
	}

	if err := m.dbusCall(obj, "org.freedesktop.DBus.Properties.Set",
		bluezAdapterInterface, "Discoverable", dbus.MakeVariant(enable)).Err; err != nil {
		return fmt.Errorf("set Discoverable=%v: %w", enable, err)
	}

	if err := m.dbusCall(obj, "org.freedesktop.DBus.Properties.Set",
		bluezAdapterInterface, "Pairable", dbus.MakeVariant(enable)).Err; err != nil {
		return fmt.Errorf("set Pairable=%v: %w", enable, err)
	}

	m.log.Infof("bluetooth: discoverable=%v pairable=%v timeout=%d", enable, enable, timeout)
	return nil
}

// SetTrusted lets the device reconnect + authorize services
func (m *Manager) SetTrusted(address string, trusted bool) error {
	devicePath := formatDevicePath(m.adapter, address)
	obj := m.conn.Object(bluezBusName, devicePath)
	return m.dbusCall(obj, "org.freedesktop.DBus.Properties.Set",
		bluezDeviceInterface, "Trusted", dbus.MakeVariant(trusted)).Err
}

func formatDevicePath(adapter dbus.ObjectPath, address string) dbus.ObjectPath {
	return dbus.ObjectPath(fmt.Sprintf("%s/dev_%s", adapter, strings.ReplaceAll(address, ":", "_")))
}

func fillDeviceInfo(info *DeviceInfo, props map[string]dbus.Variant) {
	if v, ok := props["Name"]; ok {
		info.Name, _ = v.Value().(string)
	}
	if v, ok := props["Alias"]; ok {
		info.Alias, _ = v.Value().(string)
	}
	if v, ok := props["Class"]; ok {
		if c, ok := v.Value().(uint32); ok {
			info.Class = fmt.Sprintf("%d", c)
		}
	}
	if v, ok := props["Icon"]; ok {
		info.Icon, _ = v.Value().(string)
	}
	if v, ok := props["Paired"]; ok {
		info.Paired, _ = v.Value().(bool)
	}
	if v, ok := props["Trusted"]; ok {
		info.Trusted, _ = v.Value().(bool)
	}
	if v, ok := props["Blocked"]; ok {
		info.Blocked, _ = v.Value().(bool)
	}
	if v, ok := props["Connected"]; ok {
		info.Connected, _ = v.Value().(bool)
	}
	if v, ok := props["LegacyPairing"]; ok {
		info.LegacyPairing, _ = v.Value().(bool)
	}
}

func (m *Manager) GetDeviceInfo(address string) (*DeviceInfo, error) {
	m.log.Debugf("bluetooth: GetDeviceInfo(%s) entry", address)

	devicePath := formatDevicePath(m.adapter, address)
	obj := m.conn.Object(bluezBusName, devicePath)

	props := make(map[string]dbus.Variant)
	if err := m.dbusCall(obj, "org.freedesktop.DBus.Properties.GetAll", bluezDeviceInterface).Store(&props); err != nil {
		return nil, err
	}

	info := &DeviceInfo{Address: address}
	fillDeviceInfo(info, props)

	batteryProps := make(map[string]dbus.Variant)
	if err := m.dbusCall(obj, "org.freedesktop.DBus.Properties.GetAll", bluezBatteryInterface).Store(&batteryProps); err == nil {
		if v, ok := batteryProps["Percentage"]; ok {
			if p, ok := v.Value().(uint8); ok {
				info.BatteryPercentage = int(p)
			}
		}
	}

	return info, nil
}

func (m *Manager) GetDevices() ([]DeviceInfo, error) {
	m.log.Debug("bluetooth: GetDevices entry")
	m.mu.Lock()
	defer m.mu.Unlock()

	objects := make(map[dbus.ObjectPath]map[string]map[string]dbus.Variant)
	obj := m.conn.Object(bluezBusName, "/")
	if err := m.dbusCall(obj, "org.freedesktop.DBus.ObjectManager.GetManagedObjects").Store(&objects); err != nil {
		return nil, fmt.Errorf("get managed objects: %w", err)
	}

	var devices []DeviceInfo
	for path, interfaces := range objects {
		deviceProps, ok := interfaces[bluezDeviceInterface]
		if !ok {
			continue
		}

		address := strings.TrimPrefix(string(path), string(m.adapter)+"/dev_")
		address = strings.ReplaceAll(address, "_", ":")

		info := DeviceInfo{Address: address}
		fillDeviceInfo(&info, deviceProps)

		if batteryProps, ok := interfaces[bluezBatteryInterface]; ok {
			if v, ok := batteryProps["Percentage"]; ok {
				if p, ok := v.Value().(uint8); ok {
					info.BatteryPercentage = int(p)
				}
			}
		}

		devices = append(devices, info)
	}

	return devices, nil
}

func (m *Manager) RemoveDevice(address string) error {
	m.log.Debugf("bluetooth: RemoveDevice(%s) entry", address)
	m.mu.Lock()
	defer m.mu.Unlock()

	devicePath := formatDevicePath(m.adapter, address)
	obj := m.conn.Object(bluezBusName, m.adapter)

	return m.dbusCall(obj, bluezAdapterInterface+".RemoveDevice", devicePath).Err
}

func (m *Manager) AcceptPairing() error { return m.agent.acceptPairing() }

func (m *Manager) DenyPairing() error { return m.agent.rejectPairing() }

func (m *Manager) GetCurrentPairingRequest() *PairingRequest {
	if m.agent == nil {
		return nil
	}
	return m.agent.getCurrent()
}

func (m *Manager) ConnectDevice(address string) error {
	cmd := exec.Command("nmcli", "device", "connect", address)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli connect: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	m.log.Infof("bluetooth: connected to %s via nmcli", address)

	go func() {
		deviceInfo, err := m.GetDeviceInfo(address)
		if m.emit == nil {
			return
		}
		if err != nil {
			m.log.WithError(err).Warn("bluetooth: failed to fetch device info after connect")
			m.emit(EventConnect, DeviceConnectedPayload{Address: address})
			return
		}
		m.emit(EventConnect, DeviceConnectedPayload{Address: address, Device: deviceInfo})
	}()

	return nil
}

func (m *Manager) DisconnectDevice(address string) error {
	m.log.Debugf("bluetooth: DisconnectDevice(%s) entry", address)
	m.mu.Lock()
	defer m.mu.Unlock()

	devicePath := formatDevicePath(m.adapter, address)
	obj := m.conn.Object(bluezBusName, devicePath)

	m.pendingDisconnects.Store(address, true)

	if err := m.dbusCall(obj, "org.bluez.Device1.Disconnect").Err; err != nil {
		m.pendingDisconnects.Delete(address)
		return fmt.Errorf("disconnect device: %w", err)
	}

	if m.emit != nil {
		m.emit(EventDisconnect, DeviceDisconnectedPayload{Address: address})
	}

	return nil
}

// ConnectNetwork brings up BT-PAN
func (m *Manager) ConnectNetwork(address string) error {
	return m.connectNetworkInternal(address, false)
}

// does a full teardown + reconnect
func (m *Manager) ConnectNetworkForced(address string) error {
	return m.connectNetworkInternal(address, true)
}

func (m *Manager) connectNetworkInternal(address string, force bool) error {
	m.log.Debugf("bluetooth: ConnectNetwork(%s, force=%v) entry", address, force)
	m.networkMu.Lock()
	defer m.networkMu.Unlock()

	devicePath := formatDevicePath(m.adapter, address)
	obj := m.conn.Object(bluezBusName, devicePath)

	if force {
		// tear down any existing NAP session, forces BlueZ to re-handshake
		if err := m.dbusCall(obj, bluezNetworkInterface+".Disconnect").Err; err != nil {
			m.log.WithError(err).Debugf("bluetooth: pre-reconnect Disconnect failed (ok if not connected)")
		}
		time.Sleep(500 * time.Millisecond)
	} else {
		// bnep0 already up + same address tracked
		m.panMu.Lock()
		lastAddr := m.lastPanAddress
		m.panMu.Unlock()
		if lastAddr == address {
			if link, err := netlink.LinkByName(panInterface); err == nil && link.Attrs().Flags&net.FlagUp != 0 {
				m.log.Debugf("bluetooth: ConnectNetwork(%s) skipped, PAN already up", address)
				return nil
			}
		}
	}

	if err := m.dbusCall(obj, bluezNetworkInterface+".Connect", "nap").Err; err != nil {
		return fmt.Errorf("network connect (nap): %w", err)
	}

	link, err := netlink.LinkByName(panInterface)
	if err != nil || link.Attrs().Flags&net.FlagUp == 0 {
		return fmt.Errorf("%s interface is not up", panInterface)
	}

	// NM doesnt auto manage bnep0 on the car thing
	m.requestDHCP()

	m.panMu.Lock()
	changed := m.lastPanAddress != address
	m.lastPanAddress = address
	cb := m.onLastPanAddressChanged
	m.panMu.Unlock()

	// persist only on actual change
	if changed && cb != nil {
		cb(address)
	}

	if m.emit != nil {
		m.emit(EventNetworkConnect, NetworkConnectedPayload{Address: address})
	}

	return nil
}

// gives the offline-retry loop a target before any fresh ConnectNetwork
func (m *Manager) SeedLastPanAddress(addr string) {
	if addr == "" {
		return
	}
	m.panMu.Lock()
	m.lastPanAddress = addr
	m.panMu.Unlock()
	m.log.Debugf("bluetooth: seeded lastPanAddress=%s from persisted state", addr)
}

// SetLastPanAddressChangedHandler registers a persistence callback, nil to clear
func (m *Manager) SetLastPanAddressChangedHandler(fn func(addr string)) {
	m.panMu.Lock()
	m.onLastPanAddressChanged = fn
	m.panMu.Unlock()
}

// requestDHCP runs dhclient in the background for an IPv4 lease on bnep0
func (m *Manager) requestDHCP() {
	dhclient, err := exec.LookPath("dhclient")
	if err != nil {
		m.log.Warn("bluetooth: dhclient not found, bnep0 will have no IP until configured manually")
		return
	}

	// -nw forks, -1 gives up after one DISCOVER cycle
	cmd := exec.Command(dhclient, "-nw", "-1",
		"-pf", "/run/dhclient.bnep0.pid",
		"-lf", "/run/dhclient.bnep0.leases",
		panInterface)
	if err := cmd.Start(); err != nil {
		m.log.WithError(err).Warn("bluetooth: dhclient failed to start")
		return
	}
	m.log.Infof("bluetooth: dhclient started for %s (pid %d)", panInterface, cmd.Process.Pid)

	// reap so it doesn't zombie
	go func() { _ = cmd.Wait() }()
}

func (m *Manager) NetworkUp() bool {
	link, err := netlink.LinkByName(panInterface)
	if err != nil {
		return false
	}
	return link.Attrs().Flags&net.FlagUp != 0
}
