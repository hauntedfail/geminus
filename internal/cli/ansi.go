package cli

import (
	"bytes"
	"io"
	"unicode/utf8"
)

// maxPendingLine limits how much of a line to buffer before flushing. Full-screen TUIs may
// never emit a newline and would otherwise exhaust memory.
const maxPendingLine = 64 << 10

type ansiState int

const (
	ansiGround ansiState = iota
	ansiEscape           // Immediately after ESC
	ansiEscapeIntermediate
	ansiCSI          // ESC [ through the final byte
	ansiOSC          // ESC ] through BEL or ST
	ansiOSCEscape    // ESC within OSC, possibly the first byte of ST
	ansiString       // DCS, SOS, PM, or APC through ST
	ansiStringEscape // ESC within a control string
)

// ansiStripper removes control sequences from terminal output and writes the readable text.
//
// This is not a terminal emulator. It discards colors and cursor movements and applies
// backspace and carriage return only within the current line. Full-screen TUI output may
// not line up. Sequences can span writes; call Close after the last write.
//
// Treat output as UTF-8. Drop C1 controls (U+0080 through U+009F) and stray continuation
// bytes because terminals may interpret them as 8-bit control sequences. Use --raw to see
// other encodings such as Shift_JIS unchanged.
type ansiStripper struct {
	w     io.Writer
	state ansiState
	// line buffers the unwritten part of the current line so backspace and carriage return can
	// modify it.
	line []byte
	// pendingCR marks a carriage return. A following LF ends the line; new text overwrites it.
	pendingCR bool
	// heldC2 marks a pending 0xC2 byte. The next byte determines whether it encodes a C1
	// control.
	heldC2 bool
	// contLeft counts the continuation bytes still expected for the current UTF-8 character.
	contLeft int
	err      error
}

func newANSIStripper(w io.Writer) *ansiStripper {
	return &ansiStripper{w: w}
}

// Write processes p. After a write error it returns that error without writing anything
// further.
func (s *ansiStripper) Write(p []byte) (int, error) {
	for _, b := range p {
		if s.err != nil {
			return 0, s.err
		}
		s.step(b)
	}
	if s.err != nil {
		return 0, s.err
	}
	return len(p), nil
}

// Close flushes the buffered line.
func (s *ansiStripper) Close() error {
	s.flushLine()
	return s.err
}

func (s *ansiStripper) step(b byte) {
	switch s.state {
	case ansiGround:
		s.ground(b)
	case ansiEscape:
		s.escape(b)
	case ansiEscapeIntermediate:
		if b < 0x20 || b > 0x2f {
			s.state = ansiGround
		}
	case ansiCSI:
		s.csi(b)
	case ansiOSC:
		switch b {
		case 0x07:
			s.state = ansiGround
		case 0x1b:
			s.state = ansiOSCEscape
		}
	case ansiString:
		if b == 0x1b {
			s.state = ansiStringEscape
		}
	case ansiOSCEscape, ansiStringEscape:
		// ESC followed by a backslash is ST (string terminator). Otherwise a new escape sequence
		// begins.
		if b == '\\' {
			s.state = ansiGround
			return
		}
		s.state = ansiEscape
		s.escape(b)
	}
}

func (s *ansiStripper) ground(b byte) {
	if b >= 0x80 {
		s.highByte(b)
		return
	}
	// An ASCII byte ends any incomplete (invalid) UTF-8 character.
	s.heldC2 = false
	s.contLeft = 0
	switch {
	case b == 0x1b:
		s.state = ansiEscape
	case b == '\n':
		s.pendingCR = false
		s.line = append(s.line, '\n')
		s.flushLine()
	case b == '\r':
		s.pendingCR = true
	case b == '\b':
		s.eraseLastRune()
	case b == '\t' || (b >= 0x20 && b != 0x7f):
		s.put(b)
	}
	// Discard other controls, including BEL, NUL, and DEL.
}

