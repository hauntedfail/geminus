package chat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/acp"
	"github.com/hauntedfail/geminus/internal/acp/acptest"
	"github.com/hauntedfail/geminus/internal/persona"
)

// screen is what the person would see: a concurrency-safe output buffer.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *screen) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(s.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("screen never showed %q:\n%s", want, s.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// harness is geminus wired to in-process fake agents, one state file per
// persona so that a reconnect finds the sessions of the earlier connection.
type harness struct {
	t         *testing.T
	app       *App
	screen    *screen
	errs      *bytes.Buffer
	interrupt chan os.Signal
	dials     []string
	dialErr   error
}

func directory() persona.Directory {
	return persona.Directory{
		Workspace:  "ws",
		ConfigPath: "/cfg/config.json",
		Personas: []persona.Persona{
			{Name: "chrome", Local: true, Route: &persona.Route{Runtime: "hermes", CWD: "/work"}},
			{Name: "mirai"},
			{Name: "sibyl", Route: &persona.Route{SSH: "btcvps", Runtime: "hermes", CWD: "/work"}},
		},
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, screen: &screen{}, errs: &bytes.Buffer{}, interrupt: make(chan os.Signal, 1)}
	state := t.TempDir()
	h.app = &App{
		Out:  NewPrinter(h.screen),
		Load: func(context.Context) (persona.Directory, error) { return directory(), nil },
		Dial: func(route persona.Route) (Agent, error) {
			h.dials = append(h.dials, route.Describe())
			if h.dialErr != nil {
				return nil, h.dialErr
			}
			toAgent, fromClient := io.Pipe()
			toClient, fromAgent := io.Pipe()
			go func() {
				acptest.Serve(toAgent, fromAgent, io.Discard, filepath.Join(state, route.Describe()+".json"))
				fromAgent.Close()
			}()
			return acp.New(fromClient, toClient, nil), nil
		},
		ctx:       context.Background(),
		interrupt: h.interrupt,
	}
	return h
}

// run feeds the lines as typed input and returns the exit code.
func (h *harness) run(args []string, lines ...string) int {
	h.t.Helper()
	input := strings.Join(lines, "\n")
	if len(lines) > 0 {
		input += "\n"
	}
	return h.app.Main(args, strings.NewReader(input), h.errs)
}

func (h *harness) expect(wants ...string) {
	h.t.Helper()
	got := h.screen.String()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			h.t.Errorf("screen lacks %q:\n%s", want, got)
		}
	}
}

func (h *harness) expectNot(unwanted ...string) {
	h.t.Helper()
	got := h.screen.String()
	for _, s := range unwanted {
		if strings.Contains(got, s) {
			h.t.Errorf("screen shows %q:\n%s", s, got)
		}
	}
}

func TestPersonaListsEveryoneWithLocalityAndRoute(t *testing.T) {
	h := newHarness(t)
	if code := h.run([]string{"persona"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errs)
	}
	h.expect(
		"Personas (Honcho workspace ws)",
		"1  chrome  THIS COMPUTER  hermes (local)",
		`2  mirai   REMOTE         no route: add "mirai" to /cfg/config.json`,
		"3  sibyl   REMOTE         hermes via ssh btcvps",
	)
	if len(h.dials) != 0 {
		t.Errorf("listing connected to %v", h.dials)
	}
}

func TestBareStartIsDisconnectedUntilAPersonaIsPicked(t *testing.T) {
	h := newHarness(t)
	h.run(nil, "hello?", "3", "hello sibyl")
	h.expect(
		"geminus (disconnected) ❯ hello?",
		"not connected, so nothing was sent",
		"connecting to sibyl [REMOTE] (hermes via ssh btcvps)…",
		"● connected  sibyl [REMOTE]  fake-agent 1.2.3 via ssh btcvps  session sess-0001  cwd /work",
		"sibyl [REMOTE] ❯ hello sibyl",
		"echo: hello sibyl",
	)
	if strings.Count(h.screen.String(), "echo:") != 1 {
		t.Errorf("the message typed while disconnected was sent:\n%s", h.screen.String())
	}
}

func TestPersonaNameConnectsAndKeepsOneSession(t *testing.T) {
	h := newHarness(t)
	if code := h.run([]string{"persona", "sibyl"}, "one", "two", "/sessions"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errs)
	}
	h.expect("echo: one", "echo: two", "  … thinking", "●  1  sess-0001")
	if len(h.dials) != 1 {
		t.Errorf("dialed %d times for one conversation, want 1", len(h.dials))
	}
}

func TestLocalPersonaIsAlwaysTagged(t *testing.T) {
	h := newHarness(t)
	h.run([]string{"persona", "chrome"}, "hi")
	h.expect(
		"● connected  chrome [THIS COMPUTER]  fake-agent 1.2.3 local",
		"chrome [THIS COMPUTER] ❯ hi",
	)
}

