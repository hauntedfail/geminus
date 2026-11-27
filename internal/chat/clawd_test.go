package chat

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/splash"
	"github.com/hauntedfail/geminus/internal/wire"
)

func TestClawdCommandConfigAndPersistence(t *testing.T) {
	t.Setenv(clawdEnv, "")
	w := newWorld(t)
	w.talk([]string{"persona", "chrome", "--harness", "claude"},
		[2]string{"/clawd status", "clawd mode: off (default)"},
		[2]string{"/clawd", "Clawd mode: on"},
		[2]string{"/clawd status", "clawd mode: on (Honcho)"},
		[2]string{"/config", "clawd            on (Honcho)"},
		[2]string{"/new", "  ⎿  New session"},
		[2]string{"/config clawd off", "Clawd mode: off (saved in Honcho)"},
		[2]string{"/clawd status", "clawd mode: off (Honcho)"})
}

func TestClawdCLIFlagsEnvAndReopenArgs(t *testing.T) {
	// --clawd and --no-clawd are mutually exclusive.
	if req := parseArgs([]string{"--clawd", "--no-clawd"}); !req.badUsage {
		t.Fatal("expected badUsage when both --clawd and --no-clawd are passed")
	}
	if req := parseArgs([]string{"chrome", "--clawd"}); req.badUsage || !req.clawd || req.persona != "chrome" {
		t.Fatalf("parseArgs(chrome --clawd) = %+v", req)
	}
	if req := parseArgs([]string{"chrome", "--no-clawd"}); req.badUsage || !req.noClawd || req.persona != "chrome" {
		t.Fatalf("parseArgs(chrome --no-clawd) = %+v", req)
	}

	// Precedence: CLI flag > GEMINUS_CLAWD > Honcho defaults > default(off).
	t.Setenv(clawdEnv, "1")
	w := newWorld(t)
	w.talk([]string{"persona", "chrome", "--harness", "claude"},
		[2]string{"/clawd status", "clawd mode: on (GEMINUS_CLAWD)"})

	w.talk([]string{"persona", "chrome", "--harness", "claude", "--no-clawd"},
		[2]string{"/clawd status", "clawd mode: off (CLI flag)"})

	// reopenArgs preserves explicit CLI flag across /update relaunch.
	a := &App{
		cur:       &Persona{Name: "mirai"},
		harness:   &wire.Harness{ID: "claude"},
		clawdFlag: "--clawd",
	}
	args, ok := a.reopenArgs()
	if !ok {
		t.Fatal("reopenArgs returned ok=false")
	}
	if got := strings.Join(args, " "); !strings.Contains(got, "--clawd") {
		t.Fatalf("reopenArgs = %q, want --clawd preserved", got)
	}
}

