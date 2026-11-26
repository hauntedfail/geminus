package splash

import (
	"math"
	"strconv"
	"strings"
)

// The mascot is a small figure of block characters: what sits beside the
// name when geminus opens. It is the logo's head, round, horned and
// long-eared, with one face that can be read at this size: a grotesque
// carved in stone, gloating. The eyes are half shut under heavy lids and
// look sideways, at the name beside it. The grin is as wide as the face, its
// corners drawn up to the cheekbones, both rows of uneven teeth clenched and
// a tusk at each corner hanging down past the jaw. It is drawn by hand on a
// grid of mascotW by mascotH dots, two across and two down to a character,
// and it moves now and then.

// MascotCols and MascotRows are the mascot's size in characters.
const (
	MascotCols, MascotRows = 14, 6
	mascotW, mascotH       = MascotCols * 2, MascotRows * 2
)

// A dot of the mascot: nothing, lime (the head), bone (horns, teeth, the
// white of an eye) or ink.
const (
	dotNone = '.'
	dotLime = 'L'
	dotBone = 'B'
	dotInk  = 'K'
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

// face is the mascot at rest. No character of it holds more than two
// colours, so it is drawn exactly.
var face = parseSprite([mascotH]string{
	"...BB..LLLLLLLLLLLLLL..BB...",
	"..BBBBLLLLLLLLLLLLLLLLBBBB..",
	"....LLKKKKLLLLLLKKKKLLLL....",
	"..LLLLBBBBKKLLLLBBBBKKLLLL..",
	"LLLKLLLLLLLLLLLLLLLLLLLLKLLL",
	"LLLLKLLLLLLKLLKLLLLLLLKLLLLL",
	"LKLLLKKKKKKKKKKKKKKKKKKLLLKL",
	"LLLLLKBBKBBKBBBKBKBKBBKLLLLL",
	"..LLLLBBKKKKKKKKKKKKBBLLLL..",
	"...LLLBBKBKBBKBBBKBKBBLLL...",
	".....LBBLKKKKKKKKKKLBBL.....",
	"........LLLLLLLLLLLL........",
})

// Where the features are, in dots.
const (
	// An eye is eyeW dots wide on row eyeY: the white, and the dark at one
	// end. The lid lies on the row over it, over the white; wide open the
	// eye takes that row as well. The left eye starts at
	// eyeX, the right eye as far from the other side. Both look the same
	// way, so they are drawn alike.
	eyeY, eyeX, eyeW = 3, 6, 6
	// The teeth: the upper row from teethX on teethY, teethW wide; the
	// lower row two rows down, under the line where they meet.
	teethY, teethX, teethW = 7, 6, 16
	// The tusks: tuskW dots wide from tuskX and from as far from the other
	// side, at the ends of the upper teeth, down past the jaw.
	tuskX, tuskW, tuskRows = 6, 2, 4
)

// eyes redraws both eyes: the row over the eye and the eye's own, each eyeW
// dots from left to right.
func (s *sprite) eyes(over, eye string) {
	for _, x := range [2]int{eyeX, mascotW - eyeX - eyeW} {
		copy(s[eyeY-1][x:x+eyeW], over)
		copy(s[eyeY][x:x+eyeW], eye)
	}
}

// MascotAnims names what the mascot can do.
var MascotAnims = []string{"laugh", "ogle", "shine", "glitch", "spin"}

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
// starts at rest and is at rest for most of its period.
func (m Mascot) frame(t float64) sprite {
	t = math.Max(t, 0)
	switch MascotAnims[m.anim] {
	case "laugh":
		return m.laugh(t)
	case "ogle":
		return m.ogle(t)
	case "shine":
		return m.shine(t)
	case "glitch":
		return m.glitch(t)
	}
	return m.spin(t)
}

// laugh screws the eyes shut and opens and shuts the jaw a few times: the
// lower teeth drop out of sight and come back.
func (m Mascot) laugh(t float64) sprite {
	s := face
	p := math.Mod(t, 5.0) - 3.6
	if p < 0 {
		return s
	}
	s.eyes("LLLLLL", "KKKKKK")
	if int(p*9)%2 == 1 {
		return s
	}
	// The tusks belong to the upper jaw: they stay.
	for x := tuskX + tuskW; x < mascotW-tuskX-tuskW; x++ {
		if s[teethY+2][x] == dotBone {
			s[teethY+2][x] = dotInk
		}
	}
	return s
}

// ogle opens the eyes wide, as at something it wants, looks the other way
// and back, and narrows them again.
func (m Mascot) ogle(t float64) sprite {
	s := face
	switch p := math.Mod(t, 6.0) - 3.8; {
	case p < 0:
	case p >= 0.5 && p < 1.1:
		s.eyes("KKBBBB", "KKBBBB")
	case p < 1.9:
		s.eyes("BBBBKK", "BBBBKK")
	}
	return s
}

// shine runs a gleam across the head.
func (m Mascot) shine(t float64) sprite {
	s := face
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
	return s
}

// glitch shows the mascot as a bad signal now and then: rows of it jump
// sideways.
func (m Mascot) glitch(t float64) sprite {
	s := face
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
						row[x] = face[y+dy][from]
					}
				}
				s[y+dy] = row
			}
		}
	}
	return s
}

// spin turns the mascot about its upright axis like a coin.
func (m Mascot) spin(t float64) sprite {
	p := math.Mod(t, 6.0) - 4.2
	if p < 0 {
		return face
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
				s[y][x] = face[y][from]
			}
		}
	}
	return s
}

// quadrants are the block characters by which of a character's four dots
// are filled: 1 upper left, 2 upper right, 4 lower left, 8 lower right.
var quadrants = [16]string{" ", "▘", "▝", "▀", "▖", "▌", "▞", "▛", "▗", "▚", "▐", "▜", "▄", "▙", "▟", "█"}

var dotColour = map[byte]rgb{dotLime: lime, dotBone: bone, dotInk: {0, 0, 0}, dotNone: {0, 0, 0}}

// dotOrder breaks ties when a character holds more colours than it can
// show: what carries the features wins.
const dotOrder = "KBL."

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
				// Two colours the same: a solid character, with the
				// terminal's own background showing behind it. The
				// background the last character set would hide the
				// difference between lime and bone.
				mask, bg = 15, 0
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