func TestPersonaWithoutRouteFailsWithTheFix(t *testing.T) {
	h := newHarness(t)
	if code := h.run([]string{"persona", "mirai"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := h.errs.String(); !strings.Contains(got, "no route to mirai") || !strings.Contains(got, "/cfg/config.json") {
		t.Errorf("stderr = %q", got)
	}
	if len(h.dials) != 0 {
		t.Errorf("dialed %v", h.dials)
	}
}

func TestUnknownPersonaAndBadUsage(t *testing.T) {
	h := newHarness(t)
	if code := h.run([]string{"persona", "nobody"}); code != 1 || !strings.Contains(h.errs.String(), `no persona named "nobody"`) {
		t.Errorf("exit %d, stderr %q", code, h.errs)
	}
	for _, args := range [][]string{{"run"}, {"persona", "sibyl", "--bogus"}, {"persona", "--continue"}} {
		if code := newHarness(t).run(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestUnreachablePersonaReportsWhy(t *testing.T) {
	h := newHarness(t)
	h.dialErr = errors.New(`exec: "ssh": executable file not found`)
	if code := h.run([]string{"persona", "sibyl"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := h.errs.String(); !strings.Contains(got, "cannot reach sibyl") || !strings.Contains(got, "executable file not found") {
		t.Errorf("stderr = %q", got)
	}
}

func TestContinueReopensLatestSessionAndShowsHistory(t *testing.T) {
	h := newHarness(t)
	h.run([]string{"persona", "sibyl"}, "remember plum")
	first := h.screen.String()

	h.screen = &screen{}
	h.app.Out = NewPrinter(h.screen)
	if code := h.run([]string{"persona", "sibyl", "--continue"}, "and now?"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errs)
	}
	h.expect(
		"── conversation so far ──", "you:", "remember plum", "agent:", "echo: remember plum", "── end of history ──",
		"session sess-0001",
		"echo: and now?",
	)
	if !strings.Contains(first, "session sess-0001") {
		t.Errorf("first run used another session:\n%s", first)
	}
}

func TestResumeByNumberPrefixAndUnknown(t *testing.T) {
	h := newHarness(t)
	h.run([]string{"persona", "sibyl"}, "first topic", "/new", "second topic", "/resume 2", "/status", "/resume sess-0002", "/resume zzz")
	h.expect(
		"session sess-0002",
		"first topic", // replayed when session 2 in the list (sess-0001) is reopened
		`sibyl has no session "zzz" (see /sessions)`,
	)
	if got := strings.Count(h.screen.String(), "── conversation so far ──"); got != 2 {
		t.Errorf("history shown %d times, want 2:\n%s", got, h.screen.String())
	}
	if len(h.dials) != 1 {
		t.Errorf("switching sessions redialed: %v", h.dials)
	}
}

func TestPermissionIsAskedAndDefaultsToReject(t *testing.T) {
	h := newHarness(t)
	h.run([]string{"persona", "sibyl"}, "tool one", "y", "tool two", "", "tool three", "sure why not")
	h.expect(
		"? sibyl asks to run: terminal: ls",
		"[y] allow once  [a] always allow  [n] reject (default n)",
		"permission: selected yes",
		"permission: selected no",
		"✗ terminal: ls failed",
	)
	if got := strings.Count(h.screen.String(), "permission: selected no"); got != 2 {
		t.Errorf("rejected %d times, want 2 (empty answer and unclear answer):\n%s", got, h.screen.String())
	}
}

func TestInterruptStopsTheTurnAndKeepsTheConnection(t *testing.T) {
	h := newHarness(t)
	in, typist := io.Pipe()
	done := make(chan int)
	go func() { done <- h.app.Main([]string{"persona", "sibyl"}, in, h.errs) }()

	h.screen.waitFor(t, "● connected")
	io.WriteString(typist, "slow one\n")
	h.screen.waitFor(t, "❯ slow one")
	h.interrupt <- os.Interrupt
	h.screen.waitFor(t, "(stopped)")
	io.WriteString(typist, "still there?\n")
	h.screen.waitFor(t, "echo: still there?")
	typist.Close()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(h.dials) != 1 {
		t.Errorf("dialed %v", h.dials)
	}
}

func TestLostConnectionIsShownAndResumeReconnects(t *testing.T) {
	h := newHarness(t)
	h.run([]string{"persona", "sibyl"}, "keep this", "die", "lost message", "/resume", "back")
	h.expect(
		"✗ connection to sibyl lost",
		"○ disconnected (last: sibyl [REMOTE], session sess-0001; /resume reconnects)",
		"the message may not have been delivered; it was not sent again",
		"geminus (disconnected) ❯ lost message",
		"not connected, so nothing was sent",
		"keep this", // history after reconnecting to the same session
		"echo: back",
	)
	h.expectNot("echo: lost message", "echo: die")
	if len(h.dials) != 2 {
		t.Errorf("dialed %d times, want 2 (connect, reconnect)", len(h.dials))
	}
}

func TestSwitchingPersonaClosesTheFirstConnection(t *testing.T) {
	h := newHarness(t)
	h.run([]string{"persona", "sibyl"}, "to sibyl", "/persona chrome", "to chrome", "/disconnect", "/status")
	h.expect(
		"sibyl [REMOTE] ❯ to sibyl",
		"connecting to chrome [THIS COMPUTER] (hermes (local))…",
		"chrome [THIS COMPUTER] ❯ to chrome",
		"○ disconnected",
	)
	if strings.Join(h.dials, ",") != "hermes via ssh btcvps,hermes (local)" {
		t.Errorf("dials = %v", h.dials)
	}
}

func TestAgentTextCannotControlTheTerminal(t *testing.T) {
	h := newHarness(t)
	h.run([]string{"persona", "sibyl"}, "clear \x1b[2J\x1b]0;title\x07 done")
	if strings.ContainsAny(strings.SplitN(h.screen.String(), "echo:", 2)[1], "\x1b\x07") {
		t.Errorf("escape sequences reached the screen: %q", h.screen.String())
	}
}

func TestVersionAndHelp(t *testing.T) {
	h := newHarness(t)
	if code := h.run([]string{"version"}); code != 0 || !strings.Contains(h.screen.String(), "geminus ") {
		t.Errorf("version: exit %d, %q", code, h.screen.String())
	}
	h = newHarness(t)
	if code := h.run([]string{"help"}); code != 0 || !strings.Contains(h.screen.String(), "geminus persona NAME") {
		t.Errorf("help: exit %d, %q", code, h.screen.String())
	}
}
