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
	Prompted bool
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
	a.closeAskMenu()
	a.pendingAsk = ask
	if a.live != nil {
		a.ask(askBoxPrompt)
	}
}

func (a *App) clearPendingAsk() {
	a.closeAskMenu()
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
	if m.PeerID != a.Profile.Human || meta.Request == "" || strings.TrimSpace(m.Content) == "" {
		return false
	}
	label := wire.AnswerLabel(a.session, meta.Turn, meta.Request, a.Profile.Human)
	return a.humanAuth().Verify(m.Content, label, meta.TS, meta.Sig, meta.SigAlg)
}

// offeredAnswer also binds a signed owner answer to a genuine ask in this history.
func (a *App) offeredAnswer(m honcho.Message, meta wire.Message, messages []honcho.Message) bool {
	if !a.ownAnswer(m, meta) {
		return false
	}
	for _, question := range messages {
		ask, ok := parseMeta(question)
		if !ok || ask.Role != wire.RoleAsk || ask.Turn != meta.Turn || ask.Request != meta.Request || !a.genuineAsk(question, ask) {
			continue
		}
		for _, choice := range askChoices(&pendingAsk{Choices: ask.Choices}) {
			if choice == m.Content {
				return true
			}
		}
	}
	return false
}

// askChoices returns the effective choice list for ask, defaulting to allow/deny.
func askChoices(ask *pendingAsk) []string {
	if ask != nil && len(ask.Choices) > 0 {
		return ask.Choices
	}
	return []string{"allow", "deny"}
}

// parseAskAnswer parses a user's allow/deny reply to a pending permission question.
func parseAskAnswer(line string) (string, bool) {
	return parseAskAnswerFor(line, nil)
}

// parseAskAnswerFor parses a user's reply against the active ask's choices.
func parseAskAnswerFor(line string, choices []string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(line))
	if s == "" {
		return "", false
	}
	if len(choices) == 0 {
		choices = []string{"allow", "deny"}
	}
	has := func(want string) bool {
		for _, c := range choices {
			if c == want {
				return true
			}
		}
		return false
	}
	// Exact match against any advertised choice.
	for _, c := range choices {
		if strings.ToLower(c) == s {
			return c, true
		}
	}
	// Single-digit 1-based index into choices.
	if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		idx := int(s[0] - '1')
		if idx < len(choices) {
			return choices[idx], true
		}
	}
	switch s {
	case "allow", "y", "yes", "/allow":
		if has("allow") {
			return "allow", true
		}
	case "allow_session", "session", "s":
		if has("allow_session") {
			return "allow_session", true
		}
	case "allow_always", "always", "a":
		if has("allow_always") {
			return "allow_always", true
		}
	case "deny", "n", "no", "/deny":
		if has("deny") {
			return "deny", true
		}
	}
	return "", false
}

// askChoiceMeta returns a human-readable label, description, and optional single-key hotkey.
func askChoiceMeta(choice string) (label, desc string, hotkey rune) {
	switch choice {
	case "allow":
		return "Allow", "Allow this action once (y)", 'y'
	case "allow_session":
		return "Allow for session", "Allow matching actions in this session (s)", 's'
	case "allow_always":
		return "Always allow", "Request host-native persistence (a)", 'a'
	case "deny":
		return "Deny", "Refuse this action (n)", 'n'
	default:
		return oneLine(choice), "", 0
	}
}

