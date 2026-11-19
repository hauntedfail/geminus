package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hauntedfail/geminus/internal/agent"
	"github.com/hauntedfail/geminus/internal/session"
)

// errBusy は、セッションを別のプロセスが開いていることを表す。
var errBusy = errors.New("session is already open")

// target は起動するセッションと、その相手になるアダプタ。
type target struct {
	meta    session.Meta
	adapter agent.Adapter
	// created は、このプロセスが今回新しく作ったセッションであることを表す。
	// 子を起動する前に失敗したら、空のセッションを残さないよう消す。
	created bool
}

// loadTarget は既存のセッション id を開く準備をする。
// agentFlag が空でなく、セッションのバックエンドと違えばエラーにする。
// 別のプロセスが開いていれば errBusy を返す（force で無視する）。
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

// createTarget は adapter 用の新しいセッションを作る。
func createTarget(store *session.Store, adapter agent.Adapter, title, cwd string) (target, error) {
	meta, err := store.Create(session.CreateParams{Title: title, Agent: adapter.Name(), Cwd: cwd})
	if err != nil {
		return target{}, err
	}
	return target{meta: meta, adapter: adapter, created: true}, nil
}

// keyPlan はエージェント側のセッションをどう指すかの決定。
type keyPlan struct {
	key agent.SessionKey
	// resume が true なら key の指す会話を再開する。false なら key で新しく始める。
	resume bool
	// notes は transcript と stderr に残す注記。
	notes []string
}

// planKey は t を起動するときの継続キーを決める。
//
//	新規セッション       新しいキーで始める
//	再開                 保存済みのキーで再開する
//	再開 + fresh         キーだけ作り直して新しく始める（geminus のセッションは同じ）
//	再開できないアダプタ 注記を残して新しく始める
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

// storedKey は key を session.json に入れる形にする。キーを持たないアダプタは nil。
func storedKey(key agent.SessionKey) *session.AgentSession {
	if key.Kind == agent.KeyNone || key.Kind == "" {
		return nil
	}
	return &session.AgentSession{Kind: string(key.Kind), Value: key.Value}
}

// workDir は子を動かすディレクトリを返す。
// 再開するセッションは、作ったときのディレクトリで動かす（claude は会話をディレクトリごとに持つ）。
func workDir(t target, cwd string) (string, error) {
	if t.created || t.meta.Cwd == "" {
		return cwd, nil
	}
	dir := t.meta.Cwd
	// session.json は geminus 以外も書ける。相対パスは、どこを指すかが実行場所で変わるので受けない。
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("session %s: its working directory %q is not an absolute path", t.meta.ID, dir)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("session %s: its working directory %q is no longer available", t.meta.ID, dir)
	}
	return dir, nil
}

// announceDir は、今いる場所と違うディレクトリでエージェントを動かすことを stderr に知らせる。
// エージェントは動かす場所の設定ファイルを読むので、黙って場所を変えない。
func announceDir(env Env, command, dir, cwd string) {
	if !sameDir(dir, cwd) {
		fmt.Fprintf(env.Stderr, "%s %s: running the agent in the session's directory %q\n", programName, command, dir)
	}
}

// resolveBinary は argv[0] を PATH で解決した argv を返す。
// セッションに手を付ける前に、起動できないことを見つけるために使う。
func resolveBinary(argv []string) ([]string, error) {
	resolved, err := agent.ResolveArgv(argv)
	if errors.Is(err, agent.ErrNotInstalled) {
		return nil, fmt.Errorf("%w; run '%s doctor' to see which agents are installed", err, programName)
	}
	return resolved, err
}

// rollback は、子を起動できなかったセッションを起動前の状態に戻す。
// 今回作ったセッションは消し、既存のセッションはメタを戻す。
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

// startParams は begin の入力。
type startParams struct {
	key agent.SessionKey
	// title が空でなければタイトルを置き換える。
	title string
	// argv は実際に起動するコマンド。秘密は begin が伏せてから記録する。
	argv  []string
	notes []string
	// record が false なら raw.log と output イベントを書かない（--no-record）。
	record bool
	// force が true なら、別のプロセスが開いているセッションでも開く（--force）。
	force bool
}

// activeSession は running にしたセッション。finish か abort で必ず閉じる。
type activeSession struct {
	store  *session.Store
	target target
	log    *session.Log
	record bool
}

// begin はセッションを running にし、transcript に session.start を書く。
// 失敗したら起動前の状態に戻す。
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

// recordedArgv は session.start に残す argv を返す。
//
// 通常は秘密の値だけを伏せる。--no-record では、何を起動したかが分かるコマンド名だけを残し、
// 引数（claude / hermes ではプロンプトそのもの）は残さない。値の伏せ方には取りこぼしが
// あり得るので、記録を避けたい利用者には引数ごと書かない。
func recordedArgv(argv []string, record bool) []string {
	if record {
		return agent.RedactArgv(argv)
	}
	if len(argv) <= 1 {
		return append([]string(nil), argv...)
	}
	return []string{argv[0], agent.Redacted}
}

// markRunning は session.json を running にし、このプロセスの PID を入れる。
//
// loadTarget で確かめた後に別のプロセスが同じセッションを開いていたら、書き換えずに
// errBusy を返す。ストアはロックを持たないので（plan.md 3.5）、読んでから書くまでの
// ごく短い間に2つのプロセスが重なる余地は残る。
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
		// 相手のセッションなので、メタは戻さない。
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

// abort は子を起動できなかったときに呼ぶ。cause に後始末のエラーを足して返す。
// 残るセッション（今回作ったものでない）には、session.start の後に起動できなかった旨を残す。
func (a *activeSession) abort(cause error) error {
	var noteErr error
	if !a.target.created {
		_, noteErr = a.log.AppendEvent(session.NoteEvent("the agent did not start: " + cause.Error()))
	}
	return errors.Join(cause, noteErr, a.log.Close(), rollback(a.store, a.target))
}

// finish は終了コードを記録してセッションを閉じる。addTurns は増やすターン数。
// 記録の失敗は、子の終了コードを返す妨げにしない。呼び出し側は警告として出す。
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

// childExitCode は子の終了コードを geminus の終了コードにする。
// 取れなかった場合（負の値）は geminus 自体のエラーとして扱う。
func childExitCode(code int) int {
	if code < 0 {
		return ExitError
	}
	return code
}
