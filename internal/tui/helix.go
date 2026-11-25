package tui

// Post-Human mode: Helix-style selection-first modal editing for the prompt box.
//
// Helix's editing model is selection -> action (Normal, Insert, Select modes)
// rather than Vim's operator-pending model. Bindings that do not apply to a
// chat prompt (file/buffer pickers, window splits, LSP, shell pipes, macros,
// command-line `:` mode) are omitted; viewport motions (Ctrl-u/d/b/f and `z`)
// scroll the transcript viewport, Enter submits the prompt, and `/` on an empty
// prompt enters Insert mode so geminus slash commands work effortlessly.

import (
	"strconv"
	"strings"
	"unicode"
)

// HelixMode is the active modal state when Post-Human mode is enabled.
type HelixMode int

const (
	HelixInsert HelixMode = iota
	HelixNormal
	HelixSelect
)

// Label returns the compact 3-letter mode indicator shown on the prompt border.
func (m HelixMode) Label() string {
	switch m {
	case HelixNormal:
		return "NOR"
	case HelixSelect:
		return "SEL"
	default:
		return "INS"
	}
}

// HelixAction tells the caller (live console) when a key triggers a host action
// outside the prompt buffer itself.
type HelixAction int

const (
	HelixNone HelixAction = iota
	HelixSubmit
	HelixInterrupt
	HelixEOF
	HelixComplete
	HelixRedraw
	HelixScrollUp
	HelixScrollDown
	HelixScrollHalfUp
	HelixScrollHalfDown
	HelixScrollPageUp
	HelixScrollPageDown
	HelixScrollTop
	HelixScrollBottom
)

type helixPending int

const (
	pendingNone helixPending = iota
	pendingGoto              // g
	pendingMatch             // m
	pendingMatchInner        // mi
	pendingMatchAround       // ma
	pendingSurroundAdd       // ms
	pendingSurroundDel       // md
	pendingSurroundRep1      // mr
	pendingSurroundRep2      // mr<from>
	pendingView              // z
	pendingFindNext          // f
	pendingFindPrev          // F
	pendingTillNext          // t
	pendingTillPrev          // T
	pendingReplaceChar       // r
)

type helixSnapshot struct {
	text   []rune
	anchor int
	head   int
}

type lastFindMotion struct {
	kind helixPending
	ch   rune
}

const (
	helixMaxUndo  = 100
	helixMaxCount = 1000
)

// Helix holds the modal editing state layered on top of Editor.
type Helix struct {
	mode        HelixMode
	anchor      int
	head        int
	pending     helixPending
	pendingChar rune // first delimiter for `mr<from><to>`
	count       int

	yank     []rune
	yankLine bool

	lastFind lastFindMotion

	undoStack    []helixSnapshot
	redoStack    []helixSnapshot
	insertBefore *helixSnapshot
}

// Mode returns the current Helix mode.
func (h *Helix) Mode() HelixMode { return h.mode }

// PendingLabel returns any active count prefix or minor-mode key sequence
// (for example "g", "mi", "mr(", "3") for display next to the mode badge.
func (h *Helix) PendingLabel() string {
	var b strings.Builder
	if h.count > 0 {
		b.WriteString(strconv.Itoa(h.count))
	}
	switch h.pending {
	case pendingGoto:
		b.WriteString("g")
	case pendingMatch:
		b.WriteString("m")
	case pendingMatchInner:
		b.WriteString("mi")
	case pendingMatchAround:
		b.WriteString("ma")
	case pendingSurroundAdd:
		b.WriteString("ms")
	case pendingSurroundDel:
		b.WriteString("md")
	case pendingSurroundRep1:
		b.WriteString("mr")
	case pendingSurroundRep2:
		b.WriteString("mr")
		b.WriteRune(h.pendingChar)
	case pendingView:
		b.WriteString("z")
	case pendingFindNext:
		b.WriteString("f")
	case pendingFindPrev:
		b.WriteString("F")
	case pendingTillNext:
		b.WriteString("t")
	case pendingTillPrev:
		b.WriteString("T")
	case pendingReplaceChar:
		b.WriteString("r")
	}
	return b.String()
}

// SelectionRange returns the inclusive rune range [start, end] currently
// selected in Normal or Select mode, or active=false in Insert mode or on an
// empty buffer.
func (h *Helix) SelectionRange(textLen int) (start, end int, active bool) {
	if h.mode == HelixInsert || textLen == 0 {
		return 0, 0, false
	}
	lo, hi := min(h.anchor, h.head), max(h.anchor, h.head)
	if lo >= textLen {
		return 0, 0, false
	}
	if hi >= textLen {
		hi = textLen - 1
	}
	return lo, hi, true
}

// ResetForNewPrompt returns to Insert mode and clears per-prompt state
// (undo/redo, pending keys, selection) while keeping the yank register.
func (h *Helix) ResetForNewPrompt() {
	h.mode = HelixInsert
	h.anchor = 0
	h.head = 0
	h.pending = pendingNone
	h.pendingChar = 0
	h.count = 0
	h.undoStack = nil
	h.redoStack = nil
	h.insertBefore = nil
}

// SetInsert enters Insert mode at e.Pos(), recording a snapshot so escaping
// back to Normal mode makes the Insert edit undoable as one step.
func (h *Helix) SetInsert(e *Editor) {
	snap := h.snapshot(e)
	h.insertBefore = &snap
	h.mode = HelixInsert
	h.pending = pendingNone
	h.pendingChar = 0
	h.count = 0
}

