package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/wire"
)

func TestPresencePublishedOnPromptAndResume(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"hello", "echo: hello"})
	sid := w.srv.Sessions()[0].ID

	peers, err := w.app.Honcho.Peers(context.Background())
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	var got wire.Presence
	found := false
	for _, p := range peers {
		if p.ID != w.prof.Human {
			continue
		}
		raw := p.Metadata[wire.PresenceKey]
		if len(raw) == 0 {
			t.Fatalf("human peer metadata missing %s", wire.PresenceKey)
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal presence: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatalf("human peer %s not found", w.prof.Human)
	}
	if got.Session != sid {
		t.Fatalf("presence session = %q, want %q", got.Session, sid)
	}
	if !wire.VerifyPresenceWith(w.app.humanAuth(), w.prof.Human, got) {
		t.Fatalf("presence signature did not verify: %+v", got)
	}
}

func TestBridgeAskAndSignedAnswer(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"hello", "echo: hello"})
	sid := w.srv.Sessions()[0].ID

	// Inject a signed ask on a new turn and poll it.
	ts := now.Unix()
	turn := "turn-ask-1"
	reqID := "req-abc123"
	question := "Run node scripts/harness/good-morning.mjs?"
	w.srv.Append(sid, "sibyl", question, map[string]any{wire.Kind: wire.Message{
		V: 1, Role: wire.RoleAsk, Turn: turn, Request: reqID, Tool: "Bash",
		Detail: "node scripts/harness/good-morning.mjs", Choices: []string{"allow", "deny"},
		Deadline: ts + 600, TS: ts,
		Sig: wire.Sign(w.prof.Secret, question, wire.AskLabel(sid, turn, reqID, question, "allow,deny"), ts),
	}})

	// Also inject an unsigned forged ask; it must be ignored.
	w.srv.Append(sid, "sibyl", "Forged question?", map[string]any{wire.Kind: wire.Message{
		V: 1, Role: wire.RoleAsk, Turn: turn, Request: "forged-1", Tool: "Bash", TS: ts, Sig: "deadbeef",
	}})

	if _, _, err := w.app.poll(turn); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if w.app.pendingAsk == nil || w.app.pendingAsk.Request != reqID {
		t.Fatalf("pendingAsk = %+v, want request %q", w.app.pendingAsk, reqID)
	}
	w.expect("! sibyl asks [Bash]: "+question, "type allow (y) or deny (n)")
	w.expectNot("Forged question?")

	// Sending a non-answer text while pendingAsk is active should be blocked with guidance.
	if err := w.app.handle("unrelated chat"); err == nil || !strings.Contains(err.Error(), "permission question pending") {
		t.Fatalf("expected pending ask guidance error, got %v", err)
	}
	if w.app.pendingAsk == nil {
		t.Fatalf("pendingAsk should remain set after rejected non-answer")
	}

	// Answering "y" posts a signed role=answer ("allow") and clears pendingAsk.
	if err := w.app.handle("y"); err != nil {
		t.Fatalf("handle(y): %v", err)
	}
	if w.app.pendingAsk != nil {
		t.Fatalf("pendingAsk should be cleared after answering, got %+v", w.app.pendingAsk)
	}

	msgs, err := w.app.Honcho.Messages(context.Background(), sid)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	var answered bool
	for _, m := range msgs {
		meta, ok := parseMeta(m)
		if !ok || meta.Role != wire.RoleAnswer {
			continue
		}
		if meta.Turn == turn && meta.Request == reqID && m.Content == "allow" {
			if !w.app.ownAnswer(m, meta) {
				t.Fatalf("posted role=answer signature did not verify")
			}
			answered = true
		}
	}
	if !answered {
		t.Fatalf("expected signed role=answer for request %q in Honcho", reqID)
	}

	// Reopening the session should verify the genuine ask and role=answer cleanly,
	// count only the 1 forged ask as unsigned, and not re-arm the already-answered ask.
	w.newApp()
	w.talk([]string{"persona", "sibyl", "--continue"})
	w.expect("you answered: allow", "! 1 message(s) in this conversation are not signed and are not shown")
	w.expectNot("Forged question?")
	if w.app.pendingAsk != nil {
		t.Fatalf("already-answered ask should not re-arm on resume: %+v", w.app.pendingAsk)
	}
}

func TestMultipleSequentialAsksInSameTurn(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"hello", "echo: hello"})
	sid := w.srv.Sessions()[0].ID

	ts := now.Unix()
	turn := "turn-multi-ask"
	appendAsk := func(reqID, q string) {
		w.srv.Append(sid, "sibyl", q, map[string]any{wire.Kind: wire.Message{
			V: 1, Role: wire.RoleAsk, Turn: turn, Request: reqID, Tool: "Bash",
			Detail: q, Choices: []string{"allow", "deny"}, Deadline: ts + 1800, TS: ts,
			Sig: wire.Sign(w.prof.Secret, q, wire.AskLabel(sid, turn, reqID, q, "allow,deny"), ts),
		}})
	}
	appendState := func(phase, text string) {
		w.srv.Append(sid, "sibyl", text, map[string]any{wire.Kind: wire.Message{
			V: 1, Role: wire.RoleState, Turn: turn, Phase: phase, Detail: text, TS: ts,
			Sig: wire.Sign(w.prof.Secret, text, wire.StateLabel(sid, turn, phase), ts),
		}})
	}

	// Ask #1 arrives -> user allows -> bridge emits state=answered for the same turn.
	appendAsk("req-1", "Run good-morning.mjs")
	if _, _, err := w.app.poll(turn); err != nil {
		t.Fatalf("poll ask 1: %v", err)
	}
	if w.app.pendingAsk == nil || w.app.pendingAsk.Request != "req-1" {
		t.Fatalf("pendingAsk after ask 1 = %+v, want req-1", w.app.pendingAsk)
	}
	if err := w.app.handle("allow"); err != nil {
		t.Fatalf("answer ask 1: %v", err)
	}
	appendState("answered", "answered: allow")

	// Ask #2 arrives in the SAME turn after state=answered: must arm pendingAsk.
	appendAsk("req-2", "Run git status")
	if _, _, err := w.app.poll(turn); err != nil {
		t.Fatalf("poll ask 2: %v", err)
	}
	if w.app.pendingAsk == nil || w.app.pendingAsk.Request != "req-2" {
		t.Fatalf("pendingAsk after ask 2 = %+v, want req-2 (state=answered must not poison the turn)", w.app.pendingAsk)
	}

	// Ask #2 times out -> bridge emits state=timeout -> Ask #3 arrives in the SAME turn.
	appendState("timeout", "timeout: no answer in 1800 seconds")
	appendAsk("req-3", "Run fetch_backlog_updates.mjs")
	if _, _, err := w.app.poll(turn); err != nil {
		t.Fatalf("poll ask 3: %v", err)
	}
	if w.app.pendingAsk == nil || w.app.pendingAsk.Request != "req-3" {
		t.Fatalf("pendingAsk after ask 3 = %+v, want req-3 (state=timeout must not poison later asks)", w.app.pendingAsk)
	}

	// Reopening the session must leave Ask #3 armed (not cleared by earlier answered/timeout states).
	w.newApp()
	w.talk([]string{"persona", "sibyl", "--continue"})
	if w.app.pendingAsk == nil || w.app.pendingAsk.Request != "req-3" {
		t.Fatalf("pendingAsk after resume = %+v, want req-3", w.app.pendingAsk)
	}
}
