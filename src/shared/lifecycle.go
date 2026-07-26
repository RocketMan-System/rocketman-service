package shared

import "time"

const (
	// MaxQuickRestarts is how many crash restarts are allowed inside QuickRestartWindow.
	MaxQuickRestarts = 5

	// QuickRestartWindow resets the quick-restart counter after the process stayed up this long.
	QuickRestartWindow = 10 * time.Second

	// RestartBackoff is used after MaxQuickRestarts instead of giving up forever.
	RestartBackoff = 30 * time.Second

	// ProcessStopTimeout is how long Stop waits for the monitor to reap the child.
	ProcessStopTimeout = 5 * time.Second
)

// ComputeRestartDelay returns delay before the next restart and the updated attempt counter.
// When the quick-restart budget is exhausted it backs off and resets the counter (no permanent give-up).
func ComputeRestartDelay(attempts int, lastStart time.Time) (delay time.Duration, nextAttempts int) {
	if time.Since(lastStart) >= QuickRestartWindow {
		return time.Second, 1
	}

	nextAttempts = attempts + 1
	if nextAttempts > MaxQuickRestarts {
		return RestartBackoff, 0
	}

	return time.Second, nextAttempts
}