// EnterNormal transitions from Insert (or Select) into Normal mode.
func (h *Helix) EnterNormal(e *Editor) {
	if h.mode == HelixInsert && h.insertBefore != nil {
		if !runesEqual(h.insertBefore.text, e.text) {
			h.pushUndoSnapshot(*h.insertBefore)
		}
		h.insertBefore = nil
	}
	h.mode = HelixNormal
	h.pending = pendingNone
	h.pendingChar = 0
	h.count = 0
	pos := e.pos
	if len(e.text) == 0 {
		pos = 0
	} else if pos >= len(e.text) {
		pos = len(e.text) - 1
	} else if pos > 0 && e.text[pos] == '\n' && e.text[pos-1] != '\n' {
		// Step back onto the last typed character on the line when leaving Insert.
		pos--
	}
	h.anchor, h.head = pos, pos
	e.pos = pos
}

// HandleKey processes one terminal key in Post-Human mode.
func (h *Helix) HandleKey(e *Editor, k Key, asking bool) HelixAction {
	// Global terminal/viewport keys work in every mode.
	switch k.Kind {
	case KeyInterrupt:
		return HelixInterrupt
	case KeyRedraw:
		return HelixRedraw
	case KeyPageUp:
		return HelixScrollPageUp
	case KeyPageDown:
		return HelixScrollPageDown
	case KeyScrollUp, KeyMouseScrollUp:
		return HelixScrollUp
	case KeyScrollDown, KeyMouseScrollDown:
		return HelixScrollDown
	}

	if asking || h.mode == HelixInsert {
		return h.handleInsert(e, k, asking)
	}
	return h.handleModal(e, k)
}

func (h *Helix) handleInsert(e *Editor, k Key, asking bool) HelixAction {
	if h.insertBefore == nil {
		snap := h.snapshot(e)
		h.insertBefore = &snap
	}
	switch k.Kind {
	case KeyEsc:
		if !asking {
			h.EnterNormal(e)
		}
		return HelixNone
	case KeyRune:
		e.Insert(string(k.R))
	case KeyAltRune:
		if k.R == 'd' {
			h.deleteWordForward(e)
		}
	case KeyPaste:
		e.Insert(k.Text)
	case KeyNewline:
		if !asking {
			e.Insert("\n")
		}
	case KeyEnter:
		if !asking && e.pos > 0 && e.text[e.pos-1] == '\\' {
			e.Backspace()
			e.Insert("\n")
			return HelixNone
		}
		return HelixSubmit
	case KeyBackspace:
		e.Backspace()
	case KeyDelete:
		e.Del()
	case KeyEOF:
		return HelixEOF
	case KeyLeft:
		e.Left()
	case KeyRight:
		e.Right()
	case KeyUp:
		e.Up()
	case KeyDown:
		e.Down()
	case KeyHome:
		e.Home()
	case KeyEnd:
		e.End()
	case KeyWordLeft:
		e.WordLeft()
	case KeyWordRight:
		e.WordRight()
	case KeyKillWord:
		e.KillWord()
	case KeyKillStart:
		e.KillStart()
	case KeyKillEnd:
		e.KillEnd()
	case KeyTab:
		return HelixComplete
	}
	return HelixNone
}

