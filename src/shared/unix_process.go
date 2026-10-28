//go:build linux || darwin

package shared

import (
	"log"
	"os"
	"syscall"
)

const caseInsensitivePaths = false

// SignalTerm asks the process to exit. The supervisor reaps it via Wait.
func SignalTerm(process *os.Process) {
	if process == nil {
		return
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		log.Printf("Error sending SIGTERM: %v", err)
	}
}

// ForceKill sends SIGKILL.
func ForceKill(process *os.Process) {
	if process == nil {
		return
	}

	if err := process.Signal(syscall.SIGKILL); err != nil {
		log.Printf("Error sending SIGKILL: %v", err)
	}
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

func killPID(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
