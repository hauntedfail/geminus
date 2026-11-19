package chat

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"

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

// sanitize removes control characters from text that came from the agent,
// so a reply cannot move the cursor, recolor the screen or retitle the window.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

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

func (p *Printer) write(s string) {
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
		v.titles[e.ID] = sanitize(e.Title)
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
	v.out.line(v.out.warn(fmt.Sprintf("  ? %s asks to run: %s", v.name, sanitize(req.Title))))
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
