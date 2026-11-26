package chat

import (
	"strings"
	"testing"
	"time"
)

func TestQuestionMarkExpandsShortcuts(t *testing.T) {
	got, _ := draw(frameInput{v: sibylView(), shortcuts: true, cols: 80, rows: 40})
	for _, want := range []string{"Shortcuts", "Ctrl-P / Ctrl-N", "Alt+Enter", "Ctrl-C"} {
		if !strings.Contains(got, want) {
			t.Errorf("shortcut view missing %q:\n%s", want, got)
		}
	}

	var screen strings.Builder
	lv := newLive(&screen, 80, 40, palette{}, func() time.Time { return noon })
	if line, entered, quit := lv.key(key{Kind: keyRune, R: '?'}); line != "" || entered || quit || lv.ed.String() != "" || !lv.shortcuts {
		t.Fatalf("? on an empty prompt = %q entered %v quit %v text %q open %v", line, entered, quit, lv.ed.String(), lv.shortcuts)
	}
	if !strings.Contains(screen.String(), "Ctrl-P / Ctrl-N") {
		t.Fatal("opened shortcut view was not drawn")
	}
	lv.key(key{Kind: keyRune, R: '?'})
	if lv.shortcuts {
		t.Fatal("a second ? left the shortcut view open")
	}
	lv.key(key{Kind: keyRune, R: '?'})
	lv.key(key{Kind: keyEsc})
	if lv.shortcuts {
		t.Fatal("Esc left the shortcut view open")
	}
	lv.key(key{Kind: keyRune, R: 'h'})
	lv.key(key{Kind: keyRune, R: '?'})
	if lv.ed.String() != "h?" || lv.shortcuts {
		t.Fatalf("? while typing = %q open %v", lv.ed.String(), lv.shortcuts)
	}
}
