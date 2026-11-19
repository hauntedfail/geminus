package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/agent"
	"github.com/hauntedfail/geminus/internal/session"
)

const sampleRaw = "\x1b[1;32mready\x1b[0m\r\nline two\r\n\x1b]0;title\x07line three\r\n"

// writeBrokenSession は session.json が読めないセッションを置き、その id を返す。
func writeBrokenSession(t *testing.T, s *sandbox, id, content string) string {
	t.Helper()
	dir := filepath.Join(s.home, sessionsDirName, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLsWithoutSessions(t *testing.T) {
	s := newSandbox(t)
	if r := s.ok("", "ls"); r.stdout != "" || r.stderr != "" {
		t.Errorf("ls on an empty home printed stdout %q, stderr %q", r.stdout, r.stderr)
	}
	if r := s.ok("", "ls", "--json"); strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("ls --json = %q, want an empty array", r.stdout)
	}
	if _, err := os.Stat(s.home); !os.IsNotExist(err) {
		t.Errorf("ls created the state directory (stat error: %v)", err)
	}
}

func TestLsListsNewestFirstAndTruncates(t *testing.T) {
	s := newSandbox(t)
	var ids []string
	for i := 0; i < lsLimit+3; i++ {
		ids = append(ids, s.seed("cmd", nil).ID)
	}
	newest := ids[len(ids)-1]

	r := s.ok("", "ls")
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if len(lines) != lsLimit+1 {
		t.Fatalf("ls printed %d lines, want a header and %d sessions", len(lines), lsLimit)
	}
	if !strings.HasPrefix(lines[0], "ID") || !strings.HasPrefix(lines[1], newest) {
		t.Errorf("first lines = %q, %q; want a header, then the newest session %s", lines[0], lines[1], newest)
	}
	if !strings.Contains(r.stderr, "3 older session(s) not shown") {
		t.Errorf("stderr = %q, want a note about the hidden sessions", r.stderr)
	}

	all := s.ok("", "ls", "--all")
	if got := strings.Count(all.stdout, "\n"); got != lsLimit+4 {
		t.Errorf("ls --all printed %d lines, want %d", got, lsLimit+4)
	}
	if all.stderr != "" {
		t.Errorf("ls --all stderr = %q, want empty", all.stderr)
	}

	var views []sessionView
	if err := json.Unmarshal([]byte(s.ok("", "ls", "--json").stdout), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != lsLimit || views[0].ID != newest {
		t.Errorf("ls --json returned %d sessions starting with %s", len(views), views[0].ID)
	}
}

func TestLsShowsStaleAndRunningSessions(t *testing.T) {
	s := newSandbox(t)
	running := s.seed("cmd", func(m session.Meta) session.Meta {
		m.Status = session.StatusRunning
		m.PID = os.Getpid()
		return m
	})
	stale := s.seed("cmd", func(m session.Meta) session.Meta {
		m.Status = session.StatusRunning
		m.PID = 0x7ffffffe
		return m
	})

	r := s.ok("", "ls")
	for _, line := range strings.Split(r.stdout, "\n") {
		switch {
		case strings.HasPrefix(line, running.ID) && !strings.Contains(line, "running"):
			t.Errorf("live session shown as %q", line)
		case strings.HasPrefix(line, stale.ID) && !strings.Contains(line, statusStale):
			t.Errorf("dead session shown as %q, want %s", line, statusStale)
		}
	}
	for _, view := range s.sessions() {
		if want := view.ID == running.ID; view.Active != want {
			t.Errorf("session %s: active = %v, want %v", view.ID, view.Active, want)
		}
	}
	if r := s.ok("", "show", running.ID); !strings.Contains(r.stdout, "running (pid ") {
		t.Errorf("show = %q, want the pid of the live session", r.stdout)
	}
}

func TestLsReportsBrokenSessionsButListsTheRest(t *testing.T) {
	s := newSandbox(t)
	good := s.seed("cmd", nil)
	broken := writeBrokenSession(t, s, "20200101-000000-dead", "{not json")

	for _, args := range [][]string{{"ls"}, {"ls", "--json"}} {
		r := s.run("", args...)
		if r.code != ExitError {
			t.Fatalf("%v: exit = %d, want %d", args, r.code, ExitError)
		}
		if !strings.Contains(r.stdout, good.ID) || strings.Contains(r.stdout, broken) {
			t.Errorf("%v: stdout = %q, want only the readable session", args, r.stdout)
		}
		if !strings.Contains(r.stderr, broken) || !strings.Contains(r.stderr, "corrupt") {
			t.Errorf("%v: stderr = %q, want the broken session named", args, r.stderr)
		}
	}
	var views []sessionView
	if err := json.Unmarshal([]byte(s.run("", "ls", "--json").stdout), &views); err != nil || len(views) != 1 {
		t.Errorf("ls --json with a broken session: %v, %d sessions", err, len(views))
	}
}

// session.json に仕込まれた制御シーケンスを、ls / show が端末へそのまま出さない。
func TestLsAndShowNeutralizeControlSequencesInMeta(t *testing.T) {
	s := newSandbox(t)
	meta := s.seed("cmd", func(m session.Meta) session.Meta {
		m.Title = "\x1b]0;owned\x07evil\u009b2Jtitle"
		m.Cwd = "/tmp/\x1b[31mred"
		m.AgentSession = &session.AgentSession{Kind: "name", Value: "x\x1b[2Jy"}
		return m
	})
	for _, args := range [][]string{{"ls"}, {"show", meta.ID}, {"ls", "--json"}, {"show", meta.ID, "--json"}} {
		out := s.ok("", args...).stdout
		for _, r := range out {
			if r == 0x1b || r == 0x07 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				t.Errorf("%v printed the control %U: %q", args, r, out)
				break
			}
		}
		if !strings.Contains(out, "evil") {
			t.Errorf("%v dropped the readable part of the title: %q", args, out)
		}
	}
	var view sessionView
	if err := json.Unmarshal([]byte(s.ok("", "show", meta.ID, "--json").stdout), &view); err != nil {
		t.Fatal(err)
	}
	if view.Title != meta.Title || view.Cwd != meta.Cwd {
		t.Errorf("--json changed the values: %+v", view)
	}
}

func TestShow(t *testing.T) {
	s := newSandbox(t)
	code := 3
	meta := s.seed("claude", func(m session.Meta) session.Meta {
		m.Status = session.StatusExited
		m.ExitCode = &code
		m.AgentSession = &session.AgentSession{Kind: "uuid", Value: "11111111-2222-4333-8444-555555555555"}
		m.Turns = 2
		return m
	})

	r := s.ok("", "show", meta.ID)
	for _, want := range []string{meta.ID, "seeded", "claude", "uuid 11111111-2222-4333-8444-555555555555", "exited (exit 3)", meta.Cwd} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("show does not mention %q:\n%s", want, r.stdout)
		}
	}

	var view sessionView
	if err := json.Unmarshal([]byte(s.ok("", "show", meta.ID, "--json").stdout), &view); err != nil {
		t.Fatalf("show --json: %v", err)
	}
	if view.ID != meta.ID || view.Turns != 2 || view.ExitCode == nil || *view.ExitCode != 3 || view.Active {
		t.Errorf("show --json = %+v", view)
	}
	if want := filepath.Join(s.home, sessionsDirName, meta.ID); view.Dir != want {
		t.Errorf("dir = %q, want %q", view.Dir, want)
	}
}

