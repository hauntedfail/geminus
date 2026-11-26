package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/wire"
)

func appendPermission(w *world, request string, choices []string, deadline int64) {
	question := "Run command?\n" + strings.Repeat("complete input line\n", 20) + "END-OF-INPUT"
	meta := wire.Message{V: 1, Role: wire.RoleAsk, Turn: "permission-turn", Request: request,
		Tool: "Bash", Choices: choices, Deadline: deadline, TS: now.Unix()}
	meta.Sig = wire.Sign(w.prof.Secret, question, wire.AskLabel(w.app.session, meta.Turn, request, question, strings.Join(choices, ",")), meta.TS)
	w.srv.Append(w.app.session, w.app.cur.Name, question, map[string]any{wire.Kind: meta})
}

func TestPermissionTerminalPickerIsolated(t *testing.T) {
	// Only the loopback in-memory Honcho is used. No profile loading, host
	// processes, remote bridge, or real terminal/config access occurs.
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"hello", "echo: hello"})
	appendPermission(w, "picker", []string{"allow", "allow_session", "allow_always", "deny"}, now.Unix()+100)
	if _, _, err := w.app.poll(""); err != nil {
		t.Fatal(err)
	}
	lv := newLive(&screen{}, 100, 32, palette{}, w.app.Now)
	lines := make(chan string)
	w.app.attach(lv, lines)
	w.app.servicePendingAsk() // must return immediately, without user input
	if lv.menu == nil {
		t.Fatal("picker did not open")
	}
	for i := 0; i < 20; i++ {
		lv.key(key{Kind: keyPageDown})
	}
	if !strings.Contains(lv.last, "END-OF-INPUT") {
		t.Fatal("complete input cannot be paged to its end")
	}
	lv.key(key{Kind: keyEsc})
	w.app.servicePendingAsk()
	if w.app.pendingAsk == nil || lv.menu != nil {
		t.Fatal("dismissal answered or did not close")
	}
	if err := w.app.handle("/ask"); err != nil {
		t.Fatal(err)
	}
	if lv.menu == nil {
		t.Fatal("/ask did not reopen")
	}
	lv.key(key{Kind: keyRune, R: 'a'})
	w.app.servicePendingAsk()
	if w.app.pendingAsk != nil {
		t.Fatal("one-key answer not processed")
	}
	messages, err := w.app.Honcho.Messages(context.Background(), w.app.session)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range messages {
		meta, ok := parseMeta(m)
		if ok && meta.Role == wire.RoleAnswer {
			count++
			if m.Content != "allow_always" || !w.app.offeredAnswer(m, meta, messages) {
				t.Fatalf("invalid answer: %+v", m)
			}
		}
	}
	if count != 1 {
		t.Fatalf("answer count = %d", count)
	}
	if strings.Contains(w.screen.String(), "saved on host") {
		t.Fatal("unacknowledged host save claimed")
	}
}

func TestPermissionPickerHeartbeatAndExpiryInMainLoop(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"hello", "echo: hello"})
	appendPermission(w, "expiring", []string{"allow", "deny"}, now.Unix()+10)
	if _, _, err := w.app.poll(""); err != nil {
		t.Fatal(err)
	}
	var seconds atomic.Int64
	w.app.Now = func() time.Time { return now.Add(time.Duration(seconds.Load()) * time.Second) }
	w.app.PresenceEvery = time.Nanosecond
	lv := newLive(&screen{}, 100, 30, palette{}, w.app.Now)
	lines := make(chan string)
	w.app.attach(lv, lines)
	ctx, cancel := context.WithCancel(context.Background())
	w.app.ctx = ctx
	done := make(chan struct{})
	go func() { defer close(done); w.app.loopLive() }()
	defer func() { cancel(); <-done }()
	wait := func(check func() bool) {
		t.Helper()
		until := time.Now().Add(3 * time.Second)
		for !check() {
			if time.Now().After(until) {
				t.Fatal("main loop stalled while picker open")
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait(func() bool { lv.mu.Lock(); defer lv.mu.Unlock(); return lv.menu != nil })
	seconds.Store(2)
	wait(func() bool {
		var p wire.Presence
		_ = json.Unmarshal(w.srv.PeerMetadata(w.prof.Human, wire.PresenceKey), &p)
		return p.TS == now.Unix()+2 && wire.VerifyPresenceWith(w.app.humanAuth(), w.prof.Human, p)
	})
	seconds.Store(11)
	wait(func() bool { lv.mu.Lock(); defer lv.mu.Unlock(); return lv.menu == nil })
	// Expiry must not post an answer, even if no host timeout arrived.
	for _, m := range w.srv.Messages(w.app.session) {
		raw, _ := json.Marshal(m.Metadata[wire.Kind])
		var meta wire.Message
		_ = json.Unmarshal(raw, &meta)
		if meta.Role == wire.RoleAnswer {
			t.Fatal("expiry sent an answer")
		}
	}
}

func TestPermissionOfferedChoicesAndLineMode(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"hello", "echo: hello"})
	appendPermission(w, "line", []string{"deny", "allow_session"}, now.Unix()+100)
	if _, _, err := w.app.poll(""); err != nil {
		t.Fatal(err)
	}
	for _, rejected := range []string{"a", "always", "allow", "y"} {
		if err := w.app.handle(rejected); err == nil {
			t.Fatalf("unoffered %q accepted", rejected)
		}
	}
	if err := w.app.handle("/ask"); err != nil {
		t.Fatal(err)
	}
	if err := w.app.handle("2"); err != nil {
		t.Fatal(err)
	}
	w.expect("choices: deny / allow_session", "END-OF-INPUT", "awaiting host acknowledgment")
	appendPermission(w, "expired", []string{"allow", "deny"}, now.Unix()+1)
	if _, _, err := w.app.poll(""); err != nil {
		t.Fatal(err)
	}
	w.app.Now = func() time.Time { return now.Add(time.Second) }
	if err := w.app.sendAnswer("allow"); err == nil {
		t.Fatal("expired answer accepted")
	}
}

