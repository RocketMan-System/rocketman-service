package shared

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PidFile records the PID of the supervised sing-box so that a restarted service
// can terminate a child left behind by a crashed predecessor (it would otherwise
// keep holding the TUN interface and make every new start fail).
type PidFile struct {
	Path string
}

// Write stores pid in the file.
func (p PidFile) Write(pid int) error {
	if p.Path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0755); err != nil {
		return err
	}
	return os.WriteFile(p.Path, []byte(strconv.Itoa(pid)), 0644)
}

// Remove deletes the file if it still refers to pid.
func (p PidFile) Remove(pid int) {
	if stored, ok := p.read(); ok && stored == pid {
		_ = os.Remove(p.Path)
	}
}

func (p PidFile) read() (int, bool) {
	if p.Path == "" {
		return 0, false
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// KillStale kills the recorded process if it is still alive and runs exePath.
// The executable check protects against PID reuse.
func (p PidFile) KillStale(exePath string) (killed bool, err error) {
	pid, ok := p.read()
	if !ok {
		return false, nil
	}
	defer func() { _ = os.Remove(p.Path) }()

	if pid == os.Getpid() || !pidAlive(pid) {
		return false, nil
	}

	exe, err := processExecutable(pid)
	if err != nil || !sameExecutable(exe, exePath) {
		return false, nil
	}

	if err := killPID(pid); err != nil {
		return false, fmt.Errorf("kill stale pid %d: %w", pid, err)
	}

	deadline := time.Now().Add(ProcessKillWait)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	return true, nil
}

func sameExecutable(a, b string) bool {
	a = filepath.Clean(strings.TrimSuffix(strings.TrimSpace(a), " (deleted)"))
	b = filepath.Clean(b)
	if caseInsensitivePaths {
		return strings.EqualFold(a, b)
	}
	return a == b
}
