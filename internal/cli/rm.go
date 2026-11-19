package cli

import (
	"errors"
	"fmt"

	"github.com/hauntedfail/geminus/internal/session"
)

// runRm removes a session.
//
// Without --force, a missing ID or a session held by another process is an error. --force
// allows both. A session with corrupt metadata can be removed either way.
func runRm(inv Invocation) int {
	const name = "rm"
	force := inv.Options.Rm.Force
	id, err := oneArg(inv.Args, "session id")
	if err != nil {
		return report(inv.Env, name, err)
	}
	store, err := openStore(inv)
	if err != nil {
		return fail(inv.Env, name, err)
	}

	if !force {
		// Unreadable metadata cannot tell us whether the session is running. Allow deletion because
		// there is no other way to clean it up.
		if meta, loadErr := store.Load(id); loadErr == nil && meta.Active() {
			return fail(inv.Env, name, fmt.Errorf("session %s is open in process %d (use --force to delete it anyway)", id, meta.PID))
		}
	}
	if err := store.Delete(id); err != nil {
		if force && errors.Is(err, session.ErrNotFound) {
			return ExitOK
		}
		return fail(inv.Env, name, err)
	}
	if logf := verboseLogf(inv); logf != nil {
		logf("deleted session %s", id)
	}
	return ExitOK
}
