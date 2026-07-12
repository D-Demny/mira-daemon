package bluetooth

// BLE volume peripheral
// lets car thing present itself as an input device so the connected phone accepts volume changes

import (
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	librespot "github.com/devgianlu/go-librespot"
)

const (
	gattServiceIface  = "org.bluez.GattService1"
	gattCharIface     = "org.bluez.GattCharacteristic1"
	gattDescIface     = "org.bluez.GattDescriptor1"
	gattManagerIface  = "org.bluez.GattManager1"
	leAdvIface        = "org.bluez.LEAdvertisement1"
	leAdvManagerIface = "org.bluez.LEAdvertisingManager1"
	dbusPropsIface    = "org.freedesktop.DBus.Properties"
	dbusObjMgrIface   = "org.freedesktop.DBus.ObjectManager"

	hidAppPath = dbus.ObjectPath("/com/mira/hid")
	hidAdvPath = dbus.ObjectPath("/com/mira/hid/advertisement0")

	uuidHIDService   = "00001812-0000-1000-8000-00805f9b34fb"
	uuidBattService  = "0000180f-0000-1000-8000-00805f9b34fb"
	uuidDISService   = "0000180a-0000-1000-8000-00805f9b34fb"
	uuidProtocolMode = "00002a4e-0000-1000-8000-00805f9b34fb"
	uuidReportMap    = "00002a4b-0000-1000-8000-00805f9b34fb"
	uuidReport       = "00002a4d-0000-1000-8000-00805f9b34fb"
	uuidHIDInfo      = "00002a4a-0000-1000-8000-00805f9b34fb"
	uuidControlPoint = "00002a4c-0000-1000-8000-00805f9b34fb"
	uuidBattLevel    = "00002a19-0000-1000-8000-00805f9b34fb"
	uuidPnPID        = "00002a50-0000-1000-8000-00805f9b34fb"
	uuidManufacturer = "00002a29-0000-1000-8000-00805f9b34fb"
	uuidModelNumber  = "00002a24-0000-1000-8000-00805f9b34fb"
	uuidReportRef    = "00002908-0000-1000-8000-00805f9b34fb"

	// input report bits
	hidBitVolumeDown byte = 0x01
	hidBitVolumeUp   byte = 0x02

	hidPressHold       = 30 * time.Millisecond
	hidStepGap         = 80 * time.Millisecond
	hidMaxStepsPerSend = 16
	hidRegisterRetry   = 30 * time.Second

	hidAdvAppearanceKeyboard uint16 = 0x03C1
)

var hidReportMapV16 = []byte{
	0x05, 0x0C, // Usage Page (Consumer)
	0x09, 0x01, // Usage (Consumer Control)
	0xA1, 0x01, // Collection (Application)
	0x85, 0x01, //   Report ID (1)
	0x15, 0x00, //   Logical Minimum (0)
	0x25, 0x01, //   Logical Maximum (1)
	0x75, 0x01, //   Report Size (1)
	0x95, 0x02, //   Report Count (2)
	0x09, 0xEA, //   Usage (Volume Down)
	0x09, 0xE9, //   Usage (Volume Up)
	0x81, 0x02, //   Input (Data, Variable, Absolute)
	0x95, 0x06, //   Report Count (6) padding
	0x81, 0x03, //   Input (Const, Variable, Absolute)
	0xC0, // End Collection
}

// hidInfoValue
var hidInfoValue = []byte{0x11, 0x01, 0x00, 0x03}

var pnpIDValue = []byte{0x02, 0x6B, 0x1D, 0x46, 0x02, 0x00, 0x01}

func gattErr(name, msg string) *dbus.Error {
	return dbus.NewError(name, []interface{}{msg})
}

type gattProps struct {
	iface  string
	getAll func() map[string]dbus.Variant
}

func (p *gattProps) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	all, err := p.GetAll(iface)
	if err != nil {
		return dbus.Variant{}, err
	}
	v, ok := all[prop]
	if !ok {
		return dbus.Variant{}, gattErr("org.freedesktop.DBus.Error.InvalidArgs", "unknown property "+prop)
	}
	return v, nil
}

func (p *gattProps) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != p.iface {
		return nil, gattErr("org.freedesktop.DBus.Error.InvalidArgs", "unknown interface "+iface)
	}
	return p.getAll(), nil
}

