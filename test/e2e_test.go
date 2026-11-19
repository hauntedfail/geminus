// Package e2e tests the built geminus binary by running it as a subprocess.
//
// Place test/fakeagent on PATH as claude or hermes to run it instead of the real agent. Each test
// uses its own GEMINUS_HOME.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// commandTimeout bounds a single geminus invocation so a hang becomes a test failure.
const commandTimeout = 30 * time.Second

var (
	geminusBin string
	fakeBin    string
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "geminus-e2e-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dir)

	geminusBin = filepath.Join(dir, exeName("geminus"))
	fakeBin = filepath.Join(dir, exeName("fakeagent"))
	for out, pkg := range map[string]string{geminusBin: "./cmd/geminus", fakeBin: "./test/fakeagent"} {
		if err := build(out, pkg); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
			return 1
		}
	}
	return m.Run()
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// build builds pkg from the repository root (these tests run in test/).
func build(out, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = ".."
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %v\n%s", pkg, err, output)
	}
	return nil
}

// outcome holds the result of one geminus invocation.
type outcome struct {
	code   int
	stdout string
	stderr string
}

// harness holds the state directory and environment for a single test.
type harness struct {
	t    *testing.T
	home string
	// dir is the working directory for geminus.
	dir string
	// env holds additional environment entries ("KEY=value").
	env []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{t: t, home: filepath.Join(t.TempDir(), "home"), dir: t.TempDir()}
}

// withAgents prepends a directory to PATH containing fakeagent under each of the given names.
func (h *harness) withAgents(names ...string) *harness {
	h.t.Helper()
	bin := h.t.TempDir()
	data, err := os.ReadFile(fakeBin)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(bin, exeName(name)), data, 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
	h.env = append(h.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return h
}

// environ builds the child's environment without inheriting the user's GEMINUS_HOME or FAKE_*
// variables.
func (h *harness) environ(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GEMINUS_HOME=") || strings.HasPrefix(kv, "FAKE_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GEMINUS_HOME="+h.home)
	env = append(env, h.env...)
	return append(env, extra...)
}

func (h *harness) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, geminusBin, args...)
	cmd.Dir = h.dir
	cmd.Env = h.environ()
	// Do not wait past the timeout even if a grandchild still holds the output pipe open.
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// run starts geminus with stdin piped to its standard input. Empty input produces immediate EOF.
func (h *harness) run(stdin string, args ...string) outcome {
	h.t.Helper()
	return h.runEnv(nil, stdin, args...)
}

func (h *harness) runEnv(extraEnv []string, stdin string, args ...string) outcome {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := h.command(ctx, args...)
	cmd.Env = append(cmd.Env, extraEnv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		h.t.Fatalf("geminus %s did not finish within %v\nstdout: %q\nstderr: %q", strings.Join(args, " "), commandTimeout, stdout.String(), stderr.String())
	}
	out := outcome{code: exitStatus(h.t, err), stdout: stdout.String(), stderr: stderr.String()}
	// No command should panic, regardless of its input.
	if strings.Contains(out.stderr, "panic:") || strings.Contains(out.stderr, "goroutine ") {
		h.t.Fatalf("geminus %s panicked:\n%s", strings.Join(args, " "), out.stderr)
	}
	return out
}

func exitStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	t.Fatalf("run geminus: %v", err)
	return -1
}

// ok requires an exit code of zero.
func (h *harness) ok(stdin string, args ...string) outcome {
	h.t.Helper()
	out := h.run(stdin, args...)
	if out.code != 0 {
		h.t.Fatalf("geminus %s: exit %d, want 0\nstdout: %q\nstderr: %q", strings.Join(args, " "), out.code, out.stdout, out.stderr)
	}
	return out
}

// sessionInfo contains the fields these tests inspect in ls and show JSON output.
type sessionInfo struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Agent        string `json:"agent"`
	Status       string `json:"status"`
	Active       bool   `json:"active"`
	PID          int    `json:"pid"`
	ExitCode     *int   `json:"exit_code"`
	Turns        int    `json:"turns"`
	RawBytes     int64  `json:"raw_bytes"`
	Cwd          string `json:"cwd"`
	Dir          string `json:"dir"`
	AgentSession *struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	} `json:"agent_session"`
}

// event represents one event from `log --json`.
type event struct {
	Seq    int64    `json:"seq"`
	Type   string   `json:"type"`
	Agent  string   `json:"agent"`
	Argv   []string `json:"argv"`
	Offset *int64   `json:"offset"`
	Bytes  *int64   `json:"bytes"`
	Text   string   `json:"text"`
	Code   *int     `json:"code"`
}

