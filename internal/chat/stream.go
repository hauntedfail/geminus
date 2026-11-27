package chat

// Real-time ephemeral GE2E v1 token & tool progress streaming for geminus.
// Honcho remains the authoritative durable store; this runner subscribes to
// encrypted live deltas from geminus-e2ee-proxy when E2EE is configured and
// reconciles seamlessly when Honcho's final signed RoleOutput arrives.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hauntedfail/geminus/internal/e2ee"
	"github.com/hauntedfail/geminus/internal/wire"
)

const streamReconnectDelay = 500 * time.Millisecond

type streamTurnState struct {
	streamedRaw  string
	honchoRaw    string
	lastSeq      int
	toolActivity string
	done         bool
	// pipedMode records whether deltas were written directly to a non-live writer.
	pipedMode bool
}

type streamRunner struct {
	mu          sync.Mutex
	client      *e2ee.StreamClient
	unsupported bool

	session string
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	events  chan e2ee.StreamEvent

	turns map[string]*streamTurnState
}

func (a *App) ensureStreamRunner() *streamRunner {
	if a.stream == nil {
		a.stream = &streamRunner{
			turns: map[string]*streamTurnState{},
		}
	}
	return a.stream
}

// ensureStream subscribes to the active conversation's ephemeral GE2E stream
// when E2EE is configured. Safe to call repeatedly; no-op without E2EE.
func (a *App) ensureStream() {
	if a.session == "" {
		a.stopStream()
		return
	}
	sr := a.ensureStreamRunner()
	sr.mu.Lock()
	if sr.unsupported {
		sr.mu.Unlock()
		return
	}
	if sr.client == nil {
		if strings.TrimSpace(a.Profile.E2EEURL) == "" || len(a.Profile.Secret) < e2ee.MinSecretBytes {
			sr.mu.Unlock()
			return
		}
		cli, err := e2ee.NewStreamClient(a.Profile.E2EEURL, a.Profile.Secret)
		if err != nil {
			sr.unsupported = true
			sr.mu.Unlock()
			return
		}
		sr.client = cli
	}
	if sr.session == a.session && sr.cancel != nil {
		sr.mu.Unlock()
		return
	}
	oldCancel := sr.cancel
	sr.cancel = nil
	sr.session = ""
	sr.mu.Unlock()

	if oldCancel != nil {
		oldCancel()
		sr.wg.Wait()
	}

	baseCtx := a.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	subCtx, cancel := context.WithCancel(baseCtx)
	evCh := make(chan e2ee.StreamEvent, 256)
	session := a.session

	sr.mu.Lock()
	sr.session = session
	sr.cancel = cancel
	sr.events = evCh
	cli := sr.client
	sr.wg.Add(1)
	sr.mu.Unlock()

	go func() {
		defer sr.wg.Done()
		var after uint64
		for {
			if subCtx.Err() != nil {
				return
			}
			nextAfter, err := cli.Subscribe(subCtx, session, after, func(ev e2ee.StreamEvent) {
				select {
				case evCh <- ev:
				case <-subCtx.Done():
				}
			})
			if nextAfter > after {
				after = nextAfter
			}
			if subCtx.Err() != nil {
				return
			}
			if errors.Is(err, e2ee.ErrStreamUnsupported) {
				sr.mu.Lock()
				sr.unsupported = true
				sr.mu.Unlock()
				return
			}
			if err != nil {
				timer := time.NewTimer(streamReconnectDelay)
				select {
				case <-timer.C:
				case <-subCtx.Done():
					timer.Stop()
					return
				}
			}
		}
	}()
}

// stopStream terminates any active background stream subscription cleanly.
func (a *App) stopStream() {
	if a.stream == nil {
		return
	}
	sr := a.stream
	sr.mu.Lock()
	cancel := sr.cancel
	sr.cancel = nil
	sr.session = ""
	sr.events = nil
	sr.mu.Unlock()
	if cancel != nil {
		cancel()
		sr.wg.Wait()
	}
	if a.live != nil {
		a.live.clearPreview(true)
	}
}

