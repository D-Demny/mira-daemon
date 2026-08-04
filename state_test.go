package go_librespot

import (
	"encoding/json"
	"strings"
	"testing"
)

// AppState is persisted across daemon reboots

func TestAppState_JSONRoundtripAllFields(t *testing.T) {
	t.Parallel()

	vol := uint32(42)
	original := AppState{
		DeviceId:                "device-abc",
		EventManager:            json.RawMessage(`{"foo":"bar"}`),
		LastVolume:              &vol,
		LastBluetoothPanAddress: "AA:BB:CC:DD:EE:FF",
	}
	original.Credentials.Username = "user@example.com"
	original.Credentials.Data = []byte{0x01, 0x02, 0x03}

	data, err := json.Marshal(&original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var restored AppState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got, want := restored.DeviceId, original.DeviceId; got != want {
		t.Errorf("DeviceId: got %q want %q", got, want)
	}
	if got, want := string(restored.EventManager), string(original.EventManager); got != want {
		t.Errorf("EventManager: got %s want %s", got, want)
	}
	if got, want := restored.Credentials.Username, original.Credentials.Username; got != want {
		t.Errorf("Credentials.Username: got %q want %q", got, want)
	}
	if got, want := string(restored.Credentials.Data), string(original.Credentials.Data); got != want {
		t.Errorf("Credentials.Data: got %x want %x", got, want)
	}
	if restored.LastVolume == nil || *restored.LastVolume != *original.LastVolume {
		t.Errorf("LastVolume: got %v want %v", restored.LastVolume, original.LastVolume)
	}
	if got, want := restored.LastBluetoothPanAddress, original.LastBluetoothPanAddress; got != want {
		t.Errorf("LastBluetoothPanAddress: got %q want %q", got, want)
	}
}

func TestAppState_UnmarshalsOlderShapeWithoutPanAddress(t *testing.T) {
	t.Parallel()

	olderJSON := `{
		"device_id": "device-pre-upgrade",
		"event_manager": null,
		"credentials": {
			"username": "user@example.com",
			"data": "AQID"
		},
		"last_volume": 42
	}`

	var restored AppState
	if err := json.Unmarshal([]byte(olderJSON), &restored); err != nil {
		t.Fatalf("Unmarshal of older shape failed: %v", err)
	}

	if got := restored.LastBluetoothPanAddress; got != "" {
		t.Errorf("LastBluetoothPanAddress: got %q, want empty string for older state", got)
	}
	if got := restored.DeviceId; got != "device-pre-upgrade" {
		t.Errorf("DeviceId not restored from older state: got %q", got)
	}
}

func TestAppState_LastVolumeDistinguishesNilFromZero(t *testing.T) {
	t.Parallel()

	// Case 1: nil LastVolume marshals to "null", round-trips back to nil.
	stateNil := AppState{DeviceId: "x"}
	data, err := json.Marshal(&stateNil)
	if err != nil {
		t.Fatalf("Marshal nil-volume: %v", err)
	}
	if !strings.Contains(string(data), `"last_volume":null`) {
		t.Errorf("nil LastVolume should marshal to null; got %s", data)
	}

	var restoredNil AppState
	if err := json.Unmarshal(data, &restoredNil); err != nil {
		t.Fatalf("Unmarshal nil-volume: %v", err)
	}
	if restoredNil.LastVolume != nil {
		t.Errorf("nil LastVolume not preserved across roundtrip: got %v", restoredNil.LastVolume)
	}

	// Case 2: pointer-to-zero round-trips as pointer-to-zero (NOT nil).
	zero := uint32(0)
	stateZero := AppState{DeviceId: "x", LastVolume: &zero}
	data, err = json.Marshal(&stateZero)
	if err != nil {
		t.Fatalf("Marshal zero-volume: %v", err)
	}
	if !strings.Contains(string(data), `"last_volume":0`) {
		t.Errorf("&0 LastVolume should marshal to 0; got %s", data)
	}

	var restoredZero AppState
	if err := json.Unmarshal(data, &restoredZero); err != nil {
		t.Fatalf("Unmarshal zero-volume: %v", err)
	}
	if restoredZero.LastVolume == nil {
		t.Fatalf("&0 LastVolume should NOT round-trip to nil")
	}
	if *restoredZero.LastVolume != 0 {
		t.Errorf("LastVolume value: got %d want 0", *restoredZero.LastVolume)
	}
}
