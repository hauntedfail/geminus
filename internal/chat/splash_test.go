package chat

import (
	"strings"
	"testing"
	"time"
)

func env(kv ...string) func(string) string {
	return func(k string) string {
		for i := 0; i < len(kv); i += 2 {
			if kv[i] == k {
				return kv[i+1]
			}
		}
		return ""
	}
}

// newSplashLive is a terminal that opens with the animation called name.
func newSplashLive(t *testing.T, cols, rows int, name string) (*vt, *live, *time.Time) {
	t.Helper()
	term := newVT(cols, rows)
	term.Write([]byte("prior shell output\r\n$ geminus\r\n"))
	clock := noon
	sp := newSplash(env(splashEnv, name), false, clock, 0)
	if sp == nil {
		t.Fatal("no opening screen")
	}
	lv := newLiveBehind(term, cols, rows, palette{}, func() time.Time { return clock }, sp)
	lv.set(sibylView())
	return term, lv, &clock
}

// drawn counts the characters on the screen that are not blank.
func drawn(term *vt) int {
	n := 0
	for _, r := range term.text() {
		if r != ' ' && r != '\n' {
			n++
		}
	}
	return n
}

func TestTheOpeningScreenCanBeChosenOrSwitchedOff(t *testing.T) {
	for _, off := range []string{"off", "OFF", " 0 ", "no", "false", "none"} {
		if newSplash(env(splashEnv, off), true, noon, 1) != nil {
			t.Errorf("%s=%q still opens with the animation", splashEnv, off)
		}
	}
	if got := newSplash(env(splashEnv, "Rain"), true, noon, 0).anim.Name; got != "rain" {
		t.Errorf("%s=Rain chose %q", splashEnv, got)
	}
	// Left to chance, another start can show another animation.
	seen := map[string]bool{}
	for n := uint32(0); n < 64; n++ {
		seen[newSplash(env(), true, noon, n).anim.Name] = true
	}
	if len(seen) < 5 {
		t.Errorf("64 starts showed only %d animations: %v", len(seen), seen)
	}
}

func TestTheOpeningScreenCoversEverythingAndMoves(t *testing.T) {
	for _, name := range []string{"spin", "rain", "glitch", "print", "gather", "static", "ripple"} {
		term, lv, clock := newSplashLive(t, 72, 24, name)
		lv.Write([]byte("said behind the opening screen\n"))
		frames := map[string]bool{}
		most := 0
		for i := 0; i < 120; i++ {
			*clock = clock.Add(splashEvery)
			if !lv.splashTick() {
				t.Fatalf("%s: the opening screen went away by itself after %d frames", name, i)
			}
			got := term.text()
			frames[got] = true
			most = max(most, drawn(term))
			if strings.Contains(got, "prior shell output") || strings.Contains(got, "said behind") || strings.Contains(got, "╭") {
				t.Fatalf("%s: something shows through the opening screen:\n%s", name, got)
			}
			lines := strings.Split(got, "\n")
			if len(lines) > 24 {
				t.Fatalf("%s: %d rows on a screen of 24: it scrolled", name, len(lines))
			}
			for _, line := range lines {
				if len([]rune(line)) > 72 {
					t.Fatalf("%s: a row of %d characters on a screen 72 wide", name, len([]rune(line)))
				}
			}
			if len(lines) == 24 && strings.TrimSpace(lines[23]) != splashHint {
				t.Fatalf("%s: bottom row = %q", name, lines[23])
			}
		}
		if len(frames) < 20 {
			t.Errorf("%s: 120 ticks drew %d different frames", name, len(frames))
		}
		// The logo alone is some 700 characters at this size.
		if most < 500 {
			t.Errorf("%s: at most %d characters on the screen", name, most)
		}
		lv.close()
		if got := term.text(); got != "prior shell output\n$ geminus" {
			t.Errorf("%s: after closing under the opening screen:\n%q", name, got)
		}
		if raw := term.raw.String(); !strings.HasSuffix(raw, cursorShow+altScreenOff+pasteModeOf) {
			t.Errorf("%s: the terminal was not put back", name)
		}
	}
}

