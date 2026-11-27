package tui

// Terminal notifications and agent status relay over standard VT escape
// sequences (Ghostty OSC 777 / OSC 9;4, Orca OSC 9999, Kitty OSC 99, iTerm2
// OSC 9, and BEL fallback), with tmux and GNU screen DCS passthrough.
// Focus handling is deliberately delegated to the terminal emulator and OS.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// NotifyMode chooses how geminus emits terminal notifications and status.
type NotifyMode string

const (
	NotifyAuto    NotifyMode = "auto"
	NotifyGhostty NotifyMode = "ghostty"
	NotifyOrca    NotifyMode = "orca"
	NotifyOSC9    NotifyMode = "osc9"
	NotifyOSC99   NotifyMode = "osc99"
	NotifyBell    NotifyMode = "bell"
	NotifyAll     NotifyMode = "all"
	NotifyOff     NotifyMode = "off"
)

// NotifyModes lists the canonical modes in display order.
var NotifyModes = []NotifyMode{
	NotifyAuto,
	NotifyGhostty,
	NotifyOrca,
	NotifyOSC9,
	NotifyOSC99,
	NotifyBell,
	NotifyAll,
	NotifyOff,
}

// ParseNotifyMode normalizes a user-supplied notification mode or alias.
func ParseNotifyMode(raw string) (NotifyMode, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto", "on", "true", "yes", "1", "default":
		return NotifyAuto, true
	case "ghostty", "osc777", "777":
		return NotifyGhostty, true
	case "orca", "osc9999", "9999":
		return NotifyOrca, true
	case "osc9", "9", "iterm", "iterm2":
		return NotifyOSC9, true
	case "osc99", "99", "kitty":
		return NotifyOSC99, true
	case "bell", "bel":
		return NotifyBell, true
	case "all", "both":
		return NotifyAll, true
	case "off", "false", "no", "0", "none":
		return NotifyOff, true
	default:
		return "", false
	}
}

// TerminalKind identifies the host terminal emulator from environment variables.
type TerminalKind string

const (
	TermGeneric         TerminalKind = "generic"
	TermGhostty         TerminalKind = "ghostty"
	TermOrca            TerminalKind = "orca"
	TermKitty           TerminalKind = "kitty"
	TermWezTerm         TerminalKind = "wezterm"
	TermITerm2          TerminalKind = "iterm2"
	TermWindowsTerminal TerminalKind = "windows-terminal"
)

// MuxKind identifies an enclosing terminal multiplexer that needs DCS passthrough.
type MuxKind string

const (
	MuxNone   MuxKind = "none"
	MuxTmux   MuxKind = "tmux"
	MuxScreen MuxKind = "screen"
)

// DetectTerminal inspects environment variables to identify the terminal emulator.
func DetectTerminal(getenv func(string) string) TerminalKind {
	if getenv == nil {
		return TermGeneric
	}
	prog := strings.ToLower(strings.TrimSpace(getenv("TERM_PROGRAM")))
	term := strings.ToLower(strings.TrimSpace(getenv("TERM")))
	switch {
	case prog == "orca" || getenv("ORCA_PANE_KEY") != "" || getenv("ORCA_TERMINAL_HANDLE") != "" || getenv("ORCA_APP_VERSION") != "":
		return TermOrca
	case prog == "ghostty" || term == "xterm-ghostty" || getenv("GHOSTTY_RESOURCES_DIR") != "":
		return TermGhostty
	case prog == "kitty" || term == "xterm-kitty" || getenv("KITTY_WINDOW_ID") != "":
		return TermKitty
	case prog == "wezterm" || getenv("WEZTERM_PANE") != "":
		return TermWezTerm
	case prog == "iterm.app" || getenv("ITERM_SESSION_ID") != "":
		return TermITerm2
	case getenv("WT_SESSION") != "":
		return TermWindowsTerminal
	default:
		return TermGeneric
	}
}

// DetectMux reports whether geminus is running inside tmux or GNU screen.
func DetectMux(getenv func(string) string) MuxKind {
	if getenv == nil {
		return MuxNone
	}
	if getenv("TMUX") != "" {
		return MuxTmux
	}
	if getenv("STY") != "" {
		return MuxScreen
	}
	return MuxNone
}

// WrapPassthrough wraps an OSC escape sequence for tmux or GNU screen when needed.
// Raw BEL characters must not be wrapped so multiplexer monitor-bell still works.
func WrapPassthrough(seq string, mux MuxKind) string {
	if seq == "" {
		return ""
	}
	switch mux {
	case MuxTmux:
		return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	case MuxScreen:
		return "\x1bP" + seq + "\x1b\\"
	default:
		return seq
	}
}

