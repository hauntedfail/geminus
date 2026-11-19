package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hauntedfail/geminus/internal/agent"
	"github.com/hauntedfail/geminus/internal/session"
)

// errBusy indicates that another process has the session open.
var errBusy = errors.New("session is already open")

// target pairs the session to launch with its adapter.
type target struct {
	meta    session.Meta
	adapter agent.Adapter
	// created marks a session newly created by this process. Remove it if launch fails rather
	// than leaving an empty session behind.
	created bool
}

// loadTarget prepares to open an existing session. A nonempty agentFlag must match its
// backend. If another process has it open, return errBusy unless force is set.
func loadTarget(store *session.Store, id, agentFlag string, force bool) (target, error) {
	meta, err := store.Load(id)
	if err != nil {
		return target{}, err
	}
	if agentFlag != "" && agentFlag != meta.Agent {
		return target{}, fmt.Errorf("session %s was started with agent %q, not %q", id, meta.Agent, agentFlag)
	}
	adapter, err := agent.Lookup(meta.Agent)
	if err != nil {
		return target{}, fmt.Errorf("session %s: %w", id, err)
	}
	if meta.Active() && !force {
		return target{}, fmt.Errorf("session %s: %w in process %d", id, errBusy, meta.PID)
	}
	return target{meta: meta, adapter: adapter}, nil
}

// createTarget creates a session for adapter.
func createTarget(store *session.Store, adapter agent.Adapter, title, cwd string) (target, error) {
	meta, err := store.Create(session.CreateParams{Title: title, Agent: adapter.Name(), Cwd: cwd})
	if err != nil {
		return target{}, err
	}
	return target{meta: meta, adapter: adapter, created: true}, nil
}

// keyPlan describes how to identify the agent-side session.
type keyPlan struct {
	key agent.SessionKey
	// resume selects whether to resume the conversation identified by key or start a new one
	// with that key.
	resume bool
	// notes are written to the transcript and stderr.
	notes []string
}

// planKey selects the session key for launching t.
//
//	New session:       Start with a new key.
//	Resume:            Use the stored key.
//	Resume with fresh: Start with a new key in the same geminus session.
//	No resume support: Record a note and start a new conversation.
func planKey(t target, fresh bool) (keyPlan, error) {
	caps := t.adapter.Capabilities()
	if fresh && caps.SessionKey == agent.KeyNone {
		return keyPlan{}, fmt.Errorf("--fresh does not apply: agent %q keeps no agent-side session", t.adapter.Name())
	}

	stored := t.meta.AgentSession
	if !t.created && !fresh && caps.Resume && stored != nil {
		key := agent.SessionKey{Kind: agent.KeyKind(stored.Kind), Value: stored.Value}
		return keyPlan{key: key, resume: true}, nil
	}

	key, err := t.adapter.NewKey(t.meta.ID, fresh)
	if err != nil {
		return keyPlan{}, err
	}
	plan := keyPlan{key: key}
	switch {
	case t.created:
	case fresh:
		plan.notes = append(plan.notes, "started a fresh agent-side session (--fresh)")
	case !caps.Resume:
		plan.notes = append(plan.notes, fmt.Sprintf("resume unsupported by agent %q; started it anew", t.adapter.Name()))
	default:
		plan.notes = append(plan.notes, "no agent-side session was recorded; started a new one")
	}
	return plan, nil
}

// storedKey converts key to its session.json representation, or nil for adapters without
// keys.
func storedKey(key agent.SessionKey) *session.AgentSession {
	if key.Kind == agent.KeyNone || key.Kind == "" {
		return nil
	}
	return &session.AgentSession{Kind: string(key.Kind), Value: key.Value}
}

// workDir returns the child's working directory. Resume sessions where they were created
// because Claude scopes conversations by directory.
func workDir(t target, cwd string) (string, error) {
	if t.created || t.meta.Cwd == "" {
		return cwd, nil
	}
	dir := t.meta.Cwd
	// Other programs can write session.json. Reject relative paths because their meaning
	// depends on where geminus is invoked.
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("session %s: its working directory %q is not an absolute path", t.meta.ID, dir)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("session %s: its working directory %q is no longer available", t.meta.ID, dir)
	}
	return dir, nil
}

// announceDir warns on stderr when the agent will run outside the current directory. The
// change matters because agents read configuration from their working directory.
func announceDir(env Env, command, dir, cwd string) {
	if !sameDir(dir, cwd) {
		fmt.Fprintf(env.Stderr, "%s %s: running the agent in the session's directory %q\n", programName, command, dir)
	}
}

// resolveBinary resolves argv[0] against PATH so a missing executable is detected before
// the session is changed.
func resolveBinary(argv []string) ([]string, error) {
	resolved, err := agent.ResolveArgv(argv)
	if errors.Is(err, agent.ErrNotInstalled) {
		return nil, fmt.Errorf("%w; run '%s doctor' to see which agents are installed", err, programName)
	}
	return resolved, err
}

