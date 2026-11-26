package chat

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAChoiceReplacesTheScreenAndLeavesTheConversation(t *testing.T) {
	term, lv, _ := newTestLive(80, 24)
	lv.Write([]byte("already said\n"))
	opts := []option{{key: "chrome", text: "THIS COMPUTER"}, {key: "mirai", text: "REMOTE"}}

	done := make(chan struct{})
	var picked string
	var ok bool
	go func() {
		picked, ok = lv.askChoice(context.Background(), "Personas", "persona", opts, "mirai")
		close(done)
	}()
	deadline := eventually(t, func() bool {
		return strings.Contains(term.text(), "Personas") && strings.Contains(term.text(), "mirai")
	})
	_ = deadline
	if strings.Contains(term.text(), "already said") {
		t.Fatalf("the conversation shows through the choice:\n%s", term.text())
	}
	// Down from mirai (the preferred one) wraps to chrome; Enter takes it.
	typeKeys(t, lv, "\x1b[B", "\r")
	<-done
	if !ok || picked != "chrome" {
		t.Fatalf("picked %q ok %v", picked, ok)
	}
	if got := term.text(); !strings.Contains(got, "already said") || strings.Contains(got, "THIS COMPUTER") || strings.Contains(got, "Personas") {
		t.Fatalf("after the choice:\n%s", got)
	}
}

func TestAChoiceCanBeTypedOrCancelled(t *testing.T) {
	term, lv, _ := newTestLive(80, 24)
	opts := []option{{key: "chrome", text: "here"}, {key: "mirai", text: "there"}}
	done := make(chan choiceResult, 1)
	go func() {
		v, ok := lv.askChoice(context.Background(), "Personas", "persona", opts, "")
		done <- choiceResult{v, ok}
	}()
	eventually(t, func() bool { return strings.Contains(term.text(), "chrome") })
	typeKeys(t, lv, "mi\r")
	if got := <-done; got.value != "mirai" || !got.ok {
		t.Fatalf("typed = %+v", got)
	}

	go func() {
		v, ok := lv.askChoice(context.Background(), "Personas", "persona", opts, "")
		done <- choiceResult{v, ok}
	}()
	eventually(t, func() bool { return strings.Contains(term.text(), "Personas") })
	typeKeys(t, lv, "\x03")
	if got := <-done; got.ok {
		t.Fatalf("Ctrl-C picked %q", got.value)
	}
	if strings.Contains(term.text(), "Personas") {
		t.Fatalf("the choice stayed up:\n%s", term.text())
	}
}

func eventually(t *testing.T, ready func() bool) bool {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ready() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the screen never became ready")
	return false
}
