//go:build unix

package proxy

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
)

// ptyRun は実際の PTY で /bin/sh を動かす。端末側はメモリ上の memTerminal。
type ptyRun struct {
	t    *testing.T
	term *memTerminal
	done chan runOutcome
}

func startPTY(t *testing.T, tty bool, script string, configure func(*Options)) *ptyRun {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available: %v", err)
	}
	p := &ptyRun{t: t, term: newMemTerminal(tty), done: make(chan runOutcome, 1)}
	opts := Options{
		Argv:     []string{"sh", "-c", script},
		Terminal: p.term,
		Signals:  make(chan os.Signal),
	}
	if configure != nil {
		configure(&opts)
	}
	// ハングしたらテストを止めずに子を kill して失敗させる。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	go func() {
		res, err := Run(ctx, opts)
		p.done <- runOutcome{res, err}
	}()
	t.Cleanup(func() {
		cancel()
		p.term.inW.Close()
	})
	return p
}

func (p *ptyRun) wait() runOutcome {
	p.t.Helper()
	select {
	case out := <-p.done:
		return out
	case <-time.After(30 * time.Second):
		p.t.Fatal("Run did not return")
		return runOutcome{}
	}
}

func (p *ptyRun) waitOutput(substr string) {
	p.t.Helper()
	waitFor(p.t, "output "+substr, func() bool { return strings.Contains(p.term.out.String(), substr) })
}

func TestPTYExitCode(t *testing.T) {
	for _, tt := range []struct {
		script string
		want   int
	}{
		{"exit 0", 0},
		{"exit 7", 7},
		{"exit 255", 255},
		{"kill -KILL $$", 128 + int(syscall.SIGKILL)},
	} {
		t.Run(tt.script, func(t *testing.T) {
			out := startPTY(t, false, tt.script, nil).wait()
			if out.err != nil {
				t.Fatalf("Run: %v", out.err)
			}
			if out.res.ExitCode != tt.want {
				t.Errorf("ExitCode = %d, want %d", out.res.ExitCode, tt.want)
			}
		})
	}
}

func TestPTYOutputIsRelayedAndRecordedIdentically(t *testing.T) {
	var record syncBuffer
	// 終了直前に書いた出力まで届くことを確かめるため、最後の printf の直後に exit する。
	p := startPTY(t, false, `printf 'hello \033[31mworld\033[0m'; printf ' tail'; exit 3`, func(o *Options) { o.Record = &record })
	out := p.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	const want = "hello \x1b[31mworld\x1b[0m tail"
	if got := p.term.out.String(); got != want {
		t.Errorf("terminal output = %q, want %q", got, want)
	}
	if got := record.String(); got != want {
		t.Errorf("record = %q, want %q", got, want)
	}
	if out.res.OutputBytes != int64(len(want)) || out.res.ExitCode != 3 {
		t.Errorf("result = %+v", out.res)
	}
}

// 大きな出力を出してすぐ終了しても、末尾まで欠けない。
func TestPTYLargeOutputIsNotTruncated(t *testing.T) {
	var record syncBuffer
	p := startPTY(t, false, `i=0; while [ $i -lt 2000 ]; do echo "line $i ................................................"; i=$((i+1)); done; echo END`, func(o *Options) { o.Record = &record })
	out := p.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	got := p.term.out.String()
	if !strings.HasSuffix(got, "END\r\n") {
		t.Errorf("output does not end with END: ...%q", got[max(0, len(got)-60):])
	}
	if n := strings.Count(got, "\n"); n != 2001 {
		t.Errorf("got %d lines, want 2001", n)
	}
	if record.String() != got {
		t.Error("record differs from the terminal output")
	}
	if out.res.OutputBytes != int64(len(got)) {
		t.Errorf("OutputBytes = %d, want %d", out.res.OutputBytes, len(got))
	}
}

func TestPTYChildSeesATerminal(t *testing.T) {
	p := startPTY(t, false, `if [ -t 0 ] && [ -t 1 ] && [ -t 2 ]; then echo is-a-tty; else echo not-a-tty; fi`, nil)
	out := p.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if got := p.term.out.String(); !strings.Contains(got, "is-a-tty") {
		t.Errorf("output = %q, want the child to see a tty on stdin/stdout/stderr", got)
	}
}

