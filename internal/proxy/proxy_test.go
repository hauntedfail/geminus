package proxy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type runOutcome struct {
	res Result
	err error
}

// session provides a fake terminal and child for a single call to Run.
type session struct {
	t       *testing.T
	term    *memTerminal
	child   *fakeChild
	spawner *fakeSpawner
	done    chan runOutcome
}

func startSession(t *testing.T, tty bool, configure func(*Options)) *session {
	t.Helper()
	return startSessionCtx(t, context.Background(), tty, configure)
}

func startSessionCtx(t *testing.T, ctx context.Context, tty bool, configure func(*Options)) *session {
	t.Helper()
	s := &session{
		t:     t,
		term:  newMemTerminal(tty),
		child: newFakeChild(),
		done:  make(chan runOutcome, 1),
	}
	s.spawner = &fakeSpawner{child: s.child}
	opts := Options{
		Argv:     []string{"agent", "--flag"},
		Terminal: s.term,
		Spawner:  s.spawner,
		// Use an empty channel by default to avoid catching real signals.
		Signals: make(chan os.Signal),
	}
	if configure != nil {
		configure(&opts)
	}
	go func() {
		res, err := Run(ctx, opts)
		s.done <- runOutcome{res, err}
	}()
	t.Cleanup(func() {
		s.child.exit(0, true)
		s.child.outW.Close()
		s.term.inW.Close()
	})
	return s
}

func (s *session) wait() runOutcome {
	s.t.Helper()
	select {
	case out := <-s.done:
		return out
	case <-time.After(10 * time.Second):
		s.t.Fatal("Run did not return")
		return runOutcome{}
	}
}

func TestRunRelaysOutputToTerminalAndRecord(t *testing.T) {
	var record syncBuffer
	s := startSession(t, true, func(o *Options) { o.Record = &record })

	chunks := []string{"hello ", "\x1b[31mworld\x1b[0m\r\n", "\x00\xff binary \x1b[2J"}
	for _, chunk := range chunks {
		s.child.emit(t, chunk)
	}
	s.child.exit(0, true)

	out := s.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	want := strings.Join(chunks, "")
	if got := s.term.out.String(); got != want {
		t.Errorf("terminal output = %q, want %q", got, want)
	}
	if got := record.String(); got != want {
		t.Errorf("record = %q, want %q (must be byte-identical)", got, want)
	}
	if out.res.OutputBytes != int64(len(want)) {
		t.Errorf("OutputBytes = %d, want %d", out.res.OutputBytes, len(want))
	}
	if out.res.RecordErr != nil {
		t.Errorf("RecordErr = %v", out.res.RecordErr)
	}
}

func TestRunRelaysInputToChild(t *testing.T) {
	s := startSession(t, true, nil)
	s.term.typeString(t, "ping\r")
	s.term.typeString(t, "\x1b[A\x03")
	waitFor(t, "input to reach the child", func() bool { return s.child.inputString() == "ping\r\x1b[A\x03" })

	s.child.exit(0, true)
	if out := s.wait(); out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
}

func TestRunPropagatesExitCode(t *testing.T) {
	for _, code := range []int{0, 1, 7, 130, 255} {
		s := startSession(t, false, nil)
		s.child.exit(code, true)
		out := s.wait()
		if out.err != nil {
			t.Fatalf("code %d: Run: %v", code, out.err)
		}
		if out.res.ExitCode != code {
			t.Errorf("ExitCode = %d, want %d", out.res.ExitCode, code)
		}
	}
}

func TestTTYEntersRawModeAndRestores(t *testing.T) {
	s := startSession(t, true, nil)
	waitFor(t, "spawn", func() bool { return len(s.spawner.spawned()) == 1 })
	if raw, restores, _ := s.term.counts(); raw != 1 || restores != 0 {
		t.Fatalf("while running: raw = %d, restores = %d; want 1, 0", raw, restores)
	}

	s.child.exit(0, true)
	if out := s.wait(); out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	raw, restores, watchStops := s.term.counts()
	if raw != 1 || restores != 1 {
		t.Errorf("after exit: raw = %d, restores = %d; want 1, 1", raw, restores)
	}
	if watchStops != 1 {
		t.Errorf("resize watcher stopped %d times, want 1", watchStops)
	}
}

