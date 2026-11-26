package splash

import (
	"math"
	"strconv"
	"strings"
)

// The mascot is the logo as a small figure of block characters: what sits
// beside the name when geminus opens. It is drawn by hand on a grid of
// mascotW by mascotH dots, two across and two down to a character, and it
// moves now and then.

// MascotCols and MascotRows are the mascot's size in characters.
const (
	MascotCols, MascotRows = 12, 5
	mascotW, mascotH       = MascotCols * 2, MascotRows * 2
)

// A dot of the mascot: nothing, lime shell, bone (horns, the broken rim, the
// white of the eye), the olive of the face inside, or ink.
const (
	dotNone  = '.'
	dotLime  = 'L'
	dotBone  = 'B'
	dotOlive = 'O'
	dotInk   = 'K'
)

type sprite [mascotH][mascotW]byte

func parseSprite(rows [mascotH]string) (s sprite) {
	for y, row := range rows {
		if len(row) != mascotW {
			panic("splash: a mascot row is " + strconv.Itoa(len(row)) + " dots wide")
		}
		copy(s[y][:], row)
	}
	return s
}

// broken is the mascot as the logo shows it: the shell smiling with its eye
// shut on the left, broken away on the right, and the other face inside.
var broken = parseSprite([mascotH]string{
	".....BB..........BB.....",
	"....BBBBLLLLBBOOBBBB....",
	".....LLLLLLBBKOOOOL.....",
	"....LLLLLLLLBBKKOOLL....",
	".LLLLLKLLKLBBKBBOOOLLLL.",
	".LLLLLLKKLLLBBKBOOOLLLL.",
	".LLLLKLLLLLBBKOOOOOLLLL.",
	"....LLKKKKKLBBOKKOOL....",
	".....LLLLLLBBOKOOKL.....",
	"........LLLLBOOL........",
})

// whole is the shell before it broke: the left half and its reflection.
var whole = func() sprite {
	s := broken
	for y := range s {
		for x := range s[y] {
			if x >= mascotW/2 {
				s[y][x] = s[y][mascotW-1-x]
			}
			// No crack yet: where the rim will be is still shell.
			if s[y][x] == dotBone && y > 1 {
				s[y][x] = dotLime
			}
		}
	}
	return s
}()

// The inner face's eye is two dots by two, at (eyeX, eyeY): white, with
// the pupil in a lower corner.
const eyeX, eyeY = 14, 4

// eye redraws the inner face's eye: looking the other way, or shut.
func (s *sprite) eye(other, shut bool) {
	switch {
	case shut:
		s[eyeY][eyeX], s[eyeY][eyeX+1] = dotOlive, dotOlive
		s[eyeY+1][eyeX], s[eyeY+1][eyeX+1] = dotInk, dotInk
	case other:
		s[eyeY+1][eyeX], s[eyeY+1][eyeX+1] = dotBone, dotInk
	}
}

// opening reports whether a dot is where the shell is broken away.
func opening(x, y int) bool { return broken[y][x] != whole[y][x] }

// MascotAnims names what the mascot can do.
var MascotAnims = []string{"crack", "look", "shine", "glitch", "spin"}

// Mascot is the mascot doing one of the things it can do, over and over.
type Mascot struct {
	anim int
	seed uint32
}

// NewMascot returns the mascot doing what name says, or what n selects when
// there is nothing of that name.
func NewMascot(name string, n uint32) Mascot {
	for i, a := range MascotAnims {
		if a == name {
			return Mascot{anim: i, seed: n}
		}
	}
	return Mascot{anim: int(n % uint32(len(MascotAnims))), seed: n}
}

// Name is what the mascot is doing.
func (m Mascot) Name() string { return MascotAnims[m.anim] }

// frame is the mascot's dots t seconds after it appeared. Every animation
// is at rest, as the logo shows it, for most of its period.
func (m Mascot) frame(t float64) sprite {
	t = math.Max(t, 0)
	switch MascotAnims[m.anim] {
	case "crack":
		return m.crack(t)
	case "look":
		return m.look(t)
	case "shine":
		return m.shine(t)
	case "glitch":
		return m.glitch(t)
	}
	return m.spin(t)
}

