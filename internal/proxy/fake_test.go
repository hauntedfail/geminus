package proxy

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// memTerminal is an in-memory terminal with a pipe for input and a buffer for output.
type memTerminal struct {
	inR *io.PipeReader
	inW *io.PipeWriter
	out syncBuffer

	mu         sync.Mutex
	tty        bool
	cols, rows int
	sizeErr    error
	rawErr     error
	writeErr   error
	rawCount   int
	restores   int
	notify     func()
	watchStops int
	// If sizeOnWatch is set, change the size when WatchResize registers to simulate a resize between
	// the size query and the start of monitoring.
	sizeOnWatch *[2]int
}

func newMemTerminal(tty bool) *memTerminal {
	r, w := io.Pipe()
	return &memTerminal{inR: r, inW: w, tty: tty, cols: 100, rows: 30}
}

func (m *memTerminal) Read(p []byte) (int, error) { return m.inR.Read(p) }

func (m *memTerminal) Write(p []byte) (int, error) {
	m.mu.Lock()
	err := m.writeErr
	m.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return m.out.Write(p)
}

func (m *memTerminal) IsTTY() bool { return m.tty }

func (m *memTerminal) MakeRaw() (func() error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rawErr != nil {
		return nil, m.rawErr
	}
	m.rawCount++
	return func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.restores++
		return nil
	}, nil
}

func (m *memTerminal) Size() (int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cols, m.rows, m.sizeErr
}

func (m *memTerminal) WatchResize(notify func()) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notify = notify
	if m.sizeOnWatch != nil {
		m.cols, m.rows = m.sizeOnWatch[0], m.sizeOnWatch[1]
	}
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.notify = nil
		m.watchStops++
	}
}

// resize changes the terminal size and notifies the watcher, if any.
func (m *memTerminal) resize(cols, rows int) {
	m.mu.Lock()
	m.cols, m.rows = cols, rows
	notify := m.notify
	m.mu.Unlock()
	if notify != nil {
		notify()
	}
}

func (m *memTerminal) typeString(t *testing.T, s string) {
	t.Helper()
	if _, err := m.inW.Write([]byte(s)); err != nil {
		t.Fatalf("type %q: %v", s, err)
	}
}

func (m *memTerminal) counts() (raw, restores, watchStops int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rawCount, m.restores, m.watchStops
}

// fakeChild is an in-memory child whose output and exit are controlled by the test.
type fakeChild struct {
	outR *io.PipeReader
	outW *io.PipeWriter

	exitOnce sync.Once
	exited   chan struct{}

	mu       sync.Mutex
	code     int
	waitErr  error
	input    bytes.Buffer
	resizes  [][2]int
	signals  []os.Signal
	killed   int
	closed   int
	writeErr error
	// If ignoreClose is true, Close does not unblock Read. This simulates a reader that survives
	// close, as with a blocking PTY master descriptor.
	ignoreClose bool
}

func newFakeChild() *fakeChild {
	r, w := io.Pipe()
	return &fakeChild{outR: r, outW: w, exited: make(chan struct{})}
}

func (c *fakeChild) Read(p []byte) (int, error) { return c.outR.Read(p) }

func (c *fakeChild) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.input.Write(p)
}

func (c *fakeChild) Resize(cols, rows int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resizes = append(c.resizes, [2]int{cols, rows})
	return nil
}

func (c *fakeChild) Wait() (int, error) {
	<-c.exited
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code, c.waitErr
}

func (c *fakeChild) Signal(sig os.Signal) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signals = append(c.signals, sig)
	return nil
}

func (c *fakeChild) Kill() error {
	c.mu.Lock()
	c.killed++
	c.mu.Unlock()
	c.exit(137, true)
	return nil
}

func (c *fakeChild) Close() error {
	c.mu.Lock()
	c.closed++
	ignore := c.ignoreClose
	c.mu.Unlock()
	if ignore {
		return nil
	}
	// As on a real PTY, Close unblocks a pending Read.
	return c.outR.CloseWithError(os.ErrClosed)
}

// emit sends one chunk of child output and blocks until the proxy reads it.
func (c *fakeChild) emit(t *testing.T, s string) {
	t.Helper()
	if _, err := c.outW.Write([]byte(s)); err != nil {
		t.Fatalf("emit %q: %v", s, err)
	}
}

// exit terminates the child with code. If eof is true, it also closes output, as on normal exit.
// Otherwise it simulates a grandchild holding the PTY open and preventing EOF.
func (c *fakeChild) exit(code int, eof bool) {
	c.exitOnce.Do(func() {
		c.mu.Lock()
		c.code = code
		c.mu.Unlock()
		if eof {
			c.outW.Close()
		}
		close(c.exited)
	})
}

func (c *fakeChild) inputString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.input.String()
}

func (c *fakeChild) snapshot() (resizes [][2]int, signals []os.Signal, killed, closed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][2]int(nil), c.resizes...), append([]os.Signal(nil), c.signals...), c.killed, c.closed
}

// fakeSpawner returns a prepared fakeChild and remembers the Spec it received.
type fakeSpawner struct {
	child *fakeChild
	err   error

	mu    sync.Mutex
	specs []Spec
}

func (s *fakeSpawner) Spawn(spec Spec) (Child, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs = append(s.specs, spec)
	if s.err != nil {
		return nil, s.err
	}
	return s.child, nil
}

func (s *fakeSpawner) spawned() []Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Spec(nil), s.specs...)
}

// failingWriter succeeds on the first failAfter writes and fails on every subsequent write.
type failingWriter struct {
	mu        sync.Mutex
	failAfter int
	calls     int
	written   bytes.Buffer
}

var errDiskFull = errors.New("disk full")

func (w *failingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls > w.failAfter {
		return 0, errDiskFull
	}
	return w.written.Write(p)
}

func (w *failingWriter) stats() (calls int, written string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls, w.written.String()
}

// waitFor waits until cond is true.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
