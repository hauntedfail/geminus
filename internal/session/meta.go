// Package session は geminus のセッションをローカルファイルとして保存する。
//
// レイアウト:
//
//	$GEMINUS_HOME/sessions/<id>/session.json      メタ（一時ファイル + rename で置換）
//	$GEMINUS_HOME/sessions/<id>/transcript.jsonl  追記専用のイベント列
//	$GEMINUS_HOME/sessions/<id>/raw.log           PTY の生バイト
//
// 会話には秘密が混ざり得るので、ディレクトリは 0700、ファイルは 0600 で作る。
// 同じセッションへ書くプロセスは1つだけという前提で、ロックは取らない。
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// SchemaVersion は session.json の現在の形式バージョン。
const SchemaVersion = 1

// Status はセッションの状態。
type Status string

const (
	// StatusCreated は作成済みで、まだエージェントを起動していない状態。
	StatusCreated Status = "created"
	// StatusRunning はエージェントが動いている状態。PID と組で判断する（Meta.Active）。
	StatusRunning Status = "running"
	// StatusExited はエージェントが終了した状態。
	StatusExited Status = "exited"
)

var (
	// ErrNotFound はセッションが存在しないことを表す。
	ErrNotFound = errors.New("session not found")
	// ErrCorrupt は session.json や transcript.jsonl が読めない形であることを表す。
	ErrCorrupt = errors.New("corrupt session data")
	// ErrInvalidID はセッション ID の形式が不正であることを表す。
	ErrInvalidID = errors.New("invalid session id")
	// ErrUnsupportedVersion は session.json がこのバイナリより新しい形式であることを表す。
	ErrUnsupportedVersion = errors.New("unsupported session format version")
)

// AgentSession はエージェント側のセッションを指すキー。
// claude は kind "uuid"、hermes は kind "name"。
type AgentSession struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Meta は session.json の内容。
type Meta struct {
	Version      int           `json:"version"`
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	Agent        string        `json:"agent"`
	AgentSession *AgentSession `json:"agent_session,omitempty"`
	Cwd          string        `json:"cwd"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
	Status       Status        `json:"status"`
	// PID は Status が running の間、中継している geminus プロセスの PID。
	// 二重起動の判定に使う（plan.md 3.5）。
	PID int `json:"pid,omitempty"`
	// ExitCode は子が終了するまで nil。0 も値として保存する。
	ExitCode *int  `json:"exit_code,omitempty"`
	Turns    int   `json:"turns"`
	RawBytes int64 `json:"raw_bytes"`
}

// Active は、このセッションを別のプロセスが今も開いているかを返す。
// PID の再利用で偽陽性になり得るので、呼び出し側は上書き手段（--force）を用意する。
func (m Meta) Active() bool {
	return m.Status == StatusRunning && ProcessAlive(m.PID)
}

// clone はポインタの指す先まで複製する。保存済みの値を呼び出し側と共有しないため。
func (m Meta) clone() Meta {
	out := m
	if m.AgentSession != nil {
		key := *m.AgentSession
		out.AgentSession = &key
	}
	if m.ExitCode != nil {
		code := *m.ExitCode
		out.ExitCode = &code
	}
	return out
}

var idPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{4}$`)

// ValidID は id が YYYYMMDD-HHMMSS-<4hex> の形かを返す。
// id はパスの一部になるので、ファイルを触る前に必ずこれで確かめる。
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// NewID は t のローカル時刻から YYYYMMDD-HHMMSS-<4hex> の ID を作る。
func NewID(t time.Time) (string, error) {
	var suffix [2]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return t.Local().Format("20060102-150405") + "-" + hex.EncodeToString(suffix[:]), nil
}

func checkID(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return nil
}
