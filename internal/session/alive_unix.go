//go:build unix

package session

import (
	"errors"
	"syscall"
)

// ProcessAlive reports whether the process with the given PID exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Signal 0 checks existence and permissions without delivering a signal.
	err := syscall.Kill(pid, 0)
	// EPERM means the process exists but belongs to another user.
	return err == nil || errors.Is(err, syscall.EPERM)
}
