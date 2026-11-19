// Package proxy runs a child process on a PTY and relays bytes unchanged between it and the user's
// terminal.
//
// The proxy does not interpret output. Terminal and Spawner are interfaces so tests can substitute
// in-memory implementations.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNoCommand means no command was specified.
	ErrNoCommand = errors.New("proxy: no command given")
	// ErrPTY means PTY allocation failed. There is no fallback to pipes.
	ErrPTY = errors.New("proxy: cannot allocate a pty")
	// ErrStart means the PTY was allocated but the command could not be started.
	ErrStart = errors.New("proxy: cannot start the command")
)

const (
	// Size to give the child when the terminal size is unavailable, for example when using pipes.
	DefaultCols = 80
	DefaultRows = 24

	// eot is Ctrl-D. At EOF on non-TTY input, send it once to the child's TTY (see eofSequence).
	eot = 0x04

	outputBufSize = 32 << 10
	inputBufSize  = 4 << 10

	// After the child exits, assume output is complete once it has been idle this long.
	defaultDrainIdle = 100 * time.Millisecond
	// Maximum time to wait after the child exits, even if output continues.
	drainMax = 2 * time.Second
	// Time to wait for the output goroutine to stop after closing the PTY.
	closeGrace = 250 * time.Millisecond
)

// Options configures Run.
type Options struct {
	// Argv specifies the command to run. Argv[0] is looked up on PATH.
	Argv []string
	// Dir is the child's working directory. An empty string uses the current directory.
	Dir string
	// Env is the child's environment. If nil, the child inherits the environment from geminus.
	Env []string
	// Terminal is the user's terminal. If nil, Run uses standard input and output.
	Terminal Terminal
	// Record receives a copy of the child's output (raw.log). A nil writer disables recording. A write
	// failure stops recording but not the relay.
	Record io.Writer
	// Spawner starts the child. If nil, Run uses go-pty to allocate a PTY.
	Spawner Spawner
	// Signals supplies signals to forward to the child. If nil, Run listens for OS signals (SIGINT,
	// SIGTERM, SIGHUP and SIGQUIT on Unix) from before it enters raw mode until it restores the
	// terminal. This prevents a signal from terminating geminus and leaving the terminal in raw mode.
	Signals <-chan os.Signal
	// Logf reports nonfatal errors, such as recording or resize failures. If nil, these errors are
	// discarded.
	Logf func(format string, args ...any)
	// DrainIdle is how long output must be idle after the child exits. Zero uses the default.
	DrainIdle time.Duration
}

// Result describes the outcome of the relay.
type Result struct {
	// Started reports whether the child started. If Run returns an error, callers can distinguish a
	// setup failure (raw mode, PTY allocation or command startup) from a relay failure after startup.
	Started bool
	// ExitCode is the child's exit code. On Unix, signal termination yields 128 plus the signal
	// number.
	ExitCode int
	// OutputBytes is the number of bytes read from the child.
	OutputBytes int64
	// RecordErr is the first error writing to Record. A recording failure does not fail the relay.
	RecordErr error
}

// Run starts opts.Argv on a PTY and relays data between the child and the terminal until the child
// exits.
//
// Run puts TTY input in raw mode and restores it before returning. For non-TTY input, it skips raw
// mode and sends one EOT at EOF, adding a newline first if needed. A nonzero child exit code is
// not an error; see Result.ExitCode. Canceling ctx kills the child.
//
// Terminal reads cannot be interrupted, so the input goroutine may remain blocked in Read after
// Run returns. If it reads standard input, it can consume the next input intended for another
// reader in the same process. Call Run only once, at the end of the process.
func Run(ctx context.Context, opts Options) (res Result, err error) {
	if len(opts.Argv) == 0 || opts.Argv[0] == "" {
		return Result{}, ErrNoCommand
	}
	term := opts.Terminal
	if term == nil {
		term = OSTerminal()
	}
	spawner := opts.Spawner
	if spawner == nil {
		spawner = PTYSpawner()
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// Start listening for signals before entering raw mode and stop after restoring the terminal
	// (deferred calls run in reverse order). Otherwise a signal such as SIGTERM could terminate the
	// process and leave the terminal in raw mode.
	signals := opts.Signals
	if signals == nil {
		ch := make(chan os.Signal, 16)
		signal.Notify(ch, handledSignals()...)
		defer signal.Stop(ch)
		signals = ch
	}

	isTTY := term.IsTTY()
	if isTTY {
		restore, rawErr := term.MakeRaw()
		if rawErr != nil {
			return Result{}, fmt.Errorf("proxy: enter raw mode: %w", rawErr)
		}
		defer func() {
			if restoreErr := restore(); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("proxy: restore terminal: %w", restoreErr))
			}
		}()
	}

	cols, rows, sizeErr := term.Size()
	if sizeErr != nil || cols <= 0 || rows <= 0 {
		cols, rows = DefaultCols, DefaultRows
	}
	child, err := spawner.Spawn(Spec{Argv: opts.Argv, Dir: opts.Dir, Env: opts.Env, Cols: cols, Rows: rows})
	if err != nil {
		return Result{}, err
	}

	r := &relay{term: term, child: child, record: opts.Record, logf: logf}
	r.touch()

	outputDone := make(chan struct{})
	go r.copyOutput(outputDone)
	go r.copyInput(isTTY)
	stopResize := term.WatchResize(r.resize)
	// Catch any resize between the initial Size call and the start of monitoring.
	if sizeErr == nil {
		r.syncSize(cols, rows)
	}
	stopSignals := r.forwardSignals(signals)

	waitDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if killErr := child.Kill(); killErr != nil {
				logf("proxy: kill child: %v", killErr)
			}
		case <-waitDone:
		}
	}()

	code, waitErr := child.Wait()
	close(waitDone)
	stopSignals()
	stopResize()

	drainIdle := opts.DrainIdle
	if drainIdle <= 0 {
		drainIdle = defaultDrainIdle
	}
	r.drain(outputDone, drainIdle)
	if closeErr := child.Close(); closeErr != nil {
		logf("proxy: close pty: %v", closeErr)
	}
	select {
	case <-outputDone:
	case <-time.After(closeGrace):
	}

	res, termErr := r.finish(code)
	if waitErr != nil {
		err = errors.Join(err, fmt.Errorf("proxy: wait for child: %w", waitErr))
	}
	if termErr != nil {
		err = errors.Join(err, fmt.Errorf("proxy: write to terminal: %w", termErr))
	}
	return res, err
}

