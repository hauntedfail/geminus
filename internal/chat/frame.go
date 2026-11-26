package chat

// What the bottom of the screen looks like: the prompt box and the status
// around it. render is a pure function of what is to be shown, so the whole
// look can be tested without a terminal.

import (
	"fmt"
	"strings"
	"time"
)

// view is everything about the conversation that the screen shows next to
// the prompt box. The App fills it in; the box never reads App state.
type view struct {
	persona string
	local   bool
	harness string // the agent program's name
	// reported is false when no bridge has published from that computer;
	// seen is when it last did.
	reported bool
	seen     time.Time

	// cwd is the directory chosen for the conversation, "" for the one the
	// bridge there uses when none is; home is that one, when the bridge said.
	cwd, home, session string
	model, effort      string
	mode               string
	defaultModel       string
	defaultEffort      string
	defaultMode        string

	// UI display preferences for the prompt box. When uiSet is false (for
	// example in unit tests constructing view directly), all default to true.
	uiSet      bool
	showNode   bool
	showModel  bool
	showEffort bool
	showMode   bool

	// link says why Honcho could not be read; "" when it can.
	link string

	// waiting is how many prompts have no finished answer yet; running
	// reports that the oldest has been picked up, since is when it was
	// sent, rang that the doorbell was rung for it, and slow after how long
	// "not picked up" is spelled out.
	waiting int
	running bool
	since   time.Time
	rang    bool
	slow    time.Duration

	// ask is the question the box is answering instead of taking a prompt.
	ask string

	// update is the version of a geminus release newer than this program.
	update string

	// postHuman enables Helix-style modal editing in the prompt box.
	postHuman bool
}

// frameInput is one moment of the screen's bottom.
type frameInput struct {
	v    view
	text []rune // what is typed
	pos  int    // cursor, as an index into text
	// Post-Human (Helix) modal state, active when postHuman is true.
	postHuman    bool
	helixMode    helixMode
	helixPending string
	selStart     int
	selEnd       int
	selActive    bool
	// tail is output that has not reached a line end yet.
	tail       string
	notice     string
	cols, rows int
	now        time.Time
	spin       int
	pal        palette
}

// frame is the rows to draw and where the cursor goes among them.
type frame struct {
	rows           []string
	curRow, curCol int
}

var spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// slashCommands is what geminus itself answers to; the hints under the box
// come from it. Every other slash line goes to the agent.
var slashCommands = []struct{ name, args, what string }{
	{"/persona", "[NAME]", "pick who to talk to"},
	{"/harness", "[ID]", "pick the agent program there"},
	{"/model", "[NAME]", "model for this conversation"},
	{"/effort", "[LEVEL]", "effort for this conversation"},
	{"/mode", "[MODE]", "permission mode (auto, accept-edits, bypass, read-only, manual)"},
	{"/cwd", "[DIR|save]", "directory there, or save current dir as default"},
	{"/new", "[DIR]", "start a new conversation"},
	{"/sessions", "", "list your conversations"},
	{"/resume", "[ID|N]", "reopen a conversation"},
	{"/ask", "", "reopen the pending permission question"},
	{"/status", "", "show where things stand"},
	{"/update", "[auto on|off]", "install the latest geminus, or toggle auto-update"},
	{"/config", "", "interactive settings menu"},
	{"/post-human", "[on|off]", "toggle Post-Human (Helix) modal editing"},
	{"/default", "…", "show or change your defaults"},
	{"/ui", "…", "customize prompt box display"},
	{"/help", "", "list geminus commands"},
	{"/quit", "", "leave geminus"},
}

