package chat

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/e2ee"
	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/honcho/honchotest"
	"github.com/hauntedfail/geminus/internal/profile"
	"github.com/hauntedfail/geminus/internal/wire"
)

func TestLivePreviewTailStreamsPartialTokensAndCommitsMarkdown(t *testing.T) {
	var buf bytes.Buffer
	now := func() time.Time { return time.Unix(1795682000, 0) }
	lv := newLive(&buf, 80, 24, palette{On: false}, now)
	defer lv.close()

	app := &App{
		Out:       newPrinter(lv, false),
		Now:       now,
		cur:       &Persona{Name: "sibyl"},
		harness:   &wire.Harness{ID: "hermes", Name: "Hermes Agent"},
		session:   "geminus-sibyl-20261127-abc123",
		shown:     map[string]bool{},
		waiting:   map[string]bool{"turn-1": true},
		state:     map[string]string{"turn-1": wire.StateQueued},
		shownTurn: map[string]bool{},
		live:      lv,
	}

	// Token 1 without newline must appear immediately via live preview tail.
	app.handleStreamEvent(e2ee.StreamEvent{
		Session: app.session,
		Turn:    "turn-1",
		Seq:     1,
		Kind:    e2ee.StreamKindDelta,
		Text:    "Hello ",
	})
	if app.state["turn-1"] != wire.StateRunning {
		t.Fatalf("expected turn marked running immediately, got %q", app.state["turn-1"])
	}
	if lv.preview != "Hello " {
		t.Fatalf("expected live preview %q, got %q", "Hello ", lv.preview)
	}

	// Token 2 completes bold markdown + newline -> commits cleanly, clears preview.
	app.handleStreamEvent(e2ee.StreamEvent{
		Session: app.session,
		Turn:    "turn-1",
		Seq:     2,
		Kind:    e2ee.StreamKindDelta,
		Text:    "**world**\n",
	})
	if lv.preview != "" {
		t.Fatalf("expected cleared preview after newline, got %q", lv.preview)
	}
	var joined strings.Builder
	for _, ln := range lv.lines {
		joined.WriteString(ln.text)
		joined.WriteByte('\n')
	}
	if !strings.Contains(joined.String(), "Hello world\n") {
		t.Fatalf("expected formatted markdown committed line, got %q", joined.String())
	}
	if strings.Contains(joined.String(), "**") {
		t.Fatalf("raw markdown markers leaked into transcript: %q", joined.String())
	}

	// Authoritative final Honcho output matching streamed text must NOT duplicate.
	beforeLines := len(lv.lines)
	handled := app.reconcileHonchoOutput("Hello **world**\n", wire.Message{
		Role:  wire.RoleOutput,
		Turn:  "turn-1",
		Seq:   1,
		Final: true,
		OK:    true,
	})
	if !handled {
		t.Fatal("expected reconcileHonchoOutput to handle already-streamed turn")
	}
	if len(lv.lines) != beforeLines {
		t.Fatalf("duplicate lines printed on final reconcile: before=%d after=%d", beforeLines, len(lv.lines))
	}
}

func TestStreamToolProgressNormalAndClawdMode(t *testing.T) {
	now := func() time.Time { return time.Unix(1795682000, 0) }
	for _, clawd := range []bool{false, true} {
		var out bytes.Buffer
		on := clawd
		app := &App{
			Out:           NewPrinter(&out),
			Now:           now,
			cur:           &Persona{Name: "sibyl"},
			harness:       &wire.Harness{ID: "hermes", Name: "Hermes Agent"},
			session:       "sess-tool",
			waiting:       map[string]bool{"t1": true},
			state:         map[string]string{"t1": wire.StateQueued},
			clawdOverride: &on,
		}
		app.handleStreamEvent(e2ee.StreamEvent{
			Session: "sess-tool",
			Turn:    "t1",
			Seq:     1,
			Kind:    e2ee.StreamKindToolUse,
			Tool:    "terminal",
			Detail:  "go test ./...",
		})
		if got := app.streamActivity(); got != "terminal: go test ./..." {
			t.Fatalf("clawd=%v: unexpected streamActivity %q", clawd, got)
		}
		if clawd && !strings.Contains(out.String(), "  ⎿  terminal: go test ./...") {
			t.Fatalf("clawd mode missing tool indicator: %q", out.String())
		}
		if !clawd && !strings.Contains(out.String(), "· terminal: go test ./...") {
			t.Fatalf("normal mode missing tool indicator: %q", out.String())
		}

		app.handleStreamEvent(e2ee.StreamEvent{
			Session: "sess-tool",
			Turn:    "t1",
			Seq:     2,
			Kind:    e2ee.StreamKindToolResult,
			Tool:    "terminal",
			OK:      true,
		})
		if got := app.streamActivity(); got != "" {
			t.Fatalf("clawd=%v: expected cleared streamActivity after tool_result, got %q", clawd, got)
		}
	}
}