func (h *Helix) handleModal(e *Editor, k Key) HelixAction {
	// Keep selection bounds valid against current buffer.
	h.clamp(e)

	// Ctrl viewport shortcuts in Normal/Select mode (adapted from Helix).
	switch k.Ctrl {
	case 0x15: // Ctrl-u: half-page scroll up
		h.clearPending()
		return HelixScrollHalfUp
	case 0x04: // Ctrl-d: half-page scroll down
		h.clearPending()
		return HelixScrollHalfDown
	case 0x02: // Ctrl-b: full-page scroll up
		h.clearPending()
		return HelixScrollPageUp
	case 0x06: // Ctrl-f: full-page scroll down
		h.clearPending()
		return HelixScrollPageDown
	}

	// Handle active minor-mode / char-argument wait first.
	if h.pending != pendingNone {
		return h.handlePending(e, k)
	}

	// Esc clears count/pending, exits Select mode, or collapses selection.
	if k.Kind == KeyEsc {
		if h.count > 0 {
			h.count = 0
			return HelixNone
		}
		if h.mode == HelixSelect {
			h.mode = HelixNormal
			return HelixNone
		}
		h.anchor = h.head
		return HelixNone
	}

	if k.Kind == KeyEnter {
		h.clearPending()
		return HelixSubmit
	}

	if k.Kind == KeyPaste {
		h.pushUndo(e)
		e.pos = h.head
		e.Insert(k.Text)
		h.syncCursorAfterInsert(e)
		return HelixNone
	}

	// Arrow / Home / End keys in Normal/Select mode.
	switch k.Kind {
	case KeyLeft:
		h.repeat(func() { h.moveCharLeft(e) })
		return HelixNone
	case KeyRight:
		h.repeat(func() { h.moveCharRight(e) })
		return HelixNone
	case KeyUp:
		h.repeat(func() { h.moveLineUp(e) })
		return HelixNone
	case KeyDown:
		h.repeat(func() { h.moveLineDown(e) })
		return HelixNone
	case KeyHome:
		h.clearCount()
		h.moveTo(e, lineStartAt(e.text, h.head))
		return HelixNone
	case KeyEnd:
		h.clearCount()
		h.moveTo(e, lineEndCharAt(e.text, h.head))
		return HelixNone
	case KeyBackspace:
		h.repeat(func() { h.moveCharLeft(e) })
		return HelixNone
	case KeyDelete:
		h.clearCount()
		h.deleteSelection(e, false)
		return HelixNone
	}

	// Alt-modified keys in Normal/Select mode.
	if k.Kind == KeyAltRune {
		switch k.R {
		case ';': // Alt-;: flip selection anchor and head
			h.clearCount()
			h.anchor, h.head = h.head, h.anchor
			e.pos = h.head
		case '.': // Alt-.: repeat last f/F/t/T motion
			if h.lastFind.kind != pendingNone {
				h.repeat(func() { h.applyFind(e, h.lastFind.kind, h.lastFind.ch) })
			}
		case 'd': // Alt-d: delete selection without yanking
			h.clearCount()
			h.deleteSelection(e, false)
		case 'c': // Alt-c: change selection without yanking
			h.clearCount()
			h.changeSelection(e, false)
		case '`': // Alt-`: uppercase selection
			h.clearCount()
			h.transformCase(e, unicode.ToUpper)
		}
		return HelixNone
	}

	if k.Kind != KeyRune {
		return HelixNone
	}

	r := k.R

	// Count prefix accumulation (1-9, or 0 when count already started).
	if (r >= '1' && r <= '9') || (r == '0' && h.count > 0) {
		next := h.count*10 + int(r-'0')
		if next <= helixMaxCount {
			h.count = next
		}
		return HelixNone
	}

	switch r {
	// Slash on empty buffer: enter Insert mode and type `/` for geminus commands.
	case '/':
		if len(e.text) == 0 {
			h.SetInsert(e)
			e.Insert("/")
		}
		h.clearCount()

	// Mode switches.
	case 'v':
		h.clearCount()
		if h.mode == HelixSelect {
			h.mode = HelixNormal
		} else {
			h.mode = HelixSelect
		}
	case 'i':
		h.clearCount()
		lo, _, active := h.SelectionRange(len(e.text))
		if active {
			e.pos = lo
		} else {
			e.pos = 0
		}
		h.SetInsert(e)
	case 'a':
		h.clearCount()
		_, hi, active := h.SelectionRange(len(e.text))
		if active {
			e.pos = min(hi+1, len(e.text))
		} else {
			e.pos = len(e.text)
		}
		h.SetInsert(e)
	case 'I':
		h.clearCount()
		e.pos = firstNonSpaceAt(e.text, h.head)
		h.SetInsert(e)
	case 'A':
		h.clearCount()
		e.pos = lineEndAt(e.text, h.head)
		h.SetInsert(e)
	case 'o':
		h.clearCount()
		h.pushUndo(e)
		end := lineEndAt(e.text, h.head)
		e.pos = end
		e.Insert("\n")
		h.mode = HelixInsert
		h.insertBefore = nil // undo already recorded before opening line
	case 'O':
		h.clearCount()
		h.pushUndo(e)
		start := lineStartAt(e.text, h.head)
		e.pos = start
		e.Insert("\n")
		e.pos = start
		h.mode = HelixInsert
		h.insertBefore = nil

	// Basic movements.
	case 'h':
		h.repeat(func() { h.moveCharLeft(e) })
	case 'l':
		h.repeat(func() { h.moveCharRight(e) })
	case 'j':
		h.repeat(func() { h.moveLineDown(e) })
	case 'k':
		h.repeat(func() { h.moveLineUp(e) })

	// Word motions (Helix selection-first).
	case 'w':
		h.repeat(func() { h.moveWordForward(e, false, false) })
	case 'W':
		h.repeat(func() { h.moveWordForward(e, true, false) })
	case 'e':
		h.repeat(func() { h.moveWordForward(e, false, true) })
	case 'E':
		h.repeat(func() { h.moveWordForward(e, true, true) })
	case 'b':
		h.repeat(func() { h.moveWordBackward(e, false) })
	case 'B':
		h.repeat(func() { h.moveWordBackward(e, true) })

	// Goto line / buffer bounds.
	case 'G':
		if h.count > 0 {
			line := h.count
			h.count = 0
			h.moveTo(e, lineNumberStart(e.text, line))
		} else {
			h.moveTo(e, lastLineStart(e.text))
		}

	// Selection manipulation.
	case ';':
		h.clearCount()
		h.anchor = h.head
		e.pos = h.head
	case 'x':
		h.repeat(func() { h.extendLineBelow(e) })
	case 'X':
		h.clearCount()
		h.extendToLineBounds(e)
	case '%':
		h.clearCount()
		if len(e.text) > 0 {
			h.anchor = 0
			h.head = len(e.text) - 1
			e.pos = h.head
		}
	case 'J':
		h.clearCount()
		h.joinLines(e)

	// Edits on current selection.
	case 'd':
		h.clearCount()
		h.deleteSelection(e, true)
	case 'c':
		h.clearCount()
		h.changeSelection(e, true)
	case 'y':
		h.clearCount()
		h.yankSelection(e)
		h.mode = HelixNormal
	case 'p':
		h.repeat(func() { h.paste(e, true) })
	case 'P':
		h.repeat(func() { h.paste(e, false) })
	case 'R':
		h.clearCount()
		h.replaceWithYanked(e)
	case '~':
		h.clearCount()
		h.transformCase(e, swapRuneCase)
	case '`':
		h.clearCount()
		h.transformCase(e, unicode.ToLower)
	case '>':
		h.clearCount()
		h.indentLines(e, true)
	case '<':
		h.clearCount()
		h.indentLines(e, false)
	case 'u':
		h.repeat(func() { h.undo(e) })
	case 'U':
		h.repeat(func() { h.redo(e) })

	// Minor modes & char-argument prefixes.
	case 'g':
		h.pending = pendingGoto
	case 'm':
		h.pending = pendingMatch
	case 'z':
		h.pending = pendingView
	case 'f':
		h.pending = pendingFindNext
	case 'F':
		h.pending = pendingFindPrev
	case 't':
		h.pending = pendingTillNext
	case 'T':
		h.pending = pendingTillPrev
	case 'r':
		h.pending = pendingReplaceChar
	default:
		h.clearCount()
	}
	return HelixNone
}