// promptPendingAsk opens an interactive single-keypress picker for a pending
// permission question in live mode so the owner never has to type "allow".
func (a *App) promptPendingAsk() {
	if a.live == nil || a.pendingAsk == nil || a.pendingAsk.Prompted {
		return
	}
	ask := a.pendingAsk
	ask.Prompted = true
	choices := askChoices(ask)
	items := make([]menuItem, 0, len(choices))
	hotkeys := map[rune]string{}
	for _, c := range choices {
		label, desc, hk := askChoiceMeta(c)
		items = append(items, menuItem{
			value: c,
			label: label,
			desc:  desc,
		})
		if hk != 0 {
			hotkeys[hk] = c
		}
	}
	who := "bridge"
	if a.cur != nil {
		who = oneLine(a.cur.Name)
	}
	title := fmt.Sprintf("Permission · %s", who)
	if t := oneLine(ask.Tool); t != "" {
		title += fmt.Sprintf(" [%s]", t)
	}
	done := make(chan menuResult, 1)
	m := newSelectMenu(menuConfig{
		title: title, subtitle: "Choose an offered answer · PgUp/PgDn read details",
		details:  sanitize(ask.Question + "\n" + ask.Detail),
		noSearch: true, hotkeys: hotkeys, items: items,
	}, done)
	a.live.mu.Lock()
	defer a.live.mu.Unlock()
	if a.live.closed || a.live.menu != nil {
		ask.Prompted = false
		return
	}
	a.askMenu, a.askResult = m, done
	a.live.menu = m
	a.live.draw()
}

// closeAskMenu only closes this request's picker, never an unrelated settings menu.
func (a *App) closeAskMenu() {
	if a.live != nil && a.askMenu != nil {
		a.live.mu.Lock()
		if a.live.menu == a.askMenu {
			a.live.menu = nil
			a.live.draw()
		}
		a.live.mu.Unlock()
	}
	a.askMenu, a.askResult = nil, nil
}

func (a *App) expirePendingAsk() bool {
	if a.pendingAsk != nil && a.pendingAsk.Deadline > 0 && a.Now().Unix() >= a.pendingAsk.Deadline {
		turn := a.pendingAsk.Turn
		a.clearPendingAsk()
		a.Out.line(a.Out.warn("permission question expired; no answer sent"))
		if len(a.waiting) > 0 {
			a.notifyWorking(turn, "", "")
		} else {
			a.notifyBoundary()
		}
		return true
	}
	return false
}

// servicePendingAsk runs on the main loop, so picker input never blocks
// heartbeats, polling, expiry, or host acknowledgments.
func (a *App) servicePendingAsk() {
	if a.expirePendingAsk() {
		return
	}
	select {
	case res := <-a.askResult:
		a.closeAskMenu()
		if !res.ok {
			a.Out.line(a.Out.dim("permission dialog dismissed · /ask reopens; type an offered choice or number"))
			return
		}
		if err := a.sendAnswer(res.value); err != nil {
			a.Out.line(a.Out.fail("✗ " + err.Error()))
		}
	default:
	}
	a.promptPendingAsk()
}

// sendAnswer posts a signed role=answer for the current pendingAsk.
func (a *App) sendAnswer(choice string) error {
	if a.expirePendingAsk() {
		return errors.New("permission question expired")
	}
	if a.pendingAsk == nil {
		return errors.New("no permission question is waiting for an answer")
	}
	choices := askChoices(a.pendingAsk)
	valid := false
	for _, c := range choices {
		if c == choice {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("invalid answer %q: choose one of %s", choice, strings.Join(choices, ", "))
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
	if len(a.waiting) > 0 {
		a.notifyWorking(ask.Turn, "", "")
	}
	if a.cur.endpoint != "" {
		_ = wire.Ring(a.ctx, a.Honcho.HTTP(), a.cur.endpoint)
	}
	what := oneLine(ask.Tool)
	if what != "" {
		what = " (" + what + ")"
	}
	switch choice {
	case "allow":
		a.Out.line(a.Out.accent("✓") + " allow answer sent" + what)
	case "allow_session":
		a.Out.line(a.Out.accent("✓") + " session answer sent; awaiting host acknowledgment" + what)
	case "allow_always":
		a.Out.line(a.Out.accent("✓") + " always-allow answer sent; awaiting host acknowledgment (not yet confirmed saved)" + what)
	case "deny":
		a.Out.line(a.Out.warn("✗") + " deny answer sent" + what)
	default:
		a.Out.line(a.Out.accent("✓") + " answered " + oneLine(choice) + what)
	}
	return nil
}
