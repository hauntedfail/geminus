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

// transcript.jsonl のイベント種別。
const (
	EventStart  = "session.start"
	EventOutput = "output"
	EventInput  = "input"
	EventNote   = "note"
	EventExit   = "session.exit"
)

// Event は transcript.jsonl の1行。
//
// Offset / Bytes / Code は 0 に意味があるのでポインタで持つ。
// output イベントの Offset と Bytes は raw.log の範囲 [Offset, Offset+Bytes) を指す。
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

// StartEvent はエージェントの起動を表す。
// argv の秘密は呼び出し側で伏せてから渡す（agent.RedactArgv）。
func StartEvent(agent string, argv []string) Event {
	return Event{Type: EventStart, Agent: agent, Argv: append([]string(nil), argv...)}
}

// InputEvent は利用者の入力を表す（ask と非 TTY のみ）。
func InputEvent(text string) Event {
	return Event{Type: EventInput, Text: text}
}

// NoteEvent は geminus 自身の注記を表す。
func NoteEvent(text string) Event {
	return Event{Type: EventNote, Text: text}
}

// ExitEvent は子プロセスの終了を表す。
func ExitEvent(code int) Event {
	return Event{Type: EventExit, Code: &code}
}

func outputEvent(offset, size int64, ts time.Time) Event {
	return Event{Type: EventOutput, TS: ts, Offset: &offset, Bytes: &size}
}

// ReadEvents は id の transcript.jsonl を先頭から読む。
//
// transcript.jsonl が通常ファイルでなければ（シンボリックリンク、FIFO など）読まずに
// ErrNotRegular を返す。
// 壊れた行は飛ばして残りを返し、飛ばした行を ErrCorrupt を含む error にまとめる。
// 呼び出し側は error が非 nil でも返ってきたイベントを使ってよい。
// transcript がまだ無いセッションは空を返す。
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

// existingPaths は Paths に加えて、セッションのディレクトリが実ディレクトリとして
// あることを確かめる。シンボリックリンクなら、その先を読み書きしないよう ErrNotRegular を返す。
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

// scanLines は r の空でない行ごとに visit を呼ぶ。
// terminated は行が改行で終わっていたかを表す。行の長さに上限は無い。
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

// transcriptState は追記を再開するために必要な、既存 transcript の状態。
type transcriptState struct {
	lastSeq int64
	// tornTail は最後の行が改行で終わっていないことを表す（書き込み途中のクラッシュ）。
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
		// 読めない行は seq の計算から外す。行自体は ReadEvents が報告する。
		if json.Unmarshal(line, &head) == nil && head.Seq > state.lastSeq {
			state.lastSeq = head.Seq
		}
	})
	return state, err
}