func (h *Helix) handlePending(e *Editor, k Key) HelixAction {
	if k.Kind == KeyEsc {
		h.clearPending()
		return HelixNone
	}

	p := h.pending
	switch p {
	case pendingView:
		h.clearPending()
		switch {
		case k.Kind == KeyDown || (k.Kind == KeyRune && k.R == 'j'):
			return HelixScrollDown
		case k.Kind == KeyUp || (k.Kind == KeyRune && k.R == 'k'):
			return HelixScrollUp
		case k.Kind == KeyRune && (k.R == 'b' || k.R == 'z'):
			return HelixScrollBottom
		case k.Kind == KeyRune && k.R == 't':
			return HelixScrollTop
		}
		return HelixNone

	case pendingGoto:
		h.pending = pendingNone
		if k.Kind != KeyRune {
			h.clearCount()
			return HelixNone
		}
		switch k.R {
		case 'g':
			if h.count > 0 {
				line := h.count
				h.count = 0
				h.moveTo(e, lineNumberStart(e.text, line))
			} else {
				h.moveTo(e, 0)
			}
		case 'e':
			h.clearCount()
			h.moveTo(e, lastLineStart(e.text))
		case 'h':
			h.clearCount()
			h.moveTo(e, lineStartAt(e.text, h.head))
		case 'l':
			h.clearCount()
			h.moveTo(e, lineEndCharAt(e.text, h.head))
		case 's':
			h.clearCount()
			h.moveTo(e, firstNonSpaceAt(e.text, h.head))
		default:
			h.clearCount()
		}
		return HelixNone

	case pendingMatch:
		if k.Kind != KeyRune {
			h.clearPending()
			return HelixNone
		}
		switch k.R {
		case 'm':
			h.pending = pendingNone
			h.clearCount()
			h.matchBracket(e)
		case 'i':
			h.pending = pendingMatchInner
		case 'a':
			h.pending = pendingMatchAround
		case 's':
			h.pending = pendingSurroundAdd
		case 'd':
			h.pending = pendingSurroundDel
		case 'r':
			h.pending = pendingSurroundRep1
		default:
			h.clearPending()
		}
		return HelixNone

	case pendingMatchInner, pendingMatchAround:
		inner := p == pendingMatchInner
		h.clearPending()
		if k.Kind == KeyRune {
			h.selectTextObject(e, k.R, inner)
		}
		return HelixNone

	case pendingSurroundAdd:
		h.clearPending()
		if k.Kind == KeyRune {
			h.surroundAdd(e, k.R)
		}
		return HelixNone

	case pendingSurroundDel:
		h.clearPending()
		if k.Kind == KeyRune {
			h.surroundDelete(e, k.R)
		}
		return HelixNone

	case pendingSurroundRep1:
		if k.Kind == KeyRune {
			h.pendingChar = k.R
			h.pending = pendingSurroundRep2
		} else {
			h.clearPending()
		}
		return HelixNone

	case pendingSurroundRep2:
		from := h.pendingChar
		h.clearPending()
		if k.Kind == KeyRune {
			h.surroundReplace(e, from, k.R)
		}
		return HelixNone

	case pendingFindNext, pendingFindPrev, pendingTillNext, pendingTillPrev:
		h.pending = pendingNone
		if k.Kind != KeyRune {
			h.clearCount()
			return HelixNone
		}
		h.lastFind = lastFindMotion{kind: p, ch: k.R}
		h.repeat(func() { h.applyFind(e, p, k.R) })
		return HelixNone

	case pendingReplaceChar:
		h.clearPending()
		if k.Kind == KeyRune {
			h.replaceSelectionWithChar(e, k.R)
		}
		return HelixNone
	}

	h.clearPending()
	return HelixNone
}

// --- Helpers & Motions ---

func (h *Helix) clearCount() { h.count = 0 }

func (h *Helix) clearPending() {
	h.pending = pendingNone
	h.pendingChar = 0
	h.count = 0
}

func (h *Helix) repeat(fn func()) {
	n := h.count
	if n <= 0 {
		n = 1
	}
	h.count = 0
	for i := 0; i < n; i++ {
		fn()
	}
}

func (h *Helix) clamp(e *Editor) {
	if len(e.text) == 0 {
		h.anchor, h.head, e.pos = 0, 0, 0
		return
	}
	maxIdx := len(e.text) - 1
	h.anchor = min(max(h.anchor, 0), maxIdx)
	h.head = min(max(h.head, 0), maxIdx)
	e.pos = h.head
}

func (h *Helix) moveTo(e *Editor, target int) {
	if len(e.text) == 0 {
		h.anchor, h.head, e.pos = 0, 0, 0
		return
	}
	target = min(max(target, 0), len(e.text)-1)
	if h.mode != HelixSelect {
		h.anchor = target
	}
	h.head = target
	e.pos = target
}

func (h *Helix) moveCharLeft(e *Editor) {
	if len(e.text) == 0 {
		return
	}
	h.moveTo(e, max(h.head-1, 0))
}

func (h *Helix) moveCharRight(e *Editor) {
	if len(e.text) == 0 {
		return
	}
	h.moveTo(e, min(h.head+1, len(e.text)-1))
}

func (h *Helix) moveLineUp(e *Editor) {
	start := lineStartAt(e.text, h.head)
	if start > 0 {
		col := h.head - start
		prevEnd := start - 1
		prevStart := lineStartAt(e.text, prevEnd)
		target := min(prevStart+col, lineEndCharAt(e.text, prevStart))
		h.moveTo(e, target)
		return
	}
	// At the first line: in Normal mode, browse older prompt history.
	if h.mode == HelixNormal {
		e.pos = 0
		e.Up()
		h.resetCursorToEnd(e)
	}
}

func (h *Helix) moveLineDown(e *Editor) {
	end := lineEndAt(e.text, h.head)
	if end < len(e.text) {
		col := h.head - lineStartAt(e.text, h.head)
		nextStart := end + 1
		target := min(nextStart+col, lineEndCharAt(e.text, nextStart))
		h.moveTo(e, target)
		return
	}
	// At the last line: in Normal mode, browse newer prompt history.
	if h.mode == HelixNormal {
		e.pos = len(e.text)
		e.Down()
		h.resetCursorToEnd(e)
	}
}

