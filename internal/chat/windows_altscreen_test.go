package chat

import (
	"strings"
	"testing"
	"time"
)

func TestAltScreenAndWindowsConPTYLifecycleRestore(t *testing.T) {
	term := newVT(72, 20)
	// Pre-populate primary screen (simulating PowerShell prompt scrollback).
	_, _ = term.Write([]byte("PS C:\\Users\\h.ota> geminus\r\n"))

	lv := newLive(term, 72, 20, palette{On: true}, func() time.Time { return noon })
	lv.set(sibylView())

	// Entering live mode must enable alt-screen, bracketed paste, and the wheel
	// as arrow keys. It must not ask for mouse reports, which would take text
	// selection away from Windows Terminal.
	raw := term.raw.String()
	for _, seq := range mouseReports {
		if strings.Contains(raw, seq) {
			t.Fatalf("live startup asked for mouse reports with %q", seq)
		}
	}
	for _, seq := range []string{altScreenOn, pasteModeOn, altScrollOn} {
		if !strings.Contains(raw, seq) {
			t.Fatalf("live startup missing terminal mode sequence %q", seq)
		}
	}

	// Feed Japanese IME input and a Windows CRLF bracketed paste, then draw.
	lines := make(chan string, 4)
	var p keyParser
	for _, b := range []byte("日本語テスト") {
		lv.feed(p.Feed([]byte{b}), lines)
	}
	lv.feed(p.Feed([]byte("\x1b[200~\r\n二行目\x1b[201~")), lines)
	if got := lv.ed.String(); got != "日本語テスト\n二行目" {
		t.Fatalf("editor buffer after IME + CRLF paste = %q, want %q", got, "日本語テスト\n二行目")
	}

	// Closing live mode must disable paste, restore cursor visibility, and leave alt-screen.
	lv.close()
	raw = term.raw.String()
	for _, seq := range []string{pasteModeOf, cursorShow, altScreenOff} {
		if !strings.Contains(raw, seq) {
			t.Fatalf("live close missing restore sequence %q", seq)
		}
	}
	// Primary screen scrollback must be restored without any prompt box remnants.
	restored := term.text()
	if !strings.Contains(restored, "PS C:\\Users\\h.ota> geminus") {
		t.Fatalf("primary screen not restored after close():\n%s", restored)
	}
	if strings.Contains(restored, "╭─") || strings.Contains(restored, "日本語テスト") {
		t.Fatalf("alt-screen prompt box leaked into primary screen:\n%s", restored)
	}
}
