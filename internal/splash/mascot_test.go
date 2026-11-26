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

func TestTheMascotIsTheLogoInSmall(t *testing.T) {
	for y := 0; y < mascotH; y++ {
		for x := 0; x < mascotW; x++ {
			if !strings.ContainsRune(dotOrder, rune(broken[y][x])) {
				t.Fatalf("dot %d,%d is %q", x, y, broken[y][x])
			}
			// Before it broke the shell was the same on both sides.
			if whole[y][x] != whole[y][mascotW-1-x] {
				t.Errorf("the whole shell differs at %d,%d from its other side", x, y)
			}
			// What broke away is on the right, and the face inside shows
			// only there.
			if opening(x, y) && x < mascotW/2-1 {
				t.Errorf("the shell is broken at %d,%d, on the left", x, y)
			}
			if d := broken[y][x]; d == dotOlive && !opening(x, y) {
				t.Errorf("the face inside shows at %d,%d through shell that is whole", x, y)
			}
		}
	}
	count := map[byte]int{}
	for _, row := range broken {
		for _, d := range row {
			count[d]++
		}
	}
	if count[dotLime] < 60 || count[dotOlive] < 20 || count[dotBone] < 20 || count[dotInk] < 12 {
		t.Errorf("the mascot's dots: %d lime, %d olive, %d bone, %d ink", count[dotLime], count[dotOlive], count[dotBone], count[dotInk])
	}
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
	lost := 0
	for row := 0; row < MascotRows; row++ {
		for col := 0; col < MascotCols; col++ {
			kinds := map[byte]bool{}
			for i := 0; i < 4; i++ {
				kinds[broken[row*2+i/2][col*2+i%2]] = true
			}
			if len(kinds) > 2 {
				lost++
			}
		}
	}
	if lost > 3 {
		t.Errorf("%d characters of the mascot at rest hold more than two colours", lost)
	}
}

func TestTheMascotWithFewerColours(t *testing.T) {
	m := NewMascot("look", 0)
	for _, row := range m.Rows(0, Mono) {
		if strings.Contains(row, "\x1b") || len([]rune(row)) != MascotCols {
			t.Errorf("without colour: %q", row)
		}
	}
	if rows := m.Rows(0, Mono); !strings.ContainsAny(rows[2], "█▛▜▙▟") || strings.TrimSpace(rows[0]) == "" {
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
		if start != broken && start != whole {
			t.Errorf("%s does not start on the logo", name)
		}
		frames, atRest, back := map[sprite]bool{}, 0, false
		for i := 0; i < 1200; i++ {
			f := m.frame(float64(i) * 0.05)
			if f == broken {
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
		// Mostly still: the logo as it is, for at least half the time.
		if atRest < 600 {
			t.Errorf("%s: at rest on the logo for %d of 1200 frames", name, atRest)
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
