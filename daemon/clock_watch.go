package daemon

import (
	"fmt"
	"sync"
	"time"
)

const (
	clockWatchInterval = 2 * time.Second
	clockStepMin       = 2 * time.Second
)

// returns how far wall time moved beyond monotonic elapsed time
func wallStep(prev, now time.Time) time.Duration {
	return now.Round(0).Sub(prev.Round(0)) - now.Sub(prev)
}

type clockStepTracker struct {
	mu   sync.Mutex
	last string
}

func (t *clockStepTracker) note(step time.Duration, at time.Time) {
	t.mu.Lock()
	t.last = fmt.Sprintf("%+.0fs at %s", step.Seconds(), at.Format("15:04:05"))
	t.mu.Unlock()
}

func (t *clockStepTracker) Last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}

func (app *App) startClockWatch() {
	go func() {
		prev := time.Now()
		for {
			time.Sleep(clockWatchInterval)
			now := time.Now()
			if step := wallStep(prev, now); step >= clockStepMin || step <= -clockStepMin {
				app.log.Infof("clock: wall time jumped %+.0fs (NTP step or resume from sleep) — log timestamps before this line are offset by that amount", step.Seconds())
				app.clockSteps.note(step, now)
			}
			prev = now
		}
	}()
}
