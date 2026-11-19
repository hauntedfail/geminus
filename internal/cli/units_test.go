package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/agent"
	"github.com/hauntedfail/geminus/internal/proxy"
	"github.com/hauntedfail/geminus/internal/session"
)

func TestTruncateTitle(t *testing.T) {
	long := strings.Repeat("あ", maxTitleRunes+10)
	tests := []struct{ in, want string }{
		{"", ""},
		{"  short  ", "short"},
		{"first line\nsecond line", "first line"},
		{"first\r\nsecond", "first"},
		{strings.Repeat("a", maxTitleRunes), strings.Repeat("a", maxTitleRunes)},
		{long, strings.Repeat("あ", maxTitleRunes-1) + "…"},
	}
	for _, tt := range tests {
		if got := truncateTitle(tt.in); got != tt.want {
			t.Errorf("truncateTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDefaultTitle(t *testing.T) {
	claude, err := agent.Lookup("claude")
	if err != nil {
		t.Fatal(err)
	}
	generic, err := agent.Lookup(agent.GenericName)
	if err != nil {
		t.Fatal(err)
	}
	if got := defaultTitle(claude, []string{"fix", "the", "bug"}); got != "fix the bug" {
		t.Errorf("claude title = %q", got)
	}
	if got := defaultTitle(generic, []string{"curl", "--token", "s3cret"}); got != "curl --token "+agent.Redacted {
		t.Errorf("cmd title = %q, want the secret redacted", got)
	}
	if got := defaultTitle(claude, nil); got != "" {
		t.Errorf("title without args = %q, want empty", got)
	}
}

func TestSameDir(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", dir, dir, true},
		{"unclean path", dir, filepath.Join(dir, "x", ".."), true},
		{"through a symlink", dir, link, true},
		{"different", dir, other, false},
		{"missing", dir, filepath.Join(dir, "missing"), false},
	}
	for _, tt := range tests {
		if got := sameDir(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: sameDir = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"\n\n", ""},
		{"1.2.3\n", "1.2.3"},
		{"\n  tool 2.0 (build 7)  \nmore\n", "tool 2.0 (build 7)"},
		{strings.Repeat("v", maxVersionLen+20), strings.Repeat("v", maxVersionLen)},
		{strings.Repeat("版", maxVersionLen+20), strings.Repeat("版", maxVersionLen)},
	}
	for _, tt := range tests {
		if got := firstLine(tt.in); got != tt.want {
			t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestProbeVersionFailureIsEmpty(t *testing.T) {
	if got := probeVersion(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Errorf("probeVersion of a missing binary = %q, want empty", got)
	}
}

func TestExitCodeMapping(t *testing.T) {
	if got := processExitCode(nil); got != -1 {
		t.Errorf("processExitCode(nil) = %d, want -1", got)
	}
	for in, want := range map[int]int{0: 0, 7: 7, 255: 255, -1: ExitError} {
		if got := childExitCode(in); got != want {
			t.Errorf("childExitCode(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestStoredKey(t *testing.T) {
	if got := storedKey(agent.SessionKey{Kind: agent.KeyNone}); got != nil {
		t.Errorf("storedKey(none) = %+v, want nil", got)
	}
	if got := storedKey(agent.SessionKey{}); got != nil {
		t.Errorf("storedKey(zero) = %+v, want nil", got)
	}
	got := storedKey(agent.SessionKey{Kind: agent.KeyName, Value: "abc"})
	if got == nil || got.Kind != "name" || got.Value != "abc" {
		t.Errorf("storedKey(name) = %+v", got)
	}
}

// When resuming a session without a stored key, start with a new key and record a note.
func TestPlanKeyWithoutARecordedAgentSession(t *testing.T) {
	claude, err := agent.Lookup("claude")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planKey(target{meta: session.Meta{ID: "20200101-000000-abcd"}, adapter: claude}, false)
	if err != nil {
		t.Fatalf("planKey: %v", err)
	}
	if plan.resume || !agent.IsUUID(plan.key.Value) || len(plan.notes) != 1 {
		t.Errorf("plan = %+v, want a new key and one note", plan)
	}
}

type failingLogWriter struct{ calls int }

func (w *failingLogWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("disk full")
}

func TestLenientWriterHidesErrorsAndStopsWriting(t *testing.T) {
	dst := &failingLogWriter{}
	w := &lenientWriter{w: dst}
	for i := 0; i < 3; i++ {
		if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
			t.Fatalf("Write = %d, %v; want 3, nil", n, err)
		}
	}
	if dst.calls != 1 {
		t.Errorf("wrote %d times after the first failure, want 1", dst.calls)
	}
	if err := w.firstError(); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("firstError = %v", err)
	}
}

func openTestLog(t *testing.T) (*session.Store, string, *session.Log) {
	t.Helper()
	store, err := session.Open(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Create(session.CreateParams{Agent: "cmd"})
	if err != nil {
		t.Fatal(err)
	}
	log, err := store.OpenLog(meta.ID, session.LogOptions{Raw: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return store, meta.ID, log
}

func TestInputTapRecordsOnceAndTruncates(t *testing.T) {
	store, id, log := openTestLog(t)
	input := strings.Repeat("x", maxInputRecord+100)
	tap := &inputTap{Terminal: proxy.NewTerminal(strings.NewReader(input), io.Discard), log: log}

	if _, err := io.Copy(io.Discard, tap); err != nil {
		t.Fatalf("read through the tap: %v", err)
	}
	if err := tap.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := tap.flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := store.ReadEvents(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := eventTypes(events); strings.Join(got, ",") != "input,note" {
		t.Fatalf("events = %v, want one input and one note", got)
	}
	if len(events[0].Text) != maxInputRecord {
		t.Errorf("recorded %d bytes of input, want %d", len(events[0].Text), maxInputRecord)
	}
	if !strings.Contains(events[1].Text, "not recorded") {
		t.Errorf("note = %q", events[1].Text)
	}
}

func TestInputTapWithoutInputWritesNothing(t *testing.T) {
	store, id, log := openTestLog(t)
	tap := &inputTap{Terminal: proxy.NewTerminal(strings.NewReader(""), io.Discard), log: log}
	if _, err := io.Copy(io.Discard, tap); err != nil {
		t.Fatal(err)
	}
	if err := tap.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	events, err := store.ReadEvents(id)
	if err != nil || len(events) != 0 {
		t.Errorf("events = %+v, %v; want none", events, err)
	}
}

// If the recording destination is closed, flush remembers and returns the error.
func TestInputTapReportsALogFailure(t *testing.T) {
	_, _, log := openTestLog(t)
	tap := &inputTap{Terminal: proxy.NewTerminal(strings.NewReader("abc"), io.Discard), log: log}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, tap); err != nil {
		t.Fatal(err)
	}
	if err := tap.flush(); !errors.Is(err, session.ErrClosed) {
		t.Errorf("flush = %v, want %v", err, session.ErrClosed)
	}
}

func TestReportMapsUsageErrors(t *testing.T) {
	var out, errOut strings.Builder
	env := Env{Stdout: &out, Stderr: &errOut}
	if code := report(env, "run", usagef("bad %s", "flag")); code != ExitUsage {
		t.Errorf("usage error: exit %d, want %d", code, ExitUsage)
	}
	if code := report(env, "run", errors.New("disk on fire")); code != ExitError {
		t.Errorf("plain error: exit %d, want %d", code, ExitError)
	}
	if got := errOut.String(); !strings.Contains(got, "bad flag") || !strings.Contains(got, "disk on fire") {
		t.Errorf("stderr = %q", got)
	}
}

func TestPrintable(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain title", "plain title"},
		{"日本語 é", "日本語 é"},
		{"\x1b]0;owned\x07title", "?]0;owned?title"},
		{"line\nbreak\ttab\r", "line?break?tab?"},
		{"c1\u009b31m del\x7f", "c1?31m del?"},
		{"bidi\u202eevil\u2066x", "bidi?evil?x"},
		{"broken\xff\xc2utf8", "broken??utf8"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := printable(tt.in); got != tt.want {
			t.Errorf("printable(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWriteJSONEscapesTerminalControls(t *testing.T) {
	var out strings.Builder
	in := map[string]string{"text": "a\x1b[31m\u009b1m\x7f日本語<&>"}
	if err := writeJSON(&out, in); err != nil {
		t.Fatal(err)
	}
	for _, r := range out.String() {
		if r == 0x1b || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			t.Fatalf("output holds the raw control %U:\n%q", r, out.String())
		}
	}
	if !strings.Contains(out.String(), "日本語<&>") {
		t.Errorf("output = %q, want ordinary text left readable", out.String())
	}
	var back map[string]string
	if err := json.Unmarshal([]byte(out.String()), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if back["text"] != in["text"] {
		t.Errorf("round trip = %q, want %q", back["text"], in["text"])
	}
}

func TestRecordedArgv(t *testing.T) {
	argv := []string{"claude", "--session-id", "u", "the prompt", "--token", "t0k"}
	if got := strings.Join(recordedArgv(argv, true), " "); got != "claude --session-id u the prompt --token "+agent.Redacted {
		t.Errorf("recorded = %q, want only the secret value hidden", got)
	}
	if got := strings.Join(recordedArgv(argv, false), " "); got != "claude "+agent.Redacted {
		t.Errorf("unrecorded = %q, want only the command name", got)
	}
	if got := recordedArgv([]string{"true"}, false); len(got) != 1 || got[0] != "true" {
		t.Errorf("unrecorded single argument = %q", got)
	}
}

func TestChildStdin(t *testing.T) {
	if got := childStdin(nil); got != nil {
		t.Errorf("childStdin(nil) = %v, want nil", got)
	}
	reader := strings.NewReader("x")
	if got := childStdin(reader); got != io.Reader(reader) {
		t.Errorf("a non-file reader was not passed through")
	}
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got := childStdin(file); got != io.Reader(file) {
		t.Errorf("a file that is not a terminal was not passed through")
	}
}

// If another process opens the session after loadTarget checks it, begin must refuse
// without changing it.
func TestBeginRefusesASessionOpenedInTheMeantime(t *testing.T) {
	s := newSandbox(t)
	meta := s.seed("cmd", nil)
	store := s.store()
	tgt, err := loadTarget(store, meta.ID, "", false)
	if err != nil {
		t.Fatalf("loadTarget: %v", err)
	}

	// The parent process (go test) is alive, so its PID simulates another process holding the
	// session open.
	other := os.Getppid()
	if _, err := store.Update(meta.ID, func(m session.Meta) session.Meta {
		m.Status = session.StatusRunning
		m.PID = other
		return m
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := begin(store, tgt, startParams{argv: []string{"true"}, record: true}); !errors.Is(err, errBusy) {
		t.Fatalf("begin = %v, want errBusy", err)
	}
	if got := s.meta(meta.ID); got.Status != session.StatusRunning || got.PID != other {
		t.Errorf("the refused begin changed the session: %+v", got)
	}
	if events := s.events(meta.ID); len(events) != 0 {
		t.Errorf("the refused begin wrote events: %+v", events)
	}

	active, err := begin(store, tgt, startParams{argv: []string{"true"}, record: true, force: true})
	if err != nil {
		t.Fatalf("begin with force: %v", err)
	}
	if err := active.finish(0, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got := s.meta(meta.ID); got.Status != session.StatusExited || got.PID != 0 {
		t.Errorf("after the forced run: %+v", got)
	}
}
