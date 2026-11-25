package transport

import (
	"context"
	"errors"
	"testing"

	"github.com/hauntedfail/geminus/internal/honcho"
)

type stubBackend struct {
	name      string
	peers     []honcho.Peer
	peersErr  error
	postMsg   honcho.Message
	postErr   error
	ringCalls int
}

func (s *stubBackend) Name() string { return s.name }
func (s *stubBackend) Peers(ctx context.Context) ([]honcho.Peer, error) {
	return s.peers, s.peersErr
}
func (s *stubBackend) SetPeerMetadata(ctx context.Context, peer, key string, value any) error {
	return s.peersErr
}
func (s *stubBackend) Sessions(ctx context.Context, filter map[string]any) ([]honcho.Session, error) {
	return nil, s.peersErr
}
func (s *stubBackend) CreateSession(ctx context.Context, id string, metadata any, peers ...string) error {
	return s.peersErr
}
func (s *stubBackend) SetSessionMetadata(ctx context.Context, id string, metadata any) error {
	return s.peersErr
}
func (s *stubBackend) Messages(ctx context.Context, session string) ([]honcho.Message, error) {
	return nil, s.peersErr
}
func (s *stubBackend) Latest(ctx context.Context, session string, n int) ([]honcho.Message, error) {
	return nil, s.peersErr
}
func (s *stubBackend) Post(ctx context.Context, session, peer, content string, metadata any) (honcho.Message, error) {
	return s.postMsg, s.postErr
}
func (s *stubBackend) Ring(ctx context.Context, endpoint string) error {
	s.ringCalls++
	return nil
}

func TestFailoverBackend(t *testing.T) {
	ctx := context.Background()
	primary := &stubBackend{
		name:  "direct",
		peers: []honcho.Peer{{ID: "chrome"}},
	}
	fallback := &stubBackend{
		name:  "Honcho",
		peers: []honcho.Peer{{ID: "chrome"}, {ID: "sibyl"}},
	}
	fb := NewFailover(primary, fallback)

	// 1. Primary healthy -> stays on primary.
	peers, err := fb.Peers(ctx)
	if err != nil {
		t.Fatalf("Peers primary: %v", err)
	}
	if len(peers) != 1 || fb.UsingFallback() || fb.Name() != "direct" {
		t.Fatalf("expected primary, got peers=%d fallback=%v name=%s", len(peers), fb.UsingFallback(), fb.Name())
	}

	// 2. Primary unreachable -> fails over to fallback.
	primary.peersErr = errors.New("connection refused")
	peers, err = fb.Peers(ctx)
	if err != nil {
		t.Fatalf("Peers fallback: %v", err)
	}
	if len(peers) != 2 || !fb.UsingFallback() || fb.Name() != "Honcho" {
		t.Fatalf("expected fallback, got peers=%d fallback=%v name=%s", len(peers), fb.UsingFallback(), fb.Name())
	}

	// 3. Ring uses active fallback backend.
	if err := fb.Ring(ctx, "https://example.invalid"); err != nil {
		t.Fatalf("Ring: %v", err)
	}
	if fallback.ringCalls != 1 || primary.ringCalls != 0 {
		t.Fatalf("expected ring on fallback, got primary=%d fallback=%d", primary.ringCalls, fallback.ringCalls)
	}

	// 4. NotFound does NOT fail over.
	primary.peersErr = honcho.ErrNotFound
	_, err = fb.Peers(ctx)
	if !errors.Is(err, honcho.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// 5. Context canceled does NOT fail over.
	primary.peersErr = context.Canceled
	_, err = fb.Peers(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// 6. Primary recovers -> switches back to primary.
	primary.peersErr = nil
	peers, err = fb.Peers(ctx)
	if err != nil {
		t.Fatalf("Peers recovery: %v", err)
	}
	if len(peers) != 1 || fb.UsingFallback() || fb.Name() != "direct" {
		t.Fatalf("expected recovery to primary, got peers=%d fallback=%v name=%s", len(peers), fb.UsingFallback(), fb.Name())
	}
}