// EventState is the normalized lifecycle state of the connected remote agent.
type EventState string

const (
	EventBoundary EventState = "boundary" // idle / session boundary (no alert)
	EventWorking  EventState = "working"  // agent turn or tool running
	EventBlocked  EventState = "blocked"  // permission ask requiring user action
	EventWaiting  EventState = "waiting"  // waiting on user input
	EventDone     EventState = "done"     // turn finished
	EventError    EventState = "error"    // turn failed, refused, or timed out
)

// AgentEvent describes one remote agent lifecycle event to relay to the terminal.
type AgentEvent struct {
	State       EventState
	AgentType   string
	Title       string
	Body        string
	Model       string
	Prompt      string
	ToolName    string
	ToolInput   string
	LastMessage string
	Interrupted bool
	Alert       bool
	// KeepWorking keeps the terminal progress indicator active after an alert
	// when another prompt in the same conversation is still running.
	KeepWorking bool
}

// Notifier formats terminal escape sequences for remote agent events.
type Notifier struct {
	Mode NotifyMode
	Term TerminalKind
	Mux  MuxKind
}

// Sequences returns the terminal escape sequence bytes to emit for ev.
func (n Notifier) Sequences(ev AgentEvent) string {
	mode := n.Mode
	if mode == "" {
		mode = NotifyAuto
	}
	if mode == NotifyOff {
		return ""
	}
	var out strings.Builder
	writeOSC := func(seq string) {
		if seq != "" {
			out.WriteString(WrapPassthrough(seq, n.Mux))
		}
	}
	writeProgress := func() {
		writeOSC(FormatProgress(ev))
	}

	switch mode {
	case NotifyOrca:
		writeOSC(FormatOSC9999(ev))
	case NotifyGhostty:
		writeProgress()
		if ev.Alert {
			writeOSC(FormatOSC777(ev.Title, ev.Body))
		}
	case NotifyOSC9:
		writeProgress()
		if ev.Alert {
			writeOSC(FormatOSC9(ev.Title, ev.Body))
		}
	case NotifyOSC99:
		if ev.Alert {
			writeOSC(FormatOSC99(ev.Title, ev.Body))
		}
	case NotifyBell:
		if ev.Alert {
			out.WriteByte('\x07')
		}
	case NotifyAll:
		writeOSC(FormatOSC9999(ev))
		writeProgress()
		if ev.Alert {
			writeOSC(FormatOSC777(ev.Title, ev.Body))
		}
	default: // NotifyAuto
		switch n.Term {
		case TermOrca:
			writeOSC(FormatOSC9999(ev))
		case TermGhostty, TermWezTerm:
			writeProgress()
			if ev.Alert {
				writeOSC(FormatOSC777(ev.Title, ev.Body))
			}
		case TermKitty:
			if ev.Alert {
				writeOSC(FormatOSC99(ev.Title, ev.Body))
			}
		case TermITerm2:
			writeProgress()
			if ev.Alert {
				writeOSC(FormatOSC9(ev.Title, ev.Body))
			}
		case TermWindowsTerminal:
			writeProgress()
			if ev.Alert {
				out.WriteByte('\x07')
			}
		default: // TermGeneric (e.g., SSH without TERM_PROGRAM, Apple Terminal)
			// Emit OSC 9999 so Orca captures full agent status even over SSH
			// when TERM_PROGRAM is not forwarded; non-Orca VT terminals ignore
			// unknown OSC numbers per ECMA-48. BEL provides universal alerts.
			writeOSC(FormatOSC9999(ev))
			if ev.Alert {
				out.WriteByte('\x07')
			}
		}
	}
	return out.String()
}

// Reset returns cleanup sequences to emit when leaving the terminal so no
// progress bar or working status is left behind after exit.
func (n Notifier) Reset(agentType, model string) string {
	return n.Sequences(AgentEvent{
		State:     EventBoundary,
		AgentType: agentType,
		Model:     model,
	})
}

// FormatOSC777 formats a Ghostty / WezTerm / VTE desktop notification.
func FormatOSC777(title, body string) string {
	t := cleanOSCText(title, 80, true)
	if t == "" {
		t = "geminus"
	}
	b := cleanOSCText(body, 200, false)
	if b == "" {
		b = "Attention needed"
	}
	return "\x1b]777;notify;" + t + ";" + b + "\x1b\\"
}

