package chat

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPostHumanCommandAndConfigToggle(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "chrome", "--harness", "claude"},
		[2]string{"/post-human status", "post-human mode: off"},
		[2]string{"/post-human", "Post-Human mode: on"},
		[2]string{"/config", "post_human       on"},
		[2]string{"/config post_human off", "Post-Human mode: off"},
		[2]string{"/post-human status", "post-human mode: off"})
}

func TestPostHumanLiveModalEditingAndBadges(t *testing.T) {
	var out bytes.Buffer
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	l := newLive(&out, 80, 24, palette{On: true}, func() time.Time { return now })
	l.set(view{
		persona:   "chrome",
		harness:   "hermes",
		reported:  true,
		seen:      now,
		postHuman: true,
	})

	// Starts in Insert mode with INS badge and bar cursor.
	if !strings.Contains(l.last, "INS") || !strings.Contains(l.last, cursorBar) {
		t.Fatalf("initial Post-Human frame missing INS badge or bar cursor:\n%s", l.last)
	}

	// Type text, press Esc to enter Normal mode -> NOR badge and block cursor.
	for _, k := range keysOf(t, "hello bad world") {
		l.key(k)
	}
	l.key(key{Kind: keyEsc})
	if !strings.Contains(l.last, "NOR") || !strings.Contains(l.last, cursorBlock) {
		t.Fatalf("Normal mode frame missing NOR badge or block cursor:\n%s", l.last)
	}

	// Pending minor mode indicator (`g`) appears on bottom border.
	l.key(key{Kind: keyRune, R: 'g'})
	if !strings.Contains(l.last, "NOR") || !strings.Contains(l.last, "g") {
		t.Fatalf("pending goto frame missing g indicator:\n%s", l.last)
	}
	l.key(key{Kind: keyRune, R: 'h'}) // goto line start

	// Move to second word (`w`), enter Select mode (`v`) -> SEL badge + underline cursor + inverted highlight.
	l.key(key{Kind: keyRune, R: 'w'})
	l.key(key{Kind: keyRune, R: 'v'})
	if !strings.Contains(l.last, "SEL") || !strings.Contains(l.last, cursorUnderline) || !strings.Contains(l.last, "\x1b[7m") {
		t.Fatalf("Select mode frame missing SEL badge, underline cursor, or selection highlight:\n%s", l.last)
	}
	// Exit Select mode back to Normal, move to "bad" (`w`), change it to "good" (`c`).
	l.key(key{Kind: keyRune, R: 'v'})
	l.key(key{Kind: keyRune, R: 'w'})
	for _, k := range keysOf(t, "cgood ") {
		l.key(k)
	}
	l.key(key{Kind: keyEsc})

	// Enter in Normal mode submits the edited prompt and returns to Insert mode.
	line, entered, quit := l.key(key{Kind: keyEnter})
	if !entered || quit || line != "hello good world" {
		t.Fatalf("submitted = (%q, %v, %v), want (%q, true, false)", line, entered, quit, "hello good world")
	}
	if !strings.Contains(l.last, "INS") {
		t.Fatalf("after submit, expected return to INS mode:\n%s", l.last)
	}

	// Closing restores default cursor shape before standard terminal restore sequences.
	l.close()
	raw := out.String()
	if !strings.Contains(raw, cursorDefault) {
		t.Fatalf("close did not restore default cursor shape")
	}
	if !strings.HasSuffix(raw, cursorShow+mouseModeOff+altScreenOff+pasteModeOf) {
		t.Fatalf("close did not end with standard restore sequences")
	}
}

func TestPostHumanLiveViewportScroll(t *testing.T) {
	var out bytes.Buffer
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	l := newLive(&out, 80, 14, palette{On: false}, func() time.Time { return now })
	l.set(view{
		persona:   "chrome",
		harness:   "hermes",
		reported:  true,
		seen:      now,
		postHuman: true,
	})
	for i := 0; i < 40; i++ {
		l.Write([]byte("transcript line\n"))
	}

	// Enter Normal mode and scroll up with Ctrl-u, then jump to bottom with zb.
	l.key(key{Kind: keyEsc})
	l.key(key{Kind: keyKillStart, Ctrl: 0x15})
	if l.scroll == 0 {
		t.Fatalf("Ctrl-u in Normal mode did not scroll transcript up")
	}
	l.key(key{Kind: keyRune, R: 'z'})
	l.key(key{Kind: keyRune, R: 'b'})
	if l.scroll != 0 {
		t.Fatalf("zb in Normal mode scroll = %d, want 0", l.scroll)
	}
}
