package tui

import (
	"strings"
	"testing"
)

func TestWindowsRawConsoleModes(t *testing.T) {
	// Typical default Windows ConPTY / PowerShell console flags plus an extra
	// unrelated flag (0x0040 = ENABLE_QUICK_EDIT_MODE) that must be preserved.
	const quickEdit uint32 = 0x0040
	inMode := WinEnableProcessedInput | WinEnableLineInput | WinEnableEchoInput | quickEdit
	outMode := WinEnableProcessedOutput

	rawIn, rawOut := WindowsRawConsoleModes(inMode, outMode)

	if rawIn&(WinEnableProcessedInput|WinEnableLineInput|WinEnableEchoInput) != 0 {
		t.Fatalf("rawIn still has cooked-mode bits set: 0x%x", rawIn)
	}
	if rawIn&WinEnableVirtualTerminalInput == 0 {
		t.Fatalf("rawIn missing ENABLE_VIRTUAL_TERMINAL_INPUT: 0x%x", rawIn)
	}
	if rawIn&WinEnableMouseInput != 0 {
		t.Fatalf("rawIn asks for mouse input, which stops the console selecting text: 0x%x", rawIn)
	}
	if withMouse, _ := WindowsRawConsoleModes(inMode|WinEnableMouseInput, outMode); withMouse&WinEnableMouseInput == 0 {
		t.Fatalf("rawIn dropped ENABLE_MOUSE_INPUT that was already set: 0x%x", withMouse)
	}
	if rawIn&quickEdit == 0 {
		t.Fatalf("rawIn dropped unrelated flag 0x%x: 0x%x", quickEdit, rawIn)
	}
	if rawOut&(WinEnableProcessedOutput|WinEnableVirtualTerminalOutput) != (WinEnableProcessedOutput | WinEnableVirtualTerminalOutput) {
		t.Fatalf("rawOut missing VT output bits: 0x%x", rawOut)
	}
}

func TestConPTYAndJapaneseIMEKeyInput(t *testing.T) {
	// Simulate ConPTY / Windows Terminal delivering Japanese IME output byte-by-byte
	// followed by a bracketed paste containing Windows CRLF line endings.
	var p KeyParser
	imeBytes := []byte("日本語の入力、テスト！🚀")
	var got []Key
	for i := range imeBytes {
		got = append(got, p.Feed(imeBytes[i:i+1])...)
	}
	var sb strings.Builder
	for _, k := range got {
		if k.Kind != KeyRune {
			t.Fatalf("unexpected non-rune key during byte-split IME input: %+v", k)
		}
		sb.WriteRune(k.R)
	}
	if sb.String() != "日本語の入力、テスト！🚀" {
		t.Fatalf("reconstructed IME text = %q, want %q", sb.String(), "日本語の入力、テスト！🚀")
	}

	// Bracketed paste with Windows CRLF split across reads, followed by Shift+Enter
	// (ConPTY / Windows Terminal CSI 27;2;13~ and CSI 13;2u) and BS (0x08).
	pasteChunks := []string{
		"\x1b[200~一行目\r\n二行",
		"目\r三行目\x1b[20",
		"1~\x1b[27;2;13~\x1b[13;2u\x08\x7f\x1b[3~\x1b[1;5D\x1b[1;5C\x1bOH\x1bOF\r",
	}
	var keys []Key
	for _, c := range pasteChunks {
		keys = append(keys, p.Feed([]byte(c))...)
	}
	wantKinds := []KeyKind{
		KeyPaste, KeyNewline, KeyNewline, KeyBackspace, KeyBackspace,
		KeyDelete, KeyWordLeft, KeyWordRight, KeyHome, KeyEnd, KeyEnter,
	}
	if len(keys) != len(wantKinds) {
		t.Fatalf("got %d keys (%+v), want %d (%+v)", len(keys), keys, len(wantKinds), wantKinds)
	}
	for i, wk := range wantKinds {
		if keys[i].Kind != wk {
			t.Errorf("key[%d].Kind = %v, want %v", i, keys[i].Kind, wk)
		}
	}

	var ed Editor
	ed.Insert(keys[0].Text)
	if ed.String() != "一行目\n二行目\n三行目" {
		t.Fatalf("CRLF paste normalized to %q, want %q", ed.String(), "一行目\n二行目\n三行目")
	}

	// Control chars and bidi overrides injected via paste are stripped while Japanese stays intact.
	ed.Clear()
	ed.Insert("安全\x1b[31m赤\u202e逆転\x07文字")
	if ed.String() != "安全[31m赤逆転文字" {
		t.Fatalf("sanitized editor insert = %q, want %q", ed.String(), "安全[31m赤逆転文字")
	}
}

func TestJapaneseAndWideCharWidthsAndWrapping(t *testing.T) {
	// Fullwidth Japanese = 2 cols each; halfwidth katakana = 1 col; combining dakuten = 0 cols.
	if w := TextWidth("日本語"); w != 6 {
		t.Errorf("TextWidth(日本語) = %d, want 6", w)
	}
	if w := TextWidth("ｱｲｳ"); w != 3 {
		t.Errorf("TextWidth(ｱｲｳ) = %d, want 3", w)
	}
	if w := TextWidth("か\u3099"); w != 2 {
		t.Errorf("TextWidth(か+combining dakuten) = %d, want 2", w)
	}
	if w := TextWidth("\x1b[1;36m日本語\x1b[0m"); w != 6 {
		t.Errorf("TextWidth(styled 日本語) = %d, want 6", w)
	}

	// ClipWidth / ClipLeft must never exceed the requested column budget.
	for _, limit := range []int{1, 2, 3, 4, 5, 6, 7} {
		cw := ClipWidth("日本語テスト", limit)
		if tw := TextWidth(cw); tw > limit {
			t.Errorf("ClipWidth(limit=%d) = %q has width %d > %d", limit, cw, tw, limit)
		}
		cl := ClipLeft("C:\\Users\\h.ota\\日本語ディレクトリ\\geminus", limit)
		if tw := TextWidth(cl); tw > limit {
			t.Errorf("ClipLeft(limit=%d) = %q has width %d > %d", limit, cl, tw, limit)
		}
	}

	// WrapText must keep every wrapped row <= width even on odd widths with wide runes.
	rows := WrapText("\x1b[32m日本語ABCテスト\x1b[0m", 5)
	if len(rows) < 3 {
		t.Fatalf("WrapText produced too few rows: %q", rows)
	}
	for i, r := range rows {
		if tw := TextWidth(r); tw > 5 {
			t.Errorf("row[%d] = %q has width %d > 5", i, r, tw)
		}
	}
}
