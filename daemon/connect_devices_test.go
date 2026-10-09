package daemon

import (
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"
)

// newConnectDevicesTestPlayer wires a no-op API server so the device-list
// merge can run in unit tests.
func newConnectDevicesTestPlayer(t *testing.T) *AppPlayer {
	t.Helper()
	stub, err := NewStubApiServer(&librespot.NullLogger{})
	if err != nil {
		t.Fatalf("stub api server: %v", err)
	}
	return &AppPlayer{
		app:   &App{log: &librespot.NullLogger{}, state: &librespot.AppState{}, server: stub, deviceId: "self"},
		state: &State{},
	}
}

func findConnectDevice(devs []ConnectDevice, id string) (ConnectDevice, bool) {
	for _, d := range devs {
		if d.Id == id {
			return d, true
		}
	}
	return ConnectDevice{}, false
}

// a device that ages out of the live cluster must stay selectable as an
// offline entry (sightings cache), not vanish from the picker. issue #127.
func TestConnectDevices_SightedDeviceSurvivesClusterEviction(t *testing.T) {
	p := newConnectDevicesTestPlayer(t)

	first := &connectpb.Cluster{
		ActiveDeviceId: "a",
		Device: map[string]*connectpb.DeviceInfo{
			"self": {Name: "Mira"},
			"a":    {Name: "Wohnzimmer", DeviceType: devicespb.DeviceType_AVR},
			"b":    {Name: "Laptop", DeviceType: devicespb.DeviceType_COMPUTER},
		},
	}
	p.updateConnectDevices(first)

	// the laptop leaves the cluster entirely (DEVICES_DISAPPEARED)
	second := &connectpb.Cluster{
		ActiveDeviceId: "a",
		Device:         map[string]*connectpb.DeviceInfo{"self": {Name: "Mira"}, "a": {Name: "Wohnzimmer", DeviceType: devicespb.DeviceType_AVR}},
	}
	p.updateConnectDevices(second)

	laptop, ok := findConnectDevice(p.state.connectDevices, "b")
	if !ok {
		t.Fatal("evicted device must stay in the published list (sightings cache)")
	}
	if laptop.IsActive || !laptop.IsOffline || !laptop.CanTransfer {
		t.Errorf("cached entry flags: %+v", laptop)
	}
	if laptop.Name != "Laptop" || laptop.Type != "COMPUTER" {
		t.Errorf("cached entry name/type: %q / %q", laptop.Name, laptop.Type)
	}

	if _, ok := findConnectDevice(p.state.connectDevices, "self"); ok {
		t.Error("own device id must not be listed")
	}
}

// sightings older than connectDeviceRememberTTL expire on the next cluster update.
func TestConnectDevices_SightingsExpireAfterTTL(t *testing.T) {
	p := newConnectDevicesTestPlayer(t)
	now := time.Now()
	p.state.rememberedConnectDevices = map[string]rememberedConnectDevice{
		"fresh": {name: "Fresh", typ: "AVR", lastSeen: now.Add(-time.Hour)},
		"stale": {name: "Stale", typ: "AVR", lastSeen: now.Add(-connectDeviceRememberTTL - time.Hour)},
	}
	p.updateConnectDevices(&connectpb.Cluster{})

	if _, ok := findConnectDevice(p.state.connectDevices, "fresh"); !ok {
		t.Error("fresh sighting must remain listed")
	}
	if _, ok := findConnectDevice(p.state.connectDevices, "stale"); ok {
		t.Error("sighting past TTL must be dropped")
	}
}

// for the same id a Web API entry wins over the cached sighting; a cache-only
// id still appears. issue #127.
func TestConnectDevices_WebApiEntryWinsOverSighting(t *testing.T) {
	p := newConnectDevicesTestPlayer(t)
	now := time.Now()
	p.state.rememberedConnectDevices = map[string]rememberedConnectDevice{
		"dup":   {name: "Cached Name", typ: "AVR", lastSeen: now},
		"cache": {name: "Cache Only", typ: "COMPUTER", lastSeen: now},
	}
	p.state.knownConnectDevices = map[string]knownConnectDevice{
		"dup": {name: "Web Name", typ: "AVR"},
	}
	p.updateConnectDevices(&connectpb.Cluster{})

	dup, ok := findConnectDevice(p.state.connectDevices, "dup")
	if !ok {
		t.Fatal("duplicated id must be listed")
	}
	if dup.Name != "Web Name" {
		t.Errorf("web api entry must win over the cache: got %q", dup.Name)
	}
	if _, ok := findConnectDevice(p.state.connectDevices, "cache"); !ok {
		t.Error("cache-only id must still be listed")
	}
	if n := len(p.state.connectDevices); n != 2 {
		t.Errorf("expected 2 entries without duplicates, got %d", n)
	}
}
