// Package splash draws the geminus logo as moving text: the picture a
// terminal shows when geminus opens. Every frame is a function of the time
// alone, so what is drawn can be tested without a terminal.
package splash

import (
	"bytes"
	"errors"
	"image/png"
)

// Art is the logo, kept so that the average colour of any rectangle of it is
// read in constant time.
type Art struct {
	w, h int
	// sum holds, for each corner of the pixel grid, the total of the four
	// premultiplied channels above and to the left of it.
	sum []float64
	// The rectangle of pixels that hold the figure. The margin around it is
	// left out, so the figure fills the cells it is given.
	x0, y0, x1, y1 float64
}

// pix is an average colour: premultiplied, each channel in 0..1.
type pix struct{ r, g, b, a float64 }

// Load reads the logo from PNG data.
func Load(data []byte) (*Art, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil, errors.New("splash: the logo is empty")
	}
	a := &Art{w: w, h: h, sum: make([]float64, (w+1)*(h+1)*4)}
	minX, minY, maxX, maxY := w, h, -1, -1
	for y := 0; y < h; y++ {
		var row [4]float64
		for x := 0; x < w; x++ {
			r, g, bl, al := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			row[0] += float64(r) / 0xffff
			row[1] += float64(g) / 0xffff
			row[2] += float64(bl) / 0xffff
			row[3] += float64(al) / 0xffff
			here, above := ((y+1)*(w+1)+x+1)*4, (y*(w+1)+x+1)*4
			for c := range row {
				a.sum[here+c] = a.sum[above+c] + row[c]
			}
			if al > 0x0800 {
				minX, minY, maxX, maxY = min(minX, x), min(minY, y), max(maxX, x), max(maxY, y)
			}
		}
	}
	if maxX < 0 {
		return nil, errors.New("splash: the logo is transparent everywhere")
	}
	a.x0, a.y0, a.x1, a.y1 = float64(minX), float64(minY), float64(maxX+1), float64(maxY+1)
	return a, nil
}

// Aspect is the figure's width over its height.
func (a *Art) Aspect() float64 { return (a.x1 - a.x0) / (a.y1 - a.y0) }

// total is the sum of the pixels above and to the left of the point (x, y),
// which need not be on the grid.
func (a *Art) total(x, y float64) (t [4]float64) {
	x, y = min(max(x, 0), float64(a.w)), min(max(y, 0), float64(a.h))
	xi, yi := min(int(x), a.w-1), min(int(y), a.h-1)
	fx, fy := x-float64(xi), y-float64(yi)
	at := func(px, py int) int { return (py*(a.w+1) + px) * 4 }
	i00, i10, i01, i11 := at(xi, yi), at(xi+1, yi), at(xi, yi+1), at(xi+1, yi+1)
	for c := range t {
		top := a.sum[i00+c] + (a.sum[i10+c]-a.sum[i00+c])*fx
		bot := a.sum[i01+c] + (a.sum[i11+c]-a.sum[i01+c])*fx
		t[c] = top + (bot-top)*fy
	}
	return t
}

// sample is the average colour of a rectangle of the figure. The figure is
// the unit square; what lies outside it is transparent.
func (a *Art) sample(u0, v0, u1, v1 float64) pix {
	if u1 <= u0 || v1 <= v0 || u1 <= 0 || v1 <= 0 || u0 >= 1 || v0 >= 1 {
		return pix{}
	}
	sx, sy := a.x1-a.x0, a.y1-a.y0
	area := (u1 - u0) * sx * (v1 - v0) * sy
	// Only the part inside the figure is read: the picture's own margin is
	// transparent too, but another figure may one day touch its edge.
	px0, px1 := a.x0+max(u0, 0)*sx, a.x0+min(u1, 1)*sx
	py0, py1 := a.y0+max(v0, 0)*sy, a.y0+min(v1, 1)*sy
	p11, p01, p10, p00 := a.total(px1, py1), a.total(px0, py1), a.total(px1, py0), a.total(px0, py0)
	var c [4]float64
	for i := range c {
		c[i] = (p11[i] - p01[i] - p10[i] + p00[i]) / area
	}
	return pix{c[0], c[1], c[2], c[3]}
}
