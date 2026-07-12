package bluetooth

import (
	"testing"
	"time"
)

func TestRouteHealth_DemotesAfterStreakWithHealthyAlt(t *testing.T) {
	t.Parallel()
	var s routeHealthState
	now := time.Now()

	for i := 0; i < demoteAfter-1; i++ {
		if got := s.tick(false, true, now); got != actionNone {
			t.Fatalf("probe %d: expected none, got %v", i, got)
		}
	}
	if got := s.tick(false, true, now); got != actionDemote {
		t.Fatalf("expected demote on strike %d, got %v", demoteAfter, got)
	}
	if got := s.tick(false, true, now); got != actionNone {
		t.Fatalf("expected none after demote, got %v", got)
	}
}

func TestRouteHealth_NoDemoteWithoutAlternative(t *testing.T) {
	t.Parallel()
	var s routeHealthState
	now := time.Now()

	for i := 0; i < demoteAfter*3; i++ {
		if got := s.tick(false, false, now); got != actionNone {
			t.Fatalf("probe %d: must never demote without an alternative, got %v", i, got)
		}
	}
}

func TestRouteHealth_SuccessBreaksStreak(t *testing.T) {
	t.Parallel()
	var s routeHealthState
	now := time.Now()

	s.tick(false, true, now)
	s.tick(false, true, now)
	s.tick(true, true, now)
	s.tick(false, true, now)
	s.tick(false, true, now)
	if got := s.tick(false, true, now); got != actionDemote {
		t.Fatalf("streak must need %d consecutive failures, got %v", demoteAfter, got)
	}
}

func TestRouteHealth_RestoreNeedsStreakAndHoldDown(t *testing.T) {
	t.Parallel()
	var s routeHealthState
	start := time.Now()

	for i := 0; i < demoteAfter; i++ {
		s.tick(false, true, start)
	}
	if !s.demoted {
		t.Fatal("setup: expected demoted")
	}

	for i := 0; i < restoreAfter*2; i++ {
		if got := s.tick(true, true, start.Add(time.Minute)); got != actionNone {
			t.Fatalf("restore inside hold-down forbidden, got %v", got)
		}
	}

	if got := s.tick(true, true, start.Add(restoreHoldDown+time.Second)); got != actionRestore {
		t.Fatal("expected restore after hold-down + clean streak")
	}
	if s.demoted {
		t.Fatal("restore must clear demoted")
	}
}

func TestRouteHealth_ResetForgetsDemotion(t *testing.T) {
	t.Parallel()
	var s routeHealthState
	now := time.Now()
	for i := 0; i < demoteAfter; i++ {
		s.tick(false, true, now)
	}
	s.reset()
	if s.demoted || s.failStreak != 0 {
		t.Fatal("reset must clear all state")
	}
}
