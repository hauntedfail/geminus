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

	altScreenOn     = "\x1b[?1049h" // switch to alternate screen buffer (isolates shell scrollback)
	altScreenOff    = "\x1b[?1049l" // restore original terminal screen on exit
	cursorHide      = "\x1b[?25l"   // hide cursor while navigating a select menu
	cursorShow      = "\x1b[?25h"   // restore cursor visibility
	cursorDefault   = "\x1b[0 q"    // restore default terminal cursor shape
	cursorBlock     = "\x1b[2 q"    // steady block (Post-Human Normal)
	cursorUnderline = "\x1b[4 q"    // steady underline (Post-Human Select)
	cursorBar       = "\x1b[6 q"    // steady bar (Post-Human Insert)
	cursorHome      = "\x1b[H"
	clearLineEnd    = "\x1b[K"
	syncStart       = "\x1b[?2026h" // draw the following as one update, where supported
	syncEnd         = "\x1b[?2026l"
	pasteModeOn     = "\x1b[?2004h" // mark pasted text, so a pasted line break is not Enter
	pasteModeOf     = "\x1b[?2004l"
	// altScrollOn asks the terminal to send the wheel as the up and down
	// arrow keys while the alternate screen is shown. geminus never asks for
	// mouse reports: a terminal that sends them stops selecting text on a
	// drag and shows another pointer, and there is no report for the wheel
	// alone. The mode is left on at exit, since what it was before cannot be
	// read back and it acts on the alternate screen only.
	altScrollOn      = "\x1b[?1007h"
	mouseScrollLines = 3 // rows per wheel report, from a terminal that sends them unasked
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

	ed           editor
	hx           helix
	cursorStyled bool
	v            view
	menu         *selectMenu
	lines        []transcriptLine
	tail         string
	preview      string
	curPrompt    string
	scroll       int // wrapped rows scrolled up from bottom (0 = auto-follow)
	viewRows     int // last viewport height, for PageUp/PageDown step size
	spin         int

	notice      string
	noticeUntil time.Time
	interrupted time.Time

	last   string
	closed bool
	// shortcuts is the expanded key list, opened with ? on an empty prompt.
	shortcuts bool

	// splash is the opening screen while it covers everything else.
	splash *splashScreen
	// banner is the mascot at the top of the transcript; nil for none.
	banner *banner
	// choice replaces the screen while a list is being picked, so the list
	// is not written into the transcript.
	choice *choice
	// onClose, when set, returns terminal cleanup sequences (e.g. clearing
	// OSC 9;4 progress and reporting an Orca session boundary) before exit.
	onClose func() string
}

func newLive(out io.Writer, cols, rows int, pal palette, now func() time.Time) *live {
	return newLiveBehind(out, cols, rows, pal, now, nil)
}

// newLiveBehind is newLive with an opening screen over it; nil for none.
func newLiveBehind(out io.Writer, cols, rows int, pal palette, now func() time.Time, sp *splashScreen) *live {
	return newLiveWith(out, cols, rows, pal, now, sp, false)
}

func newLiveWith(out io.Writer, cols, rows int, pal palette, now func() time.Time, sp *splashScreen, clawd bool) *live {
	l := &live{out: out, cols: cols, rows: rows, pal: pal, now: now, splash: sp, v: view{clawd: clawd}}
	l.mu.Lock()
	defer l.mu.Unlock()
	io.WriteString(out, altScreenOn+pasteModeOn+altScrollOn)
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

// hasDraft reports whether the user currently has unsent text in the prompt box.
func (l *live) hasDraft() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ed.Text()) > 0
}

// bannerRowCount returns how many transcript rows the banner occupies.
func (l *live) bannerRowCount() int {
	if l.banner == nil {
		return 0
	}
	if l.v.clawd {
		return clawdBannerRowCount
	}
	return bannerRows
}