// relay holds the state of a single relay operation.
type relay struct {
	term   Terminal
	child  Child
	record io.Writer
	logf   func(format string, args ...any)

	// mu protects writes to the terminal and recording, as well as the state below.
	mu        sync.Mutex
	stopped   bool
	outBytes  int64
	recordErr error
	termErr   error

	lastOutput atomic.Int64 // UnixNano
}

func (r *relay) touch() { r.lastOutput.Store(time.Now().UnixNano()) }

// copyOutput forwards the child's output to the terminal and the recording.
func (r *relay) copyOutput(done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, outputBufSize)
	for {
		n, err := r.child.Read(buf)
		if n > 0 {
			// Keep reading and discarding output after forwarding stops. Without a reader, child writes or
			// ConPTY shutdown can block.
			r.deliver(buf[:n])
		}
		if err != nil {
			// EOF (macOS), EIO (Linux) and a closed PTY all mean there is no more output.
			return
		}
	}
}

// deliver writes p to the terminal and copies the same bytes to the recording. It does nothing
// once forwarding stops.
func (r *relay) deliver(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	// Update the timestamp before and after writing so a slow terminal write does not count as idle
	// time.
	r.touch()
	defer r.touch()
	r.outBytes += int64(len(p))

	if _, err := r.term.Write(p); err != nil {
		// Do not leave the agent running if the user cannot see its output.
		r.termErr = err
		r.stopped = true
		go func() {
			if killErr := r.child.Kill(); killErr != nil {
				r.logf("proxy: kill child: %v", killErr)
			}
		}()
		return
	}
	if r.record != nil && r.recordErr == nil {
		if _, err := r.record.Write(p); err != nil {
			// A recording failure should not interrupt the conversation. Disable only the recording.
			r.recordErr = err
			r.logf("proxy: recording stopped: %v", err)
		}
	}
}

// copyInput forwards terminal input to the child. Run does not wait for this goroutine because
// reads from standard input cannot be interrupted.
func (r *relay) copyInput(isTTY bool) {
	buf := make([]byte, inputBufSize)
	// Before the first input byte, we are already at the start of a line.
	endsWithNewline := true
	for {
		n, err := r.term.Read(buf)
		if n > 0 {
			if _, werr := r.child.Write(buf[:n]); werr != nil {
				return // The child has exited.
			}
			endsWithNewline = buf[n-1] == '\n'
		}
		if err != nil {
			if errors.Is(err, io.EOF) && !isTTY {
				// Signal EOF on the child's TTY when piped input ends. A failed write means the child has exited.
				_, _ = r.child.Write(eofSequence(endsWithNewline))
			}
			return
		}
	}
}

// eofSequence returns the bytes to send to the child's TTY when non-TTY input ends.
//
// In canonical mode, EOT signals EOF only at the start of a line. Mid-line it merely releases the
// buffered input, so `printf abc | geminus run -- cat` would hang. Add a newline before EOT if the
// input lacks a trailing newline.
func eofSequence(endsWithNewline bool) []byte {
	if endsWithNewline {
		return []byte{eot}
	}
	return []byte{'\n', eot}
}

// resize applies the current terminal size to the child's PTY.
func (r *relay) resize() {
	cols, rows, err := r.term.Size()
	if err != nil {
		r.logf("proxy: read terminal size: %v", err)
		return
	}
	if err := r.child.Resize(cols, rows); err != nil {
		r.logf("proxy: resize pty: %v", err)
	}
}

// syncSize updates the child if the terminal dimensions have changed since it was spawned with
// cols by rows.
func (r *relay) syncSize(cols, rows int) {
	if nowCols, nowRows, err := r.term.Size(); err == nil && (nowCols != cols || nowRows != rows) {
		r.resize()
	}
}

// forwardSignals forwards signals from src to the child. The returned function stops forwarding
// and waits for any call in progress.
func (r *relay) forwardSignals(src <-chan os.Signal) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case sig := <-src:
				if !forwardable(sig) {
					continue
				}
				if err := r.child.Signal(sig); err != nil {
					r.logf("proxy: forward %v: %v", sig, err)
				}
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

// drain waits for outstanding output after the child exits.
//
// Normally PTY EOF closes done and the wait ends immediately. If a grandchild holds the PTY open
// or Windows ConPTY never reports EOF, return after output has been quiet for idle.
func (r *relay) drain(done <-chan struct{}, idle time.Duration) {
	r.touch()
	deadline := time.NewTimer(drainMax)
	defer deadline.Stop()
	tick := time.NewTicker(max(idle/4, time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-deadline.C:
			return
		case <-tick.C:
			if time.Since(time.Unix(0, r.lastOutput.Load())) >= idle {
				return
			}
		}
	}
}

// finish stops further writes and collects the result. A surviving output goroutine can no longer
// touch the terminal or recording.
func (r *relay) finish(code int) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	return Result{Started: true, ExitCode: code, OutputBytes: r.outBytes, RecordErr: r.recordErr}, r.termErr
}
