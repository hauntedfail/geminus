package chat

// The interface on a terminal: the prompt box stays at the bottom with the
// status around it, and answers scroll above it as they arrive. Sending does
// not wait for the answer, so the next prompt can be typed at once.
//
// Without a terminal on both ends (piped input, a test, TERM=dumb) geminus
// stays line by line; see loop.

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/version"
	"github.com/hauntedfail/geminus/internal/wire"
)

const (
	// frameEvery is how often the box is checked for something to redraw.
	frameEvery = 100 * time.Millisecond
	// directoryEvery is how often what the persona's computer reported is
	// read again while the box is open, so "bridge seen" stays true.
	directoryEvery = time.Minute
)

// startLive takes over the terminal. It reports false, changing nothing,
// when geminus is not on a terminal that can do it.
func (a *App) startLive(in io.Reader) (stop func(), ok bool) {
	inFile, isIn := in.(*os.File)
	a.Out.mu.Lock()
	outFile, isOut := a.Out.w.(*os.File)
	a.Out.mu.Unlock()
	if !isIn || !isOut || !isTerminal(inFile) || !isTerminal(outFile) || os.Getenv("TERM") == "dumb" {
		return nil, false
	}
	cols, rows := termSize(outFile)
	if cols < 30 || rows < 8 {
		return nil, false
	}
	restore, err := rawMode(inFile, outFile)
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.ctx = ctx
	lv := newLive(outFile, cols, rows, palette{On: a.Out.color}, a.Now)
	lv.size = func() (int, int) { return termSize(outFile) }
	lines := make(chan string, 256)
	a.attach(lv, lines)

	done := make(chan struct{})
	// After /update another program may take over this terminal, and what
	// is typed from then on is for it. So the reader stops after that line
	// until the main loop says the conversation goes on here (resume), or
	// for good when the box is closed. Where the old process has to stay
	// behind (Windows), it would otherwise take keys from the new one.
	resume := make(chan struct{}, 1)
	a.readOn = resume
	lv.park = func(line string) (goOn bool) {
		if !isUpdateLine(line) {
			return true
		}
		select {
		case <-resume:
			return true
		case <-done:
			return false
		}
	}
	go func() {
		ticker := time.NewTicker(frameEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				lv.tick()
			case <-done:
				return
			}
		}
	}()
	go func() {
		defer close(lines)
		var parser keyParser
		buf := make([]byte, 4096)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := inFile.Read(buf)
			select {
			case <-done:
				return
			default:
			}
			if n > 0 && lv.feed(parser.Feed(buf[:n]), lines) {
				cancel() // do not wait for a request that is under way
				return
			}
			if err != nil {
				return
			}
		}
	}()
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		close(done)
		cancelInputRead(inFile)
		lv.close()
		restore()
		cancel()
	}, true
}

// feed applies keys and passes entered lines on. It reports that no more
// input is to be read: the person asked to leave, or the terminal now
// belongs to another program (park).
func (l *live) feed(keys []key, lines chan<- string) (stop bool) {
	for _, k := range keys {
		line, entered, quit := l.key(k)
		if entered {
			lines <- line
			if l.park != nil && !l.park(line) {
				return true
			}
		}
		if quit {
			return true
		}
	}
	return false
}

// attach makes lv the screen: everything printed goes above its box, and
// lines come from it.
func (a *App) attach(lv *live, lines <-chan string) {
	a.live, a.lines = lv, lines
	a.Out.mu.Lock()
	a.Out.w = lv
	a.Out.mu.Unlock()
	a.sentAt = map[string]time.Time{}
	a.shownTurn = map[string]bool{}
	a.turnPrompt = map[string]string{}
	a.Out.line(a.Out.accent("◆ geminus") + a.Out.dim(" "+version.Version+" · "+oneLine(a.Profile.Human)+
		" · Honcho workspace "+oneLine(a.Profile.Workspace)))
}