// rollback restores the state before a failed launch: remove a newly created session or
// restore an existing session's metadata.
func rollback(store *session.Store, t target) error {
	if t.created {
		return store.Delete(t.meta.ID)
	}
	prev := t.meta
	_, err := store.Update(prev.ID, func(m session.Meta) session.Meta {
		m.Status = prev.Status
		m.PID = prev.PID
		m.ExitCode = prev.ExitCode
		m.AgentSession = prev.AgentSession
		m.Title = prev.Title
		return m
	})
	return err
}

// startParams holds the inputs to begin.
type startParams struct {
	key agent.SessionKey
	// A nonempty title replaces the session title.
	title string
	// argv is the command to execute. begin masks secrets before recording it.
	argv  []string
	notes []string
	// When record is false (--no-record), skip raw.log and output events.
	record bool
	// When force is true (--force), open the session even if another process has it open.
	force bool
}

// activeSession represents a session marked running. Close it with finish or abort.
type activeSession struct {
	store  *session.Store
	target target
	log    *session.Log
	record bool
}

// begin marks the session running and writes session.start to the transcript. On failure it
// restores the state before launch.
func begin(store *session.Store, t target, p startParams) (*activeSession, error) {
	if err := markRunning(store, t, p); err != nil {
		return nil, err
	}
	log, err := store.OpenLog(t.meta.ID, session.LogOptions{Raw: p.record})
	if err != nil {
		return nil, undo(store, t, err)
	}
	active := &activeSession{store: store, target: t, log: log, record: p.record}
	events := []session.Event{session.StartEvent(t.adapter.Name(), recordedArgv(p.argv, p.record))}
	for _, note := range p.notes {
		events = append(events, session.NoteEvent(note))
	}
	for _, event := range events {
		if _, err := log.AppendEvent(event); err != nil {
			return nil, active.abort(err)
		}
	}
	return active, nil
}

// recordedArgv returns the arguments to store in session.start.
//
// Normally only secret values are masked. With --no-record, keep the command name but omit
// all arguments, including Claude and Hermes prompts. Redaction can miss secrets, so don't
// record arguments at all when the user opts out.
func recordedArgv(argv []string, record bool) []string {
	if record {
		return agent.RedactArgv(argv)
	}
	if len(argv) <= 1 {
		return append([]string(nil), argv...)
	}
	return []string{argv[0], agent.Redacted}
}

// markRunning sets the status in session.json to running and records this process's PID.
//
// If another process opened the session after loadTarget checked it, return errBusy without
// changing it. The store does not lock (plan.md 3.5), so two processes can still race
// between the read and write.
func markRunning(store *session.Store, t target, p startParams) error {
	var busy error
	_, err := store.Update(t.meta.ID, func(m session.Meta) session.Meta {
		if !p.force && m.Active() && m.PID != os.Getpid() {
			busy = fmt.Errorf("session %s: %w in process %d", m.ID, errBusy, m.PID)
			return m
		}
		m.AgentSession = storedKey(p.key)
		m.Status = session.StatusRunning
		m.PID = os.Getpid()
		m.ExitCode = nil
		if p.title != "" {
			m.Title = p.title
		}
		return m
	})
	if busy != nil {
		// Don't restore metadata: the session belongs to the other process.
		return busy
	}
	if err != nil {
		return undo(store, t, err)
	}
	return nil
}

func undo(store *session.Store, t target, cause error) error {
	return errors.Join(cause, rollback(store, t))
}

// abort handles a child launch failure and returns cause together with any cleanup errors.
// For a preexisting session, record the failed launch after session.start.
func (a *activeSession) abort(cause error) error {
	var noteErr error
	if !a.target.created {
		_, noteErr = a.log.AppendEvent(session.NoteEvent("the agent did not start: " + cause.Error()))
	}
	return errors.Join(cause, noteErr, a.log.Close(), rollback(a.store, a.target))
}

// finish records the exit code, adds addTurns to the turn count, and closes the session.
// The caller warns about recording failures but still returns the child's exit code.
func (a *activeSession) finish(code, addTurns int) error {
	_, exitErr := a.log.AppendEvent(session.ExitEvent(code))
	rawBytes := a.log.RawBytes()
	closeErr := a.log.Close()
	_, saveErr := a.store.Update(a.target.meta.ID, func(m session.Meta) session.Meta {
		m.Status = session.StatusExited
		m.PID = 0
		m.ExitCode = &code
		m.Turns += addTurns
		if a.record {
			m.RawBytes = rawBytes
		}
		return m
	})
	return errors.Join(exitErr, closeErr, saveErr)
}

// childExitCode maps the child's exit code to the geminus exit code. A negative value means
// no status was available and is treated as an internal error.
func childExitCode(code int) int {
	if code < 0 {
		return ExitError
	}
	return code
}