func TestShowErrors(t *testing.T) {
	s := newSandbox(t)
	broken := writeBrokenSession(t, s, "20200101-000000-dead", "")
	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no id", []string{"show"}, ExitUsage, "missing session id"},
		{"two ids", []string{"show", "a", "b"}, ExitUsage, `unexpected argument "b"`},
		{"unknown id", []string{"show", "20200101-000000-abcd"}, ExitError, "session not found"},
		{"malformed id", []string{"show", "../../etc/passwd"}, ExitError, "invalid session id"},
		{"broken session", []string{"show", broken}, ExitError, "corrupt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := s.run("", tt.args...)
			if r.code != tt.code || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("exit = %d, stderr = %q; want %d and %q", r.code, r.stderr, tt.code, tt.want)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want empty", r.stdout)
			}
		})
	}
}

func TestLog(t *testing.T) {
	s := newSandbox(t)
	id := s.seed("cmd", nil).ID
	s.seedLog(id, sampleRaw)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"stripped by default", []string{"log", id}, "ready\nline two\nline three\n"},
		{"raw", []string{"log", id, "--raw"}, sampleRaw},
		{"tail", []string{"log", id, "--tail", "2"}, "line two\nline three\n"},
		{"tail larger than the log", []string{"log", "--tail=99", id}, "ready\nline two\nline three\n"},
		{"raw tail", []string{"log", id, "--raw", "--tail", "1"}, "\x1b]0;title\x07line three\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := s.ok("", tt.args...); r.stdout != tt.want {
				t.Errorf("stdout = %q, want %q", r.stdout, tt.want)
			}
		})
	}
}

