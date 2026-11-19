package cli

import (
	"errors"
	"fmt"

	"github.com/hauntedfail/geminus/internal/session"
)

// runRm はセッションを消す。
//
// --force が無ければ、存在しない id と、別のプロセスが開いているセッションはエラーにする。
// --force はどちらも通す。session.json が壊れたセッションは、どちらの場合も消せる。
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
		// 読めないメタは実行中かどうかを判断できない。消す以外に片付ける方法が無いので通す。
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
