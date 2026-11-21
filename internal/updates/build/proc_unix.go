//go:build unix

package build

import (
	"os/exec"
	"syscall"
)

// stopWithChildren makes cmd the head of a process group and has its
// context stop the whole group, not cmd alone.
func stopWithChildren(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