func TestPTYInputReachesChild(t *testing.T) {
	p := startPTY(t, false, `IFS= read -r line; echo "got:$line"`, nil)
	p.term.typeString(t, "ping\n")
	out := p.wait()
	if out.err != nil || out.res.ExitCode != 0 {
		t.Fatalf("Run: %+v, %v", out.res, out.err)
	}
	if got := p.term.out.String(); !strings.Contains(got, "got:ping") {
		t.Errorf("output = %q, want it to contain got:ping", got)
	}
}

// 非 TTY の入力が EOF になったら EOT が届き、cat が終了する（ハングしない）。
func TestPTYNonTTYInputEOFEndsCat(t *testing.T) {
	p := startPTY(t, false, `cat; echo "cat-exit:$?"`, nil)
	p.term.typeString(t, "abc\n")
	p.term.inW.Close()
	out := p.wait()
	if out.err != nil || out.res.ExitCode != 0 {
		t.Fatalf("Run: %+v, %v", out.res, out.err)
	}
	got := p.term.out.String()
	if !strings.Contains(got, "abc") || !strings.Contains(got, "cat-exit:0") {
		t.Errorf("output = %q", got)
	}
	if raw, _, _ := p.term.counts(); raw != 0 {
		t.Errorf("raw mode entered %d times for non-TTY input", raw)
	}
}

// 改行で終わらない入力（printf abc | geminus run ...）でも cat が終了する。
func TestPTYNonTTYInputWithoutTrailingNewlineEndsCat(t *testing.T) {
	p := startPTY(t, false, `cat; echo "cat-exit:$?"`, nil)
	p.term.typeString(t, "abc")
	p.term.inW.Close()
	out := p.wait()
	if out.err != nil || out.res.ExitCode != 0 {
		t.Fatalf("Run: %+v, %v", out.res, out.err)
	}
	got := p.term.out.String()
	if !strings.Contains(got, "abc") || !strings.Contains(got, "cat-exit:0") {
		t.Errorf("output = %q", got)
	}
}

// 入力が空（</dev/null）でも、標準入力を読む子は EOF を受けて終了する。
func TestPTYNonTTYEmptyInputEndsCat(t *testing.T) {
	p := startPTY(t, false, `cat; echo "cat-exit:$?"`, nil)
	p.term.inW.Close()
	out := p.wait()
	if out.err != nil || out.res.ExitCode != 0 {
		t.Fatalf("Run: %+v, %v", out.res, out.err)
	}
	if got := p.term.out.String(); !strings.Contains(got, "cat-exit:0") {
		t.Errorf("output = %q", got)
	}
}

func TestCheckPTY(t *testing.T) {
	if err := CheckPTY(); err != nil {
		t.Fatalf("CheckPTY: %v", err)
	}
}

func TestPTYInitialSizeAndResize(t *testing.T) {
	p := startPTY(t, true, `stty size; trap 'stty size; exit 0' WINCH; echo ready; while :; do sleep 0.05; done`, nil)
	p.waitOutput("ready")
	if got := p.term.out.String(); !strings.Contains(got, "30 100") {
		t.Fatalf("output = %q, want the initial size 30 100", got)
	}

	p.term.resize(132, 43)
	out := p.wait()
	if out.err != nil || out.res.ExitCode != 0 {
		t.Fatalf("Run: %+v, %v", out.res, out.err)
	}
	if got := p.term.out.String(); !strings.Contains(got, "43 132") {
		t.Errorf("output = %q, want the resized size 43 132", got)
	}
	if raw, restores, _ := p.term.counts(); raw != 1 || restores != 1 {
		t.Errorf("raw = %d, restores = %d; want 1, 1", raw, restores)
	}
}

