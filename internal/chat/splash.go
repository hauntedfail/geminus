package chat

// The opening screen: the geminus logo, drawn in characters and moving, over
// the whole terminal. It is shown when geminus opens and goes on until a key
// is pressed. geminus starts behind it, so whatever was to be on the screen
// by then is there when it goes.

import (
	"io"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/assets/logo"
	"github.com/hauntedfail/geminus/internal/splash"
)

const (
	// splashEnv chooses the opening screen: "off" for none, the name of an
	// animation for that one. Anything else, or nothing, is one at random.
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

// newSplash returns the opening screen for this start: the animation n
// selects, or the one the environment names. It returns nil when there is to
// be none.
func newSplash(getenv func(string) string, color bool, now time.Time, n uint32) *splashScreen {
	want := strings.ToLower(strings.TrimSpace(getenv(splashEnv)))
	switch want {
	case "off", "0", "no", "false", "none":
		return nil
	}
	art, err := splash.Load(logo.PNG)
	if err != nil {
		return nil
	}
	return &splashScreen{art: art, anim: splash.Pick(want, n), mode: splash.DetectMode(getenv, color), seed: n, start: now}
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
