package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/hauntedfail/geminus/internal/agent"
	"github.com/hauntedfail/geminus/internal/proxy"
	"github.com/hauntedfail/geminus/internal/session"
	"github.com/hauntedfail/geminus/internal/version"
)

// Diagnostic outcomes. A single failure makes doctor return ExitError.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
)

// sessionsDirName names the sessions subdirectory under the state directory and must match
// internal/session.
const sessionsDirName = "sessions"

// check represents one diagnostic check.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// echoFix is the command to restore terminal echo. doctor suggests it but never runs it.
const echoFix = "stty sane"

// terminalState describes the terminal on which doctor is running.
type terminalState struct {
	// TTY reports whether stdin is a terminal.
	TTY bool `json:"tty"`
	// Cols and Rows give the terminal dimensions, or zero if unavailable.
	Cols int `json:"cols"`
	Rows int `json:"rows"`
	// Echo reports whether the terminal echoes input. It is meaningful only if EchoSupported is
	// true.
	Echo bool `json:"echo"`
	// EchoSupported reports whether the echo setting could be read. It is false on Windows or
	// if none of the standard streams is a terminal.
	EchoSupported bool `json:"echo_supported"`
}

// doctorReport is the output of `doctor --json`.
type doctorReport struct {
	OK       bool          `json:"ok"`
	Terminal terminalState `json:"terminal"`
	Checks   []check       `json:"checks"`
}

func runDoctor(inv Invocation) int {
	const name = "doctor"
	if err := noArgs(inv.Args); err != nil {
		return report(inv.Env, name, err)
	}

	checks := []check{{Name: "version", Status: checkOK, Detail: version.String()}}
	checks = append(checks, checkHome(inv)...)
	terminal := inspectTerminal(inv.Env)
	checks = append(checks, checkPTY(), checkTerminal(inv.Env, terminal), checkEcho(terminal))
	checks = append(checks, checkAgents()...)

	result := doctorReport{OK: true, Terminal: terminal, Checks: checks}
	for _, c := range checks {
		if c.Status == checkFail {
			result.OK = false
		}
	}

	var err error
	if inv.Options.JSON {
		err = writeJSON(inv.Env.Stdout, result)
	} else {
		err = writeChecks(inv.Env.Stdout, checks)
	}
	if err != nil {
		return fail(inv.Env, name, err)
	}
	if !result.OK {
		return ExitError
	}
	return ExitOK
}

func writeChecks(w io.Writer, checks []check) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, c := range checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Status, c.Name, c.Detail)
	}
	return tw.Flush()
}

// checkHome inspects the state directory and config.json without creating directories. It
// briefly creates a temporary file in an existing directory to verify write access.
func checkHome(inv Invocation) []check {
	const name = "home"
	home, err := session.ResolveHome(inv.Options.Home)
	if err != nil {
		return []check{{Name: name, Status: checkFail, Detail: err.Error()}}
	}
	checks := []check{checkStateDir(name, home)}
	if sessions := filepath.Join(home, sessionsDirName); dirExists(sessions) {
		checks = append(checks, checkStateDir("sessions", sessions))
	}
	return append(checks, checkConfig(home))
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// checkStateDir checks that dir is writable only by its owner. A missing directory is fine:
// the first run creates it.
func checkStateDir(name, dir string) check {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return check{Name: name, Status: checkOK, Detail: dir + " (not created yet)"}
	case err != nil:
		return check{Name: name, Status: checkFail, Detail: err.Error()}
	case !info.IsDir():
		return check{Name: name, Status: checkFail, Detail: dir + " is not a directory"}
	}
	if err := probeWritable(dir); err != nil {
		return check{Name: name, Status: checkFail, Detail: fmt.Sprintf("%s is not writable: %v", dir, err)}
	}
	mode := info.Mode().Perm()
	// Windows permission bits do not reflect ACLs, so skip this check there.
	if runtime.GOOS != "windows" && mode&0o077 != 0 {
		detail := fmt.Sprintf("%s has mode %04o; others can read it. Sessions may hold secrets: chmod 700", dir, mode)
		return check{Name: name, Status: checkWarn, Detail: detail}
	}
	return check{Name: name, Status: checkOK, Detail: fmt.Sprintf("%s (mode %04o)", dir, mode)}
}

// probeWritable checks write access by creating and removing a temporary file in dir.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	return errors.Join(f.Close(), os.Remove(f.Name()))
}

