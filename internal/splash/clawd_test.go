package splash

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClawdMascotDimensionsAndColorModes(t *testing.T) {
	mono := ClawdRowsStrings(Mono)
	for i, row := range mono {
		if strings.Contains(row, "\x1b[") {
			t.Fatalf("Mono row %d contains escape sequence: %q", i, row)
		}
		if got := utf8.RuneCountInString(row); got != ClawdCols {
			t.Fatalf("Mono row %d rune count = %d, want %d (%q)", i, got, ClawdCols, row)
		}
	}

	for _, tc := range []struct {
		name string
		mode ColorMode
		fg   string
		bg   string
	}{
		{"Color256", Color256, "\x1b[38;5;", "\x1b[48;5;"},
		{"TrueColor", TrueColor, "\x1b[38;2;215;119;87m", "\x1b[48;2;0;0;0m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := ClawdRowsStrings(tc.mode)
			if !strings.Contains(rows[0], tc.fg) {
				t.Errorf("%s row 0 missing foreground %q: %q", tc.name, tc.fg, rows[0])
			}
			if !strings.Contains(rows[0], tc.bg) {
				t.Errorf("%s row 0 missing dark eye background %q: %q", tc.name, tc.bg, rows[0])
			}
			for i, row := range rows {
				if !strings.HasSuffix(row, "\x1b[0m") {
					t.Errorf("%s row %d does not end with SGR reset: %q", tc.name, i, row)
				}
				if got := utf8.RuneCountInString(stripSGR(row)); got != ClawdCols {
					t.Errorf("%s row %d visible width = %d, want %d", tc.name, i, got, ClawdCols)
				}
			}
		})
	}
}

func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "\x1b[") {
			if end := strings.IndexByte(s[i+2:], 'm'); end >= 0 {
				i += end + 3
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
