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
		out = append(out, p.feed([]byte(c))...)
	}
	return out
}

func kinds(keys []key) []keyKind {
	out := make([]keyKind, len(keys))
	for i, k := range keys {
		out[i] = k.kind
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
	if !sameKinds(kinds(keys), keyRune, keyRune, keyEnter) || keys[1].r != '日' {
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
	if !sameKinds(kinds(keys), keyRune, keyUp) || keys[0].r != '日' {
		t.Errorf("split input = %+v", keys)
	}
	// Esc alone, and sequences geminus has no use for.
	if got := kinds(keysOf(t, "\x1b", "\x1b[I\x1b[<0;1;1Mx\x1bz")); !sameKinds(got, keyEsc, keyRune) {
		t.Errorf("ignored input = %v", got)
	}
}

func TestPastedTextIsOneKeyAndItsLineBreaksDoNotSend(t *testing.T) {
	keys := keysOf(t, "\x1b[200~line one\r\nline", " two\rthree\x1b[20", "1~!")
	if !sameKinds(kinds(keys), keyPaste, keyRune) || keys[0].text != "line one\r\nline two\rthree" {
		t.Fatalf("paste = %+v", keys)
	}
	var e editor
	e.insert(keys[0].text)
	if e.String() != "line one\nline two\nthree" {
		t.Errorf("pasted into the box = %q", e.String())
	}
	// What could steer a terminal never gets into the box.
	e.clear()
	e.insert("a\x1b[2Jb\x07c‮")
	if e.String() != "a[2Jbc" {
		t.Errorf("control characters kept: %q", e.String())
	}
}

func TestEditingTheInput(t *testing.T) {
	var e editor
	e.insert("hello wörld 日本")
	e.wordLeft()
	e.wordLeft()
	e.insert("big ")
	if e.String() != "hello big wörld 日本" {
		t.Fatalf("insert in the middle = %q", e.String())
	}
	e.killWord()
	e.backspace()
	e.end()
	e.backspace()
	e.home()
	e.del()
	if e.String() != "ellowörld 日" {
		t.Errorf("after deletes = %q", e.String())
	}
	e.killEnd()
	if !e.empty() {
		t.Errorf("kill to end left %q", e.String())
	}

	// Lines: up and down move inside the text first.
	e.insert("first line\nsecond")
	e.up()
	e.insert("^")
	if e.String() != "first ^line\nsecond" {
		t.Errorf("up inside the text = %q", e.String())
	}
	e.down()
	e.killStart()
	if e.String() != "first ^line\n" {
		t.Errorf("down then kill = %q", e.String())
	}

	// History: what was sent comes back with the up arrow, and the draft is
	// not lost.
	e.clear()
	e.insert("one")
	if e.take(true) != "one" || !e.empty() {
		t.Fatal("take did not empty the box")
	}
	e.insert("two")
	e.take(true)
	e.insert("draft")
	e.up()
	e.up()
	if e.String() != "one" {
		t.Errorf("two ups = %q", e.String())
	}
	e.up()
	e.down()
	e.down()
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
	v.cwd, v.model, v.effort = "/srv/app", "opus", "high"
	got, f := draw(frameInput{v: v})
	want := `
╭─ sibyl · Hermes Agent ──────────────────────────────────── REMOTE ─╮
│ ❯ Type a prompt for Hermes Agent on sibyl                          │
╰────────────────────────────────────────────────────────────────────╯
  ● bridge seen 15s ago · model opus · effort high · in /srv/app
  / commands · Enter send · Alt+Enter new line · ↑ earlier prompts · …`
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

	// This computer's own agent is never mistaken for a remote one.
	v = sibylView()
	v.persona, v.local, v.session = "chrome", true, "geminus-chrome-20261119-abc123"
	got, _ = draw(frameInput{v: v})
	if !strings.Contains(got, "╭─ chrome · Hermes Agent ──────────────────────────── THIS COMPUTER ─╮") ||
		!strings.Contains(got, "● bridge seen 15s ago · geminus-chrome-20261119-abc123") {
		t.Errorf("local box:\n%s", got)
	}
	// And the color of the box says the same.
	styled := render(frameInput{v: v, cols: 71, rows: 24, now: noon, pal: palette{on: true}})
	if !strings.Contains(styled.rows[1], "\x1b[33m╭─") {
		t.Errorf("local border is not yellow: %q", styled.rows[1])
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
╭─ sibyl · Hermes… ─ REMOTE ─╮
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
	box := `
╭─ sibyl · Hermes Agent ────── REMOTE ─╮
│ ❯ half a pro                         │
╰──────────────────────────────────────╯
  ● bridge seen 15s ago
  / commands · Enter send · Alt+Enter n…`
	if got, want := term.text(), "first line\nsecond line, still coming\n"+box; got != want {
		t.Errorf("screen:\n%s\nwant:\n%s", got, want)
	}
	if term.cursor() != "│ ❯ half a pro" {
		t.Errorf("cursor is after %q", term.cursor())
	}
	// The unfinished line ends: it joins the transcript, in order.
	lv.Write([]byte("!\nthird\n"))
	typeKeys(t, lv, "mpt")
	if got, want := term.text(), "first line\nsecond line, still coming!\nthird\n"+strings.Replace(box, "half a pro   ", "half a prompt", 1); got != want {
		t.Errorf("screen:\n%s\nwant:\n%s", got, want)
	}
}

func TestALongTranscriptScrollsAndTheBoxStaysWhole(t *testing.T) {
	term, lv, _ := newTestLive(41, 12)
	for i := 0; i < 40; i++ {
		lv.Write([]byte("output line " + strings.Repeat("日本語 ", i%7) + "\n"))
	}
	typeKeys(t, lv, "日本語で書く")
	got := term.text()
	if strings.Count(got, "output line") != 40 || strings.Count(got, "╭─ sibyl") != 1 || strings.Count(got, "╰──") != 1 {
		t.Errorf("screen after scrolling:\n%s", got)
	}
	if term.cursor() != "│ ❯ 日本語で書く" {
		t.Errorf("cursor is after %q", term.cursor())
	}
	// A very long line with no end does not push the box away.
	lv.Write([]byte(strings.Repeat("x", 41*30)))
	got = term.text()
	if strings.Count(got, "╭─ sibyl") != 1 || !strings.HasSuffix(got, "Alt+Enter n…") || strings.Count(got, "x") != 41*30 {
		t.Errorf("screen after an endless line:\n%s", got[strings.LastIndex(got, "output line"):])
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
	if len(lines) != 0 || quit || !lv.ed.empty() {
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
	// Answers are not prompts: they do not come back with the up arrow.
	typeKeys(t, lv, "2\r")
	lv.set(sibylView())
	typeKeys(t, lv, "\x1b[A")
	if !lv.ed.empty() {
		t.Errorf("an answer was remembered as a prompt: %q", lv.ed.String())
	}
}

func TestResizingAndClosingLeaveNoBoxBehind(t *testing.T) {
	term, lv, _ := newTestLive(60, 20)
	lv.Write([]byte("said\n"))
	lv.size = func() (int, int) { return 44, 20 }
	term.cols = 44
	lv.tick()
	if got := term.text(); !strings.Contains(got, "╭─ sibyl · Hermes Agent ───────── REMOTE ─╮") {
		t.Errorf("after resize:\n%s", got)
	}
	lv.Write([]byte("no line end yet"))
	lv.close()
	lv.close()
	if got := term.text(); got != "said\nno line end yet" {
		t.Errorf("after close:\n%q", got)
	}
	if raw := term.raw.String(); !strings.HasPrefix(raw, pasteModeOn) || !strings.HasSuffix(raw, pasteModeOf) {
		t.Error("paste marking was not switched on and off again")
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
	term.waitFor(t, "◆ geminus", "╭─ sibyl · Hermes Agent ─", "REMOTE ─╮", "● bridge seen 40s ago · new conversation")

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
	order := []string{"❯ long job", "❯ second", "echo: long job", "echo: second", "╭─ sibyl"}
	for i := 1; i < len(order); i++ {
		if strings.Index(got, order[i-1]) > strings.Index(got, order[i]) {
			t.Errorf("%q comes after %q:\n%s", order[i-1], order[i], got)
		}
	}
	if strings.Count(got, "echo: long job") != 1 || strings.Count(got, "╭─") != 1 {
		t.Errorf("something is on screen twice:\n%s", got)
	}

	// A slash line geminus does not know goes to the agent; /model is its own.
	typed("/compact\r")
	term.waitFor(t, "echo: /compact")
	typed("/mo\t", "fast-1\r")
	term.waitFor(t, "model: fast-1", "· model fast-1 ·")

	// A question is asked in the box; cancelling it changes nothing.
	typed("/persona\r")
	term.waitFor(t, "╭─ persona ─", "a number or a name from the list", "  1  chrome")
	typed("\x03")
	term.waitFor(t, "╭─ sibyl · Hermes Agent ─")
	typed("/persona\r")
	term.waitFor(t, "╭─ persona ─")
	typed("chrome\r")
	term.waitFor(t, "persona ❯ chrome", "╭─ harness ─")
	typed("cl\r")
	term.waitFor(t, "harness ❯ cl", "╭─ chrome · Claude Code ─", "THIS COMPUTER ─╮", "● bridge seen 5s ago · new conversation")

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
	term.waitFor(t, "╭─ sibyl · Hermes Agent ─")
	// A directory name with an escape sequence and a line break in it, and
	// the same pasted into the box.
	typed("/new /tmp/a\x1b[2J\x1b]0;owned\x07b\r")
	typed("\x1b[200~paste\x1b[2Jd\x1b[201~")
	term.waitFor(t, "· in /tmp/a0;ownedb ·", "❯ paste[2Jd")
	if raw := term.raw.String(); strings.Contains(raw, "\x1b[2J") || strings.Contains(raw, "\x1b]0;") || strings.Contains(raw, "\x07") {
		t.Errorf("an escape sequence from outside reached the terminal: %q", raw)
	}
}
