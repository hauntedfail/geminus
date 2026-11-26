package splash

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/assets/logo"
)

func art(t testing.TB) *Art {
	t.Helper()
	a, err := Load(logo.PNG)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func filled(c *Canvas) int {
	n := 0
	for _, cell := range c.Cells {
		if cell.Ch != 0 {
			n++
		}
	}
	return n
}

func TestTheLogoIsReadWithoutItsMargin(t *testing.T) {
	a := art(t)
	if got := a.Aspect(); got < 0.8 || got > 1.0 {
		t.Errorf("aspect = %.3f, want the figure a little taller than wide", got)
	}
	if p := a.sample(0.3, 0.45, 0.4, 0.55); p.a < 0.99 {
		t.Errorf("the cheek is not opaque: %+v", p)
	}
	if p := a.sample(0, 0, 0.03, 0.03); p.a > 0.01 {
		t.Errorf("the corner beside the horn is not empty: %+v", p)
	}
	if p := a.sample(-0.5, 0.4, -0.1, 0.6); p.a != 0 {
		t.Errorf("outside the figure is not empty: %+v", p)
	}
	// Each side of the figure reaches its edge: nothing of the margin is left.
	for name, p := range map[string]pix{
		"left": a.sample(0, 0, 0.02, 1), "right": a.sample(0.98, 0, 1, 1),
		"top": a.sample(0, 0, 1, 0.02), "bottom": a.sample(0, 0.98, 1, 1),
	} {
		if p.a <= 0 {
			t.Errorf("the %s edge is empty", name)
		}
	}
	if _, err := Load([]byte("not a picture")); err == nil {
		t.Error("Load took what is not a PNG")
	}
}

func TestTheSceneFitsTheScreen(t *testing.T) {
	a := art(t)
	for _, size := range [][2]int{{30, 8}, {80, 24}, {120, 40}, {40, 60}, {240, 70}} {
		s := NewScene(a, size[0], size[1], 1)
		if s.x0 < 0 || s.y0 < 0 || s.x0+s.w > size[0] || s.y0+s.h > size[1]-1 {
			t.Errorf("%dx%d: the logo is at %d,%d and %dx%d: over the edge or on the bottom row",
				size[0], size[1], s.x0, s.y0, s.w, s.h)
		}
		// As wide as it is tall, on cells twice as tall as wide.
		if ratio := float64(s.w) / cellAspect / float64(s.h) / a.Aspect(); ratio < 0.85 || ratio > 1.18 {
			t.Errorf("%dx%d: the logo is %dx%d cells: stretched by %.2f", size[0], size[1], s.w, s.h, ratio)
		}
		// The figure is round: the middle of the logo is drawn, its corners are not.
		if s.logo(s.w/3, s.h/2).Ch == 0 {
			t.Errorf("%dx%d: nothing where the face is", size[0], size[1])
		}
		if s.logo(0, s.h-1).Ch != 0 || s.logo(s.w-1, s.h-1).Ch != 0 {
			t.Errorf("%dx%d: the lower corners are drawn", size[0], size[1])
		}
	}
}

func TestEveryAnimationDrawsTheLogoAndGoesOn(t *testing.T) {
	a := art(t)
	for _, size := range [][2]int{{30, 8}, {80, 24}, {161, 47}} {
		s := NewScene(a, size[0], size[1], 42)
		whole := 0
		for _, cell := range s.base {
			if cell.Ch != 0 {
				whole++
			}
		}
		for _, anim := range Anims {
			c := NewCanvas(size[0], size[1])
			most, frames := 0, map[string]bool{}
			// Three minutes of it: no animation ends. The step is one no
			// period is a multiple of, so the same phases are not drawn
			// over and over.
			for i := 0; i < 250; i++ {
				at := float64(i) * 0.73
				Frame(c, s, anim, at, "press any key")
				for j, cell := range c.Cells {
					if cell.Ch != 0 && (cell.Ch < 0x21 || cell.Ch > 0x7e) {
						t.Fatalf("%s %dx%d at %.1fs: cell %d holds %q", anim.Name, size[0], size[1], at, j, cell.Ch)
					}
				}
				n := filled(c)
				most = max(most, n)
				if at > 120 {
					frames[string(cellBytes(c))] = true
				}
				if i%25 == 0 {
					again := NewCanvas(size[0], size[1])
					Frame(again, s, anim, at, "press any key")
					if !equal(c.Cells, again.Cells) {
						t.Fatalf("%s at %.1fs: the same moment drawn twice differs", anim.Name, at)
					}
				}
			}
			if most < whole*3/4 {
				t.Errorf("%s %dx%d: at most %d cells drawn; the logo alone has %d", anim.Name, size[0], size[1], most, whole)
			}
			if len(frames) < 20 {
				t.Errorf("%s %dx%d: only %d different frames after two minutes: it stopped", anim.Name, size[0], size[1], len(frames))
			}
		}
	}
}

func cellBytes(c *Canvas) []byte {
	out := make([]byte, 0, len(c.Cells))
	for _, cell := range c.Cells {
		out = append(out, cell.Ch)
	}
	return out
}

func TestTheHintIsOnTheBottomRow(t *testing.T) {
	s := NewScene(art(t), 80, 24, 3)
	c := NewCanvas(80, 24)
	for _, anim := range Anims {
		Frame(c, s, anim, 5, "press any key")
		row := strings.TrimSpace(strings.ReplaceAll(string(cellBytes(c)[23*80:]), "\x00", " "))
		if row != "press any key" {
			t.Errorf("%s: bottom row = %q", anim.Name, row)
		}
	}
}

func TestPick(t *testing.T) {
	seen := map[string]bool{}
	for n := uint32(0); n < 100; n++ {
		seen[Pick("", n).Name] = true
		if got := Pick("no such animation", n).Name; got != Pick("", n).Name {
			t.Errorf("an unknown name chose %q, not the random one", got)
		}
	}
	if len(seen) != len(Anims) || len(Anims) < 2 {
		t.Errorf("chance reaches %d of %d animations", len(seen), len(Anims))
	}
	for _, a := range Anims {
		if got := Pick(a.Name, 3).Name; got != a.Name {
			t.Errorf("Pick(%q) = %q", a.Name, got)
		}
	}
}

var colourSeq = regexp.MustCompile(`\x1b\[(?:0|38;5;\d+|38;2;\d+;\d+;\d+)m`)

// rowsOf splits what Encode wrote into rows, by where each was put.
func rowsOf(t *testing.T, out string) map[int]string {
	t.Helper()
	rows := map[int]string{}
	place := regexp.MustCompile(`\x1b\[(\d+);\d+H`)
	parts := place.Split(out, -1)
	at := place.FindAllStringSubmatch(out, -1)
	if parts[0] != "" {
		t.Fatalf("output before the first row: %q", parts[0])
	}
	for i, m := range at {
		n := 0
		for _, d := range m[1] {
			n = n*10 + int(d-'0')
		}
		rows[n] = parts[i+1]
	}
	return rows
}

func TestEncodeWritesRowsThatFitAndOnlyThoseThatChanged(t *testing.T) {
	s := NewScene(art(t), 61, 19, 5)
	prev, cur := NewCanvas(61, 19), NewCanvas(61, 19)
	for _, mode := range []ColorMode{Mono, Color256, TrueColor} {
		Frame(cur, s, Pick("print", 0), 4.5, "press any key")
		var b strings.Builder
		Encode(&b, cur, nil, mode)
		rows := rowsOf(t, b.String())
		if len(rows) != 19 {
			t.Fatalf("mode %d: %d rows written, want all 19", mode, len(rows))
		}
		for n, row := range rows {
			text := colourSeq.ReplaceAllString(row, "")
			full := !strings.HasSuffix(text, "\x1b[K")
			text = strings.TrimSuffix(text, "\x1b[K")
			if strings.ContainsAny(text, "\x1b\r\n") {
				t.Errorf("mode %d row %d: left over after the colours are taken out: %q", mode, n, text)
			}
			if len(text) > 61 || full && len(text) != 61 {
				t.Errorf("mode %d row %d: %d characters on a screen 61 wide (cleared to the end: %v)", mode, n, len(text), !full)
			}
			if mode == Mono && row != text && row != text+"\x1b[K" {
				t.Errorf("mono row %d has colour: %q", n, row)
			}
			if mode != Mono && strings.TrimSpace(text) != "" && !strings.Contains(row, "\x1b[0m") {
				t.Errorf("mode %d row %d: the colour is left on", mode, n)
			}
		}
		// The same frame again: nothing to write.
		copy(prev.Cells, cur.Cells)
		b.Reset()
		Encode(&b, cur, prev, mode)
		if b.Len() != 0 {
			t.Errorf("mode %d: %d bytes for a frame that is already shown", mode, b.Len())
		}
		// One cell changed: that cell alone, at its place.
		cur.set(30, 7, bone.cell('X'))
		Encode(&b, cur, prev, mode)
		if rows := rowsOf(t, b.String()); len(rows) != 1 || colourSeq.ReplaceAllString(rows[8], "") != "X" ||
			!strings.HasPrefix(b.String(), "\x1b[8;31H") {
			t.Errorf("mode %d: after one change %q was written", mode, b.String())
		}
		// Two cells of a row changed: from the first to the last, no further.
		copy(prev.Cells, cur.Cells)
		cur.set(10, 3, bone.cell('<'))
		cur.set(19, 3, bone.cell('>'))
		b.Reset()
		Encode(&b, cur, prev, mode)
		if text := colourSeq.ReplaceAllString(b.String(), ""); !strings.HasPrefix(text, "\x1b[4;11H<") ||
			!strings.HasSuffix(text, ">") || len(text) != len("\x1b[4;11H")+10 {
			t.Errorf("mode %d: after two changes in a row %q was written", mode, b.String())
		}
		// Another size: everything.
		b.Reset()
		Encode(&b, cur, NewCanvas(60, 19), mode)
		if rows := rowsOf(t, b.String()); len(rows) != 19 {
			t.Errorf("mode %d: after a change of size %d rows were written", mode, len(rows))
		}
	}
}

func TestColoursAreTheLogos(t *testing.T) {
	// Lime as 24 bits and as one of the 256.
	cell := lime.cell('#')
	var b strings.Builder
	TrueColor.write(&b, TrueColor.key(cell))
	if got := b.String(); got != "\x1b[38;2;200;216;40m" {
		t.Errorf("lime in true colour = %q", got)
	}
	b.Reset()
	Color256.write(&b, Color256.key(cell))
	if got := b.String(); got != "\x1b[38;5;185m" {
		t.Errorf("lime in 256 colours = %q", got)
	}
}

func TestDetectMode(t *testing.T) {
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	if got := DetectMode(env("COLORTERM", "truecolor"), false); got != Mono {
		t.Errorf("without colour = %d, want Mono", got)
	}
	if got := DetectMode(env("COLORTERM", "truecolor"), true); got != TrueColor {
		t.Errorf("COLORTERM=truecolor = %d", got)
	}
	if got := DetectMode(env("TERM", "xterm-kitty"), true); got != TrueColor {
		t.Errorf("TERM=xterm-kitty = %d", got)
	}
}

func BenchmarkFrame(b *testing.B) {
	s := NewScene(art(b), 200, 60, 1)
	c := NewCanvas(200, 60)
	for _, anim := range Anims {
		b.Run(anim.Name, func(b *testing.B) {
			var out strings.Builder
			for i := 0; i < b.N; i++ {
				Frame(c, s, anim, float64(i)*0.07, "press any key")
				out.Reset()
				Encode(&out, c, nil, TrueColor)
			}
			b.ReportMetric(float64(out.Len()), "bytes/frame")
		})
	}
}