func (p *gattProps) Set(iface, prop string, value dbus.Variant) *dbus.Error {
	return gattErr("org.freedesktop.DBus.Error.PropertyReadOnly", prop+" is read-only")
}

type gattService struct {
	path dbus.ObjectPath
	uuid string
}

func (s *gattService) props() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"UUID":    dbus.MakeVariant(s.uuid),
		"Primary": dbus.MakeVariant(true),
	}
}

type gattDesc struct {
	path  dbus.ObjectPath
	uuid  string
	char  dbus.ObjectPath
	flags []string
	value []byte
}

func (d *gattDesc) props() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"UUID":           dbus.MakeVariant(d.uuid),
		"Characteristic": dbus.MakeVariant(d.char),
		"Flags":          dbus.MakeVariant(d.flags),
	}
}

func (d *gattDesc) ReadValue(options map[string]dbus.Variant) ([]byte, *dbus.Error) {
	return d.value, nil
}

func (d *gattDesc) WriteValue(value []byte, options map[string]dbus.Variant) *dbus.Error {
	return nil
}

type gattChar struct {
	path    dbus.ObjectPath
	uuid    string
	service dbus.ObjectPath
	flags   []string
	descs   []*gattDesc
	conn    *dbus.Conn
	log     librespot.Logger

	mu        sync.Mutex
	value     []byte
	notifying bool
}

func (c *gattChar) props() map[string]dbus.Variant {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]dbus.Variant{
		"UUID":      dbus.MakeVariant(c.uuid),
		"Service":   dbus.MakeVariant(c.service),
		"Flags":     dbus.MakeVariant(c.flags),
		"Notifying": dbus.MakeVariant(c.notifying),
	}
}

func (c *gattChar) ReadValue(options map[string]dbus.Variant) ([]byte, *dbus.Error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.value...), nil
}

func (c *gattChar) WriteValue(value []byte, options map[string]dbus.Variant) *dbus.Error {
	c.mu.Lock()
	c.value = append([]byte(nil), value...)
	c.mu.Unlock()
	return nil
}

func (c *gattChar) StartNotify() *dbus.Error {
	c.log.Infof("bluetooth: hid: host subscribed to %s", c.uuid)
	c.mu.Lock()
	c.notifying = true
	c.mu.Unlock()
	return nil
}

func (c *gattChar) StopNotify() *dbus.Error {
	c.log.Infof("bluetooth: hid: host unsubscribed from %s", c.uuid)
	c.mu.Lock()
	c.notifying = false
	c.mu.Unlock()
	return nil
}

func (c *gattChar) isNotifying() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notifying
}

// pushes a new value via PropertiesChanged
func (c *gattChar) notifyValue(b []byte) error {
	c.mu.Lock()
	c.value = append([]byte(nil), b...)
	c.mu.Unlock()
	return c.conn.Emit(c.path, dbusPropsIface+".PropertiesChanged", gattCharIface,
		map[string]dbus.Variant{"Value": dbus.MakeVariant(b)}, []string{})
}

type gattApp struct {
	services []*gattService
	chars    []*gattChar
}

func (a *gattApp) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	objs := make(map[dbus.ObjectPath]map[string]map[string]dbus.Variant)
	for _, s := range a.services {
		objs[s.path] = map[string]map[string]dbus.Variant{gattServiceIface: s.props()}
	}
	for _, c := range a.chars {
		objs[c.path] = map[string]map[string]dbus.Variant{gattCharIface: c.props()}
		for _, d := range c.descs {
			objs[d.path] = map[string]map[string]dbus.Variant{gattDescIface: d.props()}
		}
	}
	return objs, nil
}

// LE advertisement object
type hidAdvertisement struct {
	localName string
}

func (a *hidAdvertisement) props() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"Type":         dbus.MakeVariant("peripheral"),
		"ServiceUUIDs": dbus.MakeVariant([]string{"1812"}),
		"LocalName":    dbus.MakeVariant(a.localName),
		"Appearance":   dbus.MakeVariant(hidAdvAppearanceKeyboard),
		"Discoverable": dbus.MakeVariant(true),
	}
}

func (a *hidAdvertisement) Release() *dbus.Error { return nil }