func TestClawdLiveScreenMasqueradesAsClaudeCode(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	term := newVT(80, 24)
	lv := newLiveWith(term, 80, 24, palette{On: false}, func() time.Time { return now }, nil, true)
	lv.banner = &banner{
		mascot: splash.NewMascot("laugh", 1),
		mode:   splash.Mono,
		start:  now,
		lines:  []string{"geminus 0.6.18", "h1karu · Honcho workspace sibyl"},
	}
	lv.set(view{
		persona:      "chrome",
		local:        false,
		harness:      "hermes",
		reported:     true,
		seen:         now.Add(-5 * time.Second),
		cwd:          "/Users/vvx/projekt/acme",
		model:        "group/makima",
		defaultModel: "gpt-6-astra",
		clawd:        true,
	})

	screen := term.text()
	// Must show authentic Claude Code visual markers.
	for _, want := range []string{
		"▐▛███▜▌",
		"▝▜█████▛▘",
		"▘▘ ▝▝",
		"Claude Code",
		"Sonnet 4.6 · Claude Max",
		"~/projekt/acme",
		"────────",
		"❯",
		"? for shortcuts",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("Clawd idle screen missing %q:\n%s", want, screen)
		}
	}
	// Must NOT leak geminus / Honcho / persona / backend branding or rounded box.
	for _, forbid := range []string{
		"geminus", "Honcho", "bridge", "chrome", "hermes",
		"group/makima", "gpt-6-astra", "╭", "╰", "◆",
	} {
		if strings.Contains(screen, forbid) {
			t.Errorf("Clawd idle screen leaked %q:\n%s", forbid, screen)
		}
	}

	// While waiting (queued with doorbell rung) and while running: no Honcho/doorbell/persona leaks.
	lv.set(view{
		persona:  "chrome",
		harness:  "hermes",
		reported: true,
		seen:     now,
		cwd:      "/Users/vvx/projekt/acme",
		model:    "opus-4-6",
		waiting:  1,
		running:  false,
		rang:     true,
		since:    now.Add(-5 * time.Second),
		clawd:    true,
	})
	queuedScreen := term.text()
	if !strings.Contains(queuedScreen, "Thinking…") || !strings.Contains(queuedScreen, "Opus 4.6") {
		t.Errorf("Clawd queued screen missing Thinking… or Opus 4.6:\n%s", queuedScreen)
	}
	for _, forbid := range []string{"Honcho", "doorbell", "chrome", "hermes", "bridge"} {
		if strings.Contains(queuedScreen, forbid) {
			t.Errorf("Clawd queued screen leaked %q:\n%s", forbid, queuedScreen)
		}
	}

	// First Ctrl-C shows Claude Code exit prompt, not geminus/Honcho.
	lv.key(key{Kind: keyInterrupt})
	ctrlCScreen := term.text()
	if !strings.Contains(ctrlCScreen, "Press Ctrl-C again to exit") {
		t.Errorf("Clawd Ctrl-C notice unexpected:\n%s", ctrlCScreen)
	}
	if strings.Contains(ctrlCScreen, "geminus") || strings.Contains(ctrlCScreen, "Honcho") {
		t.Errorf("Clawd Ctrl-C notice leaked branding:\n%s", ctrlCScreen)
	}

	// ? shortcut overlay works and has no geminus branding.
	lv.key(key{Kind: keyRune, R: '?'})
	scScreen := term.text()
	if !strings.Contains(scScreen, "Shortcuts") || strings.Contains(scScreen, "geminus") {
		t.Errorf("Clawd shortcut screen invalid:\n%s", scScreen)
	}
	lv.key(key{Kind: keyEsc})

	// Typing an unknown slash command shows clean hint without persona/harness leak.
	for _, k := range keysOf(t, "/compact") {
		lv.key(k)
	}
	slashScreen := term.text()
	if strings.Contains(slashScreen, "geminus") || strings.Contains(slashScreen, "chrome") || strings.Contains(slashScreen, "hermes") {
		t.Errorf("Clawd slash hint leaked branding:\n%s", slashScreen)
	}

	// Toggling clawd off restores normal geminus UI & banner seamlessly.
	lv.ed.Clear()
	lv.set(view{
		persona:  "chrome",
		harness:  "hermes",
		reported: true,
		seen:     now,
		clawd:    false,
	})
	restored := term.text()
	if !strings.Contains(restored, "geminus") || !strings.Contains(restored, "╭") {
		t.Errorf("Restoring normal mode did not bring back geminus UI:\n%s", restored)
	}
}

func TestClawdScrollHeaderModeBadgesAndPostHuman(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	term := newVT(80, 16)
	lv := newLiveWith(term, 80, 16, palette{On: false}, func() time.Time { return now }, nil, true)
	lv.set(view{
		persona:  "mirai",
		harness:  "claude",
		reported: true,
		seen:     now,
		mode:     "accept-edits",
		clawd:    true,
	})
	if !strings.Contains(term.text(), "⏵⏵ accept edits on") {
		t.Fatalf("missing accept-edits badge:\n%s", term.text())
	}

	// Write enough lines to scroll up; scroll header should appear without persona/geminus.
	for i := 0; i < 30; i++ {
		lv.Write([]byte("line of transcript output\n"))
	}
	lv.key(key{Kind: keyPageUp})
	scrolled := term.text()
	if !strings.Contains(scrolled, "↑") {
		t.Fatalf("expected scroll badge when scrolled up:\n%s", scrolled)
	}
	if strings.Contains(scrolled, "geminus") || strings.Contains(scrolled, "mirai") {
		t.Fatalf("Clawd scroll header leaked branding:\n%s", scrolled)
	}

	// Post-Human interop in Clawd mode uses Claude Code vim-style indicators.
	lv.set(view{
		persona:   "mirai",
		harness:   "claude",
		reported:  true,
		seen:      now,
		postHuman: true,
		clawd:     true,
	})
	if !strings.Contains(term.text(), "-- INSERT --") {
		t.Fatalf("missing -- INSERT -- in Clawd + Post-Human:\n%s", term.text())
	}
	lv.key(key{Kind: keyEsc})
	if !strings.Contains(term.text(), "-- NORMAL --") {
		t.Fatalf("missing -- NORMAL -- in Clawd + Post-Human:\n%s", term.text())
	}
}

