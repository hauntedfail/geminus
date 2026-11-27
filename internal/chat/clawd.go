package chat

// Clawd mode: a visual masquerade layer that makes geminus's terminal
// interface closely resemble Claude Code while keeping every underlying
// geminus capability (Honcho transport, remote personas/harnesses, signed
// prompts, permission asks, Post-Human editing, and slash commands) intact.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hauntedfail/geminus/internal/splash"
)

const (
	clawdEnv            = "GEMINUS_CLAWD"
	clawdAppName        = "Claude Code"
	clawdDisplayVersion = "v2.1.308"
	clawdPlanLabel      = "Claude Max"
	clawdBannerRowCount = splash.ClawdRows + 1
)

var (
	clawdSpinner = []rune("·✢✳✶✻✽")
	clawdVerbs   = []string{"Thinking", "Cogitating", "Pondering", "Working", "Synthesizing", "Composing"}
)

// initClawd applies per-run Clawd mode overrides from CLI flags or GEMINUS_CLAWD.
func (a *App) initClawd(req request) {
	switch {
	case req.clawd:
		on := true
		a.clawdOverride, a.clawdFlag, a.clawdEnvSet = &on, "--clawd", false
	case req.noClawd:
		off := false
		a.clawdOverride, a.clawdFlag, a.clawdEnvSet = &off, "--no-clawd", false
	default:
		if raw := strings.TrimSpace(os.Getenv(clawdEnv)); raw != "" {
			if on, err := parseBoolToggle(raw); err == nil {
				a.clawdOverride, a.clawdFlag, a.clawdEnvSet = &on, "", true
			}
		}
	}
}

// clawdEnabled reports whether Clawd visual mode is active for this session.
func (a *App) clawdEnabled() bool {
	if a.clawdOverride != nil {
		return *a.clawdOverride
	}
	return a.defaults.ClawdEnabled()
}

// clawdSource describes where the active Clawd mode setting comes from.
func (a *App) clawdSource() string {
	if a.clawdOverride != nil {
		if a.clawdFlag != "" {
			return "CLI flag"
		}
		if a.clawdEnvSet {
			return clawdEnv
		}
		return "session"
	}
	if a.defaults.Clawd != nil {
		return "Honcho"
	}
	return "default"
}

// slashClawd toggles or sets Clawd mode (Claude Code visual interface),
// persisted in the owner's signed Honcho defaults.
func (a *App) slashClawd(arg string) error {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	var next bool
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "":
		next = !a.clawdEnabled()
	case "on", "true", "yes", "1":
		next = true
	case "off", "false", "no", "0":
		next = false
	case "status":
		a.Out.line(fmt.Sprintf("clawd mode: %s (%s)", onOff(a.clawdEnabled()), a.clawdSource()))
		return nil
	default:
		return errors.New("usage: /clawd [on|off|status]")
	}
	d := a.cloneDefaults()
	d.Clawd = &next
	if err := a.saveDefaults(d); err != nil {
		return err
	}
	// Clear any per-run override so interactive toggling takes effect immediately.
	a.clawdOverride, a.clawdFlag, a.clawdEnvSet = nil, "", false
	a.sync()
	if next {
		a.Out.line(a.Out.dim("Clawd mode: on"))
	} else {
		a.Out.line("Clawd mode: off (saved in Honcho)")
	}
	return nil
}

// clawdModelLabel returns a Claude Code style model display label without
// exposing non-Claude backend model names or harness identifiers.
func clawdModelLabel(v view) string {
	raw := strings.ToLower(strings.TrimSpace(v.model))
	if raw == "" {
		raw = strings.ToLower(strings.TrimSpace(v.defaultModel))
	}
	switch {
	case strings.Contains(raw, "opus"):
		return "Opus 4.6"
	case strings.Contains(raw, "haiku"):
		return "Haiku 4.5"
	case strings.Contains(raw, "sonnet"):
		return "Sonnet 4.6"
	default:
		return "Sonnet 4.6"
	}
}

