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

// askResult is the output of `ask --json`.
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

// askTarget selects the session for ask. Use the ID from --session, the most recently
// updated session in this directory for --continue, or a new session if neither is set.
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
	// Fail before creating a session if we already know the agent cannot start.
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

// latestAskable returns the most recently updated session created in cwd whose backend
// supports one-shot requests. If --agent is set, consider only that backend.
func latestAskable(store *session.Store, inv Invocation, cwd string) (string, error) {
	metas, listErr := store.List()
	if listErr != nil {
		if logf := verboseLogf(inv); logf != nil {
			logf("skipping unreadable sessions: %v", listErr)
		}
	}
	// List returns sessions in descending updated_at order.
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

// askLaunch holds the inputs needed to start the child.
type askLaunch struct {
	start startParams
	// exe is argv[0] resolved against PATH.
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

// exchange runs one exchange with the child over pipes, records the result, and returns the
// exit code.
func exchange(inv Invocation, active *activeSession, launch askLaunch, prompt string) int {
	const name = "ask"
	if _, err := active.log.AppendEvent(session.InputEvent(prompt)); err != nil {
		return fail(inv.Env, name, active.abort(err))
	}

	record := &lenientWriter{w: active.log}
	var captured bytes.Buffer
	var visible io.Writer = inv.Env.Stdout
	if inv.Options.JSON {
		// Buffer the reply so --json writes a single JSON value to stdout.
		visible = &captured
	}

	cmd := exec.Command(launch.exe, launch.start.argv[1:]...)
	cmd.Dir = launch.dir
	cmd.Stdin = childStdin(inv.Env.Stdin)
	cmd.Stdout = io.MultiWriter(visible, record)
	cmd.Stderr = inv.Env.Stderr
	// Start listening before launch so a signal arriving just afterward cannot terminate
	// geminus while leaving the child running.
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

// lenientWriter saves the first write error and skips subsequent writes without returning
// the error to its caller. A recording failure must not stop io.MultiWriter from displaying
// the reply.
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

// childStdin returns the input stream to pass to the ask child.
//
// Pass pipes and files through unchanged (`cat notes.txt | geminus ask "Summarize these notes"`). Do not
// pass a terminal: ask is noninteractive and supplies its single prompt as an argument. An
// agent reading from a terminal could block waiting for input.
func childStdin(stdin io.Reader) io.Reader {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return nil
	}
	return stdin
}

// catchSignals keeps geminus alive while it waits for the child so it can record the exit
// code and session state. The channel buffers signals until forwardSignals starts reading.
func catchSignals() chan os.Signal {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	return ch
}

// forwardSignals handles signals from ch. Call the returned function to stop listening.
//
// Terminal Ctrl-C and Ctrl-\ already reach the child in the same process group. Forward
// SIGTERM and SIGHUP because they may reach only geminus.
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
					// Failure means the child has already exited; Wait returns its status.
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

// processExitCode returns the exit code, or 128 plus the signal number on Unix for signal
// termination. It returns -1 if the status is unavailable.
func processExitCode(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}
