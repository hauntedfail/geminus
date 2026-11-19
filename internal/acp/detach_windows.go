//go:build windows

package acp

import (
	"os/exec"
	"syscall"
)

// detach starts the child in a new process group, which does not receive the
// console's Ctrl-C. See detach_unix.go for why.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
