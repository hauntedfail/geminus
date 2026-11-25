package chat

// Interactive Claude Code-style select option menu rendered in the bottom
// panel of the live console. Used by /model (with no argument) and designed
// to be reusable for any single-choice picker with an optional custom input.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hauntedfail/geminus/internal/wire"
)

// menuItem is one selectable entry in a selectMenu.
type menuItem struct {
	value   string // returned value when selected ("" means default)
	label   string // display name on the left
	desc    string // concise description on the right
	current bool   // true if this option matches the active value (renders ✓)
	custom  bool   // true if selecting opens the inline custom text input
}

// menuConfig describes a selectMenu instance.
type menuConfig struct {
	title             string
	subtitle          string
	searchPlaceholder string
	customTitle       string
	customSubtitle    string
	customPlaceholder string
	items             []menuItem
	validate          func(string) error
}

type menuResult struct {
	value string
	ok    bool
}

type selectMenu struct {
	cfg        menuConfig
	search     editor
	filtered   []menuItem
	matchCount int
	totalCount int
	cursor     int
	offset     int
	customMode bool
	input      editor
	errMsg     string
	done       chan menuResult
	finished   bool
}

func newSelectMenu(cfg menuConfig, done chan menuResult) *selectMenu {
	total := 0
	for _, it := range cfg.items {
		if !it.custom {
			total++
		}
	}
	m := &selectMenu{
		cfg:        cfg,
		totalCount: total,
		done:       done,
	}
	m.refreshFilter(true)
	return m
}

func (m *selectMenu) finish(value string, ok bool) {
	if m.finished {
		return
	}
	m.finished = true
	if m.done != nil {
		m.done <- menuResult{value: value, ok: ok}
	}
}

func (m *selectMenu) refreshFilter(selectCurrent bool) {
	q := strings.ToLower(strings.TrimSpace(m.search.String()))
	if q == "" {
		m.filtered = m.cfg.items
		m.matchCount = m.totalCount
		m.offset = 0
		if selectCurrent {
			m.cursor = 0
			for i, it := range m.filtered {
				if it.current {
					m.cursor = i
					break
				}
			}
		} else if len(m.filtered) == 0 {
			m.cursor = 0
		} else if m.cursor >= len(m.filtered) {
			m.cursor = len(m.filtered) - 1
		}
		return
	}

	tokens := strings.Fields(q)
	type scored struct {
		item  menuItem
		tier  int
		index int
	}
	var matches []scored
	var customs []menuItem
	for i, it := range m.cfg.items {
		if it.custom {
			customs = append(customs, it)
			continue
		}
		tier := matchMenuItem(q, tokens, it)
		if tier >= 0 {
			matches = append(matches, scored{item: it, tier: tier, index: i})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].tier != matches[j].tier {
			return matches[i].tier < matches[j].tier
		}
		return matches[i].index < matches[j].index
	})

	out := make([]menuItem, 0, len(matches)+len(customs))
	for _, s := range matches {
		out = append(out, s.item)
	}
	out = append(out, customs...)
	m.filtered = out
	m.matchCount = len(matches)
	m.cursor = 0
	m.offset = 0
}

func matchMenuItem(q string, tokens []string, it menuItem) int {
	label := strings.ToLower(oneLine(it.label))
	val := strings.ToLower(oneLine(it.value))
	desc := strings.ToLower(oneLine(it.desc))

	if label == q || (val != "" && val == q) {
		return 0
	}
	if strings.HasPrefix(label, q) || (val != "" && strings.HasPrefix(val, q)) {
		return 1
	}
	if strings.Contains(label, q) || (val != "" && strings.Contains(val, q)) {
		return 2
	}
	combined := label + " " + val + " " + desc
	allTokens := len(tokens) > 0
	for _, tok := range tokens {
		if !strings.Contains(combined, tok) {
			allTokens = false
			break
		}
	}
	if allTokens {
		return 3
	}
	if len(tokens) == 1 && (isSubsequence(q, label) || (val != "" && isSubsequence(q, val))) {
		return 4
	}
	return -1
}

func isSubsequence(needle, haystack string) bool {
	if needle == "" {
		return true
	}
	nr := []rune(needle)
	i := 0
	for _, r := range haystack {
		if r == nr[i] {
			i++
			if i == len(nr) {
				return true
			}
		}
	}
	return false
}

func (m *selectMenu) move(delta int) {
	n := len(m.filtered)
	if n == 0 {
		return
	}
	m.cursor = (m.cursor + delta%n + n) % n
}

