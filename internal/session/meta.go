// Package session stores geminus sessions in local files.
//
// Layout:
//
//	$GEMINUS_HOME/sessions/<id>/session.json      metadata (replaced by renaming a temporary file)
//	$GEMINUS_HOME/sessions/<id>/transcript.jsonl  append-only event stream
//	$GEMINUS_HOME/sessions/<id>/raw.log           raw PTY bytes
//
// Conversations may contain secrets, so directories are created with mode 0700 and files with mode
// 0600. No locking is used: each session is assumed to have a single writer process.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// SchemaVersion is the current session.json format version.
const SchemaVersion = 1

// Status is the state of a session.
type Status string

const (
	// StatusCreated means the session exists but the agent has not started.
	StatusCreated Status = "created"
	// StatusRunning means the agent is running. Check the PID as well (see Meta.Active).
	StatusRunning Status = "running"
	// StatusExited means the agent has exited.
	StatusExited Status = "exited"
)

var (
	// ErrNotFound means the session does not exist.
	ErrNotFound = errors.New("session not found")
	// ErrCorrupt means session.json or transcript.jsonl contains malformed data.
	ErrCorrupt = errors.New("corrupt session data")
	// ErrInvalidID means the session ID is malformed.
	ErrInvalidID = errors.New("invalid session id")
	// ErrUnsupportedVersion means session.json uses a format newer than this binary supports.
	ErrUnsupportedVersion = errors.New("unsupported session format version")
)

// AgentSession identifies the session on the agent side. Claude uses kind "uuid"; Hermes uses kind
// "name".
type AgentSession struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Meta holds the contents of session.json.
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
	// PID identifies the geminus relay process while Status is running. It is used to detect duplicate
	// launches (plan.md section 3.5).
	PID int `json:"pid,omitempty"`
	// ExitCode is nil until the child exits. Zero is stored as a value.
	ExitCode *int  `json:"exit_code,omitempty"`
	Turns    int   `json:"turns"`
	RawBytes int64 `json:"raw_bytes"`
}

// Active reports whether another process still has this session open. PID reuse can cause false
// positives, so callers should provide an override (--force).
func (m Meta) Active() bool {
	return m.Status == StatusRunning && ProcessAlive(m.PID)
}

// clone copies the pointed-to values too, so the stored metadata shares no pointers with the
// caller.
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

// ValidID reports whether id has the form YYYYMMDD-HHMMSS-<4hex>. Since id becomes part of a path,
// always validate it before accessing files.
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// NewID creates a YYYYMMDD-HHMMSS-<4hex> ID using the local time of t.
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