// resetStreamTurns clears per-turn streaming reconciliation state when switching conversations.
func (a *App) resetStreamTurns() {
	if a.stream == nil {
		return
	}
	a.stream.turns = map[string]*streamTurnState{}
	if a.live != nil {
		a.live.clearPreview(false)
	}
}

func (a *App) streamEvents() <-chan e2ee.StreamEvent {
	if a.stream == nil {
		return nil
	}
	a.stream.mu.Lock()
	defer a.stream.mu.Unlock()
	return a.stream.events
}

// streamActivity returns the live tool execution summary for the oldest waiting turn, if any.
func (a *App) streamActivity() string {
	if a.stream == nil || len(a.stream.turns) == 0 {
		return ""
	}
	for turn, st := range a.stream.turns {
		if a.waiting[turn] && st.toolActivity != "" {
			return st.toolActivity
		}
	}
	return ""
}

func (a *App) ensureTurnHeader(turn string) {
	if a.live == nil {
		return
	}
	if p := a.turnPrompt[turn]; p != "" {
		a.live.setPrompt(p)
	}
	if a.shownTurn == nil {
		a.shownTurn = map[string]bool{}
	}
	if !a.clawdEnabled() && !a.shownTurn[turn] {
		a.shownTurn[turn] = true
		speaker := "agent"
		if a.cur != nil {
			speaker = oneLine(a.cur.Name)
			if a.harness != nil {
				speaker += " · " + a.harnessDisplay(*a.harness)
			}
		}
		a.Out.line("")
		a.Out.line(a.Out.dim("── ") + a.Out.bold(speaker) + a.Out.dim(" ────────────────────────"))
	}
}

// handleStreamEvent applies one verified real-time GE2E stream event from the bridge.
// It returns true when the event is StreamKindDone so the caller can immediately poll Honcho.
func (a *App) handleStreamEvent(ev e2ee.StreamEvent) bool {
	if ev.Session != a.session || ev.Turn == "" {
		return false
	}
	sr := a.ensureStreamRunner()
	st := sr.turns[ev.Turn]
	if st == nil {
		st = &streamTurnState{pipedMode: a.live == nil}
		sr.turns[ev.Turn] = st
	}
	if ev.Seq <= st.lastSeq {
		return false
	}
	st.lastSeq = ev.Seq

	// Mark turn running immediately so the spinner activates before the next Honcho poll.
	if a.waiting[ev.Turn] && a.state[ev.Turn] != wire.StateRunning {
		a.state[ev.Turn] = wire.StateRunning
	}

	switch ev.Kind {
	case e2ee.StreamKindDelta:
		if ev.Text == "" {
			return false
		}
		hadTool := st.toolActivity != ""
		st.toolActivity = ""
		st.streamedRaw += ev.Text
		a.recordTurnOutput(ev.Turn, ev.Text)
		if hadTool {
			a.notifyWorking(ev.Turn, "", "")
		}
		a.emitStreamDelta(ev.Turn, ev.Text, st)
		return false

	case e2ee.StreamKindToolUse:
		label := oneLine(ev.Tool)
		if label == "" {
			label = "tool"
		}
		if detail := oneLine(ev.Detail); detail != "" {
			label += ": " + detail
		}
		st.toolActivity = label
		a.showStreamTool(ev.Turn, label)
		a.notifyWorking(ev.Turn, ev.Tool, ev.Detail)
		return false

	case e2ee.StreamKindToolResult:
		st.toolActivity = ""
		a.notifyWorking(ev.Turn, "", "")
		return false

	case e2ee.StreamKindDone:
		st.toolActivity = ""
		st.done = true
		return true
	}
	return false
}

func (a *App) showStreamTool(turn, label string) {
	if a.live != nil {
		a.live.clearPreview(false)
		a.ensureTurnHeader(turn)
	}
	if a.clawdEnabled() {
		a.Out.line(a.Out.dim("  ⎿  " + label))
		return
	}
	a.Out.line(a.Out.dim("· " + label))
}