func TestTTYInputEOFDoesNotSendEOT(t *testing.T) {
	s := startSession(t, true, nil)
	s.term.typeString(t, "x")
	s.term.inW.Close()
	waitFor(t, "input", func() bool { return s.child.inputString() == "x" })
	time.Sleep(20 * time.Millisecond)
	if got := s.child.inputString(); got != "x" {
		t.Errorf("child input = %q, want %q (no EOT on a TTY)", got, "x")
	}
	s.child.exit(0, true)
	s.wait()
}

// Append a newline before EOT if non-TTY input lacks one. In canonical mode, EOT signals EOF only
// at the start of a line; otherwise the child's read would not finish.
func TestNonTTYEOFCompletesTheLastLineBeforeEOT(t *testing.T) {
	for _, tt := range []struct {
		name  string
		input []string
		want  string
	}{
		{"no trailing newline", []string{"abc"}, "abc\n\x04"},
		{"trailing newline", []string{"abc\n"}, "abc\n\x04"},
		{"empty input", nil, "\x04"},
		{"last chunk decides", []string{"a\n", "b"}, "a\nb\n\x04"},
		{"newline in an earlier chunk only", []string{"a", "b\n"}, "ab\n\x04"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := startSession(t, false, nil)
			for _, chunk := range tt.input {
				s.term.typeString(t, chunk)
			}
			s.term.inW.Close()
			waitFor(t, "EOT", func() bool { return strings.HasSuffix(s.child.inputString(), "\x04") })
			time.Sleep(20 * time.Millisecond)
			if got := s.child.inputString(); got != tt.want {
				t.Errorf("child input = %q, want %q", got, tt.want)
			}
			s.child.exit(0, true)
			if out := s.wait(); out.err != nil {
				t.Fatalf("Run: %v", out.err)
			}
		})
	}
}

func TestNonTTYStaysCookedAndSendsEOTOnceOnEOF(t *testing.T) {
	s := startSession(t, false, nil)
	s.term.typeString(t, "abc\n")
	s.term.inW.Close()
	waitFor(t, "EOT", func() bool { return strings.HasSuffix(s.child.inputString(), "\x04") })
	time.Sleep(20 * time.Millisecond)
	if got := s.child.inputString(); got != "abc\n\x04" {
		t.Errorf("child input = %q, want %q (exactly one EOT)", got, "abc\n\x04")
	}

	s.child.exit(0, true)
	if out := s.wait(); out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if raw, restores, _ := s.term.counts(); raw != 0 || restores != 0 {
		t.Errorf("raw = %d, restores = %d; want 0, 0 for non-TTY input", raw, restores)
	}
}

func TestSpawnReceivesCommandAndTerminalSize(t *testing.T) {
	s := startSession(t, true, func(o *Options) {
		o.Argv = []string{"claude", "--session-id", "u"}
		o.Dir = "/work"
		o.Env = []string{"A=1"}
	})
	s.child.exit(0, true)
	s.wait()

	specs := s.spawner.spawned()
	if len(specs) != 1 {
		t.Fatalf("spawned %d times, want 1", len(specs))
	}
	spec := specs[0]
	if strings.Join(spec.Argv, " ") != "claude --session-id u" || spec.Dir != "/work" || strings.Join(spec.Env, ",") != "A=1" {
		t.Errorf("spec = %+v", spec)
	}
	if spec.Cols != 100 || spec.Rows != 30 {
		t.Errorf("size = %dx%d, want 100x30", spec.Cols, spec.Rows)
	}
}

