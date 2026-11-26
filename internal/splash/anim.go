package splash

import "math"

// Scene is the logo fitted to a screen of one size.
type Scene struct {
	art        *Art
	Cols, Rows int
	// The logo's place when it stands still: w by h cells at (x0, y0).
	x0, y0, w, h int
	base         []Cell
	seed         uint32
}

// cellAspect is how many times taller than wide a terminal cell is.
const cellAspect = 2.0

// NewScene fits the logo to cols by rows cells. The bottom row is kept free
// for a line of text. seed varies what is random in the animations.
func NewScene(art *Art, cols, rows int, seed uint32) *Scene {
	s := &Scene{art: art, Cols: cols, Rows: rows, seed: seed}
	// One row free above the logo, one between it and the text.
	s.h = max(rows-3, 1)
	s.w = int(float64(s.h)*cellAspect*art.Aspect() + 0.5)
	if limit := cols - 2; s.w > limit {
		s.w = max(limit, 1)
		s.h = max(int(float64(s.w)/cellAspect/art.Aspect()+0.5), 1)
	}
	s.x0, s.y0 = (cols-s.w)/2, max((rows-1-s.h)/2, 0)
	s.base = make([]Cell, s.w*s.h)
	for y := 0; y < s.h; y++ {
		for x := 0; x < s.w; x++ {
			s.base[y*s.w+x] = art.cell(float64(x)/float64(s.w), float64(y)/float64(s.h),
				float64(x+1)/float64(s.w), float64(y+1)/float64(s.h), 1)
		}
	}
	return s
}

// logo is the still logo's cell (x, y), counted from its own corner.
func (s *Scene) logo(x, y int) Cell {
	if x < 0 || y < 0 || x >= s.w || y >= s.h {
		return Cell{}
	}
	return s.base[y*s.w+x]
}

func (c Cell) gain(k float64) Cell {
	if c.Ch == 0 {
		return c
	}
	return Cell{Ch: c.Ch, R: channel(float64(c.R) * k), G: channel(float64(c.G) * k), B: channel(float64(c.B) * k)}
}

func (c Cell) tint(col rgb) Cell {
	if c.Ch == 0 {
		return c
	}
	return col.cell(c.Ch)
}

// hash mixes three numbers into one that looks random.
func hash(a, b, c uint32) uint32 {
	h := a*0x9E3779B1 ^ b*0x85EBCA77 ^ c*0xC2B2AE3D
	h ^= h >> 15
	h *= 0x2C1B3C6D
	h ^= h >> 12
	h *= 0x297A2D39
	h ^= h >> 15
	return h
}

// frand is hash as a number in [0, 1).
func frand(a, b, c uint32) float64 { return float64(hash(a, b, c)>>8) / (1 << 24) }

func clamp01(v float64) float64 { return min(max(v, 0), 1) }

// smooth eases v from 0 to 1 with no speed at either end.
func smooth(v float64) float64 { v = clamp01(v); return v * v * (3 - 2*v) }

func frac(v float64) float64 { return v - math.Floor(v) }

const noise = "01<>/\\|=+*:;-_~"

func noiseAt(a, b, c uint32) byte { return noise[hash(a, b, c)%uint32(len(noise))] }

// Anim is one animation. Draw fills an empty canvas with the frame for t
// seconds after the start. Every animation goes on for as long as it is
// drawn.
type Anim struct {
	Name string
	Draw func(c *Canvas, s *Scene, t float64)
}

// Anims lists the animations there are.
var Anims = []Anim{
	{"spin", drawSpin},
	{"rain", drawRain},
	{"glitch", drawGlitch},
	{"print", drawPrint},
	{"gather", drawGather},
	{"static", drawStatic},
	{"ripple", drawRipple},
}

// Pick returns the animation called name, or the one n selects when there is
// none of that name.
func Pick(name string, n uint32) Anim {
	for _, a := range Anims {
		if a.Name == name {
			return a
		}
	}
	return Anims[n%uint32(len(Anims))]
}

