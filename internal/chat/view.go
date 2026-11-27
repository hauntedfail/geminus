package chat

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

// The only escape sequences geminus writes itself; see Printer.style.
var ownStyles = []string{"\x1b[0m", "\x1b[1m", "\x1b[2m", "\x1b[31m", "\x1b[33m", "\x1b[1;36m", "\x1b[38;5;173m", "\x1b[1;38;5;173m"}

// Printer writes to the screen and remembers whether the cursor is at the
// start of a line, so status lines never land in the middle of a reply.
type Printer struct {
	mu      sync.Mutex
	w       io.Writer
	color   bool
	midLine bool
}

func newPrinter(w io.Writer, color bool) *Printer { return &Printer{w: w, color: color} }

// NewPrinter returns a printer without color, for tests and plain output.
func NewPrinter(w io.Writer) *Printer { return newPrinter(w, false) }

// clean is the last line of defense: everything printed goes through it, so
// a call site that forgets to sanitize an outside value still cannot emit an
// escape sequence. Only geminus's own styling survives, and only in color
// mode.
func (p *Printer) clean(s string) string {
	if !strings.ContainsFunc(s, unsafeRune) && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
scan:
	for i := 0; i < len(s); {
		if p.color && s[i] == 0x1b {
			for _, style := range ownStyles {
				if strings.HasPrefix(s[i:], style) {
					b.WriteString(style)
					i += len(style)
					continue scan
				}
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if !unsafeRune(r) && (r != utf8.RuneError || size > 1) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (p *Printer) write(s string) {
	s = p.clean(s)
	if s == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	io.WriteString(p.w, s)
	p.midLine = !strings.HasSuffix(s, "\n")
}

// line prints s on a line of its own.
func (p *Printer) line(s string) {
	s = p.clean(s)
	p.mu.Lock()
	lead := ""
	if p.midLine {
		lead = "\n"
	}
	io.WriteString(p.w, lead+s+"\n")
	p.midLine = false
	p.mu.Unlock()
}

// endLine ends the line being written, if one is.
func (p *Printer) endLine() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.midLine {
		io.WriteString(p.w, "\n")
		p.midLine = false
	}
}

func (p *Printer) atLineStart() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.midLine
}

// typed notes that the person pressed Enter, which moved the cursor to a new
// line without the printer writing anything.
func (p *Printer) typed() {
	p.mu.Lock()
	p.midLine = false
	p.mu.Unlock()
}

func (p *Printer) style(code, s string) string {
	if !p.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p *Printer) dim(s string) string  { return p.style("2", s) }
func (p *Printer) bold(s string) string { return p.style("1", s) }
func (p *Printer) warn(s string) string { return p.style("33", s) }
func (p *Printer) fail(s string) string { return p.style("31", s) }

// accent marks geminus's own voice in the transcript: the banner and the
// mark before each prompt.
func (p *Printer) accent(s string) string { return p.style("1;36", s) }

// option is one line of a list the person can choose from.
type option struct {
	// key is what selecting the option returns, and what can be typed.
	key, text string
	// disabled options are shown but cannot be chosen.
	disabled bool
}

// showOptions prints a numbered list. Disabled options get no number.
func (p *Printer) showOptions(opts []option) {
	width := 0
	for _, o := range opts {
		width = max(width, len(oneLine(o.key)))
	}
	n := 0
	for _, o := range opts {
		mark := "-"
		if !o.disabled {
			n++
			mark = fmt.Sprint(n)
		}
		p.line(fmt.Sprintf("  %s  %-*s  %s", mark, width, oneLine(o.key), oneLine(o.text)))
	}
}

// match finds the option the person meant: a number, a key, or the unique
// start of a key. It returns "" when nothing or more than one thing fits.
func match(opts []option, typed string) string {
	var enabled []string
	for _, o := range opts {
		if !o.disabled {
			enabled = append(enabled, o.key)
		}
	}
	var n int
	if _, err := fmt.Sscanf(typed, "%d", &n); err == nil && fmt.Sprint(n) == typed && n >= 1 && n <= len(enabled) {
		return enabled[n-1]
	}
	var starts []string
	for _, key := range enabled {
		if strings.EqualFold(key, typed) {
			return key
		}
		if strings.HasPrefix(strings.ToLower(key), strings.ToLower(typed)) {
			starts = append(starts, key)
		}
	}
	if len(starts) == 1 {
		return starts[0]
	}
	return ""
}

// choose shows a list and asks until one option is picked. An empty answer
// takes preferred, when there is one. It returns false if the input ended.
func (a *App) choose(title, what string, opts []option, preferred string) (string, bool) {
	if match(opts, preferred) != preferred {
		preferred = ""
	}
	// On a terminal the list replaces the screen (the mascot says it) and
	// goes away when one is picked, so the lists do not pile up in what was
	// said. One line records the answer.
	if a.live != nil {
		picked, ok := a.live.askChoice(a.ctx, title, what, opts, preferred)
		if ok {
			if a.clawdEnabled() {
				a.Out.line(a.Out.dim("  ⎿  ") + sanitize(picked))
			} else {
				a.Out.line(a.Out.dim(what+" ❯ ") + sanitize(picked))
			}
		}
		return picked, ok
	}
	a.Out.line(a.Out.bold(title))
	a.Out.showOptions(opts)
	if a.live != nil {
		defer a.ask("")
	}
	for {
		ask := what
		if preferred != "" {
			ask += " [" + oneLine(preferred) + "]"
		}
		if a.live != nil {
			a.ask(ask) // the box asks; what is entered is echoed by readLine
		} else {
			a.Out.write(ask + " ❯ ")
		}
		typed, ok := a.readLine()
		if !ok {
			return "", false
		}
		if typed == "" && preferred != "" {
			return preferred, true
		}
		if picked := match(opts, typed); picked != "" {
			return picked, true
		}
		if typed != "" {
			a.Out.line(a.Out.fail(fmt.Sprintf("✗ no %s %q here; type a number or a name from the list", what, typed)))
		}
	}
}
