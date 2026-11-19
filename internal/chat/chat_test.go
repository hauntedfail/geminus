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
	"github.com/hauntedfail/geminus/internal/harness"
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

// harness is geminus wired to in-process fake agents. Each (node, harness)
// pair has its own state file, so a reconnect finds its earlier sessions.
//
// The world: sibyl is remote and has only Hermes. chrome is this computer
// and has Hermes, OpenCode, and Claude Code without its ACP bridge. mirai
// has no route.
type world struct {
	t         *testing.T
	app       *App
	screen    *screen
	errs      *bytes.Buffer
	interrupt chan os.Signal
	dir       persona.Directory
	have      map[string][]string // node.Where() -> programs installed
	probes    []string
	dials     []string
	probeErr  error
	dialErr   error
}

func newWorld(t *testing.T) *world {
	t.Helper()
	all, err := harness.All(nil)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, screen: &screen{}, errs: &bytes.Buffer{}, interrupt: make(chan os.Signal, 1)}
	w.dir = persona.Directory{
		Workspace:  "ws",
		ConfigPath: "/cfg/config.json",
		Harnesses:  all,
		Personas: []persona.Persona{
			{Name: "chrome", Local: true, Node: &persona.Node{CWD: "/work"}},
			{Name: "mirai"},
			{Name: "sibyl", Node: &persona.Node{SSH: "btcvps", CWD: "/work"}},
		},
	}
	w.have = map[string][]string{
		"ssh btcvps":    {"hermes"},
		"this computer": {"hermes", "opencode", "claude"},
		"ssh mirai-box": {"codex", "codex-acp"},
	}
	state := t.TempDir()
	w.app = &App{
		Out:  NewPrinter(w.screen),
		Load: func(context.Context) (persona.Directory, error) { return w.dir, nil },
		Probe: func(_ context.Context, node persona.Node, _ []harness.Harness) (harness.Probe, error) {
			w.probes = append(w.probes, node.Where())
			if w.probeErr != nil {
				return harness.Probe{}, w.probeErr
			}
			probe := harness.Probe{Home: "/home/remote", Have: map[string]bool{}}
			for _, name := range w.have[node.Where()] {
				probe.Have[name] = true
			}
			return probe, nil
		},
		Dial: func(node persona.Node, h harness.Harness) (Agent, error) {
			w.dials = append(w.dials, h.ID+"@"+node.Where())
			if w.dialErr != nil {
				return nil, w.dialErr
			}
			toAgent, fromClient := io.Pipe()
			toClient, fromAgent := io.Pipe()
			go func() {
				acptest.Serve(toAgent, fromAgent, io.Discard, filepath.Join(state, h.ID+"@"+node.Where()+".json"))
				fromAgent.Close()
			}()
			return acp.New(fromClient, toClient, nil), nil
		},
		SaveDefaults: func(d persona.Defaults) error { w.dir.Defaults = d; return nil },
		SaveNode: func(name string, node persona.Node) error {
			if strings.HasPrefix(node.SSH, "-") {
				return errors.New("invalid ssh host alias")
			}
			for i := range w.dir.Personas {
				if w.dir.Personas[i].Name == name {
					w.dir.Personas[i].Node = &node
				}
			}
			return nil
		},
		ctx:       context.Background(),
		interrupt: w.interrupt,
	}
	return w
}

// run feeds the lines as typed input and returns the exit code.
func (w *world) run(args []string, lines ...string) int {
	w.t.Helper()
	input := strings.Join(lines, "\n")
	if len(lines) > 0 {
		input += "\n"
	}
	return w.app.Main(args, strings.NewReader(input), w.errs)
}

// again clears the screen for a second run in the same world.
func (w *world) again() {
	w.screen = &screen{}
	w.app.Out = NewPrinter(w.screen)
	w.errs.Reset()
	w.dials, w.probes = nil, nil
}

