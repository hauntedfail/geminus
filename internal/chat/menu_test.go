package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/wire"
)

func TestModelMenuRenderAndLayout(t *testing.T) {
	items := []menuItem{
		{value: "", label: "default", desc: "Claude Code's own default"},
		{value: "opus", label: "opus", desc: "Most capable for complex reasoning", current: true},
		{value: "sonnet", label: "sonnet", desc: "Balanced speed and intelligence"},
		{value: "haiku", label: "haiku", desc: "Fast and lightweight"},
		{label: "Custom model…", desc: "Enter any model name", custom: true},
	}
	m := newSelectMenu(menuConfig{
		title:             "Select model",
		subtitle:          "Model for this conversation · Claude Code",
		customTitle:       "Custom model",
		customSubtitle:    "Enter any model name for Claude Code",
		customPlaceholder: "e.g. group/lvmh, opus, sonnet",
		items:             items,
	}, nil)

	// Cursor starts on the currently active model (opus).
	if m.cursor != 1 {
		t.Fatalf("initial cursor = %d, want 1 (opus)", m.cursor)
	}

	v := sibylView()
	v.harness = "Claude Code"
	v.model = "opus"

	for _, cols := range []int{80, 44, 30} {
		f := renderMenu(frameInput{v: v, cols: cols, rows: 24, now: noon}, m)
		got := strings.Join(f.rows, "\n")
		if !strings.Contains(got, "╭─ Select model ") {
			t.Errorf("cols=%d missing title:\n%s", cols, got)
		}
		if !strings.Contains(got, "❯ 2. opus") || !strings.Contains(got, "✓") {
			t.Errorf("cols=%d missing highlighted active row:\n%s", cols, got)
		}
		if !strings.Contains(got, "5. Custom model…") {
			t.Errorf("cols=%d missing custom option:\n%s", cols, got)
		}
		// Every bordered row must have the exact same display width.
		wantW := max(cols-1, 24)
		for _, row := range f.rows {
			if strings.HasPrefix(row, "╭") || strings.HasPrefix(row, "│") || strings.HasPrefix(row, "╰") {
				if w := textWidth(row); w != wantW {
					t.Errorf("cols=%d row width = %d, want %d: %q", cols, w, wantW, row)
				}
			}
		}
	}
}

func TestModelMenuNavigationQuickSelectAndCustomInput(t *testing.T) {
	items := []menuItem{
		{value: "", label: "default", desc: "Hermes Agent's own default", current: true},
		{value: "group/lvmh", label: "group/lvmh", desc: "Saved default"},
		{label: "Custom model…", desc: "Enter any model name", custom: true},
	}
	cfg := menuConfig{
		title:             "Select model",
		subtitle:          "Model for this conversation · hermes",
		customTitle:       "Custom model",
		customSubtitle:    "Enter any model name for hermes",
		customPlaceholder: "e.g. group/lvmh",
		items:             items,
		validate: func(v string) error {
			if !wire.ValidSetting("model", v) {
				return errInvalidTestModel
			}
			return nil
		},
	}

	// 1. Arrow navigation with wrap-around + Enter.
	term, lv, _ := newTestLive(72, 22)
	resCh := make(chan menuResult, 1)
	go func() {
		v, ok := lv.selectOption(context.Background(), cfg)
		resCh <- menuResult{value: v, ok: ok}
	}()
	// Wait for menu to appear on screen.
	waitMenu := func(want string) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(term.text(), want) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("screen did not show %q:\n%s", want, term.text())
	}
	waitMenu("╭─ Select model")
	typeKeys(t, lv, "\x1b[B", "\r") // Down -> group/lvmh, Enter
	res := <-resCh
	if !res.ok || res.value != "group/lvmh" {
		t.Fatalf("arrow select = %+v, want {group/lvmh true}", res)
	}

	// 2. Quick number select (press '1' -> default).
	go func() {
		v, ok := lv.selectOption(context.Background(), cfg)
		resCh <- menuResult{value: v, ok: ok}
	}()
	waitMenu("╭─ Select model")
	typeKeys(t, lv, "1")
	res = <-resCh
	if !res.ok || res.value != "" {
		t.Fatalf("quick number select = %+v, want {\"\" true}", res)
	}

	// 3. Esc cancels cleanly.
	go func() {
		v, ok := lv.selectOption(context.Background(), cfg)
		resCh <- menuResult{value: v, ok: ok}
	}()
	waitMenu("╭─ Select model")
	typeKeys(t, lv, "\x1b")
	res = <-resCh
	if res.ok {
		t.Fatalf("Esc did not cancel: %+v", res)
	}

	// 4. Custom model flow: select '3', invalid input shows error, Esc goes back,
	// re-open custom, type valid model, Enter confirms.
	go func() {
		v, ok := lv.selectOption(context.Background(), cfg)
		resCh <- menuResult{value: v, ok: ok}
	}()
	waitMenu("╭─ Select model")
	typeKeys(t, lv, "3")
	waitMenu("╭─ Custom model")
	typeKeys(t, lv, "--bad-flag\r")
	waitMenu("✗ invalid test model")
	typeKeys(t, lv, "\x1b") // back to list
	waitMenu("╭─ Select model")
	typeKeys(t, lv, "3")
	waitMenu("╭─ Custom model")
	typeKeys(t, lv, "group/miumiu\r")
	res = <-resCh
	if !res.ok || res.value != "group/miumiu" {
		t.Fatalf("custom model select = %+v, want {group/miumiu true}", res)
	}
}

