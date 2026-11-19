//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/agent"
	"github.com/hauntedfail/geminus/internal/session"
)

func TestRunCmdRelaysOutputAndRecordsIt(t *testing.T) {
	s := newSandbox(t)
	r := s.ok("", "run", "--agent", "cmd", "--", "sh", "-c", `printf 'hello \033[32mworld\033[0m'`)
	if !strings.Contains(r.stdout, "hello \x1b[32mworld\x1b[0m") {
		t.Errorf("stdout = %q, want the agent's bytes unchanged", r.stdout)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty for a non-interactive run", r.stderr)
	}

	view := s.only()
	if view.Agent != "cmd" || view.Status != session.StatusExited || view.Active {
		t.Errorf("session = %+v", view)
	}
	if view.ExitCode == nil || *view.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", view.ExitCode)
	}
	if view.PID != 0 {
		t.Errorf("pid = %d, want it cleared after exit", view.PID)
	}
	if view.AgentSession != nil {
		t.Errorf("agent_session = %+v, want none for cmd", view.AgentSession)
	}
	if view.Cwd != mustGetwd(t) {
		t.Errorf("cwd = %q, want %q", view.Cwd, mustGetwd(t))
	}

	raw := s.rawLog(view.ID)
	if raw != r.stdout {
		t.Errorf("raw.log = %q, want the same bytes as stdout %q", raw, r.stdout)
	}
	if view.RawBytes != int64(len(raw)) {
		t.Errorf("raw_bytes = %d, want %d", view.RawBytes, len(raw))
	}

	events := s.events(view.ID)
	types := eventTypes(events)
	if types[0] != session.EventStart || types[len(types)-1] != session.EventExit {
		t.Errorf("events = %v, want session.start first and session.exit last", types)
	}
	if want := []string{"sh", "-c", `printf 'hello \033[32mworld\033[0m'`}; !reflect.DeepEqual(events[0].Argv, want) {
		t.Errorf("start argv = %q, want %q", events[0].Argv, want)
	}
	checkOffsets(t, events, len(raw))
}

func TestRunReturnsTheChildExitCode(t *testing.T) {
	for _, want := range []int{1, 2, 7, 255} {
		s := newSandbox(t)
		r := s.run("", "run", "--agent", "cmd", "--", "sh", "-c", "exit "+strconv.Itoa(want))
		if r.code != want {
			t.Errorf("exit = %d, want %d (stderr: %q)", r.code, want, r.stderr)
		}
		view := s.only()
		if view.ExitCode == nil || *view.ExitCode != want {
			t.Errorf("recorded exit_code = %v, want %d", view.ExitCode, want)
		}
		exit, ok := findEvent(s.events(view.ID), session.EventExit)
		if !ok || exit.Code == nil || *exit.Code != want {
			t.Errorf("session.exit event = %+v, want code %d", exit, want)
		}
	}
}

func TestRunRecordsPipedInput(t *testing.T) {
	for _, tt := range []struct {
		name  string
		stdin string
	}{
		{"with trailing newline", "hello\n"},
		{"without trailing newline", "hello"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSandbox(t)
			r := s.ok(tt.stdin, "run", "--agent", "cmd", "--", "cat")
			if !strings.Contains(r.stdout, "hello") {
				t.Errorf("stdout = %q", r.stdout)
			}
			view := s.only()
			input, ok := findEvent(s.events(view.ID), session.EventInput)
			if !ok || input.Text != tt.stdin {
				t.Errorf("input event = %+v, want text %q", input, tt.stdin)
			}
		})
	}
}