func TestStreamSubscriptionAndWatchReconcileWithProxy(t *testing.T) {
	secret := bytes.Repeat([]byte{0x6c}, 32)
	srv := honchotest.New("h1karu", "sibyl")
	defer srv.Close()

	proxy, err := e2ee.NewProxy(e2ee.ProxyConfig{
		Secret:   secret,
		Upstream: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	tsrv := httptest.NewServer(proxy)
	defer tsrv.Close()
	e2eeURL := tsrv.URL + "/geminise/e2ee"

	client, err := honcho.New(srv.URL, srv.Workspace, srv.Key)
	if err != nil {
		t.Fatalf("honcho.New: %v", err)
	}
	client, err = client.WithE2EE(e2eeURL, secret)
	if err != nil {
		t.Fatalf("WithE2EE: %v", err)
	}

	sessionID := "geminus-sibyl-20261127-stream1"
	if err := client.CreateSession(context.Background(), sessionID, map[string]any{"kind": wire.Kind}, "h1karu", "sibyl"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	app := &App{
		Out: NewPrinter(&out),
		Profile: profile.Profile{
			Human:     "h1karu",
			Workspace: srv.Workspace,
			Secret:    secret,
			E2EEURL:   e2eeURL,
		},
		Honcho:    client,
		Now:       time.Now,
		Watch:     5 * time.Second, // deliberately slow poll to prove live stream drives early output + instant completion
		Idle:      5 * time.Second,
		SlowAfter: 20 * time.Second,
		ctx:       ctx,
		cur:       &Persona{Name: "sibyl"},
		harness:   &wire.Harness{ID: "hermes", Name: "Hermes Agent"},
		session:   sessionID,
		shown:     map[string]bool{},
		waiting:   map[string]bool{"turn-live": true},
		state:     map[string]string{"turn-live": wire.StateQueued},
	}

	// Publish live stream deltas + final Honcho output from a simulated bridge.
	go func() {
		pubCli, _ := e2ee.NewStreamClient(e2eeURL, secret)
		time.Sleep(60 * time.Millisecond)
		nowTS := time.Now().Unix()
		ev1 := e2ee.StreamEvent{Session: sessionID, Turn: "turn-live", Seq: 1, Kind: e2ee.StreamKindDelta, Text: "Live ", TS: nowTS}
		ev1.Sig = e2ee.SignStreamEvent(secret, ev1)
		ev2 := e2ee.StreamEvent{Session: sessionID, Turn: "turn-live", Seq: 2, Kind: e2ee.StreamKindDelta, Text: "stream!", TS: nowTS}
		ev2.Sig = e2ee.SignStreamEvent(secret, ev2)
		_, _ = pubCli.Publish(context.Background(), []e2ee.StreamEvent{ev1, ev2})

		time.Sleep(60 * time.Millisecond)
		// Post authoritative final Honcho output, then emit stream done.
		outTS := time.Now().Unix()
		finalContent := "Live stream!"
		label := wire.OutputLabel(sessionID, "turn-live", 1, true, true, sessionID)
		meta := wire.Message{
			V: 1, Role: wire.RoleOutput, Turn: "turn-live", Seq: 1, Final: true, OK: true, Native: sessionID,
			TS: outTS, Sig: wire.Sign(secret, finalContent, label, outTS),
		}
		_, _ = client.Post(context.Background(), sessionID, "sibyl", finalContent, map[string]any{wire.Kind: meta})

		evDone := e2ee.StreamEvent{Session: sessionID, Turn: "turn-live", Seq: 3, Kind: e2ee.StreamKindDone, OK: true, TS: time.Now().Unix()}
		evDone.Sig = e2ee.SignStreamEvent(secret, evDone)
		_, _ = pubCli.Publish(context.Background(), []e2ee.StreamEvent{evDone})
	}()

	start := time.Now()
	if err := app.watch("turn-live"); err != nil {
		t.Fatalf("watch: %v", err)
	}
	// Watch interval is 5s, so finishing in < 1.5s proves StreamKindDone triggered instant completion.
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("watch waited for slow Honcho poll instead of live stream done: %v", elapsed)
	}
	got := out.String()
	if count := strings.Count(got, "Live stream!"); count != 1 {
		t.Fatalf("expected exactly one 'Live stream!' with zero duplication, got count=%d output=%q", count, got)
	}
}
