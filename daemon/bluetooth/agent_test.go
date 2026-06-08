package bluetooth

import (
	"sync"
	"testing"
)

// getCurrent must hand back a copy, not the shared pointer
func TestAgentGetCurrentReturnsCopy(t *testing.T) {
	a := &agent{}
	a.setCurrent(&PairingRequest{Device: "/dev_AA", Passkey: "123456", RequestType: "confirmation"})

	got := a.getCurrent()
	if got == nil {
		t.Fatal("getCurrent returned nil after setCurrent")
	}
	got.Passkey = "999999" // mutate the copy

	if again := a.getCurrent(); again.Passkey != "123456" {
		t.Fatalf("getCurrent did not return a copy: internal passkey mutated to %q", again.Passkey)
	}
}

func TestAgentClearCurrentIfDevice(t *testing.T) {
	a := &agent{}
	a.setCurrent(&PairingRequest{Device: "/dev_AA"})

	if a.clearCurrentIfDevice("/dev_BB") {
		t.Fatal("clearCurrentIfDevice cleared a non-matching device")
	}
	if a.getCurrent() == nil {
		t.Fatal("non-matching clear wiped the request")
	}
	if !a.clearCurrentIfDevice("/dev_AA") {
		t.Fatal("clearCurrentIfDevice did not clear the matching device")
	}
	if a.getCurrent() != nil {
		t.Fatal("matching clear left a stale request")
	}
}

// Run with -race
func TestAgentCurrentConcurrentAccess(t *testing.T) {
	a := &agent{}
	const goroutines, iters = 16, 2000

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			dev := "/dev_" + string(rune('A'+id))
			for i := 0; i < iters; i++ {
				switch i % 3 {
				case 0:
					a.setCurrent(&PairingRequest{Device: dev, Passkey: "000000", RequestType: "confirmation"})
				case 1:
					if pr := a.getCurrent(); pr != nil {
						_ = pr.Device + pr.Passkey + pr.RequestType
					}
				case 2:
					a.clearCurrentIfDevice(dev)
				}
			}
		}(g)
	}
	wg.Wait()
}
