package chat

import (
	"html"
	"strings"
	"unicode/utf8"
)

// mdState carries an agent's output across the pieces a bridge posts, so a
// marker split between two pieces is not shown raw and then repaired.
type mdState struct {
	buf   string
	fence bool
}

// takeMarkdown renders one piece of an agent's reply. Until the piece is
// final, a trailing line with no newline stays buffered: that is where a
// split ** or ` would otherwise be painted as syntax.
func (a *App) takeMarkdown(turn, chunk string, final, plain bool) string {
	if a.md == nil {
		a.md = map[string]*mdState{}
	}
	st := a.md[turn]
	if st == nil {
		st = &mdState{}
		a.md[turn] = st
	}
	st.buf += chunk
	cut := len(st.buf)
	if !final {
		if i := strings.LastIndex(st.buf, "\n"); i >= 0 {
			cut = i + 1
		} else {
			return ""
		}
	}
	ready := st.buf[:cut]
	st.buf = st.buf[cut:]
	bold, dim := func(s string) string { return s }, func(s string) string { return s }
	if !plain && a.Out != nil {
		bold, dim = a.Out.bold, a.Out.dim
	}
	out := renderAgentMarkdown(ready, &st.fence, bold, dim)
	if final {
		delete(a.md, turn)
	}
	return out
}

// renderAgentMarkdown turns the markdown agents actually write into the
// styles geminus already uses. The markers themselves are not printed.
// Text inside a fence or inline code is left unchanged, apart from HTML
// entities, so a sample of markdown is not interpreted as formatting.
func renderAgentMarkdown(s string, fence *bool, bold, dim func(string) string) string {
	if fence == nil {
		closed := false
		fence = &closed
	}
	var b strings.Builder
	for len(s) > 0 {
		line, rest, nl := strings.Cut(s, "\n")
		s = rest
		b.WriteString(renderAgentLine(line, fence, bold, dim))
		if nl {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func renderAgentLine(line string, fence *bool, bold, dim func(string) string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "```") {
		*fence = !*fence
		return ""
	}
	if *fence {
		return html.UnescapeString(line)
	}
	if n, ok := headingMarks(line); ok {
		return bold(inlineMarkdown(strings.TrimSpace(line[n:]), bold, dim))
	}
	return inlineMarkdown(line, bold, dim)
}

func headingMarks(line string) (int, bool) {
	i := 0
	for i < len(line) && line[i] == '#' && i < 6 {
		i++
	}
	if i == 0 || i >= len(line) || (line[i] != ' ' && line[i] != '\t') {
		return 0, false
	}
	return i + 1, true
}

func inlineMarkdown(s string, bold, dim func(string) string) string {
	s = html.UnescapeString(s)
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '`' {
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				b.WriteByte('`')
				b.WriteString(s[i+1 : i+1+j])
				b.WriteByte('`')
				i += j + 2
				continue
			}
		}
		if strings.HasPrefix(s[i:], "**") || strings.HasPrefix(s[i:], "__") {
			mark := s[i : i+2]
			if j := strings.Index(s[i+2:], mark); j >= 0 {
				b.WriteString(bold(s[i+2 : i+2+j]))
				i += j + 4
				continue
			}
		}
		if s[i] == '*' {
			if j := strings.IndexByte(s[i+1:], '*'); j > 0 {
				b.WriteString(dim(s[i+1 : i+1+j]))
				i += j + 2
				continue
			}
		}
		if s[i] == '[' {
			if label, url, n, ok := markdownLink(s[i:]); ok {
				b.WriteString(inlineMarkdown(label, bold, dim))
				if url != "" && url != label {
					b.WriteString(dim(" (" + url + ")"))
				}
				i += n
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

func markdownLink(s string) (label, url string, n int, ok bool) {
	end := strings.IndexByte(s, ']')
	if end <= 1 || end+1 >= len(s) || s[end+1] != '(' {
		return "", "", 0, false
	}
	close := strings.IndexByte(s[end+2:], ')')
	if close < 0 {
		return "", "", 0, false
	}
	return s[1:end], s[end+2 : end+2+close], end + 2 + close + 1, true
}
