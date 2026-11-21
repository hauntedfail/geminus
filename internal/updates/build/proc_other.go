//go:build !unix

package build

import "os/exec"

// stopWithChildren does nothing here: the context stops cmd itself, and
// WaitDelay bounds the wait for what it started.
func stopWithChildren(*exec.Cmd) {}
