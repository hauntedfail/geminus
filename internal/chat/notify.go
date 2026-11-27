package chat

// Terminal notification and agent-status proxy relay for geminus.
// As a proxy screen, geminus forwards remote agent lifecycle events (working,
// tool use, permission ask, waiting, done, failure/refusal, session boundary)
// directly to the local terminal via standard VT escape sequences (Ghostty
// OSC 777 + OSC 9;4, Orca OSC 9999, Kitty OSC 99, iTerm2 OSC 9, BEL), letting
// the terminal emulator and OS handle focus and delivery policy.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hauntedfail/geminus/internal/tui"
)

const notifyEnv = "GEMINUS_NOTIFY"

// initNotify applies per-run notification overrides from CLI flags or GEMINUS_NOTIFY.
func (a *App) initNotify(req request) {
	switch {
	case req.noNotify:
		m := tui.NotifyOff
		a.notifyOverride, a.notifyFlag, a.notifyEnvSet = &m, "--no-notify", false
	case req.notify != "":
		if m, ok := tui.ParseNotifyMode(req.notify); ok {
			a.notifyOverride, a.notifyFlag, a.notifyEnvSet = &m, "--notify", false
		}
	default:
		if raw := strings.TrimSpace(a.envValue(notifyEnv)); raw != "" {
			if m, ok := tui.ParseNotifyMode(raw); ok {
				a.notifyOverride, a.notifyFlag, a.notifyEnvSet = &m, "", true
			}
		}
	}
}

func (a *App) envValue(key string) string {
	if a.notifyEnvFn != nil {
		return a.notifyEnvFn(key)
	}
	return os.Getenv(key)
}

func (a *App) notifyMode() tui.NotifyMode {
	if a.notifyOverride != nil {
		return *a.notifyOverride
	}
	if m, ok := tui.ParseNotifyMode(a.defaults.NotifyMode()); ok {
		return m
	}
	return tui.NotifyAuto
}

func (a *App) notifySource() string {
	if a.notifyOverride != nil {
		if a.notifyFlag != "" {
			return "CLI flag"
		}
		if a.notifyEnvSet {
			return notifyEnv
		}
		return "session"
	}
	if strings.TrimSpace(a.defaults.Notify) != "" {
		return "Honcho"
	}
	return "default"
}

func (a *App) terminalNotifier() tui.Notifier {
	return tui.Notifier{
		Mode: a.notifyMode(),
		Term: tui.DetectTerminal(a.envValue),
		Mux:  tui.DetectMux(a.envValue),
	}
}

// slashNotify shows or updates the terminal notification mode, persisted in Honcho.
func (a *App) slashNotify(arg string) error {
	raw := strings.ToLower(strings.TrimSpace(arg))
	if raw == "" || raw == "status" {
		n := a.terminalNotifier()
		a.Out.line(fmt.Sprintf("notify: %s (%s · terminal: %s, mux: %s)", n.Mode, a.notifySource(), n.Term, n.Mux))
		a.Out.line(a.Out.dim("  modes: auto, ghostty (osc777), orca (osc9999), osc9, osc99, bell, all, off"))
		return nil
	}
	mode, ok := tui.ParseNotifyMode(raw)
	if !ok {
		return errors.New("usage: /notify [auto|ghostty|orca|osc9|osc99|bell|all|off|status]")
	}
	d := a.cloneDefaults()
	if mode == tui.NotifyAuto {
		d.Notify = ""
	} else {
		d.Notify = string(mode)
	}
	if err := a.saveDefaults(d); err != nil {
		return err
	}
	a.notifyOverride, a.notifyFlag, a.notifyEnvSet = nil, "", false
	a.lastNotifySig = ""
	a.Out.line(fmt.Sprintf("notify: %s (saved in Honcho)", mode))
	a.notifySyncState()
	return nil
}

func (a *App) agentIdentity() (agentType, title, model string) {
	agentType = "geminus"
	if a.harness != nil && strings.TrimSpace(a.harness.ID) != "" {
		agentType = strings.TrimSpace(a.harness.ID)
	}
	if a.tuning.Model != "" {
		model = a.tuning.Model
	} else if a.harness != nil {
		model = a.harness.DefaultFor("model")
	}
	if a.clawdEnabled() {
		title = clawdAppName
		return agentType, title, model
	}
	switch {
	case a.cur != nil && a.harness != nil:
		title = fmt.Sprintf("%s · %s", oneLine(a.cur.Name), a.harnessDisplay(*a.harness))
	case a.cur != nil:
		title = oneLine(a.cur.Name)
	default:
		title = "geminus"
	}
	return agentType, title, model
}

