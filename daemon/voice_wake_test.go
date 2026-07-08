package daemon

import (
	"context"
	"testing"
)

func TestWakeThresholdPlaybackAware(t *testing.T) {
	v := &voiceService{cfg: VoiceConfig{WakeThreshold: 0.4, WakeThresholdPlaying: 0.6}}
	if got := v.wakeThreshold(); got != 0.4 {
		t.Fatalf("idle threshold = %v, want 0.4", got)
	}
	v.playbackActive.Store(true)
	if got := v.wakeThreshold(); got != 0.6 {
		t.Fatalf("playing threshold = %v, want 0.6", got)
	}
}

func TestWakeThresholdPlayingUnsetFallsBack(t *testing.T) {
	v := &voiceService{cfg: VoiceConfig{WakeThreshold: 0.5}}
	v.playbackActive.Store(true)
	if got := v.wakeThreshold(); got != 0.5 {
		t.Fatalf("fallback threshold = %v, want 0.5", got)
	}
}

func TestNotifyPlaybackGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := &voiceService{cfg: VoiceConfig{Wake: true}, ctx: ctx, cancel: cancel}

	g0 := v.thrGen.Load()
	v.notifyPlayback(true)
	if v.thrGen.Load() != g0+1 {
		t.Fatal("state flip should bump the debounce generation")
	}
	v.notifyPlayback(true)
	if v.thrGen.Load() != g0+1 {
		t.Fatal("same-state notify must not bump the generation")
	}
	v.notifyPlayback(false)
	if v.thrGen.Load() != g0+2 {
		t.Fatal("flip back should bump the generation again")
	}
}
