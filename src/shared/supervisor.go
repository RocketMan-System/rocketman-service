package shared

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// SupervisorHooks provides the platform-specific parts of process supervision.
type SupervisorHooks struct {
	// Launch builds a new, not yet started command. It is called for every (re)start.
	// cleanup (may be nil) runs once the process has been reaped.
	Launch func() (cmd *exec.Cmd, cleanup func(), err error)

	// Interrupt asks the process to exit gracefully.
	Interrupt func(p *os.Process)

	// Kill terminates the process immediately.
	Kill func(p *os.Process)

	// OnStarted runs after every successful start, including crash restarts.
	OnStarted func(p *os.Process)

	// OnExited runs after the process has been reaped (and cleanup has run).
	OnExited func(pid int)

	// GracefulTimeout is how long to wait after Interrupt before Kill.
	GracefulTimeout time.Duration

	// Log receives lifecycle messages. Defaults to the standard logger.
	Log func(isError bool, msg string)
}

// ErrExitedImmediately is returned by Supervisor.Start when the process dies during StartupProbe.
var ErrExitedImmediately = errors.New("Process exited immediately")

// Supervisor runs a single child process and restarts it after crashes.
//
// Start/Stop are serialized, so a Start that arrives while a Stop is still
// tearing the process down waits for it instead of racing it. Every child has
// exactly one goroutine calling Wait, and liveness is derived from that Wait
// rather than from signal 0 (which reports zombies as alive).
type Supervisor struct {
	hooks SupervisorHooks

	opMu    sync.Mutex // serializes Start/Stop
	mu      sync.Mutex // guards session and session.current
	session *supervisorSession
}

type supervisorSession struct {
	stop     chan struct{}
	finished chan struct{}
	current  *childProcess
	restarts int
}

type childProcess struct {
	process   *os.Process
	startedAt time.Time
	done      chan struct{}
	err       error // valid once done is closed
}

func (c *childProcess) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// NewSupervisor creates a supervisor with the given platform hooks.
func NewSupervisor(hooks SupervisorHooks) *Supervisor {
	if hooks.Log == nil {
		hooks.Log = func(_ bool, msg string) { log.Println(msg) }
	}
	if hooks.Kill == nil {
		hooks.Kill = func(p *os.Process) { _ = p.Kill() }
	}
	if hooks.Interrupt == nil {
		hooks.Interrupt = hooks.Kill
	}
	return &Supervisor{hooks: hooks}
}

func (s *Supervisor) logf(isError bool, format string, args ...interface{}) {
	s.hooks.Log(isError, fmt.Sprintf(format, args...))
}

// Start launches the process unless it is already running.
// A session that is waiting to restart a crashed process is replaced by an immediate start.
func (s *Supervisor) Start() (pid int, alreadyRunning bool, err error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	if pid, ok := s.RunningPID(); ok {
		return pid, true, nil
	}

	s.stopSessionLocked()

	child, err := s.launch()
	if err != nil {
		return 0, false, fmt.Errorf("Failed to start process: %v", err)
	}

	select {
	case <-child.done:
		if child.err != nil {
			return 0, false, fmt.Errorf("%w: %v", ErrExitedImmediately, child.err)
		}
		return 0, false, ErrExitedImmediately
	case <-time.After(StartupProbe):
	}

	sess := &supervisorSession{
		stop:     make(chan struct{}),
		finished: make(chan struct{}),
		current:  child,
	}

	s.mu.Lock()
	s.session = sess
	s.mu.Unlock()

	go s.supervise(sess)

	return child.process.Pid, false, nil
}

// Stop terminates the process and cancels pending restarts.
// It returns true if a process was running.
func (s *Supervisor) Stop() bool {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	return s.stopSessionLocked()
}

// RunningPID returns the PID of the live process, if any.
func (s *Supervisor) RunningPID() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.session == nil || s.session.current == nil || s.session.current.exited() {
		return 0, false
	}
	return s.session.current.process.Pid, true
}

// IsActive reports whether the process is running or scheduled to be restarted.
func (s *Supervisor) IsActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.session != nil
}

// Restarts returns how many crash restarts happened in the current session.
func (s *Supervisor) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.session == nil {
		return 0
	}
	return s.session.restarts
}

// stopSessionLocked must be called with opMu held.
func (s *Supervisor) stopSessionLocked() bool {
	s.mu.Lock()
	sess := s.session
	s.session = nil
	wasRunning := sess != nil && sess.current != nil && !sess.current.exited()
	s.mu.Unlock()

	if sess == nil {
		return false
	}

	close(sess.stop)

	// supervise() may be in the middle of a launch, so allow for that on top of terminate().
	select {
	case <-sess.finished:
	case <-time.After(s.hooks.GracefulTimeout + ProcessKillWait + 3*time.Second):
		s.logf(true, "Supervisor did not finish in time, process may be left running")
	}

	return wasRunning
}

func (s *Supervisor) launch() (*childProcess, error) {
	cmd, cleanup, err := s.hooks.Launch()
	if err != nil {
		return nil, err
	}
	cmd.WaitDelay = ChildWaitDelay

	if err := cmd.Start(); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}

	child := &childProcess{
		process:   cmd.Process,
		startedAt: time.Now(),
		done:      make(chan struct{}),
	}

	go func() {
		child.err = cmd.Wait()
		if cleanup != nil {
			cleanup()
		}
		if s.hooks.OnExited != nil {
			s.hooks.OnExited(child.process.Pid)
		}
		close(child.done)
	}()

	if s.hooks.OnStarted != nil {
		s.hooks.OnStarted(cmd.Process)
	}

	return child, nil
}

// terminate stops the child gracefully, falling back to Kill.
func (s *Supervisor) terminate(child *childProcess) {
	if child == nil || child.exited() {
		return
	}

	s.hooks.Interrupt(child.process)

	select {
	case <-child.done:
		return
	case <-time.After(s.hooks.GracefulTimeout):
	}

	s.logf(true, "Process didn't exit gracefully, killing it")
	s.hooks.Kill(child.process)

	select {
	case <-child.done:
	case <-time.After(ProcessKillWait):
		s.logf(true, "Process did not exit after kill")
	}
}

// supervise watches the current child and restarts it after crashes until the session is stopped.
func (s *Supervisor) supervise(sess *supervisorSession) {
	defer close(sess.finished)

	attempts := 0

	for {
		s.mu.Lock()
		child := sess.current
		s.mu.Unlock()

		select {
		case <-sess.stop:
			s.terminate(child)
			return
		case <-child.done:
		}

		s.logf(true, "Tunnel process crashed: %v", child.err)

		s.mu.Lock()
		sess.current = nil
		s.mu.Unlock()

		lastStart := child.startedAt
		for {
			var delay time.Duration
			delay, attempts = ComputeRestartDelay(attempts, lastStart)

			if delay >= RestartBackoff {
				s.logf(true, "Quick restart budget exhausted, backing off for %v", delay)
			} else {
				s.logf(false, "Attempting to restart tunnel (attempt %d/%d) in %v",
					attempts, MaxQuickRestarts, delay)
			}

			select {
			case <-sess.stop:
				return
			case <-time.After(delay):
			}

			next, err := s.launch()
			if err != nil {
				s.logf(true, "Failed to restart tunnel: %v", err)
				lastStart = time.Now()
				continue
			}

			s.mu.Lock()
			sess.current = next
			sess.restarts++
			s.mu.Unlock()

			s.logf(false, "Tunnel restarted: PID=%d", next.process.Pid)
			break
		}
	}
}
