package tui

// Palette styles text with the terminal's own 16 colors, or not at all.
type Palette struct{ On bool }

func (p Palette) SGR(code, s string) string {
	if !p.On || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p Palette) Dim(s string) string  { return p.SGR("2", s) }
func (p Palette) Bold(s string) string { return p.SGR("1", s) }

const (
	ColorRed    = "31"
	ColorGreen  = "32"
	ColorYellow = "33"
	ColorCyan   = "36"
)
