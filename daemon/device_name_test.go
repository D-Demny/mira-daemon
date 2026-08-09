package daemon

import (
	"testing"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"
)

func TestDeviceDisplayName(t *testing.T) {
	t.Parallel()
	alias := func(id uint32, name string) map[uint32]*devicespb.DeviceAlias {
		return map[uint32]*devicespb.DeviceAlias{id: {DisplayName: name}}
	}

	tests := []struct {
		name string
		dev  *connectpb.DeviceInfo
		want string
	}{
		{"selected alias wins", &connectpb.DeviceInfo{
			Name: "0123456789abcdef0123", SelectedAliasId: 2,
			DeviceAliases: map[uint32]*devicespb.DeviceAlias{1: {DisplayName: "Kitchen"}, 2: {DisplayName: "Living Room"}},
		}, "Living Room"},
		{"lowest alias when none selected", &connectpb.DeviceInfo{
			Name: "0123456789abcdef0123", DeviceAliases: alias(1, "Kitchen"),
		}, "Kitchen"},
		{"plain name kept", &connectpb.DeviceInfo{Name: "Kaz's iPhone"}, "Kaz's iPhone"},
		{"brand+model when name is an id", &connectpb.DeviceInfo{
			Name: "0123456789abcdef0123", Brand: "Sonos", Model: "One",
		}, "Sonos One"},
		{"raw id survives as last resort", &connectpb.DeviceInfo{Name: "0123456789abcdef0123"}, "0123456789abcdef0123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &AppPlayer{state: &State{}}
			if got := p.deviceDisplayName("dev1", tt.dev); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

// a degraded cluster update must not replace a good name with a raw id
func TestDeviceDisplayNameCachesLastGood(t *testing.T) {
	t.Parallel()
	p := &AppPlayer{state: &State{}}
	if got := p.deviceDisplayName("dev1", &connectpb.DeviceInfo{Name: "Office Echo"}); got != "Office Echo" {
		t.Fatalf("got %q", got)
	}
	if got := p.deviceDisplayName("dev1", &connectpb.DeviceInfo{Name: "0123456789abcdef0123"}); got != "Office Echo" {
		t.Fatalf("degraded update should keep the cached name, got %q", got)
	}
}