func (h *Helix) resetCursorToEnd(e *Editor) {
	if len(e.text) == 0 {
		h.anchor, h.head, e.pos = 0, 0, 0
		return
	}
	last := len(e.text) - 1
	h.anchor, h.head, e.pos = last, last, last
}

type charClass int

const (
	clsSpace charClass = iota
	clsWord
	clsPunct
)

func classifyRune(r rune, longWord bool) charClass {
	if unicode.IsSpace(r) {
		return clsSpace
	}
	if longWord {
		return clsWord
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
		return clsWord
	}
	return clsPunct
}

// moveWordForward implements Helix's `w`/`W` (toEnd=false) and `e`/`E` (toEnd=true).
func (h *Helix) moveWordForward(e *Editor, longWord, toEnd bool) {
	text := e.text
	n := len(text)
	if n == 0 {
		return
	}

	start := h.head
	if h.mode == HelixNormal {
		// In Helix, repeating w/e advances past the current selection end.
		if h.anchor != h.head && h.head < n-1 {
			start = h.head + 1
		}
		h.anchor = start
	} else if start < n-1 {
		start++
	}

	i := start
	if toEnd {
		// `e` / `E`: skip whitespace, then run to the end of the word.
		if i < n && classifyRune(text[i], longWord) == clsSpace {
			for i < n && classifyRune(text[i], longWord) == clsSpace {
				i++
			}
		} else if i+1 < n && classifyRune(text[i+1], longWord) != classifyRune(text[i], longWord) {
			i++
			for i < n && classifyRune(text[i], longWord) == clsSpace {
				i++
			}
		}
		if i < n {
			cls := classifyRune(text[i], longWord)
			for i+1 < n && classifyRune(text[i+1], longWord) == cls {
				i++
			}
		}
	} else {
		// `w` / `W`: cross current word class, then cross trailing whitespace,
		// landing on the last char before the next word (so `d` removes word + space).
		if i < n {
			cls := classifyRune(text[i], longWord)
			if cls != clsSpace {
				for i < n && classifyRune(text[i], longWord) == cls {
					i++
				}
			}
			for i < n && text[i] != '\n' && unicode.IsSpace(text[i]) {
				i++
			}
			if i > start {
				i--
			}
		}
	}

	if i >= n {
		i = n - 1
	}
	h.head = i
	e.pos = h.head
}

// moveWordBackward implements Helix's `b`/`B`.
func (h *Helix) moveWordBackward(e *Editor, longWord bool) {
	text := e.text
	n := len(text)
	if n == 0 {
		return
	}

	start := h.head
	if h.mode == HelixNormal {
		if h.anchor != h.head && h.head > 0 {
			start = h.head - 1
		}
		h.anchor = start
	} else if start > 0 {
		start--
	}

	i := start
	if i > 0 && classifyRune(text[i], longWord) == clsSpace {
		for i > 0 && classifyRune(text[i], longWord) == clsSpace {
			i--
		}
	} else if i > 0 && classifyRune(text[i-1], longWord) != classifyRune(text[i], longWord) {
		i--
		for i > 0 && classifyRune(text[i], longWord) == clsSpace {
			i--
		}
	}
	cls := classifyRune(text[i], longWord)
	for i > 0 && classifyRune(text[i-1], longWord) == cls {
		i--
	}

	h.head = i
	e.pos = h.head
}

func (h *Helix) applyFind(e *Editor, kind helixPending, target rune) {
	text := e.text
	n := len(text)
	if n == 0 {
		return
	}
	found := -1
	switch kind {
	case pendingFindNext, pendingTillNext:
		start := h.head + 1
		if kind == pendingTillNext && start < n && text[start] == target {
			start++
		}
		for i := start; i < n; i++ {
			if text[i] == target {
				found = i
				break
			}
		}
		if found < 0 {
			return
		}
		if kind == pendingTillNext {
			found--
		}
	case pendingFindPrev, pendingTillPrev:
		start := h.head - 1
		if kind == pendingTillPrev && start >= 0 && text[start] == target {
			start--
		}
		for i := start; i >= 0; i-- {
			if text[i] == target {
				found = i
				break
			}
		}
		if found < 0 {
			return
		}
		if kind == pendingTillPrev {
			found++
		}
	}
	if found < 0 || found >= n {
		return
	}
	if h.mode != HelixSelect {
		h.anchor = h.head
	}
	h.head = found
	e.pos = h.head
}

func (h *Helix) extendLineBelow(e *Editor) {
	text := e.text
	n := len(text)
	if n == 0 {
		return
	}
	lo, hi := min(h.anchor, h.head), max(h.anchor, h.head)
	start := lineStartAt(text, lo)
	end := lineEndAt(text, hi)
	lineInclusiveEnd := end
	if lineInclusiveEnd >= n {
		lineInclusiveEnd = n - 1
	}
	// If the current selection already covers full line(s), extend to the next line.
	if lo == start && hi >= lineInclusiveEnd && end < n-1 {
		nextEnd := lineEndAt(text, end+1)
		if nextEnd >= n {
			nextEnd = n - 1
		}
		lineInclusiveEnd = nextEnd
	}
	h.anchor = start
	h.head = lineInclusiveEnd
	e.pos = h.head
}

func (h *Helix) extendToLineBounds(e *Editor) {
	text := e.text
	n := len(text)
	if n == 0 {
		return
	}
	lo, hi := min(h.anchor, h.head), max(h.anchor, h.head)
	start := lineStartAt(text, lo)
	end := lineEndAt(text, hi)
	if end >= n {
		end = n - 1
	}
	if h.anchor <= h.head {
		h.anchor, h.head = start, end
	} else {
		h.anchor, h.head = end, start
	}
	e.pos = h.head
}

// --- Edits ---