func TestAnyKeyTakesTheOpeningScreenAway(t *testing.T) {
	// Enter only means "go on": nothing is entered.
	term, lv, _ := newSplashLive(t, 72, 24, "spin")
	lv.Write([]byte("said behind the opening screen\n"))
	if lines, quit := typeKeys(t, lv, "\r"); len(lines) != 0 || quit {
		t.Errorf("Enter on the opening screen entered %q, quit %v", lines, quit)
	}
	got := term.text()
	if !strings.Contains(got, "said behind the opening screen") || !strings.Contains(got, "╭─ Hermes Agent") {
		t.Errorf("after Enter the console is not there:\n%s", got)
	}
	if strings.Contains(got, splashHint) || len(strings.Split(got, "\n")) > 24 {
		t.Errorf("after Enter the opening screen left something behind:\n%s", got)
	}
	if lv.splashTick() {
		t.Error("the opening screen is still ticking")
	}
	if !strings.Contains(term.raw.String()[strings.LastIndex(term.raw.String(), clearScreen):], cursorShow) {
		t.Error("the cursor stayed hidden")
	}

	// So do an arrow and the wheel.
	for _, seq := range []string{"\x1b[A", "\x1b[B", "\x1b", "\t"} {
		term, lv, _ = newSplashLive(t, 72, 24, "rain")
		lv.Write([]byte("one\ntwo\n"))
		typeKeys(t, lv, seq)
		if got := term.text(); !strings.Contains(got, "one\ntwo\n") || strings.Contains(got, splashHint) {
			t.Errorf("after %q:\n%s", seq, got)
		}
	}

	// What is typed is not lost: it starts the prompt.
	term, lv, _ = newSplashLive(t, 72, 24, "print")
	lines, _ := typeKeys(t, lv, "hello\r")
	if len(lines) != 1 || lines[0] != "hello" {
		t.Errorf("typed over the opening screen: entered %q", lines)
	}
	term, lv, _ = newSplashLive(t, 72, 24, "print")
	typeKeys(t, lv, "h")
	if got := term.text(); !strings.Contains(got, "│ ❯ h ") {
		t.Errorf("the first key typed is not in the box:\n%s", got)
	}

	// Ctrl-C twice still leaves; the first one already counts.
	_, lv, _ = newSplashLive(t, 72, 24, "static")
	if _, quit := typeKeys(t, lv, "\x03"); quit {
		t.Error("one Ctrl-C on the opening screen quit")
	}
	if lv.splash != nil {
		t.Error("Ctrl-C left the opening screen up")
	}
	if _, quit := typeKeys(t, lv, "\x03"); !quit {
		t.Error("a second Ctrl-C did not quit")
	}
}

func TestTheOpeningScreenFollowsTheTerminalsSize(t *testing.T) {
	term, lv, clock := newSplashLive(t, 90, 30, "spin")
	wide := drawn(term)
	lv.size = func() (int, int) { return 40, 12 }
	term.cols, term.rows = 40, 12
	*clock = clock.Add(100 * time.Millisecond)
	lv.tick()
	got := term.text()
	lines := strings.Split(got, "\n")
	if len(lines) != 12 || strings.TrimSpace(lines[11]) != splashHint {
		t.Fatalf("after shrinking to 40x12:\n%s", got)
	}
	for _, line := range lines {
		if len([]rune(line)) > 40 {
			t.Fatalf("a row of %d characters on a screen 40 wide", len([]rune(line)))
		}
	}
	if small := drawn(term); small == 0 || small*3 > wide {
		t.Errorf("%d characters at 90x30, %d at 40x12", wide, small)
	}
}

func TestAStartWithoutAnOpeningScreenIsAsBefore(t *testing.T) {
	term := newVT(60, 20)
	lv := newLive(term, 60, 20, palette{}, func() time.Time { return noon })
	lv.set(sibylView())
	if lv.splash != nil || lv.splashTick() {
		t.Error("newLive opened with the animation")
	}
	if got := term.text(); !strings.Contains(got, "╭─ Hermes Agent") || strings.Contains(got, splashHint) {
		t.Errorf("screen:\n%s", got)
	}
}
