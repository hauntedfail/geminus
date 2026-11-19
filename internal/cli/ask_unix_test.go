//go:build unix

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/session"
)

func TestAskPrintsTheReplyAndRecordsTheExchange(t *testing.T) {
	fakeBin(t, "claude")
	s := newSandbox(t)

	r := s.ok("", "ask", "what", "is", "this?")
	view := s.only()
	uuid := view.AgentSession.Value
	if want := "argv:-p --session-id " + uuid + " what is this?\n"; r.stdout != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty", r.stderr)
	}
	if view.Agent != "claude" || view.Turns != 1 || view.Status != session.StatusExited || view.Title != "what is this?" {
		t.Errorf("session = %+v", view)
	}

	events := s.events(view.ID)
	want := []string{session.EventStart, session.EventInput, session.EventOutput, session.EventExit}
	if got := eventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if events[1].Text != "what is this?" {
		t.Errorf("input event text = %q", events[1].Text)
	}
	if got := events[0].Argv; got[0] != "claude" || got[1] != "-p" {
		t.Errorf("start argv = %q, want the agent's name, not its resolved path", got)
	}
	raw := s.rawLog(view.ID)
	if raw != r.stdout {
		t.Errorf("raw.log = %q, want the reply %q", raw, r.stdout)
	}
	checkOffsets(t, events, len(raw))
}

func TestAskSessionAndContinueResumeTheSameConversation(t *testing.T) {
	fakeBin(t, "claude")
	s := newSandbox(t)
	s.ok("", "ask", "first")
	view := s.only()
	uuid := view.AgentSession.Value

	second := s.ok("", "ask", "--session", view.ID, "second")
	if want := "argv:-p --resume " + uuid + " second\n"; second.stdout != want {
		t.Errorf("--session stdout = %q, want %q", second.stdout, want)
	}
	third := s.ok("", "ask", "--continue", "third")
	if want := "argv:-p --resume " + uuid + " third\n"; third.stdout != want {
		t.Errorf("--continue stdout = %q, want %q", third.stdout, want)
	}
	after := s.only()
	if after.Turns != 3 || after.AgentSession.Value != uuid {
		t.Errorf("session = %+v, want 3 turns on the same agent session", after)
	}
	if raw := s.rawLog(view.ID); !strings.HasSuffix(raw, second.stdout+third.stdout) {
		t.Errorf("raw.log = %q, want every reply appended", raw)
	}
}

func TestAskContinuePicksTheLatestSessionOfThisDirectory(t *testing.T) {
	fakeBin(t, "claude", "hermes")
	s := newSandbox(t)

	here := t.TempDir()
	chdir(t, here)
	s.ok("", "ask", "older")
	older := s.only().ID
	s.ok("", "ask", "--agent", "hermes", "newer")
	// Skip cmd sessions even if they are the most recent: ask cannot continue them.
	s.ok("", "run", "--agent", "cmd", "--", "true")

	r := s.ok("", "ask", "--continue", "again")
	if !strings.Contains(r.stdout, "chat -c") {
		t.Errorf("--continue ran %q, want the newer hermes session", r.stdout)
	}
	r = s.ok("", "ask", "--continue", "--agent", "claude", "again")
	if !strings.Contains(r.stdout, "--resume") {
		t.Errorf("--continue --agent claude ran %q, want the claude session", r.stdout)
	}
	if got := s.meta(older).Turns; got != 2 {
		t.Errorf("older session has %d turns, want 2", got)
	}

	chdir(t, t.TempDir())
	elsewhere := s.run("", "ask", "--continue", "again")
	if elsewhere.code != ExitError || !strings.Contains(elsewhere.stderr, "no session to continue") {
		t.Errorf("exit = %d, stderr = %q; want an error in a directory without sessions", elsewhere.code, elsewhere.stderr)
	}
}

func TestAskJSONPrintsOneObject(t *testing.T) {
	fakeBin(t, "claude")
	t.Setenv("FAKE_EXIT", "5")
	t.Setenv("FAKE_STDERR", "agent diagnostics")
	s := newSandbox(t)

	r := s.run("", "ask", "--json", "hi")
	if r.code != 5 {
		t.Fatalf("exit = %d, want the agent's 5 (stderr: %q)", r.code, r.stderr)
	}
	var got askResult
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, r.stdout)
	}
	view := s.only()
	if got.Session != view.ID || got.Agent != "claude" || got.ExitCode != 5 {
		t.Errorf("result = %+v", got)
	}
	if !strings.HasPrefix(got.Output, "argv:-p --session-id ") {
		t.Errorf("output = %q", got.Output)
	}
	if !strings.Contains(r.stderr, "agent diagnostics") {
		t.Errorf("stderr = %q, want the agent's stderr passed through", r.stderr)
	}
	if view.ExitCode == nil || *view.ExitCode != 5 {
		t.Errorf("recorded exit_code = %v, want 5", view.ExitCode)
	}
}

func TestAskVerboseNamesTheSession(t *testing.T) {
	fakeBin(t, "claude")
	s := newSandbox(t)
	r := s.ok("", "-v", "ask", "hi")
	if id := s.only().ID; !strings.Contains(r.stderr, id) {
		t.Errorf("stderr = %q, want the session id %s", r.stderr, id)
	}
}