func TestLogJSON(t *testing.T) {
	s := newSandbox(t)
	id := s.seed("cmd", nil).ID
	s.seedLog(id, sampleRaw)

	var events []session.Event
	if err := json.Unmarshal([]byte(s.ok("", "log", id, "--json").stdout), &events); err != nil {
		t.Fatalf("log --json: %v", err)
	}
	if got := eventTypes(events); strings.Join(got, ",") != "session.start,output,session.exit" {
		t.Errorf("events = %v", got)
	}
	checkOffsets(t, events, len(sampleRaw))

	var last []session.Event
	if err := json.Unmarshal([]byte(s.ok("", "log", id, "--json", "--tail", "1").stdout), &last); err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last[0].Type != session.EventExit {
		t.Errorf("log --json --tail 1 = %+v, want the exit event", last)
	}

	empty := s.seed("cmd", nil).ID
	if r := s.ok("", "log", empty, "--json"); strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("log --json without a transcript = %q, want []", r.stdout)
	}
}

// transcript に壊れた行があっても、読めた分は出して終了コードで知らせる。
func TestLogJSONWithACorruptLine(t *testing.T) {
	s := newSandbox(t)
	id := s.seed("cmd", nil).ID
	s.seedLog(id, "x")
	paths, err := s.store().Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(paths.Transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{torn\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	r := s.run("", "log", id, "--json")
	if r.code != ExitError || !strings.Contains(r.stderr, "corrupt") {
		t.Errorf("exit = %d, stderr = %q; want the corrupt line reported", r.code, r.stderr)
	}
	var events []session.Event
	if err := json.Unmarshal([]byte(r.stdout), &events); err != nil || len(events) != 3 {
		t.Errorf("stdout: %v, %d events; want the 3 readable events", err, len(events))
	}
}

func TestLogErrors(t *testing.T) {
	s := newSandbox(t)
	unrecorded := s.seed("cmd", nil).ID
	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no id", []string{"log"}, ExitUsage, "missing session id"},
		{"negative tail", []string{"log", unrecorded, "--tail", "-1"}, ExitUsage, "must not be negative"},
		{"raw and json", []string{"log", unrecorded, "--raw", "--json"}, ExitUsage, "cannot be used together"},
		{"unknown id", []string{"log", "20200101-000000-abcd"}, ExitError, "session not found"},
		{"unknown id json", []string{"log", "20200101-000000-abcd", "--json"}, ExitError, "session not found"},
		{"no raw.log", []string{"log", unrecorded}, ExitError, "no raw.log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := s.run("", tt.args...)
			if r.code != tt.code || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("exit = %d, stderr = %q; want %d and %q", r.code, r.stderr, tt.code, tt.want)
			}
		})
	}
}

