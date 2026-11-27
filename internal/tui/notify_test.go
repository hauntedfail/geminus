package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseNotifyMode(t *testing.T) {
	cases := []struct {
		in   string
		want NotifyMode
		ok   bool
	}{
		{"", NotifyAuto, true},
		{"auto", NotifyAuto, true},
		{"on", NotifyAuto, true},
		{"ghostty", NotifyGhostty, true},
		{"osc777", NotifyGhostty, true},
		{"orca", NotifyOrca, true},
		{"osc9999", NotifyOrca, true},
		{"osc9", NotifyOSC9, true},
		{"iterm2", NotifyOSC9, true},
		{"osc99", NotifyOSC99, true},
		{"kitty", NotifyOSC99, true},
		{"bell", NotifyBell, true},
		{"bel", NotifyBell, true},
		{"all", NotifyAll, true},
		{"both", NotifyAll, true},
		{"off", NotifyOff, true},
		{"false", NotifyOff, true},
		{"none", NotifyOff, true},
		{"bogus", "", false},
	}
	for _, tc := range cases {
		got, ok := ParseNotifyMode(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseNotifyMode(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDetectTerminalAndMux(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want TerminalKind
	}{
		{map[string]string{"TERM_PROGRAM": "Orca"}, TermOrca},
		{map[string]string{"ORCA_PANE_KEY": "tab1:leaf1"}, TermOrca},
		{map[string]string{"ORCA_APP_VERSION": "1.4.262"}, TermOrca},
		{map[string]string{"TERM_PROGRAM": "ghostty"}, TermGhostty},
		{map[string]string{"TERM": "xterm-ghostty"}, TermGhostty},
		{map[string]string{"GHOSTTY_RESOURCES_DIR": "/usr/share/ghostty"}, TermGhostty},
		{map[string]string{"TERM": "xterm-kitty"}, TermKitty},
		{map[string]string{"KITTY_WINDOW_ID": "1"}, TermKitty},
		{map[string]string{"TERM_PROGRAM": "WezTerm"}, TermWezTerm},
		{map[string]string{"TERM_PROGRAM": "iTerm.app"}, TermITerm2},
		{map[string]string{"WT_SESSION": "guid-123"}, TermWindowsTerminal},
		{map[string]string{"TERM": "xterm-256color"}, TermGeneric},
		{map[string]string{}, TermGeneric},
	}
	for _, tc := range cases {
		if got := DetectTerminal(envMap(tc.env)); got != tc.want {
			t.Errorf("DetectTerminal(%v) = %q, want %q", tc.env, got, tc.want)
		}
	}
	if got := DetectMux(envMap(map[string]string{"TMUX": "/tmp/tmux-1000/default,1,0"})); got != MuxTmux {
		t.Errorf("DetectMux(TMUX) = %q, want tmux", got)
	}
	if got := DetectMux(envMap(map[string]string{"STY": "1234.pts-0"})); got != MuxScreen {
		t.Errorf("DetectMux(STY) = %q, want screen", got)
	}
	if got := DetectMux(envMap(map[string]string{})); got != MuxNone {
		t.Errorf("DetectMux(empty) = %q, want none", got)
	}
}

func TestFormatOSC777SanitizesControlAndSemicolon(t *testing.T) {
	got := FormatOSC777("mirai;claude\x1b[31m\x07\n", "line1\nline2\x07\x1b")
	want := "\x1b]777;notify;mirai,claude[31m;line1 line2\x1b\\"
	if got != want {
		t.Fatalf("FormatOSC777 = %q, want %q", got, want)
	}
}

func TestFormatOSC9GuardsProgressCollision(t *testing.T) {
	got := FormatOSC9("", "4;2;100")
	if strings.HasPrefix(got, "\x1b]9;4;") {
		t.Fatalf("FormatOSC9 collided with OSC 9;4 progress: %q", got)
	}
}

func TestFormatOSC9999MatchesOrcaContract(t *testing.T) {
	seq := FormatOSC9999(AgentEvent{
		State:     EventBlocked,
		AgentType: "claude",
		Model:     "opus",
		Prompt:    "run tests\nnow",
		ToolName:  "Bash",
		ToolInput: "go test ./...",
	})
	if !strings.HasPrefix(seq, "\x1b]9999;") || !strings.HasSuffix(seq, "\x1b\\") {
		t.Fatalf("unexpected OSC 9999 framing: %q", seq)
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]9999;"), "\x1b\\")
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("invalid OSC 9999 JSON: %v (%s)", err, raw)
	}
	if payload["state"] != "blocked" || payload["agentType"] != "claude" || payload["toolName"] != "Bash" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	if _, has := payload["sessionBoundary"]; has {
		t.Fatalf("blocked payload must not set sessionBoundary: %#v", payload)
	}

	boundary := FormatOSC9999(AgentEvent{State: EventBoundary, AgentType: "codex"})
	rawB := strings.TrimSuffix(strings.TrimPrefix(boundary, "\x1b]9999;"), "\x1b\\")
	var bp map[string]any
	if err := json.Unmarshal([]byte(rawB), &bp); err != nil {
		t.Fatalf("invalid boundary JSON: %v", err)
	}
	if bp["state"] != "done" || bp["sessionBoundary"] != true {
		t.Fatalf("boundary must emit done+sessionBoundary=true, got %#v", bp)
	}

	errSeq := FormatOSC9999(AgentEvent{State: EventError, AgentType: "claude", LastMessage: "boom"})
	rawE := strings.TrimSuffix(strings.TrimPrefix(errSeq, "\x1b]9999;"), "\x1b\\")
	var ep map[string]any
	if err := json.Unmarshal([]byte(rawE), &ep); err != nil {
		t.Fatalf("invalid error JSON: %v", err)
	}
	if ep["state"] != "done" || ep["interrupted"] != true || ep["lastAssistantMessage"] != "boom" {
		t.Fatalf("error must emit done+interrupted=true, got %#v", ep)
	}
}

func TestNotifierGhosttyOrcaAndTmux(t *testing.T) {
	doneEv := AgentEvent{
		State:       EventDone,
		AgentType:   "claude",
		Title:       "mirai · Claude Code",
		Body:        "All tests pass",
		LastMessage: "All tests pass",
		Alert:       true,
	}

	// Ghostty auto: OSC 9;4 clear + OSC 777 desktop notification, no OSC 9999.
	ghostty := Notifier{Mode: NotifyAuto, Term: TermGhostty, Mux: MuxNone}
	gOut := ghostty.Sequences(doneEv)
	if !strings.Contains(gOut, "\x1b]9;4;0\x1b\\") || !strings.Contains(gOut, "\x1b]777;notify;mirai · Claude Code;All tests pass\x1b\\") {
		t.Fatalf("Ghostty auto output = %q", gOut)
	}
	if strings.Contains(gOut, "\x1b]9999;") {
		t.Fatalf("Ghostty auto should not emit OSC 9999: %q", gOut)
	}

	// Ghostty working: indeterminate progress only, no desktop alert.
	gWork := ghostty.Sequences(AgentEvent{State: EventWorking, AgentType: "claude"})
	if gWork != "\x1b]9;4;3\x1b\\" {
		t.Fatalf("Ghostty working = %q, want OSC 9;4;3", gWork)
	}

	// Orca auto: OSC 9999 only (Orca generates its own native notifications).
	orca := Notifier{Mode: NotifyAuto, Term: TermOrca, Mux: MuxNone}
	oOut := orca.Sequences(doneEv)
	if !strings.HasPrefix(oOut, "\x1b]9999;") || strings.Contains(oOut, "\x1b]777;") {
		t.Fatalf("Orca auto output = %q", oOut)
	}

	// Generic (e.g. SSH where TERM_PROGRAM was stripped): OSC 9999 + BEL.
	generic := Notifier{Mode: NotifyAuto, Term: TermGeneric, Mux: MuxNone}
	genOut := generic.Sequences(doneEv)
	if !strings.Contains(genOut, "\x1b]9999;") || !strings.HasSuffix(genOut, "\x1b\\\x07") {
		t.Fatalf("Generic auto output should contain OSC 9999 + BEL: %q", genOut)
	}

	// tmux wraps OSC sequences in DCS passthrough with doubled ESC, leaves BEL unwrapped.
	tmuxGhostty := Notifier{Mode: NotifyGhostty, Term: TermGhostty, Mux: MuxTmux}
	tOut := tmuxGhostty.Sequences(doneEv)
	if !strings.Contains(tOut, "\x1bPtmux;\x1b\x1b]777;notify;") || !strings.HasSuffix(tOut, "\x1b\\") {
		t.Fatalf("tmux passthrough malformed: %q", tOut)
	}
	tmuxBell := Notifier{Mode: NotifyBell, Term: TermGeneric, Mux: MuxTmux}
	if got := tmuxBell.Sequences(doneEv); got != "\x07" {
		t.Fatalf("tmux BEL must stay raw \\x07, got %q", got)
	}

	// Off emits nothing, even for alerts and Reset.
	off := Notifier{Mode: NotifyOff, Term: TermGhostty}
	if off.Sequences(doneEv) != "" || off.Reset("claude", "") != "" {
		t.Fatalf("NotifyOff must emit nothing")
	}
}
