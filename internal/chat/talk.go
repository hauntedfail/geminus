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

// meta is the open conversation as its session metadata describes it.
func (a *App) meta(title string, pending bool) wire.Session {
	return wire.Session{V: 1, Human: a.Profile.Human, Persona: a.cur.Name, Harness: a.harness.ID,
		CWD: a.cwd, Title: title, Pending: pending, Updated: a.Now().Unix()}
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
		if err := a.Honcho.CreateSession(ctx, id, wire.SessionMetadata(a.meta(a.title, false)), a.Profile.Human, a.cur.Name); err != nil {
			return fmt.Errorf("nothing was sent: the conversation could not be created: %w", err)
		}
		a.session = id
	}
	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	ts := a.Now().Unix()
	label := wire.PromptLabel(a.session, a.cur.Name, a.harness.ID, a.Profile.Human, nonce)
	prompt := wire.Message{V: 1, Role: wire.RolePrompt, TS: ts, Nonce: nonce, State: wire.StateQueued,
		Sig: wire.Sign(a.Profile.Secret, text, label, ts)}
	stored, err := a.Honcho.Post(ctx, a.session, a.Profile.Human, text, map[string]any{wire.Kind: prompt})
	if err != nil {
		return fmt.Errorf("nothing was sent: %w", err)
	}
	a.shown[stored.ID], a.waiting[stored.ID] = true, true
	// The mark is what the bridge looks for. Without it the prompt is
	// stored but nobody picks it up, so say exactly that.
	if err := a.Honcho.SetSessionMetadata(ctx, a.session, wire.SessionMetadata(a.meta(a.title, true))); err != nil {
		return fmt.Errorf("the prompt is stored but could not be marked for %s to pick up (%w); send it again", a.cur.Name, err)
	}
	note := "recorded in Honcho for " + a.cur.Name
	if a.cur.endpoint != "" && wire.Ring(a.ctx, a.Honcho.HTTP(), a.cur.endpoint) == nil {
		note += " · doorbell rung"
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
			if !a.shown[m.ID] && m.PeerID == a.Profile.Human {
				// Typed in geminus on another computer.
				a.shown[m.ID], a.waiting[m.ID] = true, true
				a.Out.line(a.Out.dim("you, from another geminus: ") + sanitize(m.Content))
				printed = true
			}
			if !a.waiting[m.ID] || a.state[m.ID] == meta.State {
				continue
			}
			a.state[m.ID] = meta.State
			switch meta.State {
			case wire.StateRunning:
				a.Out.line(a.Out.dim(fmt.Sprintf("%s · %s is working…", a.cur.Name, a.harness.Name)))
				printed = true
			case wire.StateRefused:
				a.Out.line(a.Out.fail(fmt.Sprintf("✗ %s refused the prompt: %s", a.cur.Name, oneLine(meta.Detail))))
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
	label := wire.OutputLabel(a.session, meta.Turn, meta.Seq, meta.Final, meta.OK)
	return m.PeerID == a.cur.Name && wire.Verify(a.Profile.Secret, m.Content, label, meta.TS, meta.Sig)
}

// show prints one piece of an agent's output.
func (a *App) show(content string, meta wire.Message) {
	if !meta.OK {
		a.Out.line(a.Out.fail("✗ " + strings.TrimRight(sanitize(content), "\n")))
		return
	}
	a.Out.write(sanitize(content))
	if meta.Final {
		a.Out.line("")
	}
}

// conversations lists the person's conversations with the current persona,
// newest first.
func (a *App) conversations() ([]honcho.Session, error) {
	ctx, cancel := a.call()
	defer cancel()
	sessions, err := a.Honcho.Sessions(ctx, map[string]any{"kind": wire.Kind,
		wire.Kind: map[string]any{"persona": a.cur.Name, "human": a.Profile.Human}})
	if err != nil {
		return nil, err
	}
	// Honcho lists by creation; the latest conversation is the one last
	// written to.
	updated := func(s honcho.Session) int64 {
		info, _ := sessionInfo(s)
		return info.Updated
	}
	sort.SliceStable(sessions, func(i, j int) bool { return updated(sessions[i]) > updated(sessions[j]) })
	return sessions, nil
}

func (a *App) showSessions() error {
	if a.cur == nil {
		return errors.New("pick a persona first (/persona)")
	}
	sessions, err := a.conversations()
	if err != nil {
		return err
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
	sessions, err := a.conversations()
	if err != nil {
		return err
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
	a.shown, a.waiting, a.state = map[string]bool{}, map[string]bool{}, map[string]string{}

	a.Out.line(a.Out.dim("── conversation so far ──"))
	for _, m := range messages {
		meta, ok := parseMeta(m)
		if !ok {
			continue
		}
		a.shown[m.ID] = true
		switch {
		case meta.Role == wire.RolePrompt && m.PeerID == a.Profile.Human:
			a.Out.line(a.Out.dim("you: ") + sanitize(m.Content))
			a.state[m.ID] = meta.State
			if meta.State == wire.StateQueued || meta.State == wire.StateRunning {
				a.waiting[m.ID] = true
			}
		case meta.Role == wire.RoleOutput && a.genuine(m, meta):
			a.show(m.Content, meta)
			if meta.Final {
				delete(a.waiting, meta.Turn)
			}
		}
	}
	a.Out.line(a.Out.dim("── end of history ──"))
	if len(a.waiting) > 0 {
		a.Out.line(a.Out.dim(fmt.Sprintf("%d prompt(s) not answered yet; the answer appears when it arrives", len(a.waiting))))
	}
	return nil
}