func TestRm(t *testing.T) {
	s := newSandbox(t)
	id := s.seed("cmd", nil).ID
	if r := s.ok("", "rm", id); r.stdout != "" || r.stderr != "" {
		t.Errorf("rm printed stdout %q, stderr %q", r.stdout, r.stderr)
	}
	if views := s.sessions(); len(views) != 0 {
		t.Errorf("session survived rm: %+v", views)
	}

	missing := s.run("", "rm", id)
	if missing.code != ExitError || !strings.Contains(missing.stderr, "session not found") {
		t.Errorf("rm of a missing session: exit %d, stderr %q", missing.code, missing.stderr)
	}
	s.ok("", "rm", id, "--force")
	s.ok("", "rm", "--force", id)

	if r := s.run("", "rm", "--force", "not-an-id"); r.code != ExitError || !strings.Contains(r.stderr, "invalid session id") {
		t.Errorf("rm --force of a malformed id: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := s.run("", "rm"); r.code != ExitUsage {
		t.Errorf("rm without an id: exit %d, want %d", r.code, ExitUsage)
	}

	verbose := s.seed("cmd", nil).ID
	if r := s.ok("", "-v", "rm", verbose); !strings.Contains(r.stderr, verbose) {
		t.Errorf("rm -v stderr = %q, want the deleted id", r.stderr)
	}
}

func TestRmRefusesAnOpenSessionUnlessForced(t *testing.T) {
	s := newSandbox(t)
	meta := s.seed("cmd", func(m session.Meta) session.Meta {
		m.Status = session.StatusRunning
		m.PID = os.Getpid()
		return m
	})
	r := s.run("", "rm", meta.ID)
	if r.code != ExitError || !strings.Contains(r.stderr, "--force") {
		t.Fatalf("exit = %d, stderr = %q; want a refusal that mentions --force", r.code, r.stderr)
	}
	if len(s.sessions()) != 1 {
		t.Fatal("a refused rm deleted the session")
	}
	s.ok("", "rm", meta.ID, "--force")
	if len(s.sessions()) != 0 {
		t.Error("rm --force left the session behind")
	}
}

func TestRmDeletesABrokenSession(t *testing.T) {
	s := newSandbox(t)
	broken := writeBrokenSession(t, s, "20200101-000000-dead", "{not json")
	s.ok("", "rm", broken)
	if _, err := os.Stat(filepath.Join(s.home, sessionsDirName, broken)); !os.IsNotExist(err) {
		t.Errorf("broken session survived rm (stat error: %v)", err)
	}
}

func TestAgents(t *testing.T) {
	dir := fakeBin(t, "claude")
	// hermes は見つからない状態にする。
	t.Setenv("PATH", dir)
	s := newSandbox(t)

	var views []agentView
	if err := json.Unmarshal([]byte(s.ok("", "agents", "--json").stdout), &views); err != nil {
		t.Fatalf("agents --json: %v", err)
	}
	byName := map[string]agentView{}
	for _, v := range views {
		byName[v.Name] = v
	}
	claude, hermes, generic := byName["claude"], byName["hermes"], byName["cmd"]
	if !claude.Found || claude.Path != filepath.Join(dir, "claude") || claude.Version != "fake 1.2.3" {
		t.Errorf("claude = %+v", claude)
	}
	if want := (agent.Capabilities{Resume: true, Oneshot: true, SessionKey: agent.KeyUUID}); claude.Capabilities != want {
		t.Errorf("claude capabilities = %+v, want %+v", claude.Capabilities, want)
	}
	if hermes.Found || hermes.Path != "" || hermes.Error == "" || hermes.Capabilities.SessionKey != agent.KeyName {
		t.Errorf("hermes = %+v, want it reported as missing", hermes)
	}
	if !generic.Found || generic.Capabilities.Resume || generic.Capabilities.Oneshot {
		t.Errorf("cmd = %+v", generic)
	}

	text := s.ok("", "agents").stdout
	for _, want := range []string{"NAME", "claude", "fake 1.2.3", "resume, oneshot, key=uuid", "hermes", "(any command)", "key=none"} {
		if !strings.Contains(text, want) {
			t.Errorf("agents does not mention %q:\n%s", want, text)
		}
	}
	if r := s.run("", "agents", "extra"); r.code != ExitUsage {
		t.Errorf("agents with an argument: exit %d, want %d", r.code, ExitUsage)
	}
}

func TestDoctorHealthy(t *testing.T) {
	emptyPath(t)
	s := newSandbox(t)

	r := s.ok("", "doctor")
	for _, want := range []string{"pty", "terminal", "not created yet", "agent claude", "not found in PATH"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("doctor does not mention %q:\n%s", want, r.stdout)
		}
	}
	if _, err := os.Stat(s.home); !os.IsNotExist(err) {
		t.Errorf("doctor created the state directory (stat error: %v)", err)
	}

	s.seed("cmd", nil)
	var report doctorReport
	if err := json.Unmarshal([]byte(s.ok("", "doctor", "--json").stdout), &report); err != nil {
		t.Fatalf("doctor --json: %v", err)
	}
	if !report.OK {
		t.Errorf("report = %+v, want ok", report)
	}
	status := map[string]string{}
	for _, c := range report.Checks {
		status[c.Name] = c.Status
	}
	for name, want := range map[string]string{"home": checkOK, "sessions": checkOK, "config": checkOK, "terminal": checkOK, "agent claude": checkWarn, "agent cmd": checkOK} {
		if status[name] != want {
			t.Errorf("check %q = %q, want %q (%+v)", name, status[name], want, report.Checks)
		}
	}
	if runtime.GOOS != "windows" && status["pty"] != checkOK {
		t.Errorf("pty check = %q, want ok", status["pty"])
	}
	if entries, err := os.ReadDir(s.home); err != nil || len(entries) != 1 {
		t.Errorf("doctor left files in the state directory: %v, %v", entries, err)
	}
}

