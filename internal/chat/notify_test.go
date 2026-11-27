package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/e2ee"
	"github.com/hauntedfail/geminus/internal/tui"
	"github.com/hauntedfail/geminus/internal/wire"
)

func extractOSC(raw, prefix string) []string {
	var out []string
	for {
		idx := strings.Index(raw, prefix)
		if idx < 0 {
			break
		}
		rest := raw[idx+len(prefix):]
		end := strings.Index(rest, "\x1b\\")
		if end < 0 {
			break
		}
		out = append(out, rest[:end])
		raw = rest[end+2:]
	}
	return out
}

func TestNotifyCLIAndSlashCommandPersistence(t *testing.T) {
	if req := parseArgs([]string{"--notify", "ghostty", "--no-notify"}); !req.badUsage {
		t.Fatalf("conflicting --notify and --no-notify should be badUsage")
	}
	if req := parseArgs([]string{"--notify", "unknown-mode"}); !req.badUsage {
		t.Fatalf("unknown --notify mode should be badUsage")
	}
	if req := parseArgs([]string{"sibyl", "--notify", "orca"}); req.badUsage || req.notify != "orca" || req.persona != "sibyl" {
		t.Fatalf("parseArgs(sibyl --notify orca) = %+v", req)
	}

	w := newWorld(t)
	w.app.notifyEnvFn = func(k string) string {
		if k == "TERM_PROGRAM" {
			return "ghostty"
		}
		return ""
	}
	w.talk([]string{"persona", "sibyl"}, [2]string{"/notify orca", "notify: orca (saved in Honcho)"})
	if got := w.app.notifyMode(); got != tui.NotifyOrca {
		t.Fatalf("notifyMode after /notify orca = %q, want orca", got)
	}
	if got := w.app.defaults.Notify; got != "orca" {
		t.Fatalf("defaults.Notify = %q, want orca", got)
	}
	// Piped mode (a.live == nil) must never emit terminal escape sequences.
	if strings.Contains(w.screen.String(), "\x1b]9999;") || strings.Contains(w.screen.String(), "\x1b]777;") {
		t.Fatalf("piped mode emitted OSC sequence: %q", w.screen.String())
	}
}

func TestNotifyGhosttyFullLifecycle(t *testing.T) {
	w := newWorld(t)
	w.app.notifyEnvFn = func(k string) string {
		if k == "TERM_PROGRAM" {
			return "ghostty"
		}
		return ""
	}
	term, typed, done := w.onTerminal(t, "persona", "sibyl")
	term.waitFor(t, "╭─ hermes · model:default · effort:default ─")

	// Turn 1: send a prompt and wait for normal completion.
	typed("build the app\r")
	term.waitFor(t, "echo: build the app")

	// Turn 2: hold the bridge so a permission ask arrives while waiting.
	w.mu.Lock()
	w.offline["sibyl"] = true
	w.mu.Unlock()
	typed("run command\r")
	term.waitFor(t, "waiting for sibyl")

	appendPermission(w, "req-ghostty", []string{"allow", "deny"}, now.Unix()+120)
	term.waitFor(t, "Permission · sibyl [Bash]")

	// Answer with 'y' and verify exit cleanup.
	typed("y")
	term.waitFor(t, "allow answer sent (Bash)")

	typed("\x03", "\x03")
	<-done

	raw := term.raw.String()
	progress := extractOSC(raw, "\x1b]9;4;")
	if len(progress) == 0 {
		t.Fatalf("expected OSC 9;4 progress updates in Ghostty, raw=%q", raw)
	}
	hasWorking, hasPaused, hasClear := false, false, false
	for _, p := range progress {
		switch p {
		case "3":
			hasWorking = true
		case "4;100":
			hasPaused = true
		case "0":
			hasClear = true
		}
	}
	if !hasWorking || !hasPaused || !hasClear {
		t.Fatalf("missing expected OSC 9;4 states (working=%v paused=%v clear=%v): %v", hasWorking, hasPaused, hasClear, progress)
	}

	notifs := extractOSC(raw, "\x1b]777;notify;")
	if len(notifs) < 2 {
		t.Fatalf("expected at least 2 OSC 777 notifications (done + permission ask), got %v", notifs)
	}
	sawDone, sawAsk := false, false
	for _, n := range notifs {
		if strings.Contains(n, "sibyl · hermes;") && strings.Contains(n, "echo: build the app") {
			sawDone = true
		}
		if strings.Contains(n, "Permission [Bash]:") {
			sawAsk = true
		}
	}
	if !sawDone || !sawAsk {
		t.Fatalf("expected done and ask OSC 777 notifications, got %v", notifs)
	}
	if strings.Contains(raw, "\x1b]9999;") {
		t.Fatalf("Ghostty auto mode should not emit OSC 9999")
	}
}

