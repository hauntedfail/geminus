package tui

// The text being typed in the prompt box.

import (
	"strings"
	"unicode"
)

// Editor is the input line (or lines) and where the cursor is in it. The
// history is what was sent from this run of geminus; it is not kept.
type Editor struct {
	text []rune
	pos  int

	history [][]rune
	// at is the history entry being shown; len(history) means the draft.
	at    int
	draft []rune
}

func (e *Editor) String() string { return string(e.text) }

func (e *Editor) Empty() bool { return len(e.text) == 0 }

// Text returns the current buffer runes. Callers must not mutate the slice.
func (e *Editor) Text() []rune { return e.text }

// Pos returns the current cursor rune offset.
func (e *Editor) Pos() int { return e.pos }

// Insert adds s at the cursor. Line breaks are kept as "\n"; other control
// characters, and characters that reorder what a terminal shows, are left
// out: they cannot be drawn in the box.
func (e *Editor) Insert(s string) {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	var add []rune
	for _, r := range s {
		if r == unicode.ReplacementChar || UnsafeRune(r) {
			continue
		}
		add = append(add, r)
	}
	if len(add) == 0 {
		return
	}
	next := make([]rune, 0, len(e.text)+len(add))
	next = append(append(append(next, e.text[:e.pos]...), add...), e.text[e.pos:]...)
	e.text, e.pos = next, e.pos+len(add)
}

func (e *Editor) Remove(from, to int) {
	from, to = max(from, 0), min(to, len(e.text))
	if from >= to {
		return
	}
	e.text = append(append([]rune(nil), e.text[:from]...), e.text[to:]...)
	e.pos = from
}

func (e *Editor) Backspace() { e.Remove(e.pos-1, e.pos) }
func (e *Editor) Del()       { p := e.pos; e.Remove(p, p+1); e.pos = min(p, len(e.text)) }

func (e *Editor) Left()  { e.pos = max(e.pos-1, 0) }
func (e *Editor) Right() { e.pos = min(e.pos+1, len(e.text)) }

// LineStart and LineEnd are the bounds of the line the cursor is on.
func (e *Editor) LineStart() int {
	i := e.pos
	for i > 0 && e.text[i-1] != '\n' {
		i--
	}
	return i
}

func (e *Editor) LineEnd() int {
	i := e.pos
	for i < len(e.text) && e.text[i] != '\n' {
		i++
	}
	return i
}

func (e *Editor) Home() { e.pos = e.LineStart() }
func (e *Editor) End()  { e.pos = e.LineEnd() }

func (e *Editor) wordStart() int {
	i := e.pos
	for i > 0 && unicode.IsSpace(e.text[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(e.text[i-1]) {
		i--
	}
	return i
}

func (e *Editor) WordLeft() { e.pos = e.wordStart() }

func (e *Editor) WordRight() {
	i := e.pos
	for i < len(e.text) && unicode.IsSpace(e.text[i]) {
		i++
	}
	for i < len(e.text) && !unicode.IsSpace(e.text[i]) {
		i++
	}
	e.pos = i
}

func (e *Editor) KillWord()  { e.Remove(e.wordStart(), e.pos) }
func (e *Editor) KillStart() { e.Remove(e.LineStart(), e.pos) }
func (e *Editor) KillEnd()   { p := e.pos; e.Remove(p, e.LineEnd()); e.pos = p }

func (e *Editor) Set(text []rune) {
	e.text = append([]rune(nil), text...)
	e.pos = len(e.text)
}

func (e *Editor) Clear() { e.text, e.pos, e.at, e.draft = nil, 0, len(e.history), nil }

// Up moves to the line above, or from the first line to the prompt sent
// before this one.
func (e *Editor) Up() {
	start := e.LineStart()
	if start > 0 {
		col := e.pos - start
		e.pos = start - 1
		e.pos = min(e.LineStart()+col, e.pos)
		return
	}
	if e.at == 0 {
		return
	}
	if e.at == len(e.history) {
		e.draft = append([]rune(nil), e.text...)
	}
	e.at--
	e.Set(e.history[e.at])
}

// Down moves to the line below, or from the last line to the newer prompt.
func (e *Editor) Down() {
	end := e.LineEnd()
	if end < len(e.text) {
		col := e.pos - e.LineStart()
		e.pos = end + 1
		e.pos = min(e.pos+col, e.LineEnd())
		return
	}
	if e.at >= len(e.history) {
		return
	}
	e.at++
	if e.at == len(e.history) {
		e.Set(e.draft)
		return
	}
	e.Set(e.history[e.at])
}

// Take returns what was typed and empties the editor. remember adds it to
// the history.
func (e *Editor) Take(remember bool) string {
	text := string(e.text)
	if remember && strings.TrimSpace(text) != "" &&
		(len(e.history) == 0 || string(e.history[len(e.history)-1]) != text) {
		e.history = append(e.history, append([]rune(nil), e.text...))
	}
	e.Clear()
	return text
}
