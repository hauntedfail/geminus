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

// maxInputRecord limits the amount of non-TTY input saved to the transcript.
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

// busyHint adds a --force hint to errors about sessions already in use.
func busyHint(err error) error {
	if errors.Is(err, errBusy) {
		return fmt.Errorf("%w (use --force to open it anyway)", err)
	}
	return err
}

// runTarget selects an existing session for --resume or a new one otherwise.
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
	// Fail before creating a session if we already know the agent cannot start.
	if adapter.Name() == agent.GenericName && len(inv.Args) == 0 {
		return target{}, usagef("agent %q needs a command: %s run --agent %s -- <argv...>", name, programName, name)
	}
	if binary := adapter.Binary(); binary != "" {
		if _, err := resolveBinary([]string{binary}); err != nil {
			return target{}, err
		}
	}
	title := opts.Title
	// With --no-record, do not save the prompt or command line as the title in session.json.
	if title == "" && !opts.NoRecord {
		title = defaultTitle(adapter, inv.Args)
	}
	return createTarget(store, adapter, title, cwd)
}

// runLaunch holds the inputs needed to start the child.
type runLaunch struct {
	start startParams
	dir   string
}

// prepareRun selects the session key, builds argv, and verifies that the executable exists.
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

// relay runs the child on a PTY, records the result, and returns the exit code.
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
			// Record piped input in the transcript because it does not appear on screen.
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
		// The child never ran. Do not record a successful exit.
		return fail(inv.Env, name, active.abort(err))
	}
	if res.RecordErr != nil {
		warn(inv.Env, name, fmt.Errorf("recording stopped: %w", res.RecordErr))
	}
	if finishErr := active.finish(res.ExitCode, 0); finishErr != nil {
		warn(inv.Env, name, fmt.Errorf("save session: %w", finishErr))
	}
	if err != nil {
		// The child ran, but the relay failed (for example, while writing to the terminal).
		return fail(inv.Env, name, err)
	}
	if interactive || inv.Options.Verbose {
		printRunSummary(inv.Env, active.target, res.ExitCode)
	}
	return childExitCode(res.ExitCode)
}

// printRunSummary writes resume instructions to stderr after an interactive session ends.
func printRunSummary(env Env, t target, code int) {
	id := t.meta.ID
	if !t.adapter.Capabilities().Resume {
		fmt.Fprintf(env.Stderr, "%s: session %s ended (exit %d)\n", programName, id, code)
		return
	}
	fmt.Fprintf(env.Stderr, "%s: session %s ended (exit %d). Resume with: %s run --resume %s\n", programName, id, code, programName, id)
}

// inputTap copies non-TTY input as it passes through the relay and records it as an input
// event.
//
// Write the event at EOF. If the child exits first, flush after the relay finishes to
// record what has been read so far. Limit the copy to maxInputRecord.
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
		// Do not interrupt the relay on a recording failure. flush remembers the result and returns
		// it when called after the relay finishes.
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

// flush writes the buffered input as an input event once. Subsequent calls return the
// original result.
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