// since describes a length of time the way the status line says it.
func since(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func elapsed(d time.Duration) string {
	s := max(int(d.Seconds()), 0)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}

// layoutInput cuts the typed text into the rows of the box and finds the
// cursor. A row holds at most width columns.
func layoutInput(text []rune, pos, width int) (rows []string, curRow, curCol int) {
	return layoutInputSelected(text, pos, width, palette{}, 0, 0, false)
}

// layoutInputSelected cuts the typed text into rows, highlighting the
// inclusive rune selection [selStart, selEnd] when selActive is true.
func layoutInputSelected(text []rune, pos, width int, pal palette, selStart, selEnd int, selActive bool) (rows []string, curRow, curCol int) {
	var row strings.Builder
	used := 0
	flush := func() {
		rows = append(rows, row.String())
		row.Reset()
		used = 0
	}
	for i, r := range text {
		selected := selActive && i >= selStart && i <= selEnd
		if r == '\n' {
			if i == pos {
				curRow, curCol = len(rows), used
			}
			flush()
			continue
		}
		rw := runeWidth(r)
		if used+rw > width && used > 0 {
			flush()
		}
		if i == pos {
			curRow, curCol = len(rows), used
		}
		var chunk string
		if r == '	' {
			chunk = strings.Repeat(" ", tabWidth)
		} else {
			chunk = string(r)
		}
		if selected {
			row.WriteString(pal.Invert(chunk))
		} else {
			row.WriteString(chunk)
		}
		used += rw
	}
	if pos >= len(text) {
		if used >= width {
			flush() // the cursor needs a cell of its own
		}
		curRow, curCol = len(rows), used
	}
	flush()
	return rows, curRow, curCol
}

// who names the persona and program for a sentence.
func (v view) target() string {
	switch {
	case v.persona == "":
		return "an agent"
	case v.harness == "":
		return v.persona
	}
	return v.harness + " on " + v.persona
}

// accent is the color of the box: it tells a remote computer from this one
// at a glance, and shows when nothing can be sent.
func (v view) accent() string {
	switch {
	case v.ask != "":
		return colorCyan
	case v.persona == "" || v.harness == "" || !v.reported:
		return colorRed
	case v.local:
		return colorYellow
	}
	return colorCyan
}

// headerInput is one moment of the sticky 1-line bar at the top of the screen.
type headerInput struct {
	v      view
	prompt string // one-line user prompt for what is currently in view
	scroll int    // wrapped rows scrolled up from the bottom (0 = following latest)
	atTop  bool   // true when scrolled all the way to the top of the transcript
	cols   int
	pal    palette
}

// renderHeader draws the fixed single-line header at row 0: the active prompt
// for the visible output on the left, and a scroll badge or persona on the right.
func renderHeader(in headerInput) string {
	width := max(in.cols-1, 24)
	pal, v := in.pal, in.v

	var rightPlain, rightStyled string
	switch {
	case in.scroll > 0 && in.atTop:
		rightPlain = fmt.Sprintf("TOP · ↑ %dL", in.scroll)
		rightStyled = pal.SGR(colorYellow, rightPlain)
	case in.scroll > 0:
		rightPlain = fmt.Sprintf("↑ %dL", in.scroll)
		rightStyled = pal.SGR(colorYellow, rightPlain)
	case oneLine(in.prompt) != "" && v.persona != "":
		rightPlain = oneLine(v.persona)
		rightStyled = pal.Dim(rightPlain)
	}
	rightWidth := 0
	if rightPlain != "" {
		rightWidth = textWidth(rightPlain) + 2 // leading space + trailing space
	}

	room := max(width-4-rightWidth, 8)
	var leftPlain, leftStyled string
	if p := oneLine(in.prompt); p != "" {
		clipped := clipWidth(p, room)
		leftPlain = "❯ " + clipped
		leftStyled = pal.SGR("1;"+v.accent(), "❯") + " " + pal.Bold(clipped)
	} else {
		title := "geminus"
		if v.persona != "" && v.harness != "" {
			title += " · " + oneLine(v.persona) + " · " + oneLine(v.harness)
		} else if v.persona != "" {
			title += " · " + oneLine(v.persona)
		}
		clipped := clipWidth(title, room)
		leftPlain = "◆ " + clipped
		leftStyled = pal.SGR("1;"+colorCyan, "◆") + " " + pal.Bold(clipped)
	}

	used := textWidth(leftPlain)
	if rightPlain == "" {
		fill := max(width-used-1, 0)
		if fill == 0 {
			return leftStyled
		}
		return leftStyled + " " + pal.Dim(strings.Repeat("─", fill))
	}
	fill := max(width-used-textWidth(rightPlain)-2, 1)
	return leftStyled + " " + pal.Dim(strings.Repeat("─", fill)) + " " + rightStyled
}

func (v view) nodeShown() bool   { return !v.uiSet || v.showNode }
func (v view) modelShown() bool  { return !v.uiSet || v.showModel }
func (v view) effortShown() bool { return !v.uiSet || v.showEffort }
func (v view) modeShown() bool   { return !v.uiSet || v.showMode }

// render lays out the bottom of the screen for one moment.
func render(in frameInput) frame {
	width := max(in.cols-1, 24) // the last column is left free: writing there wraps on some terminals
	pal, v := in.pal, in.v
	border := func(s string) string { return pal.SGR(v.accent(), s) }
	var f frame

	// Output that is still on an unfinished line.
	if in.tail != "" {
		f.rows = append(f.rows, wrapText(in.tail, width)...)
	}
	f.rows = append(f.rows, "")
	if row := activity(in, width); row != "" {
		f.rows = append(f.rows, row)
	}

	// Top edge: harness · model · effort on the left, node name on the right.
	// The node badge is never dropped when shown; on narrow terminals the
	// trailing left-side items yield first.
	title, plainTitle, right, tagWidth := pal.Bold("geminus"), "geminus", "", 0
	switch {
	case v.ask != "":
		plainTitle = clipWidth(v.ask, width-7)
		title = pal.Bold(plainTitle)
	case v.persona != "":
		if v.nodeShown() {
			tag := v.persona
			right, tagWidth = " "+pal.SGR("1;"+v.accent(), tag)+" ", textWidth(tag)+2
		}
		room := max(width-7-tagWidth, 2)
		primary := v.harness
		if primary == "" {
			primary = v.persona
		}
		plainTitle = clipWidth(primary, room)
		title = pal.SGR("1;"+v.accent(), plainTitle)

		appendSegment := func(plain, styled string) {
			left := room - textWidth(plainTitle) - 3
			if left < 2 {
				return
			}
			if textWidth(plain) > left {
				plain = clipWidth(plain, left)
				styled = pal.Dim(plain)
			}
			plainTitle += " · " + plain
			title += pal.Dim(" · ") + styled
		}
		if v.harness != "" {
			if v.modelShown() {
				switch {
				case v.model != "":
					appendSegment(v.model, v.model)
				case v.defaultModel != "":
					appendSegment(v.defaultModel, pal.Dim(v.defaultModel))
				default:
					appendSegment("model:default", pal.Dim("model:default"))
				}
			}
			if v.effortShown() {
				switch {
				case v.effort != "":
					appendSegment(v.effort, v.effort)
				case v.defaultEffort != "":
					appendSegment(v.defaultEffort, pal.Dim(v.defaultEffort))
				default:
					appendSegment("effort:default", pal.Dim("effort:default"))
				}
			}
			if v.modeShown() {
				switch {
				case v.mode != "":
					appendSegment(v.mode, v.mode)
				case v.defaultMode != "":
					appendSegment(v.defaultMode, pal.Dim(v.defaultMode))
				}
			}
		}
	}
	fill := max(width-6-textWidth(plainTitle)-tagWidth, 0)
	f.rows = append(f.rows, border("╭─")+" "+title+" "+border(strings.Repeat("─", fill))+right+border("─╮"))

	// The input.
	inner := width - 6 // edges, a space inside each, and the prompt mark
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
		placeholder = "Type a prompt for " + v.target()
		switch {
		case v.ask != "":
			placeholder = "a number or a name from the list"
		case v.persona == "":
			placeholder = "/persona picks who to talk to"
		case !v.reported || v.harness == "":
			placeholder = "/harness picks the agent program · /persona picks someone else"
		}
		placeholder = clipWidth(placeholder, inner)
	}
	for i, line := range lines {
		mark := "  "
		if i == 0 {
			mark = pal.SGR("1;"+v.accent(), "❯") + " "
		}
		shown, used := line, textWidth(line)
		if placeholder != "" {
			shown, used = pal.Dim(placeholder), textWidth(placeholder)
		}
		f.rows = append(f.rows, border("│")+" "+mark+shown+strings.Repeat(" ", max(inner-used, 0))+" "+border("│"))
	}
	f.curRow, f.curCol = len(f.rows)-len(lines)+curRow, 4+curCol
	if in.postHuman {
		modePlain := in.helixMode.Label()
		modeColor := colorGreen
		switch in.helixMode {
		case helixNormal:
			modeColor = colorYellow
		case helixSelect:
			modeColor = colorMagenta
		}
		badge := pal.SGR("1;"+modeColor, modePlain)
		badgePlain := modePlain
		if in.helixPending != "" {
			badge += pal.Dim(" · ") + pal.SGR("1;"+colorCyan, in.helixPending)
			badgePlain += " · " + in.helixPending
		}
		bottomFill := max(width-5-textWidth(badgePlain), 0)
		f.rows = append(f.rows, border("╰─")+" "+badge+" "+border(strings.Repeat("─", bottomFill)+"╯"))
	} else {
		f.rows = append(f.rows, border("╰"+strings.Repeat("─", width-2)+"╯"))
	}

	f.rows = append(f.rows, statusRows(in, width)...)
	f.rows = append(f.rows, hintRows(in, width)...)
	return f
}

