//go:build unix

package proxy

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// handledSignals lists the signals geminus handles while relaying.
//
// Forward SIGINT, SIGTERM, SIGHUP and SIGQUIT. Ctrl-C travels through the PTY as a byte in raw
// mode, so these signals come from outside. Catch SIGPIPE without forwarding it: writing to closed
// stdout (`geminus run | head`) then returns EPIPE instead of terminating geminus before it can
// restore the terminal.
func handledSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGPIPE}
}

// forwardable reports whether to forward sig to the child.
func forwardable(sig os.Signal) bool {
	return sig != syscall.SIGPIPE
}

// signalChild sends sig to the child's process group. go-pty starts the child with Setsid, so it
// is the session leader and its PGID equals its PID.
//
// The caller (ptyChild) stops signaling after Wait returns. There is a brief race between the
// kernel reaping the child and the caller marking it as reaped. Reuse of the same PGID in that
// interval would require PID wraparound, so we ignore it.
func signalChild(proc *os.Process, _ io.Writer, sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return proc.Signal(sig)
	}
	err := syscall.Kill(-proc.Pid, s)
	if errors.Is(err, syscall.ESRCH) {
		return nil // The child has already exited.
	}
	return err
}

func killChild(proc *os.Process) error {
	return signalChild(proc, nil, syscall.SIGKILL)
}

// exitCode returns the exit status, using the shell convention of 128 plus the signal number for
// signal termination.
func exitCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}

// watchResize calls notify on each SIGWINCH.
func watchResize(_ *stdTerminal, notify func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ch:
				notify()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
		wg.Wait()
	}
}

// enableVT is needed only for Windows consoles. Unix terminals already interpret VT sequences.
func enableVT(_, _ *os.File) func() {
	return func() {}
}