type hidVolume struct {
	log     librespot.Logger
	conn    *dbus.Conn
	adapter dbus.ObjectPath
	input   *gattChar

	regMu         sync.Mutex
	appRegistered bool
	advRegistered bool

	peerConnected func() bool

	sendCh   chan int // signed steps
	stop     chan struct{}
	stopOnce sync.Once
}

// builds and exports the GATT tree + advertisement
func newHIDVolume(log librespot.Logger, conn *dbus.Conn, adapter dbus.ObjectPath, localName string) (*hidVolume, error) {
	hidSvc := &gattService{path: hidAppPath + "/service0", uuid: uuidHIDService}
	battSvc := &gattService{path: hidAppPath + "/service1", uuid: uuidBattService}
	disSvc := &gattService{path: hidAppPath + "/service2", uuid: uuidDISService}

	newChar := func(svc dbus.ObjectPath, idx int, uuid string, flags []string, value []byte) *gattChar {
		return &gattChar{
			path:    dbus.ObjectPath(fmt.Sprintf("%s/char%d", svc, idx)),
			uuid:    uuid,
			service: svc,
			flags:   flags,
			value:   value,
			conn:    conn,
			log:     log,
		}
	}

	protocolMode := newChar(hidSvc.path, 0, uuidProtocolMode, []string{"read", "write-without-response"}, []byte{0x01})
	reportMap := newChar(hidSvc.path, 1, uuidReportMap, []string{"encrypt-read"}, hidReportMapV16)
	input := newChar(hidSvc.path, 2, uuidReport, []string{"encrypt-read", "encrypt-notify"}, []byte{0x00})
	input.descs = []*gattDesc{{
		path: input.path + "/desc0", uuid: uuidReportRef,
		char: input.path, flags: []string{"read"},
		value: []byte{0x01, 0x01}, // Report ID 1
	}}
	hidInfo := newChar(hidSvc.path, 3, uuidHIDInfo, []string{"read"}, hidInfoValue)
	controlPoint := newChar(hidSvc.path, 4, uuidControlPoint, []string{"write-without-response"}, []byte{})
	battLevel := newChar(battSvc.path, 0, uuidBattLevel, []string{"read", "notify"}, []byte{100})
	pnp := newChar(disSvc.path, 0, uuidPnPID, []string{"read"}, pnpIDValue)
	manufacturer := newChar(disSvc.path, 1, uuidManufacturer, []string{"read"}, []byte(localName))
	model := newChar(disSvc.path, 2, uuidModelNumber, []string{"read"}, []byte("Car Thing"))

	app := &gattApp{
		services: []*gattService{hidSvc, battSvc, disSvc},
		chars:    []*gattChar{protocolMode, reportMap, input, hidInfo, controlPoint, battLevel, pnp, manufacturer, model},
	}

	if err := conn.Export(app, hidAppPath, dbusObjMgrIface); err != nil {
		return nil, fmt.Errorf("export hid app: %w", err)
	}
	for _, s := range app.services {
		s := s
		if err := conn.Export(s, s.path, gattServiceIface); err != nil {
			return nil, fmt.Errorf("export hid service %s: %w", s.uuid, err)
		}
		if err := conn.Export(&gattProps{gattServiceIface, s.props}, s.path, dbusPropsIface); err != nil {
			return nil, fmt.Errorf("export hid service props %s: %w", s.uuid, err)
		}
	}
	for _, c := range app.chars {
		c := c
		if err := conn.Export(c, c.path, gattCharIface); err != nil {
			return nil, fmt.Errorf("export hid char %s: %w", c.uuid, err)
		}
		if err := conn.Export(&gattProps{gattCharIface, c.props}, c.path, dbusPropsIface); err != nil {
			return nil, fmt.Errorf("export hid char props %s: %w", c.uuid, err)
		}
		for _, d := range c.descs {
			d := d
			if err := conn.Export(d, d.path, gattDescIface); err != nil {
				return nil, fmt.Errorf("export hid desc %s: %w", d.uuid, err)
			}
			if err := conn.Export(&gattProps{gattDescIface, d.props}, d.path, dbusPropsIface); err != nil {
				return nil, fmt.Errorf("export hid desc props %s: %w", d.uuid, err)
			}
		}
	}
	adv := &hidAdvertisement{localName: localName}
	if err := conn.Export(adv, hidAdvPath, leAdvIface); err != nil {
		return nil, fmt.Errorf("export hid advertisement: %w", err)
	}
	if err := conn.Export(&gattProps{leAdvIface, adv.props}, hidAdvPath, dbusPropsIface); err != nil {
		return nil, fmt.Errorf("export hid advertisement props: %w", err)
	}

	return &hidVolume{
		log:     log,
		conn:    conn,
		adapter: adapter,
		input:   input,
		sendCh:  make(chan int, 8),
		stop:    make(chan struct{}),
	}, nil
}

