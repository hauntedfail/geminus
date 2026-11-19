//go:build unix

package acp

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its own process group. A terminal delivers Ctrl-C
// to every process in the foreground group; the connection must survive it,
// because Ctrl-C means "stop this turn", which geminus sends as a message.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
