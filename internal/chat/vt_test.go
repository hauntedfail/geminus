package chat

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

// vt is a small terminal: enough of one to check what the live screen leaves
// on the glass. It understands what geminus writes (text, CR, LF, cursor up
// and right, erase below) and ignores styling and modes.
type vt struct {
	mu         sync.Mutex
	cols, rows int
	lines      [][]rune // everything, scrollback included
	top        int      // index in lines of the screen's first row
	row, col   int      // cursor, relative to the screen
	raw        strings.Builder
}

func newVT(cols, rows int) *vt { return &vt{cols: cols, rows: rows, lines: [][]rune{nil}} }

func (t *vt) line() *[]rune {
	for len(t.lines) <= t.top+t.row {
		t.lines = append(t.lines, nil)
	}
	return &t.lines[t.top+t.row]
}

func (t *vt) feedLine() {
	if t.row == t.rows-1 {
		t.top++
	} else {
		t.row++
	}
	t.line()
}

func (t *vt) put(r rune) {
	w := runeWidth(r)
	if w == 0 {
		return
	}
	if t.col+w > t.cols {
		t.col = 0
		t.feedLine()
	}
	l := t.line()
	for len(*l) < t.col+w {
		*l = append(*l, ' ')
	}
	(*l)[t.col] = r
	for i := 1; i < w; i++ {
		(*l)[t.col+i] = 0 // the second half of a wide character
	}
	t.col += w
}

func (t *vt) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.raw.Write(p)
	s := string(p)
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\r':
			t.col = 0
			i++
		case c == '\n':
			t.feedLine()
			i++
		case c == 0x1b && i+1 < len(s) && s[i+1] == '[':
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			arg, _ := strconv.Atoi(strings.TrimLeft(s[i+2:j], "?"))
			switch s[j] {
			case 'A':
				t.row = max(t.row-max(arg, 1), 0)
			case 'C':
				t.col = min(t.col+max(arg, 1), t.cols-1)
			case 'J':
				l := t.line()
				if len(*l) > t.col {
					*l = (*l)[:t.col]
				}
				t.lines = t.lines[:t.top+t.row+1]
			}
			i = j + 1
		default:
			r, size := decodeRune(s[i:])
			t.put(r)
			i += size
		}
	}
	return len(p), nil
}

// text is everything on the terminal, scrollback first, without trailing
// blanks.
func (t *vt) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, l := range t.lines {
		var b strings.Builder
		for _, r := range l {
			if r != 0 {
				b.WriteRune(r)
			}
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// cursor is the text of the cursor's line up to the cursor.
func (t *vt) cursor() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := *t.line()
	var b strings.Builder
	for i := 0; i < t.col && i < len(l); i++ {
		if l[i] != 0 {
			b.WriteRune(l[i])
		}
	}
	return b.String()
}

func TestVTBehavesLikeATerminal(t *testing.T) {
	term := newVT(10, 3)
	term.Write([]byte("one\r\ntwo\r\nthree\r\nfour"))
	if got := term.text(); got != "one\ntwo\nthree\nfour" {
		t.Errorf("scrolled text = %q", got)
	}
	term.Write([]byte("\x1b[1A\r\x1b[Jx\x1b[2Cy"))
	if got := term.text(); got != "one\ntwo\nx  y" {
		t.Errorf("after erase = %q", got)
	}
	wide := newVT(5, 3)
	wide.Write([]byte("日本語"))
	if got := wide.text(); got != "日本\n語" {
		t.Errorf("wide wrap = %q", got)
	}
}
