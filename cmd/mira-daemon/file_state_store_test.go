package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
)

func TestFileStateStoreSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStateStore(filepath.Join(dir, "state.json"), "", &librespot.NullLogger{})

	want := &librespot.AppState{DeviceId: "abc123"}
	want.Credentials.Username = "user@example.com"
	want.Credentials.Data = []byte{1, 2, 3, 4}
	want.LastBluetoothPanAddress = "AA:BB:CC:DD:EE:FF"

	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DeviceId != want.DeviceId ||
		got.Credentials.Username != want.Credentials.Username ||
		string(got.Credentials.Data) != string(want.Credentials.Data) ||
		got.LastBluetoothPanAddress != want.LastBluetoothPanAddress {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}
}

// issue #56: setAccountID persists via persistState -> Save, so the resolved
// liked-songs account id must actually land in state.json (as "account_id")
// and survive a restart — otherwise every fresh daemon run would re-send the
// dead pseudo context until /v1/me succeeds again.
func TestFileStateStoreSavePersistsAccountID(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	s := NewFileStateStore(statePath, "", &librespot.NullLogger{})

	want := &librespot.AppState{DeviceId: "abc123", AccountID: "sp-user-42"}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading state.json: %v", err)
	}
	if !strings.Contains(string(raw), `"account_id":"sp-user-42"`) {
		t.Fatalf("state.json does not carry the persisted account id:\n%s", raw)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccountID != "sp-user-42" {
		t.Errorf("reloaded AccountID: got %q want sp-user-42", got.AccountID)
	}
}

// Save renames its temp file into place and must not leak temp files or open descriptors
// repeated saves the directory should contain only the final state.json
func TestFileStateStoreSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStateStore(filepath.Join(dir, "state.json"), "", &librespot.NullLogger{})

	for i := 0; i < 5; i++ {
		if err := s.Save(&librespot.AppState{DeviceId: "x"}); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only state.json after saves, found: %v", names)
	}
}
