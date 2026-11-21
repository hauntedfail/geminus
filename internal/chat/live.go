package chat

// The live part of the screen. Everything geminus prints scrolls above it;
// the prompt box and the status around it stay at the bottom and are redrawn
// in place. Typing is handled here, on the goroutine that reads the keyboard,
// so the box stays responsive while the main loop is talking to Honcho.

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	// cancelled is the line delivered when a question is cancelled with
	// Ctrl-C. No typed line can be equal to it: the editor drops control
	// characters.
	cancelled = "\x03"
	// quitWindow is how long after one Ctrl-C a second one quits.
	quitWindow = 2 * time.Second

	syncStart   = "\x1b[?2026h" // draw the following as one update, where supported
	syncEnd     = "\x1b[?2026l"
	pasteModeOn = "\x1b[?2004h" // mark pasted text, so a pasted line break is not Enter
	pasteModeOf = "\x1b[?2004l"
)

type live struct {
	// park, when set, is asked after each entered line whether input is to
	// be read on. It may wait before it answers.
	park func(line string) (goOn bool)

	mu  sync.Mutex
	out io.Writer
	pal palette
	now func() time.Time
	// size reports the terminal's size; nil when it cannot change.
	size       func() (cols, rows int)
	cols, rows int

	ed   editor
	v    view
	tail string
	spin int

	notice      string
	noticeUntil time.Time
	interrupted time.Time

	// What is on screen: the cursor's row within the drawn rows, and the
	// drawn text itself.
	curRow int
	last   string
	closed bool
}

func newLive(out io.Writer, cols, rows int, pal palette, now func() time.Time) *live {
	l := &live{out: out, cols: cols, rows: rows, pal: pal, now: now}
	l.mu.Lock()
	defer l.mu.Unlock()
	io.WriteString(out, pasteModeOn)
	l.draw("")
	return l
}

func (l *live) width() int { return max(l.cols-1, 24) }

// Write prints p above the box. Text that does not end a line yet is kept
// just above the box until its line ends.
func (l *live) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.out.Write(p)
	}
	text := l.tail + string(p)
	commit, tail := "", text
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		commit, tail = text[:i+1], text[i+1:]
	}
	// A very long unfinished line would push the box off the screen: its
	// first rows are let go as lines of their own.
	if rows, most := wrapText(tail, l.width()), max(1, min(8, l.rows/4)); len(rows) > most {
		commit += strings.Join(rows[:len(rows)-most], "\n") + "\n"
		tail = strings.Join(rows[len(rows)-most:], "")
	}
	l.tail = tail
	l.draw(commit)
	return len(p), nil
}

// draw replaces what is drawn with the current frame, after printing commit
// (whole lines) above it. It does nothing when nothing changed.
func (l *live) draw(commit string) {
	if l.closed {
		return
	}
	if l.notice != "" && !l.now().Before(l.noticeUntil) {
		l.notice = ""
	}
	f := render(frameInput{v: l.v, text: l.ed.text, pos: l.ed.pos, tail: l.tail, notice: l.notice,
		cols: l.cols, rows: l.rows, now: l.now(), spin: l.spin, pal: l.pal})
	// Never taller than the screen: the rows are found again by counting up
	// from the cursor.
	if over := len(f.rows) - max(l.rows-1, 4); over > 0 {
		f.rows, f.curRow = f.rows[over:], max(f.curRow-over, 0)
	}
	body := strings.Join(f.rows, "\r\n")
	if up := len(f.rows) - 1 - f.curRow; up > 0 {
		body += fmt.Sprintf("\x1b[%dA", up)
	}
	body += "\r"
	if f.curCol > 0 {
		body += fmt.Sprintf("\x1b[%dC", f.curCol)
	}
	if commit == "" && body == l.last {
		return
	}
	var b strings.Builder
	b.WriteString(syncStart)
	b.WriteString(l.erase())
	b.WriteString(strings.ReplaceAll(commit, "\n", "\r\n"))
	b.WriteString(body)
	b.WriteString(syncEnd)
	io.WriteString(l.out, b.String())
	l.last, l.curRow = body, f.curRow
}

