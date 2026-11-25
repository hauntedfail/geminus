package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/profile"
	"github.com/hauntedfail/geminus/internal/transport/p2p"
	"github.com/hauntedfail/geminus/internal/wire"
)

// TestChatEndToEndOverDirectP2PWithoutHoncho proves that a full geminus
// conversation (directory discovery, session creation, signed prompt delivery,
// signed bridge reply, and completion) works end-to-end over Transport v2
// (Obfuscated QUIC + LocalStore) with zero Honcho server running.
func TestChatEndToEndOverDirectP2PWithoutHoncho(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	secret := []byte("0123456789abcdef0123456789abcdef-e2e-direct")
	store, err := p2p.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Register a signed node card for persona "chrome" directly in the bridge's LocalStore.
	now := time.Unix(1764000000, 0)
	card := wire.NodeCard{
		V:   1,
		TS:  now.Unix(),
		CWD: "/Users/vvx/projekt/go/geminus",
		Harnesses: []wire.Harness{
			{ID: "hermes", Name: "Hermes Agent"},
		},
	}
	card.Sig = wire.Sign(secret, "hermes", wire.NodeLabel("chrome"), card.TS)
	card.OptionsSig = wire.Sign(secret, wire.CardText(card.Harnesses), wire.OptionsLabel("chrome"), card.TS)
	card.CWDSig = wire.Sign(secret, card.CWD, wire.CWDLabel("chrome"), card.TS)
	if err := store.SetPeerMetadata(ctx, "chrome", wire.NodeKey, card); err != nil {
		t.Fatal(err)
	}

	// Simulate the bridge responding immediately whenever a signed prompt arrives.
	store.OnMessage = func(sessionID string, msg honcho.Message) {
		raw, ok := msg.Metadata[wire.Kind]
		if !ok {
			return
		}
		var wm wire.Message
		if err := json.Unmarshal(raw, &wm); err != nil || wm.Role != wire.RolePrompt {
			return
		}
		replyText := "pong over direct obfs-quic"
		ts := now.Unix() + 1
		reply := wire.Message{
			V:     1,
			Role:  wire.RoleOutput,
			Turn:  msg.ID,
			Seq:   1,
			Final: true,
			OK:    true,
			TS:    ts,
			State: wire.StateDone,
			Sig:   wire.Sign(secret, replyText, wire.OutputLabel(sessionID, msg.ID, 1, true, true, ""), ts),
		}
		_, _ = store.Post(context.Background(), sessionID, "chrome", replyText, map[string]any{wire.Kind: reply})
	}

	srv, err := p2p.ListenServer(p2p.ServerOptions{
		ListenAddr: "127.0.0.1:0",
		Secret:     secret,
		PadMax:     128,
		Backend:    store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cli, err := p2p.NewClient(p2p.ClientOptions{
		RemoteAddr:   srv.Addr(),
		Secret:       secret,
		ServerPubKey: srv.PublicKey(),
		PadMax:       128,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	var out, errw bytes.Buffer
	app := &App{
		Out: newPrinter(&out, false),
		Profile: profile.Profile{
			Human:   "h1karu",
			Local:   "mirai",
			Members: map[string]string{"h1karu": "human", "chrome": "agent", "mirai": "agent"},
			Secret:  secret,
		},
		// Honcho is intentionally nil — Transport v2 runs completely without Honcho!
		Honcho:    nil,
		Backend:   cli,
		Now:       func() time.Time { return now },
		Watch:     10 * time.Millisecond,
		Idle:      10 * time.Millisecond,
		SlowAfter: 5 * time.Second,
		ctx:       ctx,
		interrupt: make(chan os.Signal, 1),
	}

	code := app.main(request{persona: "chrome", harness: "hermes"}, strings.NewReader("hello direct\n/quit\n"), &errw)
	if code != 0 {
		t.Fatalf("exit code %d, stderr=%q, stdout=%q", code, errw.String(), out.String())
	}
	got := out.String()
	for _, want := range []string{
		"recorded in direct for chrome",
		"pong over direct obfs-quic",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stdout missing %q:\n%s", want, got)
		}
	}
}
