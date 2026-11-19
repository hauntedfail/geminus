//go:build unix

package acp

import (
	"os/exec"
	"syscall"
	"testing"
)

// Ctrl-C in a terminal signals the whole foreground process group. The agent
// connection must not be in it, or interrupting a turn would kill ssh.
func TestStartKeepsTheChildOutOfOurProcessGroup(t *testing.T) {
	p, err := Start(exec.Command("sh", "-c", "cat"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	childGroup, err := syscall.Getpgid(p.cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if childGroup == syscall.Getpgrp() {
		t.Fatalf("child shares our process group %d; a terminal interrupt would reach it", childGroup)
	}
	if childGroup != p.cmd.Process.Pid {
		t.Errorf("child group = %d, want its own pid %d", childGroup, p.cmd.Process.Pid)
	}
}