func (h *Helix) deleteSelection(e *Editor, yank bool) {
	lo, hi, active := h.SelectionRange(len(e.text))
	if !active {
		h.mode = HelixNormal
		return
	}
	h.pushUndo(e)
	if yank {
		h.yank = append([]rune(nil), e.text[lo:hi+1]...)
		h.yankLine = len(h.yank) > 0 && h.yank[len(h.yank)-1] == '\n'
	}
	e.Remove(lo, hi+1)
	h.mode = HelixNormal
	if len(e.text) == 0 {
		h.anchor, h.head, e.pos = 0, 0, 0
		return
	}
	pos := min(lo, len(e.text)-1)
	h.anchor, h.head, e.pos = pos, pos, pos
}

func (h *Helix) changeSelection(e *Editor, yank bool) {
	lo, hi, active := h.SelectionRange(len(e.text))
	h.pushUndo(e)
	if active {
		if yank {
			h.yank = append([]rune(nil), e.text[lo:hi+1]...)
			h.yankLine = len(h.yank) > 0 && h.yank[len(h.yank)-1] == '\n'
		}
		e.Remove(lo, hi+1)
		e.pos = lo
	}
	h.mode = HelixInsert
	h.insertBefore = nil // undo already pushed
}

func (h *Helix) yankSelection(e *Editor) {
	lo, hi, active := h.SelectionRange(len(e.text))
	if !active {
		return
	}
	h.yank = append([]rune(nil), e.text[lo:hi+1]...)
	h.yankLine = len(h.yank) > 0 && h.yank[len(h.yank)-1] == '\n'
}

func (h *Helix) paste(e *Editor, after bool) {
	if len(h.yank) == 0 {
		return
	}
	h.pushUndo(e)
	lo, hi, active := h.SelectionRange(len(e.text))
	at := 0
	if active {
		if h.yankLine {
			if after {
				end := lineEndAt(e.text, hi)
				if end < len(e.text) {
					at = end + 1
				} else {
					e.pos = len(e.text)
					e.Insert("\n")
					at = len(e.text)
				}
			} else {
				at = lineStartAt(e.text, lo)
			}
		} else if after {
			at = min(hi+1, len(e.text))
		} else {
			at = lo
		}
	}
	e.pos = at
	e.Insert(string(h.yank))
	h.mode = HelixNormal
	end := max(at+len(h.yank)-1, at)
	h.anchor = min(at, max(len(e.text)-1, 0))
	h.head = min(end, max(len(e.text)-1, 0))
	e.pos = h.head
}

func (h *Helix) replaceWithYanked(e *Editor) {
	if len(h.yank) == 0 {
		return
	}
	lo, hi, active := h.SelectionRange(len(e.text))
	h.pushUndo(e)
	if active {
		e.Remove(lo, hi+1)
		e.pos = lo
	} else {
		e.pos = 0
	}
	e.Insert(string(h.yank))
	h.mode = HelixNormal
	h.anchor = min(lo, max(len(e.text)-1, 0))
	h.head = min(lo+len(h.yank)-1, max(len(e.text)-1, 0))
	e.pos = h.head
}

func (h *Helix) replaceSelectionWithChar(e *Editor, ch rune) {
	lo, hi, active := h.SelectionRange(len(e.text))
	if !active || UnsafeRune(ch) {
		return
	}
	h.pushUndo(e)
	next := append([]rune(nil), e.text...)
	for i := lo; i <= hi; i++ {
		if next[i] != '\n' {
			next[i] = ch
		}
	}
	e.text = next
	h.mode = HelixNormal
	e.pos = h.head
}

func (h *Helix) transformCase(e *Editor, fn func(rune) rune) {
	lo, hi, active := h.SelectionRange(len(e.text))
	if !active {
		return
	}
	h.pushUndo(e)
	next := append([]rune(nil), e.text...)
	for i := lo; i <= hi; i++ {
		next[i] = fn(next[i])
	}
	e.text = next
	h.mode = HelixNormal
}

func (h *Helix) joinLines(e *Editor) {
	lo, hi, active := h.SelectionRange(len(e.text))
	if !active {
		return
	}
	// If selection is on one line, join it with the line below.
	start := lineStartAt(e.text, lo)
	end := lineEndAt(e.text, hi)
	if !containsNewline(e.text[lo:hi+1]) && end < len(e.text) {
		end = lineEndAt(e.text, end+1)
	}
	if !containsNewline(e.text[start:end]) {
		return
	}
	h.pushUndo(e)
	var joined []rune
	i := start
	for i < end {
		if e.text[i] == '\n' {
			// Skip leading horizontal spaces on the joined line.
			i++
			for i < end && (e.text[i] == ' ' || e.text[i] == '\t') {
				i++
			}
			if len(joined) > 0 && joined[len(joined)-1] != ' ' {
				joined = append(joined, ' ')
			}
			continue
		}
		joined = append(joined, e.text[i])
		i++
	}
	next := make([]rune, 0, len(e.text)-end+start+len(joined))
	next = append(next, e.text[:start]...)
	next = append(next, joined...)
	next = append(next, e.text[end:]...)
	e.text = next
	h.mode = HelixNormal
	h.anchor = start
	h.head = min(start+max(len(joined)-1, 0), max(len(e.text)-1, 0))
	e.pos = h.head
}

func (h *Helix) indentLines(e *Editor, indent bool) {
	lo, hi, active := h.SelectionRange(len(e.text))
	if !active {
		return
	}
	h.pushUndo(e)
	start := lineStartAt(e.text, lo)
	end := lineEndAt(e.text, hi)
	lines := strings.Split(string(e.text[start:end]), "\n")
	for i, line := range lines {
		if indent {
			lines[i] = "  " + line
		} else if strings.HasPrefix(line, "  ") {
			lines[i] = line[2:]
		} else if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ") {
			lines[i] = line[1:]
		}
	}
	rep := []rune(strings.Join(lines, "\n"))
	next := make([]rune, 0, len(e.text)-end+start+len(rep))
	next = append(next, e.text[:start]...)
	next = append(next, rep...)
	next = append(next, e.text[end:]...)
	e.text = next
	h.mode = HelixNormal
	h.anchor = start
	h.head = min(start+max(len(rep)-1, 0), max(len(e.text)-1, 0))
	e.pos = h.head
}

