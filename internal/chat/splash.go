package chat

// How geminus opens. The mascot, the logo as a small figure of block
// characters, stands beside the name at the top of what is said, and moves
// now and then. It is part of the transcript: it scrolls away with it.
//
// The opening screen is the logo drawn in characters and moving, over the
// whole terminal, until a key is pressed. It is shown only when asked for.
// geminus starts behind it, so whatever was to be on the screen by then is
// there when it goes.

import (
	"io"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/assets/logo"
	"github.com/hauntedfail/geminus/internal/splash"
)

const (
	// splashEnv chooses how geminus opens: "off" for neither mascot nor
	// opening screen, "full" for the opening screen with an animation at
	// random, the name of one of its animations for that one. Anything
	// else, or nothing, is the mascot alone.
	splashEnv = "GEMINUS_SPLASH"
	// splashEvery is how often the animation is drawn.
	splashEvery = 70 * time.Millisecond
	splashHint  = "press any key"
	clearScreen = "\x1b[0m\x1b[H\x1b[2J"
)

type splashScreen struct {
	art   *splash.Art
	anim  splash.Anim
	mode  splash.ColorMode
	seed  uint32
	start time.Time
	// scene is the logo fitted to the terminal's size; shown is the frame
	// on the screen and next the one being drawn. shown is nil until a
	// frame has been written at this size.
	scene       *splash.Scene
	shown, next *splash.Canvas
}

// opening is how this start opens: the opening screen and the mascot, either
// of which may be nil. n is what chance chose.
func opening(getenv func(string) string, color bool, now time.Time, n uint32) (*splashScreen, *banner) {
	want := strings.ToLower(strings.TrimSpace(getenv(splashEnv)))
	switch want {
	case "off", "0", "no", "false", "none":
		return nil, nil
	}
	mode := splash.DetectMode(getenv, color)
	mascot := &banner{mascot: splash.NewMascot("", n), mode: mode, start: now}
	full := want == "full"
	for _, a := range splash.Anims {
		full = full || a.Name == want
	}
	if !full {
		return nil, mascot
	}
	art, err := splash.Load(logo.PNG)
	if err != nil {
		return nil, mascot
	}
	return &splashScreen{art: art, anim: splash.Pick(want, n), mode: mode, seed: n, start: now}, mascot
}

// banner is the mascot with the lines that stand beside it.
type banner struct {
	mascot splash.Mascot
	mode   splash.ColorMode
	start  time.Time
	// lines stand to the right of the mascot, from its second row down.
	lines []string
}

// bannerRows is how many rows of the transcript the banner takes: the
// mascot and an empty row under it.
const bannerRows = splash.MascotRows + 1

// rows draws the banner as it is now, no wider than width.
func (b *banner) rows(now time.Time, width int) []string {
	out := make([]string, 0, bannerRows)
	for i, row := range b.mascot.Rows(now.Sub(b.start).Seconds(), b.mode) {
		row = " " + row
		if k := i - 1; k >= 0 && k < len(b.lines) {
			row += "  " + b.lines[k]
		}
		out = append(out, clipStyled(row, width))
	}
	return append(out, "")
}

// clipStyled cuts s to at most width columns. Colour sequences take no
// room, and a row that was cut does not leave its colours on.
func clipStyled(s string, width int) string {
	if textWidth(s) <= width {
		return s
	}
	var b strings.Builder
	used := 0
	for i := 0; i < len(s); {
		if n := skipStyle(s[i:]); n > 0 {
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		r, size := decodeRune(s[i:])
		if used += runeWidth(r); used > width {
			break
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String() + "\x1b[0m"
}

// drawSplash writes the animation's frame for now: the rows that changed.
func (l *live) drawSplash() {
	sp := l.splash
	var out strings.Builder
	out.WriteString(syncStart)
	if sp.scene == nil || sp.scene.Cols != l.cols || sp.scene.Rows != l.rows {
		sp.scene = splash.NewScene(sp.art, l.cols, l.rows, sp.seed)
		sp.shown, sp.next = nil, splash.NewCanvas(l.cols, l.rows)
		out.WriteString(cursorHide + clearScreen)
	}
	splash.Frame(sp.next, sp.scene, sp.anim, l.now().Sub(sp.start).Seconds(), splashHint)
	before := out.Len()
	splash.Encode(&out, sp.next, sp.shown, sp.mode)
	if sp.shown != nil && out.Len() == before {
		return
	}
	out.WriteString(syncEnd)
	io.WriteString(l.out, out.String())
	if sp.shown == nil {
		sp.shown = splash.NewCanvas(l.cols, l.rows)
	}
	sp.shown, sp.next = sp.next, sp.shown
}

// endSplash takes the opening screen away. What geminus shows is drawn by
// the next draw.
func (l *live) endSplash() {
	l.splash, l.last = nil, ""
	io.WriteString(l.out, clearScreen)
}

// splashTick draws the next frame of the opening screen. It reports whether
// the screen is still shown.
func (l *live) splashTick() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.splash == nil {
		return false
	}
	l.draw()
	return true
}

// splashKey says what a key does to the opening screen. Any key takes it
// away. What was typed is for the box, and so is being asked to leave: those
// are passed on. A key that only meant "go on" is used up.
func (l *live) splashKey(k key) (passOn bool) {
	l.endSplash()
	switch k.Kind {
	case keyRune, keyAltRune, keyPaste, keyPasteStart, keyInterrupt, keyEOF:
		return true
	}
	l.draw()
	return false
}
