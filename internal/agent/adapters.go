package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

// claudeAdapter は Claude Code。
//
//	新規:        claude --session-id <uuid> [prompt]
//	再開:        claude --resume <uuid> [prompt]
//	ワンショット: claude -p (--session-id|--resume) <uuid> <prompt>
//
// --session-id は UUID 必須なので、geminus が UUIDv4 を生成して保存する。
type claudeAdapter struct{}

func (claudeAdapter) Name() string   { return "claude" }
func (claudeAdapter) Binary() string { return "claude" }

func (claudeAdapter) Capabilities() Capabilities {
	return Capabilities{Resume: true, Oneshot: true, SessionKey: KeyUUID}
}

func (claudeAdapter) NewKey(string, bool) (SessionKey, error) {
	id, err := NewUUID()
	if err != nil {
		return SessionKey{}, err
	}
	return SessionKey{Kind: KeyUUID, Value: id}, nil
}

func (a claudeAdapter) sessionArgs(req Request) ([]string, error) {
	if req.Key.Kind != KeyUUID || !IsUUID(req.Key.Value) {
		return nil, fmt.Errorf("%w: claude needs a uuid, got %s %q", ErrInvalidKey, req.Key.Kind, req.Key.Value)
	}
	if req.Resume {
		return []string{"--resume", req.Key.Value}, nil
	}
	return []string{"--session-id", req.Key.Value}, nil
}

func (a claudeAdapter) Interactive(req Request) ([]string, error) {
	session, err := a.sessionArgs(req)
	if err != nil {
		return nil, err
	}
	argv := append([]string{a.Binary()}, session...)
	if prompt := joinPrompt(req.Args); prompt != "" {
		if err := checkPromptArg(prompt); err != nil {
			return nil, err
		}
		argv = append(argv, prompt)
	}
	return argv, nil
}

func (a claudeAdapter) Oneshot(req Request, prompt string) ([]string, error) {
	session, err := a.sessionArgs(req)
	if err != nil {
		return nil, err
	}
	if err := checkPrompt(prompt); err != nil {
		return nil, err
	}
	argv := append([]string{a.Binary(), "-p"}, session...)
	return append(argv, prompt), nil
}

// hermesAdapter は Hermes Agent。
//
//	新規・再開:  hermes chat -c <name> --create-if-missing [-q <prompt>]
//	ワンショット: hermes chat -c <name> --create-if-missing -q <prompt> --oneshot
//
// -c <name> --create-if-missing は「無ければ作る、あれば再開」なので、新規と再開で同じ argv になる。
// 名前には geminus のセッション id をそのまま使う。
type hermesAdapter struct{}

// hermes のセッション名として渡す文字種。先頭を英数字に限り、フラグと解釈されないようにする。
var hermesNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (hermesAdapter) Name() string   { return "hermes" }
func (hermesAdapter) Binary() string { return "hermes" }

func (hermesAdapter) Capabilities() Capabilities {
	return Capabilities{Resume: true, Oneshot: true, SessionKey: KeyName}
}

func (hermesAdapter) NewKey(geminusID string, fresh bool) (SessionKey, error) {
	name := geminusID
	if fresh {
		var suffix [2]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return SessionKey{}, fmt.Errorf("generate hermes session name: %w", err)
		}
		name += "-" + hex.EncodeToString(suffix[:])
	}
	if !hermesNamePattern.MatchString(name) {
		return SessionKey{}, fmt.Errorf("%w: %q cannot be used as a hermes session name", ErrInvalidKey, name)
	}
	return SessionKey{Kind: KeyName, Value: name}, nil
}

func (a hermesAdapter) baseArgv(req Request) ([]string, error) {
	if req.Key.Kind != KeyName || !hermesNamePattern.MatchString(req.Key.Value) {
		return nil, fmt.Errorf("%w: hermes needs a session name, got %s %q", ErrInvalidKey, req.Key.Kind, req.Key.Value)
	}
	return []string{a.Binary(), "chat", "-c", req.Key.Value, "--create-if-missing"}, nil
}

func (a hermesAdapter) Interactive(req Request) ([]string, error) {
	argv, err := a.baseArgv(req)
	if err != nil {
		return nil, err
	}
	// TTY 上の -q は対話セッションの最初のターンとして送られる。
	if prompt := joinPrompt(req.Args); prompt != "" {
		if err := checkPromptArg(prompt); err != nil {
			return nil, err
		}
		argv = append(argv, "-q", prompt)
	}
	return argv, nil
}

func (a hermesAdapter) Oneshot(req Request, prompt string) ([]string, error) {
	argv, err := a.baseArgv(req)
	if err != nil {
		return nil, err
	}
	if err := checkPrompt(prompt); err != nil {
		return nil, err
	}
	return append(argv, "-q", prompt, "--oneshot"), nil
}

// genericAdapter は任意のコマンドをそのまま起動する。継続キーを持たず、再開もできない。
type genericAdapter struct{}

func (genericAdapter) Name() string   { return GenericName }
func (genericAdapter) Binary() string { return "" }

func (genericAdapter) Capabilities() Capabilities {
	return Capabilities{Resume: false, Oneshot: false, SessionKey: KeyNone}
}

func (genericAdapter) NewKey(string, bool) (SessionKey, error) {
	return SessionKey{Kind: KeyNone}, nil
}

func (genericAdapter) Interactive(req Request) ([]string, error) {
	if req.Resume {
		return nil, ErrNoResume
	}
	if len(req.Args) == 0 || req.Args[0] == "" {
		return nil, ErrNoCommand
	}
	return append([]string(nil), req.Args...), nil
}

func (genericAdapter) Oneshot(Request, string) ([]string, error) {
	return nil, ErrNoOneshot
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID は s が 8-4-4-4-12 桁の16進表記かを返す。
func IsUUID(s string) bool {
	return uuidPattern.MatchString(s)
}

// NewUUID は乱数から UUIDv4（RFC 4122）を作る。
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
