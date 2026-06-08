package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestSessionRetryBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second}, // 32s, cap kicks in
		{10, 30 * time.Second},
		{30, 30 * time.Second}, // far past the cap, no overflow
		{31, 30 * time.Second}, // 1<<31 overflows a 32-bit int
		{63, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tt.attempt), func(t *testing.T) {
			t.Parallel()
			if got := sessionRetryBackoff(tt.attempt); got != tt.want {
				t.Errorf("sessionRetryBackoff(%d) = %v, want %v", tt.attempt, got, tt.want)
			}
		})
	}
}

// the backoff is always a usable positive delay no greater than the cap
func TestSessionRetryBackoffAlwaysPositiveAndCapped(t *testing.T) {
	t.Parallel()
	const cap = 30 * time.Second
	for attempt := 0; attempt <= 70; attempt++ {
		if got := sessionRetryBackoff(attempt); got <= 0 || got > cap {
			t.Fatalf("sessionRetryBackoff(%d) = %v, want in (0, %v]", attempt, got, cap)
		}
	}
}

// setOnlineState/waitOnline barrier tests. uses testing/synctest for deterministic goroutine parking

func TestWaitOnline_ReturnsImmediatelyWhenOnline(t *testing.T) {
	t.Parallel()

	app := &App{onlineCh: make(chan struct{})}
	app.setOnlineState(true)

	err := app.waitOnline(context.Background(), 1*time.Second)
	if err != nil {
		t.Errorf("waitOnline when online: got %v, want nil", err)
	}
}

func TestWaitOnline_BlocksThenReleasesOnSetOnlineTrue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := &App{onlineCh: make(chan struct{})}

		done := make(chan error, 1)
		go func() {
			done <- app.waitOnline(context.Background(), 10*time.Second)
		}()

		// wait until the waiter goroutine is durably blocked at the select
		synctest.Wait()

		// trigger offline to online
		app.setOnlineState(true)

		// wait again until the goroutine completes
		synctest.Wait()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("waitOnline: got %v, want nil after setOnlineState(true)", err)
			}
		default:
			t.Fatal("waitOnline did not release after setOnlineState(true)")
		}
	})
}

func TestWaitOnline_BroadcastsToMultipleWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// close() broadcasts to all waiters, locks in that semantics
		app := &App{onlineCh: make(chan struct{})}

		const N = 5
		done := make(chan error, N)
		for i := 0; i < N; i++ {
			go func() {
				done <- app.waitOnline(context.Background(), 10*time.Second)
			}()
		}

		synctest.Wait()
		app.setOnlineState(true)
		synctest.Wait()

		for i := 0; i < N; i++ {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("waiter %d: got %v, want nil", i, err)
				}
			default:
				t.Fatalf("only %d/%d waiters released", i, N)
			}
		}
	})
}

func TestWaitOnline_RespectsContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := &App{onlineCh: make(chan struct{})}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- app.waitOnline(ctx, 10*time.Second)
		}()

		synctest.Wait()
		cancel()
		synctest.Wait()

		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waitOnline: got %v, want context.Canceled", err)
		}
	})
}

func TestWaitOnline_SafetyTimeoutFiresOnIndefiniteOffline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// regression: network monitor wedged should not deadlock the daemon
		app := &App{onlineCh: make(chan struct{})}

		done := make(chan error, 1)
		go func() {
			done <- app.waitOnline(context.Background(), 100*time.Millisecond)
		}()

		synctest.Wait()
		// synthetic time advances the timer inside waitOnline
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()

		err := <-done
		if err == nil {
			t.Fatal("waitOnline: got nil, want timeout error")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Errorf("waitOnline error: got %q, want 'timed out'", err.Error())
		}
	})
}

func TestSetOnlineState_NoOpWhenAlreadyOnline(t *testing.T) {
	t.Parallel()

	app := &App{onlineCh: make(chan struct{})}
	app.setOnlineState(true)
	chAfterFirst := app.onlineCh

	app.setOnlineState(true) // must be a no-op

	if app.onlineCh != chAfterFirst {
		t.Error("setOnlineState(true) when online should not replace onlineCh")
	}
	// Verify the channel is NOT closed
	select {
	case <-app.onlineCh:
		t.Error("onlineCh was closed by no-op setOnlineState(true); double-close protection failed")
	default:
	}
}

func TestSetOnlineState_NoOpWhenAlreadyOffline(t *testing.T) {
	t.Parallel()

	app := &App{onlineCh: make(chan struct{})}
	chBefore := app.onlineCh

	app.setOnlineState(false) // no-op

	if app.onlineCh != chBefore {
		t.Error("setOnlineState(false) when offline should not replace onlineCh")
	}
}

func TestSetOnlineState_OnlineToOfflineDoesNotReleaseWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// online to offline must NOT close onlineCh
		app := &App{onlineCh: make(chan struct{})}
		app.setOnlineState(true)  // now online
		app.setOnlineState(false) // back offline; channel should stay un-closed

		done := make(chan error, 1)
		go func() {
			// Short timeout so the test doesn't take forever, we EXPECT a timeout error here.
			done <- app.waitOnline(context.Background(), 50*time.Millisecond)
		}()

		synctest.Wait()
		time.Sleep(75 * time.Millisecond)
		synctest.Wait()

		err := <-done
		if err == nil {
			t.Error("waiter released on online, offline transition; should have timed out")
		}
	})
}