func TestPTYSignalIsForwardedToChildProcessGroup(t *testing.T) {
	signals := make(chan os.Signal)
	p := startPTY(t, true, `trap 'echo got-term; exit 42' TERM; echo ready; while :; do sleep 0.05; done`, func(o *Options) { o.Signals = signals })
	p.waitOutput("ready")

	signals <- syscall.SIGTERM
	out := p.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if out.res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", out.res.ExitCode)
	}
	if got := p.term.out.String(); !strings.Contains(got, "got-term") {
		t.Errorf("output = %q", got)
	}
}

func TestPTYContextCancelKillsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	term := newMemTerminal(true)
	defer term.inW.Close()
	done := make(chan runOutcome, 1)
	go func() {
		res, err := Run(ctx, Options{Argv: []string{"sh", "-c", "echo ready; sleep 60"}, Terminal: term, Signals: make(chan os.Signal)})
		done <- runOutcome{res, err}
	}()
	waitFor(t, "ready", func() bool { return strings.Contains(term.out.String(), "ready") })

	cancel()
	select {
	case out := <-done:
		if out.res.ExitCode != 128+int(syscall.SIGKILL) {
			t.Errorf("ExitCode = %d, want %d", out.res.ExitCode, 128+int(syscall.SIGKILL))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if _, restores, _ := term.counts(); restores != 1 {
		t.Errorf("restores = %d, want 1", restores)
	}
}

// バックグラウンドの孫が PTY を握ったままでも、子の終了で Run が返る。
func TestPTYReturnsWhileGrandchildHoldsThePTY(t *testing.T) {
	p := startPTY(t, false, `sleep 30 & echo started; exit 4`, nil)
	start := time.Now()
	out := p.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if out.res.ExitCode != 4 {
		t.Errorf("ExitCode = %d, want 4", out.res.ExitCode)
	}
	if !strings.Contains(p.term.out.String(), "started") {
		t.Errorf("output = %q", p.term.out.String())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run took %v, want it not to wait for the grandchild", elapsed)
	}
}

func TestPTYWorkingDirectoryAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	p := startPTY(t, false, `pwd -P; echo "var=$GEMINUS_TEST_VAR"`, func(o *Options) {
		o.Dir = dir
		o.Env = append(os.Environ(), "GEMINUS_TEST_VAR=from-geminus")
	})
	out := p.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	got := p.term.out.String()
	if !strings.Contains(got, strings.TrimPrefix(dir, "/private")) || !strings.Contains(got, "var=from-geminus") {
		t.Errorf("output = %q, want cwd %s and the variable", got, dir)
	}
}

func TestPTYInheritsEnvironmentByDefault(t *testing.T) {
	t.Setenv("GEMINUS_TEST_INHERIT", "inherited")
	p := startPTY(t, false, `echo "var=$GEMINUS_TEST_INHERIT"`, nil)
	out := p.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if got := p.term.out.String(); !strings.Contains(got, "var=inherited") {
		t.Errorf("output = %q", got)
	}
}

func TestPTYMissingBinary(t *testing.T) {
	term := newMemTerminal(true)
	defer term.inW.Close()
	_, err := Run(context.Background(), Options{
		Argv:     []string{"geminus-no-such-binary-xyz"},
		Terminal: term,
		Signals:  make(chan os.Signal),
	})
	if !errors.Is(err, ErrStart) {
		t.Fatalf("err = %v, want ErrStart", err)
	}
	if errors.Is(err, ErrPTY) {
		t.Errorf("err = %v must not be reported as a pty failure", err)
	}
	if raw, restores, _ := term.counts(); raw != 1 || restores != 1 {
		t.Errorf("raw = %d, restores = %d; want the terminal restored", raw, restores)
	}
}