var errInvalidTestModel = &testErr{"invalid test model"}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

func TestEmptyModelCommandOpensInteractiveMenuInLiveMode(t *testing.T) {
	w := newWorld(t)
	term := newVT(76, 22)
	lv := newLive(term, 76, 22, palette{}, func() time.Time { return now })
	lines := make(chan string, 16)
	w.app.ctx = context.Background()
	w.app.attach(lv, lines)
	if err := w.app.loadDirectory(); err != nil {
		t.Fatal(err)
	}
	if err := w.app.start("chrome", "claude"); err != nil {
		t.Fatal(err)
	}
	w.app.sync()

	done := make(chan error, 1)
	go func() {
		done <- w.app.handle("/model")
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(term.text(), "╭─ Select model") && strings.Contains(term.text(), "opus") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(term.text(), "╭─ Select model") || !strings.Contains(term.text(), "opus") {
		t.Fatalf("menu did not render for /model:\n%s", term.text())
	}

	// Pick option 2 (opus) with quick number key.
	typeKeys(t, lv, "2")
	if err := <-done; err != nil {
		t.Fatalf("/model failed: %v", err)
	}
	if w.app.tuning.Model != "opus" {
		t.Fatalf("tuning.Model = %q, want opus", w.app.tuning.Model)
	}
	if len(w.app.recentModels["claude"]) == 0 || w.app.recentModels["claude"][0] != "opus" {
		t.Errorf("recentModels = %+v, want opus recorded", w.app.recentModels)
	}
}

func TestModelMenuScrollsWhenManyItems(t *testing.T) {
	items := make([]menuItem, 0, 15)
	for i := 0; i < 14; i++ {
		items = append(items, menuItem{value: "m" + string(rune('a'+i)), label: "model-" + string(rune('a'+i)), desc: "desc"})
	}
	items = append(items, menuItem{label: "Custom model…", custom: true})
	m := newSelectMenu(menuConfig{title: "Select model", items: items}, nil)
	v := sibylView()

	f := renderMenu(frameInput{v: v, cols: 70, rows: 16, now: noon}, m)
	got := strings.Join(f.rows, "\n")
	if !strings.Contains(got, "↓ ") || !strings.Contains(got, "more") {
		t.Errorf("expected downward scroll indicator:\n%s", got)
	}

	// Jump to end -> upward indicator appears.
	m.handleKey(key{kind: keyEnd})
	f = renderMenu(frameInput{v: v, cols: 70, rows: 16, now: noon}, m)
	got = strings.Join(f.rows, "\n")
	if !strings.Contains(got, "↑ ") || !strings.Contains(got, "more") || !strings.Contains(got, "Custom model…") {
		t.Errorf("expected upward scroll indicator and custom item:\n%s", got)
	}
}
