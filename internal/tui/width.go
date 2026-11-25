package tui

// How many terminal columns text takes. The prompt box is drawn by hand, so
// its right edge and the cursor only land where they should if this agrees
// with the terminal.

import (
	"strings"
	"unicode"
)

// TabWidth is how wide a tab is drawn inside the prompt box.
const TabWidth = 4

// wideRanges are the characters terminals draw two columns wide: East Asian
// wide and fullwidth forms, and emoji. Sorted, inclusive.
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x2329, 0x232A}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3},
	{0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1},
	{0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE}, {0x26D4, 0x26D4}, {0x26EA, 0x26EA},
	{0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA}, {0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B},
	{0x2728, 0x2728}, {0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27B0, 0x27B0}, {0x27BF, 0x27BF}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55}, {0x2E80, 0x303E},
	{0x3041, 0x33FF}, {0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0xA000, 0xA4CF}, {0xA960, 0xA97F}, {0xAC00, 0xD7A3},
	{0xF900, 0xFAFF}, {0xFE10, 0xFE19}, {0xFE30, 0xFE6F}, {0xFF00, 0xFF60}, {0xFFE0, 0xFFE6}, {0x1F004, 0x1F004},
	{0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A}, {0x1F200, 0x1F251}, {0x1F300, 0x1F64F},
	{0x1F680, 0x1F6FF}, {0x1F900, 0x1F9FF}, {0x1FA70, 0x1FAFF}, {0x20000, 0x3FFFD},
}

func isWide(r rune) bool {
	lo, hi := 0, len(wideRanges)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case r < wideRanges[mid][0]:
			hi = mid
		case r > wideRanges[mid][1]:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// RuneWidth is the number of columns r takes.
func RuneWidth(r rune) int {
	switch {
	case r == '\t':
		return TabWidth
	case r < 0x20 || r == 0x7f:
		return 0
	case r < 0x300:
		return 1
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r):
		return 0
	case isWide(r):
		return 2
	}
	return 1
}

// SkipStyle returns the length of the styling sequence (ESC [ ... m) at the
// start of s, or 0.
func SkipStyle(s string) int {
	if !strings.HasPrefix(s, "\x1b[") {
		return 0
	}
	for i := 2; i < len(s); i++ {
		if c := s[i]; c == 'm' {
			return i + 1
		} else if (c < '0' || c > '9') && c != ';' {
			return 0
		}
	}
	return 0
}

// TextWidth is the number of columns s takes; styling takes none.
func TextWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if n := SkipStyle(s[i:]); n > 0 {
			i += n
			continue
		}
		r, size := rune(s[i]), 1
		if r >= 0x80 {
			r, size = DecodeRune(s[i:])
		}
		w += RuneWidth(r)
		i += size
	}
	return w
}

// ClipWidth shortens unstyled s to at most w columns, ending in "…" when
// something was cut.
func ClipWidth(s string, w int) string {
	if TextWidth(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := RuneWidth(r)
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
}

// ClipLeft shortens unstyled s to at most w columns by dropping its start,
// which "…" then stands for. A path keeps the part that names the place.
func ClipLeft(s string, w int) string {
	if TextWidth(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	runes := []rune(s)
	used, from := 0, len(runes)
	for from > 0 && used+RuneWidth(runes[from-1]) <= w-1 {
		from--
		used += RuneWidth(runes[from])
	}
	return "…" + string(runes[from:])
}

// WrapText cuts s, which has no line breaks, into rows of at most w columns.
// Styling is kept with the text it precedes.
func WrapText(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	var rows []string
	var row strings.Builder
	used := 0
	for i := 0; i < len(s); {
		if n := SkipStyle(s[i:]); n > 0 {
			row.WriteString(s[i : i+n])
			i += n
			continue
		}
		r, size := rune(s[i]), 1
		if r >= 0x80 {
			r, size = DecodeRune(s[i:])
		}
		i += size
		piece, rw := string(r), RuneWidth(r)
		if r == '\t' {
			piece = strings.Repeat(" ", TabWidth)
		}
		if used+rw > w && used > 0 {
			rows = append(rows, row.String())
			row.Reset()
			used = 0
		}
		row.WriteString(piece)
		used += rw
	}
	return append(rows, row.String())
}