// activity is the line above the box while an answer is awaited.
func activity(in frameInput, width int) string {
	v, pal := in.v, in.pal
	if v.waiting == 0 {
		return ""
	}
	took := in.now.Sub(v.since)
	var plain, mark string
	if v.running {
		mark = pal.SGR(colorCyan, string(spinner[in.spin%len(spinner)]))
		plain = v.target() + " is working… " + elapsed(took)
	} else {
		mark = pal.Dim("◌")
		plain = "in Honcho"
		if v.rang {
			plain += " · doorbell rung"
		}
		plain += " · waiting for " + v.persona + " to pick it up… " + elapsed(took)
		if v.slow > 0 && took > v.slow {
			plain += " · its bridge was seen " + since(in.now.Sub(v.seen)) + "; the prompt stays in Honcho until the bridge reads it"
		}
	}
	if v.waiting > 1 {
		plain += fmt.Sprintf(" · %d prompts waiting", v.waiting)
	}
	plain = clipWidth(plain, width-3)
	if v.running {
		return " " + mark + " " + plain
	}
	return " " + mark + " " + pal.Dim(plain)
}

// statusRows are the lines under the box: the connection, where on that
// computer the conversation runs, its settings and which conversation it is.
// What does not fit beside the rest goes to a second line.
func statusRows(in frameInput, width int) []string {
	v, pal := in.v, in.pal
	type item struct{ plain, styled string }
	var items []item
	add := func(plain, styled string) { items = append(items, item{plain, styled}) }
	switch {
	case v.link != "":
		text := "✗ Honcho cannot be reached: " + clipWidth(v.link, 48)
		add(text, pal.SGR(colorRed, text))
	case v.persona == "":
		add("○ no persona chosen", pal.Dim("○ no persona chosen"))
	case !v.reported:
		text := "○ no bridge has reported from " + v.persona
		add(text, pal.SGR(colorRed, text))
	default:
		text, color := "bridge seen "+since(in.now.Sub(v.seen)), colorGreen
		if in.now.Sub(v.seen) > staleAfter {
			text, color = text+" (it may be off)", colorYellow
		}
		add("● "+text, pal.SGR(color, "●")+" "+text)
	}
	if v.update != "" {
		// Second, so that a narrow terminal drops other items before it.
		add("update "+v.update+" · /update", pal.SGR(colorYellow, "update "+v.update)+pal.Dim(" · /update"))
	}
	if v.harness != "" {
		// A long path keeps its end and never takes the whole line.
		room := max(width/2, 16)
		switch {
		case v.cwd != "":
			dir := clipLeft(v.cwd, room)
			add("in "+dir, pal.Dim("in ")+dir)
		case v.home != "":
			dir := clipLeft(v.home, room-10)
			add("in "+dir+" (default)", pal.Dim("in ")+dir+pal.Dim(" (default)"))
		default:
			// A bridge too old to say which one that is.
			add("default directory", pal.Dim("default directory"))
		}
		if v.session != "" {
			add(v.session, pal.Dim(v.session))
		} else {
			add("new conversation", pal.Dim("new conversation"))
		}
	}
	const most = 2
	rows, used := []string{"  "}, 2
	for i, it := range items {
		sep := " · "
		if used == 2 {
			sep = ""
		}
		if used+textWidth(sep)+textWidth(it.plain) > width {
			if i == 0 {
				rows[0] += clipWidth(it.plain, width-used)
				break
			}
			if len(rows) == most || 2+textWidth(it.plain) > width {
				break
			}
			rows, used, sep = append(rows, "  "), 2, ""
		}
		rows[len(rows)-1] += pal.Dim(sep) + it.styled
		used += textWidth(sep) + textWidth(it.plain)
	}
	return rows
}