// highByte handles bytes at or above 0x80, either UTF-8 lead or continuation bytes.
func (s *ansiStripper) highByte(b byte) {
	continuation := b&0xc0 == 0x80
	switch {
	case s.heldC2:
		s.heldC2 = false
		if !continuation {
			// No continuation byte followed 0xC2. Drop it and process the current byte again.
			s.highByte(b)
			return
		}
		if b <= 0x9f {
			return // U+0080 through U+009F: C1 controls
		}
		s.put(0xc2)
		s.put(b)
	case continuation:
		if s.contLeft == 0 {
			return // A stray continuation byte might be an 8-bit C1 control.
		}
		s.contLeft--
		s.put(b)
	case b == 0xc2:
		s.heldC2 = true
		s.contLeft = 0
	default:
		s.contLeft = continuationBytes(b)
		s.put(b)
	}
}

// continuationBytes returns the number of continuation bytes expected after lead.
func continuationBytes(lead byte) int {
	switch {
	case lead >= 0xc3 && lead <= 0xdf:
		return 1
	case lead >= 0xe0 && lead <= 0xef:
		return 2
	case lead >= 0xf0 && lead <= 0xf4:
		return 3
	default:
		return 0
	}
}

// put appends one display byte to the current line.
func (s *ansiStripper) put(b byte) {
	if s.pendingCR {
		// Output after a carriage return overwrites the line, as in a progress indicator.
		s.pendingCR = false
		s.line = s.line[:0]
	}
	s.line = append(s.line, b)
	if len(s.line) >= maxPendingLine {
		s.flushLine()
	}
}

func (s *ansiStripper) escape(b byte) {
	switch {
	case b == '[':
		s.state = ansiCSI
	case b == ']':
		s.state = ansiOSC
	case b == 'P' || b == 'X' || b == '^' || b == '_':
		s.state = ansiString
	case b >= 0x20 && b <= 0x2f:
		s.state = ansiEscapeIntermediate
	case b == 0x1b:
		// Consecutive ESC bytes: restart at the second ESC.
	case b < 0x20:
		// Apply controls such as newline within a sequence as a terminal would.
		s.state = ansiGround
		s.ground(b)
	default:
		// Two-byte sequences such as ESC 7 or ESC =.
		s.state = ansiGround
	}
}

func (s *ansiStripper) csi(b byte) {
	switch {
	case b >= 0x20 && b <= 0x3f:
		// Parameter and intermediate bytes.
	case b == 0x1b:
		s.state = ansiEscape
	case b < 0x20:
		// Malformed sequence: reprocess the byte as a control.
		s.state = ansiGround
		s.ground(b)
	default:
		s.state = ansiGround
	}
}

// eraseLastRune removes the last character from the current line. It cannot erase text
// already flushed.
func (s *ansiStripper) eraseLastRune() {
	if len(s.line) == 0 {
		return
	}
	_, size := utf8.DecodeLastRune(s.line)
	s.line = s.line[:len(s.line)-size]
}

func (s *ansiStripper) flushLine() {
	if len(s.line) == 0 || s.err != nil {
		return
	}
	_, s.err = s.w.Write(s.line)
	s.line = s.line[:0]
}

// tailWriter keeps the last n lines and writes them to w on Close. A final unterminated
// line counts as one line.
type tailWriter struct {
	w       io.Writer
	n       int
	lines   [][]byte // Complete lines, oldest first
	partial []byte   // Line not yet terminated by a newline
}

func newTailWriter(w io.Writer, n int) *tailWriter {
	return &tailWriter{w: w, n: n}
}

func (t *tailWriter) Write(p []byte) (int, error) {
	rest := p
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			t.partial = append(t.partial, rest...)
			return len(p), nil
		}
		line := append(t.partial, rest[:i+1]...)
		t.partial = nil
		t.push(line)
		rest = rest[i+1:]
	}
}

func (t *tailWriter) push(line []byte) {
	t.lines = append(t.lines, line)
	if len(t.lines) > t.n {
		// Release the oldest entry to keep at most n lines.
		t.lines[0] = nil
		t.lines = t.lines[1:]
	}
}

// Close writes the retained lines to w.
func (t *tailWriter) Close() error {
	if len(t.partial) > 0 {
		t.push(t.partial)
		t.partial = nil
	}
	for _, line := range t.lines {
		if _, err := t.w.Write(line); err != nil {
			return err
		}
	}
	t.lines = nil
	return nil
}