// erase moves to the first drawn row and clears from there down.
func (l *live) erase() string {
	s := "\r\x1b[J"
	if l.curRow > 0 {
		s = fmt.Sprintf("\x1b[%dA", l.curRow) + s
	}
	return s
}

// set replaces what the status around the box shows.
func (l *live) set(v view) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if v.ask != l.v.ask {
		// A question starts with an empty box, and so does what follows it.
		l.ed.clear()
	}
	l.v = v
	l.draw("")
}

// tick advances the spinner, follows the terminal's size, and redraws when
// what is shown has changed (ages, a notice that ran out).
func (l *live) tick() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if l.size != nil {
		if cols, rows := l.size(); cols > 0 && rows > 0 && (cols != l.cols || rows != l.rows) {
			// Redrawn from where the box started. A terminal that rewraps
			// what is on screen when it gets narrower can leave part of the
			// old box above the new one; guessing how far it moved would
			// risk erasing output instead.
			l.cols, l.rows, l.last = cols, rows, ""
		}
	}
	if l.v.waiting > 0 {
		l.spin++
	}
	l.draw("")
}

func (l *live) say(notice string) {
	l.notice, l.noticeUntil = notice, l.now().Add(quitWindow)
}

// key applies one key. It returns a line when one was entered, and quit when
// the person asked to leave.
func (l *live) key(k key) (line string, entered, quit bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return "", false, false
	}
	e := &l.ed
	asking := l.v.ask != ""
	if k.kind != keyInterrupt {
		l.notice = ""
	}
	switch k.kind {
	case keyRune:
		e.insert(string(k.r))
	case keyPaste:
		e.insert(k.text)
	case keyNewline:
		if !asking {
			e.insert("\n")
		}
	case keyEnter:
		switch {
		case !asking && e.pos > 0 && e.text[e.pos-1] == '\\':
			// A backslash before Enter continues on a new line.
			e.backspace()
			e.insert("\n")
		case asking || strings.TrimSpace(e.String()) != "":
			line, entered = e.take(!asking), true
		}
	case keyBackspace:
		e.backspace()
	case keyDelete:
		e.del()
	case keyLeft:
		e.left()
	case keyRight:
		e.right()
	case keyUp:
		e.up()
	case keyDown:
		e.down()
	case keyHome:
		e.home()
	case keyEnd:
		e.end()
	case keyWordLeft:
		e.wordLeft()
	case keyWordRight:
		e.wordRight()
	case keyKillWord:
		e.killWord()
	case keyKillStart:
		e.killStart()
	case keyKillEnd:
		e.killEnd()
	case keyTab:
		l.complete()
	case keyEsc:
		// Nothing to dismiss; kept so that Esc is not typed into the box.
	case keyRedraw:
		l.last = ""
	case keyEOF:
		if e.empty() {
			quit = true
		} else {
			e.del()
		}
	case keyInterrupt:
		switch {
		case !e.empty():
			e.clear()
		case asking:
			line, entered = cancelled, true
		case l.now().Sub(l.interrupted) < quitWindow:
			quit = true
		default:
			l.interrupted = l.now()
			l.say("Press Ctrl-C again to quit geminus (agents keep working; answers stay in Honcho)")
		}
	}
	l.draw("")
	return line, entered, quit
}

// complete finishes the geminus command being typed, as far as it is
// unambiguous.
func (l *live) complete() {
	word, ok := slashWord(l.ed.text)
	if !ok || strings.HasPrefix(word, "//") {
		return
	}
	found := slashMatches(word)
	if len(found) == 0 {
		return
	}
	common := slashCommands[found[0]].name
	for _, i := range found[1:] {
		name := slashCommands[i].name
		n := 0
		for n < len(common) && n < len(name) && common[n] == name[n] {
			n++
		}
		common = common[:n]
	}
	if len(found) == 1 && slashCommands[found[0]].args != "" {
		common += " "
	}
	if len(common) > len(word) {
		l.ed.set([]rune(common))
	}
}

// close removes the box and leaves the cursor on a fresh line.
func (l *live) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	end := l.erase()
	if l.tail != "" {
		end += l.tail + "\r\n"
	}
	io.WriteString(l.out, end+pasteModeOf)
	l.closed = true
}