// Frame draws one frame of a into c: the animation, and hint on the bottom
// row.
func Frame(c *Canvas, s *Scene, a Anim, t float64, hint string) {
	c.clear()
	a.Draw(c, s, t)
	if hint != "" && len(hint) <= c.Cols {
		for x := 0; x < c.Cols; x++ {
			c.set(x, c.Rows-1, Cell{})
		}
		c.text(c.Rows-1, hint, olive.scale(1.25))
	}
}

// drawSpin turns the logo about its upright axis like a coin: it rests on
// its face, turns once, and rests again.
func drawSpin(c *Canvas, s *Scene, t float64) {
	const period = 6.0
	angle := 2 * math.Pi * smooth((frac(t/period)-0.25)/0.75)
	turn := math.Cos(angle)
	if math.Abs(turn) < 0.05 {
		// Edge on: a line as tall as the figure.
		for y := 0; y < s.h; y++ {
			if s.art.sample(0, float64(y)/float64(s.h), 1, float64(y+1)/float64(s.h)).a > 0.04 {
				c.set(s.x0+s.w/2, s.y0+y, lime.scale(0.7).cell('|'))
			}
		}
		return
	}
	if turn > 0.9995 {
		// At rest: the still logo, which is already drawn.
		for y := 0; y < s.h; y++ {
			for x := 0; x < s.w; x++ {
				c.set(s.x0+x, s.y0+y, s.logo(x, y))
			}
		}
		return
	}
	// The back is the same picture seen from behind, and darker.
	gain := 0.6 + 0.4*math.Abs(turn)
	if turn < 0 {
		gain *= 0.6
	}
	for y := 0; y < s.h; y++ {
		v0, v1 := float64(y)/float64(s.h), float64(y+1)/float64(s.h)
		for x := 0; x < s.w; x++ {
			u0 := (float64(x)/float64(s.w)-0.5)/turn + 0.5
			u1 := (float64(x+1)/float64(s.w)-0.5)/turn + 0.5
			if u0 > u1 {
				u0, u1 = u1, u0
			}
			c.set(s.x0+x, s.y0+y, s.art.cell(u0, v0, u1, v1, gain))
		}
	}
}

// drawRain lets characters fall down every column of the screen. The first
// ones to cross the logo uncover it; the later ones light it up as they pass.
func drawRain(c *Canvas, s *Scene, t float64) {
	tick := uint32(t * 10)
	for x := 0; x < c.Cols; x++ {
		col := uint32(x)
		speed := 14 + 18*frand(col, 1, s.seed)
		trail := 5 + int(13*frand(col, 2, s.seed))
		cycle := float64(c.Rows+trail) + 4 + float64(c.Rows)*frand(col, 3, s.seed)
		// Every column has started within the first second and a half.
		fallen := t*speed - 1.5*speed*frand(col, 4, s.seed)
		head := math.Mod(fallen, cycle)
		for y := 0; y < c.Rows-1; y++ {
			behind := head - float64(y)
			lit := 0.0
			if fallen >= float64(y) && behind >= 0 && behind < float64(trail) {
				lit = 1 - behind/float64(trail)
			}
			if cell := s.logo(x-s.x0, y-s.y0); cell.Ch != 0 {
				switch {
				case fallen < float64(y):
				case lit > 0 && behind < 1:
					c.set(x, y, bone.scale(1.05).cell(noiseAt(col, uint32(y), tick)))
				default:
					c.set(x, y, cell.gain(0.82+0.38*lit))
				}
				continue
			}
			if lit > 0 {
				colour := olive.scale(0.6 + 1.3*lit)
				if behind < 1 {
					colour = lime
				}
				c.set(x, y, colour.cell(noiseAt(col, uint32(y), tick)))
			}
		}
	}
}