// blink shuts the inner eye for a moment every few seconds.
func blink(s *sprite, t float64) {
	if p := math.Mod(t, 4.3); p > 3.9 && p < 4.05 {
		s.eye(false, true)
	}
}

// crack starts with the shell whole. A crack runs down it, the right half
// falls away from the face inside, and later it closes again.
func (m Mascot) crack(t float64) sprite {
	const period = 10.0
	p := math.Mod(t, period)
	// How far the broken part has fallen, in dots: 0 is whole, mascotH gone.
	var fallen float64
	switch {
	case p < 1.6:
		fallen = 0
	case p < 2.6:
		// The crack first, then the fall.
		fallen = math.Max(p-2.0, 0) / 0.6 * mascotH
	case p < 8.4:
		fallen = mascotH
	case p < 9.0:
		fallen = (9.0 - p) / 0.6 * mascotH
	}
	s := broken
	if fallen < mascotH {
		d := int(fallen)
		for y := 0; y < mascotH; y++ {
			for x := 0; x < mascotW; x++ {
				if !opening(x, y) {
					continue
				}
				// The shell that was d dots higher is here now, if it was
				// part of what broke away.
				if from := y - d; from >= 0 && opening(x, from) {
					s[y][x] = whole[from][x]
				}
			}
		}
	}
	// The crack itself shows from the moment it starts to run.
	if p < 1.6 || p >= 9.0 {
		return whole
	}
	if p < 2.0 {
		s = whole
		reach := int((p - 1.6) / 0.4 * mascotH)
		for y := 0; y <= reach && y < mascotH; y++ {
			for x := 0; x < mascotW; x++ {
				if broken[y][x] == dotBone && whole[y][x] == dotLime {
					s[y][x] = dotBone
				}
			}
		}
		return s
	}
	if fallen >= mascotH {
		blink(&s, t)
	}
	return s
}

// look lets the eye of the face inside wander and blink.
func (m Mascot) look(t float64) sprite {
	s := broken
	switch p := math.Mod(t, 6.0); {
	case p > 1.5 && p < 2.6:
		s.eye(true, false)
	case p > 3.4 && p < 3.55, p > 3.75 && p < 3.9:
		s.eye(false, true)
	}
	return s
}

// shine runs a gleam across the shell.
func (m Mascot) shine(t float64) sprite {
	s := broken
	// At rest first, as in every animation: geminus opens on the logo.
	if p := math.Mod(t, 5.0) - 4.0; p >= 0 {
		at := int(p*float64(mascotW+2*mascotH+6)) - 3
		for y := range s {
			for x := range s[y] {
				if d := x + 2*y - at; s[y][x] == dotLime && d >= 0 && d < 3 {
					s[y][x] = dotBone
				}
			}
		}
	}
	blink(&s, t)
	return s
}

// glitch shows the mascot as a bad signal now and then: rows of it jump
// sideways.
func (m Mascot) glitch(t float64) sprite {
	s := broken
	if p := math.Mod(t, 4.5); p >= 3.8 {
		tick := uint32(t * 12)
		for y := 0; y < mascotH; y += 2 {
			if frand(uint32(y), tick, m.seed) > 0.45 {
				continue
			}
			shift := int(hash(uint32(y), tick, m.seed+1)%7) - 3
			for dy := 0; dy < 2; dy++ {
				var row [mascotW]byte
				for x := range row {
					row[x] = dotNone
					if from := x - shift; from >= 0 && from < mascotW {
						row[x] = broken[y+dy][from]
					}
				}
				s[y+dy] = row
			}
		}
		return s
	}
	blink(&s, t)
	return s
}

// spin turns the mascot about its upright axis like a coin.
func (m Mascot) spin(t float64) sprite {
	p := math.Mod(t, 6.0) - 4.2
	if p < 0 {
		s := broken
		blink(&s, t)
		return s
	}
	turn := math.Cos(2 * math.Pi * smooth(p/1.8))
	var s sprite
	centre := float64(mascotW-1) / 2
	for y := range s {
		for x := range s[y] {
			s[y][x] = dotNone
			if math.Abs(turn) < 0.12 {
				// Edge on.
				if (x == mascotW/2-1 || x == mascotW/2) && y > 0 {
					s[y][x] = dotLime
				}
				continue
			}
			from := int(math.Round(centre + (float64(x)-centre)/turn))
			if from >= 0 && from < mascotW {
				s[y][x] = broken[y][from]
			}
		}
	}
	return s
}

