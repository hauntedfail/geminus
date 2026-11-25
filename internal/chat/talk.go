package chat

// Conversations: writing prompts into Honcho and showing what comes back.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/wire"
)

// latestCount is how many of the newest messages are read while watching.
const latestCount = 50

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("no random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// humanAuth builds the signer/verifier for human-authored records.
func (a *App) humanAuth() wire.HumanAuth {
	return wire.HumanAuth{
		Secret:         a.Profile.Secret,
		Priv:           a.Profile.HumanKey,
		Pub:            a.Profile.HumanPubKey,
		Pubs:           a.Profile.HumanPubKeys,
		RequireEd25519: a.Profile.RequireEd25519,
	}
}

// meta is the signed description of the conversation with the given ID.
func (a *App) meta(id, title string, pending bool) wire.Session {
	return wire.SealSessionWith(a.humanAuth(), id, wire.Session{V: 1, Human: a.Profile.Human, Persona: a.cur.Name,
		Harness: a.harness.ID, CWD: a.cwd, Model: a.tuning.Model, Effort: a.tuning.Effort, Title: title,
		Pending: pending, Updated: a.Now().Unix()})
}

func parseMeta(m honcho.Message) (wire.Message, bool) {
	var meta wire.Message
	raw := m.Metadata[wire.Kind]
	if len(raw) == 0 || json.Unmarshal(raw, &meta) != nil || meta.Role == "" {
		return wire.Message{}, false
	}
	return meta, true
}

func sessionInfo(s honcho.Session) (wire.Session, bool) {
	var info wire.Session
	if len(s.Metadata[wire.Kind]) == 0 || json.Unmarshal(s.Metadata[wire.Kind], &info) != nil {
		return wire.Session{}, false
	}
	return info, true
}

// say sends one prompt, word for word, and shows the answer as it arrives.
func (a *App) say(text string) error {
	if a.cur == nil || a.harness == nil {
		return errors.New("nothing was sent: pick a persona first (/persona)")
	}
	ctx, cancel := a.call()
	defer cancel()
	if a.session == "" {
		suffix, err := randomHex(3)
		if err != nil {
			return err
		}
		id := fmt.Sprintf("geminus-%s-%s-%s", a.cur.Name, a.Now().Format("20060102"), suffix)
		a.title = clip(oneLine(text), 60)
		if err := a.Honcho.CreateSession(ctx, id, wire.SessionMetadata(a.meta(id, a.title, false)), a.Profile.Human, a.cur.Name); err != nil {
			return fmt.Errorf("nothing was sent: the conversation could not be created: %w", err)
		}
		a.session = id
	}
	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	ts := a.Now().Unix()
	label := wire.PromptLabel(a.session, a.cur.Name, a.harness.ID, a.Profile.Human, nonce, a.cwd, a.tuning)
	sig, sigAlg := a.humanAuth().Sign(text, label, ts)
	prompt := wire.Message{V: 1, Role: wire.RolePrompt, TS: ts, Nonce: nonce, State: wire.StateQueued,
		Model: a.tuning.Model, Effort: a.tuning.Effort, Sig: sig, SigAlg: sigAlg}
	stored, err := a.Honcho.Post(ctx, a.session, a.Profile.Human, text, map[string]any{wire.Kind: prompt})
	if err != nil {
		return fmt.Errorf("nothing was sent: %w", err)
	}
	a.shown[stored.ID], a.waiting[stored.ID] = true, true
	if a.turnPrompt == nil {
		a.turnPrompt = map[string]string{}
	}
	a.turnPrompt[stored.ID] = text
	// The mark is what the bridge looks for. Without it the prompt is
	// stored but nobody picks it up, so say exactly that.
	if err := a.Honcho.SetSessionMetadata(ctx, a.session, wire.SessionMetadata(a.meta(a.session, a.title, true))); err != nil {
		return fmt.Errorf("the prompt is stored but could not be marked for %s to pick up (%w); send it again", a.cur.Name, err)
	}
	a.state[stored.ID] = wire.StateQueued
	note := "recorded in Honcho for " + a.cur.Name
	rang := a.cur.endpoint != "" && wire.Ring(a.ctx, a.Honcho.HTTP(), a.cur.endpoint) == nil
	if rang {
		note += " · doorbell rung"
	}
	if a.live != nil {
		// The line above the box says how far the prompt got, and the box
		// takes the next one meanwhile: the answer shows when it arrives.
		a.sentAt[stored.ID], a.rang = a.Now(), rang
		return nil
	}
	a.Out.line(a.Out.dim(note))
	return a.watch(stored.ID)
}

// watch shows the answer to one prompt until it ends or the person stops
// watching. Stopping does not stop the agent.
func (a *App) watch(turn string) error {
	started := a.Now()
	hinted := false
	ticker := time.NewTicker(a.Watch)
	defer ticker.Stop()
	for {
		done, _, err := a.poll(turn)
		if err != nil {
			return fmt.Errorf("the prompt was sent, but its answer cannot be read right now: %w", err)
		}
		if done {
			return nil
		}
		if !hinted && a.state[turn] == wire.StateQueued && a.Now().Sub(started) > a.SlowAfter {
			hinted = true
			a.Out.line(a.Out.dim(fmt.Sprintf("%s has not picked it up yet (bridge seen %s). It stays in Honcho and runs when the bridge reads it.",
				a.cur.Name, a.ago(a.cur.Seen))))
		}
		select {
		case <-ticker.C:
		case <-a.interrupt:
			a.Out.line(a.Out.dim("(not watching any more; the agent keeps working and the answer appears when it arrives)"))
			return nil
		case <-a.ctx.Done():
			return a.ctx.Err()
		}
	}
}

// arrivals shows what arrived for the open conversation while idle. It
// reports whether anything was printed.
func (a *App) arrivals() bool {
	_, printed, _ := a.poll("")
	return printed
}

// poll reads the newest messages of the open conversation and prints what is
// new. done reports that the answer to turn has ended.
func (a *App) poll(turn string) (done, printed bool, err error) {
	ctx, cancel := a.call()
	defer cancel()
	messages, err := a.Honcho.Latest(ctx, a.session, latestCount)
	if err != nil {
		return false, false, err
	}
	for _, m := range messages {
		meta, ok := parseMeta(m)
		if !ok {
			continue
		}
		switch meta.Role {
		case wire.RolePrompt:
			if !a.shown[m.ID] {
				a.shown[m.ID] = true
				printed = true
				if !a.own(m, meta) {
					// Written under the owner's name without the owner's signature.
					a.Out.line(a.Out.warn("! ignored a message in this conversation that claims to be yours but is not signed"))
					continue
				}
				// Typed in geminus on another computer.
				a.waiting[m.ID] = true
				if a.turnPrompt == nil {
					a.turnPrompt = map[string]string{}
				}
				a.turnPrompt[m.ID] = m.Content
				if a.live != nil {
					a.live.setPrompt(m.Content)
					a.Out.line("")
					a.Out.line(a.Out.dim("── ") + a.Out.accent("you (another geminus)") + a.Out.dim(" ──────────"))
					a.Out.line(a.Out.accent("❯ ") + a.Out.bold(sanitize(m.Content)))
				} else {
					a.Out.line(a.Out.dim("you, from another geminus: ") + sanitize(m.Content))
				}
			}
			if !a.waiting[m.ID] || a.state[m.ID] == meta.State {
				continue
			}
			a.state[m.ID] = meta.State
			switch meta.State {
			case wire.StateRunning:
				if a.live == nil { // with the box, the line above it says so
					a.Out.line(a.Out.dim(fmt.Sprintf("%s · %s is working…", a.cur.Name, a.harness.Name)))
					printed = true
				}
			case wire.StateRefused:
				a.Out.line(a.Out.fail(fmt.Sprintf("✗ %s refused the prompt: %s", a.cur.Name, oneLine(meta.Detail))))
				if hint := wire.RefusalHint(a.cur.Name, a.cur.Card, meta.Detail, len(a.humanAuth().Priv) > 0); hint != "" {
					a.Out.line(a.Out.warn("! " + hint))
				}
				delete(a.waiting, m.ID)
				printed = true
				done = done || m.ID == turn
			}
		case wire.RoleOutput:
			if a.shown[m.ID] {
				continue
			}
			a.shown[m.ID] = true
			if !a.genuine(m, meta) {
				a.Out.line(a.Out.warn(fmt.Sprintf("! ignored a message in this conversation that is not signed by %s's computer", a.cur.Name)))
				printed = true
				continue
			}
			printed = true
			a.show(m.Content, meta)
			if meta.Final {
				delete(a.waiting, meta.Turn)
				done = done || meta.Turn == turn
			}
		}
	}
	return done, printed, nil
}

// genuine reports whether an output message was written by the persona's
// bridge: Honcho itself does not prove who wrote a message.
func (a *App) genuine(m honcho.Message, meta wire.Message) bool {
	label := wire.OutputLabel(a.session, meta.Turn, meta.Seq, meta.Final, meta.OK, meta.Native)
	return m.PeerID == a.cur.Name && wire.Verify(a.Profile.Secret, m.Content, label, meta.TS, meta.Sig)
}

// own reports whether a prompt message was signed by the owner for this
// conversation, harness and directory, with the settings it carries.
func (a *App) own(m honcho.Message, meta wire.Message) bool {
	label := wire.PromptLabel(a.session, a.cur.Name, a.harness.ID, a.Profile.Human, meta.Nonce, a.cwd,
		wire.Tuning{Model: meta.Model, Effort: meta.Effort})
	return m.PeerID == a.Profile.Human && a.humanAuth().Verify(m.Content, label, meta.TS, meta.Sig, meta.SigAlg)
}

// show prints one piece of an agent's output.
func (a *App) show(content string, meta wire.Message) {
	if a.live != nil {
		if p := a.turnPrompt[meta.Turn]; p != "" {
			a.live.setPrompt(p)
		}
		if a.shownTurn == nil {
			a.shownTurn = map[string]bool{}
		}
		if !a.shownTurn[meta.Turn] {
			a.shownTurn[meta.Turn] = true
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
	if !meta.OK {
		a.Out.line(a.Out.fail("✗ " + strings.TrimRight(sanitize(content), "\n")))
		return
	}
	a.Out.write(sanitize(content))
	if meta.Final && a.live != nil {
		a.Out.endLine() // the box keeps its own distance
	} else if meta.Final {
		a.Out.line("")
	}
}

// conversations lists the person's conversations with the current persona,
// most recently used first, and how many sessions in Honcho claim to be such
// a conversation without the group's signature.
//
// An unsigned description is not a conversation: anyone holding the Honcho
// token can create a session that names a harness and a directory, and
// reopening it would send the owner's next prompt there.
func (a *App) conversations() (signed []honcho.Session, unsigned int, err error) {
	ctx, cancel := a.call()
	defer cancel()
	sessions, err := a.Honcho.Sessions(ctx, map[string]any{"kind": wire.Kind,
		wire.Kind: map[string]any{"persona": a.cur.Name, "human": a.Profile.Human}})
	if err != nil {
		return nil, 0, err
	}
	updated := map[string]int64{}
	for _, s := range sessions {
		info, ok := sessionInfo(s)
		if !ok || info.Persona != a.cur.Name || info.Human != a.Profile.Human || !wire.VerifySessionWith(a.humanAuth(), s.ID, info) {
			unsigned++
			continue
		}
		updated[s.ID] = info.Updated
		signed = append(signed, s)
	}
	// Honcho lists by creation; the latest conversation is the one last
	// written to.
	sort.SliceStable(signed, func(i, j int) bool { return updated[signed[i].ID] > updated[signed[j].ID] })
	return signed, unsigned, nil
}

// unsignedNote says that sessions were left out, when any were.
func (a *App) unsignedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d session(s) in Honcho claim to be your conversations with %s but are not signed; they are ignored", n, a.cur.Name)
}

func (a *App) showSessions() error {
	if a.cur == nil {
		return errors.New("pick a persona first (/persona)")
	}
	sessions, unsigned, err := a.conversations()
	if err != nil {
		return err
	}
	if note := a.unsignedNote(unsigned); note != "" {
		a.Out.line(a.Out.warn("! " + note))
	}
	if len(sessions) == 0 {
		a.Out.line("  no conversations with " + a.cur.Name + " yet")
	}
	for i, s := range sessions {
		info, _ := sessionInfo(s)
		mark := " "
		if s.ID == a.session {
			mark = "●"
		}
		title := oneLine(info.Title)
		if title == "" {
			title = "(untitled)"
		}
		a.Out.line(fmt.Sprintf("%s %2d  %s  %-8s  %s", mark, i+1, oneLine(s.ID), oneLine(info.Harness), title))
	}
	return nil
}

// resume reopens a conversation: the latest, a number from /sessions, an ID,
// or the unique end of one.
func (a *App) resume(want string) error {
	if a.cur == nil {
		return errors.New("pick a persona first (/persona)")
	}
	sessions, unsigned, err := a.conversations()
	if err != nil {
		return err
	}
	if note := a.unsignedNote(unsigned); note != "" {
		a.Out.line(a.Out.warn("! " + note))
	}
	if len(sessions) == 0 {
		return fmt.Errorf("there are no conversations with %s yet", a.cur.Name)
	}
	var picked *honcho.Session
	var matches []int
	for i, s := range sessions {
		if s.ID == want {
			matches = []int{i}
			break
		}
		if want != "" && (strings.HasSuffix(s.ID, want) || strings.HasPrefix(s.ID, want)) {
			matches = append(matches, i)
		}
	}
	// A number means a line of /sessions; it wins over an ID that happens
	// to start or end with the same digits.
	n, numErr := strconv.Atoi(want)
	exact := len(matches) == 1 && sessions[matches[0]].ID == want
	switch {
	case want == "":
		picked = &sessions[0]
	case exact:
		picked = &sessions[matches[0]]
	case numErr == nil && n >= 1 && n <= len(sessions):
		picked = &sessions[n-1]
	case len(matches) == 1:
		picked = &sessions[matches[0]]
	case len(matches) > 1:
		return fmt.Errorf("%q matches %d conversations; type more of the ID", want, len(matches))
	default:
		return fmt.Errorf("no conversation %q with %s (see /sessions)", want, a.cur.Name)
	}
	info, ok := sessionInfo(*picked)
	if !ok {
		return fmt.Errorf("conversation %s has no readable description", picked.ID)
	}
	ctx, cancel := a.call()
	defer cancel()
	messages, err := a.Honcho.Messages(ctx, picked.ID)
	if err != nil {
		return err
	}

	// A conversation belongs to the harness and directory it was started with.
	harness := wire.Harness{ID: info.Harness, Name: info.Harness}
	for _, h := range a.cur.Harnesses {
		if h.ID == info.Harness {
			harness = h
		}
	}
	a.harness, a.session, a.cwd, a.title = &harness, picked.ID, info.CWD, info.Title
	a.tuning = wire.Tuning{Model: info.Model, Effort: info.Effort}
	a.shown, a.waiting, a.state = map[string]bool{}, map[string]bool{}, map[string]string{}
	a.shownTurn, a.turnPrompt = map[string]bool{}, map[string]string{}

	// The description was verified when the conversations were listed. As a
	// second check, the prompts were signed for the same harness and
	// directory: if there are prompts and none verifies, something is wrong
	// and the next prompt must not go there.
	prompts, verified := 0, 0
	for _, m := range messages {
		if meta, ok := parseMeta(m); ok && meta.Role == wire.RolePrompt {
			prompts++
			if a.own(m, meta) {
				verified++
			}
		}
	}
	if prompts > 0 && verified == 0 {
		a.harness, a.session, a.cwd, a.title, a.tuning = nil, "", "", "", wire.Tuning{}
		return fmt.Errorf("conversation %s is not reopened: its harness or directory in Honcho does not match what its prompts were signed for", picked.ID)
	}

	a.Out.line(a.Out.dim("── conversation so far ──"))
	unsigned = 0
	for _, m := range messages {
		meta, ok := parseMeta(m)
		if !ok {
			continue
		}
		a.shown[m.ID] = true
		switch {
		case meta.Role == wire.RolePrompt && a.own(m, meta):
			a.turnPrompt[m.ID] = m.Content
			if a.live != nil {
				a.live.setPrompt(m.Content)
				a.Out.line("")
				a.Out.line(a.Out.dim("── ") + a.Out.accent("you") + a.Out.dim(" ────────────────────────────"))
				for i, line := range strings.Split(m.Content, "\n") {
					mark := a.Out.dim("│ ")
					if i == 0 {
						mark = a.Out.accent("❯ ")
					}
					a.Out.line(mark + a.Out.bold(sanitize(line)))
				}
			} else {
				a.Out.line(a.Out.dim("you: ") + sanitize(m.Content))
			}
			a.state[m.ID] = meta.State
			if meta.State == wire.StateQueued || meta.State == wire.StateRunning {
				a.waiting[m.ID] = true
			}
		case meta.Role == wire.RoleOutput && a.genuine(m, meta):
			a.show(m.Content, meta)
			if meta.Final {
				delete(a.waiting, meta.Turn)
			}
		default:
			unsigned++
		}
	}
	if unsigned > 0 {
		a.Out.line(a.Out.warn(fmt.Sprintf("! %d message(s) in this conversation are not signed and are not shown", unsigned)))
	}
	a.Out.line(a.Out.dim("── end of history ──"))
	if len(a.waiting) > 0 {
		a.Out.line(a.Out.dim(fmt.Sprintf("%d prompt(s) not answered yet; the answer appears when it arrives", len(a.waiting))))
	}
	return nil
}
