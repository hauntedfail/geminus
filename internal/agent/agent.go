// Package agent は claude / hermes / 任意コマンドを同じ形で起動するためのアダプタを持つ。
//
// セッション継続キーの作り方と argv の組み立てはバックエンドごとに違うので、ここに閉じ込める。
// このパッケージは子プロセスを起動しない。認証情報にも触れない。
package agent

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const (
	// DefaultName は --agent も config も無いときに使うバックエンド。
	DefaultName = "claude"
	// GenericName は任意コマンドをそのまま起動するバックエンド。
	GenericName = "cmd"
)

var (
	// ErrNoResume はバックエンドがセッションの再開に対応していないことを表す。
	ErrNoResume = errors.New("agent does not support resume")
	// ErrNoOneshot はバックエンドがワンショット実行に対応していないことを表す。
	ErrNoOneshot = errors.New("agent does not support one-shot prompts")
	// ErrUnknownAgent は登録されていないバックエンド名を表す。
	ErrUnknownAgent = errors.New("unknown agent")
	// ErrNotInstalled はバックエンドのバイナリが見つからないことを表す。
	ErrNotInstalled = errors.New("agent binary not found")
	// ErrInvalidKey はセッション継続キーがそのバックエンドで使えない形であることを表す。
	ErrInvalidKey = errors.New("invalid agent session key")
	// ErrNoCommand は起動するコマンドが空であることを表す。
	ErrNoCommand = errors.New("no command given")
	// ErrNoPrompt はワンショットのプロンプトが空であることを表す。
	ErrNoPrompt = errors.New("prompt is empty")
	// ErrInvalidPrompt はプロンプトをそのままエージェントの引数にできないことを表す。
	ErrInvalidPrompt = errors.New("invalid prompt")
)

// KeyKind はセッション継続キーの種類。session.json の agent_session.kind に入る値。
type KeyKind string

const (
	// KeyNone は継続キーを持たない（再開できない）。
	KeyNone KeyKind = "none"
	// KeyUUID は geminus が生成する UUIDv4（claude）。
	KeyUUID KeyKind = "uuid"
	// KeyName はセッション名（hermes）。
	KeyName KeyKind = "name"
)

// SessionKey はエージェント側のセッションを指すキー。
type SessionKey struct {
	Kind  KeyKind
	Value string
}

// Capabilities はバックエンドの能力表。
type Capabilities struct {
	// Resume は同じ会話を再開できるか。
	Resume bool `json:"resume"`
	// Oneshot は1往復で終わる非対話実行ができるか。
	Oneshot bool `json:"oneshot"`
	// SessionKey は継続キーの種類。
	SessionKey KeyKind `json:"session_key"`
}

// Request は argv を組み立てるための入力。
type Request struct {
	// Key は NewKey が返した継続キー。
	Key SessionKey
	// Resume が true なら Key の指す会話を再開する。false なら Key で新しく始める。
	Resume bool
	// Args は run の位置引数。claude / hermes では最初のプロンプト、cmd では起動するコマンド行。
	Args []string
}

// Adapter は1つのバックエンドの起動方法。
type Adapter interface {
	// Name は --agent に渡す名前。
	Name() string
	// Binary は PATH から探す実行ファイル名。cmd は空を返す。
	Binary() string
	Capabilities() Capabilities
	// NewKey は geminus セッション id に対応する継続キーを作る。
	// fresh が true なら、同じ id に対して以前と重ならないキーを返す（--fresh）。
	NewKey(geminusID string, fresh bool) (SessionKey, error)
	// Interactive は対話セッション（PTY で動かす）の argv を返す。
	Interactive(req Request) ([]string, error)
	// Oneshot は prompt を1回送って終了する argv を返す。req.Args は使わない。
	Oneshot(req Request, prompt string) ([]string, error)
}

// adapters は登録済みのバックエンド。ヘルプや候補一覧に出す順で並べる。
func adapters() []Adapter {
	return []Adapter{claudeAdapter{}, hermesAdapter{}, genericAdapter{}}
}

// All は登録済みのアダプタを返す。
func All() []Adapter { return adapters() }

// Names は登録済みのバックエンド名を返す。
func Names() []string {
	all := adapters()
	names := make([]string, 0, len(all))
	for _, a := range all {
		names = append(names, a.Name())
	}
	return names
}

// UnknownAgentError は未知のバックエンド名と、指定できる候補を持つ。
type UnknownAgentError struct {
	Name  string
	Known []string
}

func (e *UnknownAgentError) Error() string {
	return fmt.Sprintf("unknown agent %q (available: %s)", e.Name, strings.Join(e.Known, ", "))
}

// Is は errors.Is(err, ErrUnknownAgent) を成立させる。
func (e *UnknownAgentError) Is(target error) bool { return target == ErrUnknownAgent }

// Lookup は name のアダプタを返す。未知の名前は候補を並べた *UnknownAgentError になる。
func Lookup(name string) (Adapter, error) {
	for _, a := range adapters() {
		if a.Name() == name {
			return a, nil
		}
	}
	return nil, &UnknownAgentError{Name: name, Known: Names()}
}

// Detection はバックエンド1つの検出結果。`geminus agents` と `doctor` が使う。
type Detection struct {
	Name         string       `json:"name"`
	Binary       string       `json:"binary,omitempty"`
	Path         string       `json:"path,omitempty"`
	Found        bool         `json:"found"`
	Capabilities Capabilities `json:"capabilities"`
	Error        string       `json:"error,omitempty"`
}

// Detect は a のバイナリを PATH から探す。バイナリを持たない cmd は常に Found になる。
func Detect(a Adapter) Detection {
	d := Detection{Name: a.Name(), Binary: a.Binary(), Capabilities: a.Capabilities()}
	if d.Binary == "" {
		d.Found = true
		return d
	}
	path, err := LookPath(d.Binary)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.Path = path
	d.Found = true
	return d
}

// DetectAll は登録済みの全バックエンドを検出する。
func DetectAll() []Detection {
	all := adapters()
	out := make([]Detection, 0, len(all))
	for _, a := range all {
		out = append(out, Detect(a))
	}
	return out
}

// LookPath は binary を PATH から探す。見つからなければ ErrNotInstalled を返す。
func LookPath(binary string) (string, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("%w: %q (%v)", ErrNotInstalled, binary, err)
	}
	return path, nil
}

// ResolveArgv は argv[0] を PATH で解決したパスに置き換えた新しい argv を返す。
// 起動前にバイナリの不在を検出するために使う。
func ResolveArgv(argv []string) ([]string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, ErrNoCommand
	}
	path, err := LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	return append([]string{path}, argv[1:]...), nil
}

// joinPrompt は run の位置引数を1つのプロンプトにまとめる。
func joinPrompt(args []string) string {
	return strings.TrimSpace(strings.Join(args, " "))
}

// checkPrompt はワンショットのプロンプトを確かめる。空は ErrNoPrompt。
func checkPrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return ErrNoPrompt
	}
	return checkPromptArg(prompt)
}

// checkPromptArg は、プロンプトがエージェントのフラグとして解釈されないことを確かめる。
// "-" で始まるプロンプトを位置引数や -q の値として渡すと、エージェント側の引数解析が
// フラグとして読む（例: "--dangerously-skip-permissions"）。黙って意味を変えずに拒否する。
func checkPromptArg(prompt string) error {
	if strings.HasPrefix(strings.TrimSpace(prompt), "-") {
		return fmt.Errorf("%w: a prompt must not start with \"-\" (the agent would read it as a flag)", ErrInvalidPrompt)
	}
	return nil
}