func decode[T any](t *testing.T, what, data string) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(strings.NewReader(data))
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", what, err, data)
	}
	// --json must emit exactly one JSON value.
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("%s has trailing data after the JSON value:\n%s", what, data)
	}
	return v
}

func (h *harness) list() []sessionInfo {
	h.t.Helper()
	return decode[[]sessionInfo](h.t, "ls --json", h.ok("", "ls", "--json", "--all").stdout)
}

// only verifies that there is exactly one session and returns it.
func (h *harness) only() sessionInfo {
	h.t.Helper()
	sessions := h.list()
	if len(sessions) != 1 {
		h.t.Fatalf("got %d sessions, want 1: %+v", len(sessions), sessions)
	}
	return sessions[0]
}

func (h *harness) show(id string) sessionInfo {
	h.t.Helper()
	return decode[sessionInfo](h.t, "show --json", h.ok("", "show", id, "--json").stdout)
}

func (h *harness) events(id string) []event {
	h.t.Helper()
	return decode[[]event](h.t, "log --json", h.ok("", "log", id, "--json").stdout)
}

func (h *harness) sessionFile(id, name string) string {
	return filepath.Join(h.home, "sessions", id, name)
}

func (h *harness) readFile(path string) string {
	h.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

// needPTY skips tests that run a fake agent through a PTY on non-Unix systems. For Windows ConPTY,
// only compilation is guaranteed (plan.md section 9).
func needPTY(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PTY relay is exercised on Unix only")
	}
}

// agentLine matches a line from the fake agent. Do not anchor it to the start of the line: PTY
// input echo (such as "^D" or part of the input) may appear on the same line before the agent's
// output.
var agentLine = regexp.MustCompile(`(argv:[^\r\n]*|tty:(?:true|false)|fakeagent ready|echo:[^\r\n]*|reply:[^\r\n]*|bye)\r?(?:\n|$)`)

// agentLines extracts the fake agent's lines in order from text with ANSI sequences already
// removed.
func agentLines(text string) []string {
	var lines []string
	for _, match := range agentLine.FindAllStringSubmatch(text, -1) {
		lines = append(lines, match[1])
	}
	return lines
}

// checkOffsets verifies that output events cover raw.log from the beginning without gaps.
func checkOffsets(t *testing.T, events []event, rawLen int) {
	t.Helper()
	var next int64
	outputs := 0
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d, want %d", i, e.Seq, i+1)
		}
		if e.Type != "output" {
			continue
		}
		outputs++
		if e.Offset == nil || e.Bytes == nil {
			t.Fatalf("output event without offset/bytes: %+v", e)
		}
		if *e.Offset != next || *e.Bytes <= 0 {
			t.Errorf("output event seq %d covers [%d,+%d), want it to start at %d", e.Seq, *e.Offset, *e.Bytes, next)
		}
		next = *e.Offset + *e.Bytes
	}
	if next != int64(rawLen) {
		t.Errorf("output events cover %d bytes, raw.log has %d", next, rawLen)
	}
	if rawLen > 0 && outputs == 0 {
		t.Error("raw.log has data but the transcript has no output event")
	}
}

