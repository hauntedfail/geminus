package chat

import "testing"

func TestConfigNonTerminalSummaryAndShortcuts(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "chrome", "--harness", "claude"},
		[2]string{"/config", "geminus config (saved in Honcho)"},
		[2]string{"/config auto_update on", "auto_update: on (saved in Honcho)"},
		[2]string{"/config", "auto_update      on (Honcho)"},
		[2]string{"/config node off", "ui node: off (saved in Honcho)"},
		[2]string{"/config", "ui.node          off"})
}

func TestConfigInteractiveMenuToggleSearchAndSubmenu(t *testing.T) {
	w := newWorld(t)
	term, typed, done := w.onTerminal(t, "persona", "chrome", "--harness", "claude")
	term.waitFor(t, "╭─ Claude Code · model:default · effort:default ─", "chrome ─╮")

	// Open /config interactive menu.
	typed("/config\r")
	term.waitFor(t, "╭─ Settings", "Auto-update", "Show node badge")

	// 1. Enter toggles Auto-update inline and keeps menu open on Auto-update.
	typed("\r")
	term.waitFor(t, "Saved: auto_update = on (Honcho)")

	// 2. Down + Enter toggles Show node badge off.
	typed("\x1b[B\r")
	term.waitFor(t, "Saved: ui.node = off (Honcho)")

	// 3. Filter with search ("persona") and open Default persona submenu.
	typed("persona")
	term.waitFor(t, "Search: persona", "Default persona")
	typed("\r")
	term.waitFor(t, "╭─ Default persona", "chrome")

	// Pick chrome (option 2: 1=(none), 2=chrome).
	typed("2")
	term.waitFor(t, "╭─ Settings", "Saved: default.persona = chrome (Honcho)")

	// Close menu with Esc.
	typed("\x1b")
	term.waitFor(t, "config closed (3 updated, saved in Honcho)")

	// Verify persisted values via /update auto and /default.
	typed("/update auto\r")
	term.waitFor(t, "auto_update: on (Honcho)")
	typed("/default\r")
	term.waitFor(t, "persona  chrome", "auto_update  on (Honcho)")

	typed("/quit\r")
	if code := <-done; code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestEffortInteractiveMenu(t *testing.T) {
	w := newWorld(t)
	term, typed, done := w.onTerminal(t, "persona", "chrome", "--harness", "claude")
	term.waitFor(t, "╭─ Claude Code · model:default · effort:default ─", "chrome ─╮")

	typed("/effort\r")
	term.waitFor(t, "╭─ Select effort", "high")
	typed("high\r")
	term.waitFor(t, "effort: high", "model:default · high")

	typed("/quit\r")
	if code := <-done; code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}