// drawGlitch shows the logo as a bad signal: now and then bands of it jump
// sideways and lose their colour, while a scan line runs down the screen.
func drawGlitch(c *Canvas, s *Scene, t float64) {
	tick := uint32(t * 14)
	// The picture arrives in a burst; later bursts come at random.
	burst := t < 0.7 || frand(uint32(t*1.7), 7, s.seed) < 0.36
	bandHeight := 2 + int(hash(tick, 5, s.seed)%4)
	offset := int(hash(tick, 3, s.seed) % 7)
	reach := float64(max(s.w/5, 3))
	for y := 0; y < s.h; y++ {
		shift, pale := 0, false
		if burst {
			band := uint32((y + offset) / bandHeight)
			if r := frand(band, tick, s.seed); r < 0.30 {
				shift = int((frand(band, tick+1, s.seed)*2 - 1) * reach)
				pale = r < 0.09
			}
		}
		for x := -int(reach); x < s.w+int(reach); x++ {
			cell := s.logo(x-shift, y)
			if cell.Ch == 0 {
				continue
			}
			if pale {
				cell = cell.tint(bone)
				if hash(uint32(x), uint32(y), tick)%4 == 0 {
					cell.Ch = noiseAt(uint32(x), uint32(y), tick)
				}
			}
			c.set(s.x0+x, s.y0+y, cell)
		}
	}
	if burst {
		// Specks over the whole screen.
		for i := 0; i < c.Cols*c.Rows/90; i++ {
			x := int(hash(uint32(i), tick, s.seed) % uint32(c.Cols))
			y := int(hash(uint32(i), tick, s.seed+1) % uint32(max(c.Rows-1, 1)))
			if c.at(x, y).Ch == 0 {
				c.set(x, y, olive.scale(0.9).cell(noiseAt(uint32(i), tick, 9)))
			}
		}
	}
	scan := int(frac(t/3.4) * float64(c.Rows-1))
	for x := 0; x < c.Cols; x++ {
		if cell := c.at(x, scan); cell.Ch != 0 {
			c.set(x, scan, cell.tint(bone))
		} else if hash(uint32(x), tick, 11)%3 != 0 {
			c.set(x, scan, olive.scale(0.7).cell('-'))
		}
	}
}

// drawPrint covers the screen with the logo as a repeating print, every
// other one mirrored: its twin. The print drifts and its scale breathes
// between one logo and many.
func drawPrint(c *Canvas, s *Scene, t float64) {
	const period = 18.0
	w := 2 * math.Pi / period
	small := 0.5 - 0.5*math.Cos(w*t) // 0: one logo fills the screen, 1: many
	// At its smallest a logo is still half the screen tall: smaller, and
	// its face can no longer be read.
	scale := 1 - 0.5*smooth(small)
	th := float64(s.h) * scale
	tw := float64(s.w) * scale
	// The drift is the time spent small, so the print stands still whenever
	// one logo fills the screen.
	drift := 0.5*t - 0.5*math.Sin(w*t)/w
	const pitch = 1.16 // tiles are this far apart, in tile sizes
	cx, cy := float64(c.Cols)/2, float64(c.Rows-1)/2
	for y := 0; y < c.Rows-1; y++ {
		py := (float64(y)+0.5-cy)/th + drift*0.035
		j := math.Floor(py/pitch + 0.5)
		v := py - j*pitch + 0.5
		if v < 0 || v >= 1 {
			continue
		}
		odd := int64(j)&1 != 0
		for x := 0; x < c.Cols; x++ {
			px := (float64(x)+0.5-cx)/tw + drift*0.11
			if odd {
				px += pitch / 2
			}
			i := math.Floor(px/pitch + 0.5)
			u := px - i*pitch + 0.5
			if u < 0 || u >= 1 {
				continue
			}
			if (int64(i)+int64(j))&1 != 0 {
				u = 1 - u
			}
			du, dv := 0.5/tw, 0.5/th
			c.set(x, y, s.art.cell(u-du, v-dv, u+du, v+dv, 1))
		}
	}
}

