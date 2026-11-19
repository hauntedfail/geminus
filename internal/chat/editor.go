package chat

// The text being typed in the prompt box.

import (
	"strings"
	"unicode"
)

// editor is the input line (or lines) and where the cursor is in it. The
// history is what was sent from this run of geminus; it is not kept.
type editor struct {
	text []rune
	pos  int

	history [][]rune
	// at is the history entry being shown; len(history) means the draft.
	at    int
	draft []rune
}

func (e *editor) String() string { return string(e.text) }

func (e *editor) empty() bool { return len(e.text) == 0 }

// insert adds s at the cursor. Line breaks are kept as "\n"; other control
// characters, and characters that reorder what a terminal shows, are left
// out: they cannot be drawn in the box.
func (e *editor) insert(s string) {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	var add []rune
	for _, r := range s {
		if r == unicode.ReplacementChar || unsafeRune(r) {
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

func (e *editor) remove(from, to int) {
	from, to = max(from, 0), min(to, len(e.text))
	if from >= to {
		return
	}
	e.text = append(append([]rune(nil), e.text[:from]...), e.text[to:]...)
	e.pos = from
}

func (e *editor) backspace() { e.remove(e.pos-1, e.pos) }
func (e *editor) del()       { p := e.pos; e.remove(p, p+1); e.pos = min(p, len(e.text)) }

func (e *editor) left()  { e.pos = max(e.pos-1, 0) }
func (e *editor) right() { e.pos = min(e.pos+1, len(e.text)) }

// lineStart and lineEnd are the bounds of the line the cursor is on.
func (e *editor) lineStart() int {
	i := e.pos
	for i > 0 && e.text[i-1] != '\n' {
		i--
	}
	return i
}

func (e *editor) lineEnd() int {
	i := e.pos
	for i < len(e.text) && e.text[i] != '\n' {
		i++
	}
	return i
}

func (e *editor) home() { e.pos = e.lineStart() }
func (e *editor) end()  { e.pos = e.lineEnd() }

func (e *editor) wordStart() int {
	i := e.pos
	for i > 0 && unicode.IsSpace(e.text[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(e.text[i-1]) {
		i--
	}
	return i
}

func (e *editor) wordLeft() { e.pos = e.wordStart() }

func (e *editor) wordRight() {
	i := e.pos
	for i < len(e.text) && unicode.IsSpace(e.text[i]) {
		i++
	}
	for i < len(e.text) && !unicode.IsSpace(e.text[i]) {
		i++
	}
	e.pos = i
}

func (e *editor) killWord()  { e.remove(e.wordStart(), e.pos) }
func (e *editor) killStart() { e.remove(e.lineStart(), e.pos) }
func (e *editor) killEnd()   { p := e.pos; e.remove(p, e.lineEnd()); e.pos = p }

func (e *editor) set(text []rune) {
	e.text = append([]rune(nil), text...)
	e.pos = len(e.text)
}

func (e *editor) clear() { e.text, e.pos, e.at, e.draft = nil, 0, len(e.history), nil }

// up moves to the line above, or from the first line to the prompt sent
// before this one.
func (e *editor) up() {
	start := e.lineStart()
	if start > 0 {
		col := e.pos - start
		e.pos = start - 1
		e.pos = min(e.lineStart()+col, e.pos)
		return
	}
	if e.at == 0 {
		return
	}
	if e.at == len(e.history) {
		e.draft = append([]rune(nil), e.text...)
	}
	e.at--
	e.set(e.history[e.at])
}

// down moves to the line below, or from the last line to the newer prompt.
func (e *editor) down() {
	end := e.lineEnd()
	if end < len(e.text) {
		col := e.pos - e.lineStart()
		e.pos = end + 1
		e.pos = min(e.pos+col, e.lineEnd())
		return
	}
	if e.at >= len(e.history) {
		return
	}
	e.at++
	if e.at == len(e.history) {
		e.set(e.draft)
		return
	}
	e.set(e.history[e.at])
}

// take returns what was typed and empties the editor. remember adds it to
// the history.
func (e *editor) take(remember bool) string {
	text := string(e.text)
	if remember && strings.TrimSpace(text) != "" &&
		(len(e.history) == 0 || string(e.history[len(e.history)-1]) != text) {
		e.history = append(e.history, append([]rune(nil), e.text...))
	}
	e.clear()
	return text
}