func (w *world) expect(wants ...string) {
	w.t.Helper()
	got := w.screen.String()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			w.t.Errorf("screen lacks %q:\n%s", want, got)
		}
	}
}

func (w *world) expectNot(unwanted ...string) {
	w.t.Helper()
	got := w.screen.String()
	for _, s := range unwanted {
		if strings.Contains(got, s) {
			w.t.Errorf("screen shows %q:\n%s", s, got)
		}
	}
}

func TestPersonaListShowsLocalityRouteAndDefault(t *testing.T) {
	w := newWorld(t)
	w.dir.Defaults.Persona = "sibyl"
	if code := w.run([]string{"persona"}); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect(
		"Personas (Honcho workspace ws)",
		"1  chrome  THIS COMPUTER  this computer",
		"2  mirai   REMOTE         no route yet",
		"3  sibyl   REMOTE         ssh btcvps  (default)",
	)
	if len(w.dials)+len(w.probes) != 0 {
		t.Errorf("listing touched a node: probes %v, dials %v", w.probes, w.dials)
	}
}

func TestBareStartAsksForAPersonaThenConnects(t *testing.T) {
	w := newWorld(t)
	w.run(nil, "nobody", "3", "hello sibyl")
	w.expect(
		"persona ❯ nobody",
		`✗ no persona "nobody" here; type a number or a name from the list`,
		"looking at sibyl [REMOTE] (ssh btcvps)…",
		"connecting to sibyl [REMOTE] · Hermes Agent (ssh btcvps)…",
		"● connected  sibyl [REMOTE]  Hermes Agent (fake-agent 1.2.3)  via ssh btcvps  session sess-0001  cwd /work",
		"sibyl [REMOTE] · hermes ❯ hello sibyl",
		"echo: hello sibyl",
	)
	// sibyl has one usable harness, so there is nothing to ask.
	w.expectNot("Harnesses on")
}

func TestBareStartUsesTheDefaultsWithoutAsking(t *testing.T) {
	w := newWorld(t)
	w.dir.Defaults = persona.Defaults{Persona: "chrome", Harness: "opencode"}
	if code := w.run(nil, "hi"); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect(
		"● connected  chrome [THIS COMPUTER]  OpenCode (fake-agent 1.2.3)  on this computer",
		"chrome [THIS COMPUTER] · opencode ❯ hi",
		"echo: hi",
	)
	w.expectNot("persona ❯", "harness ❯")
}