// wrappedCount returns the total number of wrapped rows for the current
// transcript and unfinished tail at width w.
func (l *live) wrappedCount(w int) int {
	n := l.bannerRowCount()
	for _, entry := range l.lines {
		n += len(wrapText(entry.text, w))
	}
	if tail := l.combinedTail(); tail != "" {
		n += len(wrapText(tail, w))
	}
	return n
}

func (l *live) wrapAll(w int) []wrappedLine {
	out := make([]wrappedLine, 0, len(l.lines)+1+l.bannerRowCount())
	if l.banner != nil {
		var rows []string
		if l.v.clawd {
			rows = clawdBannerRows(l.v, l.banner.mode, l.pal, w)
		} else {
			// Drawn anew each time: the mascot moves.
			rows = l.banner.rows(l.now(), w)
		}
		for _, row := range rows {
			out = append(out, wrappedLine{text: row})
		}
	}
	for _, entry := range l.lines {
		for _, row := range wrapText(entry.text, w) {
			out = append(out, wrappedLine{text: row, prompt: entry.prompt})
		}
	}
	if tail := l.combinedTail(); tail != "" {
		for _, row := range wrapText(tail, w) {
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
	if l.splash != nil {
		l.drawSplash()
		return
	}
	if l.choice != nil {
		l.drawChoice()
		return
	}
	if l.notice != "" && !l.now().Before(l.noticeUntil) {
		l.notice = ""
	}
	postHuman := l.v.postHuman && l.v.ask == ""
	selStart, selEnd, selActive := 0, 0, false
	if postHuman {
		selStart, selEnd, selActive = l.hx.SelectionRange(len(l.ed.Text()))
	}
	in := frameInput{
		v:            l.v,
		text:         l.ed.Text(),
		pos:          l.ed.Pos(),
		postHuman:    postHuman,
		helixMode:    l.hx.Mode(),
		helixPending: l.hx.PendingLabel(),
		selStart:     selStart,
		selEnd:       selEnd,
		selActive:    selActive,
		notice:       l.notice,
		cols:         l.cols,
		rows:         l.rows,
		now:          l.now(),
		spin:         l.spin,
		pal:          l.pal,
		shortcuts:    l.shortcuts,
	}
	var f frame
	hideCursor := false
	if l.menu != nil {
		f = renderMenu(in, l.menu)
		hideCursor = !l.menu.customMode
	} else {
		f = render(in)
	}
	// Reserve row 0 for the sticky header unless Clawd mode is at the bottom
	// (real Claude Code shows no sticky bar above the transcript at rest).
	showHeader := !l.v.clawd || l.scroll > 0
	topRows := 0
	if showHeader {
		topRows = 1
	}
	maxBottom := max(l.rows-topRows-1, 1)
	if over := len(f.rows) - maxBottom; over > 0 {
		f.rows, f.curRow = f.rows[over:], max(f.curRow-over, 0)
	}
	viewHeight := max(l.rows-topRows-len(f.rows), 0)
	l.viewRows = viewHeight

	wrapped := l.wrapAll(l.width())
	maxScroll := max(len(wrapped)-viewHeight, 0)
	if l.scroll > maxScroll {
		l.scroll = maxScroll
	}
	if l.scroll < 0 {
		l.scroll = 0
	}
	// If clamping scroll dropped it back to 0 in Clawd mode, drop the header row.
	if l.v.clawd && showHeader && l.scroll == 0 {
		showHeader, topRows = false, 0
		viewHeight = max(l.rows-len(f.rows), 0)
		l.viewRows = viewHeight
	}
	end := len(wrapped) - l.scroll
	start := max(end-viewHeight, 0)
	visible := wrapped[start:end]

	var body strings.Builder
	if showHeader {
		hin := headerInput{
			v:      l.v,
			prompt: l.activeHeaderPrompt(wrapped, start, end),
			scroll: l.scroll,
			atTop:  maxScroll > 0 && l.scroll == maxScroll,
			cols:   l.cols,
			pal:    l.pal,
		}
		header := renderHeader(hin)
		if l.v.clawd {
			header = renderClawdHeader(hin)
		}
		body.WriteString(header)
		body.WriteString(clearLineEnd)
		body.WriteString("\r\n")
	}

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

	curRow := topRows + viewHeight + f.curRow
	body.WriteString(fmt.Sprintf("\x1b[%d;%dH", curRow+1, f.curCol+1))
	if hideCursor {
		body.WriteString(cursorHide)
	} else {
		if postHuman {
			switch l.hx.Mode() {
			case helixNormal:
				body.WriteString(cursorBlock)
			case helixSelect:
				body.WriteString(cursorUnderline)
			default:
				body.WriteString(cursorBar)
			}
			l.cursorStyled = true
		} else if l.cursorStyled {
			body.WriteString(cursorDefault)
			l.cursorStyled = false
		}
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
		l.ed.Clear()
		l.hx.ResetForNewPrompt()
	} else if v.postHuman != l.v.postHuman {
		l.hx.ResetForNewPrompt()
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
	if l.splash != nil && !l.splashKey(k) {
		return "", false, false
	}
	if l.choice != nil {
		l.choiceKey(k)
		return "", false, false
	}
	if l.menu != nil {
		if l.menu.cfg.details != "" && (k.Kind == keyPageUp || k.Kind == keyPageDown || k.Kind == keyMouseScrollUp || k.Kind == keyMouseScrollDown) {
			l.menu.handleKey(k)
			l.draw()
			return "", false, false
		}
		switch k.Kind {
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
		case keyMouseScrollUp:
			l.scroll += mouseScrollLines
		case keyMouseScrollDown:
			l.scroll = max(l.scroll-mouseScrollLines, 0)
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
	// The up and down arrows scroll what was said. The wheel arrives as these
	// keys (altScrollOn) and cannot be told from them, so they never move the
	// cursor or bring back a prompt: Ctrl-P and Ctrl-N do that.
	if k.Ctrl == 0 {
		switch k.Kind {
		case keyUp:
			k.Kind = keyScrollUp
		case keyDown:
			k.Kind = keyScrollDown
		}
	}
	e := &l.ed
	asking := l.v.ask != ""
	if k.Kind == keyRune && k.R == '?' && !asking && e.Empty() {
		l.notice = ""
		l.shortcuts = !l.shortcuts
		l.draw()
		return "", false, false
	}
	if l.shortcuts && k.Kind == keyEsc {
		l.shortcuts = false
		l.draw()
		return "", false, false
	}
	if l.shortcuts && k.Kind == keyRune {
		l.shortcuts = false
	}
	if k.Kind != keyInterrupt {
		l.notice = ""
	}
	if l.v.postHuman && !asking {
		act := l.hx.HandleKey(e, k, false)
		switch act {
		case helixSubmit:
			if strings.TrimSpace(e.String()) != "" {
				line, entered = e.Take(true), true
				l.scroll = 0
				l.hx.ResetForNewPrompt()
			}
		case helixComplete:
			l.complete()
		case helixRedraw:
			l.last = ""
		case helixScrollUp:
			step := 1
			if k.Kind == keyMouseScrollUp {
				step = mouseScrollLines
			}
			l.scroll += step
		case helixScrollDown:
			step := 1
			if k.Kind == keyMouseScrollDown {
				step = mouseScrollLines
			}
			l.scroll = max(l.scroll-step, 0)
		case helixScrollHalfUp:
			step := max(l.viewRows/2, 1)
			l.scroll += step
		case helixScrollHalfDown:
			step := max(l.viewRows/2, 1)
			l.scroll = max(l.scroll-step, 0)
		case helixScrollPageUp:
			step := max(l.viewRows-2, 1)
			l.scroll += step
		case helixScrollPageDown:
			step := max(l.viewRows-2, 1)
			l.scroll = max(l.scroll-step, 0)
		case helixScrollTop:
			l.scroll = max(l.wrappedCount(l.width())-l.viewRows, 0)
		case helixScrollBottom:
			l.scroll = 0
		case helixEOF:
			if e.Empty() {
				quit = true
			} else {
				e.Del()
			}
		case helixInterrupt:
			switch {
			case !e.Empty():
				e.Clear()
				l.hx.ResetForNewPrompt()
			case l.now().Sub(l.interrupted) < quitWindow:
				quit = true
			default:
				l.interrupted = l.now()
				if l.v.clawd {
					l.say("Press Ctrl-C again to exit")
				} else {
					l.say("Press Ctrl-C again to quit geminus (agents keep working; answers stay in Honcho)")
				}
			}
		}
		l.draw()
		return line, entered, quit
	}
	switch k.Kind {
	case keyRune:
		e.Insert(string(k.R))
	case keyPaste:
		e.Insert(k.Text)
	case keyNewline:
		if !asking {
			e.Insert("\n")
		}
	case keyEnter:
		switch {
		case !asking && e.Pos() > 0 && e.Text()[e.Pos()-1] == '\\':
			// A backslash before Enter continues on a new line.
			e.Backspace()
			e.Insert("\n")
		case asking || strings.TrimSpace(e.String()) != "":
			line, entered = e.Take(!asking), true
			if !asking {
				l.scroll = 0
			}
		}
	case keyBackspace:
		e.Backspace()
	case keyDelete:
		e.Del()
	case keyLeft:
		e.Left()
	case keyRight:
		e.Right()
	case keyUp:
		e.Up()
	case keyDown:
		e.Down()
	case keyHome:
		e.Home()
	case keyEnd:
		e.End()
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
	case keyMouseScrollUp:
		l.scroll += mouseScrollLines
	case keyMouseScrollDown:
		l.scroll = max(l.scroll-mouseScrollLines, 0)
	case keyWordLeft:
		e.WordLeft()
	case keyWordRight:
		e.WordRight()
	case keyKillWord:
		e.KillWord()
	case keyKillStart:
		e.KillStart()
	case keyKillEnd:
		e.KillEnd()
	case keyTab:
		l.complete()
	case keyEsc:
		// Nothing to dismiss; kept so that Esc is not typed into the box.
	case keyRedraw:
		l.last = ""
	case keyEOF:
		if e.Empty() {
			quit = true
		} else {
			e.Del()
		}
	case keyInterrupt:
		switch {
		case !e.Empty():
			e.Clear()
		case asking:
			line, entered = cancelled, true
		case l.now().Sub(l.interrupted) < quitWindow:
			quit = true
		default:
			l.interrupted = l.now()
			if l.v.clawd {
				l.say("Press Ctrl-C again to exit")
			} else {
				l.say("Press Ctrl-C again to quit geminus (agents keep working; answers stay in Honcho)")
			}
		}
	}
	l.draw()
	return line, entered, quit
}

// complete finishes the geminus command being typed, as far as it is
// unambiguous.
func (l *live) complete() {
	word, ok := slashWord(l.ed.Text())
	if !ok || strings.HasPrefix(word, "//") {
		return
	}
	// /mo historically completed /model. Keep that shortcut now /mode
	// shares its prefix; /mode itself remains an exact command.
	if word == "/mo" || word == "/mod" {
		l.ed.Set([]rune("/model "))
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
	if len(found) == 1 && slashCommands[found[0]].args != "" || word == "/mode" {
		common += " "
	}
	if len(common) > len(word) {
		l.ed.Set([]rune(common))
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

// emit writes a non-printing terminal control sequence (OSC / BEL) directly
// to the underlying terminal under l.mu so it never interleaves with a redraw.
func (l *live) emit(seq string) {
	if seq == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	io.WriteString(l.out, seq)
}

// close leaves the alternate screen buffer and switches paste marking off.
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
	reset := ""
	if l.cursorStyled {
		reset = cursorDefault
		l.cursorStyled = false
	}
	cleanup := ""
	if l.onClose != nil {
		cleanup = l.onClose()
	}
	io.WriteString(l.out, cleanup+reset+cursorShow+altScreenOff+pasteModeOf)
	l.closed = true
}