// slashMatches lists geminus's commands that start with what is typed.
func slashMatches(typed string) []int {
	var found []int
	for i, c := range slashCommands {
		if strings.HasPrefix(c.name, typed) {
			found = append(found, i)
		}
	}
	return found
}

// slashWord returns the command being typed, when the input is one word
// that starts with a slash.
func slashWord(text []rune) (string, bool) {
	s := string(text)
	if !strings.HasPrefix(s, "/") || strings.ContainsAny(s, " \t\n") {
		return "", false
	}
	return s, true
}

// hintRows are the lines at the very bottom: what the keys do, or what the
// slash line being typed will do.
func hintRows(in frameInput, width int) []string {
	v, pal := in.v, in.pal
	one := func(s string) []string { return []string{"  " + pal.Dim(clipWidth(s, width-2))} }
	if in.notice != "" {
		return []string{"  " + pal.SGR(colorYellow, clipWidth(in.notice, width-2))}
	}
	if v.ask != "" {
		return one("Enter takes the one in [brackets] · Ctrl-C cancels")
	}
	if hint, ok := shellHint(string(in.text), v); ok {
		return one(hint)
	}
	if word, ok := slashWord(in.text); ok {
		if strings.HasPrefix(word, "//") {
			return one("sent to " + v.target() + " as " + word[1:])
		}
		found := slashMatches(word)
		if len(found) == 0 {
			return one("not a geminus command: sent to " + v.target() + " as typed")
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
			rows = append(rows, "  "+pal.SGR(colorCyan, left)+" "+pal.Dim(clipWidth(c.what, max(width-22, 0))))
		}
		if word == "/" {
			rows = append(rows, "  "+pal.Dim(clipWidth("any other slash line goes to "+v.target()+" as typed", width-2)))
		}
		return rows
	}
	if in.postHuman {
		if in.helixMode == helixInsert {
			return one("Esc normal · Enter send · Alt+Enter new line · / commands · Ctrl-C twice to quit")
		}
		return one("i/a insert · v select · w/b/e/x/% motion · d/c/y/p edit · g/m/z minor · Enter send")
	}
	return one("/ commands · Enter send · ↑↓ scroll · Ctrl-P history · Alt+Enter new line · Ctrl-C twice to quit")
}
