package splash

import "strings"

// ClawdCols and ClawdRows are the dimensions of the Clawd mascot in characters.
const (
	ClawdCols = 9
	ClawdRows = 3
)

// clawdCoral is Claude Code's terracotta coral (#D77757).
var clawdCoral = rgb{215, 119, 87}

// clawdLines is the canonical 3-row block-character silhouette of Clawd.
// On row 0, ▛ and ▜ have corner cutouts that form the dark eyes.
var clawdLines = [ClawdRows]string{
	" ▐▛███▜▌ ",
	"▝▜█████▛▘",
	"  ▘▘ ▝▝  ",
}

// ClawdRowsStrings renders the Clawd mascot in the requested color mode.
// Each row is ClawdCols display cells wide.
func ClawdRowsStrings(mode ColorMode) [ClawdRows]string {
	if mode == Mono {
		return clawdLines
	}
	var out [ClawdRows]string
	for row, line := range clawdLines {
		var b strings.Builder
		foreSet, backDark := false, false
		for col, r := range []rune(line) {
			if r == ' ' {
				if backDark {
					b.WriteString("\x1b[49m")
					backDark = false
				}
				b.WriteByte(' ')
				continue
			}
			eye := row == 0 && (col == 2 || col == 6)
			if !foreSet {
				mode.sgr(&b, false, clawdCoral)
				foreSet = true
			}
			if eye != backDark {
				if eye {
					mode.sgr(&b, true, rgb{0, 0, 0})
				} else {
					b.WriteString("\x1b[49m")
				}
				backDark = eye
			}
			b.WriteRune(r)
		}
		if foreSet || backDark {
			b.WriteString("\x1b[0m")
		}
		out[row] = b.String()
	}
	return out
}
