package chat

// Live ephemeral preview tail support for real-time token streaming.
// Completed lines are committed into l.lines via l.Write; the currently
// unfinished streaming line lives in l.preview and is redrawn in place until
// a newline commits it with full Markdown formatting.

func (l *live) combinedTail() string {
	if l.preview == "" {
		return l.tail
	}
	return l.tail + l.preview
}

// setPreview updates the in-place unfinished streaming preview line and redraws.
func (l *live) setPreview(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.preview == text {
		return
	}
	w := l.width()
	before := 0
	if l.scroll > 0 {
		before = l.wrappedCount(w)
	}
	l.preview = text
	if l.scroll > 0 {
		if delta := l.wrappedCount(w) - before; delta > 0 {
			l.scroll += delta
		}
	}
	l.draw()
}

// clearPreview removes any in-place streaming preview line. When redraw is true
// the viewport is redrawn immediately.
func (l *live) clearPreview(redraw bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.preview == "" {
		return
	}
	l.preview = ""
	if redraw && !l.closed {
		l.draw()
	}
}
