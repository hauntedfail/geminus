package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"
)

// Event types used in transcript.jsonl.
const (
	EventStart  = "session.start"
	EventOutput = "output"
	EventInput  = "input"
	EventNote   = "note"
	EventExit   = "session.exit"
)

// Event represents one line of transcript.jsonl.
//
// Offset, Bytes and Code are pointers because zero is meaningful. For an output event, Offset and
// Bytes identify the range [Offset, Offset+Bytes) in raw.log.
type Event struct {
	Seq    int64     `json:"seq"`
	TS     time.Time `json:"ts"`
	Type   string    `json:"type"`
	Agent  string    `json:"agent,omitempty"`
	Argv   []string  `json:"argv,omitempty"`
	Offset *int64    `json:"offset,omitempty"`
	Bytes  *int64    `json:"bytes,omitempty"`
	Text   string    `json:"text,omitempty"`
	Code   *int      `json:"code,omitempty"`
}

// StartEvent records agent startup. Callers must redact secrets from argv first
// (agent.RedactArgv).
func StartEvent(agent string, argv []string) Event {
	return Event{Type: EventStart, Agent: agent, Argv: append([]string(nil), argv...)}
}

// InputEvent records user input (ask and non-TTY input only).
func InputEvent(text string) Event {
	return Event{Type: EventInput, Text: text}
}

// NoteEvent records a note from geminus itself.
func NoteEvent(text string) Event {
	return Event{Type: EventNote, Text: text}
}

// ExitEvent records the child's exit.
func ExitEvent(code int) Event {
	return Event{Type: EventExit, Code: &code}
}

func outputEvent(offset, size int64, ts time.Time) Event {
	return Event{Type: EventOutput, TS: ts, Offset: &offset, Bytes: &size}
}

// ReadEvents reads the session's transcript.jsonl from the beginning.
//
// If transcript.jsonl is not a regular file (for example, a symlink or FIFO), ReadEvents returns
// ErrNotRegular without reading it. It skips corrupt lines and reports them together in an error
// wrapping ErrCorrupt. Callers can use the returned events even when the error is non-nil. A
// session without a transcript yields no events.
func (s *Store) ReadEvents(id string) ([]Event, error) {
	paths, err := s.existingPaths(id)
	if err != nil {
		return nil, err
	}
	f, err := openRegular(paths.Transcript)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session %s: open transcript: %w", id, err)
	}
	defer f.Close()

	var events []Event
	var broken []error
	err = scanLines(f, func(lineNo int, line []byte, _ bool) {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			broken = append(broken, fmt.Errorf("session %s: transcript line %d: %w: %v", id, lineNo, ErrCorrupt, err))
			return
		}
		events = append(events, event)
	})
	if err != nil {
		return events, fmt.Errorf("session %s: read transcript: %w", id, err)
	}
	return events, errors.Join(broken...)
}

// existingPaths also checks that the session directory returned by Paths exists and is a real
// directory. It returns ErrNotRegular for a symlink to avoid reading or writing through it.
func (s *Store) existingPaths(id string) (Paths, error) {
	paths, err := s.Paths(id)
	if err != nil {
		return Paths{}, err
	}
	if err := requireSessionDir(paths.Dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Paths{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
		}
		return Paths{}, fmt.Errorf("session %s: %w", id, err)
	}
	return paths, nil
}

// scanLines calls visit for each nonempty line in r. The terminated flag reports whether the line
// ended in a newline. Line length is unbounded.
func scanLines(r io.Reader, visit func(lineNo int, line []byte, terminated bool)) error {
	reader := bufio.NewReader(r)
	for lineNo := 1; ; lineNo++ {
		line, err := reader.ReadBytes('\n')
		terminated := err == nil
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			visit(lineNo, trimmed, terminated)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// transcriptState holds the information needed to resume appending to an existing transcript.
type transcriptState struct {
	lastSeq int64
	// tornTail means the last line lacks a newline, as after a crash during a write.
	tornTail bool
}

func readTranscriptState(path string) (transcriptState, error) {
	f, err := openRegular(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return transcriptState{}, nil
		}
		return transcriptState{}, err
	}
	defer f.Close()

	var state transcriptState
	err = scanLines(f, func(_ int, line []byte, terminated bool) {
		state.tornTail = !terminated
		var head struct {
			Seq int64 `json:"seq"`
		}
		// Ignore unreadable lines when calculating seq. ReadEvents reports the lines themselves.
		if json.Unmarshal(line, &head) == nil && head.Seq > state.lastSeq {
			state.lastSeq = head.Seq
		}
	})
	return state, err
}