func TestAskErrors(t *testing.T) {
	fakeBin(t, "claude")
	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no prompt", []string{"ask"}, ExitUsage, "missing prompt"},
		{"blank prompt", []string{"ask", "  "}, ExitUsage, "missing prompt"},
		{"session and continue", []string{"ask", "--session", "20200101-000000-abcd", "--continue", "hi"}, ExitUsage, "cannot be used together"},
		{"prompt that looks like a flag", []string{"ask", "--", "--version"}, ExitUsage, `must not start with "-"`},
		{"unknown agent", []string{"ask", "--agent", "bogus", "hi"}, ExitUsage, "unknown agent"},
		{"agent without one-shot", []string{"ask", "--agent", "cmd", "hi"}, ExitError, "one-shot"},
		{"unknown session", []string{"ask", "--session", "20200101-000000-abcd", "hi"}, ExitError, "session not found"},
		{"nothing to continue", []string{"ask", "--continue", "hi"}, ExitError, "no session to continue"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSandbox(t)
			r := s.run("", tt.args...)
			if r.code != tt.code {
				t.Fatalf("exit = %d, want %d (stderr: %q)", r.code, tt.code, r.stderr)
			}
			if !strings.Contains(r.stderr, tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", r.stderr, tt.want)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want empty", r.stdout)
			}
			if views := s.sessions(); len(views) != 0 {
				t.Errorf("a failed ask left %d session(s) behind", len(views))
			}
		})
	}
}

func TestAskFailsBeforeCreatingASessionWhenTheAgentIsMissing(t *testing.T) {
	emptyPath(t)
	s := newSandbox(t)
	r := s.run("", "ask", "hi")
	if r.code != ExitError || !strings.Contains(r.stderr, "geminus doctor") {
		t.Fatalf("exit = %d, stderr = %q", r.code, r.stderr)
	}
	if _, err := os.Stat(s.home); !os.IsNotExist(err) {
		t.Errorf("state directory was created for an ask that never started (stat error: %v)", err)
	}
}

func TestAskRefusesASessionThatIsAlreadyOpenOrCannotAnswer(t *testing.T) {
	fakeBin(t, "claude")
	s := newSandbox(t)
	busy := s.seed("claude", func(m session.Meta) session.Meta {
		m.Status = session.StatusRunning
		m.PID = os.Getpid()
		return m
	})
	r := s.run("", "ask", "--session", busy.ID, "hi")
	if r.code != ExitError || !strings.Contains(r.stderr, "already open") {
		t.Errorf("exit = %d, stderr = %q; want a refusal", r.code, r.stderr)
	}

	generic := s.seed("cmd", nil)
	r = s.run("", "ask", "--session", generic.ID, "hi")
	if r.code != ExitError || !strings.Contains(r.stderr, "one-shot") {
		t.Errorf("exit = %d, stderr = %q; want cmd sessions to be rejected", r.code, r.stderr)
	}
}

// An ask that fails before launch must leave an existing session unchanged.
func TestAskLeavesAnExistingSessionUntouchedWhenItCannotStart(t *testing.T) {
	fakeBin(t, "claude")
	s := newSandbox(t)
	s.ok("", "ask", "first")
	before := s.only()

	emptyPath(t)
	r := s.run("", "ask", "--session", before.ID, "second")
	if r.code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr: %q)", r.code, ExitError, r.stderr)
	}
	after := s.meta(before.ID)
	if after.Status != session.StatusExited || after.Turns != 1 || after.AgentSession.Value != before.AgentSession.Value {
		t.Errorf("session changed: %+v", after)
	}
}

func TestAskCleansUpWhenTheAgentCannotStart(t *testing.T) {
	dir := fakeBin(t, "claude")
	s := newSandbox(t)
	s.ok("", "ask", "first")
	before := s.only()

	// Replace claude with a file that resolves on PATH but fails to execute.
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("not a program\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := s.run("", "ask", "--session", before.ID, "second")
	if existing.code != ExitError || existing.stdout != "" {
		t.Fatalf("exit = %d, stdout = %q (stderr: %q)", existing.code, existing.stdout, existing.stderr)
	}
	after := s.only()
	if after.Status != session.StatusExited || after.Turns != 1 || after.PID != 0 {
		t.Errorf("session after the failed ask = %+v, want it as it was", after)
	}
	events := s.events(before.ID)
	if last := events[len(events)-1]; last.Type != session.EventNote || !strings.Contains(last.Text, "did not start") {
		t.Errorf("last event = %+v, want a note that the agent did not start", last)
	}

	fresh := s.run("", "ask", "third")
	if fresh.code != ExitError {
		t.Fatalf("exit = %d, want %d", fresh.code, ExitError)
	}
	if views := s.sessions(); len(views) != 1 {
		t.Errorf("an ask that never started left a session behind: %d sessions", len(views))
	}
}

func TestAskPassesPipedInputToTheAgent(t *testing.T) {
	dir := fakeBin(t)
	script := "#!/bin/sh\nprintf 'stdin:'\ncat\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newSandbox(t)
	if r := s.ok("piped notes\n", "ask", "summarize"); r.stdout != "stdin:piped notes\n" {
		t.Errorf("stdout = %q, want the agent to read the piped input", r.stdout)
	}
}
