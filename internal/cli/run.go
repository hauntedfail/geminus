package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/hauntedfail/geminus/internal/agent"
	"github.com/hauntedfail/geminus/internal/proxy"
	"github.com/hauntedfail/geminus/internal/session"
)

// maxInputRecord は、非 TTY の入力のうち transcript に残す上限。
const maxInputRecord = 64 << 10

func runRun(inv Invocation) int {
	const name = "run"
	opts := inv.Options.Run
	if opts.Fresh && opts.Resume == "" {
		return usageError(inv.Env, name, "--fresh requires --resume")
	}
	store, err := openStore(inv)
	if err != nil {
		return fail(inv.Env, name, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(inv.Env, name, fmt.Errorf("resolve working directory: %w", err))
	}

	t, err := runTarget(store, inv, cwd)
	if err != nil {
		return report(inv.Env, name, busyHint(err))
	}
	launch, err := prepareRun(t, inv, cwd)
	if err != nil {
		if t.created {
			err = errors.Join(err, rollback(store, t))
		}
		return report(inv.Env, name, err)
	}
	for _, note := range launch.start.notes {
		fmt.Fprintf(inv.Env.Stderr, "%s %s: %s\n", programName, name, note)
	}
	announceDir(inv.Env, name, launch.dir, cwd)

	active, err := begin(store, t, launch.start)
	if err != nil {
		return fail(inv.Env, name, busyHint(err))
	}
	return relay(inv, active, launch)
}

// busyHint は、開かれているセッションのエラーに --force の案内を足す。
func busyHint(err error) error {
	if errors.Is(err, errBusy) {
		return fmt.Errorf("%w (use --force to open it anyway)", err)
	}
	return err
}

// runTarget は run の対象セッションを決める。--resume があれば既存、無ければ新規。
func runTarget(store *session.Store, inv Invocation, cwd string) (target, error) {
	opts := inv.Options.Run
	if opts.Resume != "" {
		return loadTarget(store, opts.Resume, inv.Options.Agent, opts.Force)
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
	if adapter.Name() == agent.GenericName && len(inv.Args) == 0 {
		return target{}, usagef("agent %q needs a command: %s run --agent %s -- <argv...>", name, programName, name)
	}
	if binary := adapter.Binary(); binary != "" {
		if _, err := resolveBinary([]string{binary}); err != nil {
			return target{}, err
		}
	}
	title := opts.Title
	// --no-record では、プロンプトやコマンド行をタイトルとして session.json に残さない。
	if title == "" && !opts.NoRecord {
		title = defaultTitle(adapter, inv.Args)
	}
	return createTarget(store, adapter, title, cwd)
}

// runLaunch は子の起動に必要な内容。
type runLaunch struct {
	start startParams
	dir   string
}

// prepareRun は継続キーと argv を決め、バイナリがあることを確かめる。
func prepareRun(t target, inv Invocation, cwd string) (runLaunch, error) {
	opts := inv.Options.Run
	plan, err := planKey(t, opts.Fresh)
	if err != nil {
		return runLaunch{}, err
	}
	argv, err := t.adapter.Interactive(agent.Request{Key: plan.key, Resume: plan.resume, Args: inv.Args})
	if err != nil {
		if errors.Is(err, agent.ErrNoCommand) || errors.Is(err, agent.ErrInvalidPrompt) {
			return runLaunch{}, &usageErr{msg: err.Error()}
		}
		return runLaunch{}, err
	}
	if _, err := resolveBinary(argv); err != nil {
		return runLaunch{}, err
	}
	dir, err := workDir(t, cwd)
	if err != nil {
		return runLaunch{}, err
	}
	title := ""
	if !t.created {
		title = opts.Title
	}
	return runLaunch{
		start: startParams{key: plan.key, title: title, argv: argv, notes: plan.notes, record: !opts.NoRecord, force: opts.Force},
		dir:   dir,
	}, nil
}

// relay は子を PTY 上で動かし、終わったら結果を記録して終了コードを返す。
func relay(inv Invocation, active *activeSession, launch runLaunch) int {
	const name = "run"
	term := proxy.NewTerminal(inv.Env.Stdin, inv.Env.Stdout)
	interactive := term.IsTTY()

	popts := proxy.Options{
		Argv:     launch.start.argv,
		Dir:      launch.dir,
		Terminal: term,
		Logf:     verboseLogf(inv),
	}
	var tap *inputTap
	if active.record {
		popts.Record = active.log
		if !interactive {
			// パイプで渡された入力は画面に残らないので、transcript に残す。
			tap = &inputTap{Terminal: term, log: active.log}
			popts.Terminal = tap
		}
	}

	res, err := proxy.Run(context.Background(), popts)
	if tap != nil {
		if tapErr := tap.flush(); tapErr != nil {
			warn(inv.Env, name, fmt.Errorf("record input: %w", tapErr))
		}
	}
	if err != nil && !res.Started {
		// 子は一度も動いていない。終了コード 0 の実行として記録しない。
		return fail(inv.Env, name, active.abort(err))
	}
	if res.RecordErr != nil {
		warn(inv.Env, name, fmt.Errorf("recording stopped: %w", res.RecordErr))
	}
	if finishErr := active.finish(res.ExitCode, 0); finishErr != nil {
		warn(inv.Env, name, fmt.Errorf("save session: %w", finishErr))
	}
	if err != nil {
		// 子は動いたが、中継が壊れた（端末への書き込み失敗など）。
		return fail(inv.Env, name, err)
	}
	if interactive || inv.Options.Verbose {
		printRunSummary(inv.Env, active.target, res.ExitCode)
	}
	return childExitCode(res.ExitCode)
}

// printRunSummary は、対話セッションの終わりに再開方法を stderr へ出す。
func printRunSummary(env Env, t target, code int) {
	id := t.meta.ID
	if !t.adapter.Capabilities().Resume {
		fmt.Fprintf(env.Stderr, "%s: session %s ended (exit %d)\n", programName, id, code)
		return
	}
	fmt.Fprintf(env.Stderr, "%s: session %s ended (exit %d). Resume with: %s run --resume %s\n", programName, id, code, programName, id)
}

// inputTap は非 TTY の入力を中継しながら写しを取り、input イベントとして残す。
//
// 入力が終わった時点（EOF）で書く。子が先に終了した場合は、中継の後で flush を呼んで
// それまでに読んだ分を書く。写しは maxInputRecord で打ち切る。
type inputTap struct {
	proxy.Terminal
	log *session.Log

	mu        sync.Mutex
	buf       []byte
	truncated bool
	flushed   bool
	flushErr  error
}

func (t *inputTap) Read(p []byte) (int, error) {
	n, err := t.Terminal.Read(p)
	if n > 0 {
		t.capture(p[:n])
	}
	if err != nil {
		// 記録の失敗で中継は止めない。結果は flush が覚えていて、中継の後の flush が返す。
		_ = t.flush()
	}
	return n, err
}

func (t *inputTap) capture(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.flushed {
		return
	}
	room := maxInputRecord - len(t.buf)
	if len(p) > room {
		p = p[:room]
		t.truncated = true
	}
	t.buf = append(t.buf, p...)
}

// flush は溜めた入力を input イベントとして書く。
// 書くのは最初の1回だけで、2回目以降はそのときの結果を返す。
func (t *inputTap) flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.flushed {
		t.flushed = true
		t.flushErr = t.write()
	}
	return t.flushErr
}

func (t *inputTap) write() error {
	if len(t.buf) == 0 {
		return nil
	}
	if _, err := t.log.AppendEvent(session.InputEvent(string(t.buf))); err != nil {
		return err
	}
	if !t.truncated {
		return nil
	}
	note := fmt.Sprintf("input was longer than %d bytes; the rest was not recorded", maxInputRecord)
	_, err := t.log.AppendEvent(session.NoteEvent(note))
	return err
}
