package bluetooth

import (
	"errors"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	librespot "github.com/devgianlu/go-librespot"
)

func TestWatchdogClaim_FirstClaimSucceeds(t *testing.T) {
	t.Parallel()

	h := &hidVolume{}
	if !h.watchdogClaim("AA:BB:CC:DD:EE:FF", time.Now()) {
		t.Error("first claim should succeed")
	}
}

func TestWatchdogClaim_SecondClaimWithinCooldownIsRejected(t *testing.T) {
	t.Parallel()

	h := &hidVolume{}
	now := time.Now()
	h.watchdogClaim("AA:BB:CC:DD:EE:FF", now)
	if h.watchdogClaim("AA:BB:CC:DD:EE:FF", now.Add(hidWatchdogCooldown/2)) {
		t.Error("claim within cooldown should be rejected")
	}
}

func TestWatchdogClaim_ClaimAfterCooldownSucceeds(t *testing.T) {
	t.Parallel()

	h := &hidVolume{}
	now := time.Now()
	h.watchdogClaim("AA:BB:CC:DD:EE:FF", now)
	if !h.watchdogClaim("AA:BB:CC:DD:EE:FF", now.Add(hidWatchdogCooldown+time.Second)) {
		t.Error("claim after cooldown should succeed")
	}
}

func TestWatchdogClaim_AddressesAreIndependent(t *testing.T) {
	t.Parallel()

	h := &hidVolume{}
	now := time.Now()
	h.watchdogClaim("AA:BB:CC:DD:EE:FF", now)
	if !h.watchdogClaim("11:22:33:44:55:66", now) {
		t.Error("a claim for one address must not block another")
	}
}

func TestAdvWanted_GatedWithoutBond(t *testing.T) {
	t.Parallel()

	h := &hidVolume{}
	if h.advWantedLocked() {
		t.Error("advertisement must stay down with no bond")
	}
}

func TestAdvWanted_UpWithBond(t *testing.T) {
	t.Parallel()

	h := &hidVolume{bondGate: true}
	if !h.advWantedLocked() {
		t.Error("advertisement should be wanted once a bond exists")
	}
}

func TestAdvWanted_SetupSuppressWinsOverBond(t *testing.T) {
	t.Parallel()

	h := &hidVolume{bondGate: true, setupSuppress: true}
	if h.advWantedLocked() {
		t.Error("open pairing screen must suppress the advertisement even with a bond")
	}
}

func TestClearWatchdog_ResetsCooldown(t *testing.T) {
	t.Parallel()

	h := &hidVolume{}
	now := time.Now()
	h.watchdogClaim("AA:BB:CC:DD:EE:FF", now)
	h.clearWatchdog("AA:BB:CC:DD:EE:FF")
	if !h.watchdogClaim("AA:BB:CC:DD:EE:FF", now.Add(time.Second)) {
		t.Error("claim after clearWatchdog should succeed")
	}
}

func TestSuppressExpired_ClearsSuppressionOnce(t *testing.T) {
	t.Parallel()

	h := &hidVolume{log: &librespot.NullLogger{}, pokeCh: make(chan struct{}, 1), setupSuppress: true}
	h.suppressExpired()
	if h.setupSuppress {
		t.Error("suppressExpired should clear setupSuppress")
	}
	select {
	case <-h.pokeCh:
	default:
		t.Error("suppressExpired should poke the reconciler")
	}
	h.suppressExpired()
}

func TestSuppressExpired_NoOpWhenClosed(t *testing.T) {
	t.Parallel()

	h := &hidVolume{pokeCh: make(chan struct{}, 1), setupSuppress: true, closed: true}
	h.suppressExpired()
	if !h.setupSuppress {
		t.Error("suppressExpired must not mutate state after close")
	}
}

func TestIsBluezErr_MatchesNameExactly(t *testing.T) {
	t.Parallel()

	err := error(dbus.Error{Name: "org.bluez.Error.AlreadyExists"})
	if !isBluezErr(err, "AlreadyExists") {
		t.Error("should match org.bluez.Error.AlreadyExists")
	}
	if isBluezErr(err, "DoesNotExist") {
		t.Error("must not match a different bluez error")
	}
	if isBluezErr(errors.New("plain"), "AlreadyExists") {
		t.Error("must not match a non-dbus error")
	}
}

func TestAdvState_ReasonStrings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		h    *hidVolume
		want string
	}{
		{"no app", &hidVolume{}, "gatt service not registered"},
		{"setup open", &hidVolume{appRegistered: true, bondGate: true, setupSuppress: true}, "paused while pairing screen open"},
		{"no bond", &hidVolume{appRegistered: true}, "waiting for first phone pairing"},
		{"adv pending", &hidVolume{appRegistered: true, bondGate: true}, "advertisement pending (bluez retry)"},
		{"advertising", &hidVolume{appRegistered: true, bondGate: true, advRegistered: true}, "advertising"},
	}
	for _, c := range cases {
		if got := c.h.advState(); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func newTestChar() *gattChar {
	return &gattChar{log: &librespot.NullLogger{}}
}

func TestAvailable_ConnectedFallbackWhenNotDead(t *testing.T) {
	t.Parallel()

	h := &hidVolume{
		appRegistered: true,
		input:         newTestChar(),
		peerConnected: func() bool { return true },
	}
	if !h.available() {
		t.Error("connected peer without StartNotify should stay optimistic (silent CCC restore)")
	}
}

func TestAvailable_SubDeadBlocksSends(t *testing.T) {
	t.Parallel()

	h := &hidVolume{
		appRegistered: true,
		subDead:       true,
		input:         newTestChar(),
		peerConnected: func() bool { return true },
	}
	if h.available() {
		t.Error("a nudge-proven dead subscription must report the knob unusable")
	}
}

func TestAvailable_SubscribedWinsOverSubDead(t *testing.T) {
	t.Parallel()

	input := newTestChar()
	input.notifying = true
	h := &hidVolume{appRegistered: true, subDead: true, input: input}
	if !h.available() {
		t.Error("a live subscription always means the knob works")
	}
}

func TestStartNotify_ClearsSubDead(t *testing.T) {
	t.Parallel()

	h := &hidVolume{log: &librespot.NullLogger{}, subDead: true}
	input := newTestChar()
	input.onStartNotify = func() { h.setSubDead(false) }
	if err := input.StartNotify(); err != nil {
		t.Fatalf("StartNotify: %v", err)
	}
	h.regMu.Lock()
	dead := h.subDead
	h.regMu.Unlock()
	if dead {
		t.Error("an explicit resubscribe must clear the dead flag")
	}
}

func TestArmWatchdog_ClearsSubDeadAtArmTime(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	close(stop)
	h := &hidVolume{log: &librespot.NullLogger{}, subDead: true, stop: stop}
	h.armSubscribeWatchdog("AA:BB:CC:DD:EE:FF", func() bool { return true })
	h.regMu.Lock()
	dead := h.subDead
	h.regMu.Unlock()
	if dead {
		t.Error("arming (fresh connect) must reset to optimistic")
	}
}

func TestClearWatchdog_ClearsSubDead(t *testing.T) {
	t.Parallel()

	h := &hidVolume{log: &librespot.NullLogger{}, subDead: true, watchdogAt: map[string]time.Time{}}
	h.clearWatchdog("AA:BB:CC:DD:EE:FF")
	h.regMu.Lock()
	dead := h.subDead
	h.regMu.Unlock()
	if dead {
		t.Error("bond removal must reset the dead flag")
	}
}