func (h *Helix) deleteWordForward(e *Editor) {
	text := e.text
	n := len(text)
	i := e.pos
	if i >= n {
		return
	}
	for i < n && unicode.IsSpace(text[i]) {
		i++
	}
	for i < n && !unicode.IsSpace(text[i]) {
		i++
	}
	e.Remove(e.pos, i)
}

// --- Match Mode (`m`) ---

func delimiterPair(ch rune) (open, close rune) {
	switch ch {
	case '(', ')':
		return '(', ')'
	case '[', ']':
		return '[', ']'
	case '{', '}':
		return '{', '}'
	case '<', '>':
		return '<', '>'
	default:
		return ch, ch
	}
}

func isBracket(r rune) bool {
	return r == '(' || r == ')' || r == '[' || r == ']' || r == '{' || r == '}' || r == '<' || r == '>'
}

func (h *Helix) matchBracket(e *Editor) {
	text := e.text
	n := len(text)
	if n == 0 {
		return
	}
	pos := h.head
	if !isBracket(text[pos]) {
		// Scan forward on current line for the next bracket.
		end := lineEndAt(text, pos)
		found := -1
		for i := pos + 1; i < end; i++ {
			if isBracket(text[i]) {
				found = i
				break
			}
		}
		if found < 0 {
			return
		}
		pos = found
	}

	ch := text[pos]
	open, close := delimiterPair(ch)
	if ch == open {
		depth := 0
		for i := pos; i < n; i++ {
			if text[i] == open {
				depth++
			} else if text[i] == close {
				depth--
				if depth == 0 {
					h.moveTo(e, i)
					return
				}
			}
		}
	} else {
		depth := 0
		for i := pos; i >= 0; i-- {
			if text[i] == close {
				depth++
			} else if text[i] == open {
				depth--
				if depth == 0 {
					h.moveTo(e, i)
					return
				}
			}
		}
	}
}

func (h *Helix) selectTextObject(e *Editor, obj rune, inner bool) {
	text := e.text
	n := len(text)
	if n == 0 {
		return
	}
	switch obj {
	case 'w', 'W':
		longWord := obj == 'W'
		cls := classifyRune(text[h.head], longWord)
		lo, hi := h.head, h.head
		for lo > 0 && classifyRune(text[lo-1], longWord) == cls {
			lo--
		}
		for hi+1 < n && classifyRune(text[hi+1], longWord) == cls {
			hi++
		}
		if !inner && cls != clsSpace {
			// Include trailing spaces if present, else leading spaces.
			if hi+1 < n && text[hi+1] != '\n' && unicode.IsSpace(text[hi+1]) {
				for hi+1 < n && text[hi+1] != '\n' && unicode.IsSpace(text[hi+1]) {
					hi++
				}
			} else {
				for lo > 0 && text[lo-1] != '\n' && unicode.IsSpace(text[lo-1]) {
					lo--
				}
			}
		}
		h.anchor, h.head = lo, hi
		e.pos = hi

	case 'p':
		lo := lineStartAt(text, h.head)
		for lo > 0 {
			prevEnd := lo - 1
			prevStart := lineStartAt(text, prevEnd)
			if strings.TrimSpace(string(text[prevStart:prevEnd])) == "" {
				break
			}
			lo = prevStart
		}
		hi := lineEndAt(text, h.head)
		for hi < n {
			nextStart := hi + 1
			nextEnd := lineEndAt(text, nextStart)
			if strings.TrimSpace(string(text[nextStart:nextEnd])) == "" {
				break
			}
			hi = nextEnd
		}
		if !inner && hi < n {
			hi++ // include trailing blank line break
		}
		if hi >= n {
			hi = n - 1
		}
		h.anchor, h.head = lo, hi
		e.pos = hi

	default:
		openIdx, closeIdx, ok := findEnclosingPair(text, h.head, obj)
		if !ok {
			return
		}
		if inner {
			if openIdx+1 > closeIdx-1 {
				return
			}
			h.anchor, h.head = openIdx+1, closeIdx-1
		} else {
			h.anchor, h.head = openIdx, closeIdx
		}
		e.pos = h.head
	}
}

func (h *Helix) surroundAdd(e *Editor, ch rune) {
	lo, hi, active := h.SelectionRange(len(e.text))
	if !active || UnsafeRune(ch) {
		return
	}
	open, close := delimiterPair(ch)
	h.pushUndo(e)
	next := make([]rune, 0, len(e.text)+2)
	next = append(next, e.text[:lo]...)
	next = append(next, open)
	next = append(next, e.text[lo:hi+1]...)
	next = append(next, close)
	next = append(next, e.text[hi+1:]...)
	e.text = next
	h.mode = HelixNormal
	h.anchor, h.head = lo, hi+2
	e.pos = h.head
}

func (h *Helix) surroundDelete(e *Editor, ch rune) {
	openIdx, closeIdx, ok := findEnclosingPair(e.text, h.head, ch)
	if !ok {
		return
	}
	h.pushUndo(e)
	next := make([]rune, 0, len(e.text)-2)
	next = append(next, e.text[:openIdx]...)
	next = append(next, e.text[openIdx+1:closeIdx]...)
	next = append(next, e.text[closeIdx+1:]...)
	e.text = next
	h.mode = HelixNormal
	if len(e.text) == 0 {
		h.anchor, h.head, e.pos = 0, 0, 0
		return
	}
	pos := min(max(h.head-1, openIdx), len(e.text)-1)
	h.anchor, h.head, e.pos = pos, pos, pos
}

