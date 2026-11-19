package honcho_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/honcho/honchotest"
)

func client(t *testing.T, srv *honchotest.Server) *honcho.Client {
	t.Helper()
	c, err := honcho.New(srv.URL, srv.Workspace, srv.Key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPeersArePagedAndMetadataIsMergedNotReplaced(t *testing.T) {
	var names []string
	for i := 0; i < 120; i++ {
		names = append(names, fmt.Sprintf("peer-%03d", i))
	}
	srv := honchotest.New(names...)
	defer srv.Close()
	c, ctx := client(t, srv), context.Background()
	srv.SetPeerMetadata("peer-007", "geminise_push", map[string]any{"endpoint": "https://x"})

	peers, err := c.Peers(ctx)
	if err != nil || len(peers) != 120 {
		t.Fatalf("Peers = %d, %v; want all 120 across pages", len(peers), err)
	}
	if err := c.SetPeerMetadata(ctx, "peer-007", "geminus", map[string]any{"persona": "sibyl"}); err != nil {
		t.Fatalf("SetPeerMetadata: %v", err)
	}
	if got := string(srv.PeerMetadata("peer-007", "geminise_push")); !strings.Contains(got, "https://x") {
		t.Errorf("another key of the peer's metadata was lost: %s", got)
	}
	if got := string(srv.PeerMetadata("peer-007", "geminus")); !strings.Contains(got, "sibyl") {
		t.Errorf("the new key was not stored: %s", got)
	}
	if err := c.SetPeerMetadata(ctx, "nobody", "geminus", 1); !errors.Is(err, honcho.ErrNotFound) {
		t.Errorf("unknown peer: err = %v", err)
	}
}

func TestSessionsAreFilteredByNestedMetadataAndMessagesKeepOrder(t *testing.T) {
	srv := honchotest.New("h1karu", "mirai")
	defer srv.Close()
	c, ctx := client(t, srv), context.Background()
	meta := func(persona string, pending bool) map[string]any {
		return map[string]any{"kind": "geminus", "geminus": map[string]any{"persona": persona, "pending": pending}}
	}
	for id, m := range map[string]map[string]any{"a": meta("mirai", true), "b": meta("mirai", false), "c": meta("sibyl", true)} {
		if err := c.CreateSession(ctx, id, m, "h1karu", "mirai"); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.CreateSession(ctx, "mail", map[string]any{"kind": "geminise"}, "h1karu")

	got, err := c.Sessions(ctx, map[string]any{"kind": "geminus", "geminus": map[string]any{"persona": "mirai", "pending": true}})
	if err != nil || len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("filtered sessions = %+v, %v", got, err)
	}
	if err := c.SetSessionMetadata(ctx, "a", meta("mirai", false)); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Sessions(ctx, map[string]any{"kind": "geminus", "geminus": map[string]any{"pending": true}}); len(got) != 1 || got[0].ID != "c" {
		t.Errorf("after clearing the mark: %+v", got)
	}

	for i := 0; i < 130; i++ {
		if _, err := c.Post(ctx, "a", "h1karu", fmt.Sprintf("m%03d 日本語", i), map[string]any{"geminus": map[string]any{"n": i}}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := c.Messages(ctx, "a")
	if err != nil || len(all) != 130 || all[0].Content != "m000 日本語" || all[129].Content != "m129 日本語" {
		t.Fatalf("Messages: %d, %v", len(all), err)
	}
	latest, err := c.Latest(ctx, "a", 50)
	if err != nil || len(latest) != 50 || latest[0].Content != "m080 日本語" || latest[49].Content != "m129 日本語" {
		t.Fatalf("Latest: %d, first %q", len(latest), latest[0].Content)
	}
	var n struct{ N int }
	_ = json.Unmarshal(latest[49].Metadata["geminus"], &n)
	if n.N != 129 {
		t.Errorf("metadata not kept: %s", latest[49].Metadata["geminus"])
	}
}

func TestErrorsNameTheStatusAndNeverTheToken(t *testing.T) {
	srv := honchotest.New("h1karu")
	c, err := honcho.New(srv.URL, srv.Workspace, "wrong-token-value")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Peers(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "wrong-token-value") {
		t.Errorf("err = %v", err)
	}
	srv.Close()
	_, err = c.Peers(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not reachable") || strings.Contains(err.Error(), "wrong-token-value") {
		t.Errorf("err = %v", err)
	}
	if _, err := honcho.New("ftp://x", "ws", "k"); err == nil {
		t.Error("accepted a non-HTTP address")
	}
}
