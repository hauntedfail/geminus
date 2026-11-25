package p2p

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/honcho"
)

func testSecret() []byte {
	return []byte("0123456789abcdef0123456789abcdef-geminise-v2-test")
}

func TestObfsPacketConnSilentDropAndPadding(t *testing.T) {
	srvUDP, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srvUDP.Close()

	obfsSrv, err := NewObfsPacketConn(srvUDP, testSecret(), 256)
	if err != nil {
		t.Fatal(err)
	}

	cliUDP, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer cliUDP.Close()

	obfsCli, err := NewObfsPacketConn(cliUDP, testSecret(), 256)
	if err != nil {
		t.Fatal(err)
	}

	// Verify wire ciphertext does not leak plaintext.
	payload := []byte("secret-quic-initial-packet")
	wire, err := obfsCli.SealPacket(payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, payload) {
		t.Fatal("wire packet leaked plaintext")
	}

	// Send unauthenticated garbage + tampered packet directly to srvUDP.
	rogue, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rogue.Close()
	_, _ = rogue.WriteTo([]byte("GET / HTTP/1.1\r\n\r\n"), srvUDP.LocalAddr())
	tampered := append([]byte(nil), wire...)
	tampered[len(tampered)-1] ^= 0xff
	_, _ = rogue.WriteTo(tampered, srvUDP.LocalAddr())

	// Now send a valid obfuscated packet.
	if _, err := obfsCli.WriteTo(payload, srvUDP.LocalAddr()); err != nil {
		t.Fatal(err)
	}

	_ = obfsSrv.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := obfsSrv.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom valid packet: %v", err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Fatalf("got %q, want %q", buf[:n], payload)
	}
	if obfsSrv.Dropped() < 2 {
		t.Fatalf("expected >=2 dropped packets, got %d", obfsSrv.Dropped())
	}

	// Rogue sender must receive zero bytes (silent drop).
	_ = rogue.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if rn, _, rerr := rogue.ReadFrom(buf); rerr == nil {
		t.Fatalf("rogue probe received %d bytes; expected silent drop", rn)
	}
}