func TestRunRelaysTheAgentAndRecordsIt(t *testing.T) {
	needPTY(t)
	h := newHarness(t)

	out := h.ok("hello\nworld\n", "run", "--agent", "cmd", "--", fakeBin, "one", "two")
	if out.stderr != "" {
		t.Errorf("stderr = %q, want empty", out.stderr)
	}
	// ANSI sequences arrive unchanged; the PTY converts newlines to CRLF.
	if !strings.Contains(out.stdout, "\x1b[1;32mfakeagent\x1b[0m ready\r\n") {
		t.Errorf("stdout = %q, want the banner with its escape sequences", out.stdout)
	}

	session := h.only()
	wantLines := []string{"argv:one two", "tty:true", "fakeagent ready", "echo:hello", "echo:world", "bye"}
	text := h.ok("", "log", session.ID).stdout
	if got := agentLines(text); !reflect.DeepEqual(got, wantLines) {
		t.Errorf("agent output = %q, want %q", got, wantLines)
	}
	if strings.ContainsAny(text, "\x1b\r") {
		t.Errorf("log still has control characters: %q", text)
	}

	// ls, show and log refer to the same session, and the recording matches the bytes sent to the
	// terminal.
	shown := h.show(session.ID)
	if shown.ID != session.ID || shown.Agent != "cmd" || shown.Status != "exited" || shown.Active {
		t.Errorf("show = %+v", shown)
	}
	if shown.ExitCode == nil || *shown.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", shown.ExitCode)
	}
	raw := h.readFile(h.sessionFile(session.ID, "raw.log"))
	if raw != out.stdout {
		t.Errorf("raw.log differs from what the terminal received\nraw:    %q\nstdout: %q", raw, out.stdout)
	}
	if got := h.ok("", "log", session.ID, "--raw").stdout; got != raw {
		t.Errorf("log --raw = %q, want raw.log unchanged", got)
	}
	if shown.RawBytes != int64(len(raw)) || session.RawBytes != shown.RawBytes {
		t.Errorf("raw_bytes: ls %d, show %d, file %d", session.RawBytes, shown.RawBytes, len(raw))
	}
	if shown.Dir != filepath.Dir(h.sessionFile(session.ID, "raw.log")) {
		t.Errorf("dir = %q", shown.Dir)
	}

	events := h.events(session.ID)
	first, last := events[0], events[len(events)-1]
	if first.Type != "session.start" || first.Agent != "cmd" || !reflect.DeepEqual(first.Argv, []string{fakeBin, "one", "two"}) {
		t.Errorf("first event = %+v", first)
	}
	if last.Type != "session.exit" || last.Code == nil || *last.Code != 0 {
		t.Errorf("last event = %+v", last)
	}
	checkOffsets(t, events, len(raw))
	var input string
	for _, e := range events {
		if e.Type == "input" {
			input += e.Text
		}
	}
	if input != "hello\nworld\n" {
		t.Errorf("recorded input = %q, want the piped stdin", input)
	}
}

func TestRunPropagatesTheExitCode(t *testing.T) {
	needPTY(t)
	h := newHarness(t)

	out := h.runEnv([]string{"FAKE_EXIT=7"}, "", "run", "--agent", "cmd", "--", fakeBin)
	if out.code != 7 {
		t.Fatalf("exit = %d, want 7 (stderr: %q)", out.code, out.stderr)
	}
	if got := h.only(); got.ExitCode == nil || *got.ExitCode != 7 || got.Status != "exited" {
		t.Errorf("session = %+v, want exit_code 7", got)
	}

	if out := h.run("", "run", "--agent", "cmd", "--", "sh", "-c", "exit 42"); out.code != 42 {
		t.Errorf("sh -c 'exit 42': exit = %d", out.code)
	}
}

// Piped input must arrive intact and run must exit, even for empty input, large input or input
// without a final newline.
func TestRunWithNonTTYInput(t *testing.T) {
	needPTY(t)
	var many strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&many, "line %04d of the piped input\n", i)
	}
	tests := []struct {
		name  string
		stdin string
		// want is the newline-terminated line the agent should receive.
		want string
	}{
		{"no trailing newline", "abc", "abc\n"},
		{"empty", "", ""},
		{"blank line", "\n", "\n"},
		{"several lines without a final newline", "a\nb\nc", "a\nb\nc\n"},
		{"many lines", many.String(), many.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			received := filepath.Join(t.TempDir(), "received")
			out := h.runEnv([]string{"FAKE_RECORD=" + received}, tt.stdin, "run", "--agent", "cmd", "--", fakeBin)
			if out.code != 0 {
				t.Fatalf("exit %d (stderr: %q)", out.code, out.stderr)
			}
			if got := h.readFile(received); got != tt.want {
				t.Errorf("the agent received %d bytes, want %d\ngot:  %.200q\nwant: %.200q", len(got), len(tt.want), got, tt.want)
			}
			session := h.only()
			raw := h.readFile(h.sessionFile(session.ID, "raw.log"))
			if raw != out.stdout {
				t.Errorf("raw.log (%d bytes) differs from stdout (%d bytes)", len(raw), len(out.stdout))
			}
			checkOffsets(t, h.events(session.ID), len(raw))
		})
	}
}