func TestDoctorFindsProblems(t *testing.T) {
	fakeBin(t, "claude")

	t.Run("home is a file", func(t *testing.T) {
		s := newSandbox(t)
		if err := os.WriteFile(s.home, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		r := s.run("", "doctor")
		if r.code != ExitError || !strings.Contains(r.stdout, "is not a directory") {
			t.Errorf("exit = %d, stdout = %q", r.code, r.stdout)
		}
	})

	t.Run("broken config", func(t *testing.T) {
		s := newSandbox(t)
		if err := os.MkdirAll(s.home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.home, configFileName), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := s.run("", "doctor", "--json")
		var report doctorReport
		if err := json.Unmarshal([]byte(r.stdout), &report); err != nil {
			t.Fatalf("doctor --json: %v", err)
		}
		if r.code != ExitError || report.OK {
			t.Errorf("exit = %d, report = %+v; want a failure", r.code, report)
		}
	})

	t.Run("config names an unknown agent", func(t *testing.T) {
		s := newSandbox(t)
		if err := os.MkdirAll(s.home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.home, configFileName), []byte(`{"agent":"bogus"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if r := s.run("", "doctor"); r.code != ExitError || !strings.Contains(r.stdout, "bogus") {
			t.Errorf("exit = %d, stdout = %q", r.code, r.stdout)
		}
	})

	t.Run("home readable by others is a warning", func(t *testing.T) {
		s := newSandbox(t)
		if err := os.MkdirAll(s.home, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.home, configFileName), []byte(`{"agent":"claude"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		r := s.ok("", "doctor")
		if !strings.Contains(r.stdout, "warn") || !strings.Contains(r.stdout, "chmod 700") {
			t.Errorf("stdout = %q, want a warning about the mode", r.stdout)
		}
		if !strings.Contains(r.stdout, "fake 1.2.3") || !strings.Contains(r.stdout, "default agent: claude (config.json)") {
			t.Errorf("stdout = %q, want the agent version and the configured default", r.stdout)
		}
	})

	t.Run("unexpected argument", func(t *testing.T) {
		if r := newSandbox(t).run("", "doctor", "extra"); r.code != ExitUsage {
			t.Errorf("exit = %d, want %d", r.code, ExitUsage)
		}
	})
}

// GEMINUS_HOME は --home が無いときに使われ、--home はそれを上書きする。
func TestHomeResolution(t *testing.T) {
	envHome := filepath.Join(t.TempDir(), "from-env")
	t.Setenv(session.EnvHome, envHome)
	store, err := session.Open(envHome)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Create(session.CreateParams{Agent: "cmd", Cwd: mustGetwd(t)})
	if err != nil {
		t.Fatal(err)
	}

	fromEnv := run(t, "ls")
	if fromEnv.code != ExitOK || !strings.Contains(fromEnv.stdout, meta.ID) {
		t.Errorf("ls with GEMINUS_HOME: exit %d, stdout %q", fromEnv.code, fromEnv.stdout)
	}
	overridden := run(t, "ls", "--home", filepath.Join(t.TempDir(), "other"))
	if overridden.code != ExitOK || overridden.stdout != "" {
		t.Errorf("ls --home: exit %d, stdout %q; want the flag to win", overridden.code, overridden.stdout)
	}
}