// FormatOSC9 formats an iTerm2 / ConEmu desktop notification.
func FormatOSC9(title, body string) string {
	t := cleanOSCText(title, 80, false)
	b := cleanOSCText(body, 200, false)
	msg := b
	if t != "" && b != "" {
		msg = t + ": " + b
	} else if t != "" {
		msg = t
	}
	if msg == "" {
		msg = "geminus"
	}
	// Never collide with OSC 9;4 progress sequences.
	if strings.HasPrefix(msg, "4;") {
		msg = "geminus: " + msg
	}
	return "\x1b]9;" + msg + "\x1b\\"
}

// FormatOSC99 formats a Kitty desktop notification (title + body).
func FormatOSC99(title, body string) string {
	t := cleanOSCText(title, 80, false)
	if t == "" {
		t = "geminus"
	}
	b := cleanOSCText(body, 200, false)
	if b == "" {
		return "\x1b]99;i=geminus:d=1:p=title;" + t + "\x1b\\"
	}
	return "\x1b]99;i=geminus:d=0:p=title;" + t + "\x1b\\\x1b]99;i=geminus:d=1:p=body;" + b + "\x1b\\"
}

// FormatProgress formats an OSC 9;4 tab progress sequence for ev.
func FormatProgress(ev AgentEvent) string {
	switch ev.State {
	case EventWorking:
		return "\x1b]9;4;3\x1b\\" // indeterminate progress
	case EventBlocked, EventWaiting:
		return "\x1b]9;4;4;100\x1b\\" // paused / attention needed
	case EventError:
		if ev.KeepWorking {
			return "\x1b]9;4;3\x1b\\"
		}
		return "\x1b]9;4;2;100\x1b\\" // error
	case EventDone:
		if ev.KeepWorking {
			return "\x1b]9;4;3\x1b\\"
		}
		return "\x1b]9;4;0\x1b\\" // clear
	case EventBoundary:
		return "\x1b]9;4;0\x1b\\" // clear
	default:
		return ""
	}
}

type orcaStatusPayload struct {
	State                string `json:"state"`
	AgentType            string `json:"agentType,omitempty"`
	Model                string `json:"model,omitempty"`
	Prompt               string `json:"prompt,omitempty"`
	ToolName             string `json:"toolName,omitempty"`
	ToolInput            string `json:"toolInput,omitempty"`
	LastAssistantMessage string `json:"lastAssistantMessage,omitempty"`
	Interrupted          bool   `json:"interrupted,omitempty"`
	SessionBoundary      bool   `json:"sessionBoundary,omitempty"`
}

// FormatOSC9999 formats an Orca agent-status OSC 9999 payload matching
// Orca's src/shared/agent-status/contracts/agent-status-types.ts contract.
func FormatOSC9999(ev AgentEvent) string {
	p := orcaStatusPayload{
		AgentType: cleanOSCText(ev.AgentType, 40, false),
		Model:     cleanOSCText(ev.Model, 60, false),
		Prompt:    cleanOSCText(ev.Prompt, 120, false),
	}
	if p.AgentType == "" {
		p.AgentType = "geminus"
	}
	switch ev.State {
	case EventWorking:
		p.State = "working"
		p.ToolName = cleanOSCText(ev.ToolName, 60, false)
		p.ToolInput = cleanOSCText(ev.ToolInput, 160, false)
	case EventBlocked:
		p.State = "blocked"
		p.ToolName = cleanOSCText(ev.ToolName, 60, false)
		p.ToolInput = cleanOSCText(ev.ToolInput, 160, false)
	case EventWaiting:
		p.State = "waiting"
		p.ToolName = cleanOSCText(ev.ToolName, 60, false)
		p.ToolInput = cleanOSCText(ev.ToolInput, 160, false)
	case EventDone:
		p.State = "done"
		p.LastAssistantMessage = cleanOSCText(ev.LastMessage, 500, false)
	case EventError:
		p.State = "done"
		p.Interrupted = true
		p.LastAssistantMessage = cleanOSCText(ev.LastMessage, 500, false)
	case EventBoundary:
		p.State = "done"
		p.SessionBoundary = true
	default:
		return ""
	}
	if ev.Interrupted && p.State == "done" {
		p.Interrupted = true
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("\x1b]9999;%s\x1b\\", raw)
}

// cleanOSCText strips control characters, collapses whitespace to one line,
// optionally replaces semicolons (for OSC 777 title), and truncates by runes.
func cleanOSCText(s string, maxRunes int, stripSemicolon bool) string {
	s = OneLine(s)
	if stripSemicolon {
		s = strings.ReplaceAll(s, ";", ",")
	}
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return string(r[:maxRunes])
	}
	return string(r[:maxRunes-1]) + "…"
}