func TestRunNoRecord(t *testing.T) {
	needPTY(t)
	h := newHarness(t)
	out := h.ok("secret\n", "run", "--agent", "cmd", "--no-record", "--", fakeBin)
	if !strings.Contains(out.stdout, "echo:secret") {
		t.Errorf("stdout = %q, want the relay to work without recording", out.stdout)
	}
	session := h.only()
	if _, err := os.Stat(h.sessionFile(session.ID, "raw.log")); !os.IsNotExist(err) {
		t.Errorf("raw.log exists with --no-record (stat error: %v)", err)
	}
	var types []string
	for _, e := range h.events(session.ID) {
		types = append(types, e.Type)
	}
	if !reflect.DeepEqual(types, []string{"session.start", "session.exit"}) {
		t.Errorf("events = %v, want only start and exit", types)
	}
	if strings.Contains(h.readFile(h.sessionFile(session.ID, "transcript.jsonl")), "secret") {
		t.Error("the transcript holds the input despite --no-record")
	}
	if out := h.run("", "log", session.ID); out.code != 1 {
		t.Errorf("log on an unrecorded session: exit %d, want 1", out.code)
	}
}

func TestStateIsPrivate(t *testing.T) {
	needPTY(t)
	h := newHarness(t)
	h.ok("x\n", "run", "--agent", "cmd", "--", fakeBin)
	id := h.only().ID
	for path, want := range map[string]os.FileMode{
		h.home:                                0o700,
		filepath.Join(h.home, "sessions"):     0o700,
		h.sessionFile(id, ""):                 0o700,
		h.sessionFile(id, "session.json"):     0o600,
		h.sessionFile(id, "transcript.jsonl"): 0o600,
		h.sessionFile(id, "raw.log"):          0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat %s: %v", path, err)
			continue
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %04o, want %04o", path, got, want)
		}
	}
}

func TestResumeAppendsToTheSameSession(t *testing.T) {
	needPTY(t)
	h := newHarness(t).withAgents("claude")

	first := h.ok("", "run", "first", "prompt")
	session := h.only()
	if session.AgentSession == nil || session.AgentSession.Kind != "uuid" {
		t.Fatalf("agent_session = %+v, want a uuid", session.AgentSession)
	}
	uuid := session.AgentSession.Value
	if got := agentLines(first.stdout)[0]; got != "argv:--session-id "+uuid+" first prompt" {
		t.Errorf("first argv line = %q", got)
	}

	// Resuming from another directory continues the same conversation in the directory where the
	// session was created.
	elsewhere := *h
	elsewhere.dir = t.TempDir()
	resumed := elsewhere.ok("", "run", "--resume", session.ID)
	if got := agentLines(resumed.stdout)[0]; got != "argv:--resume "+uuid {
		t.Errorf("resumed argv line = %q", got)
	}

	fresh := h.ok("", "run", "--resume", session.ID, "--fresh")
	after := h.only()
	if after.ID != session.ID || after.AgentSession.Value == uuid {
		t.Errorf("--fresh: session %s key %s, want the same session with a new key", after.ID, after.AgentSession.Value)
	}
	if got := agentLines(fresh.stdout)[0]; got != "argv:--session-id "+after.AgentSession.Value {
		t.Errorf("fresh argv line = %q", got)
	}

	raw := h.readFile(h.sessionFile(session.ID, "raw.log"))
	if raw != first.stdout+resumed.stdout+fresh.stdout {
		t.Errorf("raw.log is not the three runs appended:\n%q", raw)
	}
	events := h.events(session.ID)
	checkOffsets(t, events, len(raw))
	starts := 0
	for _, e := range events {
		if e.Type == "session.start" {
			starts++
		}
	}
	if starts != 3 {
		t.Errorf("got %d session.start events, want 3", starts)
	}
}