// quadrants are the block characters by which of a character's four dots
// are filled: 1 upper left, 2 upper right, 4 lower left, 8 lower right.
var quadrants = [16]string{" ", "▘", "▝", "▀", "▖", "▌", "▞", "▛", "▗", "▚", "▐", "▜", "▄", "▙", "▟", "█"}

var dotColour = map[byte]rgb{dotLime: lime, dotBone: bone, dotOlive: olive, dotInk: {0, 0, 0}, dotNone: {0, 0, 0}}

// dotOrder breaks ties when a character holds more colours than it can
// show: what carries the features wins.
const dotOrder = "KBLO."

// pair picks the two colours a character shows for its four dots, and says
// which dots are in the first.
func pair(dots [4]byte) (first, second byte, mask int) {
	var count [len(dotOrder)]int
	for _, d := range dots {
		count[strings.IndexByte(dotOrder, d)]++
	}
	a, b := -1, -1
	for i, n := range count {
		switch {
		case n == 0:
		case a < 0 || n > count[a]:
			a, b = i, a
		case b < 0 || n > count[b]:
			b = i
		}
	}
	first = dotOrder[a]
	if b < 0 {
		return first, first, 15
	}
	second = dotOrder[b]
	near := func(d byte) bool {
		c, f, s := dotColour[d], dotColour[first], dotColour[second]
		dist := func(o rgb) float64 { return math.Abs(c.r-o.r) + math.Abs(c.g-o.g) + math.Abs(c.b-o.b) }
		return dist(f) <= dist(s)
	}
	for i, d := range dots {
		if d == first || d != second && near(d) {
			mask |= 1 << i
		}
	}
	return first, second, mask
}

func (m ColorMode) sgr(b *strings.Builder, back bool, c rgb) {
	lead := "\x1b[38;"
	if back {
		lead = "\x1b[48;"
	}
	b.WriteString(lead)
	if m == TrueColor {
		b.WriteString("2;" + strconv.Itoa(int(channel(c.r))) + ";" + strconv.Itoa(int(channel(c.g))) + ";" + strconv.Itoa(int(channel(c.b))))
	} else {
		b.WriteString("5;" + strconv.Itoa(Color256.key(c.cell(' '))))
	}
	b.WriteByte('m')
}

// Rows draws the mascot t seconds after it appeared: MascotRows rows of
// MascotCols characters each, with their colours.
func (m Mascot) Rows(t float64, mode ColorMode) [MascotRows]string {
	s := m.frame(t)
	var out [MascotRows]string
	for row := 0; row < MascotRows; row++ {
		var b strings.Builder
		fore, back := byte(0), byte(0) // what the terminal is set to; 0 is its own
		for col := 0; col < MascotCols; col++ {
			x, y := col*2, row*2
			first, second, mask := pair([4]byte{s[y][x], s[y][x+1], s[y+1][x], s[y+1][x+1]})
			if mode == Mono {
				// Without colour only the light parts are drawn.
				mask = 0
				for i, d := range [4]byte{s[y][x], s[y][x+1], s[y+1][x], s[y+1][x+1]} {
					if d == dotLime || d == dotBone {
						mask |= 1 << i
					}
				}
				b.WriteString(quadrants[mask])
				continue
			}
			// The character is drawn in one colour on the other. Nothing is
			// never drawn: it is what shows behind.
			fg, bg := first, second
			switch {
			case fg == bg && fg == dotNone:
				mask = 0
			case fg == bg:
				mask, bg = 15, back
			case fg == dotNone:
				fg, bg, mask = second, 0, 15^mask
			case bg == dotNone:
				bg = 0
			}
			if mask == 0 {
				bg = 0
			}
			if bg != back {
				if bg == 0 {
					b.WriteString("\x1b[49m")
				} else {
					mode.sgr(&b, true, dotColour[bg])
				}
				back = bg
			}
			if mask != 0 && fg != fore {
				mode.sgr(&b, false, dotColour[fg])
				fore = fg
			}
			b.WriteString(quadrants[mask])
		}
		if fore != 0 || back != 0 {
			b.WriteString("\x1b[0m")
		}
		out[row] = b.String()
	}
	return out
}