func (a *App) emitStreamDelta(turn, delta string, st *streamTurnState) {
	clean := sanitize(delta)
	if clean == "" {
		return
	}
	if a.live == nil {
		// Piped / non-TTY mode: write incrementally without buffering for newline.
		st.pipedMode = true
		a.Out.write(clean)
		return
	}

	a.ensureTurnHeader(turn)
	// Commit any completed lines through takeMarkdown, then show the unfinished
	// trailing line immediately via live.setPreview so every token appears live.
	rendered := a.takeMarkdown(turn, clean, false, false)
	if rendered != "" {
		a.live.clearPreview(false)
		if a.clawdEnabled() {
			a.writeClawdReply(turn, rendered, false)
		} else {
			a.Out.write(rendered)
		}
	}
	a.updateLiveMarkdownPreview(turn)
}

func (a *App) updateLiveMarkdownPreview(turn string) {
	if a.live == nil || a.md == nil {
		return
	}
	md := a.md[turn]
	if md == nil || md.buf == "" {
		a.live.clearPreview(true)
		return
	}
	preview := renderAgentLine(md.buf, ptrBool(md.fence), a.Out.bold, a.Out.dim)
	if a.clawdEnabled() {
		if a.shownTurn == nil || !a.shownTurn[turn] {
			preview = a.Out.bold("●") + " " + preview
		} else {
			preview = "  " + preview
		}
	}
	a.live.setPreview(a.Out.clean(preview))
}

func ptrBool(v bool) *bool { return &v }

// reconcileHonchoOutput merges an authoritative Honcho RoleOutput piece with
// any live deltas already displayed for meta.Turn. Returns true if handled
// (avoiding duplicate output); returns false to fall back to standard a.show().
func (a *App) reconcileHonchoOutput(content string, meta wire.Message) bool {
	if a.stream == nil {
		return false
	}
	st := a.stream.turns[meta.Turn]
	if st == nil || st.streamedRaw == "" {
		if st != nil && meta.Final {
			delete(a.stream.turns, meta.Turn)
		}
		return false
	}

	if !meta.OK {
		if a.live != nil {
			a.live.clearPreview(true)
		}
		if meta.Final {
			delete(a.stream.turns, meta.Turn)
		}
		return false
	}

	st.honchoRaw += content
	if strings.HasPrefix(st.streamedRaw, st.honchoRaw) {
		// Everything in Honcho so far was already streamed live.
		if meta.Final {
			a.finalizeStreamedTurn(meta.Turn, "", st)
			delete(a.stream.turns, meta.Turn)
		}
		return true
	}

	if strings.HasPrefix(st.honchoRaw, st.streamedRaw) {
		// Honcho has additional suffix bytes that were not streamed live.
		suffix := st.honchoRaw[len(st.streamedRaw):]
		st.streamedRaw = st.honchoRaw
		if meta.Final {
			a.finalizeStreamedTurn(meta.Turn, suffix, st)
			delete(a.stream.turns, meta.Turn)
		} else if suffix != "" {
			a.emitStreamDelta(meta.Turn, suffix, st)
		}
		return true
	}

	// Diverged (e.g., stream missed initial chunks): clear preview and let
	// authoritative Honcho output display cleanly.
	if a.live != nil {
		a.live.clearPreview(true)
	}
	if a.md != nil {
		delete(a.md, meta.Turn)
	}
	if meta.Final {
		delete(a.stream.turns, meta.Turn)
	}
	return false
}

func (a *App) finalizeStreamedTurn(turn, suffix string, st *streamTurnState) {
	if a.live == nil || st.pipedMode {
		if suffix != "" {
			a.Out.write(sanitize(suffix))
		}
		a.Out.endLine()
		if a.md != nil {
			delete(a.md, turn)
		}
		return
	}

	a.live.clearPreview(false)
	text := a.takeMarkdown(turn, sanitize(suffix), true, false)
	if a.clawdEnabled() {
		a.writeClawdReply(turn, text, true)
		return
	}
	if text != "" {
		a.Out.write(text)
	}
	a.Out.endLine()
}

// formatStreamStatus renders a short debug/status summary if needed.
func (a *App) streamActive() bool {
	if a.stream == nil {
		return false
	}
	a.stream.mu.Lock()
	defer a.stream.mu.Unlock()
	return a.stream.cancel != nil && !a.stream.unsupported
}

var _ = fmt.Sprintf
