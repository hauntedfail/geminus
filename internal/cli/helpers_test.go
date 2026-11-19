package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/session"
)

// sandbox holds the state directory for one test.
type sandbox struct {
	t    *testing.T
	home string
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	// Always pass --home to avoid touching the user's ~/.geminus or GEMINUS_HOME.
	return &sandbox{t: t, home: filepath.Join(t.TempDir(), "home")}
}

// run invokes `geminus --home <sandbox> args...` with stdin as input.
func (s *sandbox) run(stdin string, args ...string) result {
	s.t.Helper()
	var out, errOut bytes.Buffer
	code := Run(append([]string{"--home", s.home}, args...), strings.NewReader(stdin), &out, &errOut)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// ok requires exit code zero.
func (s *sandbox) ok(stdin string, args ...string) result {
	s.t.Helper()
	r := s.run(stdin, args...)
	if r.code != ExitOK {
		s.t.Fatalf("geminus %s: exit = %d, want 0\nstdout: %q\nstderr: %q", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

func (s *sandbox) store() *session.Store {
	s.t.Helper()
	store, err := session.Open(s.home)
	if err != nil {
		s.t.Fatalf("session.Open: %v", err)
	}
	return store
}

// sessions returns the results of ls --json --all.
func (s *sandbox) sessions() []sessionView {
	s.t.Helper()
	r := s.ok("", "ls", "--json", "--all")
	var views []sessionView
	if err := json.Unmarshal([]byte(r.stdout), &views); err != nil {
		s.t.Fatalf("ls --json is not a JSON array: %v\n%s", err, r.stdout)
	}
	return views
}

// only requires exactly one session and returns it.
func (s *sandbox) only() sessionView {
	s.t.Helper()
	views := s.sessions()
	if len(views) != 1 {
		s.t.Fatalf("got %d sessions, want 1: %+v", len(views), views)
	}
	return views[0]
}

func (s *sandbox) meta(id string) session.Meta {
	s.t.Helper()
	meta, err := s.store().Load(id)
	if err != nil {
		s.t.Fatalf("Load(%s): %v", id, err)
	}
	return meta
}

func (s *sandbox) events(id string) []session.Event {
	s.t.Helper()
	events, err := s.store().ReadEvents(id)
	if err != nil {
		s.t.Fatalf("ReadEvents(%s): %v", id, err)
	}
	return events
}

func (s *sandbox) rawLog(id string) string {
	s.t.Helper()
	paths, err := s.store().Paths(id)
	if err != nil {
		s.t.Fatalf("Paths(%s): %v", id, err)
	}
	data, err := os.ReadFile(paths.Raw)
	if err != nil {
		s.t.Fatalf("read raw.log: %v", err)
	}
	return string(data)
}

// seed creates a session through the store API and applies change to it.
func (s *sandbox) seed(agentName string, change func(session.Meta) session.Meta) session.Meta {
	s.t.Helper()
	store := s.store()
	meta, err := store.Create(session.CreateParams{Title: "seeded", Agent: agentName, Cwd: mustGetwd(s.t)})
	if err != nil {
		s.t.Fatalf("Create: %v", err)
	}
	if change == nil {
		return meta
	}
	meta, err = store.Update(meta.ID, change)
	if err != nil {
		s.t.Fatalf("Update: %v", err)
	}
	return meta
}

// seedLog records one chunk of raw output in a seeded session.
func (s *sandbox) seedLog(id, raw string) {
	s.t.Helper()
	log, err := s.store().OpenLog(id, session.LogOptions{Raw: true})
	if err != nil {
		s.t.Fatalf("OpenLog: %v", err)
	}
	if _, err := log.AppendEvent(session.StartEvent("cmd", []string{"seed"})); err != nil {
		s.t.Fatalf("AppendEvent: %v", err)
	}
	if _, err := log.Write([]byte(raw)); err != nil {
		s.t.Fatalf("Write: %v", err)
	}
	if _, err := log.AppendEvent(session.ExitEvent(0)); err != nil {
		s.t.Fatalf("AppendEvent: %v", err)
	}
	if err := log.Close(); err != nil {
		s.t.Fatalf("Close: %v", err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return dir
}

// chdir switches to dir and restores the working directory after the test.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev := mustGetwd(t)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prev); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}

// needShell skips tests that use shell-script agents when sh is unavailable.
func needShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
}

// fakeAgentScript stands in for claude or hermes. It prints its arguments on one line and
// exits with FAKE_EXIT.
const fakeAgentScript = `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "fake 1.2.3"
  exit 0
fi
echo "argv:$*"
if [ -n "$FAKE_STDERR" ]; then
  echo "$FAKE_STDERR" >&2
fi
exit "${FAKE_EXIT:-0}"
`

// fakeBin prepends a directory to PATH containing only the named fake agents.
func fakeBin(t *testing.T, names ...string) string {
	t.Helper()
	needShell(t)
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fakeAgentScript), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// emptyPath sets PATH to a single empty directory so no agents can be found.
func emptyPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// argvLine extracts the contents of the fake agent's "argv:..." line.
func argvLine(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.Index(line, "argv:"); i >= 0 {
			return line[i+len("argv:"):]
		}
	}
	t.Fatalf("no argv line in output %q", output)
	return ""
}

func eventTypes(events []session.Event) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func findEvent(events []session.Event, typ string) (session.Event, bool) {
	for _, event := range events {
		if event.Type == typ {
			return event, true
		}
	}
	return session.Event{}, false
}

// checkOffsets verifies that output events cover raw.log without gaps.
func checkOffsets(t *testing.T, events []session.Event, rawLen int) {
	t.Helper()
	var next int64
	for _, event := range events {
		if event.Type != session.EventOutput {
			continue
		}
		if event.Offset == nil || event.Bytes == nil {
			t.Fatalf("output event without offset/bytes: %+v", event)
		}
		if *event.Offset != next {
			t.Errorf("output event seq %d starts at %d, want %d", event.Seq, *event.Offset, next)
		}
		next = *event.Offset + *event.Bytes
	}
	if next != int64(rawLen) {
		t.Errorf("output events cover %d bytes, raw.log has %d", next, rawLen)
	}
}
