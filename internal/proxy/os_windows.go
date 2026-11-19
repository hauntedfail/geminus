//go:build windows

package proxy

import (
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

const (
	enableVirtualTerminalProcessing = 0x0004
	disableNewlineAutoReturn        = 0x0008
	enableVirtualTerminalInput      = 0x0200

	// Windows has no SIGWINCH; poll for size changes at this interval.
	resizePollInterval = 250 * time.Millisecond
)

var procSetConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

// handledSignals lists the signals geminus handles while relaying.
func handledSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// forwardable reports whether to forward sig to the child.
func forwardable(os.Signal) bool {
	return true
}

// signalChild sends Ctrl-C as ConPTY input. For other signals the only option is to terminate the
// child forcibly.
func signalChild(proc *os.Process, input io.Writer, sig os.Signal) error {
	if sig == os.Interrupt {
		_, err := input.Write([]byte{0x03})
		return err
	}
	return proc.Kill()
}

func killChild(proc *os.Process) error {
	return proc.Kill()
}

func exitCode(state *os.ProcessState) int {
	return state.ExitCode()
}

// watchResize polls the console size and calls notify when it changes.
func watchResize(t *stdTerminal, notify func()) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(resizePollInterval)
		defer ticker.Stop()
		lastCols, lastRows, _ := t.Size()
		for {
			select {
			case <-ticker.C:
				cols, rows, err := t.Size()
				if err != nil || (cols == lastCols && rows == lastRows) {
					continue
				}
				lastCols, lastRows = cols, rows
				notify()
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// enableVT enables VT sequences on the console: arrow keys and similar input arrive as escape
// sequences, and the console interprets sequences in child output. GetConsoleMode fails for
// redirected handles, which are left alone. Only restore output here; term.Restore handles input.
func enableVT(in, out *os.File) func() {
	addConsoleMode(in, enableVirtualTerminalInput)
	return addConsoleMode(out, enableVirtualTerminalProcessing|disableNewlineAutoReturn)
}

func addConsoleMode(f *os.File, flags uint32) (restore func()) {
	restore = func() {}
	if f == nil {
		return restore
	}
	handle := syscall.Handle(f.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(handle, &mode); err != nil {
		return restore
	}
	if !setConsoleMode(handle, mode|flags) {
		// Older consoles without VT support can still use raw mode.
		return restore
	}
	return func() { setConsoleMode(handle, mode) }
}

func setConsoleMode(handle syscall.Handle, mode uint32) bool {
	ret, _, _ := procSetConsoleMode.Call(uintptr(handle), uintptr(mode))
	return ret != 0
}
