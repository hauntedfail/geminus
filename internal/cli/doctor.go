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

// 診断1件の結果。fail が1つでもあれば doctor は ExitError を返す。
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
)

// sessionsDirName は状態ディレクトリの下でセッションを置くディレクトリ名（internal/session と同じ）。
const sessionsDirName = "sessions"

// check は診断1件。
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// echoFix は echo が切れたままの端末を直すコマンド。doctor は案内するだけで実行しない。
const echoFix = "stty sane"

// terminalState は doctor を実行した端末の状態。
type terminalState struct {
	// TTY は標準入力が端末かどうか。
	TTY bool `json:"tty"`
	// Cols と Rows は端末のサイズ。取れなければ 0。
	Cols int `json:"cols"`
	Rows int `json:"rows"`
	// Echo は端末が入力を echo する設定かどうか。EchoSupported が false なら意味を持たない。
	Echo bool `json:"echo"`
	// EchoSupported は Echo を読めたかどうか。
	// Windows と、標準入出力のどれも端末でない場合は false。
	EchoSupported bool `json:"echo_supported"`
}

// doctorReport は `doctor --json` の出力。
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

// checkHome は状態ディレクトリと config.json を調べる。ディレクトリは作らない。
// 既にあるディレクトリには、書けることを確かめるための一時ファイルを作ってすぐ消す。
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

// checkStateDir は dir が自分だけの書けるディレクトリかを調べる。
// まだ無いディレクトリは、最初の run で作るので正常とする。
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
	// Windows の権限ビットは ACL を表さないので、見ても意味が無い。
	if runtime.GOOS != "windows" && mode&0o077 != 0 {
		detail := fmt.Sprintf("%s has mode %04o; others can read it. Sessions may hold secrets: chmod 700", dir, mode)
		return check{Name: name, Status: checkWarn, Detail: detail}
	}
	return check{Name: name, Status: checkOK, Detail: fmt.Sprintf("%s (mode %04o)", dir, mode)}
}

// probeWritable は dir に一時ファイルを作って消し、書けることを確かめる。
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

// inspectTerminal は標準入出力から端末の状態を読む。読むだけで、設定は変えない。
func inspectTerminal(env Env) terminalState {
	state := terminalState{TTY: isTTY(env.Stdin)}
	// 0x0 を返す端末もある（サイズ未設定の PTY）。中継は既定のサイズを使うので、不明として扱う。
	if cols, rows, err := proxy.NewTerminal(env.Stdin, env.Stdout).Size(); err == nil && cols > 0 && rows > 0 {
		state.Cols, state.Rows = cols, rows
	}
	// echo は端末そのものの設定なので、端末を指している口のどれから読んでも同じ。
	// `doctor < file` や `doctor | cat` でも読めるよう、順に試す。
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

// checkTerminal は標準入出力が端末かどうかとサイズを報告する。
// 端末でなくても run は動くので、失敗にはしない。
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

// checkEcho は端末の echo が切れたままになっていないかを報告する。
//
// geminus は中継の間だけ端末を raw mode にする。復元せずに落ちると echo が切れたまま残り、
// 打った文字が見えなくなる。doctor は直さず、直し方を1行で案内する。
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

// checkAgents はバックエンドの検出結果を並べる。
// claude も hermes も無くても cmd は使えるので、未検出は警告にとどめる。
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