func TestRunNoRecordKeepsOnlyMetaAndLifecycleEvents(t *testing.T) {
	s := newSandbox(t)
	r := s.run("secret\n", "run", "--agent", "cmd", "--no-record", "--", "sh", "-c", "cat; exit 4")
	if r.code != 4 {
		t.Fatalf("exit = %d, want 4 (stderr: %q)", r.code, r.stderr)
	}
	view := s.only()
	if view.ExitCode == nil || *view.ExitCode != 4 || view.RawBytes != 0 {
		t.Errorf("session = %+v", view)
	}
	if got := eventTypes(s.events(view.ID)); !reflect.DeepEqual(got, []string{session.EventStart, session.EventExit}) {
		t.Errorf("events = %v, want only start and exit", got)
	}
	paths, err := s.store().Paths(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.Raw); !os.IsNotExist(err) {
		t.Errorf("raw.log exists with --no-record (stat error: %v)", err)
	}
	if r := s.run("", "log", view.ID); r.code != ExitError || !strings.Contains(r.stderr, "--no-record") {
		t.Errorf("log on an unrecorded session: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestRunTitle(t *testing.T) {
	s := newSandbox(t)
	s.ok("", "run", "--agent", "cmd", "--title", "my title", "--", "true")
	if got := s.only().Title; got != "my title" {
		t.Errorf("title = %q, want %q", got, "my title")
	}

	s = newSandbox(t)
	s.ok("", "run", "--agent", "cmd", "--", "sh", "-c", "true", "--token", "hunter2")
	if got := s.only().Title; strings.Contains(got, "hunter2") || !strings.Contains(got, agent.Redacted) {
		t.Errorf("default title = %q, want the secret redacted", got)
	}
}

func TestRunRedactsSecretsInTheTranscript(t *testing.T) {
	s := newSandbox(t)
	s.ok("", "run", "--agent", "cmd", "--", "sh", "-c", "true", "--api-key", "hunter2")
	start, _ := findEvent(s.events(s.only().ID), session.EventStart)
	if got := strings.Join(start.Argv, " "); strings.Contains(got, "hunter2") || !strings.Contains(got, agent.Redacted) {
		t.Errorf("recorded argv = %q, want the secret redacted", got)
	}
}

// Read session.json and transcript.jsonl directly to verify that neither contains secrets
// embedded in a single argument, such as quoted assignments or headers.
func TestRunKeepsEmbeddedSecretsOutOfTheSessionFiles(t *testing.T) {
	scripts := map[string]string{
		"double quoted assignment": `: API_KEY="s3cr3t"`,
		"single quoted assignment": `: TOKEN='s3cr3t'`,
		"leading assignment":       `API_KEY="s3cr3t" true`,
		"api key header":           `: curl -H 'X-Api-Key: s3cr3t' https://example.com`,
		"authorization header":     `: curl -H "Authorization: s3cr3t" https://example.com`,
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			s.ok("", "run", "--agent", "cmd", "--", "sh", "-c", script)
			view := s.only()
			dir := filepath.Join(s.home, sessionsDirName, view.ID)
			for _, file := range []string{"session.json", "transcript.jsonl"} {
				data, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "s3cr3t") {
					t.Errorf("%s holds the secret:\n%s", file, data)
				}
				if !strings.Contains(string(data), agent.Redacted) {
					t.Errorf("%s has no %s marker:\n%s", file, agent.Redacted, data)
				}
			}
		})
	}
}

