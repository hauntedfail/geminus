package chat

// The live full-screen console. geminus runs inside the terminal's alternate
// screen buffer so scrolling never leaks into the shell's prior log. Row 0 is
// a sticky one-line prompt header, the middle is a scrollable transcript
// viewport, and the prompt box + status stay anchored at the bottom. Typing is
// handled on the keyboard goroutine so the box stays responsive while the main
// loop talks to Honcho.

import (
	"context"
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

	// maxTranscriptLines caps retained logical transcript lines so long
	// sessions stay bounded in memory.
	maxTranscriptLines = 20000

	altScreenOn  = "\x1b[?1049h" // switch to alternate screen buffer (isolates shell scrollback)
	altScreenOff = "\x1b[?1049l" // restore original terminal screen on exit
	cursorHide   = "\x1b[?25l"   // hide cursor while navigating a select menu
	cursorShow   = "\x1b[?25h"   // restore cursor visibility
	cursorHome   = "\x1b[H"
	clearLineEnd = "\x1b[K"
	syncStart    = "\x1b[?2026h" // draw the following as one update, where supported
	syncEnd      = "\x1b[?2026l"
	pasteModeOn  = "\x1b[?2004h" // mark pasted text, so a pasted line break is not Enter
	pasteModeOf  = "\x1b[?2004l"
)

type transcriptLine struct {
	text   string
	prompt string // one-line user prompt active when this line was written
}

type wrappedLine struct {
	text   string
	prompt string
}

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

	ed        editor
	v         view
	menu      *selectMenu
	lines     []transcriptLine
	tail      string
	curPrompt string
	scroll    int // wrapped rows scrolled up from bottom (0 = auto-follow)
	viewRows  int // last viewport height, for PageUp/PageDown step size
	spin      int

	notice      string
	noticeUntil time.Time
	interrupted time.Time

	last   string
	closed bool
}

func newLive(out io.Writer, cols, rows int, pal palette, now func() time.Time) *live {
	l := &live{out: out, cols: cols, rows: rows, pal: pal, now: now}
	l.mu.Lock()
	defer l.mu.Unlock()
	io.WriteString(out, altScreenOn+pasteModeOn)
	l.draw()
	return l
}

func (l *live) width() int { return max(l.cols-1, 24) }

// setPrompt records the one-line user prompt for subsequent transcript lines.
func (l *live) setPrompt(prompt string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.curPrompt = oneLine(prompt)
	l.draw()
}

// wrappedCount returns the total number of wrapped rows for the current
// transcript and unfinished tail at width w.
func (l *live) wrappedCount(w int) int {
	n := 0
	for _, entry := range l.lines {
		n += len(wrapText(entry.text, w))
	}
	if l.tail != "" {
		n += len(wrapText(l.tail, w))
	}
	return n
}

func (l *live) wrapAll(w int) []wrappedLine {
	out := make([]wrappedLine, 0, len(l.lines)+1)
	for _, entry := range l.lines {
		for _, row := range wrapText(entry.text, w) {
			out = append(out, wrappedLine{text: row, prompt: entry.prompt})
		}
	}
	if l.tail != "" {
		for _, row := range wrapText(l.tail, w) {
			out = append(out, wrappedLine{text: row, prompt: l.curPrompt})
		}
	}
	return out
}

// Write appends p to the transcript buffer and redraws the viewport.
// Text that does not end with a newline stays in tail until completed.
func (l *live) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.out.Write(p)
	}
	w := l.width()
	before := 0
	if l.scroll > 0 {
		before = l.wrappedCount(w)
	}

	text := l.tail + string(p)
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		completed := strings.Split(text[:i], "\n")
		for _, line := range completed {
			l.lines = append(l.lines, transcriptLine{text: line, prompt: l.curPrompt})
		}
		l.tail = text[i+1:]
		if over := len(l.lines) - maxTranscriptLines; over > 0 {
			l.lines = append([]transcriptLine(nil), l.lines[over:]...)
		}
	} else {
		l.tail = text
	}

	if l.scroll > 0 {
		after := l.wrappedCount(w)
		if delta := after - before; delta > 0 {
			l.scroll += delta
		}
	}
	l.draw()
	return len(p), nil
}

// activeHeaderPrompt picks which user prompt the sticky top bar should display
// for the currently visible slice of wrapped lines.
func (l *live) activeHeaderPrompt(wrapped []wrappedLine, start, end int) string {
	if l.scroll == 0 {
		if l.curPrompt != "" {
			return l.curPrompt
		}
		for i := end - 1; i >= start; i-- {
			if wrapped[i].prompt != "" {
				return wrapped[i].prompt
			}
		}
		return ""
	}
	if start < end && wrapped[start].prompt != "" {
		return wrapped[start].prompt
	}
	for i := start - 1; i >= 0; i-- {
		if wrapped[i].prompt != "" {
			return wrapped[i].prompt
		}
	}
	return ""
}