func TestBareStartEndsQuietlyWhenNoPersonaIsChosen(t *testing.T) {
	w := newWorld(t)
	if code := w.run(nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(w.probes)+len(w.dials) != 0 {
		t.Errorf("touched a node without a persona: %v %v", w.probes, w.dials)
	}
}

func TestHarnessChoicesAreWhatThatComputerHas(t *testing.T) {
	w := newWorld(t)
	w.run([]string{"persona", "chrome"}, "", "claude", "open", "which one?")
	w.expect(
		"Harnesses on chrome [THIS COMPUTER]",
		"  1  hermes    Hermes Agent",
		"  2  opencode  OpenCode",
		"  -  claude    Claude Code: installed, but its ACP bridge is missing (npm install -g @zed-industries/claude-agent-acp)",
		`✗ no harness "claude" here`,
		"● connected  chrome [THIS COMPUTER]  OpenCode (fake-agent 1.2.3)  on this computer",
		"chrome [THIS COMPUTER] · opencode ❯ which one?",
	)
	// Not installed on chrome, so not offered at all.
	w.expectNot("Codex", "Gemini", "Grok", "  3  ")
	if strings.Join(w.dials, ",") != "opencode@this computer" {
		t.Errorf("dials = %v", w.dials)
	}
}

func TestPersonaDefaultHarnessBeatsTheGeneralOne(t *testing.T) {
	w := newWorld(t)
	w.dir.Defaults.Harness = "hermes"
	w.dir.Personas[0].Node.Harness = "opencode"
	w.run([]string{"persona", "chrome"}, "hi")
	w.expect("chrome [THIS COMPUTER] · opencode ❯ hi")
	w.expectNot("harness ❯")

	// A default the computer does not have is not forced on it.
	w = newWorld(t)
	w.dir.Defaults.Harness = "codex"
	w.run([]string{"persona", "sibyl"}, "hi")
	w.expect("sibyl [REMOTE] · hermes ❯ hi")
}

func TestHarnessFlag(t *testing.T) {
	w := newWorld(t)
	if code := w.run([]string{"persona", "chrome", "--harness", "hermes"}, "hi"); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect("chrome [THIS COMPUTER] · hermes ❯ hi")

	w = newWorld(t)
	if code := w.run([]string{"persona", "sibyl", "--harness", "opencode"}); code != 1 || !strings.Contains(w.errs.String(), `sibyl has no harness "opencode" (it has: hermes)`) {
		t.Errorf("exit %d, stderr %q", code, w.errs)
	}
	w = newWorld(t)
	if code := w.run([]string{"persona", "chrome", "--harness", "claude"}); code != 1 || !strings.Contains(w.errs.String(), "ACP bridge is missing: npm install -g @zed-industries/claude-agent-acp") {
		t.Errorf("exit %d, stderr %q", code, w.errs)
	}
}

func TestComputerWithNoUsableHarnessSaysWhatIsMissing(t *testing.T) {
	w := newWorld(t)
	w.have["ssh btcvps"] = []string{"pi"}
	if code := w.run([]string{"persona", "sibyl"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	got := w.errs.String()
	if !strings.Contains(got, "sibyl (ssh btcvps) has no agent program geminus can talk to") || !strings.Contains(got, "pi is installed, but its ACP bridge is missing: npm install -g pi-acp") {
		t.Errorf("stderr = %q", got)
	}
}

func TestPersonaWithoutRouteIsAskedForOnceAndRemembered(t *testing.T) {
	w := newWorld(t)
	w.run(nil, "mirai", "mirai-box", "hi mirai")
	w.expect(
		"mirai [REMOTE] has no route yet",
		"ssh host alias for mirai (empty cancels) ❯ mirai-box",
		"saved in /cfg/config.json",
		"looking at mirai [REMOTE] (ssh mirai-box)…",
		// No cwd was configured, so sessions run in the remote home.
		"● connected  mirai [REMOTE]  Codex (fake-agent 1.2.3)  via ssh mirai-box  session sess-0001  cwd /home/remote",
		"mirai [REMOTE] · codex ❯ hi mirai",
	)
	if node := w.dir.Personas[1].Node; node == nil || node.SSH != "mirai-box" {
		t.Errorf("node not saved: %+v", node)
	}

	w = newWorld(t)
	if code := w.run([]string{"persona", "mirai"}, ""); code != 1 || !strings.Contains(w.errs.String(), "no route to mirai") {
		t.Errorf("empty alias: exit %d, stderr %q", code, w.errs)
	}
	if len(w.probes) != 0 {
		t.Errorf("probed without a route: %v", w.probes)
	}
}

func TestUnknownPersonaAndBadUsage(t *testing.T) {
	w := newWorld(t)
	if code := w.run([]string{"persona", "nobody"}); code != 1 || !strings.Contains(w.errs.String(), `no persona named "nobody"`) {
		t.Errorf("exit %d, stderr %q", code, w.errs)
	}
	for _, args := range [][]string{
		{"run"}, {"persona", "sibyl", "--bogus"}, {"persona", "--continue"}, {"--harness"},
		{"persona", "sibyl", "--resume"}, {"persona", "sibyl", "--continue", "--resume", "x"}, {"sibyl"},
	} {
		if code := newWorld(t).run(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestUnreachablePersonaReportsWhy(t *testing.T) {
	w := newWorld(t)
	w.probeErr = errors.New("ssh: connect to host btcvps port 22: Connection refused")
	if code := w.run([]string{"persona", "sibyl"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := w.errs.String(); !strings.Contains(got, "cannot reach sibyl (ssh btcvps)") || !strings.Contains(got, "Connection refused") {
		t.Errorf("stderr = %q", got)
	}

	// A broken default is reported, and geminus stays up to pick another.
	w = newWorld(t)
	w.probeErr = errors.New("Connection refused")
	w.dir.Defaults.Persona = "sibyl"
	if code := w.run(nil, "/status"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	w.expect("✗ cannot reach sibyl (ssh btcvps): Connection refused", "○ disconnected  no persona chosen (/persona)")
}

func TestStartFailureOfTheHarnessIsReported(t *testing.T) {
	w := newWorld(t)
	w.dialErr = errors.New(`exec: "ssh": executable file not found`)
	if code := w.run([]string{"persona", "sibyl"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := w.errs.String(); !strings.Contains(got, "cannot start Hermes Agent on sibyl") || !strings.Contains(got, "executable file not found") {
		t.Errorf("stderr = %q", got)
	}
}

func TestOneConnectionServesTheWholeConversation(t *testing.T) {
	w := newWorld(t)
	if code := w.run([]string{"persona", "sibyl"}, "one", "two", "/sessions"); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect("echo: one", "echo: two", "  … thinking", "●  1  sess-0001")
	if len(w.dials) != 1 {
		t.Errorf("dialed %d times for one conversation, want 1", len(w.dials))
	}
}

func TestContinueReopensLatestSessionAndShowsHistory(t *testing.T) {
	w := newWorld(t)
	w.run([]string{"persona", "sibyl"}, "remember plum")
	w.again()
	if code := w.run([]string{"persona", "sibyl", "--continue"}, "and now?"); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect(
		"── conversation so far ──", "you:", "remember plum", "agent:", "echo: remember plum", "── end of history ──",
		"session sess-0001",
		"echo: and now?",
	)
}

func TestSessionDefaultContinue(t *testing.T) {
	w := newWorld(t)
	w.dir.Defaults = persona.Defaults{Persona: "sibyl", Session: persona.SessionContinue}
	// Nothing to continue yet: start a session instead of failing.
	if code := w.run(nil, "first"); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect("session sess-0001", "echo: first")
	w.again()
	w.run(nil, "second")
	w.expect("── conversation so far ──", "echo: first", "session sess-0001", "echo: second")
	w.expectNot("sess-0002")
}

func TestResumeByNumberPrefixAndUnknown(t *testing.T) {
	w := newWorld(t)
	w.run([]string{"persona", "sibyl"}, "first topic", "/new", "second topic", "/resume 2", "/status", "/resume sess-0002", "/resume zzz")
	w.expect(
		"session sess-0002",
		"first topic",
		`sibyl has no session "zzz" (see /sessions)`,
	)
	if got := strings.Count(w.screen.String(), "── conversation so far ──"); got != 2 {
		t.Errorf("history shown %d times, want 2:\n%s", got, w.screen.String())
	}
	if len(w.dials) != 1 {
		t.Errorf("switching sessions redialed: %v", w.dials)
	}
}

func TestSlashCommandsGoToGeminusOrToTheAgent(t *testing.T) {
	w := newWorld(t)
	w.run([]string{"persona", "sibyl"}, "hi", "/model fast", "/help", "//help", "/zzz", "/status")
	w.expect(
		// The agent announced /model; geminus has no such command.
		"model set to fast",
		// /help is geminus's, and lists the agent's commands too.
		"geminus commands", "/harness [ID]",
		"Hermes Agent commands (sent to the agent as typed; //NAME for one geminus also has)",
		"/model", "Switch the model",
		// //help reaches the agent's own /help.
		"the agent's own help",
		"✗ unknown command /zzz (see /help)",
	)
	w.expectNot("echo: /zzz", "echo: /status")
}

func TestHarnessCommandSwitchesTheAgentOnTheSamePersona(t *testing.T) {
	w := newWorld(t)
	w.run([]string{"persona", "chrome", "--harness", "hermes"}, "to hermes", "/harness opencode", "to opencode", "/harness", "1", "back", "/harness grok")
	w.expect(
		"chrome [THIS COMPUTER] · hermes ❯ to hermes",
		"● connected  chrome [THIS COMPUTER]  OpenCode (fake-agent 1.2.3)",
		"chrome [THIS COMPUTER] · opencode ❯ to opencode",
		"Harnesses on chrome [THIS COMPUTER]",
		"chrome [THIS COMPUTER] · hermes ❯ back",
		`✗ chrome has no harness "grok" (it has: hermes, opencode)`,
	)
	if got := strings.Join(w.dials, ","); got != "hermes@this computer,opencode@this computer,hermes@this computer" {
		t.Errorf("dials = %s", got)
	}
	// Every /harness looks at the computer again.
	if len(w.probes) != 4 {
		t.Errorf("probed %d times, want 4 (start and three /harness)", len(w.probes))
	}
}

func TestDefaultCommand(t *testing.T) {
	w := newWorld(t)
	w.run([]string{"persona", "chrome", "--harness", "opencode"},
		"/default", "/default save", "/default session continue", "/default harness hermes",
		"/default persona nobody", "/default harness nope", "/default session maybe", "/default persona", "/default")
	w.expect(
		"defaults (/cfg/config.json)\n  persona  (none)\n  harness  (none)\n  session  new",
		"geminus now starts with chrome [THIS COMPUTER] · OpenCode (saved in /cfg/config.json)",
		"default session: continue",
		"default harness: hermes",
		`✗ no persona named "nobody"`,
		`✗ no harness named "nope"`,
		`✗ session is "new" or "continue"`,
		"✗ usage: /default persona VALUE (or none)",
		"  persona  chrome\n  harness  hermes\n  session  continue",
		"  harness on chrome  opencode",
	)
	want := persona.Defaults{Persona: "chrome", Harness: "hermes", Session: persona.SessionContinue}
	if w.dir.Defaults != want {
		t.Errorf("saved defaults = %+v, want %+v", w.dir.Defaults, want)
	}
	if got := w.dir.Personas[0].Node.Harness; got != "opencode" {
		t.Errorf("chrome's own harness = %q, want opencode", got)
	}

	// The saved choice is what a bare start now uses.
	w.again()
	w.run(nil, "hi")
	w.expect("chrome [THIS COMPUTER] · opencode ❯ hi")
	w.expectNot("persona ❯", "harness ❯")
}

func TestPermissionIsAskedAndDefaultsToReject(t *testing.T) {
	w := newWorld(t)
	w.run([]string{"persona", "sibyl"}, "tool one", "y", "tool two", "", "tool three", "sure why not")
	w.expect(
		"? sibyl asks to run: terminal: ls",
		"[y] allow once  [a] always allow  [n] reject (default n)",
		"permission: selected yes",
		"permission: selected no",
		"✗ terminal: ls failed",
	)
	if got := strings.Count(w.screen.String(), "permission: selected no"); got != 2 {
		t.Errorf("rejected %d times, want 2 (empty answer and unclear answer):\n%s", got, w.screen.String())
	}
}

func TestInterruptStopsTheTurnAndKeepsTheConnection(t *testing.T) {
	w := newWorld(t)
	in, typist := io.Pipe()
	done := make(chan int)
	go func() { done <- w.app.Main([]string{"persona", "sibyl"}, in, w.errs) }()

	w.screen.waitFor(t, "● connected")
	io.WriteString(typist, "slow one\n")
	w.screen.waitFor(t, "❯ slow one")
	w.interrupt <- os.Interrupt
	w.screen.waitFor(t, "(stopped)")
	io.WriteString(typist, "still there?\n")
	w.screen.waitFor(t, "echo: still there?")
	typist.Close()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(w.dials) != 1 {
		t.Errorf("dialed %v", w.dials)
	}
}

func TestLostConnectionIsShownAndResumeReconnects(t *testing.T) {
	w := newWorld(t)
	w.run([]string{"persona", "sibyl"}, "keep this", "die", "lost message", "/resume", "back")
	w.expect(
		"✗ connection to sibyl lost",
		"○ disconnected  sibyl [REMOTE]  Hermes Agent  (last session sess-0001; /resume reconnects)",
		"the message may not have been delivered; it was not sent again",
		"geminus (disconnected) ❯ lost message",
		"not connected, so nothing was sent",
		"keep this",
		"echo: back",
	)
	w.expectNot("echo: lost message", "echo: die")
	if len(w.dials) != 2 {
		t.Errorf("dialed %d times, want 2 (connect, reconnect)", len(w.dials))
	}
}

func TestSwitchingPersonaClosesTheFirstConnection(t *testing.T) {
	w := newWorld(t)
	w.dir.Defaults.Harness = "hermes"
	w.run([]string{"persona", "sibyl"}, "to sibyl", "/persona chrome", "to chrome", "/persona", "sib", "again", "/disconnect", "/status")
	w.expect(
		"sibyl [REMOTE] · hermes ❯ to sibyl",
		"connecting to chrome [THIS COMPUTER] · Hermes Agent (this computer)…",
		"chrome [THIS COMPUTER] · hermes ❯ to chrome",
		"persona ❯ sib",
		"sibyl [REMOTE] · hermes ❯ again",
		"○ disconnected  sibyl [REMOTE]  Hermes Agent",
	)
	if got := strings.Join(w.dials, ","); got != "hermes@ssh btcvps,hermes@this computer,hermes@ssh btcvps" {
		t.Errorf("dials = %s", got)
	}
}

func TestAgentTextCannotControlTheTerminal(t *testing.T) {
	w := newWorld(t)
	w.run([]string{"persona", "sibyl"}, "clear \x1b[2J\x1b]0;title\x07 done")
	if strings.ContainsAny(strings.SplitN(w.screen.String(), "echo:", 2)[1], "\x1b\x07") {
		t.Errorf("escape sequences reached the screen: %q", w.screen.String())
	}
}

func TestClip(t *testing.T) {
	if got := clip("短い説明", 10); got != "短い説明" {
		t.Errorf("clip = %q", got)
	}
	if got := clip("とても長い説明文です", 5); got != "とても長…" {
		t.Errorf("clip = %q", got)
	}
}

// refusing is an agent whose turns are refused with an error of its own,
// as when an account has no credit left.
type refusing struct{ Agent }

func (refusing) Prompt(context.Context, string, string, *acp.Handler) (string, error) {
	return "", &acp.RPCError{Method: "session/prompt", Code: -32603, Message: "Insufficient balance"}
}

func TestAgentRefusalIsShownInItsOwnWordsAndKeepsTheConnection(t *testing.T) {
	w := newWorld(t)
	dial := w.app.Dial
	w.app.Dial = func(node persona.Node, h harness.Harness) (Agent, error) {
		agent, err := dial(node, h)
		return refusing{agent}, err
	}
	w.run([]string{"persona", "sibyl"}, "hello", "/status")
	w.expect(
		"✗ Hermes Agent did not take the message: Insufficient balance",
		"● connected  sibyl [REMOTE]",
	)
	w.expectNot("session/prompt", "-32603", "connection to sibyl lost")
}

func TestVersionAndHelp(t *testing.T) {
	w := newWorld(t)
	if code := w.run([]string{"version"}); code != 0 || !strings.Contains(w.screen.String(), "geminus ") {
		t.Errorf("version: exit %d, %q", code, w.screen.String())
	}
	w = newWorld(t)
	if code := w.run([]string{"help"}); code != 0 || !strings.Contains(w.screen.String(), "geminus persona NAME") {
		t.Errorf("help: exit %d, %q", code, w.screen.String())
	}
}
