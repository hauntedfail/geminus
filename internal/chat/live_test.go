package chat

import (
	"strings"
	"testing"
	"time"
)

func keysOf(t *testing.T, chunks ...string) []key {
	t.Helper()
	var p keyParser
	var out []key
	for _, c := range chunks {
		out = append(out, p.Feed([]byte(c))...)
	}
	return out
}

func kinds(keys []key) []keyKind {
	out := make([]keyKind, len(keys))
	for i, k := range keys {
		out[i] = k.Kind
	}
	return out
}

func sameKinds(a []keyKind, b ...keyKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWhatTheTerminalSendsBecomesKeys(t *testing.T) {
	keys := keysOf(t, "a日\r")
	if !sameKinds(kinds(keys), keyRune, keyRune, keyEnter) || keys[1].R != '日' {
		t.Errorf("typed text = %+v", keys)
	}
	if got := kinds(keysOf(t, "\x1b[A\x1b[B\x1b[C\x1b[D\x1b[H\x1b[F\x1b[3~\x1bOA\x1b[1;5C\x1bb")); !sameKinds(got,
		keyUp, keyDown, keyRight, keyLeft, keyHome, keyEnd, keyDelete, keyUp, keyWordRight, keyWordLeft) {
		t.Errorf("arrows = %v", got)
	}
	if got := kinds(keysOf(t, "\x03\x04\x7f\x15\x17\x0b\t\x01\x05")); !sameKinds(got,
		keyInterrupt, keyEOF, keyBackspace, keyKillStart, keyKillWord, keyKillEnd, keyTab, keyHome, keyEnd) {
		t.Errorf("control keys = %v", got)
	}
	// A line break inside the input, however the terminal reports it.
	if got := kinds(keysOf(t, "\x1b\r", "\n", "\x1b[13;2u", "\x1b[27;2;13~")); !sameKinds(got,
		keyNewline, keyNewline, keyNewline, keyNewline) {
		t.Errorf("newlines = %v", got)
	}
	// A character or a sequence split across two reads.
	keys = keysOf(t, "\xe6\x97", "\xa5\x1b[", "A")
	if !sameKinds(kinds(keys), keyRune, keyUp) || keys[0].R != '日' {
		t.Errorf("split input = %+v", keys)
	}
	// Esc alone, and sequences geminus has no use for.
	if got := kinds(keysOf(t, "\x1b", "\x1b[I\x1b[<0;1;1Mx\x1bz")); !sameKinds(got, keyEsc, keyRune) {
		t.Errorf("ignored input = %v", got)
	}
	// Mouse wheel events (SGR and legacy X10), including modifiers and split reads.
	if got := kinds(keysOf(t,
		"\x1b[<64;10;5M", "\x1b[<65;10;5M",
		"\x1b[<68;10;5M", "\x1b[<81;10;5M", // Shift+wheel up, Ctrl+wheel down
		"\x1b[<64;10;5m", "\x1b[<66;10;5M", // release and horizontal wheel ignored
		"\x1b[M\x60\x21\x21", "\x1b[M\x61\x21\x21", // X10 wheel up/down
		"\x1b[M", "\x60", "\x21\x21", // X10 split across reads
		"\x1b[M\x20\x21\x21a", // X10 left click ignored; trailing 'a' preserved
	)); !sameKinds(got,
		keyMouseScrollUp, keyMouseScrollDown,
		keyMouseScrollUp, keyMouseScrollDown,
		keyMouseScrollUp, keyMouseScrollDown,
		keyMouseScrollUp,
		keyRune,
	) {
		t.Errorf("mouse wheel = %v", got)
	}
}

func TestPastedTextIsOneKeyAndItsLineBreaksDoNotSend(t *testing.T) {
	keys := keysOf(t, "\x1b[200~line one\r\nline", " two\rthree\x1b[20", "1~!")
	if !sameKinds(kinds(keys), keyPaste, keyRune) || keys[0].Text != "line one\r\nline two\rthree" {
		t.Fatalf("paste = %+v", keys)
	}
	var e editor
	e.Insert(keys[0].Text)
	if e.String() != "line one\nline two\nthree" {
		t.Errorf("pasted into the box = %q", e.String())
	}
	// What could steer a terminal never gets into the box.
	e.Clear()
	e.Insert("a\x1b[2Jb\x07c‮")
	if e.String() != "a[2Jbc" {
		t.Errorf("control characters kept: %q", e.String())
	}
}

func TestEditingTheInput(t *testing.T) {
	var e editor
	e.Insert("hello wörld 日本")
	e.WordLeft()
	e.WordLeft()
	e.Insert("big ")
	if e.String() != "hello big wörld 日本" {
		t.Fatalf("insert in the middle = %q", e.String())
	}
	e.KillWord()
	e.Backspace()
	e.End()
	e.Backspace()
	e.Home()
	e.Del()
	if e.String() != "ellowörld 日" {
		t.Errorf("after deletes = %q", e.String())
	}
	e.KillEnd()
	if !e.Empty() {
		t.Errorf("kill to end left %q", e.String())
	}

	// Lines: up and down move inside the text first.
	e.Insert("first line\nsecond")
	e.Up()
	e.Insert("^")
	if e.String() != "first ^line\nsecond" {
		t.Errorf("up inside the text = %q", e.String())
	}
	e.Down()
	e.KillStart()
	if e.String() != "first ^line\n" {
		t.Errorf("down then kill = %q", e.String())
	}

	// History: what was sent comes back with the up arrow, and the draft is
	// not lost.
	e.Clear()
	e.Insert("one")
	if e.Take(true) != "one" || !e.Empty() {
		t.Fatal("take did not empty the box")
	}
	e.Insert("two")
	e.Take(true)
	e.Insert("draft")
	e.Up()
	e.Up()
	if e.String() != "one" {
		t.Errorf("two ups = %q", e.String())
	}
	e.Up()
	e.Down()
	e.Down()
	if e.String() != "draft" {
		t.Errorf("back down = %q", e.String())
	}
}

func TestWidthsAgreeWithTerminals(t *testing.T) {
	for s, want := range map[string]int{"abc": 3, "日本語": 6, "é": 1, "é": 1, "│❯●…": 4, "ｶﾞ": 2, "🙂": 2,
		"\x1b[1;36mab\x1b[0m": 2, "a\tb": 2 + tabWidth} {
		if got := textWidth(s); got != want {
			t.Errorf("width of %q = %d, want %d", s, got, want)
		}
	}
	if got := clipWidth("日本語のテキスト", 7); got != "日本語…" || textWidth(got) > 7 {
		t.Errorf("clip = %q", got)
	}
	rows := wrapText("abc日本語def", 4)
	if strings.Join(rows, "|") != "abc|日本|語de|f" {
		t.Errorf("wrap = %q", rows)
	}
}

var noon = time.Unix(1_795_000_000, 0)

func sibylView() view {
	return view{persona: "sibyl", harness: "Hermes Agent", reported: true, seen: noon.Add(-15 * time.Second),
		slow: 20 * time.Second}
}

func draw(in frameInput) (string, frame) {
	if in.cols == 0 {
		in.cols = 71
	}
	if in.rows == 0 {
		in.rows = 24
	}
	if in.now.IsZero() {
		in.now = noon
	}
	f := render(in)
	return strings.Join(f.rows, "\n"), f
}

func TestTheBoxShowsWhoWhatAndWhere(t *testing.T) {
	v := sibylView()
	v.harness, v.cwd, v.model, v.effort = "hermes", "/srv/app", "group/lvmh", "medium"
	got, f := draw(frameInput{v: v})
	want := `
╭─ hermes · group/lvmh · medium ───────────────────────────── sibyl ─╮
│ ❯ Type a prompt for hermes on sibyl                                │
╰────────────────────────────────────────────────────────────────────╯
  ● bridge seen 15s ago · in /srv/app · new conversation
  / commands · Enter send · ↑↓ scroll · Ctrl-P history · Alt+Enter ne…`
	if got != want {
		t.Errorf("empty box:\n%s\nwant:\n%s", got, want)
	}
	if f.curRow != 2 || f.curCol != 4 {
		t.Errorf("cursor at %d,%d", f.curRow, f.curCol)
	}
	for _, row := range f.rows {
		if w := textWidth(row); w > 70 {
			t.Errorf("row %q is %d wide", row, w)
		}
	}

	// Node name is shown on the right; unset model/effort show defaults.
	v = sibylView()
	v.persona, v.harness, v.local, v.session = "chrome", "hermes", true, "geminus-chrome-20261119-abc123"
	got, _ = draw(frameInput{v: v})
	if !strings.Contains(got, "╭─ hermes · model:default · effort:default ───────────────── chrome ─╮") ||
		!strings.Contains(got, "  ● bridge seen 15s ago · default directory\n  geminus-chrome-20261119-abc123\n") {
		t.Errorf("local box:\n%s", got)
	}
	// And the color of the box says the same.
	styled := render(frameInput{v: v, cols: 71, rows: 24, now: noon, pal: palette{On: true}})
	if !strings.Contains(styled.rows[1], "\x1b[33m╭─") {
		t.Errorf("local border is not yellow: %q", styled.rows[1])
	}

	// User UI preferences can toggle model, effort and node display.
	v.uiSet, v.showNode, v.showModel, v.showEffort = true, true, true, false
	v.model = "group/lvmh"
	got, _ = draw(frameInput{v: v})
	if !strings.Contains(got, "╭─ hermes · group/lvmh ───────────────────────────────────── chrome ─╮") {
		t.Errorf("effort hidden:\n%s", got)
	}
	v.showNode, v.showModel, v.showEffort = false, false, false
	got, _ = draw(frameInput{v: v})
	if !strings.Contains(got, "╭─ hermes ───────────────────────────────────────────────────────────╮") {
		t.Errorf("all toggles off:\n%s", got)
	}
}

func TestTheDirectoryIsAlwaysUnderTheBox(t *testing.T) {
	// Everything between the box and the one line of key hints.
	status := func(v view, cols int) string {
		_, f := draw(frameInput{v: v, cols: cols})
		for i, row := range f.rows {
			if strings.HasPrefix(row, "╰") {
				return strings.Join(f.rows[i+1:len(f.rows)-1], "\n")
			}
		}
		return ""
	}
	// None chosen: the one the bridge there uses, as it reported it.
	v := sibylView()
	v.home = "/home/sibyl"
	if got, want := status(v, 81), "  ● bridge seen 15s ago · in /home/sibyl (default) · new conversation"; got != want {
		t.Errorf("default directory:\n%q\nwant\n%q", got, want)
	}
	// A bridge too old to say which one that is.
	v.home = ""
	if got, want := status(v, 81), "  ● bridge seen 15s ago · default directory · new conversation"; got != want {
		t.Errorf("unknown default directory:\n%q\nwant\n%q", got, want)
	}
	// A chosen one wins, and a long one keeps its end: that is the part
	// that tells two projects apart. What does not fit beside it moves to a
	// second line.
	v.home, v.cwd, v.session = "/home/sibyl", "/home/sibyl/projects/clients/日本語/api-server", "geminus-sibyl-20261119-0a1b2c"
	if got, want := status(v, 81), "  ● bridge seen 15s ago · in …ibyl/projects/clients/日本語/api-server\n  geminus-sibyl-20261119-0a1b2c"; got != want {
		t.Errorf("long directory:\n%q\nwant\n%q", got, want)
	}
	// On a narrow screen it has a line to itself, and what no longer fits
	// in two lines is left out.
	if got, want := status(v, 51), "  ● bridge seen 15s ago\n  in …lients/日本語/api-server"; got != want {
		t.Errorf("narrow:\n%q\nwant\n%q", got, want)
	}
	// Nothing is said about a directory before there is a program to run in it.
	v = view{persona: "mirai"}
	if got := status(v, 81); strings.Contains(got, " in ") {
		t.Errorf("no bridge: %q", got)
	}
}

func TestTheStatusSaysWhenThingsAreNotConnected(t *testing.T) {
	v := sibylView()
	v.seen = noon.Add(-40 * time.Minute)
	if got, _ := draw(frameInput{v: v}); !strings.Contains(got, "● bridge seen 40 min ago (it may be off)") {
		t.Errorf("stale bridge:\n%s", got)
	}
	v = view{persona: "mirai"}
	got, _ := draw(frameInput{v: v})
	if !strings.Contains(got, "○ no bridge has reported from mirai") || !strings.Contains(got, "/harness picks the agent program") {
		t.Errorf("no bridge:\n%s", got)
	}
	v = sibylView()
	v.link = "dial tcp: connection refused"
	if got, _ := draw(frameInput{v: v}); !strings.Contains(got, "✗ Honcho cannot be reached: dial tcp: connection refused") {
		t.Errorf("no Honcho:\n%s", got)
	}
	if got, _ := draw(frameInput{}); !strings.Contains(got, "○ no persona chosen") || !strings.Contains(got, "╭─ geminus ─") {
		t.Errorf("nobody chosen:\n%s", got)
	}
}

func TestTheLineAboveTheBoxFollowsThePrompt(t *testing.T) {
	v := sibylView()
	v.waiting, v.since, v.rang = 1, noon.Add(-3*time.Second), true
	got, _ := draw(frameInput{v: v, cols: 100})
	if !strings.Contains(got, " ◌ in Honcho · doorbell rung · waiting for sibyl to pick it up… 3s") {
		t.Errorf("queued:\n%s", got)
	}
	v.since = noon.Add(-45 * time.Second)
	got, _ = draw(frameInput{v: v, cols: 200})
	if !strings.Contains(got, "its bridge was seen 15s ago; the prompt stays in Honcho until the bridge reads it") {
		t.Errorf("not picked up:\n%s", got)
	}
	v.running, v.waiting, v.since = true, 2, noon.Add(-65*time.Second)
	got, _ = draw(frameInput{v: v, cols: 100, spin: 1})
	if !strings.Contains(got, " ⠙ Hermes Agent on sibyl is working… 1m 05s · 2 prompts waiting") {
		t.Errorf("running:\n%s", got)
	}
}

func TestTypedTextWrapsInsideTheBoxAndTheCursorFollows(t *testing.T) {
	text := []rune("日本語のプロンプトを書く。and some English after it\nsecond line")
	got, f := draw(frameInput{v: sibylView(), text: text, pos: len(text), cols: 31})
	want := `
╭─ Hermes Agent ───── sibyl ─╮
│ ❯ 日本語のプロンプトを書く │
│   。and some English after │
│    it                      │
│   second line              │
╰────────────────────────────╯`
	if !strings.HasPrefix(got, want) {
		t.Errorf("wrapped input:\n%s\nwant:\n%s", got, want)
	}
	if f.curRow != 5 || f.curCol != 4+len("second line") {
		t.Errorf("cursor at %d,%d", f.curRow, f.curCol)
	}
	for _, row := range f.rows[:7] {
		if w := textWidth(row); w != 30 && row != "" {
			t.Errorf("row %q is %d wide, want 30", row, w)
		}
	}
	// The cursor in the middle of wide text.
	_, f = draw(frameInput{v: sibylView(), text: text, pos: 3, cols: 31})
	if f.curRow != 2 || f.curCol != 4+6 {
		t.Errorf("cursor after three wide characters at %d,%d", f.curRow, f.curCol)
	}
	// A box taller than a third of the screen scrolls with the cursor.
	long := []rune(strings.Repeat("line\n", 30) + "last")
	got, f = draw(frameInput{v: sibylView(), text: long, pos: len(long), cols: 40, rows: 24})
	if n := strings.Count(got, "│ "); n != 8 || !strings.Contains(got, "│   last") {
		t.Errorf("long input shows %d rows:\n%s", n, got)
	}
}

func TestSlashHintsSayWhatALineWillDo(t *testing.T) {
	hint := func(typed string) string {
		got, _ := draw(frameInput{v: sibylView(), text: []rune(typed), pos: len([]rune(typed)), cols: 80})
		return got[strings.Index(got, "╰"):]
	}
	if got := hint("/mo"); !strings.Contains(got, "/model [NAME]") || !strings.Contains(got, "model for this conversation") ||
		strings.Contains(got, "/persona") {
		t.Errorf("/mo:\n%s", got)
	}
	if got := hint("/compact"); !strings.Contains(got, "not a geminus command: sent to Hermes Agent on sibyl as typed") {
		t.Errorf("/compact:\n%s", got)
	}
	if got := hint("//help"); !strings.Contains(got, "sent to Hermes Agent on sibyl as /help") {
		t.Errorf("//help:\n%s", got)
	}
	if got := hint("/"); !strings.Contains(got, "/persona [NAME]") || !strings.Contains(got, "more · Tab completes") ||
		!strings.Contains(got, "any other slash line goes to Hermes Agent on sibyl as typed") {
		t.Errorf("/:\n%s", got)
	}
	if got := hint("/model opus"); strings.Contains(got, "geminus command") || !strings.Contains(got, "Enter send") {
		t.Errorf("with an argument:\n%s", got)
	}
	// Every hint is a command geminus really keeps for itself.
	w := newWorld(t)
	answers := make(chan string)
	close(answers) // a command that asks something gets no answer
	w.app.lines = answers
	for _, c := range slashCommands {
		if c.name == "/quit" {
			continue
		}
		_ = w.app.handle(c.name)
	}
	if len(w.ran) != 0 {
		t.Errorf("listed as geminus commands but sent to the agent: %v", w.ran)
	}
}

func typeKeys(t *testing.T, lv *live, chunks ...string) (lines []string, quit bool) {
	t.Helper()
	ch := make(chan string, 16)
	quit = lv.feed(keysOf(t, chunks...), ch)
	close(ch)
	for line := range ch {
		lines = append(lines, line)
	}
	return lines, quit
}

func newTestLive(cols, rows int) (*vt, *live, *time.Time) {
	term := newVT(cols, rows)
	clock := noon
	lv := newLive(term, cols, rows, palette{}, func() time.Time { return clock })
	lv.set(sibylView())
	return term, lv, &clock
}

func TestOutputScrollsAboveTheBoxWhileTyping(t *testing.T) {
	term, lv, _ := newTestLive(41, 20)
	typeKeys(t, lv, "half a pro")
	lv.Write([]byte("first line\nsecond "))
	lv.Write([]byte("line, still coming"))
	got := term.text()
	if !strings.HasPrefix(got, "◆ geminus · sibyl · Hermes Agent ───────\nfirst line\nsecond line, still coming\n") {
		t.Errorf("screen top:\n%s", got)
	}
	if !strings.Contains(got, "│ ❯ half a pro                         │") {
		t.Errorf("prompt box missing draft:\n%s", got)
	}
	if term.cursor() != "│ ❯ half a pro" {
		t.Errorf("cursor is after %q", term.cursor())
	}
	// The unfinished line ends: it joins the transcript, in order.
	lv.Write([]byte("!\nthird\n"))
	typeKeys(t, lv, "mpt")
	got = term.text()
	if !strings.Contains(got, "first line\nsecond line, still coming!\nthird\n") ||
		!strings.Contains(got, "│ ❯ half a prompt                      │") {
		t.Errorf("screen after finish:\n%s", got)
	}
}

func TestALongTranscriptScrollsAndTheBoxStaysWhole(t *testing.T) {
	term, lv, _ := newTestLive(41, 12)
	for i := 0; i < 40; i++ {
		lv.Write([]byte("output line " + strings.Repeat("日本語 ", i%7) + "\n"))
	}
	typeKeys(t, lv, "日本語で書く")
	got := term.text()
	// Full-screen console: only the viewport fits on screen, no terminal scrollback overflow.
	if lines := strings.Split(got, "\n"); len(lines) > 12 {
		t.Errorf("screen overflowed terminal height (%d rows):\n%s", len(lines), got)
	}
	if strings.Count(got, "╭─ Hermes Agent") != 1 || strings.Count(got, "╰──") != 1 {
		t.Errorf("screen after scrolling:\n%s", got)
	}
	if term.cursor() != "│ ❯ 日本語で書く" {
		t.Errorf("cursor is after %q", term.cursor())
	}
	// A very long line with no end wraps inside the viewport without pushing the box away.
	lv.Write([]byte(strings.Repeat("x", 41*30)))
	got = term.text()
	if strings.Count(got, "╭─ Hermes Agent") != 1 || !strings.HasSuffix(got, "↑↓ scroll ·…") || !strings.Contains(got, "xxxx") {
		t.Errorf("screen after an endless line:\n%s", got)
	}
	if term.cursor() != "│ ❯ 日本語で書く" {
		t.Errorf("cursor is after %q", term.cursor())
	}
}

func TestEnterSendsAndOtherKeysDoNot(t *testing.T) {
	_, lv, clock := newTestLive(60, 20)
	lines, _ := typeKeys(t, lv, "  \r", "\x15hello\x1b\rworld\\\r!", "\r")
	if len(lines) != 1 || lines[0] != "hello\nworld\n!" {
		t.Fatalf("entered = %q", lines)
	}
	lines, _ = typeKeys(t, lv, "\x1b[200~pasted\nlines\x1b[201~", "\r")
	if len(lines) != 1 || lines[0] != "pasted\nlines" {
		t.Errorf("pasted then Enter = %q", lines)
	}
	// Tab completes a geminus command.
	lines, _ = typeKeys(t, lv, "/mo\t", "opus\r", "/s\t\r", "/zz\t\r")
	if strings.Join(lines, "|") != "/model opus|/s|/zz" {
		t.Errorf("completion = %q", lines)
	}

	// Ctrl-C clears what is typed; on an empty box it takes two.
	lines, quit := typeKeys(t, lv, "abc\x03")
	if len(lines) != 0 || quit || !lv.ed.Empty() {
		t.Errorf("Ctrl-C with text: lines %q quit %v text %q", lines, quit, lv.ed.String())
	}
	if _, quit = typeKeys(t, lv, "\x03"); quit || !strings.Contains(lv.notice, "Press Ctrl-C again") {
		t.Errorf("first Ctrl-C: quit %v notice %q", quit, lv.notice)
	}
	*clock = clock.Add(3 * time.Second)
	if _, quit = typeKeys(t, lv, "\x03"); quit {
		t.Error("a Ctrl-C long after the first one quit")
	}
	if _, quit = typeKeys(t, lv, "\x03"); !quit {
		t.Error("two Ctrl-C in a row did not quit")
	}
	if _, quit = typeKeys(t, lv, "x\x04"); quit {
		t.Error("Ctrl-D with text quit")
	}
	if _, quit = typeKeys(t, lv, "\x7f\x04"); !quit {
		t.Error("Ctrl-D on an empty box did not quit")
	}
}

func TestAQuestionTakesAnEmptyAnswerAndCanBeCancelled(t *testing.T) {
	term, lv, _ := newTestLive(60, 20)
	typeKeys(t, lv, "a draft")
	v := sibylView()
	v.ask = "persona [mirai]"
	lv.set(v)
	if got := term.text(); !strings.Contains(got, "╭─ persona [mirai] ─") || !strings.Contains(got, "a number or a name from the list") ||
		!strings.Contains(got, "Enter takes the one in [brackets] · Ctrl-C cancels") {
		t.Errorf("question:\n%s", got)
	}
	if lines, _ := typeKeys(t, lv, "\r"); len(lines) != 1 || lines[0] != "" {
		t.Errorf("empty answer = %q", lines)
	}
	if lines, quit := typeKeys(t, lv, "\x03"); quit || len(lines) != 1 || lines[0] != cancelled {
		t.Errorf("Ctrl-C at a question = %q, quit %v", lines, quit)
	}
	// Answers are not prompts: Ctrl-P does not bring them back.
	typeKeys(t, lv, "2\r")
	lv.set(sibylView())
	typeKeys(t, lv, "\x10")
	if !lv.ed.Empty() {
		t.Errorf("an answer was remembered as a prompt: %q", lv.ed.String())
	}
}

func TestResizingAndClosingLeaveNoBoxBehind(t *testing.T) {
	term := newVT(60, 20)
	term.Write([]byte("prior shell output\r\n$ geminus\r\n"))
	clock := noon
	lv := newLive(term, 60, 20, palette{}, func() time.Time { return clock })
	lv.set(sibylView())

	// Prior shell output is isolated from the alternate screen buffer.
	if got := term.text(); strings.Contains(got, "prior shell output") {
		t.Errorf("prior shell output leaked into geminus console:\n%s", got)
	}
	lv.Write([]byte("said\n"))
	lv.size = func() (int, int) { return 44, 20 }
	term.cols = 44
	lv.tick()
	if got := term.text(); !strings.Contains(got, "╭─ Hermes Agent · model:default ── sibyl ─╮") {
		t.Errorf("after resize:\n%s", got)
	}
	lv.Write([]byte("no line end yet"))
	lv.close()
	lv.close()
	// Closing leaves the alternate screen and restores the original shell screen.
	if got := term.text(); got != "prior shell output\n$ geminus" {
		t.Errorf("after close:\n%q", got)
	}
	raw := term.raw.String()
	if !strings.Contains(raw, altScreenOn+pasteModeOn+altScrollOn) || !strings.HasSuffix(raw, cursorShow+altScreenOff+pasteModeOf) {
		t.Error("alternate screen / paste marking was not switched on and off again")
	}
}

func TestPageUpDownScrollAndStickyPromptHeader(t *testing.T) {
	term := newVT(71, 16)
	term.Write([]byte("old shell log line 1\r\nold shell log line 2\r\n"))
	clock := noon
	lv := newLive(term, 71, 16, palette{}, func() time.Time { return clock })
	lv.set(sibylView())

	// Turn 1: 15 lines of output.
	lv.setPrompt("first prompt: inspect auth")
	lv.Write([]byte("── you ──\n❯ first prompt: inspect auth\n"))
	for i := 1; i <= 15; i++ {
		lv.Write([]byte(strings.Repeat(" ", 0) + "turn1-line-" + string(rune('a'+i-1)) + "\n"))
	}
	// Turn 2: 15 lines of output.
	lv.setPrompt("second prompt: fix timeout")
	lv.Write([]byte("── you ──\n❯ second prompt: fix timeout\n"))
	for i := 1; i <= 15; i++ {
		lv.Write([]byte("turn2-line-" + string(rune('a'+i-1)) + "\n"))
	}

	// Keep a draft in the prompt box while scrolling.
	typeKeys(t, lv, "draft while reading")
	firstRow := strings.SplitN(term.text(), "\n", 2)[0]
	if !strings.Contains(firstRow, "❯ second prompt: fix timeout") {
		t.Errorf("at bottom, sticky header = %q, want second prompt", firstRow)
	}

	// Scroll up with PageUp (CSI 5~) into Turn 1's output without losing prompt focus.
	for i := 0; i < 4; i++ {
		typeKeys(t, lv, "\x1b[5~")
	}
	got := term.text()
	firstRow = strings.SplitN(got, "\n", 2)[0]
	if !strings.Contains(firstRow, "❯ first prompt: inspect auth") || !strings.Contains(firstRow, "↑ ") {
		t.Errorf("after PageUp into turn 1, sticky header = %q", firstRow)
	}
	if !strings.Contains(got, "turn1-line-") {
		t.Errorf("after PageUp, turn 1 lines not visible:\n%s", got)
	}
	if strings.Contains(got, "old shell log") {
		t.Errorf("shell log leaked when scrolled up:\n%s", got)
	}
	if term.cursor() != "│ ❯ draft while reading" {
		t.Errorf("prompt focus lost while scrolling: cursor=%q", term.cursor())
	}

	// Scroll all the way to the top: stops at geminus transcript top, never shows shell logs.
	for i := 0; i < 20; i++ {
		typeKeys(t, lv, "\x1b[5~")
	}
	got = term.text()
	firstRow = strings.SplitN(got, "\n", 2)[0]
	if !strings.Contains(firstRow, "TOP · ↑ ") || strings.Contains(got, "old shell log") {
		t.Errorf("at top: header=%q\nscreen:\n%s", firstRow, got)
	}

	// PageDown (CSI 6~) returns to the bottom and restores the latest prompt in the header.
	for i := 0; i < 25; i++ {
		typeKeys(t, lv, "\x1b[6~")
	}
	firstRow = strings.SplitN(term.text(), "\n", 2)[0]
	if !strings.Contains(firstRow, "❯ second prompt: fix timeout") || strings.Contains(firstRow, "↑ ") {
		t.Errorf("after PageDown to bottom, sticky header = %q", firstRow)
	}
	if lv.ed.String() != "draft while reading" {
		t.Errorf("draft text altered by scrolling: %q", lv.ed.String())
	}
}

func TestMouseWheelScrollsViewportWithoutMovingPromptHistory(t *testing.T) {
	term := newVT(71, 16)
	clock := noon
	lv := newLive(term, 71, 16, palette{}, func() time.Time { return clock })
	lv.set(sibylView())

	// Seed prompt history so an accidental arrow-key fallback would visibly change the box.
	lv.ed.Insert("older prompt")
	lv.ed.Take(true)
	lv.ed.Insert("current draft")

	lv.setPrompt("turn 1 prompt")
	lv.Write([]byte("── you ──\n❯ turn 1 prompt\n"))
	for i := 1; i <= 15; i++ {
		lv.Write([]byte("turn1-row-" + string(rune('a'+i-1)) + "\n"))
	}
	lv.setPrompt("turn 2 prompt")
	lv.Write([]byte("── you ──\n❯ turn 2 prompt\n"))
	for i := 1; i <= 15; i++ {
		lv.Write([]byte("turn2-row-" + string(rune('a'+i-1)) + "\n"))
	}

	// Scroll up via SGR mouse wheel (button 64).
	for i := 0; i < 6; i++ {
		typeKeys(t, lv, "\x1b[<64;20;8M")
	}
	got := term.text()
	firstRow := strings.SplitN(got, "\n", 2)[0]
	if !strings.Contains(firstRow, "❯ turn 1 prompt") || !strings.Contains(firstRow, "↑ ") {
		t.Errorf("after mouse wheel up, sticky header = %q", firstRow)
	}
	if !strings.Contains(got, "turn1-row-") {
		t.Errorf("after mouse wheel up, turn 1 rows not visible:\n%s", got)
	}
	if lv.ed.String() != "current draft" {
		t.Errorf("mouse wheel up changed prompt box to %q, want %q", lv.ed.String(), "current draft")
	}

	// Legacy X10 mouse wheel up also scrolls without disturbing the prompt box.
	prevScroll := lv.scroll
	typeKeys(t, lv, "\x1b[M\x60\x21\x21")
	if lv.scroll <= prevScroll {
		t.Errorf("X10 wheel up did not increase scroll: before=%d after=%d", prevScroll, lv.scroll)
	}
	if lv.ed.String() != "current draft" {
		t.Errorf("X10 wheel up changed prompt box to %q", lv.ed.String())
	}

	// Mouse wheel down returns to the bottom.
	for i := 0; i < 10; i++ {
		typeKeys(t, lv, "\x1b[<65;20;8M")
	}
	if lv.scroll != 0 {
		t.Errorf("after mouse wheel down to bottom, scroll = %d, want 0", lv.scroll)
	}
	if lv.ed.String() != "current draft" {
		t.Errorf("mouse wheel down changed prompt box to %q", lv.ed.String())
	}

	// Ctrl-P still recalls prompt history.
	typeKeys(t, lv, "\x10")
	if lv.ed.String() != "older prompt" {
		t.Errorf("Ctrl-P after mouse scroll = %q, want %q", lv.ed.String(), "older prompt")
	}
}

// mouseReports are the modes that make a terminal send clicks and drags to the
// program instead of selecting text with them.
var mouseReports = []string{"\x1b[?9h", "\x1b[?1000h", "\x1b[?1002h", "\x1b[?1003h", "\x1b[?1005h", "\x1b[?1006h", "\x1b[?1015h"}

func TestTheTerminalKeepsItsOwnSelectionAndTheWheelStillScrolls(t *testing.T) {
	term := newVT(71, 16)
	lv := newLive(term, 71, 16, palette{}, func() time.Time { return noon })
	lv.set(sibylView())

	lv.ed.Insert("older prompt")
	lv.ed.Take(true)
	typeKeys(t, lv, "line one\x1b\rline two")
	const draft = "line one\nline two"
	if lv.ed.String() != draft {
		t.Fatalf("draft = %q", lv.ed.String())
	}
	pos := lv.ed.Pos()

	lv.setPrompt("turn 1 prompt")
	for i := 0; i < 40; i++ {
		lv.Write([]byte("row-" + string(rune('a'+i%26)) + "\n"))
	}

	// With no mouse reports the wheel is the arrow keys, in either form a
	// terminal sends them. They scroll, and leave the box and its cursor alone.
	typeKeys(t, lv, "\x1b[A\x1b[A\x1b[A", "\x1bOA")
	if lv.scroll != 4 {
		t.Errorf("after four arrows up, scroll = %d, want 4", lv.scroll)
	}
	if first := strings.SplitN(term.text(), "\n", 2)[0]; !strings.Contains(first, "↑ 4L") {
		t.Errorf("after four arrows up, header = %q", first)
	}
	typeKeys(t, lv, "\x1b[B", "\x1bOB\x1bOB\x1b[B\x1b[B")
	if lv.scroll != 0 {
		t.Errorf("after arrows down past the bottom, scroll = %d, want 0", lv.scroll)
	}
	if lv.ed.String() != draft || lv.ed.Pos() != pos {
		t.Errorf("arrows changed the box: %q at %d, want %q at %d", lv.ed.String(), lv.ed.Pos(), draft, pos)
	}

	// Ctrl-P and Ctrl-N are the keys for the line above and below, and from
	// the first line for the prompt sent before.
	typeKeys(t, lv, "\x10")
	if lv.ed.String() != draft || lv.ed.Pos() >= pos-len("line two") {
		t.Errorf("Ctrl-P did not move to the first line: %q at %d", lv.ed.String(), lv.ed.Pos())
	}
	typeKeys(t, lv, "\x10")
	if lv.ed.String() != "older prompt" {
		t.Errorf("Ctrl-P on the first line = %q, want %q", lv.ed.String(), "older prompt")
	}
	typeKeys(t, lv, "\x0e")
	if lv.ed.String() != draft {
		t.Errorf("Ctrl-N back = %q, want %q", lv.ed.String(), draft)
	}

	// The same holds in Post-Human mode, in Insert and in Normal.
	v := sibylView()
	v.postHuman = true
	lv.set(v)
	typeKeys(t, lv, "\x1b[A\x1b[A")
	if lv.scroll != 2 {
		t.Errorf("Post-Human Insert: after two arrows up, scroll = %d, want 2", lv.scroll)
	}
	typeKeys(t, lv, "\x1b")
	typeKeys(t, lv, "\x1b[A", "\x1b[B\x1b[B\x1b[B")
	if lv.scroll != 0 {
		t.Errorf("Post-Human Normal: scroll = %d, want 0", lv.scroll)
	}
	if lv.ed.String() != draft {
		t.Errorf("Post-Human: arrows changed the box: %q", lv.ed.String())
	}

	lv.close()
	raw := term.raw.String()
	for _, seq := range mouseReports {
		if strings.Contains(raw, seq) {
			t.Errorf("geminus asked the terminal for mouse reports (%q): text can no longer be selected with a drag", seq)
		}
	}
	if !strings.Contains(raw, altScrollOn) {
		t.Error("the terminal was not asked to send the wheel as arrow keys")
	}
}

// onTerminal runs the app with the prompt box on a test terminal. typed sends
// what the keyboard would.
func (w *world) onTerminal(t *testing.T, args ...string) (term *vt, typed func(chunks ...string), done chan int) {
	t.Helper()
	term = newVT(81, 40)
	lv := newLive(term, 81, 40, palette{}, w.app.Now)
	lines := make(chan string, 64)
	w.app.attach(lv, lines)
	done = make(chan int, 1)
	go func() { done <- w.app.Main(args, nil, w.errs) }()
	closed := false
	typed = func(chunks ...string) {
		if !closed && lv.feed(keysOf(t, chunks...), lines) {
			closed = true
			close(lines)
		}
	}
	return term, typed, done
}

func (t *vt) waitFor(tb testing.TB, wants ...string) {
	tb.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, missing := t.text(), ""
		for _, want := range wants {
			if !strings.Contains(got, want) {
				missing = want
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("the terminal never showed %q:\n%s", missing, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (t *vt) waitGone(tb testing.TB, unwanted string) {
	tb.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Contains(t.text(), unwanted) {
		if time.Now().After(deadline) {
			tb.Fatalf("the terminal still shows %q:\n%s", unwanted, t.text())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOnATerminalTheBoxTakesTheNextPromptWhileAnAnswerIsAwaited(t *testing.T) {
	w := newWorld(t)
	w.mu.Lock()
	w.offline["sibyl"] = true
	w.mu.Unlock()
	term, typed, done := w.onTerminal(t, "persona", "sibyl")
	term.waitFor(t, "◆ geminus", "╭─ hermes · model:default · effort:default ─", "sibyl ─╮", "● bridge seen 40s ago · default directory · new conversation")

	// Sending returns at once: the second prompt goes out while the first
	// has not even been picked up.
	typed("long job\r")
	term.waitFor(t, "❯ long job", "◌ in Honcho · waiting for sibyl to pick it up…", "· geminus-sibyl-")
	typed("second\r")
	term.waitFor(t, "❯ second", "2 prompts waiting")

	w.mu.Lock()
	w.offline["sibyl"] = false
	w.mu.Unlock()
	term.waitFor(t, "echo: long job", "echo: second")
	term.waitGone(t, "waiting for sibyl")
	got := term.text()
	header, body, _ := strings.Cut(got, "\n")
	if !strings.Contains(header, "❯ second") {
		t.Errorf("sticky header = %q, want second prompt", header)
	}
	order := []string{"── you ──", "❯ long job", "❯ second", "── sibyl · hermes ──", "echo: long job", "echo: second", "╭─ hermes"}
	for i := 1; i < len(order); i++ {
		if strings.Index(body, order[i-1]) > strings.Index(body, order[i]) {
			t.Errorf("%q comes after %q:\n%s", order[i-1], order[i], body)
		}
	}
	if strings.Count(body, "echo: long job") != 1 || strings.Count(body, "╭─") != 1 {
		t.Errorf("something is on screen twice:\n%s", body)
	}

	// A slash line geminus does not know goes to the agent; /model is its own.
	typed("/compact\r")
	term.waitFor(t, "echo: /compact")
	typed("/mo	", "fast-1\r")
	term.waitFor(t, "model: fast-1", "╭─ hermes · fast-1 · effort:default ─")

	// A question is asked in the box; cancelling it changes nothing.
	typed("/persona\r")
	term.waitFor(t, "╭─ persona ─", "a number or a name from the list", "  1  chrome")
	typed("\x03")
	term.waitFor(t, "╭─ hermes · fast-1 · effort:default ─")
	typed("/persona\r")
	term.waitFor(t, "╭─ persona ─")
	typed("chrome\r")
	term.waitFor(t, "persona ❯ chrome", "╭─ harness ─")
	typed("cl\r")
	term.waitFor(t, "harness ❯ cl", "╭─ Claude Code · model:default · effort:default ─", "chrome ─╮", "● bridge seen 5s ago · default directory · new conversation")

	// The directory is under the box, and can be changed there until the
	// first prompt of the conversation is sent.
	typed("/cw\t", "~/projekt/api\r")
	term.waitFor(t, "❯ /cwd ~/projekt/api", "● bridge seen 5s ago · in ~/projekt/api · new conversation")
	typed("/cwd default\r")
	term.waitFor(t, "● bridge seen 5s ago · default directory · new conversation")

	// !cd moves from the directory chosen so far, like a shell there.
	typed("!c")
	term.waitFor(t, "!cd DIR moves on chrome from the default directory")
	typed("d projekt/api\r")
	term.waitFor(t, "❯ !cd projekt/api", "● bridge seen 5s ago · in projekt/api · new conversation")
	typed("!cd ..\r")
	term.waitFor(t, "● bridge seen 5s ago · in projekt · new conversation")

	typed("\x03", "\x03")
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("geminus did not quit:\n%s", term.text())
	}
	if strings.Join(w.ran, "|") != "sibyl/hermes: long job|sibyl/hermes: second|sibyl/hermes: /compact" {
		t.Errorf("ran = %v", w.ran)
	}
}

func TestOnATerminalTheStatusSaysWhenHonchoCannotBeRead(t *testing.T) {
	w := newWorld(t)
	w.mu.Lock()
	w.offline["sibyl"] = true
	w.mu.Unlock()
	term, typed, _ := w.onTerminal(t, "persona", "sibyl")
	typed("hello\r")
	term.waitFor(t, "waiting for sibyl")
	w.srv.Close()
	term.waitFor(t, "✗ Honcho cannot be reached")
}

func TestNothingFromOutsideCanControlTheTerminalThroughTheBox(t *testing.T) {
	w := newWorld(t)
	term, typed, _ := w.onTerminal(t, "persona", "sibyl")
	term.waitFor(t, "╭─ hermes · model:default · effort:default ─")
	// A directory name with an escape sequence and a line break in it, and
	// the same pasted into the box.
	typed("/new /tmp/a\x1b[2J\x1b]0;owned\x07b\r")
	typed("\x1b[200~paste\x1b[2Jd\x1b[201~")
	term.waitFor(t, "· in /tmp/a0;ownedb ·", "❯ paste[2Jd")
	if raw := term.raw.String(); strings.Contains(raw, "\x1b[2J") || strings.Contains(raw, "\x1b]0;") || strings.Contains(raw, "\x07") {
		t.Errorf("an escape sequence from outside reached the terminal: %q", raw)
	}
}

func TestUICommandAndDefaultTuningPersistInHoncho(t *testing.T) {
	w := newWorld(t)
	term, typed, done := w.onTerminal(t, "persona", "sibyl")
	term.waitFor(t, "╭─ hermes · model:default · effort:default ─", "sibyl ─╮")

	typed("/default model group/lvmh\r")
	term.waitFor(t, "default model: group/lvmh (saved in Honcho)")
	typed("/default effort medium\r")
	term.waitFor(t, "default effort: medium (saved in Honcho)")
	typed("/new\r")
	term.waitFor(t, "╭─ hermes · group/lvmh · medium ─", "sibyl ─╮")

	typed("/ui effort off\r")
	term.waitFor(t, "ui effort: off (saved in Honcho)", "╭─ hermes · group/lvmh ───")
	typed("/ui label hermes h-agent\r")
	term.waitFor(t, "ui label hermes: h-agent (saved in Honcho)", "╭─ h-agent · group/lvmh ───")
	typed("/ui reset\r")
	term.waitFor(t, "ui settings reset to defaults (saved in Honcho)", "╭─ hermes · group/lvmh · medium ─")

	typed("\x03", "\x03")
	<-done
}