func TestSizeFallsBackWhenTerminalHasNone(t *testing.T) {
	term := newMemTerminal(false)
	term.sizeErr = ErrNoSize
	child := newFakeChild()
	spawner := &fakeSpawner{child: child}
	child.exit(0, true)
	if _, err := Run(context.Background(), Options{Argv: []string{"x"}, Terminal: term, Spawner: spawner, Signals: make(chan os.Signal)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	term.inW.Close()
	spec := spawner.spawned()[0]
	if spec.Cols != DefaultCols || spec.Rows != DefaultRows {
		t.Errorf("size = %dx%d, want %dx%d", spec.Cols, spec.Rows, DefaultCols, DefaultRows)
	}
}

func TestResizeIsForwardedToChild(t *testing.T) {
	s := startSession(t, true, nil)
	waitFor(t, "resize watcher", func() bool {
		s.term.mu.Lock()
		defer s.term.mu.Unlock()
		return s.term.notify != nil
	})

	s.term.resize(132, 43)
	s.term.resize(80, 24)
	waitFor(t, "resizes", func() bool {
		resizes, _, _, _ := s.child.snapshot()
		return len(resizes) == 2
	})
	resizes, _, _, _ := s.child.snapshot()
	if resizes[0] != [2]int{132, 43} || resizes[1] != [2]int{80, 24} {
		t.Errorf("resizes = %v, want [[132 43] [80 24]]", resizes)
	}

	s.child.exit(0, true)
	s.wait()
}

func TestSignalsAreForwardedToChild(t *testing.T) {
	signals := make(chan os.Signal)
	s := startSession(t, true, func(o *Options) { o.Signals = signals })

	signals <- os.Interrupt
	signals <- os.Kill
	waitFor(t, "signals", func() bool {
		_, got, _, _ := s.child.snapshot()
		return len(got) == 2
	})
	_, got, _, _ := s.child.snapshot()
	if got[0] != os.Interrupt || got[1] != os.Kill {
		t.Errorf("signals = %v", got)
	}

	s.child.exit(3, true)
	if out := s.wait(); out.res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", out.res.ExitCode)
	}
}

func TestRecordFailureDoesNotStopRelay(t *testing.T) {
	record := &failingWriter{failAfter: 1}
	var logged []string
	s := startSession(t, true, func(o *Options) {
		o.Record = record
		o.Logf = func(format string, args ...any) { logged = append(logged, format) }
	})

	for _, chunk := range []string{"one ", "two ", "three"} {
		s.child.emit(t, chunk)
	}
	s.child.exit(0, true)

	out := s.wait()
	if out.err != nil {
		t.Fatalf("Run returned an error for a record failure: %v", out.err)
	}
	if got := s.term.out.String(); got != "one two three" {
		t.Errorf("terminal output = %q, want everything despite the record failure", got)
	}
	if !errors.Is(out.res.RecordErr, errDiskFull) {
		t.Errorf("RecordErr = %v, want errDiskFull", out.res.RecordErr)
	}
	calls, written := record.stats()
	if calls != 2 || written != "one " {
		t.Errorf("record got %d writes (%q); want 2 calls, then no more after the failure", calls, written)
	}
	if len(logged) == 0 {
		t.Error("record failure was not reported through Logf")
	}
}

func TestSpawnFailureRestoresTerminal(t *testing.T) {
	term := newMemTerminal(true)
	defer term.inW.Close()
	spawnErr := errors.Join(ErrPTY, errors.New("out of ptys"))
	_, err := Run(context.Background(), Options{
		Argv:     []string{"agent"},
		Terminal: term,
		Spawner:  &fakeSpawner{err: spawnErr},
		Signals:  make(chan os.Signal),
	})
	if !errors.Is(err, ErrPTY) {
		t.Fatalf("err = %v, want ErrPTY", err)
	}
	if raw, restores, _ := term.counts(); raw != 1 || restores != 1 {
		t.Errorf("raw = %d, restores = %d; want 1, 1", raw, restores)
	}
}

func TestRawModeFailureDoesNotSpawn(t *testing.T) {
	term := newMemTerminal(true)
	defer term.inW.Close()
	term.rawErr = errors.New("tcsetattr failed")
	spawner := &fakeSpawner{child: newFakeChild()}
	_, err := Run(context.Background(), Options{Argv: []string{"agent"}, Terminal: term, Spawner: spawner, Signals: make(chan os.Signal)})
	if err == nil || !strings.Contains(err.Error(), "tcsetattr failed") {
		t.Fatalf("err = %v, want the raw mode failure", err)
	}
	if n := len(spawner.spawned()); n != 0 {
		t.Errorf("spawned %d times, want 0", n)
	}
}

// Result.Started tells callers whether the child started. Failures before startup (raw mode, PTY
// allocation or empty argv) leave it false.
func TestResultStartedTellsWhetherTheChildRan(t *testing.T) {
	t.Run("raw mode failure", func(t *testing.T) {
		term := newMemTerminal(true)
		defer term.inW.Close()
		term.rawErr = errors.New("tcsetattr failed")
		res, err := Run(context.Background(), Options{Argv: []string{"agent"}, Terminal: term, Spawner: &fakeSpawner{child: newFakeChild()}, Signals: make(chan os.Signal)})
		if err == nil || res.Started {
			t.Errorf("res = %+v, err = %v; want an error and Started false", res, err)
		}
	})
	t.Run("spawn failure", func(t *testing.T) {
		term := newMemTerminal(false)
		defer term.inW.Close()
		res, err := Run(context.Background(), Options{Argv: []string{"agent"}, Terminal: term, Spawner: &fakeSpawner{err: ErrStart}, Signals: make(chan os.Signal)})
		if !errors.Is(err, ErrStart) || res.Started {
			t.Errorf("res = %+v, err = %v; want ErrStart and Started false", res, err)
		}
	})
	t.Run("empty argv", func(t *testing.T) {
		res, err := Run(context.Background(), Options{Terminal: newMemTerminal(false)})
		if !errors.Is(err, ErrNoCommand) || res.Started {
			t.Errorf("res = %+v, err = %v; want ErrNoCommand and Started false", res, err)
		}
	})
	t.Run("child ran", func(t *testing.T) {
		s := startSession(t, false, nil)
		s.child.exit(3, true)
		out := s.wait()
		if out.err != nil || !out.res.Started || out.res.ExitCode != 3 {
			t.Errorf("res = %+v, err = %v; want Started true with exit code 3", out.res, out.err)
		}
	})
	// A terminal write failure after startup still counts as a successful start.
	t.Run("child ran but the relay failed", func(t *testing.T) {
		s := startSession(t, false, nil)
		s.term.mu.Lock()
		s.term.writeErr = errors.New("broken pipe")
		s.term.mu.Unlock()
		s.child.emit(t, "output")
		out := s.wait()
		if out.err == nil || !out.res.Started {
			t.Errorf("res = %+v, err = %v; want an error with Started true", out.res, out.err)
		}
	})
}

func TestEmptyArgv(t *testing.T) {
	for _, argv := range [][]string{nil, {}, {""}} {
		spawner := &fakeSpawner{child: newFakeChild()}
		_, err := Run(context.Background(), Options{Argv: argv, Terminal: newMemTerminal(false), Spawner: spawner})
		if !errors.Is(err, ErrNoCommand) {
			t.Errorf("argv %q: err = %v, want ErrNoCommand", argv, err)
		}
		if n := len(spawner.spawned()); n != 0 {
			t.Errorf("argv %q: spawned %d times", argv, n)
		}
	}
}

func TestContextCancelKillsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := startSessionCtx(t, ctx, true, nil)
	waitFor(t, "spawn", func() bool { return len(s.spawner.spawned()) == 1 })

	cancel()
	out := s.wait()
	if _, _, killed, _ := s.child.snapshot(); killed != 1 {
		t.Errorf("killed = %d, want 1", killed)
	}
	if out.res.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137", out.res.ExitCode)
	}
	if _, restores, _ := s.term.counts(); restores != 1 {
		t.Errorf("restores = %d, want 1", restores)
	}
}

