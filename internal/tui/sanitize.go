package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// UnsafeRune reports whether r could make a terminal do something other than
// show a character: control characters (escape sequences start with one) and
// the bidirectional overrides that reorder what is displayed.
func UnsafeRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// Sanitize removes everything from s that could control the terminal, so
// text from an agent, a remote shell or the directory cannot move the cursor,
// recolor or clear the screen, or retitle the window.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if UnsafeRune(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
}

// OneLine is Sanitize for values shown inside a line, such as a name or an
// ID: line breaks become spaces so the value cannot imitate a status line.
func OneLine(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\n", " ", "\t", " ").Replace(Sanitize(s)))
}

// Clip shortens s to at most n characters.
func Clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