func TestPTYChildCloseIsIdempotent(t *testing.T) {
	child, err := PTYSpawner().Spawn(Spec{Argv: []string{"sh", "-c", "exit 2"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	go io.Copy(io.Discard, child)
	code, err := child.Wait()
	if err != nil || code != 2 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	// 終了後のシグナルと kill は何もしない（回収済みの PID に送らない）。
	if err := child.Signal(syscall.SIGTERM); err != nil {
		t.Errorf("Signal after exit: %v", err)
	}
	if err := child.Kill(); err != nil {
		t.Errorf("Kill after exit: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := child.Close(); err != nil {
			t.Errorf("Close #%d: %v", i+1, err)
		}
	}
}

func TestPTYSpawnerRejectsEmptyArgv(t *testing.T) {
	if _, err := PTYSpawner().Spawn(Spec{}); !errors.Is(err, ErrNoCommand) {
		t.Errorf("err = %v, want ErrNoCommand", err)
	}
}

// 利用者側の端末も実際の PTY にして、raw mode・サイズ・SIGWINCH の経路を通す。
func TestOSTerminalOnRealPTY(t *testing.T) {
	pty, err := gopty.New()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer pty.Close()
	unix, ok := pty.(gopty.UnixPty)
	if !ok {
		t.Skip("not a unix pty")
	}
	if err := pty.Resize(90, 33); err != nil {
		t.Fatal(err)
	}
	slave := unix.Slave()
	term := NewTerminal(slave, slave)

	if !term.IsTTY() {
		t.Fatal("IsTTY = false for a pty slave")
	}
	cols, rows, err := term.Size()
	if err != nil || cols != 90 || rows != 33 {
		t.Errorf("Size = %d, %d, %v; want 90, 33", cols, rows, err)
	}

	restore, err := term.MakeRaw()
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	// raw mode では改行を待たずに1バイトずつ読める。
	if _, err := pty.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 8)
		n, _ := term.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if s != "a" {
			t.Errorf("read %q in raw mode, want %q", s, "a")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read blocked: the terminal is not in raw mode")
	}
	for i := 0; i < 2; i++ {
		if err := restore(); err != nil {
			t.Errorf("restore #%d: %v", i+1, err)
		}
	}

	notified := make(chan struct{}, 4)
	stop := term.WatchResize(func() { notified <- struct{}{} })
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Error("SIGWINCH did not trigger a resize notification")
	}
	stop()
}

// Signals を渡さない既定の経路: geminus が受けた SIGTERM が子へ届く。
func TestPTYForwardsRealSignals(t *testing.T) {
	// 転送の登録より先にシグナルが届いてもテストプロセスが死なないようにする。
	guard := make(chan os.Signal, 16)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	p := startPTY(t, true, `trap 'exit 42' TERM; echo ready; while :; do sleep 0.05; done`, func(o *Options) { o.Signals = nil })
	p.waitOutput("ready")

	var out runOutcome
	for sent := false; !sent; {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case out = <-p.done:
			sent = true
		case <-time.After(100 * time.Millisecond):
		}
	}
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if out.res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", out.res.ExitCode)
	}
}

// シグナルは子だけでなく、同じプロセスグループの孫にも届く。
func TestPTYSignalReachesGrandchild(t *testing.T) {
	signals := make(chan os.Signal)
	script := `sh -c 'trap "echo grandchild-term; exit 0" TERM; echo grandchild-ready; while :; do sleep 0.05; done' &
trap 'wait; exit 42' TERM
echo ready
wait`
	p := startPTY(t, true, script, func(o *Options) { o.Signals = signals })
	p.waitOutput("ready")
	p.waitOutput("grandchild-ready")

	signals <- syscall.SIGTERM
	out := p.wait()
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	if out.res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", out.res.ExitCode)
	}
	if got := p.term.out.String(); !strings.Contains(got, "grandchild-term") {
		t.Errorf("output = %q, want the grandchild to receive SIGTERM", got)
	}
}

// パス区切りを含む相対パスは、子の作業ディレクトリから解決する。
func TestPTYRelativeCommandResolvesAgainstDir(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "agent.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho from-agent\nexit 6\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	term := newMemTerminal(false)
	defer term.inW.Close()
	res, err := Run(context.Background(), Options{
		Argv:     []string{"./agent.sh"},
		Dir:      dir,
		Terminal: term,
		Signals:  make(chan os.Signal),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 6 || !strings.Contains(term.out.String(), "from-agent") {
		t.Errorf("result = %+v, output = %q", res, term.out.String())
	}
}