func TestTerminalWriteFailureKillsChild(t *testing.T) {
	s := startSession(t, true, nil)
	s.term.mu.Lock()
	s.term.writeErr = errors.New("stdout closed")
	s.term.mu.Unlock()

	go s.child.outW.Write([]byte("lost"))
	out := s.wait()
	if out.err == nil || !strings.Contains(out.err.Error(), "stdout closed") {
		t.Fatalf("err = %v, want the terminal write failure", out.err)
	}
	if _, _, killed, _ := s.child.snapshot(); killed != 1 {
		t.Errorf("killed = %d, want 1", killed)
	}
	if _, restores, _ := s.term.counts(); restores != 1 {
		t.Errorf("restores = %d, want 1", restores)
	}
}

// Do not lose output that arrives just after the child exits.
func TestOutputAfterExitIsDrained(t *testing.T) {
	var record syncBuffer
	s := startSession(t, false, func(o *Options) { o.Record = &record })
	s.child.emit(t, "before ")
	s.child.exit(0, false) // Wait returns, but output is still open.
	go func() {
		time.Sleep(10 * time.Millisecond)
		s.child.outW.Write([]byte("after"))
		s.child.outW.Close()
	}()

	out := s.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if got := s.term.out.String(); got != "before after" {
		t.Errorf("terminal output = %q, want %q", got, "before after")
	}
	if got := record.String(); got != "before after" {
		t.Errorf("record = %q", got)
	}
}

// Run returns even if a grandchild keeps the PTY open and no EOF arrives.
func TestRunReturnsWhenOutputNeverCloses(t *testing.T) {
	var record syncBuffer
	s := startSession(t, false, func(o *Options) {
		o.Record = &record
		o.DrainIdle = 20 * time.Millisecond
	})
	s.child.emit(t, "partial")
	s.child.exit(5, false)

	start := time.Now()
	out := s.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if out.res.ExitCode != 5 {
		t.Errorf("ExitCode = %d, want 5", out.res.ExitCode)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Run took %v to give up on a PTY that never closes", elapsed)
	}
	if _, _, _, closed := s.child.snapshot(); closed == 0 {
		t.Error("child was not closed")
	}
}

