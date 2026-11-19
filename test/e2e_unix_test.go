//go:build unix

package e2e

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
)

// liveRun is a running `geminus run` process with standard input held open.
type liveRun struct {
	t      *testing.T
	stdin  io.WriteCloser
	stdout *lockedBuffer
	stderr *lockedBuffer
	pid    int
	done   chan error
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// start launches geminus and keeps its standard input pipe open. The fake agent runs until EOF.
func (h *harness) start(args ...string) *liveRun {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	h.t.Cleanup(cancel)
	cmd := h.command(ctx, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		h.t.Fatal(err)
	}
	live := &liveRun{t: h.t, stdin: stdin, stdout: &lockedBuffer{}, stderr: &lockedBuffer{}, done: make(chan error, 1)}
	cmd.Stdout, cmd.Stderr = live.stdout, live.stderr
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	live.pid = cmd.Process.Pid
	go func() { live.done <- cmd.Wait() }()
	return live
}

// wait waits for geminus to exit and returns its exit code.
func (l *liveRun) wait() int {
	l.t.Helper()
	select {
	case err := <-l.done:
		if err == nil {
			return 0
		}
		if state, ok := err.(interface{ ExitCode() int }); ok {
			return state.ExitCode()
		}
		l.t.Fatalf("wait for geminus: %v", err)
	case <-time.After(commandTimeout):
		l.t.Fatalf("geminus did not exit\nstdout: %q\nstderr: %q", l.stdout.String(), l.stderr.String())
	}
	return -1
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startAgent runs the fake agent, waits until it is ready and returns the session.
func (h *harness) startAgent() (*liveRun, sessionInfo) {
	h.t.Helper()
	live := h.start("run", "--agent", "cmd", "--", fakeBin)
	waitUntil(h.t, "the agent to start", func() bool { return strings.Contains(live.stdout.String(), "ready") })
	return live, h.only()
}

func TestRunningSessionCannotBeOpenedTwice(t *testing.T) {
	h := newHarness(t)
	live, session := h.startAgent()

	if !session.Active || session.Status != "running" || session.PID != live.pid {
		t.Fatalf("while running: %+v, want an active session owned by pid %d", session, live.pid)
	}
	if out := h.ok("", "ls"); !strings.Contains(out.stdout, "running") {
		t.Errorf("ls = %q, want the session shown as running", out.stdout)
	}

	second := h.run("", "run", "--resume", session.ID, "--", fakeBin)
	if second.code != 1 || !strings.Contains(second.stderr, "already open") || !strings.Contains(second.stderr, "--force") {
		t.Errorf("second run: exit %d, stderr %q; want a refusal that mentions --force", second.code, second.stderr)
	}
	if out := h.run("", "rm", session.ID); out.code != 1 {
		t.Errorf("rm of a running session: exit %d, want 1", out.code)
	}
	if got := h.show(session.ID); !got.Active || got.PID != live.pid {
		t.Errorf("the refused commands changed the session: %+v", got)
	}

	// --force allows opening a session that is still running.
	if forced := h.run("", "run", "--resume", session.ID, "--force", "--", fakeBin); forced.code != 0 {
		t.Errorf("run --force: exit %d (stderr: %q)", forced.code, forced.stderr)
	}

	if _, err := io.WriteString(live.stdin, "still here\n"); err != nil {
		t.Fatal(err)
	}
	if err := live.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if code := live.wait(); code != 0 {
		t.Fatalf("first run: exit %d (stderr: %q)", code, live.stderr.String())
	}
	if !strings.Contains(live.stdout.String(), "echo:still here") {
		t.Errorf("first run stdout = %q, want it to keep relaying", live.stdout.String())
	}
	if got := h.show(session.ID); got.Active || got.Status != "exited" || got.PID != 0 {
		t.Errorf("after exit: %+v", got)
	}
	h.ok("", "rm", session.ID)
}

// geminus forwards SIGTERM to the child and records the child's exit in the session.
func TestSignalIsForwardedAndTheExitIsRecorded(t *testing.T) {
	h := newHarness(t)
	live, session := h.startAgent()

	if err := syscall.Kill(live.pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	want := 128 + int(syscall.SIGTERM)
	if code := live.wait(); code != want {
		t.Fatalf("exit = %d, want %d (stderr: %q)", code, want, live.stderr.String())
	}
	got := h.show(session.ID)
	if got.Status != "exited" || got.ExitCode == nil || *got.ExitCode != want || got.Active {
		t.Errorf("after SIGTERM: %+v, want exit_code %d", got, want)
	}
	events := h.events(session.ID)
	if last := events[len(events)-1]; last.Type != "session.exit" || last.Code == nil || *last.Code != want {
		t.Errorf("last event = %+v", last)
	}
}

// The next run can open the session even after geminus is killed with kill -9.
func TestSessionSurvivesACrashOfGeminus(t *testing.T) {
	h := newHarness(t)
	live, session := h.startAgent()

	if err := syscall.Kill(live.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	live.wait()

	crashed := h.show(session.ID)
	if crashed.Status != "running" || crashed.Active {
		t.Errorf("after the crash: %+v, want status running but not active", crashed)
	}
	if out := h.ok("", "ls"); !strings.Contains(out.stdout, "stale") {
		t.Errorf("ls = %q, want the crashed session shown as stale", out.stdout)
	}

	// Reopen without --force and append to the previous recording.
	h.ok("after crash\n", "run", "--resume", session.ID, "--", fakeBin)
	got := h.show(session.ID)
	if got.Status != "exited" || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("after the resumed run: %+v", got)
	}
	raw := h.readFile(h.sessionFile(session.ID, "raw.log"))
	if !strings.Contains(raw, "echo:after crash") {
		t.Errorf("raw.log = %q, want the resumed run appended", raw)
	}
	// Output from the crashed run remains in raw.log, but its output event may not have been flushed
	// because events are batched by time window. Events after resume need only point to the newly
	// appended data.
	var end int64
	for _, e := range h.events(session.ID) {
		if e.Type != "output" {
			continue
		}
		if *e.Offset < end || *e.Offset+*e.Bytes > int64(len(raw)) {
			t.Errorf("output event %+d..+%d does not fit raw.log (%d bytes, previous end %d)", *e.Offset, *e.Bytes, len(raw), end)
		}
		end = *e.Offset + *e.Bytes
	}
	if end != int64(len(raw)) {
		t.Errorf("the last output event ends at %d, raw.log has %d bytes", end, len(raw))
	}
}

var (
	rawModeLeft = regexp.MustCompile(`(^|\s)-icanon(\s|;|$)`)
	echoOff     = regexp.MustCompile(`(^|\s)-echo(\s|;|$)`)
)

// terminalSession is a shell running on a PTY with the test acting as the terminal.
type terminalSession struct {
	t      *testing.T
	pty    gopty.Pty
	cmd    *gopty.Cmd
	screen *lockedBuffer
}

// onTerminal runs script in sh on a PTY. The script can use $GEMINUS and $FAKE.
func (h *harness) onTerminal(script string, env ...string) *terminalSession {
	h.t.Helper()
	pty, err := gopty.New()
	if err != nil {
		h.t.Skipf("cannot allocate a pty: %v", err)
	}
	h.t.Cleanup(func() { pty.Close() })
	if err := pty.Resize(100, 30); err != nil {
		h.t.Fatal(err)
	}
	cmd := pty.Command("sh", "-c", script)
	cmd.Dir = h.dir
	cmd.Env = h.environ(append([]string{"GEMINUS=" + geminusBin, "FAKE=" + fakeBin}, env...)...)
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	term := &terminalSession{t: h.t, pty: pty, cmd: cmd, screen: &lockedBuffer{}}
	// Closing the PTY unblocks Read and lets this goroutine exit.
	go io.Copy(term.screen, pty)
	return term
}

func (s *terminalSession) expect(text string) {
	s.t.Helper()
	waitUntil(s.t, "the terminal to show "+text, func() bool { return strings.Contains(s.screen.String(), text) })
}

func (s *terminalSession) typeKeys(keys string) {
	s.t.Helper()
	if _, err := io.WriteString(s.pty, keys); err != nil {
		s.t.Fatal(err)
	}
}

// finish waits for the script's final output marker, verifies the shell exited and returns the
// screen contents.
func (s *terminalSession) finish(marker string) string {
	s.t.Helper()
	s.expect(marker)
	if err := s.cmd.Wait(); err != nil {
		s.t.Fatalf("wait for the shell: %v", err)
	}
	return s.screen.String()
}

// On a TTY, relay in raw mode and restore the terminal settings on exit.
func TestRunOnATerminalRestoresIt(t *testing.T) {
	h := newHarness(t)
	// Run stty after geminus to verify that the terminal is back in cooked mode.
	term := h.onTerminal(`"$GEMINUS" run --agent cmd -- "$FAKE"; echo "rc=$?"; stty -a; echo stty-done`, "FAKE_EXIT=5")
	term.expect("ready")
	term.typeKeys("hello\r")
	term.expect("echo:hello")
	term.typeKeys("quit\r")
	out := term.finish("stty-done")

	if !strings.Contains(out, "rc=5") {
		t.Fatalf("screen = %q, want geminus to return the agent's exit code 5", out)
	}
	session := h.only()
	if !strings.Contains(out, "session "+session.ID+" ended (exit 5)") {
		t.Errorf("screen = %q, want the closing line with the session id", out)
	}
	settings := out[strings.Index(out, "rc=5"):]
	if rawModeLeft.MatchString(settings) || echoOff.MatchString(settings) {
		t.Errorf("the terminal was left in raw mode:\n%s", settings)
	}

	// TTY input does not produce input events; it appears in raw.log only as echoed output.
	for _, e := range h.events(session.ID) {
		if e.Type == "input" {
			t.Errorf("a terminal session recorded an input event: %+v", e)
		}
	}
	text := h.ok("", "log", session.ID).stdout
	if !strings.Contains(text, "tty:true") || !strings.Contains(text, "echo:hello") {
		t.Errorf("log = %q", text)
	}
	if got := h.show(session.ID); got.ExitCode == nil || *got.ExitCode != 5 {
		t.Errorf("session = %+v, want exit_code 5", got)
	}
}

// The child's PTY inherits the terminal dimensions. Without a terminal, use 80x24.
func TestTerminalSizeReachesTheAgent(t *testing.T) {
	h := newHarness(t)
	out := h.onTerminal(`"$GEMINUS" run --agent cmd -- sh -c 'echo "size=$(stty size)"'; echo size-done`).finish("size-done")
	if !strings.Contains(out, "size=30 100") {
		t.Errorf("screen = %q, want the agent to see the terminal's 30 rows and 100 columns", out)
	}
	piped := h.ok("", "run", "--agent", "cmd", "--", "sh", "-c", `echo "size=$(stty size)"`)
	if !strings.Contains(piped.stdout, "size=24 80") {
		t.Errorf("without a terminal the agent saw %q, want the 24x80 fallback", piped.stdout)
	}
}
