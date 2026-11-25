package chat

import "github.com/hauntedfail/geminus/internal/tui"

type (
	key       = tui.Key
	keyKind   = tui.KeyKind
	keyParser = tui.KeyParser
	editor    = tui.Editor
	palette   = tui.Palette
)

const (
	tabWidth = tui.TabWidth

	keyNone            = tui.KeyNone
	keyRune            = tui.KeyRune
	keyPaste           = tui.KeyPaste
	keyEnter           = tui.KeyEnter
	keyNewline         = tui.KeyNewline
	keyBackspace       = tui.KeyBackspace
	keyDelete          = tui.KeyDelete
	keyLeft            = tui.KeyLeft
	keyRight           = tui.KeyRight
	keyUp              = tui.KeyUp
	keyDown            = tui.KeyDown
	keyHome            = tui.KeyHome
	keyEnd             = tui.KeyEnd
	keyPageUp          = tui.KeyPageUp
	keyPageDown        = tui.KeyPageDown
	keyScrollUp        = tui.KeyScrollUp
	keyScrollDown      = tui.KeyScrollDown
	keyMouseScrollUp   = tui.KeyMouseScrollUp
	keyMouseScrollDown = tui.KeyMouseScrollDown
	keyWordLeft        = tui.KeyWordLeft
	keyWordRight       = tui.KeyWordRight
	keyKillWord        = tui.KeyKillWord
	keyKillStart       = tui.KeyKillStart
	keyKillEnd         = tui.KeyKillEnd
	keyTab             = tui.KeyTab
	keyEsc             = tui.KeyEsc
	keyInterrupt       = tui.KeyInterrupt
	keyEOF             = tui.KeyEOF
	keyRedraw          = tui.KeyRedraw
	keyPasteStart      = tui.KeyPasteStart

	colorRed    = tui.ColorRed
	colorGreen  = tui.ColorGreen
	colorYellow = tui.ColorYellow
	colorCyan   = tui.ColorCyan
)

var (
	isTerminal      = tui.IsTerminal
	rawMode         = tui.RawMode
	termSize        = tui.TermSize
	cancelInputRead = tui.CancelInputRead
	unsafeRune = tui.UnsafeRune
	sanitize   = tui.Sanitize
	oneLine    = tui.OneLine
	clip       = tui.Clip
	decodeRune = tui.DecodeRune
	runeWidth  = tui.RuneWidth
	skipStyle  = tui.SkipStyle
	textWidth  = tui.TextWidth
	clipWidth  = tui.ClipWidth
	clipLeft   = tui.ClipLeft
	wrapText   = tui.WrapText
)