// registers with bluez and launches the send worker
func (h *hidVolume) start() {
	if err := h.register(); err != nil {
		h.log.WithError(err).Warn("bluetooth: hid: registration failed, will retry in background")
		go h.retryRegister()
	} else {
		h.log.Infof("bluetooth: hid: volume-key service registered and advertising")
	}
	go h.sendWorker()
}

func (h *hidVolume) register() error {
	h.regMu.Lock()
	defer h.regMu.Unlock()
	obj := h.conn.Object(bluezBusName, h.adapter)
	if !h.appRegistered {
		call := obj.Call(gattManagerIface+".RegisterApplication", 0, hidAppPath, map[string]dbus.Variant{})
		if call.Err != nil {
			return fmt.Errorf("register hid gatt app: %w", call.Err)
		}
		h.appRegistered = true
	}
	if !h.advRegistered {
		call := obj.Call(leAdvManagerIface+".RegisterAdvertisement", 0, hidAdvPath, map[string]dbus.Variant{})
		if call.Err != nil {
			return fmt.Errorf("register hid advertisement: %w", call.Err)
		}
		h.advRegistered = true
	}
	return nil
}

func (h *hidVolume) retryRegister() {
	t := time.NewTicker(hidRegisterRetry)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			if err := h.register(); err != nil {
				h.log.WithError(err).Debug("bluetooth: hid: registration retry failed")
				continue
			}
			h.log.Infof("bluetooth: hid: volume-key service registered and advertising (after retry)")
			return
		}
	}
}

// reports whether volume keys can be used atm
func (h *hidVolume) available() bool {
	h.regMu.Lock()
	registered := h.appRegistered
	h.regMu.Unlock()
	if !registered {
		return false
	}
	if h.input.isNotifying() {
		return true
	}
	return h.peerConnected != nil && h.peerConnected()
}

func (h *hidVolume) sendSteps(steps int) bool {
	if steps == 0 || !h.available() {
		return false
	}
	select {
	case h.sendCh <- steps:
		return true
	default:
		h.log.Debug("bluetooth: hid: volume queue full, dropping step burst")
		return false
	}
}

func (h *hidVolume) sendWorker() {
	for {
		select {
		case <-h.stop:
			return
		case steps := <-h.sendCh:
			bit := hidBitVolumeUp
			if steps < 0 {
				bit = hidBitVolumeDown
				steps = -steps
			}
			if steps > hidMaxStepsPerSend {
				steps = hidMaxStepsPerSend
			}
			for i := 0; i < steps; i++ {
				if err := h.input.notifyValue([]byte{bit}); err != nil {
					h.log.WithError(err).Debug("bluetooth: hid: volume press dropped")
					break
				}
				time.Sleep(hidPressHold)
				if err := h.input.notifyValue([]byte{0x00}); err != nil {
					h.log.WithError(err).Debug("bluetooth: hid: volume release dropped")
					break
				}
				if i < steps-1 {
					time.Sleep(hidStepGap)
				}
			}
		}
	}
}

func (h *hidVolume) close() {
	h.stopOnce.Do(func() { close(h.stop) })
	h.regMu.Lock()
	defer h.regMu.Unlock()
	obj := h.conn.Object(bluezBusName, h.adapter)
	if h.advRegistered {
		if call := obj.Call(leAdvManagerIface+".UnregisterAdvertisement", 0, hidAdvPath); call.Err != nil {
			h.log.WithError(call.Err).Debug("bluetooth: hid: advertisement unregister failed")
		}
		h.advRegistered = false
	}
	if h.appRegistered {
		if call := obj.Call(gattManagerIface+".UnregisterApplication", 0, hidAppPath); call.Err != nil {
			h.log.WithError(call.Err).Debug("bluetooth: hid: gatt app unregister failed")
		}
		h.appRegistered = false
	}
}
