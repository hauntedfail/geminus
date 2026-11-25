package chat

// Turning what the terminal sends in raw mode into keys.

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

func decodeRune(s string) (rune, int) { return utf8.DecodeRuneInString(s) }

type keyKind int

const (
	keyNone  keyKind = iota
	keyRune          // a character; see key.r
	keyPaste         // pasted text; see key.text
	keyEnter
	keyNewline // a line break inside the input (Alt+Enter, Shift+Enter, Ctrl-J)
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyPageUp
	keyPageDown
	keyScrollUp
	keyScrollDown
	keyMouseScrollUp
	keyMouseScrollDown
	keyWordLeft
	keyWordRight
	keyKillWord  // delete the word before the cursor
	keyKillStart // delete to the start of the line
	keyKillEnd   // delete to the end of the line
	keyTab
	keyEsc
	keyInterrupt // Ctrl-C
	keyEOF       // Ctrl-D
	keyRedraw    // Ctrl-L
	keyPasteStart
)

type key struct {
	kind keyKind
	r    rune
	text string
}

const pasteEnd = "\x1b[201~"

// keyParser keeps what one read left unfinished: half a character, half an
// escape sequence, or a paste that has not ended.
type keyParser struct {
	rest    []byte
	pasting bool
	paste   []byte
}

var controlKeys = map[byte]keyKind{
	0x01: keyHome, 0x02: keyLeft, 0x03: keyInterrupt, 0x04: keyEOF, 0x05: keyEnd, 0x06: keyRight,
	0x08: keyBackspace, 0x09: keyTab, 0x0a: keyNewline, 0x0b: keyKillEnd, 0x0c: keyRedraw, 0x0d: keyEnter,
	0x0e: keyDown, 0x10: keyUp, 0x15: keyKillStart, 0x17: keyKillWord, 0x7f: keyBackspace,
}

// feed returns the keys in b, a chunk as read from the terminal.
func (p *keyParser) feed(b []byte) []key {
	buf := append(p.rest, b...)
	p.rest = nil
	var out []key
	for i := 0; i < len(buf); {
		if p.pasting {
			end := bytes.Index(buf[i:], []byte(pasteEnd))
			if end < 0 {
				// The end mark may be split across reads.
				keep := 0
				for n := min(len(pasteEnd)-1, len(buf)-i); n > 0; n-- {
					if strings.HasPrefix(pasteEnd, string(buf[len(buf)-n:])) {
						keep = n
						break
					}
				}
				p.paste = append(p.paste, buf[i:len(buf)-keep]...)
				p.rest = append([]byte(nil), buf[len(buf)-keep:]...)
				return out
			}
			p.paste = append(p.paste, buf[i:i+end]...)
			out = append(out, key{kind: keyPaste, text: string(p.paste)})
			p.paste, p.pasting = nil, false
			i += end + len(pasteEnd)
			continue
		}
		c := buf[i]
		switch {
		case c == 0x1b:
			k, n, complete := parseEscape(buf[i:])
			if !complete {
				if len(buf)-i == 1 {
					// Nothing came with it: the Esc key itself.
					out = append(out, key{kind: keyEsc})
					return out
				}
				p.rest = append([]byte(nil), buf[i:]...)
				return out
			}
			switch k.kind {
			case keyPasteStart:
				p.pasting = true
			case keyNone:
			default:
				out = append(out, k)
			}
			i += n
		case c < 0x20 || c == 0x7f:
			if kind, ok := controlKeys[c]; ok {
				out = append(out, key{kind: kind})
			}
			i++
		default:
			r, size := utf8.DecodeRune(buf[i:])
			if r == utf8.RuneError && size == 1 {
				if !utf8.FullRune(buf[i:]) {
					p.rest = append([]byte(nil), buf[i:]...)
					return out
				}
				i++
				continue
			}
			out = append(out, key{kind: keyRune, r: r})
			i += size
		}
	}
	return out
}