func checkConfig(home string) check {
	const name = "config"
	cfg, err := loadConfig(home)
	if err != nil {
		return check{Name: name, Status: checkFail, Detail: err.Error()}
	}
	if cfg.Agent == "" {
		return check{Name: name, Status: checkOK, Detail: "default agent: " + agent.DefaultName}
	}
	if _, err := agent.Lookup(cfg.Agent); err != nil {
		return check{Name: name, Status: checkFail, Detail: fmt.Sprintf("%s: %v", configFileName, err)}
	}
	return check{Name: name, Status: checkOK, Detail: fmt.Sprintf("default agent: %s (%s)", cfg.Agent, configFileName)}
}

func checkPTY() check {
	const name = "pty"
	if err := proxy.CheckPTY(); err != nil {
		return check{Name: name, Status: checkFail, Detail: err.Error()}
	}
	return check{Name: name, Status: checkOK, Detail: "a pty can be allocated"}
}

// inspectTerminal reads terminal state from the standard streams without changing any
// settings.
func inspectTerminal(env Env) terminalState {
	state := terminalState{TTY: isTTY(env.Stdin)}
	// A PTY with no configured size can report 0x0. Treat it as unknown; the relay uses its
	// default size.
	if cols, rows, err := proxy.NewTerminal(env.Stdin, env.Stdout).Size(); err == nil && cols > 0 && rows > 0 {
		state.Cols, state.Rows = cols, rows
	}
	// Echo belongs to the terminal, not the stream. Try each stream so `doctor < file` and
	// `doctor | cat` can still read it.
	for _, stream := range []any{env.Stdin, env.Stdout, env.Stderr} {
		f, ok := stream.(*os.File)
		if !ok {
			continue
		}
		if echo, err := proxy.TerminalEcho(f); err == nil {
			state.Echo, state.EchoSupported = echo, true
			break
		}
	}
	return state
}

// checkTerminal reports terminal availability and size. A missing terminal is not a failure
// because run works without one.
func checkTerminal(env Env, state terminalState) check {
	const name = "terminal"
	detail := fmt.Sprintf("stdin: %s, stdout: %s", ttyText(env.Stdin), ttyText(env.Stdout))
	if state.Cols == 0 {
		detail += fmt.Sprintf(", size unknown (agents get %dx%d)", proxy.DefaultCols, proxy.DefaultRows)
	} else {
		detail += fmt.Sprintf(", size %dx%d", state.Cols, state.Rows)
	}
	return check{Name: name, Status: checkOK, Detail: detail}
}

// checkEcho reports whether terminal echo has been left off.
//
// geminus uses raw mode only while relaying. A crash before restoration can leave echo off,
// hiding typed characters. doctor prints a one-line remedy rather than changing the
// setting.
func checkEcho(state terminalState) check {
	const name = "terminal echo"
	switch {
	case state.EchoSupported && state.Echo:
		return check{Name: name, Status: checkOK, Detail: "on"}
	case state.EchoSupported:
		detail := "off: typed characters are not shown (a program left the terminal in raw mode). Run: " + echoFix
		return check{Name: name, Status: checkWarn, Detail: detail}
	case runtime.GOOS == "windows":
		return check{Name: name, Status: checkOK, Detail: "unsupported on windows (not checked)"}
	default:
		return check{Name: name, Status: checkOK, Detail: "not checked (no terminal attached)"}
	}
}

func isTTY(stream any) bool {
	f, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func ttyText(stream any) string {
	if isTTY(stream) {
		return "tty"
	}
	return "not a tty"
}

// checkAgents lists backend detection results. Missing Claude and Hermes warrant only a
// warning because cmd still works.
func checkAgents() []check {
	var checks []check
	for _, v := range detectAgents() {
		name := "agent " + v.Name
		switch {
		case v.Binary == "":
			checks = append(checks, check{Name: name, Status: checkOK, Detail: "runs any command (no resume)"})
		case !v.Found:
			checks = append(checks, check{Name: name, Status: checkWarn, Detail: fmt.Sprintf("%q not found in PATH", v.Binary)})
		case v.Version == "":
			checks = append(checks, check{Name: name, Status: checkOK, Detail: v.Path + " (version unknown)"})
		default:
			checks = append(checks, check{Name: name, Status: checkOK, Detail: fmt.Sprintf("%s (%s)", v.Path, v.Version)})
		}
	}
	return checks
}
