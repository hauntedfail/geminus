package chat

// Presence heartbeats and interactive permission questions (role=ask / role=answer)
// exchanged with resident bridges (geminish 0.10+).

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/wire"
)

const askBoxPrompt = "allow / deny"

// pendingAsk is a bridge-emitted permission question awaiting the owner's answer.
type pendingAsk struct {
	Turn     string
	Request  string
	Tool     string
	Question string
	Detail   string
	Choices  []string
	Deadline int64
}

// publishPresence writes a signed geminus_presence heartbeat to the human
// peer's metadata so a resident bridge knows this conversation is open and
// posts permission questions instead of holding them.
func (a *App) publishPresence(force bool) {
	if a.session == "" || a.Honcho == nil || a.Profile.Human == "" {
		return
	}
	every := a.PresenceEvery
	if every <= 0 {
		every = presenceEvery
	}
	now := a.Now()
	if !force && a.presenceSession == a.session && !a.presenceSent.IsZero() && now.Sub(a.presenceSent) < every {
		return
	}
	rec := wire.SealPresenceWith(a.humanAuth(), a.Profile.Human, a.session, now.Unix())
	ctx, cancel := a.call()
	defer cancel()
	if err := a.Honcho.SetPeerMetadata(ctx, a.Profile.Human, wire.PresenceKey, rec); err != nil {
		return
	}
	a.presenceSession = a.session
	a.presenceSent = now
}

func (a *App) resetAskState() {
	a.clearPendingAsk()
	a.answeredAsk = map[string]bool{}
}

func (a *App) setPendingAsk(ask *pendingAsk) {
	a.pendingAsk = ask
	if a.live != nil {
		a.ask(askBoxPrompt)
	}
}

func (a *App) clearPendingAsk() {
	a.pendingAsk = nil
	if a.live != nil && a.asking == askBoxPrompt {
		a.ask("")
	}
}

func (a *App) markAskAnswered(request string) {
	if request == "" {
		return
	}
	if a.answeredAsk == nil {
		a.answeredAsk = map[string]bool{}
	}
	a.answeredAsk[request] = true
	if a.pendingAsk != nil && a.pendingAsk.Request == request {
		a.clearPendingAsk()
	}
}

// genuineState reports whether a role=state message was signed by the persona's bridge.
func (a *App) genuineState(m honcho.Message, meta wire.Message) bool {
	if a.cur == nil || m.PeerID != a.cur.Name || meta.Phase == "" {
		return false
	}
	label := wire.StateLabel(a.session, meta.Turn, meta.Phase)
	return wire.Verify(a.Profile.Secret, m.Content, label, meta.TS, meta.Sig)
}

// genuineAsk reports whether a role=ask message was signed by the persona's bridge.
func (a *App) genuineAsk(m honcho.Message, meta wire.Message) bool {
	if a.cur == nil || m.PeerID != a.cur.Name || meta.Request == "" {
		return false
	}
	choices := strings.Join(meta.Choices, ",")
	if choices == "" {
		choices = "allow,deny"
	}
	label := wire.AskLabel(a.session, meta.Turn, meta.Request, m.Content, choices)
	return wire.Verify(a.Profile.Secret, m.Content, label, meta.TS, meta.Sig)
}

// ownAnswer reports whether a role=answer message was signed by the owner.
func (a *App) ownAnswer(m honcho.Message, meta wire.Message) bool {
	if m.PeerID != a.Profile.Human || meta.Request == "" || (m.Content != "allow" && m.Content != "deny") {
		return false
	}
	label := wire.AnswerLabel(a.session, meta.Turn, meta.Request, a.Profile.Human)
	return a.humanAuth().Verify(m.Content, label, meta.TS, meta.Sig, meta.SigAlg)
}

// parseAskAnswer parses a user's allow/deny reply to a pending permission question.
func parseAskAnswer(line string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "allow", "y", "yes", "/allow":
		return "allow", true
	case "deny", "n", "no", "/deny":
		return "deny", true
	default:
		return "", false
	}
}

// sendAnswer posts a signed role=answer ("allow" or "deny") for the current pendingAsk.
func (a *App) sendAnswer(choice string) error {
	if a.pendingAsk == nil {
		return errors.New("no permission question is waiting for an answer")
	}
	if choice != "allow" && choice != "deny" {
		return fmt.Errorf("invalid answer %q: use allow or deny", choice)
	}
	if a.session == "" || a.cur == nil {
		a.clearPendingAsk()
		return errors.New("no open conversation for the pending question")
	}
	ask := a.pendingAsk
	ts := a.Now().Unix()
	label := wire.AnswerLabel(a.session, ask.Turn, ask.Request, a.Profile.Human)
	sig, sigAlg := a.humanAuth().Sign(choice, label, ts)
	msg := wire.Message{
		V:       1,
		Role:    wire.RoleAnswer,
		Turn:    ask.Turn,
		Request: ask.Request,
		TS:      ts,
		Sig:     sig,
		SigAlg:  sigAlg,
	}
	ctx, cancel := a.call()
	defer cancel()
	stored, err := a.Honcho.Post(ctx, a.session, a.Profile.Human, choice, map[string]any{wire.Kind: msg})
	if err != nil {
		return fmt.Errorf("answer was not sent: %w", err)
	}
	if a.shown == nil {
		a.shown = map[string]bool{}
	}
	a.shown[stored.ID] = true
	a.markAskAnswered(ask.Request)
	if a.cur.endpoint != "" {
		_ = wire.Ring(a.ctx, a.Honcho.HTTP(), a.cur.endpoint)
	}
	what := oneLine(ask.Tool)
	if what != "" {
		what = " (" + what + ")"
	}
	if choice == "allow" {
		a.Out.line(a.Out.accent("✓") + " allowed" + what)
	} else {
		a.Out.line(a.Out.warn("✗") + " denied" + what)
	}
	return nil
}
