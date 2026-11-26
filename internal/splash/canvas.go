package splash

import (
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Cell is one character of the screen. Ch is 0 where nothing is drawn.
type Cell struct {
	Ch      byte
	R, G, B uint8
}

// Canvas is one frame: Cols by Rows cells, row after row.
type Canvas struct {
	Cols, Rows int
	Cells      []Cell
}

// NewCanvas returns an empty frame.
func NewCanvas(cols, rows int) *Canvas {
	return &Canvas{Cols: cols, Rows: rows, Cells: make([]Cell, cols*rows)}
}

func (c *Canvas) clear() { clear(c.Cells) }

func (c *Canvas) set(x, y int, cell Cell) {
	if x >= 0 && y >= 0 && x < c.Cols && y < c.Rows {
		c.Cells[y*c.Cols+x] = cell
	}
}

func (c *Canvas) at(x, y int) Cell {
	if x >= 0 && y >= 0 && x < c.Cols && y < c.Rows {
		return c.Cells[y*c.Cols+x]
	}
	return Cell{}
}

// text writes s on row y, centred.
func (c *Canvas) text(y int, s string, col rgb) {
	x := (c.Cols - len(s)) / 2
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			c.set(x+i, y, col.cell(s[i]))
		}
	}
}

type rgb struct{ r, g, b float64 }

// The logo's own three colours.
var (
	bone  = rgb{239, 228, 157}
	lime  = rgb{202, 212, 47}
	olive = rgb{93, 99, 12}
)

func (c rgb) scale(k float64) rgb { return rgb{c.r * k, c.g * k, c.b * k} }

func (c rgb) cell(ch byte) Cell {
	return Cell{Ch: ch, R: channel(c.r), G: channel(c.g), B: channel(c.b)}
}

func channel(v float64) uint8 { return uint8(min(max(v, 0), 255) + 0.5) }

// glyph is a character and how much of each part of its cell it fills, 0..1.
type glyph struct {
	ch   byte
	mean float32
	ink  [subW * subH]float32
}

// glyphs is every printable character, the emptiest first.
var glyphs = func() []glyph {
	// The fullest part of any character: no part of a cell is ever all ink.
	const full = 130
	out := make([]glyph, 0, len(glyphInk))
	for _, g := range glyphInk {
		v := glyph{ch: g.ch}
		for i, ink := range g.ink {
			v.ink[i] = float32(math.Min(float64(ink)/full, 1))
			v.mean += v.ink[i] / float32(len(g.ink))
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mean < out[j].mean })
	return out
}()

// tone spreads the logo's brightness out: it is mostly mid tones.
var tone = func() (t [256]float32) {
	for i := range t {
		t[i] = float32(math.Pow(math.Min(float64(i)/float64(len(t)-1)/0.78, 1), 0.9))
	}
	return t
}()

// cell turns a rectangle of the logo into one character: the one whose
// shape is closest to that part of the picture, in the colour of its light
// parts. So a line of the drawing becomes a stroke that runs the same way,
// and on a dark screen the picture reads the way it was drawn. gain scales
// the brightness.
func (a *Art) cell(u0, v0, u1, v1, gain float64) Cell {
	if gain <= 0 || a.sample(u0, v0, u1, v1).a < 0.16 {
		return Cell{}
	}
	// The totals at the corners of the cell's parts, read once each.
	var grid [(subW + 1) * (subH + 1)][4]float64
	sx, sy := a.x1-a.x0, a.y1-a.y0
	for j := 0; j <= subH; j++ {
		py := a.y0 + clamp01(v0+(v1-v0)*float64(j)/subH)*sy
		for i := 0; i <= subW; i++ {
			px := a.x0 + clamp01(u0+(u1-u0)*float64(i)/subW)*sx
			grid[j*(subW+1)+i] = a.total(px, py)
		}
	}
	// A part that is half outside the figure is half transparent.
	perArea := subW * subH / ((u1 - u0) * sx * (v1 - v0) * sy)
	var (
		want       [subW * subH]float32
		mean       float32
		r, g, b, w float64
	)
	for j := 0; j < subH; j++ {
		for i := 0; i < subW; i++ {
			p00, p10 := &grid[j*(subW+1)+i], &grid[j*(subW+1)+i+1]
			p01, p11 := &grid[(j+1)*(subW+1)+i], &grid[(j+1)*(subW+1)+i+1]
			pr := (p11[0] - p01[0] - p10[0] + p00[0]) * perArea
			pg := (p11[1] - p01[1] - p10[1] + p00[1]) * perArea
			pb := (p11[2] - p01[2] - p10[2] + p00[2]) * perArea
			pa := (p11[3] - p01[3] - p10[3] + p00[3]) * perArea
			// Premultiplied, so a part that is half outside is half as light.
			lum := 0.30*pr + 0.59*pg + 0.11*pb
			l := tone[int(clamp01(lum)*float64(len(tone)-1)+0.5)]
			want[j*subW+i] = l
			mean += l / float32(len(want))
			r, g, b, w = r+pr*lum, g+pg*lum, b+pb*lum, w+pa*lum
		}
	}
	// What tells one part of a cell from another matters more than how
	// light the cell is: that is where the lines are.
	const contrast = 1.7
	for i, l := range want {
		want[i] = min(max(mean+(l-mean)*contrast, 0), 1) * 0.8
	}
	// The fullest characters fill well under half their cell: a cell that is
	// light all over is to become one of those.
	fullest := glyphs[len(glyphs)-1].mean
	best, least := byte(0), float32(math.MaxFloat32)
	for pass := 0; pass < 2 && best == 0; pass++ {
		for k := range glyphs {
			gl := &glyphs[k]
			// A character much emptier or fuller than the cell cannot be
			// it. The second pass, for a cell no character is close to,
			// takes any.
			if d := gl.mean - mean*fullest; pass == 0 && (d < -0.10 || d > 0.10) {
				continue
			}
			var miss float32
			for i := 0; i < len(want) && miss < least; i += subW {
				for j := i; j < i+subW; j++ {
					d := want[j] - gl.ink[j]
					miss += d * d
				}
			}
			if miss < least {
				best, least = gl.ch, miss
			}
		}
	}
	if w <= 0 {
		return olive.scale(gain).cell(best)
	}
	r, g, b = r/w, g/w, b/w
	// Keep the hue, lift the darkest cells so their characters can be seen.
	m := max(r, g, b, 1e-6)
	lift := (0.40 + 0.60*m) / m * gain * 255
	return rgb{r * lift, g * lift, b * lift}.cell(best)
}