func TestNotifyOrcaLiveLifecycle(t *testing.T) {
	w := newWorld(t)
	w.app.notifyEnvFn = func(k string) string {
		if k == "TERM_PROGRAM" {
			return "Orca"
		}
		return ""
	}
	term, typed, done := w.onTerminal(t, "persona", "sibyl")
	term.waitFor(t, "╭─ hermes · model:default · effort:default ─")

	// Turn 1: completes normally.
	typed("check status\r")
	term.waitFor(t, "echo: check status")

	// Turn 2: triggers a live permission ask while waiting, then user answers.
	w.mu.Lock()
	w.offline["sibyl"] = true
	w.mu.Unlock()
	typed("deploy now\r")
	term.waitFor(t, "waiting for sibyl")

	appendPermission(w, "req-orca", []string{"allow", "deny"}, now.Unix()+120)
	term.waitFor(t, "Permission · sibyl [Bash]")
	typed("y")
	term.waitFor(t, "allow answer sent (Bash)")

	typed("\x03", "\x03")
	<-done

	raw := term.raw.String()
	if strings.Contains(raw, "\x1b]777;") {
		t.Fatalf("Orca auto mode should not emit OSC 777")
	}
	frames := extractOSC(raw, "\x1b]9999;")
	if len(frames) == 0 {
		t.Fatalf("expected OSC 9999 frames in Orca mode")
	}
	var sawBoundary, sawWorking, sawBlocked, sawDone bool
	for _, f := range frames {
		var p map[string]any
		if err := json.Unmarshal([]byte(f), &p); err != nil {
			t.Fatalf("invalid OSC 9999 JSON %q: %v", f, err)
		}
		if p["agentType"] != "hermes" {
			t.Errorf("expected agentType=hermes, got %v in %s", p["agentType"], f)
		}
		switch p["state"] {
		case "done":
			if p["sessionBoundary"] == true {
				sawBoundary = true
			} else if msg, _ := p["lastAssistantMessage"].(string); strings.Contains(msg, "echo: check status") {
				sawDone = true
			}
		case "working":
			sawWorking = true
		case "blocked":
			if p["toolName"] == "Bash" {
				sawBlocked = true
			}
		}
	}
	if !sawBoundary || !sawWorking || !sawBlocked || !sawDone {
		t.Fatalf("missing Orca lifecycle states: boundary=%v working=%v blocked=%v done=%v\nframes=%v",
			sawBoundary, sawWorking, sawBlocked, sawDone, frames)
	}
}

func TestNotifyOrcaStreamToolsAndErrors(t *testing.T) {
	w := newWorld(t)
	w.app.notifyEnvFn = func(k string) string {
		if k == "TERM_PROGRAM" {
			return "Orca"
		}
		return ""
	}
	term := newVT(100, 30)
	lv := newLive(term, 100, 30, palette{}, w.app.Now)
	w.app.attach(lv, make(chan string))
	w.app.cur = &Persona{Name: "mirai"}
	w.app.harness = &wire.Harness{ID: "claude", Name: "Claude Code"}
	w.app.session = "sess-1"
	w.app.waiting = map[string]bool{"turn-1": true}
	w.app.state = map[string]string{"turn-1": wire.StateQueued}
	w.app.turnPrompt = map[string]string{"turn-1": "run tests"}

	w.app.handleStreamEvent(e2ee.StreamEvent{
		Session: "sess-1",
		Turn:    "turn-1",
		Seq:     1,
		Kind:    e2ee.StreamKindToolUse,
		Tool:    "Bash",
		Detail:  "go test ./...",
	})
	w.app.handleStreamEvent(e2ee.StreamEvent{
		Session: "sess-1",
		Turn:    "turn-1",
		Seq:     2,
		Kind:    e2ee.StreamKindToolResult,
		Tool:    "Bash",
	})
	delete(w.app.waiting, "turn-1")
	w.app.notifyError("turn-1", "command exited with status 1")
	lv.close()

	frames := extractOSC(term.raw.String(), "\x1b]9999;")
	var sawTool, sawClearedTool, sawError, sawCloseBoundary bool
	for _, f := range frames {
		var p map[string]any
		if err := json.Unmarshal([]byte(f), &p); err != nil {
			t.Fatalf("invalid OSC 9999 JSON %q: %v", f, err)
		}
		if p["agentType"] != "claude" {
			t.Errorf("expected agentType=claude, got %v", p["agentType"])
		}
		switch p["state"] {
		case "working":
			if p["toolName"] == "Bash" && p["toolInput"] == "go test ./..." {
				sawTool = true
			} else if _, has := p["toolName"]; !has && sawTool {
				sawClearedTool = true
			}
		case "done":
			if p["interrupted"] == true && p["lastAssistantMessage"] == "command exited with status 1" {
				sawError = true
			}
			if p["sessionBoundary"] == true {
				sawCloseBoundary = true
			}
		}
	}
	if !sawTool || !sawClearedTool || !sawError || !sawCloseBoundary {
		t.Fatalf("missing stream/error states: tool=%v cleared=%v error=%v close=%v\nframes=%v",
			sawTool, sawClearedTool, sawError, sawCloseBoundary, frames)
	}
}

func TestNotifyClawdTitleAndDeduplication(t *testing.T) {
	w := newWorld(t)
	on := true
	w.app.clawdOverride = &on
	w.app.notifyEnvFn = func(k string) string {
		if k == "TERM" {
			return "xterm-ghostty"
		}
		return ""
	}
	term := newVT(100, 30)
	lv := newLiveWith(term, 100, 30, palette{}, w.app.Now, nil, true)
	w.app.attach(lv, make(chan string))
	w.app.cur = &Persona{Name: "mirai"}
	w.app.harness = &wire.Harness{ID: "claude", Name: "Claude Code"}

	// Same turn completed twice (e.g. RoleOutput final + RolePrompt done) notifies once.
	w.app.notifyDone("turn-1", "Done refactoring")
	w.app.notifyDone("turn-1", "Done refactoring")

	notifs := extractOSC(term.raw.String(), "\x1b]777;notify;")
	if len(notifs) != 1 {
		t.Fatalf("expected exactly 1 deduplicated OSC 777 notification, got %d: %v", len(notifs), notifs)
	}
	if notifs[0] != "Claude Code;Done refactoring" {
		t.Fatalf("Clawd mode notification = %q, want %q", notifs[0], "Claude Code;Done refactoring")
	}
}