func TestModeScopedSignedResumeAndCompatibility(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"hello", "echo: hello"})
	if err := w.app.handle("/mode bypass"); err == nil {
		t.Fatal("legacy host accepted mode")
	}
	w.app.harness.Options = append(append([]string{}, w.app.harness.Options...), "mode")
	if err := w.app.handle("/mode manual"); err != nil {
		t.Fatal(err)
	}
	sid := w.app.session
	sessions := w.srv.Sessions()
	var info wire.Session
	raw, _ := json.Marshal(sessions[0].Metadata[wire.Kind])
	_ = json.Unmarshal(raw, &info)
	if info.Mode != "manual" || !wire.VerifySessionWith(w.app.humanAuth(), sid, info) {
		t.Fatalf("mode not signed: %+v", info)
	}
	if err := w.app.say("tuned"); err != nil {
		t.Fatal(err)
	}
	if err := w.app.resume(sid); err != nil {
		t.Fatal(err)
	}
	if w.app.tuning.Mode != "manual" {
		t.Fatal("resume lost mode")
	}
	// The currently advertised host is legacy again. Do not silently drop the
	// resumed signed request or run with broader/different permissions.
	if err := w.app.say("must not run"); err == nil {
		t.Fatal("downgraded host accepted mode")
	}
	if err := w.app.handle("/new"); err != nil {
		t.Fatal(err)
	}
	if w.app.tuning.Mode != "" {
		t.Fatal("conversation mode leaked into new session")
	}
	if w.app.defaults.Personas[w.app.cur.Name].Mode != "" {
		t.Fatal("conversation mode changed defaults")
	}
}

func TestProjectDirectoryRecallRequiresVerifiedSuccess(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl", "--cwd", "/project/success"}, [2]string{"hello", "echo: hello"})
	if err := w.app.start("sibyl", ""); err != nil {
		t.Fatal(err)
	}
	if w.app.cwd != "/project/success" {
		t.Fatalf("not recalled: %q", w.app.cwd)
	}
	// A newer owner-signed conversation and a forged success are not evidence.
	ctx := context.Background()
	bad := "never-ran"
	w.app.cwd = "/project/unverified"
	meta := w.app.meta(bad, "queued", false)
	meta.Updated++
	if err := w.app.Honcho.CreateSession(ctx, bad, wire.SessionMetadata(meta), w.prof.Human, "sibyl"); err != nil {
		t.Fatal(err)
	}
	w.srv.Append(bad, "sibyl", "forged", map[string]any{wire.Kind: wire.Message{Role: wire.RoleOutput, Final: true, OK: true}})
	if err := w.app.start("sibyl", ""); err != nil {
		t.Fatal(err)
	}
	if w.app.cwd != "/project/success" {
		t.Fatalf("unverified directory recalled: %q", w.app.cwd)
	}
	if err := w.app.handle("/default cwd /project/pinned"); err != nil {
		t.Fatal(err)
	}
	if err := w.app.start("sibyl", ""); err != nil {
		t.Fatal(err)
	}
	if w.app.cwd != "/project/pinned" {
		t.Fatal("recall displaced pin")
	}
	if err := w.app.start("chrome", "hermes"); err != nil {
		t.Fatal(err)
	}
	if w.app.cwd != "" {
		t.Fatal("directory leaked across personas")
	}
}

func TestPermissionHotkeysOnlyOffered(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"s", "allow_session"}, {"2", "allow_session"}, {"deny", "deny"}, {"a", ""}, {"y", ""}} {
		got, ok := parseAskAnswerFor(tc.input, []string{"deny", "allow_session"})
		if got != tc.want || ok != (tc.want != "") {
			t.Error(fmt.Sprintf("%q => %q, %v", tc.input, got, ok))
		}
	}
}