func (h *Helix) surroundReplace(e *Editor, from, to rune) {
	if UnsafeRune(to) {
		return
	}
	openIdx, closeIdx, ok := findEnclosingPair(e.text, h.head, from)
	if !ok {
		return
	}
	open, close := delimiterPair(to)
	h.pushUndo(e)
	next := append([]rune(nil), e.text...)
	next[openIdx] = open
	next[closeIdx] = close
	e.text = next
	h.mode = HelixNormal
}

func findEnclosingPair(text []rune, pos int, ch rune) (openIdx, closeIdx int, ok bool) {
	n := len(text)
	if n < 2 || pos < 0 || pos >= n {
		return 0, 0, false
	}
	open, close := delimiterPair(ch)
	if open == close {
		// Symmetric delimiter (quotes, backticks): pair up occurrences in buffer.
		left := -1
		for i := pos; i >= 0; i-- {
			if text[i] == open {
				left = i
				break
			}
		}
		if left < 0 {
			return 0, 0, false
		}
		right := -1
		startRight := pos + 1
		if left == pos {
			startRight = pos + 1
		}
		for i := startRight; i < n; i++ {
			if text[i] == close {
				right = i
				break
			}
		}
		if right < 0 {
			return 0, 0, false
		}
		return left, right, true
	}

	// Asymmetric balanced delimiters: (), [], {}, <>.
	depth := 0
	left := -1
	for i := pos; i >= 0; i-- {
		if text[i] == close && i != pos {
			depth++
		} else if text[i] == open {
			if depth == 0 {
				left = i
				break
			}
			depth--
		}
	}
	if left < 0 {
		return 0, 0, false
	}
	depth = 0
	for i := left; i < n; i++ {
		if text[i] == open {
			depth++
		} else if text[i] == close {
			depth--
			if depth == 0 {
				if i >= pos {
					return left, i, true
				}
				return 0, 0, false
			}
		}
	}
	return 0, 0, false
}

// --- Undo / Redo ---

func (h *Helix) snapshot(e *Editor) helixSnapshot {
	return helixSnapshot{
		text:   append([]rune(nil), e.text...),
		anchor: h.anchor,
		head:   h.head,
	}
}

func (h *Helix) pushUndo(e *Editor) {
	h.pushUndoSnapshot(h.snapshot(e))
}

func (h *Helix) pushUndoSnapshot(s helixSnapshot) {
	h.undoStack = append(h.undoStack, s)
	if len(h.undoStack) > helixMaxUndo {
		h.undoStack = append([]helixSnapshot(nil), h.undoStack[len(h.undoStack)-helixMaxUndo:]...)
	}
	h.redoStack = nil
}

func (h *Helix) undo(e *Editor) {
	if len(h.undoStack) == 0 {
		return
	}
	h.redoStack = append(h.redoStack, h.snapshot(e))
	prev := h.undoStack[len(h.undoStack)-1]
	h.undoStack = h.undoStack[:len(h.undoStack)-1]
	e.text = append([]rune(nil), prev.text...)
	h.anchor, h.head = prev.anchor, prev.head
	h.clamp(e)
}

func (h *Helix) redo(e *Editor) {
	if len(h.redoStack) == 0 {
		return
	}
	h.undoStack = append(h.undoStack, h.snapshot(e))
	next := h.redoStack[len(h.redoStack)-1]
	h.redoStack = h.redoStack[:len(h.redoStack)-1]
	e.text = append([]rune(nil), next.text...)
	h.anchor, h.head = next.anchor, next.head
	h.clamp(e)
}

func (h *Helix) syncCursorAfterInsert(e *Editor) {
	if len(e.text) == 0 {
		h.anchor, h.head, e.pos = 0, 0, 0
		return
	}
	pos := min(max(e.pos-1, 0), len(e.text)-1)
	h.anchor, h.head, e.pos = pos, pos, pos
}

// --- Line index helpers ---

func lineStartAt(text []rune, pos int) int {
	if pos > len(text) {
		pos = len(text)
	}
	i := pos
	for i > 0 && text[i-1] != '\n' {
		i--
	}
	return i
}

func lineEndAt(text []rune, pos int) int {
	if pos > len(text) {
		pos = len(text)
	}
	i := pos
	for i < len(text) && text[i] != '\n' {
		i++
	}
	return i
}

// lineEndCharAt returns the index of the last character on the line (before '\n'),
// or lineStart if the line is empty.
func lineEndCharAt(text []rune, pos int) int {
	start := lineStartAt(text, pos)
	end := lineEndAt(text, pos)
	if end > start {
		return end - 1
	}
	return start
}

func firstNonSpaceAt(text []rune, pos int) int {
	start := lineStartAt(text, pos)
	end := lineEndAt(text, pos)
	for i := start; i < end; i++ {
		if !unicode.IsSpace(text[i]) {
			return i
		}
	}
	return start
}

func lineNumberStart(text []rune, lineNum int) int {
	if lineNum <= 1 || len(text) == 0 {
		return 0
	}
	cur := 1
	for i, r := range text {
		if r == '\n' {
			cur++
			if cur == lineNum {
				if i+1 < len(text) {
					return i + 1
				}
				return i
			}
		}
	}
	return lastLineStart(text)
}

func lastLineStart(text []rune) int {
	if len(text) == 0 {
		return 0
	}
	return lineStartAt(text, len(text)-1)
}

func containsNewline(rs []rune) bool {
	for _, r := range rs {
		if r == '\n' {
			return true
		}
	}
	return false
}

func swapRuneCase(r rune) rune {
	if unicode.IsUpper(r) {
		return unicode.ToLower(r)
	}
	if unicode.IsLower(r) {
		return unicode.ToUpper(r)
	}
	return r
}

func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
