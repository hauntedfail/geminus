//go:build !unix && !windows

package acp

import "os/exec"

func detach(*exec.Cmd) {}
