package bluetooth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stub sidecar
const stubSidecar = `#!/bin/sh
echo '{"event":"ready"}'
while read line; do
  case "$line" in
    *disconnect*) echo '{"event":"state","state":"disconnected","addr":"AA:BB:CC:DD:EE:FF"}' ;;
    *connect*)    echo '{"event":"state","state":"connected","addr":"AA:BB:CC:DD:EE:FF"}' ;;
    *ping*)       echo '{"event":"pong"}' ;;
  esac
done
`

func newStubIap2(t *testing.T) *iap2Volume {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "iap2-sidecar")
	if err := os.WriteFile(path, []byte(stubSidecar), 0o755); err != nil {
		t.Fatal(err)
	}
	v := &iap2Volume{log: newTestManager().log, path: path}
	t.Cleanup(v.Close)
	return v
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func TestIap2Volume_SessionLifecycle(t *testing.T) {
	v := newStubIap2(t)

	if v.Connected() {
		t.Fatal("must not report connected before any session")
	}
	if v.SendVolumeSteps(3) {
		t.Fatal("volume must fail with no session")
	}

	v.EnsureSession("AA:BB:CC:DD:EE:FF")
	if !waitFor(t, 3*time.Second, v.Connected) {
		t.Fatal("session never reported connected")
	}
	if !v.SendVolumeSteps(3) {
		t.Fatal("volume must succeed on a connected session")
	}

	v.DropSession("AA:BB:CC:DD:EE:FF")
	if !waitFor(t, 3*time.Second, func() bool { return !v.Connected() }) {
		t.Fatal("session never reported disconnected")
	}
	if v.SendVolumeSteps(1) {
		t.Fatal("volume must fail after disconnect")
	}
}

func TestIap2Volume_DropSessionIgnoresOtherAddr(t *testing.T) {
	v := newStubIap2(t)

	v.EnsureSession("AA:BB:CC:DD:EE:FF")
	if !waitFor(t, 3*time.Second, v.Connected) {
		t.Fatal("session never reported connected")
	}
	v.DropSession("11:22:33:44:55:66") // different peer
	time.Sleep(100 * time.Millisecond)
	if !v.Connected() {
		t.Fatal("drop for a different address must not tear the session down")
	}
}

func TestIap2Volume_NilReceiverSafe(t *testing.T) {
	t.Parallel()
	var v *iap2Volume
	v.EnsureSession("AA:BB:CC:DD:EE:FF")
	v.DropSession("")
	v.Close()
	if v.Connected() || v.SendVolumeSteps(1) {
		t.Fatal("nil iap2Volume must report not-connected and refuse sends")
	}
}