func TestAskRunsOneExchangeThroughAPipe(t *testing.T) {
	h := newHarness(t).withAgents("claude", "hermes")

	first := h.ok("", "ask", "what is this?")
	session := h.only()
	uuid := session.AgentSession.Value
	want := "argv:-p --session-id " + uuid + " what is this?\ntty:false\nreply:what is this?\n"
	if first.stdout != want {
		t.Errorf("stdout = %q, want %q", first.stdout, want)
	}
	if session.Turns != 1 || session.Status != "exited" || session.Agent != "claude" {
		t.Errorf("session = %+v", session)
	}

	second := h.ok("", "ask", "--continue", "and this?")
	if !strings.HasPrefix(second.stdout, "argv:-p --resume "+uuid+" and this?\n") {
		t.Errorf("--continue stdout = %q, want the same agent session resumed", second.stdout)
	}
	third := h.runEnv([]string{"FAKE_EXIT=3"}, "", "ask", "--session", session.ID, "--json", "third")
	if third.code != 3 {
		t.Fatalf("ask --json: exit %d, want the agent's 3 (stderr: %q)", third.code, third.stderr)
	}
	result := decode[struct {
		Session  string `json:"session"`
		Agent    string `json:"agent"`
		ExitCode int    `json:"exit_code"`
		Output   string `json:"output"`
	}](t, "ask --json", third.stdout)
	if result.Session != session.ID || result.ExitCode != 3 || !strings.HasSuffix(result.Output, "reply:third\n") {
		t.Errorf("ask --json = %+v", result)
	}

	raw := h.readFile(h.sessionFile(session.ID, "raw.log"))
	if raw != first.stdout+second.stdout+result.Output {
		t.Errorf("raw.log is not the three replies appended:\n%q", raw)
	}
	events := h.events(session.ID)
	checkOffsets(t, events, len(raw))
	var prompts []string
	for _, e := range events {
		if e.Type == "input" {
			prompts = append(prompts, e.Text)
		}
	}
	if !reflect.DeepEqual(prompts, []string{"what is this?", "and this?", "third"}) {
		t.Errorf("recorded prompts = %q", prompts)
	}
	if got := h.show(session.ID); got.Turns != 3 || got.ExitCode == nil || *got.ExitCode != 3 {
		t.Errorf("after three asks: %+v", got)
	}

	hermes := h.ok("", "ask", "--agent", "hermes", "hi")
	if !strings.Contains(hermes.stdout, "--create-if-missing -q hi --oneshot\n") || !strings.Contains(hermes.stdout, "reply:hi\n") {
		t.Errorf("hermes ask stdout = %q", hermes.stdout)
	}
}

