package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/hauntedfail/geminus/internal/session"
)

func runLog(inv Invocation) int {
	const name = "log"
	opts := inv.Options.Log
	id, err := oneArg(inv.Args, "session id")
	if err != nil {
		return report(inv.Env, name, err)
	}
	if opts.Tail < 0 {
		return usageError(inv.Env, name, "--tail must not be negative")
	}
	if opts.Raw && inv.Options.JSON {
		return usageError(inv.Env, name, "--raw and --json cannot be used together")
	}
	store, err := openStore(inv)
	if err != nil {
		return fail(inv.Env, name, err)
	}

	if inv.Options.JSON {
		return logEvents(inv, store, id)
	}
	if err := logOutput(inv, store, id); err != nil {
		return fail(inv.Env, name, err)
	}
	return ExitOK
}

// logEvents writes transcript.jsonl events as a JSON array. If a line is corrupt, emit the
// readable events and signal the error via the exit code.
func logEvents(inv Invocation, store *session.Store, id string) int {
	const name = "log"
	events, readErr := store.ReadEvents(id)
	if readErr != nil && !errors.Is(readErr, session.ErrCorrupt) {
		return fail(inv.Env, name, readErr)
	}
	if n := inv.Options.Log.Tail; n > 0 && len(events) > n {
		events = events[len(events)-n:]
	}
	if events == nil {
		events = []session.Event{}
	}
	if err := writeJSON(inv.Env.Stdout, events); err != nil {
		return fail(inv.Env, name, err)
	}
	if readErr != nil {
		return fail(inv.Env, name, readErr)
	}
	return ExitOK
}

// logOutput writes raw.log, stripping control sequences by default and preserving the
// original bytes with --raw.
func logOutput(inv Invocation, store *session.Store, id string) (err error) {
	raw, err := store.OpenRaw(id)
	if err != nil {
		if errors.Is(err, session.ErrNoRawLog) {
			return fmt.Errorf("%w (it was recorded with --no-record, or has not started yet)", err)
		}
		return err
	}
	defer func() {
		err = errors.Join(err, raw.Close())
	}()

	// Writes flow through control-sequence stripping, the last-N-lines filter, and the buffer
	// before reaching the terminal.
	out := bufio.NewWriter(inv.Env.Stdout)
	stages := []io.Closer{}
	var dst io.Writer = out
	if n := inv.Options.Log.Tail; n > 0 {
		tail := newTailWriter(dst, n)
		stages = append(stages, tail)
		dst = tail
	}
	if !inv.Options.Log.Raw {
		stripper := newANSIStripper(dst)
		stages = append(stages, stripper)
		dst = stripper
	}

	if _, err := io.Copy(dst, raw); err != nil {
		return fmt.Errorf("session %s: read raw.log: %w", id, err)
	}
	// Close writers in that order so each flushes its pending data into the next.
	for i := len(stages) - 1; i >= 0; i-- {
		if err := stages[i].Close(); err != nil {
			return err
		}
	}
	return out.Flush()
}