// draw renders the entire alternate-screen frame (sticky header + viewport +
// prompt box) and writes it atomically when anything changed.
func (l *live) draw() {
	if l.closed {
		return
	}
	if l.notice != "" && !l.now().Before(l.noticeUntil) {
		l.notice = ""
	}
	in := frameInput{v: l.v, text: l.ed.text, pos: l.ed.pos, notice: l.notice,
		cols: l.cols, rows: l.rows, now: l.now(), spin: l.spin, pal: l.pal}
	var f frame
	hideCursor := false
	if l.menu != nil {
		f = renderMenu(in, l.menu)
		hideCursor = !l.menu.customMode
	} else {
		f = render(in)
	}
	// Reserve at least row 0 for the sticky header and keep total height <= l.rows.
	maxBottom := max(l.rows-2, 1)
	if over := len(f.rows) - maxBottom; over > 0 {
		f.rows, f.curRow = f.rows[over:], max(f.curRow-over, 0)
	}
	viewHeight := max(l.rows-1-len(f.rows), 0)
	l.viewRows = viewHeight

	wrapped := l.wrapAll(l.width())
	maxScroll := max(len(wrapped)-viewHeight, 0)
	if l.scroll > maxScroll {
		l.scroll = maxScroll
	}
	if l.scroll < 0 {
		l.scroll = 0
	}
	end := len(wrapped) - l.scroll
	start := max(end-viewHeight, 0)
	visible := wrapped[start:end]

	headerPrompt := l.activeHeaderPrompt(wrapped, start, end)
	header := renderHeader(headerInput{
		v:      l.v,
		prompt: headerPrompt,
		scroll: l.scroll,
		atTop:  maxScroll > 0 && l.scroll == maxScroll,
		cols:   l.cols,
		pal:    l.pal,
	})

	var body strings.Builder
	body.WriteString(header)
	body.WriteString(clearLineEnd)
	body.WriteString("\r\n")

	for i := 0; i < viewHeight; i++ {
		if i < len(visible) {
			body.WriteString(visible[i].text)
		}
		body.WriteString(clearLineEnd)
		body.WriteString("\r\n")
	}

	for i, row := range f.rows {
		body.WriteString(row)
		body.WriteString(clearLineEnd)
		if i < len(f.rows)-1 {
			body.WriteString("\r\n")
		}
	}

	curRow := 1 + viewHeight + f.curRow
	body.WriteString(fmt.Sprintf("\x1b[%d;%dH", curRow+1, f.curCol+1))
	if hideCursor {
		body.WriteString(cursorHide)
	} else {
		body.WriteString(cursorShow)
	}

	rendered := body.String()
	if rendered == l.last {
		return
	}
	var out strings.Builder
	out.WriteString(syncStart)
	out.WriteString(cursorHome)
	out.WriteString(rendered)
	out.WriteString(syncEnd)
	io.WriteString(l.out, out.String())
	l.last = rendered
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
	l.draw()
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
			l.cols, l.rows, l.last = cols, rows, ""
		}
	}
	if l.v.waiting > 0 {
		l.spin++
	}
	l.draw()
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
	if l.menu != nil {
		switch k.kind {
		case keyPageUp:
			step := max(l.viewRows-2, 1)
			l.scroll += step
		case keyPageDown:
			step := max(l.viewRows-2, 1)
			l.scroll = max(l.scroll-step, 0)
		case keyScrollUp:
			l.scroll++
		case keyScrollDown:
			l.scroll = max(l.scroll-1, 0)
		case keyRedraw:
			l.last = ""
		default:
			if l.menu.handleKey(k) {
				l.menu = nil
			}
		}
		l.draw()
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
			if !asking {
				l.scroll = 0
			}
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
	case keyPageUp:
		step := max(l.viewRows-2, 1)
		l.scroll += step
	case keyPageDown:
		step := max(l.viewRows-2, 1)
		l.scroll = max(l.scroll-step, 0)
	case keyScrollUp:
		l.scroll++
	case keyScrollDown:
		l.scroll = max(l.scroll-1, 0)
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
	l.draw()
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

// selectOption opens an interactive select menu in the bottom panel and
// waits until the user picks an item, enters a custom value, or cancels.
func (l *live) selectOption(ctx context.Context, cfg menuConfig) (string, bool) {
	done := make(chan menuResult, 1)
	m := newSelectMenu(cfg, done)
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return "", false
	}
	l.menu = m
	l.draw()
	l.mu.Unlock()

	select {
	case res := <-done:
		return res.value, res.ok
	case <-ctx.Done():
		l.mu.Lock()
		if l.menu == m {
			l.menu = nil
			l.draw()
		}
		l.mu.Unlock()
		return "", false
	}
}

// close leaves the alternate screen buffer and restores paste mode.
func (l *live) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if l.menu != nil {
		l.menu.finish("", false)
		l.menu = nil
	}
	io.WriteString(l.out, cursorShow+altScreenOff+pasteModeOf)
	l.closed = true
}
