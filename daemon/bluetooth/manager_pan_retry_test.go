package bluetooth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shouldRetryPan up to panRetryThreshold (3) within panRetryWindow (30s), 4th rejected
func TestShouldRetryPan_EmptyHistoryAllowsAndAppends(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	now := time.Now()

	allowed, recent := m.shouldRetryPan(now)
	if !allowed {
		t.Fatalf("expected allowed=true on empty history, got false")
	}
	if recent != 0 {
		t.Errorf("recentDrops: got %d want 0 (no entries before this call)", recent)
	}
	if got := len(m.panRetryHistory); got != 1 {
		t.Errorf("history len after success: got %d want 1", got)
	}
}

func TestShouldRetryPan_ThirdCallStillAllowed(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	base := time.Now()
	m.panRetryHistory = []time.Time{base, base.Add(time.Second)}

	allowed, recent := m.shouldRetryPan(base.Add(2 * time.Second))
	if !allowed {
		t.Fatalf("3rd call within window: expected allowed=true")
	}
	if recent != 2 {
		t.Errorf("recentDrops: got %d want 2", recent)
	}
	if got := len(m.panRetryHistory); got != 3 {
		t.Errorf("history len: got %d want 3 after appending the 3rd entry", got)
	}
}

func TestShouldRetryPan_FourthCallRejectedAndDoesNotAppend(t *testing.T) {
	t.Parallel()

	m := newTestManager()
	base := time.Now()
	m.panRetryHistory = []time.Time{
		base,
		base.Add(time.Second),
		base.Add(2 * time.Second),
	}

	allowed, recent := m.shouldRetryPan(base.Add(3 * time.Second))
	if allowed {
		t.Fatalf("4th call within window: expected allowed=false (circuit open)")
	}
	if recent != 3 {
		t.Errorf("recentDrops: got %d want 3 (threshold count)", recent)
	}
	if got := len(m.panRetryHistory); got != 3 {
		t.Errorf("rejected call must not append; got history len %d want 3", got)
	}
}

func TestShouldRetryPan_OldEntriesPrunedAndCallPasses(t *testing.T) {
	t.Parallel()

	// regression: stale flap history must not block legit retries after the situation has cleared
	m := newTestManager()
	now := time.Now()
	ancient := now.Add(-2 * time.Hour)
	m.panRetryHistory = []time.Time{
		ancient,
		ancient.Add(time.Second),
		ancient.Add(2 * time.Second),
	}

	allowed, recent := m.shouldRetryPan(now)
	if !allowed {
		t.Fatalf("expected allowed=true after pruning all old entries")
	}
	if recent != 0 {
		t.Errorf("recentDrops after prune: got %d want 0 (all 3 should be pruned)", recent)
	}
	if got := len(m.panRetryHistory); got != 1 {
		t.Errorf("history after prune+append: got len %d want 1 (only the new entry)", got)
	}
}

func TestShouldRetryPan_MixedOldAndFreshCountsOnlyFresh(t *testing.T) {
	t.Parallel()

	// 2 old + 2 fresh, only fresh count
	m := newTestManager()
	now := time.Now()
	ancient := now.Add(-2 * time.Hour)
	m.panRetryHistory = []time.Time{
		ancient,
		ancient.Add(time.Second),
		now.Add(-10 * time.Second),
		now.Add(-5 * time.Second),
	}

	allowed, recent := m.shouldRetryPan(now)
	if !allowed {
		t.Fatalf("expected allowed=true with 2 fresh + 2 old (only fresh count)")
	}
	if recent != 2 {
		t.Errorf("recentDrops: got %d want 2 (only the 2 fresh entries)", recent)
	}
	if got := len(m.panRetryHistory); got != 3 {
		t.Errorf("history after prune+append: got len %d want 3", got)
	}
}

func TestShouldRetryPan_ConcurrentCallersSerializeAtThreshold(t *testing.T) {
	t.Parallel()

	// 10 concurrent callers, exactly panRetryThreshold (3) get true
	m := newTestManager()
	now := time.Now()

	const N = 10
	var wg sync.WaitGroup
	var trueCount, falseCount atomic.Int32
	start := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			allowed, _ := m.shouldRetryPan(now)
			if allowed {
				trueCount.Add(1)
			} else {
				falseCount.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got, want := trueCount.Load(), int32(panRetryThreshold); got != want {
		t.Errorf("trueCount: got %d want %d", got, want)
	}
	if got, want := falseCount.Load(), int32(N-panRetryThreshold); got != want {
		t.Errorf("falseCount: got %d want %d", got, want)
	}
	if got, want := len(m.panRetryHistory), panRetryThreshold; got != want {
		t.Errorf("final history len: got %d want %d (only allowed callers append)", got, want)
	}
}
