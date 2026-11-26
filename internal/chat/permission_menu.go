package chat

import (
	"fmt"
	"strings"
)

// renderPermissionMenu always reserves space for both complete, paged context
// and an offered choice, including on the smallest supported terminal.
func renderPermissionMenu(in frameInput, m *selectMenu) frame {
	width := max(in.cols-1, 24)
	inner := width - 4
	pal := in.pal
	border := func(s string) string { return pal.SGR(in.v.accent(), s) }
	row := func(text string) string {
		text = clipWidth(text, inner)
		return border("│") + " " + text + strings.Repeat(" ", max(inner-textWidth(text), 0)) + " " + border("│")
	}
	budget := max(in.rows-2, 6)
	choiceRows := min(len(m.filtered), min(4, max(1, (budget-4)/2)))
	detailRows := max(1, min(12, budget-choiceRows-4))
	m.detailStep = detailRows
	var details []string
	for _, line := range strings.Split(m.cfg.details, "\n") {
		details = append(details, wrapText(line, inner)...)
	}
	m.detailOffset = min(m.detailOffset, max(len(details)-detailRows, 0))
	end := min(m.detailOffset+detailRows, len(details))
	title := clipWidth(m.cfg.title, width-6)
	f := frame{rows: []string{border("╭─ ") + title + " " + border(strings.Repeat("─", max(width-5-textWidth(title), 0))+"╮")}}
	for _, line := range details[m.detailOffset:end] {
		f.rows = append(f.rows, row(line))
	}
	f.rows = append(f.rows, row(fmt.Sprintf("Details %d-%d/%d · PgUp/PgDn", m.detailOffset+1, end, len(details))))
	m.offset = max(min(m.offset, m.cursor), m.cursor-choiceRows+1)
	for i := m.offset; i < min(m.offset+choiceRows, len(m.filtered)); i++ {
		item := m.filtered[i]
		prefix := "  "
		if i == m.cursor {
			prefix = "❯ "
		}
		f.rows = append(f.rows, row(fmt.Sprintf("%s%d. %s · %s", prefix, i+1, oneLine(item.label), oneLine(item.desc))))
	}
	f.rows = append(f.rows, border("╰"+strings.Repeat("─", width-2)+"╯"), pal.Dim(clipWidth("↑↓/j/k · number/hotkey · Enter · Esc dismiss", width)))
	return f
}
