package tui

import "testing"

func feedHelix(h *Helix, e *Editor, raw string) []HelixAction {
	var p KeyParser
	var acts []HelixAction
	for _, k := range p.Feed([]byte(raw)) {
		if act := h.HandleKey(e, k, false); act != HelixNone {
			acts = append(acts, act)
		}
	}
	return acts
}

func TestHelixSelectionFirstWordAndEdit(t *testing.T) {
	var e Editor
	var h Helix

	// Type in Insert mode, escape to Normal.
	feedHelix(&h, &e, "hello world again")
	feedHelix(&h, &e, "\x1b")
	if h.Mode() != HelixNormal {
		t.Fatalf("mode = %v, want Normal", h.Mode())
	}

	// Goto start of line (`gh`), select first word + space (`w`), delete (`d`).
	feedHelix(&h, &e, "ghwd")
	if got := e.String(); got != "world again" {
		t.Fatalf("after ghwd: %q, want %q", got, "world again")
	}

	// Undo (`u`) restores deleted word; Redo (`U`) deletes it again.
	feedHelix(&h, &e, "u")
	if got := e.String(); got != "hello world again" {
		t.Fatalf("after u: %q, want %q", got, "hello world again")
	}
	feedHelix(&h, &e, "U")
	if got := e.String(); got != "world again" {
		t.Fatalf("after U: %q, want %q", got, "world again")
	}

	// Paste yanked "hello " before selection (`P`).
	feedHelix(&h, &e, "P")
	if got := e.String(); got != "hello world again" {
		t.Fatalf("after P: %q, want %q", got, "hello world again")
	}
}

func TestHelixSelectModeAndCaseAndChange(t *testing.T) {
	var e Editor
	var h Helix

	feedHelix(&h, &e, "foo bar baz")
	feedHelix(&h, &e, "\x1b")

	// Move to start, enter Select mode (`v`), extend over "foo bar" (`e` twice), uppercase (`Alt-``).
	feedHelix(&h, &e, "ghvee")
	if h.Mode() != HelixSelect {
		t.Fatalf("mode = %v, want Select", h.Mode())
	}
	lo, hi, ok := h.SelectionRange(len(e.Text()))
	if !ok || string(e.Text()[lo:hi+1]) != "foo bar" {
		t.Fatalf("selection = %q (%d..%d, ok=%v), want %q", string(e.Text()[lo:hi+1]), lo, hi, ok, "foo bar")
	}
	feedHelix(&h, &e, "\x1b`")
	if got := e.String(); got != "FOO BAR baz" {
		t.Fatalf("after Alt-`: %q, want %q", got, "FOO BAR baz")
	}
	if h.Mode() != HelixNormal {
		t.Fatalf("mode after Alt-` = %v, want Normal", h.Mode())
	}

	// Change selection (`c`) replaces "FOO BAR" with "qux".
	feedHelix(&h, &e, "cqux")
	feedHelix(&h, &e, "\x1b")
	if got := e.String(); got != "qux baz" {
		t.Fatalf("after change: %q, want %q", got, "qux baz")
	}
}

func TestHelixLineSelectionJoinAndIndent(t *testing.T) {
	var e Editor
	var h Helix

	e.Insert("one\ntwo\nthree")
	h.EnterNormal(&e)

	// Goto line 1 (`gg`), select line (`x`), extend to line 2 (`x`), indent (`>`).
	feedHelix(&h, &e, "ggxx>")
	if got := e.String(); got != "  one\n  two\nthree" {
		t.Fatalf("after ggxx>: %q", got)
	}

	// Unindent (`<`).
	feedHelix(&h, &e, "<")
	if got := e.String(); got != "one\ntwo\nthree" {
		t.Fatalf("after <: %q", got)
	}

	// Join first two lines (`ggJ`).
	feedHelix(&h, &e, "ggJ")
	if got := e.String(); got != "one two\nthree" {
		t.Fatalf("after ggJ: %q", got)
	}

	// Select all (`%`) and delete (`d`).
	feedHelix(&h, &e, "%d")
	if got := e.String(); got != "" {
		t.Fatalf("after %%d: %q, want empty", got)
	}
}