// sync shows the current state around the box.
func (a *App) sync() {
	if a.live == nil {
		return
	}
	v := view{ask: a.asking, link: a.link, cwd: oneLine(a.cwd), session: oneLine(a.session),
		model: oneLine(a.tuning.Model), effort: oneLine(a.tuning.Effort), slow: a.SlowAfter, rang: a.rang,
		update:     a.newerRelease(),
		uiSet:      true,
		showNode:   a.defaults.UI.NodeVisible(),
		showModel:  a.defaults.UI.ModelVisible(),
		showEffort: a.defaults.UI.EffortVisible()}
	if a.cur != nil {
		v.persona, v.local, v.reported, v.seen = oneLine(a.cur.Name), a.cur.Local, a.cur.Reported, a.cur.Seen
		v.home = oneLine(a.cur.Home)
	}
	if a.harness != nil {
		v.harness = a.harnessDisplay(*a.harness)
	}
	// The line above the box follows the prompt that has waited longest.
	for id := range a.sentAt {
		if !a.waiting[id] {
			delete(a.sentAt, id)
		}
	}
	for id := range a.waiting {
		at, known := a.sentAt[id]
		if !known {
			at = a.Now() // found waiting when the conversation was reopened
			a.sentAt[id] = at
		}
		if v.waiting == 0 || at.Before(v.since) {
			v.since, v.running = at, a.state[id] == wire.StateRunning
		}
		v.waiting++
	}
	a.live.set(v)
}

// ask puts a question in the box, or ends it with "".
func (a *App) ask(question string) {
	a.asking = question
	a.sync()
}

// loopLive is the main loop on a terminal: lines from the box, and Honcho
// read on a timer for the answers that are awaited.
func (a *App) loopLive() {
	ticker := time.NewTicker(a.Watch)
	defer ticker.Stop()
	refreshed := a.Now()
	a.sync()
	a.maybeAutoUpdate()
	if a.restarting {
		return
	}
	for {
		select {
		case line, ok := <-a.lines:
			if !ok {
				return
			}
			line = strings.TrimSpace(line)
			if line == "/quit" || line == "/exit" {
				return
			}
			if line == "" || line == cancelled {
				continue
			}
			a.echoPrompt(line)
			if err := a.handle(line); err != nil {
				a.Out.line(a.Out.fail("✗ " + err.Error()))
			}
			if a.restarting {
				return
			}
			a.goOnReading(line)
			a.maybeAutoUpdate()
			if a.restarting {
				return
			}
		case v := <-a.newerFound:
			a.newer = v
			a.maybeAutoUpdate()
			if a.restarting {
				return
			}
		case <-ticker.C:
			a.maybeAutoUpdate()
			if a.restarting {
				return
			}
			if len(a.waiting) > 0 && a.session != "" {
				_, _, err := a.poll("")
				a.linked(err)
			}
			if a.Now().Sub(refreshed) >= directoryEvery {
				refreshed = a.Now()
				a.linked(a.refresh())
			}
		case <-a.interrupt:
			return
		case <-a.ctx.Done():
			return
		}
		a.sync()
	}
}

// linked records whether Honcho could be read just now, for the status line.
func (a *App) linked(err error) {
	if err == nil {
		a.link = ""
		return
	}
	if a.ctx.Err() == nil {
		a.link = oneLine(err.Error())
	}
}

// isGeminusCommand reports whether line is handled locally by geminus rather
// than sent to the remote agent as a prompt.
func isGeminusCommand(line string) bool {
	if !strings.HasPrefix(line, "/") || strings.HasPrefix(line, "//") {
		return false
	}
	cmd, _, _ := strings.Cut(line, " ")
	switch cmd {
	case "/help", "/status", "/persona", "/personas", "/harness", "/new", "/cwd",
		"/sessions", "/resume", "/default", "/ui", "/update", "/model", "/effort", "/quit", "/exit":
		return true
	}
	return false
}

// echoPrompt puts what was entered into the transcript with a clear user
// utterance boundary, and updates the sticky prompt header when it is a prompt.
func (a *App) echoPrompt(text string) {
	if a.live != nil && !isGeminusCommand(text) {
		prompt := text
		if strings.HasPrefix(prompt, "//") {
			prompt = prompt[1:]
		}
		a.live.setPrompt(prompt)
	}
	a.Out.line("")
	if !isGeminusCommand(text) {
		a.Out.line(a.Out.dim("── ") + a.Out.accent("you") + a.Out.dim(" ────────────────────────────"))
	}
	for i, line := range strings.Split(text, "\n") {
		mark := a.Out.dim("│ ")
		if i == 0 {
			mark = a.Out.accent("❯ ")
		}
		a.Out.line(mark + a.Out.bold(sanitize(line)))
	}
}