// clawdDirLabel formats the conversation directory in Claude Code's compact
// home-shortened style (e.g. ~/projekt/go/geminus).
func clawdDirLabel(v view) string {
	dir := strings.TrimSpace(v.cwd)
	if dir == "" {
		dir = strings.TrimSpace(v.home)
	}
	if dir == "" {
		return "~"
	}
	for _, prefix := range []string{"/Users/", "/home/"} {
		if strings.HasPrefix(dir, prefix) {
			rest := dir[len(prefix):]
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				return "~" + rest[i:]
			}
			return "~"
		}
	}
	// Windows-style C:\Users\<name>\...
	if len(dir) >= 10 && dir[1] == ':' && strings.EqualFold(dir[2:9], `\Users\`) {
		rest := dir[9:]
		if i := strings.IndexByte(rest, '\\'); i >= 0 {
			return "~" + rest[i:]
		}
		return "~"
	}
	return dir
}

// clawdBannerRows draws the compact Claude Code welcome header with Clawd
// standing beside the app name, model/plan, and working directory.
func clawdBannerRows(v view, mode splash.ColorMode, pal palette, width int) []string {
	mascot := splash.ClawdRowsStrings(mode)
	dirRoom := max(width-splash.ClawdCols-4, 12)
	info := [splash.ClawdRows]string{
		pal.Bold(clawdAppName) + " " + pal.Dim(clawdDisplayVersion),
		pal.Dim(clawdModelLabel(v) + " · " + clawdPlanLabel),
		pal.Dim(clipLeft(clawdDirLabel(v), dirRoom)),
	}
	out := make([]string, 0, clawdBannerRowCount)
	for i := 0; i < splash.ClawdRows; i++ {
		out = append(out, clipStyled(" "+mascot[i]+"   "+info[i], width))
	}
	return append(out, "")
}

// renderClawdHeader draws a minimal dim scroll indicator when scrolled up in
// Clawd mode. When at the bottom (scroll == 0), Clawd mode shows no sticky bar.
func renderClawdHeader(in headerInput) string {
	width := max(in.cols-1, 24)
	pal := in.pal
	badge := fmt.Sprintf("↑ %dL", in.scroll)
	if in.atTop {
		badge = "TOP · " + badge
	}
	fill := max(width-textWidth(badge)-1, 0)
	if fill == 0 {
		return pal.Dim(clipWidth(badge, width))
	}
	return pal.Dim(strings.Repeat("─", fill)) + " " + pal.Dim(badge)
}

// renderClawd renders the bottom input area in Claude Code's signature layout:
// coral sparkle activity indicator, full-width dim horizontal rules around the
// ❯ prompt, and a minimal status/shortcut footer with no geminus branding.
func renderClawd(in frameInput) frame {
	width := max(in.cols-1, 24)
	pal, v := in.pal, in.v
	rule := pal.Dim(strings.Repeat("─", width))
	var f frame

	if in.tail != "" {
		f.rows = append(f.rows, wrapText(in.tail, width)...)
	}
	f.rows = append(f.rows, "")
	if row := clawdActivity(in, width); row != "" {
		f.rows = append(f.rows, row)
	}

	f.rows = append(f.rows, rule)
	if v.ask != "" {
		f.rows = append(f.rows, " "+pal.Bold(clipWidth(v.ask, max(width-2, 8))))
	}

	inner := max(width-2, 8) // "❯ " prefix
	lines, curRow, curCol := layoutInputSelected(in.text, in.pos, inner, pal, in.selStart, in.selEnd, in.postHuman && in.selActive)
	if most := max(1, min(10, in.rows/3)); len(lines) > most {
		first := 0
		if curRow >= most {
			first = curRow - most + 1
		}
		lines, curRow = lines[first:first+most], curRow-first
	}

	placeholder := ""
	if len(in.text) == 0 {
		placeholder = `Try "explain this codebase"`
		switch {
		case v.ask != "":
			placeholder = "Enter a number or name"
		case v.persona == "":
			placeholder = "Select a session to begin"
		}
		placeholder = clipWidth(placeholder, inner)
	}
	for i, line := range lines {
		mark := "  "
		if i == 0 {
			mark = pal.Bold("❯") + " "
		}
		shown := line
		if placeholder != "" {
			shown = pal.Dim(placeholder)
		}
		f.rows = append(f.rows, mark+shown)
	}
	f.curRow, f.curCol = len(f.rows)-len(lines)+curRow, 2+curCol
	f.rows = append(f.rows, rule)
	f.rows = append(f.rows, clawdHintRows(in, width)...)
	return f
}

// clawdActivity renders Claude Code's coral sparkle spinner and thinking verb
// above the prompt rule while awaiting a response.
func clawdActivity(in frameInput, width int) string {
	v, pal := in.v, in.pal
	if v.waiting == 0 {
		return ""
	}
	took := in.now.Sub(v.since)
	secs := max(int(took.Seconds()), 0)
	mark := pal.SGR(colorCoral, string(clawdSpinner[in.spin%len(clawdSpinner)]))

	verb := "Thinking…"
	if v.running {
		verb = clawdVerbs[(secs/4)%len(clawdVerbs)] + "…"
	} else if v.slow > 0 && took > v.slow {
		verb = "Connecting…"
	}

	detail := fmt.Sprintf("(%s · esc to interrupt)", elapsed(took))
	if v.waiting > 1 {
		detail = fmt.Sprintf("(%s · %d queued)", elapsed(took), v.waiting)
	}
	plain := verb + " " + detail
	if textWidth(plain) > width-3 {
		return " " + mark + " " + pal.SGR(colorCoral, clipWidth(plain, max(width-3, 1)))
	}
	return " " + mark + " " + pal.SGR(colorCoral, verb) + " " + pal.Dim(detail)
}

// clawdModeBadge maps geminus permission modes onto Claude Code footer badges.
func clawdModeBadge(v view, pal palette) (plain, styled string) {
	mode := strings.ToLower(strings.TrimSpace(v.mode))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(v.defaultMode))
	}
	switch mode {
	case "accept-edits":
		plain = "⏵⏵ accept edits on"
		return plain, pal.SGR(colorMagenta, plain)
	case "bypass":
		plain = "⏵⏵ bypass permissions on"
		return plain, pal.SGR(colorRed, plain)
	case "read-only":
		plain = "⏸ plan mode on"
		return plain, pal.SGR(colorCyan, plain)
	case "auto":
		plain = "⏵⏵ auto-accept on"
		return plain, pal.SGR(colorGreen, plain)
	default:
		return "", ""
	}
}

// clawdFooterRow builds the single-line bottom footer under the lower rule.
func clawdFooterRow(in frameInput, width int) string {
	v, pal := in.v, in.pal
	var leftPlain, leftStyled string
	switch {
	case v.link != "":
		leftPlain = "✗ Connection error"
		leftStyled = pal.SGR(colorRed, leftPlain)
	case v.persona != "" && !v.reported:
		leftPlain = "○ Offline"
		leftStyled = pal.SGR(colorYellow, leftPlain)
	case in.postHuman:
		label, color := "-- INSERT --", colorGreen
		switch in.helixMode {
		case helixNormal:
			label, color = "-- NORMAL --", colorYellow
		case helixSelect:
			label, color = "-- VISUAL --", colorMagenta
		}
		leftPlain = label
		leftStyled = pal.SGR("1;"+color, label)
		if in.helixPending != "" {
			leftPlain += " " + in.helixPending
			leftStyled += " " + pal.SGR(colorCoral, in.helixPending)
		}
	default:
		leftPlain, leftStyled = clawdModeBadge(v, pal)
		if leftPlain == "" {
			leftPlain = "? for shortcuts"
			leftStyled = pal.Dim(leftPlain)
		}
	}

	var rightPlain, rightStyled string
	switch {
	case v.update != "":
		rightPlain = "Update available"
		rightStyled = pal.Dim(rightPlain)
	case leftPlain == "? for shortcuts":
		rightPlain = clawdModelLabel(v)
		rightStyled = pal.Dim(rightPlain)
	default:
		rightPlain = "? for shortcuts"
		rightStyled = pal.Dim(rightPlain)
	}

	room := max(width-2, 1)
	if textWidth(leftPlain) >= room {
		return "  " + pal.Dim(clipWidth(leftPlain, room))
	}
	gap := room - textWidth(leftPlain) - textWidth(rightPlain)
	if rightPlain == "" || gap < 2 {
		return "  " + leftStyled
	}
	return "  " + leftStyled + strings.Repeat(" ", gap) + rightStyled
}

// clawdHintRows builds the footer or expanded slash/shortcut overlay in Clawd mode.
func clawdHintRows(in frameInput, width int) []string {
	v, pal := in.v, in.pal
	one := func(s string) []string { return []string{"  " + pal.Dim(clipWidth(s, max(width-2, 1)))} }
	if in.notice != "" {
		return one(in.notice)
	}
	if v.ask != "" {
		return one("Enter to confirm · Ctrl-C to cancel")
	}
	if in.shortcuts {
		return clawdShortcutRows(in, width)
	}
	if _, _, ok := shellLine(string(in.text)); ok {
		return one("shell directory command")
	}
	if word, ok := slashWord(in.text); ok {
		if strings.HasPrefix(word, "//") {
			return one("sent as " + word[1:])
		}
		found := slashMatches(word)
		if len(found) == 0 {
			return one("slash command")
		}
		most := max(1, min(6, in.rows/4))
		var rows []string
		for n, i := range found {
			if n == most {
				rows = append(rows, "  "+pal.Dim(fmt.Sprintf("… %d more · Tab completes", len(found)-most)))
				break
			}
			c := slashCommands[i]
			left := fmt.Sprintf("%-18s", strings.TrimSpace(c.name+" "+c.args))
			rows = append(rows, "  "+pal.SGR(colorCoral, left)+" "+pal.Dim(clipWidth(c.what, max(width-22, 0))))
		}
		return rows
	}
	return []string{clawdFooterRow(in, width)}
}

// clawdShortcutRows draws the ? shortcut overlay without geminus branding.
func clawdShortcutRows(in frameInput, width int) []string {
	pal := in.pal
	items := [][2]string{
		{"?", "hide shortcuts"},
		{"/", "commands"},
		{"Enter", "send message"},
		{"Alt+Enter", "newline"},
		{"↑ ↓ / PgUp PgDn", "scroll transcript"},
		{"Ctrl-P / Ctrl-N", "prompt history"},
		{"Tab", "complete command"},
		{"Ctrl-A / Ctrl-E", "line start / end"},
		{"Ctrl-U / Ctrl-K", "clear before / after"},
		{"Ctrl-W", "delete word"},
		{"Ctrl-L", "redraw"},
		{"Ctrl-C", "clear input; twice to exit"},
		{"Ctrl-D", "exit on empty input"},
		{"Esc", "close"},
	}
	rows := []string{"  " + pal.Bold("Shortcuts") + pal.Dim("  ·  ? or Esc to close")}
	room := max(width-2, 20)
	for _, item := range items {
		left := item[0]
		line := left + "  " + item[1]
		if textWidth(line) > room {
			line = clipWidth(line, room)
		}
		rows = append(rows, "  "+pal.SGR(colorCoral, left)+pal.Dim(strings.TrimPrefix(line, left)))
	}
	return rows
}

// printTransitionStatus prints a concise session status line after switching
// persona, harness, or conversation, keeping the transcript disguised in Clawd mode.
func (a *App) printTransitionStatus(action string) {
	if a.clawdEnabled() {
		a.Out.line(a.Out.dim("  ⎿  " + action))
		return
	}
	a.Out.line(a.status())
}

// writeClawdReply prints an agent response chunk in Claude Code's bullet style:
// the first line of a turn starts with "● " and subsequent lines align with
// two leading spaces, safe across streamed chunks.
func (a *App) writeClawdReply(turn, text string, final bool) {
	if text != "" {
		var out strings.Builder
		atStart := a.Out.atLineStart()
		started := a.shownTurn[turn]
		for len(text) > 0 {
			line, rest, nl := strings.Cut(text, "\n")
			text = rest
			if !started {
				a.Out.line("")
				a.shownTurn[turn] = true
				started = true
				out.WriteString(a.Out.bold("●") + " ")
				atStart = false
			} else if atStart && (line != "" || nl) {
				out.WriteString("  ")
				atStart = false
			}
			out.WriteString(line)
			if nl {
				out.WriteByte('\n')
				atStart = true
			}
		}
		if out.Len() > 0 {
			a.Out.write(out.String())
		}
	}
	if final && a.live != nil {
		a.Out.endLine()
	} else if final {
		a.Out.line("")
	}
}