func TestBrokenSessionFilesDoNotBreakTheCommands(t *testing.T) {
	h := newHarness(t)
	sessions := filepath.Join(h.home, "sessions")
	write := func(id, name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(sessions, id), 0o700); err != nil {
			t.Fatal(err)
		}
		if name == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(sessions, id, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const good = "20260101-000000-0001"
	write(good, "session.json", `{"version":1,"id":"`+good+`","title":"good","agent":"cmd","cwd":"/tmp",
		"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","status":"exited","exit_code":0,"turns":0,"raw_bytes":0}`)
	write(good, "transcript.jsonl", `{"seq":1,"ts":"2026-01-01T00:00:00Z","type":"session.start","agent":"cmd","argv":["x"]}`+"\n"+`{"seq":2,"ts":"2026-01-01T`)
	write(good, "raw.log", "kept\r\n")

	broken := map[string]string{
		"20260101-000000-0002": "{not json",
		"20260101-000000-0003": "",
		"20260101-000000-0004": `{"version":1,"id":"20990101-000000-ffff","agent":"cmd"}`,
		"20260101-000000-0005": `{"version":99,"id":"20260101-000000-0005"}`,
		"20260101-000000-0006": `[1,2,3]`,
		"20260101-000000-0007": "\x00\xff\xfe binary",
	}
	for id, content := range broken {
		write(id, "session.json", content)
	}
	const empty = "20260101-000000-0008"
	write(empty, "", "")
	broken[empty] = ""

	for _, args := range [][]string{{"ls"}, {"ls", "--json"}, {"ls", "--all"}} {
		out := h.run("", args...)
		if out.code != 1 {
			t.Errorf("%v: exit %d, want 1 while sessions are unreadable", args, out.code)
		}
		if !strings.Contains(out.stdout, good) {
			t.Errorf("%v: stdout %q does not list the readable session", args, out.stdout)
		}
		for id := range broken {
			if !strings.Contains(out.stderr, id) {
				t.Errorf("%v: stderr does not name the unreadable session %s", args, id)
			}
			if strings.Contains(out.stdout, id) {
				t.Errorf("%v: stdout lists the unreadable session %s", args, id)
			}
		}
	}
	if listed := decode[[]sessionInfo](t, "ls --json", h.run("", "ls", "--json").stdout); len(listed) != 1 || listed[0].ID != good {
		t.Errorf("ls --json = %+v, want only the readable session", listed)
	}

	// Even with a truncated transcript tail, the readable events and raw.log remain available.
	torn := h.run("", "log", good, "--json")
	if events := decode[[]event](t, "log --json", torn.stdout); torn.code != 1 || len(events) != 1 {
		t.Errorf("log --json with a torn transcript: exit %d, %d events; want 1 and the readable event", torn.code, len(events))
	}
	if out := h.ok("", "log", good); out.stdout != "kept\n" {
		t.Errorf("log = %q", out.stdout)
	}

	for id := range broken {
		for _, args := range [][]string{{"show", id}, {"show", id, "--json"}, {"log", id}, {"run", "--resume", id, "--agent", "cmd", "--", "true"}, {"ask", "--session", id, "hi"}} {
			if out := h.run("", args...); out.code != 1 || out.stdout != "" {
				t.Errorf("%v: exit %d, stdout %q; want exit 1 and nothing on stdout", args, out.code, out.stdout)
			}
		}
		h.ok("", "rm", id)
	}
	if out := h.ok("", "ls"); !strings.Contains(out.stdout, good) || out.stderr != "" {
		t.Errorf("after removing the broken sessions: stdout %q, stderr %q", out.stdout, out.stderr)
	}
}

func TestRm(t *testing.T) {
	needPTY(t)
	h := newHarness(t)
	h.ok("", "run", "--agent", "cmd", "--", fakeBin)
	id := h.only().ID

	h.ok("", "rm", id)
	if _, err := os.Stat(h.sessionFile(id, "")); !os.IsNotExist(err) {
		t.Errorf("the session directory survived rm (stat error: %v)", err)
	}
	if out := h.run("", "rm", id); out.code != 1 {
		t.Errorf("rm of a missing session: exit %d, want 1", out.code)
	}
	h.ok("", "rm", id, "--force")
}

func TestAgentsDoctorAndUsage(t *testing.T) {
	h := newHarness(t).withAgents("claude")

	agents := decode[[]struct {
		Name         string `json:"name"`
		Path         string `json:"path"`
		Found        bool   `json:"found"`
		Version      string `json:"version"`
		Capabilities struct {
			Resume     bool   `json:"resume"`
			Oneshot    bool   `json:"oneshot"`
			SessionKey string `json:"session_key"`
		} `json:"capabilities"`
	}](t, "agents --json", h.ok("", "agents", "--json").stdout)
	names := map[string]bool{}
	for _, a := range agents {
		names[a.Name] = true
		if a.Name != "claude" {
			continue
		}
		if !a.Found || a.Version != "fakeagent 0.0.1" || filepath.Base(a.Path) != exeName("claude") {
			t.Errorf("claude = %+v", a)
		}
		if !a.Capabilities.Resume || !a.Capabilities.Oneshot || a.Capabilities.SessionKey != "uuid" {
			t.Errorf("claude capabilities = %+v", a.Capabilities)
		}
	}
	if !names["claude"] || !names["hermes"] || !names["cmd"] {
		t.Errorf("agents = %+v, want claude, hermes and cmd", agents)
	}

	doctor := h.ok("", "doctor")
	for _, want := range []string{"pty", "terminal", "home", "agent claude"} {
		if !strings.Contains(doctor.stdout, want) {
			t.Errorf("doctor does not mention %q:\n%s", want, doctor.stdout)
		}
	}
	if _, err := os.Stat(h.home); !os.IsNotExist(err) {
		t.Errorf("agents / doctor created the state directory (stat error: %v)", err)
	}

	if out := h.ok("", "version"); !strings.HasPrefix(out.stdout, "geminus ") {
		t.Errorf("version = %q", out.stdout)
	}
	for _, args := range [][]string{{}, {"bogus"}, {"ls", "--nope"}, {"show"}, {"run", "--fresh"}, {"ask"}, {"run", "--agent", "cmd"}} {
		if out := h.run("", args...); out.code != 2 {
			t.Errorf("geminus %v: exit %d, want 2 (stderr: %q)", args, out.code, out.stderr)
		}
	}
	if out := h.run("", "run", "--agent", "cmd", "--", filepath.Join(h.dir, "no-such-command")); out.code != 1 {
		t.Errorf("run of a missing command: exit %d, want 1", out.code)
	}
}

// --home takes precedence over GEMINUS_HOME.
func TestHomeFlagOverridesTheEnvironment(t *testing.T) {
	needPTY(t)
	h := newHarness(t)
	other := filepath.Join(t.TempDir(), "other-home")
	h.ok("", "run", "--home", other, "--agent", "cmd", "--", fakeBin)
	if sessions := h.list(); len(sessions) != 0 {
		t.Errorf("GEMINUS_HOME holds %d session(s); --home should have been used", len(sessions))
	}
	listed := decode[[]sessionInfo](t, "ls --json", h.ok("", "ls", "--json", "--home", other).stdout)
	if len(listed) != 1 {
		t.Errorf("--home holds %d session(s), want 1", len(listed))
	}
}