func (a *App) currentPrompt(turn string) string {
	if turn != "" && a.turnPrompt != nil {
		if p := a.turnPrompt[turn]; p != "" {
			return p
		}
	}
	if a.title != "" {
		return a.title
	}
	return ""
}

func (a *App) emitAgentEvent(ev tui.AgentEvent) {
	if a.live == nil {
		return
	}
	n := a.terminalNotifier()
	if n.Mode == tui.NotifyOff {
		return
	}
	sig := ""
	if !ev.Alert {
		sig = fmt.Sprintf("%s|%s|%s|%s|%s|%s|%t",
			ev.State, ev.AgentType, ev.Model, ev.Prompt, ev.ToolName, ev.ToolInput, ev.KeepWorking)
		if sig == a.lastNotifySig {
			return
		}
	}
	seq := n.Sequences(ev)
	if seq != "" {
		a.live.emit(seq)
	}
	a.lastNotifySig = sig
}

// notifyResetSequence returns the cleanup sequence for live.close().
func (a *App) notifyResetSequence() string {
	n := a.terminalNotifier()
	if n.Mode == tui.NotifyOff {
		return ""
	}
	agentType, _, model := a.agentIdentity()
	return n.Reset(agentType, model)
}

// notifyResetTurnState clears per-session notification deduplication maps.
func (a *App) notifyResetTurnState() {
	a.notifiedTurn = map[string]bool{}
	a.notifiedAsk = map[string]bool{}
	a.turnLastOut = map[string]string{}
	a.lastNotifySig = ""
}

// notifyBoundary reports an idle/session boundary (startup, switch, /new, idle /resume).
func (a *App) notifyBoundary() {
	agentType, title, model := a.agentIdentity()
	a.emitAgentEvent(tui.AgentEvent{
		State:     tui.EventBoundary,
		AgentType: agentType,
		Title:     title,
		Model:     model,
	})
}

// notifySyncState emits the current non-alerting status after startup, /resume, or /notify change.
func (a *App) notifySyncState() {
	if a.pendingAsk != nil {
		a.notifyBlocked(a.pendingAsk)
		return
	}
	if len(a.waiting) > 0 {
		for id := range a.waiting {
			a.notifyWorking(id, "", "")
			return
		}
	}
	a.notifyBoundary()
}

// notifyWorking reports that the remote agent is actively working (optionally on a tool).
func (a *App) notifyWorking(turn, toolName, toolInput string) {
	agentType, title, model := a.agentIdentity()
	a.emitAgentEvent(tui.AgentEvent{
		State:     tui.EventWorking,
		AgentType: agentType,
		Title:     title,
		Model:     model,
		Prompt:    a.currentPrompt(turn),
		ToolName:  toolName,
		ToolInput: toolInput,
	})
}

// notifyBlocked relays a remote permission/confirmation ask to the terminal.
func (a *App) notifyBlocked(ask *pendingAsk) {
	if ask == nil {
		return
	}
	if a.notifiedAsk == nil {
		a.notifiedAsk = map[string]bool{}
	}
	if ask.Request != "" && a.notifiedAsk[ask.Request] {
		return
	}
	if ask.Request != "" {
		a.notifiedAsk[ask.Request] = true
	}
	agentType, title, model := a.agentIdentity()
	tool := oneLine(ask.Tool)
	detail := oneLine(ask.Detail)
	question := oneLine(ask.Question)
	input := detail
	if input == "" {
		input = question
	}
	body := "Permission required"
	if tool != "" && question != "" {
		body = fmt.Sprintf("Permission [%s]: %s", tool, question)
	} else if tool != "" && detail != "" {
		body = fmt.Sprintf("Permission [%s]: %s", tool, detail)
	} else if tool != "" {
		body = fmt.Sprintf("Permission required: %s", tool)
	} else if question != "" {
		body = fmt.Sprintf("Permission: %s", question)
	}
	a.emitAgentEvent(tui.AgentEvent{
		State:     tui.EventBlocked,
		AgentType: agentType,
		Title:     title,
		Body:      body,
		Model:     model,
		Prompt:    a.currentPrompt(ask.Turn),
		ToolName:  tool,
		ToolInput: input,
		Alert:     true,
	})
}

// notifyWaiting relays a non-ask waiting state from the remote bridge.
func (a *App) notifyWaiting(turn, text string) {
	agentType, title, model := a.agentIdentity()
	msg := oneLine(text)
	if msg == "" {
		msg = "Waiting for input"
	}
	a.emitAgentEvent(tui.AgentEvent{
		State:     tui.EventWaiting,
		AgentType: agentType,
		Title:     title,
		Body:      msg,
		Model:     model,
		Prompt:    a.currentPrompt(turn),
		ToolInput: msg,
		Alert:     true,
	})
}

