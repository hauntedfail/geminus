//go:build unix

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	gopty "github.com/aymanbagabas/go-pty"
	"golang.org/x/term"
)

// openTTY は実際の PTY を確保し、端末側（slave）を cols x rows にして返す。
func openTTY(t *testing.T, cols, rows int) *os.File {
	t.Helper()
	pty, err := gopty.New()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { pty.Close() })
	unix, ok := pty.(gopty.UnixPty)
	if !ok {
		t.Skip("not a unix pty")
	}
	if err := pty.Resize(cols, rows); err != nil {
		t.Fatal(err)
	}
	return unix.Slave()
}

// doctorOn は stdin を tty にして doctor を実行する。
func doctorOn(t *testing.T, s *sandbox, tty *os.File, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(append([]string{"--home", s.home, "doctor"}, args...), tty, &out, &errOut)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func decodeDoctor(t *testing.T, r result) doctorReport {
	t.Helper()
	var report doctorReport
	if err := json.Unmarshal([]byte(r.stdout), &report); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, r.stdout)
	}
	return report
}

func findCheck(report doctorReport, name string) check {
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	return check{}
}

func TestDoctorReportsTheTerminalState(t *testing.T) {
	emptyPath(t)
	s := newSandbox(t)
	tty := openTTY(t, 90, 33)

	r := doctorOn(t, s, tty, "--json")
	report := decodeDoctor(t, r)
	if want := (terminalState{TTY: true, Cols: 90, Rows: 33, Echo: true, EchoSupported: true}); report.Terminal != want {
		t.Errorf("terminal = %+v, want %+v", report.Terminal, want)
	}
	if c := findCheck(report, "terminal echo"); c.Status != checkOK {
		t.Errorf("echo check = %+v, want ok", c)
	}
	if r.code != ExitOK || !report.OK {
		t.Errorf("exit = %d, ok = %v", r.code, report.OK)
	}

	// --json のキー名は外から読まれるので固定する。
	var raw struct {
		Terminal map[string]any `json:"terminal"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tty", "cols", "rows", "echo", "echo_supported"} {
		if _, ok := raw.Terminal[key]; !ok {
			t.Errorf("terminal has no %q key: %v", key, raw.Terminal)
		}
	}
	if len(raw.Terminal) != 5 {
		t.Errorf("terminal = %v, want exactly 5 keys", raw.Terminal)
	}
}

// 前回の geminus が raw mode のまま落ちた状態。doctor は警告し、直し方を1行添える。直しはしない。
func TestDoctorWarnsWhenEchoIsOff(t *testing.T) {
	emptyPath(t)
	s := newSandbox(t)
	tty := openTTY(t, 80, 24)
	state, err := term.MakeRaw(int(tty.Fd()))
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	defer term.Restore(int(tty.Fd()), state)

	r := doctorOn(t, s, tty, "--json")
	report := decodeDoctor(t, r)
	if report.Terminal.Echo || !report.Terminal.EchoSupported || !report.Terminal.TTY {
		t.Errorf("terminal = %+v, want a tty with echo off", report.Terminal)
	}
	c := findCheck(report, "terminal echo")
	if c.Status != checkWarn || !strings.Contains(c.Detail, "stty sane") {
		t.Errorf("echo check = %+v, want a warning that mentions stty sane", c)
	}
	// 警告であって失敗ではない。
	if r.code != ExitOK || !report.OK {
		t.Errorf("exit = %d, ok = %v; want a warning only", r.code, report.OK)
	}

	text := doctorOn(t, s, tty)
	var line string
	for _, l := range strings.Split(text.stdout, "\n") {
		if strings.Contains(l, "terminal echo") {
			line = l
		}
	}
	if !strings.HasPrefix(line, "warn") || !strings.Contains(line, "stty sane") {
		t.Errorf("text output has no one-line warning with the fix: %q", text.stdout)
	}
	if n := strings.Count(text.stdout, "stty sane"); n != 1 {
		t.Errorf("stty sane appears %d times, want once:\n%s", n, text.stdout)
	}

	// doctor は端末の設定を変えない。
	if after := decodeDoctor(t, doctorOn(t, s, tty, "--json")); after.Terminal.Echo {
		t.Error("doctor turned echo back on; it must only report")
	}
}

// 端末が無ければ echo は読めない。失敗にも警告にもしない。
func TestDoctorWithoutATerminal(t *testing.T) {
	emptyPath(t)
	s := newSandbox(t)
	report := decodeDoctor(t, s.ok("", "doctor", "--json"))
	if want := (terminalState{}); report.Terminal != want {
		t.Errorf("terminal = %+v, want the zero value", report.Terminal)
	}
	if c := findCheck(report, "terminal echo"); c.Status != checkOK || strings.Contains(c.Detail, "stty") {
		t.Errorf("echo check = %+v, want ok without advice", c)
	}
}
