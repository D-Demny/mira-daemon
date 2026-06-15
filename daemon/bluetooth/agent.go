package bluetooth

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"

	librespot "github.com/devgianlu/go-librespot"
)

type agent struct {
	log     librespot.Logger
	conn    *dbus.Conn
	manager *Manager
	path    dbus.ObjectPath

	// mu guards current
	mu      sync.Mutex
	current *PairingRequest
}

func (a *agent) setCurrent(pr *PairingRequest) {
	a.mu.Lock()
	a.current = pr
	a.mu.Unlock()
}

// getCurrent returns a copy of the in-flight request (or nil) so callers never read a struct another goroutine may be mutating
func (a *agent) getCurrent() *PairingRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil {
		return nil
	}
	cp := *a.current
	return &cp
}

// clearCurrent drops the in-flight request unconditionally
func (a *agent) clearCurrent() {
	a.mu.Lock()
	a.current = nil
	a.mu.Unlock()
}

// clearCurrentIfDevice drops the in-flight request only when it belongs to devicePath
func (a *agent) clearCurrentIfDevice(devicePath string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current != nil && a.current.Device == devicePath {
		a.current = nil
		return true
	}
	return false
}

func newAgent(log librespot.Logger, conn *dbus.Conn, manager *Manager) (*agent, error) {
	a := &agent{
		log:     log,
		conn:    conn,
		manager: manager,
		path:    dbus.ObjectPath(bluezAgentPath),
	}

	if err := conn.Export(a, a.path, bluezAgentInterface); err != nil {
		return nil, err
	}

	node := &introspect.Node{
		Name: bluezAgentPath,
		Interfaces: []introspect.Interface{
			{
				Name:    bluezAgentInterface,
				Methods: introspect.Methods(a),
			},
		},
	}

	if err := conn.Export(introspect.NewIntrospectable(node), a.path,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbusCallTimeout)
	defer cancel()

	obj := conn.Object(bluezBusName, dbus.ObjectPath(bluezObjectPath))
	log.Debugf("bluetooth: registering agent at %s", a.path)

	// we have to show the passkey or connection will fail
	if err := obj.CallWithContext(ctx, bluezAgentManager+".RegisterAgent", 0,
		a.path, "DisplayYesNo").Err; err != nil {
		return nil, fmt.Errorf("register agent: %w", err)
	}

	// claim default so prompts route to us
	if err := obj.CallWithContext(ctx, bluezAgentManager+".RequestDefaultAgent", 0,
		a.path).Err; err != nil {
		log.WithError(err).Warn("bluetooth: failed to request default agent, pairing prompts may go to another agent")
	}

	log.Info("bluetooth: agent registered (DisplayYesNo, default)")

	return a, nil
}

func (a *agent) addressFromPath(devicePath string) string {
	addr := strings.TrimPrefix(devicePath, string(a.manager.adapter)+"/dev_")
	return strings.ReplaceAll(addr, "_", ":")
}

// BlueZ Agent1 methods

func (a *agent) Release() *dbus.Error {
	a.log.Info("bluetooth agent released")
	return nil
}

func (a *agent) RequestConfirmation(device dbus.ObjectPath, passkey uint32) *dbus.Error {
	a.log.Infof("bluetooth: pairing confirmation requested from %s (passkey %06d)", device, passkey)

	address := a.addressFromPath(string(device))
	passkeyStr := fmt.Sprintf("%06d", passkey)
	a.setCurrent(&PairingRequest{
		Device:      string(device),
		Passkey:     passkeyStr,
		RequestType: "confirmation",
	})

	if a.manager.emit != nil {
		a.manager.emit(EventPairing, PairingStartedPayload{
			Address:    address,
			PairingKey: passkeyStr,
		})
	}

	// trust the device immediately
	go func() {
		if err := a.manager.SetTrusted(address, true); err != nil {
			a.log.WithError(err).Warn("bluetooth: failed to mark device trusted after pair")
		} else {
			a.log.Debugf("bluetooth: marked %s trusted", address)
		}
	}()

	return nil
}

func (a *agent) RequestAuthorization(device dbus.ObjectPath) *dbus.Error {
	a.log.Infof("bluetooth: authorization requested from %s", device)
	return nil
}

// must exist or BlueZ rejects the first post-pair service connection
func (a *agent) AuthorizeService(device dbus.ObjectPath, uuid string) *dbus.Error {
	a.log.Debugf("bluetooth: AuthorizeService %s from %s", uuid, device)
	return nil
}

func (a *agent) Cancel() *dbus.Error {
	a.log.Info("bluetooth: pairing cancelled by remote")

	if pr := a.getCurrent(); pr != nil && a.manager.emit != nil {
		a.manager.emit(EventPairingCancelled, DeviceDisconnectedPayload{
			Address: a.addressFromPath(pr.Device),
		})
	}

	a.clearCurrent()
	return nil
}

// internal methods called via manager.go (not exported on D-Bus)
func (a *agent) acceptPairing() error {
	pr := a.getCurrent()
	if pr == nil {
		return fmt.Errorf("no pairing request in progress")
	}

	address := a.addressFromPath(pr.Device)

	// GetDeviceInfo takes Manager.mu
	deviceInfo, err := a.manager.GetDeviceInfo(address)
	if err != nil {
		a.log.WithError(err).Warn("bluetooth: failed to fetch device info after pairing")
		deviceInfo = &DeviceInfo{
			Address: address,
			Paired:  true,
		}
	}

	if a.manager.emit != nil {
		a.manager.emit(EventPaired, DevicePairedPayload{Device: deviceInfo})
	}

	// only clear our own request
	a.clearCurrentIfDevice(pr.Device)
	return nil
}

func (a *agent) rejectPairing() error {
	if a.getCurrent() == nil {
		return fmt.Errorf("no pairing request in progress")
	}
	a.clearCurrent()
	return nil
}