// drawGather brings the logo's characters in from all over the screen,
// holds the logo, and scatters them again.
func drawGather(c *Canvas, s *Scene, t float64) {
	const period = 10.0
	p := frac(t/period) * period
	round := uint32(t / period)
	for y := 0; y < s.h; y++ {
		for x := 0; x < s.w; x++ {
			cell := s.logo(x, y)
			if cell.Ch == 0 {
				continue
			}
			id := uint32(y*s.w + x)
			delay := frand(id, 1, s.seed)
			// 0..3 s: come in. 3..7.4 s: hold. 7.4..9.6 s: leave.
			in := smooth((p - 0.9*delay) / 2.1)
			out := smooth((p - 7.4 - 0.9*delay) / 1.3)
			from := round
			k := 1 - in
			if out > 0 {
				from, k = round+1, out*out
			}
			if k >= 1 {
				continue
			}
			// Where this character is when it is not in the logo. It leaves
			// for the place it comes back from.
			fx := frand(id, 2, s.seed+from) * float64(c.Cols)
			fy := frand(id, 3, s.seed+from) * float64(c.Rows-1)
			tx, ty := float64(s.x0+x), float64(s.y0+y)
			c.set(int(tx+(fx-tx)*k+0.5), int(ty+(fy-ty)*k+0.5), cell.gain(1-0.55*k))
		}
	}
}

// drawStatic fills the screen with noise that settles into the logo and
// then loses it again.
func drawStatic(c *Canvas, s *Scene, t float64) {
	const period = 11.0
	p := frac(t/period) * period
	// 0..3 s: settle. 3..8.5 s: hold. 8.5..11 s: come apart.
	settled := smooth(p/3) - smooth((p-8.5)/2.5)
	tick := uint32(t * 11)
	for y := 0; y < c.Rows-1; y++ {
		for x := 0; x < c.Cols; x++ {
			ux, uy := uint32(x), uint32(y)
			if cell := s.logo(x-s.x0, y-s.y0); cell.Ch != 0 {
				if settled*1.05 > frand(ux, uy, s.seed) {
					c.set(x, y, cell)
				} else {
					c.set(x, y, cell.gain(0.75).tint(lime.scale(0.6)))
					c.Cells[y*c.Cols+x].Ch = noiseAt(ux, uy, tick)
				}
				continue
			}
			if frand(ux, uy, tick) < 0.30*(1-settled) {
				c.set(x, y, olive.scale(0.8).cell(noiseAt(ux, uy, tick+1)))
			}
		}
	}
}

// drawRipple sways the logo like a reflection on water, while rings run
// outward from it over the whole screen.
func drawRipple(c *Canvas, s *Scene, t float64) {
	sway := 2.6 * (0.5 - 0.5*math.Cos(2*math.Pi*t/9))
	length := math.Max(float64(s.h)/2.4, 3)
	cx, cy := float64(c.Cols)/2, float64(s.y0)+float64(s.h)/2
	ring := func(x, y int) float64 {
		d := math.Hypot((float64(x)-cx)/cellAspect, float64(y)-cy)
		return frac(d/9 - t/2.2)
	}
	for y := 0; y < s.h; y++ {
		shift := int(math.Round(sway * math.Sin(2*math.Pi*(float64(y)/length-t/2.4))))
		for x := -3; x < s.w+3; x++ {
			cell := s.logo(x-shift, y)
			if cell.Ch == 0 {
				continue
			}
			k := 0.88
			if ring(s.x0+x, s.y0+y) < 0.16 {
				k = 1.2
			}
			c.set(s.x0+x, s.y0+y, cell.gain(k))
		}
	}
	reach := math.Hypot(cx/cellAspect, cy)
	for y := 0; y < c.Rows-1; y++ {
		for x := 0; x < c.Cols; x++ {
			if c.at(x, y).Ch != 0 {
				continue
			}
			if ph := ring(x, y); ph < 0.10 {
				d := math.Hypot((float64(x)-cx)/cellAspect, float64(y)-cy)
				ch := byte('.')
				if ph < 0.04 {
					ch = ':'
				}
				c.set(x, y, olive.scale(1.1-0.7*d/reach).cell(ch))
			}
		}
	}
}