// After Run returns, a surviving output goroutine must not write to the recording or terminal. The
// caller can close the recording immediately.
func TestNoWritesAfterRunReturns(t *testing.T) {
	var record syncBuffer
	s := startSession(t, false, func(o *Options) {
		o.Record = &record
		o.DrainIdle = 20 * time.Millisecond
	})
	s.child.mu.Lock()
	s.child.ignoreClose = true // Close does not unblock Read, so the reader survives.
	s.child.mu.Unlock()

	s.child.emit(t, "before")
	s.child.exit(0, false)
	if out := s.wait(); out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}

	// emit blocks until the surviving reader receives "late".
	s.child.emit(t, "late")
	s.child.emit(t, "later")
	if got := record.String(); got != "before" {
		t.Errorf("record = %q after Run returned, want %q", got, "before")
	}
	if got := s.term.out.String(); got != "before" {
		t.Errorf("terminal = %q after Run returned, want %q", got, "before")
	}
}

// Forward signals received before startup once the child is running.
func TestSignalQueuedBeforeSpawnIsForwarded(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	s := startSession(t, true, func(o *Options) { o.Signals = signals })
	waitFor(t, "queued signal", func() bool {
		_, got, _, _ := s.child.snapshot()
		return len(got) == 1 && got[0] == os.Interrupt
	})
	s.child.exit(0, true)
	s.wait()
}

// Catch a resize between the initial size query and registration of the watcher.
func TestResizeDuringStartupIsNotLost(t *testing.T) {
	term := newMemTerminal(true)
	term.sizeOnWatch = &[2]int{120, 40}
	child := newFakeChild()
	spawner := &fakeSpawner{child: child}
	done := make(chan runOutcome, 1)
	go func() {
		res, err := Run(context.Background(), Options{Argv: []string{"agent"}, Terminal: term, Spawner: spawner, Signals: make(chan os.Signal)})
		done <- runOutcome{res, err}
	}()
	defer term.inW.Close()

	waitFor(t, "catch-up resize", func() bool {
		resizes, _, _, _ := child.snapshot()
		return len(resizes) == 1
	})
	if resizes, _, _, _ := child.snapshot(); resizes[0] != [2]int{120, 40} {
		t.Errorf("resizes = %v, want [[120 40]]", resizes)
	}
	if spec := spawner.spawned()[0]; spec.Cols != 100 || spec.Rows != 30 {
		t.Errorf("initial size = %dx%d, want 100x30", spec.Cols, spec.Rows)
	}
	child.exit(0, true)
	if out := <-done; out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
}

func TestWaitErrorIsReturned(t *testing.T) {
	s := startSession(t, true, nil)
	s.child.mu.Lock()
	s.child.waitErr = errors.New("wait failed")
	s.child.mu.Unlock()
	s.child.exit(0, true)
	out := s.wait()
	if out.err == nil || !strings.Contains(out.err.Error(), "wait failed") {
		t.Fatalf("err = %v, want the wait failure", out.err)
	}
	if _, restores, _ := s.term.counts(); restores != 1 {
		t.Errorf("restores = %d, want 1", restores)
	}
}

func TestNewTerminalOverStreamsIsNotATTY(t *testing.T) {
	var out syncBuffer
	term := NewTerminal(strings.NewReader("in"), &out)
	if term.IsTTY() {
		t.Error("IsTTY = true for an in-memory reader")
	}
	if _, _, err := term.Size(); !errors.Is(err, ErrNoSize) {
		t.Errorf("Size err = %v, want ErrNoSize", err)
	}
	buf := make([]byte, 8)
	n, _ := term.Read(buf)
	if string(buf[:n]) != "in" {
		t.Errorf("Read = %q", buf[:n])
	}
	if _, err := term.Write([]byte("out")); err != nil || out.String() != "out" {
		t.Errorf("Write: %v, out = %q", err, out.String())
	}
	if _, err := term.MakeRaw(); err == nil {
		t.Error("MakeRaw succeeded on a non-TTY")
	}
	stop := term.WatchResize(func() { t.Error("resize notified on a non-TTY") })
	stop()
}

func TestNewTerminalOverNonTTYFiles(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	term := NewTerminal(r, w)
	if term.IsTTY() {
		t.Error("IsTTY = true for a pipe")
	}
	if _, _, err := term.Size(); !errors.Is(err, ErrNoSize) {
		t.Errorf("Size err = %v, want ErrNoSize", err)
	}
}
