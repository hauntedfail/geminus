package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/splash"
	"github.com/hauntedfail/geminus/internal/version"
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
	sp, _ := opening(env(splashEnv, name), false, clock, 0)
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

func TestHowGeminusOpensCanBeChosen(t *testing.T) {
	for _, off := range []string{"off", "OFF", " 0 ", "no", "false", "none"} {
		if screen, mascot := opening(env(splashEnv, off), true, noon, 1); screen != nil || mascot != nil {
			t.Errorf("%s=%q still opens with something", splashEnv, off)
		}
	}
	// Left alone: the mascot, and no opening screen in the way.
	screen, mascot := opening(env(), true, noon, 1)
	if screen != nil || mascot == nil {
		t.Fatalf("by default: opening screen %v, mascot %v", screen != nil, mascot != nil)
	}
	// Another start can show the mascot doing something else.
	seen := map[string]bool{}
	for n := uint32(0); n < 64; n++ {
		_, m := opening(env(), true, noon, n)
		seen[m.mascot.Name()] = true
	}
	if len(seen) < 4 {
		t.Errorf("64 starts showed the mascot doing only %d things: %v", len(seen), seen)
	}
	// The opening screen when asked for, by name or left to chance.
	if screen, mascot = opening(env(splashEnv, "Rain"), true, noon, 0); screen == nil || screen.anim.Name != "rain" || mascot == nil {
		t.Errorf("%s=Rain: opening screen %+v, mascot %v", splashEnv, screen, mascot != nil)
	}
	seen = map[string]bool{}
	for n := uint32(0); n < 64; n++ {
		if screen, _ = opening(env(splashEnv, "full"), true, noon, n); screen == nil {
			t.Fatalf("%s=full: no opening screen", splashEnv)
		}
		seen[screen.anim.Name] = true
	}
	if len(seen) < 5 {
		t.Errorf("64 starts with %s=full showed only %d animations: %v", splashEnv, len(seen), seen)
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

// newMascotLive is a terminal with the mascot at the top of what is said.
func newMascotLive(cols, rows int, doing string) (*vt, *live, *time.Time) {
	term := newVT(cols, rows)
	clock := noon
	lv := newLive(term, cols, rows, palette{}, func() time.Time { return clock })
	lv.banner = &banner{mascot: splash.NewMascot(doing, 0), mode: splash.Mono, start: clock,
		lines: []string{"geminus 9.9.9", "me · Honcho workspace ws"}}
	lv.set(sibylView())
	return term, lv, &clock
}

func TestTheMascotStandsBesideTheNameAtTheTop(t *testing.T) {
	term, lv, _ := newMascotLive(72, 24, "look")
	lv.Write([]byte("first line\n"))
	rows := strings.Split(term.text(), "\n")
	// Row 0 is the sticky header; the mascot's five rows follow, then an
	// empty one, then what was said.
	if len(rows) < 8 || !strings.HasPrefix(rows[0], "◆ geminus · sibyl") {
		t.Fatalf("screen:\n%s", term.text())
	}
	for i := 1; i <= splash.MascotRows; i++ {
		if !strings.ContainsAny(rows[i], "▀▄█▌▐▛▜▙▟▘▝▖▗▚▞") {
			t.Errorf("row %d has nothing of the mascot: %q", i, rows[i])
		}
	}
	if !strings.HasSuffix(rows[2], "  geminus 9.9.9") || !strings.HasSuffix(rows[3], "  me · Honcho workspace ws") {
		t.Errorf("beside the mascot: %q / %q", rows[2], rows[3])
	}
	if strings.ContainsAny(rows[1], "9w") || rows[6] != "" || rows[7] != "first line" {
		t.Errorf("around the banner: %q / %q / %q", rows[1], rows[6], rows[7])
	}
	// No key is waited for: the box is there at once.
	if !strings.Contains(term.text(), "╭─ Hermes Agent") {
		t.Errorf("the prompt box is missing:\n%s", term.text())
	}
}

func TestTheMascotMovesAndComesToRestAgain(t *testing.T) {
	for _, doing := range splash.MascotAnims {
		term, lv, clock := newMascotLive(72, 24, doing)
		// The mascot's own rows: the status under the box changes too, as
		// what it reports ages.
		mascot := func() string {
			return strings.Join(strings.Split(term.text(), "\n")[1:1+splash.MascotRows], "\n")
		}
		rest := mascot()
		frames, back := map[string]bool{rest: true}, false
		for i := 0; i < 400; i++ {
			*clock = clock.Add(frameEvery)
			lv.tick()
			got := mascot()
			if frames[got] = true; len(frames) > 1 && got == rest {
				back = true
			}
			if rows := strings.Split(term.text(), "\n"); len(rows) > 24 || !strings.HasSuffix(rows[2], "  geminus 9.9.9") {
				t.Fatalf("%s after %d ticks: the screen is out of place:\n%s", doing, i, term.text())
			}
		}
		if len(frames) < 2 || !back {
			t.Errorf("%s: %d different screens in 40 s, back at rest: %v", doing, len(frames), back)
		}
	}
}

func TestTheMascotScrollsAwayWithWhatWasSaid(t *testing.T) {
	term, lv, clock := newMascotLive(72, 16, "look")
	// Nothing on this screen ages, so only the mascot could change it.
	lv.set(view{persona: "sibyl", harness: "Hermes Agent"})
	for i := 0; i < 30; i++ {
		lv.Write([]byte("line\n"))
	}
	if got := term.text(); strings.Contains(got, "geminus 9.9.9") || strings.ContainsAny(got, "█▛▜") {
		t.Fatalf("the mascot is still on the screen after 30 lines:\n%s", got)
	}
	// Out of sight it costs nothing: time passing writes nothing.
	before := term.raw.Len()
	for i := 0; i < 100; i++ {
		*clock = clock.Add(frameEvery)
		lv.tick()
	}
	if wrote := term.raw.Len() - before; wrote != 0 {
		t.Errorf("%d bytes written while the mascot was out of sight", wrote)
	}
	// Scrolled back to the top, it is there.
	for i := 0; i < 10; i++ {
		typeKeys(t, lv, "\x1b[5~")
	}
	if got := term.text(); !strings.Contains(got, "  geminus 9.9.9") {
		t.Errorf("at the top after PageUp:\n%s", got)
	}
}

func TestTheMascotFitsANarrowTerminalAndLeavesNoColourOn(t *testing.T) {
	b := &banner{mascot: splash.NewMascot("look", 0), mode: splash.TrueColor, start: noon,
		lines: []string{"\x1b[1mgeminus\x1b[0m\x1b[2m 9.9.9\x1b[0m", "\x1b[2mme · Honcho workspace ws\x1b[0m"}}
	for _, width := range []int{120, 30, 20, 9, 1} {
		rows := b.rows(noon, width)
		if len(rows) != bannerRows || rows[bannerRows-1] != "" {
			t.Fatalf("width %d: %d rows, last %q", width, len(rows), rows[len(rows)-1])
		}
		for i, row := range rows {
			if w := textWidth(row); w > width {
				t.Errorf("width %d: row %d is %d wide", width, i, w)
			}
			if strings.Contains(row, "\x1b[") && !strings.HasSuffix(row, "\x1b[0m") {
				t.Errorf("width %d: row %d leaves its colours on: %q", width, i, row)
			}
		}
	}
	if rows := b.rows(noon, 120); !strings.Contains(rows[0], "\x1b[38;2;239;228;157m") || !strings.Contains(rows[2], "\x1b[38;2;202;212;47m") {
		t.Errorf("the mascot is not in the logo's colours: %q / %q", rows[0], rows[2])
	}
}

func TestTheNameIsBesideTheMascotOnlyWhenThereIsOne(t *testing.T) {
	w := newWorld(t)
	term := newVT(81, 40)
	lv := newLive(term, 81, 40, palette{}, w.app.Now)
	lv.banner = &banner{mascot: splash.NewMascot("look", 0), mode: splash.Mono, start: w.app.Now()}
	w.app.attach(lv, make(chan string, 1))
	got := term.text()
	if strings.Contains(got, "◆ geminus "+version.Version) || !strings.Contains(got, "  geminus "+version.Version) ||
		!strings.Contains(got, "Honcho workspace ws") {
		t.Errorf("with the mascot:\n%s", got)
	}

	w = newWorld(t)
	term = newVT(81, 40)
	lv = newLive(term, 81, 40, palette{}, w.app.Now)
	w.app.attach(lv, make(chan string, 1))
	if got := term.text(); !strings.Contains(got, "◆ geminus "+version.Version+" · ") || strings.ContainsAny(got, "█▛▜") {
		t.Errorf("without the mascot:\n%s", got)
	}
}