// recordTurnOutput retains a short preview of the agent's latest output for notifications.
func (a *App) recordTurnOutput(turn, content string) {
	if turn == "" || content == "" {
		return
	}
	if a.turnLastOut == nil {
		a.turnLastOut = map[string]string{}
	}
	combined := a.turnLastOut[turn] + content
	if len(combined) > 2048 {
		combined = combined[len(combined)-2048:]
	}
	a.turnLastOut[turn] = combined
}

func previewLine(raw, fallback string) string {
	for _, ln := range strings.Split(sanitize(raw), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			return clip(s, 180)
		}
	}
	return fallback
}

// notifyDone relays a completed remote agent turn.
func (a *App) notifyDone(turn, output string) {
	if turn != "" {
		if a.notifiedTurn == nil {
			a.notifiedTurn = map[string]bool{}
		}
		if a.notifiedTurn[turn] {
			return
		}
		a.notifiedTurn[turn] = true
	}
	if output == "" && a.turnLastOut != nil {
		output = a.turnLastOut[turn]
	}
	preview := previewLine(output, "Turn complete")
	agentType, title, model := a.agentIdentity()
	a.emitAgentEvent(tui.AgentEvent{
		State:       tui.EventDone,
		AgentType:   agentType,
		Title:       title,
		Body:        preview,
		Model:       model,
		Prompt:      a.currentPrompt(turn),
		LastMessage: preview,
		Alert:       true,
		KeepWorking: len(a.waiting) > 0,
	})
	delete(a.turnLastOut, turn)
}

// notifyError relays a failed, refused, or timed-out remote agent turn.
func (a *App) notifyError(turn, detail string) {
	if turn != "" {
		if a.notifiedTurn == nil {
			a.notifiedTurn = map[string]bool{}
		}
		if a.notifiedTurn[turn] {
			return
		}
		a.notifiedTurn[turn] = true
	}
	msg := previewLine(detail, "Turn failed")
	agentType, title, model := a.agentIdentity()
	a.emitAgentEvent(tui.AgentEvent{
		State:       tui.EventError,
		AgentType:   agentType,
		Title:       title,
		Body:        msg,
		Model:       model,
		Prompt:      a.currentPrompt(turn),
		LastMessage: msg,
		Interrupted: true,
		Alert:       true,
		KeepWorking: len(a.waiting) > 0,
	})
	delete(a.turnLastOut, turn)
}

func notifyModeDesc(m tui.NotifyMode) string {
	switch m {
	case tui.NotifyAuto:
		return "Auto-detect terminal (Ghostty OSC 777, Orca OSC 9999, BEL fallback)"
	case tui.NotifyGhostty:
		return "Ghostty / WezTerm (OSC 777 notifications + OSC 9;4 progress)"
	case tui.NotifyOrca:
		return "Orca agent status (OSC 9999)"
	case tui.NotifyOSC9:
		return "iTerm2 / OSC 9 notifications + OSC 9;4 progress"
	case tui.NotifyOSC99:
		return "Kitty desktop notifications (OSC 99)"
	case tui.NotifyBell:
		return "Terminal bell (BEL)"
	case tui.NotifyAll:
		return "Orca OSC 9999 + Ghostty OSC 777 + OSC 9;4 progress"
	case tui.NotifyOff:
		return "Disable terminal notifications and status sequences"
	default:
		return string(m)
	}
}

func (a *App) configPickNotify() (string, bool, error) {
	cur := a.notifyMode()
	items := make([]menuItem, 0, len(tui.NotifyModes))
	for _, m := range tui.NotifyModes {
		items = append(items, menuItem{
			value:   string(m),
			label:   string(m),
			desc:    notifyModeDesc(m),
			current: m == cur,
		})
	}
	n := a.terminalNotifier()
	picked, ok := a.live.selectOption(a.ctx, menuConfig{
		title:        "Terminal notifications",
		subtitle:     fmt.Sprintf("Detected terminal: %s · mux: %s", n.Term, n.Mux),
		initialValue: string(cur),
		items:        items,
	})
	if !ok {
		return "Notify unchanged", false, nil
	}
	mode, valid := tui.ParseNotifyMode(picked)
	if !valid {
		return "", false, fmt.Errorf("unknown notify mode %q", picked)
	}
	d := a.cloneDefaults()
	if mode == tui.NotifyAuto {
		d.Notify = ""
	} else {
		d.Notify = string(mode)
	}
	if err := a.saveDefaults(d); err != nil {
		return "", false, err
	}
	a.notifyOverride, a.notifyFlag, a.notifyEnvSet = nil, "", false
	a.lastNotifySig = ""
	a.notifySyncState()
	return fmt.Sprintf("Saved: notify = %s (Honcho)", mode), true, nil
}
