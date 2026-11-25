package tui

// Turning what the terminal sends in raw mode into keys.

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DecodeRune decodes the first UTF-8 rune in s.
func DecodeRune(s string) (rune, int) { return utf8.DecodeRuneInString(s) }

type KeyKind int

const (
	KeyNone  KeyKind = iota
	KeyRune          // a character; see Key.R
	KeyPaste         // pasted text; see Key.Text
	KeyEnter
	KeyNewline // a line break inside the input (Alt+Enter, Shift+Enter, Ctrl-J)
	KeyBackspace
	KeyDelete
	KeyLeft
	KeyRight
	KeyUp
	KeyDown
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyScrollUp
	KeyScrollDown
	KeyMouseScrollUp
	KeyMouseScrollDown
	KeyWordLeft
	KeyWordRight
	KeyKillWord  // delete the word before the cursor
	KeyKillStart // delete to the start of the line
	KeyKillEnd   // delete to the end of the line
	KeyTab
	KeyEsc
	KeyInterrupt // Ctrl-C
	KeyEOF       // Ctrl-D
	KeyRedraw    // Ctrl-L
	KeyPasteStart
)

type Key struct {
	Kind KeyKind
	R    rune
	Text string
}

const pasteEnd = "\x1b[201~"

// KeyParser keeps what one read left unfinished: half a character, half an
// escape sequence, or a paste that has not ended.
type KeyParser struct {
	rest    []byte
	pasting bool
	paste   []byte
}

var controlKeys = map[byte]KeyKind{
	0x01: KeyHome, 0x02: KeyLeft, 0x03: KeyInterrupt, 0x04: KeyEOF, 0x05: KeyEnd, 0x06: KeyRight,
	0x08: KeyBackspace, 0x09: KeyTab, 0x0a: KeyNewline, 0x0b: KeyKillEnd, 0x0c: KeyRedraw, 0x0d: KeyEnter,
	0x0e: KeyDown, 0x10: KeyUp, 0x15: KeyKillStart, 0x17: KeyKillWord, 0x7f: KeyBackspace,
}

// Feed returns the keys in b, a chunk as read from the terminal.
func (p *KeyParser) Feed(b []byte) []Key {
	buf := append(p.rest, b...)
	p.rest = nil
	var out []Key
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
			out = append(out, Key{Kind: KeyPaste, Text: string(p.paste)})
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
					out = append(out, Key{Kind: KeyEsc})
					return out
				}
				p.rest = append([]byte(nil), buf[i:]...)
				return out
			}
			switch k.Kind {
			case KeyPasteStart:
				p.pasting = true
			case KeyNone:
			default:
				out = append(out, k)
			}
			i += n
		case c < 0x20 || c == 0x7f:
			if kind, ok := controlKeys[c]; ok {
				out = append(out, Key{Kind: kind})
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
			out = append(out, Key{Kind: KeyRune, R: r})
			i += size
		}
	}
	return out
}

var csiKeys = map[string]KeyKind{
	"A": KeyUp, "B": KeyDown, "C": KeyRight, "D": KeyLeft, "H": KeyHome, "F": KeyEnd,
	"1~": KeyHome, "7~": KeyHome, "4~": KeyEnd, "8~": KeyEnd, "3~": KeyDelete, "200~": KeyPasteStart,
	"5~": KeyPageUp, "5;2~": KeyPageUp, "5;5~": KeyPageUp,
	"6~": KeyPageDown, "6;2~": KeyPageDown, "6;5~": KeyPageDown,
	"1;2A": KeyScrollUp, "1;2B": KeyScrollDown,
	// With Ctrl or Alt held: move by words.
	"1;5C": KeyWordRight, "1;3C": KeyWordRight, "1;5D": KeyWordLeft, "1;3D": KeyWordLeft,
	"1;2H": KeyHome, "1;2F": KeyEnd,
	// Shift+Enter and Alt+Enter as terminals that report modifiers send them.
	"13;2u": KeyNewline, "13;3u": KeyNewline, "13;5u": KeyNewline, "27;2;13~": KeyNewline, "27;3;13~": KeyNewline,
	"27;5;13~": KeyNewline,
}

var ss3Keys = map[byte]KeyKind{'A': KeyUp, 'B': KeyDown, 'C': KeyRight, 'D': KeyLeft, 'H': KeyHome, 'F': KeyEnd, 'M': KeyEnter}

// parseEscape reads the sequence at the start of b, which begins with ESC.
// complete is false when more bytes are needed to tell what it is.
func parseEscape(b []byte) (k Key, n int, complete bool) {
	if len(b) < 2 {
		return Key{}, 0, false
	}
	switch b[1] {
	case '[':
		// Legacy X10 mouse report: ESC [ M Cb Cx Cy (3 raw bytes after 'M').
		if len(b) >= 3 && b[2] == 'M' {
			if len(b) < 6 {
				return Key{}, 0, false
			}
			return parseX10Mouse(b[3]), 6, true
		}
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				seq := string(b[2 : i+1])
				if strings.HasPrefix(seq, "<") && (b[i] == 'M' || b[i] == 'm') {
					return parseSGRMouse(seq), i + 1, true
				}
				return Key{Kind: csiKeys[seq]}, i + 1, true
			}
		}
		if len(b) > 64 {
			return Key{}, len(b), true // not a sequence a terminal sends
		}
		return Key{}, 0, false
	case 'O':
		if len(b) < 3 {
			return Key{}, 0, false
		}
		return Key{Kind: ss3Keys[b[2]]}, 3, true
	case '\r', '\n':
		return Key{Kind: KeyNewline}, 2, true
	case 0x7f, 0x08:
		return Key{Kind: KeyKillWord}, 2, true
	case 'b':
		return Key{Kind: KeyWordLeft}, 2, true
	case 'f':
		return Key{Kind: KeyWordRight}, 2, true
	case 0x1b:
		return Key{Kind: KeyEsc}, 1, true
	}
	return Key{}, 2, true // Alt with a key geminus has no use for
}

// parseSGRMouse decodes an SGR mouse sequence body like "<64;12;8M" or "<0;1;1m".
func parseSGRMouse(seq string) Key {
	if len(seq) < 2 || seq[0] != '<' {
		return Key{}
	}
	pressed := seq[len(seq)-1] == 'M'
	if !pressed {
		return Key{}
	}
	parts := strings.Split(seq[1:len(seq)-1], ";")
	if len(parts) != 3 {
		return Key{}
	}
	code, err := strconv.Atoi(parts[0])
	if err != nil {
		return Key{}
	}
	return mouseButtonKey(code)
}

// parseX10Mouse decodes the button byte from a legacy X10 mouse sequence (ESC [ M Cb Cx Cy).
func parseX10Mouse(cb byte) Key {
	if cb < 32 {
		return Key{}
	}
	return mouseButtonKey(int(cb - 32))
}

// mouseButtonKey maps an xterm mouse button code to a scroll key, ignoring clicks and motion.
func mouseButtonKey(code int) Key {
	// Strip Shift (4), Alt/Meta (8), and Ctrl (16) modifier bits.
	switch code &^ (4 | 8 | 16) {
	case 64:
		return Key{Kind: KeyMouseScrollUp}
	case 65:
		return Key{Kind: KeyMouseScrollDown}
	default:
		return Key{}
	}
}