func (m *selectMenu) pickIndex(idx int) (closed bool) {
	if idx < 0 || idx >= len(m.filtered) {
		return false
	}
	it := m.filtered[idx]
	if it.custom {
		m.customMode = true
		m.input.clear()
		if q := strings.TrimSpace(m.search.String()); q != "" {
			m.input.set([]rune(q))
		}
		m.errMsg = ""
		return false
	}
	m.finish(it.value, true)
	return true
}

// handleKey processes one key while the menu is open. It returns true when
// the menu has closed (either confirmed or cancelled).
func (m *selectMenu) handleKey(k key) (closed bool) {
	if m.customMode {
		return m.handleCustomKey(k)
	}
	switch k.kind {
	case keyUp:
		m.move(-1)
	case keyDown:
		m.move(1)
	case keyHome:
		if m.search.empty() {
			m.cursor = 0
		} else {
			m.search.home()
		}
	case keyEnd:
		if m.search.empty() {
			if len(m.filtered) > 0 {
				m.cursor = len(m.filtered) - 1
			}
		} else {
			m.search.end()
		}
	case keyLeft:
		m.search.left()
	case keyRight:
		m.search.right()
	case keyWordLeft:
		m.search.wordLeft()
	case keyWordRight:
		m.search.wordRight()
	case keyBackspace:
		if !m.search.empty() {
			m.search.backspace()
			m.refreshFilter(m.search.empty())
		}
	case keyDelete:
		if !m.search.empty() {
			m.search.del()
			m.refreshFilter(m.search.empty())
		}
	case keyKillWord:
		if !m.search.empty() {
			m.search.killWord()
			m.refreshFilter(m.search.empty())
		}
	case keyKillStart:
		if !m.search.empty() {
			m.search.killStart()
			m.refreshFilter(m.search.empty())
		}
	case keyKillEnd:
		if !m.search.empty() {
			m.search.killEnd()
			m.refreshFilter(m.search.empty())
		}
	case keyPaste:
		if s := oneLine(k.text); s != "" {
			m.search.insert(s)
			m.refreshFilter(false)
		}
	case keyEnter:
		return m.pickIndex(m.cursor)
	case keyEsc, keyInterrupt:
		if !m.search.empty() {
			m.search.clear()
			m.refreshFilter(true)
			return false
		}
		m.finish("", false)
		return true
	case keyEOF:
		m.finish("", false)
		return true
	case keyRune:
		if m.search.empty() && k.r >= '1' && k.r <= '9' {
			idx := int(k.r - '1')
			if idx < len(m.filtered) {
				m.cursor = idx
				return m.pickIndex(idx)
			}
		}
		m.search.insert(string(k.r))
		m.refreshFilter(false)
	}
	return false
}

func (m *selectMenu) handleCustomKey(k key) (closed bool) {
	e := &m.input
	switch k.kind {
	case keyRune:
		m.errMsg = ""
		e.insert(string(k.r))
	case keyPaste:
		m.errMsg = ""
		e.insert(oneLine(k.text))
	case keyBackspace:
		m.errMsg = ""
		e.backspace()
	case keyDelete:
		m.errMsg = ""
		e.del()
	case keyLeft:
		e.left()
	case keyRight:
		e.right()
	case keyHome:
		e.home()
	case keyEnd:
		e.end()
	case keyWordLeft:
		e.wordLeft()
	case keyWordRight:
		e.wordRight()
	case keyKillWord:
		m.errMsg = ""
		e.killWord()
	case keyKillStart:
		m.errMsg = ""
		e.killStart()
	case keyKillEnd:
		m.errMsg = ""
		e.killEnd()
	case keyEsc:
		m.customMode = false
		m.errMsg = ""
		e.clear()
	case keyInterrupt:
		if !e.empty() {
			e.clear()
			m.errMsg = ""
			return false
		}
		m.finish("", false)
		return true
	case keyEOF:
		m.finish("", false)
		return true
	case keyEnter:
		val := strings.TrimSpace(e.String())
		if val == "" {
			m.errMsg = "enter a model name, or press Esc to go back"
			return false
		}
		if val == "default" || val == "none" {
			m.finish("", true)
			return true
		}
		if m.cfg.validate != nil {
			if err := m.cfg.validate(val); err != nil {
				m.errMsg = err.Error()
				return false
			}
		}
		m.finish(val, true)
		return true
	}
	return false
}