func TestHelixMatchModeObjectsAndSurround(t *testing.T) {
	var e Editor
	var h Helix

	e.Insert(`say (hello world)`)
	h.EnterNormal(&e)

	// Jump to '(' via `ghf(` then `mm` jumps to ')'.
	feedHelix(&h, &e, "ghf(")
	feedHelix(&h, &e, "mm")
	if e.Pos() != len(e.Text())-1 {
		t.Fatalf("mm pos = %d, want %d", e.Pos(), len(e.Text())-1)
	}

	// Move inside parens, `mi(` selects "hello world", `c` changes it.
	feedHelix(&h, &e, "hmi(chi")
	feedHelix(&h, &e, "\x1b")
	if got := e.String(); got != "say (hi)" {
		t.Fatalf("after mi(c: %q, want %q", got, "say (hi)")
	}

	// Replace surrounding parens with brackets (`mr([`).
	feedHelix(&h, &e, "mr([")
	if got := e.String(); got != "say [hi]" {
		t.Fatalf("after mr([: %q, want %q", got, "say [hi]")
	}

	// Delete surrounding brackets (`md[`).
	feedHelix(&h, &e, "md[")
	if got := e.String(); got != "say hi" {
		t.Fatalf("after md[: %q, want %q", got, "say hi")
	}

	// Select inner word (`miw`) and surround with quotes (`ms"`).
	feedHelix(&h, &e, "miwms\"")
	if got := e.String(); got != `say "hi"` {
		t.Fatalf("after miwms\": %q, want %q", got, `say "hi"`)
	}
}

func TestHelixFindTillAndRepeat(t *testing.T) {
	var e Editor
	var h Helix

	e.Insert("alpha-beta-gamma")
	h.EnterNormal(&e)

	// `ghf-` selects "alpha-"; `Alt-.` repeats to next '-'.
	feedHelix(&h, &e, "ghf-")
	if e.Pos() != 5 {
		t.Fatalf("after f-: pos = %d, want 5", e.Pos())
	}
	feedHelix(&h, &e, "\x1b.")
	if e.Pos() != 10 {
		t.Fatalf("after Alt-.: pos = %d, want 10", e.Pos())
	}

	// `F-` searches backward to index 5.
	feedHelix(&h, &e, "F-")
	if e.Pos() != 5 {
		t.Fatalf("after F-: pos = %d, want 5", e.Pos())
	}
}

func TestHelixChatPromptAdaptations(t *testing.T) {
	var e Editor
	var h Helix

	// Seed prompt history.
	e.Insert("first prompt")
	e.Take(true)
	e.Insert("second prompt")
	e.Take(true)

	// In Normal mode on empty buffer: `/` enters Insert mode and types `/`.
	h.EnterNormal(&e)
	feedHelix(&h, &e, "/")
	if h.Mode() != HelixInsert || e.String() != "/" {
		t.Fatalf("after / on empty: mode=%v text=%q", h.Mode(), e.String())
	}

	// Esc -> Normal, `d` clears `/`, `k` recalls previous prompt history.
	feedHelix(&h, &e, "\x1b")
	feedHelix(&h, &e, "dk")
	if got := e.String(); got != "second prompt" {
		t.Fatalf("after k history recall: %q, want %q", got, "second prompt")
	}

	// Enter in Normal mode submits the prompt.
	acts := feedHelix(&h, &e, "\r")
	if len(acts) != 1 || acts[0] != HelixSubmit {
		t.Fatalf("Enter in Normal mode = %v, want [HelixSubmit]", acts)
	}

	// Viewport scroll bindings in Normal mode: Ctrl-u, Ctrl-d, Ctrl-b, Ctrl-f, zt, zb, zj, zk.
	acts = feedHelix(&h, &e, "\x15\x04\x02\x06ztzbzjzk")
	want := []HelixAction{
		HelixScrollHalfUp, HelixScrollHalfDown,
		HelixScrollPageUp, HelixScrollPageDown,
		HelixScrollTop, HelixScrollBottom,
		HelixScrollDown, HelixScrollUp,
	}
	if len(acts) != len(want) {
		t.Fatalf("scroll actions = %v, want %v", acts, want)
	}
	for i := range want {
		if acts[i] != want[i] {
			t.Fatalf("scroll[%d] = %v, want %v", i, acts[i], want[i])
		}
	}

	// Pending label shows minor mode and count.
	feedHelix(&h, &e, "3g")
	if got := h.PendingLabel(); got != "3g" {
		t.Fatalf("PendingLabel = %q, want %q", got, "3g")
	}
	feedHelix(&h, &e, "\x1b")
	if got := h.PendingLabel(); got != "" {
		t.Fatalf("PendingLabel after Esc = %q, want empty", got)
	}
}

func TestHelixUnicodeUTF8(t *testing.T) {
	var e Editor
	var h Helix

	feedHelix(&h, &e, "こんにちは 世界")
	feedHelix(&h, &e, "\x1b")
	// Select last word (`b`) and surround with brackets (`ms[`).
	feedHelix(&h, &e, "bms[")
	if got := e.String(); got != "こんにちは [世界]" {
		t.Fatalf("after bms[: %q, want %q", got, "こんにちは [世界]")
	}
}
