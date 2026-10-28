//go:build linux || darwin

package shared

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

func testSupervisor(t *testing.T, script string) *Supervisor {
	t.Helper()
	s := NewSupervisor(SupervisorHooks{
		Launch: func() (*exec.Cmd, func(), error) {
			return exec.Command("sh", "-c", script), nil, nil
		},
		Interrupt:       SignalTerm,
		Kill:            ForceKill,
		GracefulTimeout: time.Second,
		Log:             func(_ bool, msg string) { t.Log(msg) },
	})
	t.Cleanup(func() { s.Stop() })
	return s
}

func TestSupervisorStartStop(t *testing.T) {
	s := testSupervisor(t, "exec sleep 30")

	pid, already, err := s.Start()
	if err != nil || already || pid == 0 {
		t.Fatalf("Start() = %d, %v, %v", pid, already, err)
	}

	if _, already, _ := s.Start(); !already {
		t.Fatal("second Start should report already running")
	}

	if !s.Stop() {
		t.Fatal("Stop should report it was running")
	}
	if _, ok := s.RunningPID(); ok || s.IsActive() {
		t.Fatal("supervisor still active after Stop")
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("process still alive after Stop")
	}
	if s.Stop() {
		t.Fatal("second Stop should report not running")
	}
}

func TestSupervisorExitedImmediately(t *testing.T) {
	s := testSupervisor(t, "exit 3")

	if _, _, err := s.Start(); err == nil {
		t.Fatal("expected error for a process that exits immediately")
	}
	if s.IsActive() {
		t.Fatal("no session should be kept after a failed start")
	}
}

// A process that has exited must not be reported as running (signal 0 succeeds on zombies).
func TestSupervisorDetectsExitAndRestarts(t *testing.T) {
	marker := t.TempDir() + "/started"
	// First run dies after the startup probe, the restart keeps running.
	s := testSupervisor(t, `if [ -e "`+marker+`" ]; then exec sleep 30; fi; touch "`+marker+`"; sleep 0.8; exit 1`)

	first, _, err := s.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, ok := s.RunningPID(); ok && pid != first {
			if s.Restarts() != 1 {
				t.Fatalf("Restarts() = %d, want 1", s.Restarts())
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("crashed process was not restarted")
}

// Start arriving while Stop is still in progress must not leave an orphan or a dead tunnel.
func TestSupervisorStartDuringStop(t *testing.T) {
	// Ignore SIGTERM so Stop has to wait for the kill fallback.
	s := testSupervisor(t, `trap "" TERM; while :; do sleep 0.1; done`)

	if _, _, err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.Stop()
	}()
	time.Sleep(100 * time.Millisecond)

	pid, already, err := s.Start()
	wg.Wait()

	if err != nil || already {
		t.Fatalf("Start during Stop = %d, %v, %v", pid, already, err)
	}
	if got, ok := s.RunningPID(); !ok || got != pid {
		t.Fatalf("RunningPID() = %d, %v, want %d", got, ok, pid)
	}
}

func TestPidFileKillStale(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found")
	}
	cmd := exec.Command(sleepPath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()

	pf := PidFile{Path: t.TempDir() + "/x.pid"}
	if err := pf.Write(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}

	if killed, _ := pf.KillStale("/nonexistent/binary"); killed {
		t.Fatal("must not kill a process running a different executable")
	}

	_ = pf.Write(cmd.Process.Pid)
	resolved, _ := processExecutable(cmd.Process.Pid)
	killed, err := pf.KillStale(resolved)
	if err != nil || !killed {
		t.Fatalf("KillStale = %v, %v", killed, err)
	}
	if _, err := os.Stat(pf.Path); !os.IsNotExist(err) {
		t.Fatal("pid file should be removed")
	}
}