func TestClawdMenusAndChoiceUseClawdStyle(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	in := frameInput{
		v: view{
			persona:  "mirai",
			harness:  "hermes",
			reported: true,
			seen:     now,
			clawd:    true,
		},
		cols: 80,
		rows: 24,
		now:  now,
		pal:  palette{On: false},
	}
	m := newSelectMenu(menuConfig{
		title:    "Select model",
		subtitle: "Choose a model",
		items: []menuItem{
			{value: "sonnet", label: "Sonnet", desc: "Balanced", current: true},
			{value: "opus", label: "Opus", desc: "Deep reasoning"},
		},
	}, make(chan menuResult, 1))
	f := renderMenu(in, m)
	rendered := strings.Join(f.rows, "\n")
	if strings.Contains(rendered, "mirai") || strings.Contains(rendered, "╭") || strings.Contains(rendered, "╰") {
		t.Fatalf("Clawd selectMenu leaked persona badge or rounded border:\n%s", rendered)
	}
	if !strings.Contains(rendered, "────────") || !strings.Contains(rendered, "Select model") {
		t.Fatalf("Clawd selectMenu missing horizontal rule or title:\n%s", rendered)
	}

	// Choice picker in Clawd mode shows Clawd mascot instead of horned monster.
	term := newVT(80, 24)
	lv := newLiveWith(term, 80, 24, palette{On: false}, func() time.Time { return now }, nil, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		lv.askChoice(ctx, "Select session", "session", []option{{key: "one", text: "first"}}, "one")
	}()
	// Wait briefly until choice is active and drawn.
	for i := 0; i < 50; i++ {
		lv.mu.Lock()
		active := lv.choice != nil
		lv.mu.Unlock()
		if active {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	choiceScreen := term.text()
	cancel()
	<-done
	if !strings.Contains(choiceScreen, "▐▛███▜▌") || !strings.Contains(choiceScreen, "❯ ") {
		t.Fatalf("Clawd choice screen missing Clawd mascot or ❯ marker:\n%s", choiceScreen)
	}
}

func TestClawdTranscriptFormattingAndStreaming(t *testing.T) {
	var buf bytes.Buffer
	on := true
	a := &App{
		Out:           NewPrinter(&buf),
		cur:           &Persona{Name: "chrome"},
		harness:       &wire.Harness{ID: "hermes", Name: "Hermes"},
		clawdOverride: &on,
		shownTurn:     map[string]bool{},
		turnPrompt:    map[string]string{},
	}

	a.echoPrompt("fix the build\nwith tests")
	a.show("First line\nSec", wire.Message{Turn: "t1", OK: true, Final: false})
	a.show("ond line\nThird line", wire.Message{Turn: "t1", OK: true, Final: true})
	a.showBridgeState("running go test", wire.Message{Turn: "t1", Phase: "running"})
	a.showBridgeAsk("Run go test ./...", wire.Message{Turn: "t1", Tool: "Bash"})

	got := buf.String()
	for _, want := range []string{
		"❯ fix the build",
		"  with tests",
		"● First line",
		"  Second line",
		"  Third line",
		"  ⎿  running go test",
		"● Bash",
		"  ⎿  Run go test ./...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Clawd transcript missing %q:\n%s", want, got)
		}
	}
	for _, forbid := range []string{"── you", "── chrome", "chrome asks", "· chrome:"} {
		if strings.Contains(got, forbid) {
			t.Errorf("Clawd transcript leaked %q:\n%s", forbid, got)
		}
	}
}
