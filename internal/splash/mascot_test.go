package splash

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// shown is what a terminal shows for rows the mascot wrote: for each dot,
// the colour it is drawn in ({-1,-1,-1} where nothing is drawn).
type shownDot [3]int

var sgrSeq = regexp.MustCompile(`^\x1b\[([0-9;]*)m`)

func decodeMascot(t *testing.T, rows [MascotRows]string) (dots [mascotH][mascotW]shownDot) {
	t.Helper()
	none := shownDot{-1, -1, -1}
	glyph := map[rune]int{}
	for mask, g := range quadrants {
		glyph[[]rune(g)[0]] = mask
	}
	for row, text := range rows {
		fore, back, col := none, none, 0
		for len(text) > 0 {
			if m := sgrSeq.FindStringSubmatch(text); m != nil {
				text = text[len(m[0]):]
				p := strings.Split(m[1], ";")
				switch {
				case m[1] == "0":
					fore, back = none, none
				case m[1] == "49":
					back = none
				case len(p) == 5 && p[1] == "2":
					var c shownDot
					for i := range c {
						c[i], _ = strconv.Atoi(p[2+i])
					}
					if p[0] == "38" {
						fore = c
					} else {
						back = c
					}
				case len(p) == 3 && p[1] == "5":
					n, _ := strconv.Atoi(p[2])
					c := shownDot{n, n, n}
					if p[0] == "38" {
						fore = c
					} else {
						back = c
					}
				default:
					t.Fatalf("row %d: unknown sequence %q", row, m[0])
				}
				continue
			}
			r := []rune(text)[0]
			text = text[len(string(r)):]
			mask, ok := glyph[r]
			if !ok {
				t.Fatalf("row %d: %q is not a block character", row, r)
			}
			if col >= MascotCols {
				t.Fatalf("row %d is wider than %d characters", row, MascotCols)
			}
			for i := 0; i < 4; i++ {
				c := back
				if mask&(1<<i) != 0 {
					c = fore
					if fore == none {
						t.Fatalf("row %d col %d: drawn before any colour was set", row, col)
					}
				}
				dots[row*2+i/2][col*2+i%2] = c
			}
			col++
		}
		if col != MascotCols {
			t.Fatalf("row %d is %d characters wide, want %d", row, col, MascotCols)
		}
		if fore != none || back != none {
			t.Fatalf("row %d leaves its colours on", row)
		}
	}
	return dots
}

func colourOf(d byte) shownDot {
	if d == dotNone {
		return shownDot{-1, -1, -1}
	}
	c := dotColour[d]
	return shownDot{int(channel(c.r)), int(channel(c.g)), int(channel(c.b))}
}

func TestTheMascotIsAHornedHeadWithAFaceThatReads(t *testing.T) {
	count := map[byte]int{}
	for y := 0; y < mascotH; y++ {
		for x := 0; x < mascotW; x++ {
			d := face[y][x]
			if !strings.ContainsRune(dotOrder, rune(d)) {
				t.Fatalf("dot %d,%d is %q", x, y, d)
			}
			count[d]++
			// One face, the same on both sides, but for where the eyes
			// look, for the lid over each eye, and for the teeth, which are uneven.
			if d != face[y][mascotW-1-x] && y != eyeY-1 && y != eyeY && y != teethY-2 && y != teethY && y != teethY+2 {
				t.Errorf("the face differs at %d,%d from its other side", x, y)
			}
			// Ink on the rim would be lost against a dark screen: every
			// feature has the head around it.
			if d == dotInk {
				for _, n := range [][2]int{{x - 1, y}, {x + 1, y}, {x, y - 1}, {x, y + 1}} {
					if n[0] < 0 || n[0] >= mascotW || n[1] < 0 || n[1] >= mascotH || face[n[1]][n[0]] == dotNone {
						t.Errorf("the ink at %d,%d is on the rim of the head", x, y)
					}
				}
			}
		}
	}
	if count[dotLime] < 120 || count[dotBone] < 30 || count[dotInk] < 60 {
		t.Errorf("the mascot's dots: %d lime, %d bone, %d ink", count[dotLime], count[dotBone], count[dotInk])
	}
	// Horns of bone at the top, and nothing of bone below them but teeth.
	if face[0][3] != dotBone || face[0][mascotW-4] != dotBone {
		t.Error("no horn tips on the top row")
	}
	// Two rows of teeth clenched: a lip of ink over them, a line of ink
	// where they meet, and gaps between the teeth, not evenly spaced.
	tusk := func(x int) bool {
		return x >= tuskX && x < tuskX+tuskW || x >= mascotW-tuskX-tuskW && x < mascotW-tuskX
	}
	upper, lower := 0, 0
	for x := teethX; x < teethX+teethW; x++ {
		if face[teethY][x] == dotBone {
			upper++
		}
		if face[teethY+2][x] == dotBone {
			lower++
		}
		if face[teethY-1][x] != dotInk {
			t.Errorf("no lip over the teeth at %d", x)
		}
		// A tusk runs from the upper teeth down past the jaw, and only a
		// tusk breaks the line where the rows meet.
		for dy := 0; dy < tuskRows; dy++ {
			if tusk(x) && face[teethY+dy][x] != dotBone {
				t.Errorf("the tusk at %d is broken %d rows down", x, dy)
			}
		}
		if !tusk(x) && face[teethY+1][x] != dotInk {
			t.Errorf("the line between the teeth is broken at %d", x)
		}
	}
	if upper < 10 || upper == teethW || lower < 6 || float64(teethW) < 0.55*float64(mascotW) {
		t.Errorf("the grin: %d dots of upper teeth, %d of lower, %d wide", upper, lower, teethW)
	}
	if string(face[teethY][teethX:teethX+teethW/2]) == reverse(string(face[teethY][teethX+teethW/2:teethX+teethW])) {
		t.Error("the upper teeth are the same on both sides")
	}
	// The eyes are narrow, white with the dark at one end, both looking
	// the same way. The lid is over the white only, so the dark stands
	// clear of it.
	for _, x := range [2]int{eyeX, mascotW - eyeX - eyeW} {
		if over, eye := string(face[eyeY-1][x:x+eyeW]), string(face[eyeY][x:x+eyeW]); over != "KKKKLL" || eye != "BBBBKK" {
			t.Errorf("the eye at %d: %q under %q", x, eye, over)
		}
	}
	// The corners of the mouth are drawn up to under the eyes, and two
	// nostrils sit apart, under the eyes and clear of the lip.
	if face[teethY-3][3] != dotInk || face[teethY-2][4] != dotInk || face[teethY-1][5] != dotInk {
		t.Error("the corner of the mouth is not drawn up to the eye")
	}
	if face[teethY-2][11] != dotInk || face[teethY-2][12] != dotLime || face[teethY-2][14] != dotInk ||
		face[teethY-3][11] != dotLime || face[teethY-3][14] != dotLime || teethY-2 <= eyeY {
		t.Error("no two nostrils under the eyes and clear of the lip")
	}
}

func reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

func TestTheMascotIsDrawnAsItsDotsSay(t *testing.T) {
	for _, name := range MascotAnims {
		m := NewMascot(name, 3)
		for i := 0; i < 240; i++ {
			at := float64(i) * 0.11
			s := m.frame(at)
			dots := decodeMascot(t, m.Rows(at, TrueColor))
			for row := 0; row < MascotRows; row++ {
				for col := 0; col < MascotCols; col++ {
					cell := [4]byte{s[row*2][col*2], s[row*2][col*2+1], s[row*2+1][col*2], s[row*2+1][col*2+1]}
					kinds := map[byte]bool{}
					for _, d := range cell {
						kinds[d] = true
					}
					for i, d := range cell {
						got := dots[row*2+i/2][col*2+i%2]
						// A character shows two colours. With more in its
						// four dots, each is shown as one of them.
						if len(kinds) <= 2 && got != colourOf(d) {
							t.Fatalf("%s at %.2fs: dot %d,%d is %q but is drawn %v", name, at, col*2+i%2, row*2+i/2, d, got)
						}
						ok := false
						for k := range kinds {
							ok = ok || got == colourOf(k)
						}
						if !ok {
							t.Fatalf("%s at %.2fs: dot %d,%d is drawn %v, a colour not in its character", name, at, col*2+i%2, row*2+i/2, got)
						}
					}
				}
			}
		}
	}
	// At rest no character has to give up a colour.
	for row := 0; row < MascotRows; row++ {
		for col := 0; col < MascotCols; col++ {
			kinds := map[byte]bool{}
			for i := 0; i < 4; i++ {
				kinds[face[row*2+i/2][col*2+i%2]] = true
			}
			if len(kinds) > 2 {
				t.Errorf("the character at %d,%d of the mascot at rest holds %d colours", col, row, len(kinds))
			}
		}
	}
}

func TestTheMascotWithFewerColours(t *testing.T) {
	m := NewMascot("ogle", 0)
	for _, row := range m.Rows(0, Mono) {
		if strings.Contains(row, "\x1b") || len([]rune(row)) != MascotCols {
			t.Errorf("without colour: %q", row)
		}
	}
	if rows := m.Rows(0, Mono); !strings.ContainsAny(rows[3], "█▛▜▙▟") || strings.TrimSpace(rows[0]) == "" {
		t.Errorf("without colour the mascot is not there: %q", rows)
	}
	for i, row := range m.Rows(0, Color256) {
		if strings.Contains(row, ";2;") || !strings.Contains(row, "\x1b[38;5;") {
			t.Errorf("row %d in 256 colours: %q", i, row)
		}
	}
	decodeMascot(t, m.Rows(0, Color256))
}

func TestEveryMascotAnimationStartsAtRestMovesAndComesBack(t *testing.T) {
	for _, name := range MascotAnims {
		m := NewMascot(name, 9)
		if m.Name() != name {
			t.Fatalf("NewMascot(%q) is doing %q", name, m.Name())
		}
		start := m.frame(0)
		if start != face {
			t.Errorf("%s does not start at rest", name)
		}
		frames, atRest, back := map[sprite]bool{}, 0, false
		for i := 0; i < 1200; i++ {
			f := m.frame(float64(i) * 0.05)
			if f == face {
				atRest++
			}
			if frames[f] = true; len(frames) > 1 && i > 600 && f == start {
				back = true
			}
			for _, row := range f {
				for _, d := range row {
					if !strings.ContainsRune(dotOrder, rune(d)) {
						t.Fatalf("%s: a dot is %q", name, d)
					}
				}
			}
			if f != m.frame(float64(i)*0.05) {
				t.Fatalf("%s: the same moment drawn twice differs", name)
			}
		}
		if len(frames) < 3 || !back {
			t.Errorf("%s: %d different frames in a minute, back at the start: %v", name, len(frames), back)
		}
		// Mostly still: at rest for at least half the time.
		if atRest < 600 {
			t.Errorf("%s: at rest for %d of 1200 frames", name, atRest)
		}
	}
	seen := map[string]bool{}
	for n := uint32(0); n < 50; n++ {
		seen[NewMascot("", n).Name()] = true
		if NewMascot("no such thing", n).Name() != NewMascot("", n).Name() {
			t.Error("an unknown name did not leave the choice to chance")
		}
	}
	if len(seen) != len(MascotAnims) {
		t.Errorf("chance reaches %d of %d animations", len(seen), len(MascotAnims))
	}
}