var csiKeys = map[string]keyKind{
	"A": keyUp, "B": keyDown, "C": keyRight, "D": keyLeft, "H": keyHome, "F": keyEnd,
	"1~": keyHome, "7~": keyHome, "4~": keyEnd, "8~": keyEnd, "3~": keyDelete, "200~": keyPasteStart,
	"5~": keyPageUp, "5;2~": keyPageUp, "5;5~": keyPageUp,
	"6~": keyPageDown, "6;2~": keyPageDown, "6;5~": keyPageDown,
	"1;2A": keyScrollUp, "1;2B": keyScrollDown,
	// With Ctrl or Alt held: move by words.
	"1;5C": keyWordRight, "1;3C": keyWordRight, "1;5D": keyWordLeft, "1;3D": keyWordLeft,
	"1;2H": keyHome, "1;2F": keyEnd,
	// Shift+Enter and Alt+Enter as terminals that report modifiers send them.
	"13;2u": keyNewline, "13;3u": keyNewline, "13;5u": keyNewline, "27;2;13~": keyNewline, "27;3;13~": keyNewline,
	"27;5;13~": keyNewline,
}

var ss3Keys = map[byte]keyKind{'A': keyUp, 'B': keyDown, 'C': keyRight, 'D': keyLeft, 'H': keyHome, 'F': keyEnd, 'M': keyEnter}

// parseEscape reads the sequence at the start of b, which begins with ESC.
// complete is false when more bytes are needed to tell what it is.
func parseEscape(b []byte) (k key, n int, complete bool) {
	if len(b) < 2 {
		return key{}, 0, false
	}
	switch b[1] {
	case '[':
		// Legacy X10 mouse report: ESC [ M Cb Cx Cy (3 raw bytes after 'M').
		if len(b) >= 3 && b[2] == 'M' {
			if len(b) < 6 {
				return key{}, 0, false
			}
			return parseX10Mouse(b[3]), 6, true
		}
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				seq := string(b[2 : i+1])
				if strings.HasPrefix(seq, "<") && (b[i] == 'M' || b[i] == 'm') {
					return parseSGRMouse(seq), i + 1, true
				}
				return key{kind: csiKeys[seq]}, i + 1, true
			}
		}
		if len(b) > 64 {
			return key{}, len(b), true // not a sequence a terminal sends
		}
		return key{}, 0, false
	case 'O':
		if len(b) < 3 {
			return key{}, 0, false
		}
		return key{kind: ss3Keys[b[2]]}, 3, true
	case '\r', '\n':
		return key{kind: keyNewline}, 2, true
	case 0x7f, 0x08:
		return key{kind: keyKillWord}, 2, true
	case 'b':
		return key{kind: keyWordLeft}, 2, true
	case 'f':
		return key{kind: keyWordRight}, 2, true
	case 0x1b:
		return key{kind: keyEsc}, 1, true
	}
	return key{}, 2, true // Alt with a key geminus has no use for
}

// parseSGRMouse decodes an SGR mouse sequence body like "<64;12;8M" or "<0;1;1m".
func parseSGRMouse(seq string) key {
	if len(seq) < 2 || seq[0] != '<' {
		return key{}
	}
	pressed := seq[len(seq)-1] == 'M'
	if !pressed {
		return key{}
	}
	parts := strings.Split(seq[1:len(seq)-1], ";")
	if len(parts) != 3 {
		return key{}
	}
	code, err := strconv.Atoi(parts[0])
	if err != nil {
		return key{}
	}
	return mouseButtonKey(code)
}

// parseX10Mouse decodes the button byte from a legacy X10 mouse sequence (ESC [ M Cb Cx Cy).
func parseX10Mouse(cb byte) key {
	if cb < 32 {
		return key{}
	}
	return mouseButtonKey(int(cb - 32))
}

// mouseButtonKey maps an xterm mouse button code to a scroll key, ignoring clicks and motion.
func mouseButtonKey(code int) key {
	// Strip Shift (4), Alt/Meta (8), and Ctrl (16) modifier bits.
	switch code &^ (4 | 8 | 16) {
	case 64:
		return key{kind: keyMouseScrollUp}
	case 65:
		return key{kind: keyMouseScrollDown}
	default:
		return key{}
	}
}