func TestLocalStorePersistenceAndFilter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPeerMetadata(ctx, "chrome", "geminise_node", map[string]any{"cwd": "/tmp"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, "sess-1", map[string]any{
		"kind":    "geminus",
		"geminus": map[string]any{"persona": "chrome", "human": "h1karu"},
	}, "h1karu", "chrome"); err != nil {
		t.Fatal(err)
	}
	msg1, err := store.Post(ctx, "sess-1", "h1karu", "hello", map[string]any{"role": "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	msg2, err := store.Post(ctx, "sess-1", "chrome", "world", map[string]any{"role": "output"})
	if err != nil {
		t.Fatal(err)
	}

	// Reload from disk and verify everything persisted.
	reloaded, err := NewLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	peers, err := reloaded.Peers(ctx)
	if err != nil || len(peers) != 2 {
		t.Fatalf("Peers: len=%d err=%v", len(peers), err)
	}
	sessions, err := reloaded.Sessions(ctx, map[string]any{
		"kind":    "geminus",
		"geminus": map[string]any{"persona": "chrome", "human": "h1karu"},
	})
	if err != nil || len(sessions) != 1 || sessions[0].ID != "sess-1" {
		t.Fatalf("Sessions filter: %+v err=%v", sessions, err)
	}
	none, err := reloaded.Sessions(ctx, map[string]any{
		"geminus": map[string]any{"persona": "mirai"},
	})
	if err != nil || len(none) != 0 {
		t.Fatalf("expected 0 sessions for mirai, got %d err=%v", len(none), err)
	}
	latest, err := reloaded.Latest(ctx, "sess-1", 1)
	if err != nil || len(latest) != 1 || latest[0].ID != msg2.ID {
		t.Fatalf("Latest(1): %+v err=%v", latest, err)
	}
	all, err := reloaded.Messages(ctx, "sess-1")
	if err != nil || len(all) != 2 || all[0].ID != msg1.ID || all[1].ID != msg2.ID {
		t.Fatalf("Messages: %+v err=%v", all, err)
	}
	if _, err := reloaded.Messages(ctx, "missing"); !errors.Is(err, honcho.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDirectQUICRoundTripAndSilentDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, err := NewLocalStore("")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ListenServer(ServerOptions{
		ListenAddr: "127.0.0.1:0",
		Secret:     testSecret(),
		PadMax:     128,
		Backend:    store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Unauthenticated UDP probe must get silent drop.
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	_, _ = probe.WriteTo([]byte("unauthorized-scanner-probe"), srv.Addr())
	_ = probe.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 512)
	if n, _, rerr := probe.ReadFrom(buf); rerr == nil {
		t.Fatalf("probe received %d bytes; expected silent drop", n)
	}

	// Wrong pinned Ed25519 pubkey must fail handshake.
	wrongPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	badClient, err := NewClient(ClientOptions{
		RemoteAddr:   srv.Addr(),
		Secret:       testSecret(),
		ServerPubKey: wrongPub,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer badClient.Close()
	badCtx, badCancel := context.WithTimeout(ctx, 2*time.Second)
	if err := badClient.Ping(badCtx); err == nil {
		badCancel()
		t.Fatal("expected handshake failure for mismatched ServerPubKey")
	}
	badCancel()

	// Legitimate client pinned to srv.PublicKey().
	cli, err := NewClient(ClientOptions{
		RemoteAddr:   srv.Addr(),
		Secret:       testSecret(),
		ServerPubKey: srv.PublicKey(),
		PadMax:       128,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	if err := cli.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := cli.SetPeerMetadata(ctx, "chrome", "node", map[string]any{"ok": true}); err != nil {
		t.Fatalf("SetPeerMetadata: %v", err)
	}
	peers, err := cli.Peers(ctx)
	if err != nil || len(peers) != 1 || peers[0].ID != "chrome" {
		t.Fatalf("Peers: %+v err=%v", peers, err)
	}
	if err := cli.CreateSession(ctx, "s1", map[string]any{"kind": "geminus"}, "h1karu", "chrome"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	posted, err := cli.Post(ctx, "s1", "h1karu", "ping over obfs-quic", map[string]any{"v": 1})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	msgs, err := cli.Latest(ctx, "s1", 10)
	if err != nil || len(msgs) != 1 || msgs[0].ID != posted.ID || msgs[0].Content != "ping over obfs-quic" {
		t.Fatalf("Latest: %+v err=%v", msgs, err)
	}
	if _, err := cli.Messages(ctx, "no-such-session"); !errors.Is(err, honcho.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if srv.DroppedPackets() < 1 {
		t.Fatalf("expected >=1 dropped packet from scanner probe, got %d", srv.DroppedPackets())
	}
}

func TestBlindRelayEndToEndQUIC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	relay, err := ListenRelay("127.0.0.1:0", testSecret())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	srvPub, srvPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cliPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var srvID, cliID [nodeIDSize]byte
	copy(srvID[:], srvPub)
	copy(cliID[:], cliPub)

	srvRelayConn, err := NewRelayPacketConn(relay.Addr().String(), testSecret(), srvID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewLocalStore("")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ListenServer(ServerOptions{
		PacketConn: srvRelayConn,
		Secret:     testSecret(),
		Identity:   srvPriv,
		Backend:    store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cliRelayConn, err := NewRelayPacketConn(relay.Addr().String(), testSecret(), cliID)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := NewClient(ClientOptions{
		RemoteAddr:   RelayAddr{NodeID: srvID},
		PacketConn:   cliRelayConn,
		Secret:       testSecret(),
		ServerPubKey: srvPub,
		Label:        "relay",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	// Brief pause so both REGISTER packets reach the relay.
	time.Sleep(50 * time.Millisecond)

	if err := cli.Ping(ctx); err != nil {
		t.Fatalf("Ping over blind relay: %v", err)
	}
	if err := cli.CreateSession(ctx, "relay-s1", map[string]any{"kind": "geminus"}, "h1karu", "chrome"); err != nil {
		t.Fatalf("CreateSession over relay: %v", err)
	}
	msg, err := cli.Post(ctx, "relay-s1", "h1karu", "hello through blind relay", map[string]any{"v": 1})
	if err != nil {
		t.Fatalf("Post over relay: %v", err)
	}
	msgs, err := cli.Messages(ctx, "relay-s1")
	if err != nil || len(msgs) != 1 || msgs[0].ID != msg.ID {
		t.Fatalf("Messages over relay: %+v err=%v", msgs, err)
	}

	// Verify automatic UDP hole-punching upgraded to direct peer-to-peer.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cliRelayConn.HasDirectRoute(srvID) && srvRelayConn.HasDirectRoute(cliID) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cliRelayConn.HasDirectRoute(srvID) || !srvRelayConn.HasDirectRoute(cliID) {
		t.Fatalf("expected direct hole-punch upgrade: cli=%v srv=%v",
			cliRelayConn.HasDirectRoute(srvID), srvRelayConn.HasDirectRoute(cliID))
	}

	// Kill the relay completely — the upgraded direct QUIC session must survive!
	_ = relay.Close()
	if err := cli.Ping(ctx); err != nil {
		t.Fatalf("Ping after relay shutdown (direct path): %v", err)
	}
	msg2, err := cli.Post(ctx, "relay-s1", "h1karu", "survived relay outage via direct punch", map[string]any{"v": 1})
	if err != nil {
		t.Fatalf("Post after relay shutdown: %v", err)
	}
	msgs, err = cli.Messages(ctx, "relay-s1")
	if err != nil || len(msgs) != 2 || msgs[1].ID != msg2.ID {
		t.Fatalf("Messages after relay shutdown: %+v err=%v", msgs, err)
	}
}

func TestPeerCacheSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), PeerCacheFile)
	c, err := LoadPeerCache(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Upsert(CachedPeer{
		Name:       "chrome",
		NodeID:     "abcd",
		DirectAddr: "192.168.1.10:4433",
		RelayAddr:  "sibyl.haunted.fail:4433",
	})
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions, got %o", info.Mode().Perm())
	}
	loaded, err := LoadPeerCache(path)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := loaded.Peers["chrome"]
	if !ok || p.DirectAddr != "192.168.1.10:4433" || p.RelayAddr != "sibyl.haunted.fail:4433" {
		t.Fatalf("unexpected cached peer: %+v ok=%v", p, ok)
	}
}