// renderMenu draws the rounded select option panel at the bottom of the screen.
func renderMenu(in frameInput, m *selectMenu) frame {
	width := max(in.cols-1, 24)
	inner := max(width-4, 16) // "│ " + inner + " │"
	pal, v := in.pal, in.v
	accent := v.accent()
	border := func(s string) string { return pal.sgr(accent, s) }
	boxRow := func(styled string, plainW int) string {
		return border("│") + " " + styled + strings.Repeat(" ", max(inner-plainW, 0)) + " " + border("│")
	}

	var f frame
	if in.tail != "" {
		f.rows = append(f.rows, wrapText(in.tail, width)...)
	}
	f.rows = append(f.rows, "")

	// Top border: title on the left, node badge on the right.
	right, tagWidth := "", 0
	if v.persona != "" && v.nodeShown() {
		tag := v.persona
		right, tagWidth = " "+pal.sgr("1;"+accent, tag)+" ", textWidth(tag)+2
	}
	titleText := m.cfg.title
	if m.customMode && m.cfg.customTitle != "" {
		titleText = m.cfg.customTitle
	}
	room := max(width-6-tagWidth, 2)
	plainTitle := clipWidth(titleText, room)
	styledTitle := pal.sgr("1;"+accent, plainTitle)
	fill := max(width-6-textWidth(plainTitle)-tagWidth, 0)
	f.rows = append(f.rows, border("╭─")+" "+styledTitle+" "+border(strings.Repeat("─", fill))+right+border("─╮"))

	// Subtitle.
	sub := m.cfg.subtitle
	if m.customMode && m.cfg.customSubtitle != "" {
		sub = m.cfg.customSubtitle
	}
	if sub != "" {
		clipped := clipWidth(sub, inner)
		f.rows = append(f.rows, boxRow(pal.dim(clipped), textWidth(clipped)))
	}
	if in.rows >= 12 {
		f.rows = append(f.rows, boxRow("", 0))
	}

	if m.customMode {
		inputInner := max(inner-2, 8) // "❯ " + input
		lines, curRow, curCol := layoutInput(m.input.text, m.input.pos, inputInner)
		if len(lines) > 3 {
			first := 0
			if curRow >= 3 {
				first = curRow - 2
			}
			lines, curRow = lines[first:first+3], curRow-first
		}
		placeholder := ""
		if len(m.input.text) == 0 && m.cfg.customPlaceholder != "" {
			placeholder = clipWidth(m.cfg.customPlaceholder, inputInner)
		}
		inputStartRow := len(f.rows)
		for i, line := range lines {
			mark := "  "
			if i == 0 {
				mark = pal.sgr("1;"+accent, "❯") + " "
			}
			shown, used := line, textWidth(line)
			if placeholder != "" {
				shown, used = pal.dim(placeholder), textWidth(placeholder)
			}
			f.rows = append(f.rows, boxRow(mark+shown, 2+used))
		}
		f.curRow = inputStartRow + curRow
		f.curCol = 4 + curCol // "│ ❯ "
		if m.errMsg != "" {
			errText := clipWidth("✗ "+oneLine(m.errMsg), inner)
			f.rows = append(f.rows, boxRow(pal.sgr(colorRed, errText), textWidth(errText)))
		}
		f.rows = append(f.rows, border("╰"+strings.Repeat("─", width-2)+"╯"))
		hint := clipWidth("Enter apply · Esc back to list · Ctrl-C cancel", width-2)
		f.rows = append(f.rows, "  "+pal.dim(hint))
		return f
	}

	// List mode: search bar + filtered scrollable window.
	searching := !m.search.empty()
	searchPrefix := "Search: "
	countPlain, countStyled, countW := "", "", 0
	if searching {
		countPlain = fmt.Sprintf(" %d/%d", m.matchCount, m.totalCount)
		countStyled = pal.dim(countPlain)
		countW = textWidth(countPlain)
	}
	searchRoom := inner - len(searchPrefix) - countW
	if searchRoom < 6 {
		countPlain, countStyled, countW = "", "", 0
		searchRoom = max(inner-len(searchPrefix), 4)
	}
	if !searching {
		ph := m.cfg.searchPlaceholder
		if ph == "" {
			ph = "Type to search…"
		}
		clipped := clipWidth(ph, searchRoom)
		row := pal.sgr(accent, searchPrefix) + pal.dim(clipped)
		f.curRow = len(f.rows)
		f.curCol = 2 + len(searchPrefix)
		f.rows = append(f.rows, boxRow(row, len(searchPrefix)+textWidth(clipped)))
	} else {
		shown, curCol := clipSearchWindow(m.search.text, m.search.pos, searchRoom)
		shownW := textWidth(shown)
		pad := ""
		if countW > 0 {
			pad = strings.Repeat(" ", max(inner-len(searchPrefix)-shownW-countW, 0))
		}
		row := pal.sgr("1;"+accent, searchPrefix) + shown + pad + countStyled
		f.curRow = len(f.rows)
		f.curCol = 2 + len(searchPrefix) + curCol
		f.rows = append(f.rows, boxRow(row, len(searchPrefix)+shownW+len(pad)+countW))
	}
	if in.rows >= 12 {
		f.rows = append(f.rows, boxRow(pal.dim(strings.Repeat("─", inner)), inner))
	}

	if searching && m.matchCount == 0 {
		noMatch := clipWidth("  No matching options", inner)
		f.rows = append(f.rows, boxRow(pal.dim(noMatch), textWidth(noMatch)))
	}

	items := m.filtered
	n := len(items)
	maxVisible := max(3, min(10, in.rows/2))
	if maxVisible > n {
		maxVisible = n
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+maxVisible {
		m.offset = m.cursor - maxVisible + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
	if m.offset+maxVisible > n {
		m.offset = max(n-maxVisible, 0)
	}

	if m.offset > 0 {
		more := clipWidth(fmt.Sprintf("  ↑ %d more", m.offset), inner)
		f.rows = append(f.rows, boxRow(pal.dim(more), textWidth(more)))
	}

	numWidth := len(fmt.Sprint(n))
	labelCol := 0
	for _, it := range items {
		if w := textWidth(oneLine(it.label)); w > labelCol {
			labelCol = w
		}
	}
	maxLabel := max(10, min(28, inner/2))
	if labelCol > maxLabel {
		labelCol = maxLabel
	}

	end := min(m.offset+maxVisible, n)
	for i := m.offset; i < end; i++ {
		it := items[i]
		selected := i == m.cursor

		prefix := "  "
		if selected {
			prefix = pal.sgr("1;"+accent, "❯") + " "
		}
		numPlain, numStyled := "", ""
		if !searching {
			numPlain = fmt.Sprintf("%*d. ", numWidth, i+1)
			numStyled = pal.dim(numPlain)
			if selected {
				numStyled = pal.sgr("1;"+accent, numPlain)
			}
		}

		checkStyled, checkW := "", 0
		if it.current {
			checkStyled = " " + pal.sgr("1;"+colorGreen, "✓")
			checkW = 2
		}

		headW := 2 + len(numPlain) // prefix + optional number
		avail := max(inner-headW-checkW, 4)

		rawLabel := oneLine(it.label)
		descRoom := avail - labelCol - 2
		labelMax := min(labelCol, avail)
		if descRoom < 8 {
			labelMax = avail
		}
		labelPlain := clipWidth(rawLabel, labelMax)
		labelW := textWidth(labelPlain)
		labelStyled := labelPlain
		if selected {
			labelStyled = pal.bold(labelPlain)
		} else if it.custom {
			labelStyled = pal.sgr(accent, labelPlain)
		}

		// Description column (aligned neatly when room permits).
		descPlain, descStyled, gap := "", "", ""
		if it.desc != "" && descRoom >= 8 {
			pad := max(labelCol-labelW, 0)
			gap = strings.Repeat(" ", pad+2)
			descPlain = clipWidth(oneLine(it.desc), descRoom)
			descStyled = pal.dim(descPlain)
		}

		used := headW + labelW + len(gap) + textWidth(descPlain)
		rightPad := ""
		if checkW > 0 {
			rightPad = strings.Repeat(" ", max(inner-used-checkW, 0))
			used += len(rightPad) + checkW
		}

		row := prefix + numStyled + labelStyled + gap + descStyled + rightPad + checkStyled
		f.rows = append(f.rows, boxRow(row, used))
	}

	if below := n - end; below > 0 {
		more := clipWidth(fmt.Sprintf("  ↓ %d more", below), inner)
		f.rows = append(f.rows, boxRow(pal.dim(more), textWidth(more)))
	}

	f.rows = append(f.rows, border("╰"+strings.Repeat("─", width-2)+"╯"))
	hintText := "↑↓ navigate · type to search · 1-9 select · Enter confirm · Esc cancel"
	if searching {
		hintText = "↑↓ navigate · Enter confirm · Esc clear search"
	} else if n < 9 {
		hintText = fmt.Sprintf("↑↓ navigate · type to search · 1-%d select · Enter confirm · Esc cancel", n)
	}
	f.rows = append(f.rows, "  "+pal.dim(clipWidth(hintText, width-2)))
	return f
}

// clipSearchWindow slices text to fit within maxW display cells while keeping
// the rune cursor at pos visible, returning the displayed string and the
// cursor's display column within it.
func clipSearchWindow(text []rune, pos, maxW int) (string, int) {
	if maxW <= 0 || len(text) == 0 {
		return "", 0
	}
	if pos < 0 {
		pos = 0
	}
	if pos > len(text) {
		pos = len(text)
	}
	widths := make([]int, len(text))
	for i, r := range text {
		widths[i] = textWidth(string(r))
	}
	left := 0
	before := 0
	for i := 0; i < pos; i++ {
		before += widths[i]
	}
	limit := maxW
	if pos < len(text) && maxW > 1 {
		limit = maxW - 1
	}
	for left < pos && before > limit {
		before -= widths[left]
		left++
	}
	right := left
	used := 0
	for right < len(text) && used+widths[right] <= maxW {
		used += widths[right]
		right++
	}
	return string(text[left:right]), before
}

// harnessModelPresets are curated default suggestions for harnesses whose
// standard model aliases are well-known. They are only suggestions: custom
// model names and bridge-advertised models always work.
var harnessModelPresets = map[string][]struct{ id, desc string }{
	"claude": {
		{"opus", "Most capable for complex reasoning"},
		{"sonnet", "Balanced speed and intelligence"},
		{"haiku", "Fast and lightweight"},
	},
	"codex": {
		{"gpt-5.4", "Latest frontier coding model"},
		{"o3", "Deep reasoning"},
		{"o4-mini", "Fast reasoning"},
	},
	"grok": {
		{"grok-4", "Flagship reasoning model"},
		{"grok-3", "Balanced general model"},
		{"grok-3-mini", "Fast and lightweight"},
	},
}

// modelMenuItems builds the deduplicated option list for /model on the
// currently selected persona and harness.
func (a *App) modelMenuItems() []menuItem {
	h := *a.harness
	curModel := a.tuning.Model
	savedHere := ""
	if a.cur != nil {
		savedHere = a.defaults.Personas[a.cur.Name].Model
	}

	items := []menuItem{
		{
			value:   "",
			label:   "default",
			desc:    oneLine(h.Name) + "'s own default",
			current: curModel == "",
		},
	}
	seen := map[string]int{"": 0}
	add := func(id, desc string) {
		id = strings.TrimSpace(id)
		if id == "" || !wire.ValidSetting("model", id) {
			return
		}
		if idx, ok := seen[id]; ok {
			if items[idx].desc == "" && desc != "" {
				items[idx].desc = desc
			}
			return
		}
		seen[id] = len(items)
		items = append(items, menuItem{
			value:   id,
			label:   id,
			desc:    desc,
			current: curModel == id,
		})
	}

	// 1. Bridge-advertised models (if the bridge publishes them).
	for _, m := range h.Models {
		add(m, "Available on "+oneLine(a.cur.Name))
	}

	// 2. Saved default model for this persona.
	if savedHere != "" {
		if idx, ok := seen[savedHere]; ok {
			items[idx].desc = appendTag(items[idx].desc, "saved default")
		} else {
			add(savedHere, "Saved default")
		}
	}

	// 3. Curated presets for known harnesses.
	for _, p := range harnessModelPresets[h.ID] {
		if idx, ok := seen[p.id]; ok {
			if items[idx].desc == "Saved default" {
				items[idx].desc = p.desc + " · saved default"
			}
		} else {
			add(p.id, p.desc)
		}
	}

	// 4. Current conversation model (if custom / not already listed).
	if curModel != "" {
		add(curModel, "Current conversation")
	}

	// 5. Recently used models for this harness in this geminus run.
	for _, m := range a.recentModels[h.ID] {
		add(m, "Recently used")
	}

	// 6. Saved models on other personas (sorted for deterministic order).
	if len(a.defaults.Personas) > 0 {
		names := make([]string, 0, len(a.defaults.Personas))
		for name := range a.defaults.Personas {
			if a.cur != nil && name == a.cur.Name {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			pd := a.defaults.Personas[name]
			if pd.Model == "" {
				continue
			}
			if pd.Harness == "" || pd.Harness == h.ID {
				add(pd.Model, "Saved on "+oneLine(name))
			}
		}
	}

	// Final entry: inline custom model input.
	items = append(items, menuItem{
		label:  "Custom model…",
		desc:   "Enter any model name",
		custom: true,
	})
	return items
}

func appendTag(base, tag string) string {
	if base == "" {
		return tag
	}
	if strings.Contains(base, tag) {
		return base
	}
	return base + " · " + tag
}
