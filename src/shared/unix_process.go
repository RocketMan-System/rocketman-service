//go:build linux || darwin

package shared

import (
	"log"
	"os"
	"syscall"
	"time"
)

// IsProcessAlive returns true if the process is still running.
// It uses signal 0, which checks process existence without delivering a signal.
func IsProcessAlive(process *os.Process) bool {
	return process.Signal(syscall.Signal(0)) == nil
}

// GracefulStop sends SIGTERM and waits up to timeout for the process to exit.
// If it doesn't exit in time, SIGKILL is sent.
func GracefulStop(process *os.Process, timeout time.Duration) {
	if err := process.Signal(syscall.SIGTERM); err != nil {
		log.Printf("Error sending SIGTERM: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := process.Wait()
		done <- err
	}()

	select {
	case <-done:
		// exited gracefully
	case <-time.After(timeout):
		log.Println("Process didn't exit gracefully, sending SIGKILL")
		_ = process.Signal(syscall.SIGKILL)
		_, _ = process.Wait()
	}
}