func TestRunUsageErrors(t *testing.T) {
	fakeBin(t, "claude")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"fresh without resume", []string{"run", "--fresh", "--agent", "cmd", "--", "true"}, "--fresh requires --resume"},
		{"cmd without a command", []string{"run", "--agent", "cmd"}, "needs a command"},
		{"unknown agent", []string{"run", "--agent", "bogus", "hi"}, `unknown agent "bogus"`},
		{"prompt that looks like a flag", []string{"run", "--", "--dangerously-skip-permissions"}, `must not start with "-"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSandbox(t)
			r := s.run("", tt.args...)
			if r.code != ExitUsage {
				t.Fatalf("exit = %d, want %d (stderr: %q)", r.code, ExitUsage, r.stderr)
			}
			if !strings.Contains(r.stderr, tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", r.stderr, tt.want)
			}
			if views := s.sessions(); len(views) != 0 {
				t.Errorf("a failed start left %d session(s) behind", len(views))
			}
		})
	}
}

func TestRunFailsBeforeCreatingASessionWhenTheAgentIsMissing(t *testing.T) {
	emptyPath(t)
	s := newSandbox(t)
	r := s.run("", "run", "hello")
	if r.code != ExitError {
		t.Fatalf("exit = %d, want %d", r.code, ExitError)
	}
	if !strings.Contains(r.stderr, `"claude"`) || !strings.Contains(r.stderr, "geminus doctor") {
		t.Errorf("stderr = %q, want the missing binary and a pointer to doctor", r.stderr)
	}
	if _, err := os.Stat(s.home); !os.IsNotExist(err) {
		t.Errorf("state directory was created for a run that never started (stat error: %v)", err)
	}
}

func TestRunRollsBackWhenTheCommandIsMissing(t *testing.T) {
	s := newSandbox(t)
	r := s.run("", "run", "--agent", "cmd", "--", filepath.Join(t.TempDir(), "no-such-binary"))
	if r.code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr: %q)", r.code, ExitError, r.stderr)
	}
	if views := s.sessions(); len(views) != 0 {
		t.Errorf("a failed start left %d session(s) behind", len(views))
	}
}

func TestRunClaudeKeepsOneAgentSessionAcrossResume(t *testing.T) {
	fakeBin(t, "claude")
	s := newSandbox(t)

	first := s.ok("", "run", "fix", "the", "bug")
	view := s.only()
	if view.Agent != "claude" || view.Title != "fix the bug" {
		t.Errorf("session = %+v", view)
	}
	if view.AgentSession == nil || view.AgentSession.Kind != "uuid" || !agent.IsUUID(view.AgentSession.Value) {
		t.Fatalf("agent_session = %+v, want a uuid", view.AgentSession)
	}
	uuid := view.AgentSession.Value
	if got, want := argvLine(t, first.stdout), "--session-id "+uuid+" fix the bug"; got != want {
		t.Errorf("first run argv = %q, want %q", got, want)
	}

	resumed := s.ok("", "run", "--resume", view.ID)
	if got, want := argvLine(t, resumed.stdout), "--resume "+uuid; got != want {
		t.Errorf("resumed argv = %q, want %q", got, want)
	}
	if got := s.only().AgentSession.Value; got != uuid {
		t.Errorf("uuid changed across resume: %q -> %q", uuid, got)
	}

	fresh := s.ok("", "run", "--resume", view.ID, "--fresh", "--title", "second try")
	after := s.only()
	if after.ID != view.ID {
		t.Fatalf("--fresh changed the geminus session: %s -> %s", view.ID, after.ID)
	}
	if after.AgentSession.Value == uuid || !agent.IsUUID(after.AgentSession.Value) {
		t.Errorf("--fresh kept the agent session %q", after.AgentSession.Value)
	}
	if got, want := argvLine(t, fresh.stdout), "--session-id "+after.AgentSession.Value; got != want {
		t.Errorf("fresh argv = %q, want %q", got, want)
	}
	if after.Title != "second try" {
		t.Errorf("title = %q, want --title to rename a resumed session", after.Title)
	}
	if !strings.Contains(fresh.stderr, "--fresh") {
		t.Errorf("stderr = %q, want a note about the fresh agent session", fresh.stderr)
	}

	events := s.events(view.ID)
	starts := 0
	for i, event := range events {
		if event.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d; seq must continue across resume", i, event.Seq)
		}
		if event.Type == session.EventStart {
			starts++
		}
	}
	if starts != 3 {
		t.Errorf("got %d session.start events, want 3", starts)
	}
	raw := s.rawLog(view.ID)
	if want := first.stdout + resumed.stdout + fresh.stdout; raw != want {
		t.Errorf("raw.log = %q, want the three runs appended %q", raw, want)
	}
	checkOffsets(t, events, len(raw))
}

func TestRunHermesUsesTheSessionIDAsItsName(t *testing.T) {
	fakeBin(t, "hermes")
	s := newSandbox(t)

	first := s.ok("", "run", "--agent", "hermes", "hello")
	view := s.only()
	if view.AgentSession == nil || view.AgentSession.Kind != "name" || view.AgentSession.Value != view.ID {
		t.Fatalf("agent_session = %+v, want name %s", view.AgentSession, view.ID)
	}
	if got, want := argvLine(t, first.stdout), "chat -c "+view.ID+" --create-if-missing -q hello"; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}

	resumed := s.ok("", "run", "--resume", view.ID)
	if got, want := argvLine(t, resumed.stdout), "chat -c "+view.ID+" --create-if-missing"; got != want {
		t.Errorf("resumed argv = %q, want %q", got, want)
	}

	s.ok("", "run", "--resume", view.ID, "--fresh")
	name := s.only().AgentSession.Value
	if name == view.ID || !strings.HasPrefix(name, view.ID+"-") {
		t.Errorf("--fresh name = %q, want %s-<suffix>", name, view.ID)
	}
}

func TestRunResumeOfCmdStartsAnewWithANote(t *testing.T) {
	s := newSandbox(t)
	first := s.ok("", "run", "--agent", "cmd", "--", "sh", "-c", "printf one")
	id := s.only().ID

	second := s.ok("", "run", "--resume", id, "--", "sh", "-c", "printf two")
	if !strings.Contains(second.stderr, "resume unsupported") {
		t.Errorf("stderr = %q, want a warning that cmd cannot resume", second.stderr)
	}
	note, ok := findEvent(s.events(id), session.EventNote)
	if !ok || !strings.Contains(note.Text, "resume unsupported") {
		t.Errorf("note event = %+v", note)
	}
	raw := s.rawLog(id)
	if raw != first.stdout+second.stdout {
		t.Errorf("raw.log = %q, want both runs appended", raw)
	}
	checkOffsets(t, s.events(id), len(raw))
}

func TestRunResumeErrors(t *testing.T) {
	fakeBin(t, "claude")
	s := newSandbox(t)
	s.ok("", "run", "--agent", "cmd", "--", "true")
	id := s.only().ID

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown id", []string{"run", "--resume", "20200101-000000-abcd"}, "session not found"},
		{"malformed id", []string{"run", "--resume", "../etc"}, "invalid session id"},
		{"agent mismatch", []string{"run", "--resume", id, "--agent", "claude"}, `was started with agent "cmd"`},
		{"fresh on cmd", []string{"run", "--resume", id, "--fresh", "--", "true"}, "--fresh does not apply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := s.run("", tt.args...)
			if r.code != ExitError {
				t.Fatalf("exit = %d, want %d (stderr: %q)", r.code, ExitError, r.stderr)
			}
			if !strings.Contains(r.stderr, tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", r.stderr, tt.want)
			}
		})
	}
	if got := s.only(); got.Status != session.StatusExited || got.PID != 0 {
		t.Errorf("failed resumes changed the session: %+v", got)
	}
}

func TestRunRefusesASessionThatIsAlreadyOpen(t *testing.T) {
	s := newSandbox(t)
	// Use the test process's PID to simulate a session held by a live process.
	meta := s.seed("cmd", func(m session.Meta) session.Meta {
		m.Status = session.StatusRunning
		m.PID = os.Getpid()
		return m
	})

	r := s.run("", "run", "--resume", meta.ID, "--", "true")
	if r.code != ExitError || !strings.Contains(r.stderr, "already open") || !strings.Contains(r.stderr, "--force") {
		t.Fatalf("exit = %d, stderr = %q; want a refusal that mentions --force", r.code, r.stderr)
	}
	if got := s.meta(meta.ID); got.Status != session.StatusRunning || got.PID != os.Getpid() {
		t.Errorf("a refused start changed the session: %+v", got)
	}

	s.ok("", "run", "--resume", meta.ID, "--force", "--", "true")
	if got := s.meta(meta.ID); got.Status != session.StatusExited || got.PID != 0 {
		t.Errorf("after --force: %+v, want exited", got)
	}
}

// A session can be resumed after a crash if the recorded PID no longer exists.
func TestRunOpensAStaleSession(t *testing.T) {
	s := newSandbox(t)
	meta := s.seed("cmd", func(m session.Meta) session.Meta {
		m.Status = session.StatusRunning
		m.PID = 0x7ffffffe // Nonexistent PID
		return m
	})
	s.ok("", "run", "--resume", meta.ID, "--", "true")
}

func TestRunResumeUsesTheSessionDirectory(t *testing.T) {
	s := newSandbox(t)
	origin := t.TempDir()
	chdir(t, origin)
	s.ok("", "run", "--agent", "cmd", "--", "true")
	id := s.only().ID

	chdir(t, t.TempDir())
	r := s.ok("", "run", "--resume", id, "--", "sh", "-c", "pwd -P")
	resolved, err := filepath.EvalSymlinks(origin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.stdout, resolved) {
		t.Errorf("resumed in %q, want the session's directory %q", r.stdout, resolved)
	}

}

func TestRunResumeFailsWhenTheSessionDirectoryIsGone(t *testing.T) {
	s := newSandbox(t)
	missing := filepath.Join(t.TempDir(), "removed")
	meta := s.seed("cmd", func(m session.Meta) session.Meta {
		m.Status = session.StatusExited
		m.Cwd = missing
		return m
	})
	gone := s.run("", "run", "--resume", meta.ID, "--", "true")
	if gone.code != ExitError || !strings.Contains(gone.stderr, "no longer available") {
		t.Errorf("exit = %d, stderr = %q; want an error about the missing directory", gone.code, gone.stderr)
	}
	if got := s.meta(meta.ID); got.Status != session.StatusExited || got.PID != 0 {
		t.Errorf("a failed resume changed the session: %+v", got)
	}
}

func TestRunUsesTheConfiguredDefaultAgent(t *testing.T) {
	s := newSandbox(t)
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(s.home, configFileName)
	if err := os.WriteFile(config, []byte(`{"agent":"cmd"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := s.run("", "run", "--", "sh", "-c", "exit 3"); r.code != 3 {
		t.Fatalf("exit = %d, want 3 (stderr: %q)", r.code, r.stderr)
	}
	if got := s.only().Agent; got != "cmd" {
		t.Errorf("agent = %q, want the configured default cmd", got)
	}

	if err := os.WriteFile(config, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := s.run("", "run", "--", "true")
	if r.code != ExitError || !strings.Contains(r.stderr, configFileName) {
		t.Errorf("exit = %d, stderr = %q; want a broken config to be reported", r.code, r.stderr)
	}
}

func TestRunVerbosePrintsTheSessionID(t *testing.T) {
	s := newSandbox(t)
	r := s.ok("", "-v", "run", "--agent", "cmd", "--", "true")
	if id := s.only().ID; !strings.Contains(r.stderr, id) {
		t.Errorf("stderr = %q, want the session id %s", r.stderr, id)
	}
}

// brokenProgram creates an executable file that fails to launch with an exec format error.
// PATH lookup succeeds, so the test exercises the child launch failure itself.
func brokenProgram(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("not a program\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunDeletesANewSessionWhenTheChildCannotStart(t *testing.T) {
	s := newSandbox(t)
	r := s.run("", "run", "--agent", "cmd", "--", brokenProgram(t, t.TempDir(), "broken"))
	if r.code != ExitError || !strings.Contains(r.stderr, "cannot start the command") {
		t.Fatalf("exit = %d, stderr = %q; want the start failure", r.code, r.stderr)
	}
	if views := s.sessions(); len(views) != 0 {
		t.Errorf("a run that never started left a session behind: %+v", views)
	}
}

func TestRunRestoresAnExistingSessionWhenTheChildCannotStart(t *testing.T) {
	s := newSandbox(t)
	if r := s.run("", "run", "--agent", "cmd", "--title", "kept", "--", "sh", "-c", "printf first; exit 3"); r.code != 3 {
		t.Fatalf("first run: exit %d (stderr: %q)", r.code, r.stderr)
	}
	before := s.only()

	r := s.run("", "run", "--resume", before.ID, "--title", "renamed", "--", brokenProgram(t, t.TempDir(), "broken"))
	if r.code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr: %q)", r.code, ExitError, r.stderr)
	}
	after := s.only()
	if after.Status != session.StatusExited || after.PID != 0 || after.Title != "kept" || after.RawBytes != before.RawBytes {
		t.Errorf("session after the failed resume = %+v, want it as it was", after)
	}
	if after.ExitCode == nil || *after.ExitCode != 3 {
		t.Errorf("exit_code = %v, want the previous run's 3 (not a recorded exit 0)", after.ExitCode)
	}
	// The transcript must record the failed launch attempt.
	events := s.events(before.ID)
	last := events[len(events)-1]
	if last.Type != session.EventNote || !strings.Contains(last.Text, "did not start") {
		t.Errorf("last event = %+v, want a note that the agent did not start", last)
	}
	exits := 0
	for _, event := range events {
		if event.Type == session.EventExit {
			exits++
		}
	}
	if exits != 1 {
		t.Errorf("got %d session.exit events, want only the first run's", exits)
	}
}

func TestRunNoRecordKeepsArgumentsOutOfTheSession(t *testing.T) {
	fakeBin(t, "claude")
	s := newSandbox(t)
	s.ok("", "run", "--no-record", "my", "secret", "prompt")
	view := s.only()
	if view.Title != "" {
		t.Errorf("title = %q, want none with --no-record", view.Title)
	}
	start, _ := findEvent(s.events(view.ID), session.EventStart)
	if want := []string{"claude", agent.Redacted}; !reflect.DeepEqual(start.Argv, want) {
		t.Errorf("recorded argv = %q, want %q", start.Argv, want)
	}
	paths, err := s.store().Paths(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{paths.Meta, paths.Transcript} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "secret") {
			t.Errorf("%s holds the prompt despite --no-record:\n%s", filepath.Base(file), data)
		}
	}

	s.ok("", "run", "--no-record", "--title", "chosen", "--agent", "cmd", "--", "true")
	if got := s.sessions()[0].Title; got != "chosen" {
		t.Errorf("title = %q, want an explicit --title to be kept", got)
	}
}

func TestRunAnnouncesTheSessionDirectory(t *testing.T) {
	s := newSandbox(t)
	origin := t.TempDir()
	chdir(t, origin)
	s.ok("", "run", "--agent", "cmd", "--", "true")
	id := s.only().ID

	if r := s.ok("", "run", "--resume", id, "--", "true"); strings.Contains(r.stderr, "session's directory") {
		t.Errorf("stderr = %q, want no notice when already in the session's directory", r.stderr)
	}
	chdir(t, t.TempDir())
	if r := s.ok("", "run", "--resume", id, "--", "true"); !strings.Contains(r.stderr, "running the agent in the session's directory") {
		t.Errorf("stderr = %q, want a notice that the agent runs elsewhere", r.stderr)
	}
}

func TestRunRejectsARelativeSessionDirectory(t *testing.T) {
	s := newSandbox(t)
	meta := s.seed("cmd", func(m session.Meta) session.Meta {
		m.Cwd = "relative/dir"
		return m
	})
	r := s.run("", "run", "--resume", meta.ID, "--", "true")
	if r.code != ExitError || !strings.Contains(r.stderr, "not an absolute path") {
		t.Errorf("exit = %d, stderr = %q", r.code, r.stderr)
	}
}
