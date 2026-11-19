package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/term"

	"github.com/hauntedfail/geminus/internal/agent"
	"github.com/hauntedfail/geminus/internal/session"
)

// askResult は `ask --json` の出力。
type askResult struct {
	Session  string `json:"session"`
	Agent    string `json:"agent"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

func runAsk(inv Invocation) int {
	const name = "ask"
	opts := inv.Options.Ask
	prompt := strings.TrimSpace(strings.Join(inv.Args, " "))
	if prompt == "" {
		return usageError(inv.Env, name, "missing prompt")
	}
	if opts.Session != "" && opts.Continue {
		return usageError(inv.Env, name, "--session and --continue cannot be used together")
	}
	store, err := openStore(inv)
	if err != nil {
		return fail(inv.Env, name, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(inv.Env, name, fmt.Errorf("resolve working directory: %w", err))
	}

	t, err := askTarget(store, inv, cwd, prompt)
	if err != nil {
		return report(inv.Env, name, err)
	}
	launch, err := prepareAsk(t, cwd, prompt)
	if err != nil {
		if t.created {
			err = errors.Join(err, rollback(store, t))
		}
		return report(inv.Env, name, err)
	}
	announceDir(inv.Env, name, launch.dir, cwd)
	if logf := verboseLogf(inv); logf != nil {
		logf("session %s", t.meta.ID)
		for _, note := range launch.start.notes {
			logf("%s", note)
		}
	}

	active, err := begin(store, t, launch.start)
	if err != nil {
		return fail(inv.Env, name, err)
	}
	return exchange(inv, active, launch, prompt)
}

// askTarget は ask の対象セッションを決める。
// --session は指定の id、--continue はこのディレクトリで最後に更新されたもの、どちらも無ければ新規。
func askTarget(store *session.Store, inv Invocation, cwd, prompt string) (target, error) {
	opts := inv.Options.Ask
	id := opts.Session
	if opts.Continue {
		latest, err := latestAskable(store, inv, cwd)
		if err != nil {
			return target{}, err
		}
		id = latest
	}
	if id != "" {
		t, err := loadTarget(store, id, inv.Options.Agent, false)
		if err != nil {
			return target{}, err
		}
		return t, requireOneshot(t.adapter)
	}

	name, err := defaultAgentName(inv, store.Home())
	if err != nil {
		return target{}, err
	}
	adapter, err := lookupAdapter(name)
	if err != nil {
		return target{}, err
	}
	// セッションを作る前に、起動できないことが分かる場合は止める。
	if err := requireOneshot(adapter); err != nil {
		return target{}, err
	}
	if _, err := resolveBinary([]string{adapter.Binary()}); err != nil {
		return target{}, err
	}
	return createTarget(store, adapter, truncateTitle(prompt), cwd)
}

func requireOneshot(adapter agent.Adapter) error {
	if !adapter.Capabilities().Oneshot {
		return fmt.Errorf("agent %q: %w", adapter.Name(), agent.ErrNoOneshot)
	}
	return nil
}

// latestAskable は --continue の対象を選ぶ。cwd で作られ、ワンショットに対応した
// バックエンドのセッションのうち、最後に更新されたものの id を返す。
// --agent があれば、そのバックエンドのセッションに限る。
func latestAskable(store *session.Store, inv Invocation, cwd string) (string, error) {
	metas, listErr := store.List()
	if listErr != nil {
		if logf := verboseLogf(inv); logf != nil {
			logf("skipping unreadable sessions: %v", listErr)
		}
	}
	// List は updated_at の降順で返す。
	for _, meta := range metas {
		if inv.Options.Agent != "" && meta.Agent != inv.Options.Agent {
			continue
		}
		adapter, err := agent.Lookup(meta.Agent)
		if err != nil || !adapter.Capabilities().Oneshot {
			continue
		}
		if sameDir(meta.Cwd, cwd) {
			return meta.ID, nil
		}
	}
	return "", fmt.Errorf("--continue: no session to continue in %s", cwd)
}

// askLaunch は子の起動に必要な内容。
type askLaunch struct {
	start startParams
	// exe は argv[0] を PATH で解決したパス。
	exe string
	dir string
}

func prepareAsk(t target, cwd, prompt string) (askLaunch, error) {
	plan, err := planKey(t, false)
	if err != nil {
		return askLaunch{}, err
	}
	argv, err := t.adapter.Oneshot(agent.Request{Key: plan.key, Resume: plan.resume}, prompt)
	if err != nil {
		if errors.Is(err, agent.ErrNoPrompt) || errors.Is(err, agent.ErrInvalidPrompt) {
			return askLaunch{}, &usageErr{msg: err.Error()}
		}
		return askLaunch{}, err
	}
	resolved, err := resolveBinary(argv)
	if err != nil {
		return askLaunch{}, err
	}
	dir, err := workDir(t, cwd)
	if err != nil {
		return askLaunch{}, err
	}
	return askLaunch{
		start: startParams{key: plan.key, argv: argv, notes: plan.notes, record: true},
		exe:   resolved[0],
		dir:   dir,
	}, nil
}

// exchange は子をパイプで動かして1往復し、結果を記録して終了コードを返す。
func exchange(inv Invocation, active *activeSession, launch askLaunch, prompt string) int {
	const name = "ask"
	if _, err := active.log.AppendEvent(session.InputEvent(prompt)); err != nil {
		return fail(inv.Env, name, active.abort(err))
	}

	record := &lenientWriter{w: active.log}
	var captured bytes.Buffer
	var visible io.Writer = inv.Env.Stdout
	if inv.Options.JSON {
		// --json では stdout に JSON 1個だけを出すので、返答は溜めておく。
		visible = &captured
	}

	cmd := exec.Command(launch.exe, launch.start.argv[1:]...)
	cmd.Dir = launch.dir
	cmd.Stdin = childStdin(inv.Env.Stdin)
	cmd.Stdout = io.MultiWriter(visible, record)
	cmd.Stderr = inv.Env.Stderr
	// 起動の前から受ける。起動直後に届いたシグナルで geminus だけが先に終了しないようにする。
	signals := catchSignals()
	if err := cmd.Start(); err != nil {
		signal.Stop(signals)
		return fail(inv.Env, name, active.abort(fmt.Errorf("start %s: %w", launch.exe, err)))
	}
	stopSignals := forwardSignals(signals, cmd.Process)
	waitErr := cmd.Wait()
	stopSignals()

	code := processExitCode(cmd.ProcessState)
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		warn(inv.Env, name, fmt.Errorf("wait for %s: %w", active.target.adapter.Name(), waitErr))
	}
	if err := record.firstError(); err != nil {
		warn(inv.Env, name, fmt.Errorf("recording stopped: %w", err))
	}
	if err := active.finish(code, 1); err != nil {
		warn(inv.Env, name, fmt.Errorf("save session: %w", err))
	}
	if inv.Options.JSON {
		result := askResult{Session: active.target.meta.ID, Agent: active.target.adapter.Name(), ExitCode: code, Output: captured.String()}
		if err := writeJSON(inv.Env.Stdout, result); err != nil {
			return fail(inv.Env, name, err)
		}
	}
	return childExitCode(code)
}

// lenientWriter は書き込みの失敗を呼び出し側に返さない。
// 記録の失敗で返答の表示（io.MultiWriter のもう片方）を止めないため。
// 最初のエラーを覚え、以後は書かない。
type lenientWriter struct {
	w io.Writer

	mu  sync.Mutex
	err error
}

func (l *lenientWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		_, l.err = l.w.Write(p)
	}
	return len(p), nil
}

func (l *lenientWriter) firstError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// childStdin は ask の子に渡す標準入力を返す。
//
// パイプやファイルはそのまま渡す（`cat notes.txt | geminus ask "要約して"`）。
// 端末は渡さない。ask は1往復で終わる非対話の実行で、プロンプトは引数で渡している。
// 端末を渡すと、標準入力を読もうとするエージェントが入力待ちで止まる。
func childStdin(stdin io.Reader) io.Reader {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return nil
	}
	return stdin
}

// catchSignals は、子を待つ間に届いたシグナルで geminus が先に終了しないようにする。
// 先に終了すると、終了コードとセッションの状態を記録できない。
// 受けたシグナルは、forwardSignals が読み始めるまでチャネルに溜まる。
func catchSignals() chan os.Signal {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	return ch
}

// forwardSignals は ch に届いたシグナルを処理する。返す関数は受け取りをやめる。
//
// 端末の Ctrl-C と Ctrl-\ は、同じプロセスグループにいる子にも届くので転送しない。
// SIGTERM と SIGHUP は geminus だけに届くことがあるので、子へ転送する。
func forwardSignals(ch chan os.Signal, child *os.Process) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case sig := <-ch:
				if sig == syscall.SIGTERM || sig == syscall.SIGHUP {
					// 失敗は子が既に終了したことを意味する。Wait がその結果を返す。
					_ = child.Signal(sig)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
		wg.Wait()
	}
}

// processExitCode は終了コードを返す。Unix でシグナルにより終了した場合は 128+番号にする。
// 状態が取れなければ -1。
func processExitCode(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}