// ColorMode is how colours are written.
type ColorMode int

const (
	// Mono writes characters only.
	Mono ColorMode = iota
	// Color256 writes the nearest of the 256 indexed colours.
	Color256
	// TrueColor writes 24-bit colours.
	TrueColor
)

// DetectMode picks the richest colour mode the terminal is known to show.
func DetectMode(getenv func(string) string, color bool) ColorMode {
	if !color {
		return Mono
	}
	if ct := strings.ToLower(getenv("COLORTERM")); strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit") {
		return TrueColor
	}
	if getenv("WT_SESSION") != "" || runtime.GOOS == "windows" {
		return TrueColor
	}
	switch getenv("TERM_PROGRAM") {
	case "iTerm.app", "WezTerm", "ghostty", "vscode", "Hyper":
		return TrueColor
	}
	term := strings.ToLower(getenv("TERM"))
	for _, name := range []string{"direct", "kitty", "ghostty", "alacritty", "foot", "wezterm"} {
		if strings.Contains(term, name) {
			return TrueColor
		}
	}
	return Color256
}

func (m ColorMode) key(c Cell) int {
	switch m {
	case TrueColor:
		// Neighbouring cells differ by a little in every channel: 16 steps
		// a channel, so a run of them shares one colour sequence.
		return int(c.R>>4)<<8 | int(c.G>>4)<<4 | int(c.B>>4)
	case Color256:
		level := func(v uint8) int { return (int(v)*5 + 127) / 255 }
		return 16 + 36*level(c.R) + 6*level(c.G) + level(c.B)
	}
	return 0
}

func (m ColorMode) write(b *strings.Builder, key int) {
	switch m {
	case TrueColor:
		// The middle of each step, so the brightest is not dimmed much.
		b.WriteString("\x1b[38;2;")
		b.WriteString(strconv.Itoa(key>>8<<4 | 8))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(key>>4&15<<4 | 8))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(key&15<<4 | 8))
		b.WriteByte('m')
	case Color256:
		b.WriteString("\x1b[38;5;")
		b.WriteString(strconv.Itoa(key))
		b.WriteByte('m')
	}
}

// Encode writes what differs between prev and cur as terminal output: in
// each row, the cells from the first that changed to the last. prev may be
// nil, or of another size; then every row is written whole and cleared to
// its end.
func Encode(b *strings.Builder, cur, prev *Canvas, mode ColorMode) {
	same := prev != nil && prev.Cols == cur.Cols && prev.Rows == cur.Rows
	for y := 0; y < cur.Rows; y++ {
		row := cur.Cells[y*cur.Cols : (y+1)*cur.Cols]
		from, to := 0, len(row)
		if same {
			old := prev.Cells[y*cur.Cols : (y+1)*cur.Cols]
			for from < to && mode.same(row[from], old[from]) {
				from++
			}
			for to > from && mode.same(row[to-1], old[to-1]) {
				to--
			}
			if from == to {
				continue
			}
		} else {
			for to > 0 && row[to-1].Ch == 0 {
				to--
			}
		}
		b.WriteString("\x1b[")
		b.WriteString(strconv.Itoa(y + 1))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(from + 1))
		b.WriteByte('H')
		last := -1
		for _, cell := range row[from:to] {
			if cell.Ch == 0 {
				b.WriteByte(' ')
				continue
			}
			if mode != Mono {
				if key := mode.key(cell); key != last {
					mode.write(b, key)
					last = key
				}
			}
			b.WriteByte(cell.Ch)
		}
		if last >= 0 {
			b.WriteString("\x1b[0m")
		}
		// After a character in the last column the cursor has nowhere to
		// go, and clearing from it would take that character away.
		if !same && to < len(row) {
			b.WriteString("\x1b[K")
		}
	}
}

// same reports whether two cells look alike when written in this mode.
func (m ColorMode) same(a, b Cell) bool {
	return a.Ch == b.Ch && (a.Ch == 0 || m.key(a) == m.key(b))
}

func equal(a, b []Cell) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
