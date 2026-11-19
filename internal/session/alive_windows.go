//go:build windows

package session

import (
	"errors"
	"syscall"
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// ProcessAlive reports whether the process with the given PID exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// Access denied means the process exists but belongs to another user or runs elevated. Treat it
		// like EPERM on Unix.
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(handle)

	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}
