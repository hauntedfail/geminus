package chat

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/hauntedfail/geminus/internal/acp"
)

func isTerminal(w any) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// unsafeRune reports whether r could make a terminal do something other than
// show a character: control characters (escape sequences start with one) and
// the bidirectional overrides that reorder what is displayed.
func unsafeRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// sanitize removes everything from s that could control the terminal, so
// text from an agent, a remote shell or the directory cannot move the cursor,
// recolor or clear the screen, or retitle the window.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if unsafeRune(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
}

// oneLine is sanitize for values shown inside a line, such as a name or an
// ID: line breaks become spaces so the value cannot imitate a status line.
func oneLine(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\n", " ", "\t", " ").Replace(sanitize(s)))
}

// The only escape sequences geminus writes itself; see Printer.style.
var ownStyles = []string{"\x1b[0m", "\x1b[1m", "\x1b[2m", "\x1b[31m", "\x1b[33m"}

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

// turnView shows one turn of the agent: its reply as plain text, and its
// thinking, tool calls and permission requests as dim side notes.
type turnView struct {
	out      *Printer
	name     string
	thinking bool
	titles   map[string]string
}

func newTurnView(out *Printer, name string) *turnView {
	return &turnView{out: out, name: name, titles: map[string]string{}}
}

func (v *turnView) text(s string) {
	v.thinking = false
	s = sanitize(s)
	if v.out.atLineStart() {
		// Agents often open a reply with blank lines; after a side note
		// they would leave a gap.
		s = strings.TrimLeft(s, "\n")
	}
	v.out.write(s)
}

// thought shows that the agent is thinking, once, not what it thinks.
func (v *turnView) thought(string) {
	if !v.thinking {
		v.thinking = true
		v.out.line(v.out.dim("  … thinking"))
	}
}

func (v *turnView) tool(e acp.ToolEvent) {
	v.thinking = false
	if !e.Update {
		v.titles[e.ID] = oneLine(e.Title)
		v.out.line(v.out.dim("  ⚙ " + v.titles[e.ID]))
		return
	}
	if e.Status == "failed" {
		v.out.line(v.out.dim("  ✗ " + v.titles[e.ID] + " failed"))
	}
}

func (v *turnView) end() { v.out.line("") }

// permission asks the person whether a tool call may run and returns the ID
// of the chosen option. Anything but an explicit yes declines.
func (v *turnView) permission(req acp.PermissionRequest, lines <-chan string, stop <-chan struct{}, entered func(string)) string {
	byKind := map[string]string{}
	for _, o := range req.Options {
		if _, seen := byKind[o.Kind]; !seen {
			byKind[o.Kind] = o.ID
		}
	}
	choices := []string{}
	if byKind["allow_once"] != "" {
		choices = append(choices, "[y] allow once")
	}
	if byKind["allow_always"] != "" {
		choices = append(choices, "[a] always allow")
	}
	choices = append(choices, "[n] reject")
	v.out.line(v.out.warn(fmt.Sprintf("  ? %s asks to run: %s", v.name, oneLine(req.Title))))
	v.out.write("    " + strings.Join(choices, "  ") + " (default n) ❯ ")

	var answer string
	select {
	case line, ok := <-lines:
		if !ok {
			v.out.line("")
			return ""
		}
		entered(line)
		answer = strings.ToLower(strings.TrimSpace(line))
	case <-stop:
		v.out.line("")
		return ""
	}
	switch answer {
	case "y", "yes":
		return byKind["allow_once"]
	case "a", "always":
		return byKind["allow_always"]
	}
	return byKind["reject_once"]
}

// replay prints the conversation so far when a session is reopened.
type replay struct {
	out  *Printer
	role string
	any  bool
}

func newReplay(out *Printer) *replay { return &replay{out: out} }

func (r *replay) chunk(role, label, s string) {
	if !r.any {
		r.any = true
		r.out.line(r.out.dim("── conversation so far ──"))
	}
	if r.role != role {
		r.role = role
		r.out.line(r.out.dim(label))
	}
	r.out.write(r.out.dim(sanitize(s)))
}

func (r *replay) handler() *acp.Handler {
	return &acp.Handler{
		UserText: func(s string) { r.chunk("user", "you:", s) },
		Text:     func(s string) { r.chunk("agent", "agent:", s) },
	}
}

func (r *replay) end() {
	if r.any {
		r.out.line(r.out.dim("── end of history ──"))
	}
}

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
	a.Out.line(a.Out.bold(title))
	a.Out.showOptions(opts)
	for {
		ask := what
		if preferred != "" {
			ask += " [" + oneLine(preferred) + "]"
		}
		a.Out.write(ask + " ❯ ")
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
