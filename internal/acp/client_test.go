package acp_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/acp"
	"github.com/hauntedfail/geminus/internal/acp/acptest"
)

// TestMain lets the test binary act as the fake agent, so Start can be
// exercised against a real child process.
func TestMain(m *testing.M) {
	if os.Getenv("ACPTEST_AGENT") == "1" {
		os.Exit(acptest.Serve(os.Stdin, os.Stdout, os.Stderr, os.Getenv("ACPTEST_STATE")))
	}
	os.Exit(m.Run())
}

// connect returns a client wired to an in-process fake agent.
func connect(t *testing.T) *acp.Client {
	t.Helper()
	toAgent, fromClient := io.Pipe()
	toClient, fromAgent := io.Pipe()
	go func() {
		acptest.Serve(toAgent, fromAgent, io.Discard, "")
		fromAgent.Close()
	}()
	c := acp.New(fromClient, toClient, nil)
	t.Cleanup(func() { c.Close() })
	return c
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func newSession(t *testing.T, c *acp.Client) string {
	t.Helper()
	if _, err := c.Initialize(ctx(t), "test"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	id, err := c.NewSession(ctx(t), "/work")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return id
}

func TestInitializeReportsAgentAndCapabilities(t *testing.T) {
	info, err := connect(t).Initialize(ctx(t), "test")
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	want := acp.Info{Name: acptest.AgentName, Version: "1.2.3", CanLoad: true, CanList: true}
	if info != want {
		t.Errorf("info = %+v, want %+v", info, want)
	}
}

func TestPromptStreamsTextAndThoughts(t *testing.T) {
	c := connect(t)
	id := newSession(t, c)
	var text, thought strings.Builder
	h := &acp.Handler{Text: func(s string) { text.WriteString(s) }, Thought: func(s string) { thought.WriteString(s) }}
	stop, err := c.Prompt(ctx(t), id, "hello", h)
	if err != nil || stop != "end_turn" {
		t.Fatalf("Prompt = %q, %v; want end_turn", stop, err)
	}
	if text.String() != "echo: hello" {
		t.Errorf("text = %q", text.String())
	}
	if thought.String() != "thinking about it" {
		t.Errorf("thought = %q", thought.String())
	}
}

func TestPermissionChoiceReachesAgent(t *testing.T) {
	for _, tc := range []struct{ choose, want string }{
		{"yes", "permission: selected yes"},
		{"", "permission: cancelled "},
		{"not-offered", "permission: cancelled "},
	} {
		c := connect(t)
		id := newSession(t, c)
		var text strings.Builder
		var asked acp.PermissionRequest
		var tools []acp.ToolEvent
		h := &acp.Handler{
			Text:       func(s string) { text.WriteString(s) },
			Tool:       func(e acp.ToolEvent) { tools = append(tools, e) },
			Permission: func(r acp.PermissionRequest) string { asked = r; return tc.choose },
		}
		if _, err := c.Prompt(ctx(t), id, "tool please", h); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if text.String() != tc.want {
			t.Errorf("choose %q: reply = %q, want %q", tc.choose, text.String(), tc.want)
		}
		if asked.Title != "terminal: ls" || len(asked.Options) != 3 || asked.Options[0].Kind != "allow_once" {
			t.Errorf("request = %+v", asked)
		}
		if len(tools) != 2 || tools[0].Title != "terminal: ls" || tools[0].Update || !tools[1].Update {
			t.Errorf("tool events = %+v", tools)
		}
	}
}

func TestPermissionIsDeclinedWithoutHandler(t *testing.T) {
	c := connect(t)
	id := newSession(t, c)
	var text strings.Builder
	if _, err := c.Prompt(ctx(t), id, "tool", &acp.Handler{Text: func(s string) { text.WriteString(s) }}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if text.String() != "permission: cancelled " {
		t.Errorf("reply = %q", text.String())
	}
}

func TestCancelStopsTheTurn(t *testing.T) {
	c := connect(t)
	id := newSession(t, c)
	turn, cancel := context.WithCancel(ctx(t))
	time.AfterFunc(50*time.Millisecond, cancel)
	stop, err := c.Prompt(turn, id, "slow", nil)
	if err != nil || stop != "cancelled" {
		t.Fatalf("Prompt = %q, %v; want cancelled", stop, err)
	}
	// The connection stays usable after a cancelled turn.
	if stop, err := c.Prompt(ctx(t), id, "again", nil); err != nil || stop != "end_turn" {
		t.Fatalf("next Prompt = %q, %v", stop, err)
	}
}

func TestLoadSessionReplaysHistory(t *testing.T) {
	c := connect(t)
	id := newSession(t, c)
	if _, err := c.Prompt(ctx(t), id, "first", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	var replay []string
	h := &acp.Handler{
		UserText: func(s string) { replay = append(replay, "user:"+s) },
		Text:     func(s string) { replay = append(replay, "agent:"+s) },
	}
	if err := c.LoadSession(ctx(t), "/work", id, h); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got := strings.Join(replay, "|"); got != "user:first|agent:echo: first" {
		t.Errorf("replay = %q", got)
	}
}

func TestLoadUnknownSession(t *testing.T) {
	c := connect(t)
	newSession(t, c)
	err := c.LoadSession(ctx(t), "/work", "nope", nil)
	if !errors.Is(err, acp.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestListSessionsNewestFirst(t *testing.T) {
	c := connect(t)
	first := newSession(t, c)
	second, err := c.NewSession(ctx(t), "/work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prompt(ctx(t), first, "the title", nil); err != nil {
		t.Fatal(err)
	}
	got, err := c.ListSessions(ctx(t), "/work")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 || got[0].ID != second || got[1].ID != first || got[1].Title != "the title" {
		t.Errorf("sessions = %+v", got)
	}
}

func TestRPCErrorCarriesAgentMessage(t *testing.T) {
	c := connect(t)
	newSession(t, c)
	_, err := c.Prompt(ctx(t), "unknown-session", "hi", nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Message != "session not found" {
		t.Fatalf("err = %v, want RPCError with the agent's message", err)
	}
}

// agentCommand runs this test binary as the fake agent.
func agentCommand(t *testing.T, extra ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "ACPTEST_AGENT=1")
	cmd.Env = append(cmd.Env, extra...)
	return cmd
}

func TestStartTalksToChildProcess(t *testing.T) {
	p, err := acp.Start(agentCommand(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	id := newSession(t, p.Client)
	var text strings.Builder
	if _, err := p.Prompt(ctx(t), id, "over a pipe", &acp.Handler{Text: func(s string) { text.WriteString(s) }}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if text.String() != "echo: over a pipe" {
		t.Errorf("text = %q", text.String())
	}
}

func TestDeadAgentErrorIncludesItsStderr(t *testing.T) {
	p, err := acp.Start(agentCommand(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	id := newSession(t, p.Client)
	_, err = p.Prompt(ctx(t), id, "die now", nil)
	if !errors.Is(err, acp.ErrClosed) || !strings.Contains(err.Error(), "asked to die") {
		t.Fatalf("err = %v, want ErrClosed carrying the agent's stderr", err)
	}
}

func TestCommandThatIsNotAnAgent(t *testing.T) {
	// The shape of a broken route: the remote shell cannot find the agent.
	p, err := acp.Start(exec.Command("sh", "-c", "echo 'sh: 1: exec: hermes: not found' >&2; exit 127"))
	if err != nil {
		t.Skipf("no sh: %v", err)
	}
	defer p.Close()
	_, err = p.Initialize(ctx(t), "test")
	if !errors.Is(err, acp.ErrClosed) || !strings.Contains(err.Error(), "hermes: not found") {
		t.Fatalf("err = %v, want ErrClosed carrying the shell's message", err)
	}
}

func TestCloseEndsTheAgent(t *testing.T) {
	p, err := acp.Start(agentCommand(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	newSession(t, p.Client)
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
}

func TestAgentCommandsAreRemembered(t *testing.T) {
	c := connect(t)
	id := newSession(t, c)
	// The list arrives as a notification after session/new; a round trip
	// guarantees it has been read.
	var text strings.Builder
	if _, err := c.Prompt(ctx(t), id, "/model fast", &acp.Handler{Text: func(s string) { text.WriteString(s) }}); err != nil {
		t.Fatal(err)
	}
	cmds := c.Commands(id)
	if len(cmds) != 2 || cmds[0].Name != "model" || cmds[0].Description != "Switch the model" {
		t.Errorf("commands = %+v", cmds)
	}
	if text.String() != "model set to fast" {
		t.Errorf("reply = %q", text.String())
	}
	if got := c.Commands("another-session"); len(got) != 0 {
		t.Errorf("commands of an unknown session = %+v", got)
	}
}

func TestTraceRecordsBothDirections(t *testing.T) {
	toAgent, fromClient := io.Pipe()
	toClient, fromAgent := io.Pipe()
	go func() {
		acptest.Serve(toAgent, fromAgent, io.Discard, "")
		fromAgent.Close()
	}()
	var mu sync.Mutex
	var log strings.Builder
	c := acp.New(fromClient, toClient, nil)
	c.Trace(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return log.Write(p)
	}))
	defer c.Close()
	id := newSession(t, c)
	if _, err := c.Prompt(ctx(t), id, strings.Repeat("long ", 2000), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := log.String()
	mu.Unlock()
	for _, want := range []string{` > {"id":1,"jsonrpc":"2.0","method":"initialize"`, ` < {"id":1,"jsonrpc":"2.0","result"`, `"method":"session/prompt"`, "…\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("trace lacks %q:\n%.600s", want, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 4100 {
			t.Errorf("a traced line has %d bytes", len(line))
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
