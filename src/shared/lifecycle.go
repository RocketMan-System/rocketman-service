package shared

import "time"

const (
	// MaxQuickRestarts is how many crash restarts are allowed inside QuickRestartWindow.
	MaxQuickRestarts = 5

	// QuickRestartWindow resets the quick-restart counter after the process stayed up this long.
	QuickRestartWindow = 10 * time.Second

	// RestartBackoff is used after MaxQuickRestarts instead of giving up forever.
	RestartBackoff = 30 * time.Second

	// StartupProbe is how long Start waits to see whether the process dies right away.
	StartupProbe = 500 * time.Millisecond

	// ProcessKillWait is how long to wait for the process to be reaped after a forced kill.
	ProcessKillWait = 2 * time.Second

	// ChildWaitDelay bounds how long Wait keeps draining stdout/stderr after the process exited
	// (e.g. when a grandchild inherited the pipes).
	ChildWaitDelay = 2 * time.Second

	// AppUnresponsiveTimeout is how long the main app may stay silent before the tunnel is stopped.
	AppUnresponsiveTimeout = 15 * time.Second

	// AppResumeGrace suppresses app-liveness verdicts right after a system sleep/resume.
	AppResumeGrace = 30 * time.Second
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
