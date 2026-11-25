// Package transport defines the pluggable transport/store backend used by
// geminus. Both the legacy Honcho HTTPS client and the edge-to-edge P2P
// Transport v2 (Obfuscated QUIC + Ed25519 + blind relay) implement Backend.
package transport

import (
	"context"
	"net/http"

	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/wire"
)

// Backend is the minimal transport and state contract required by internal/chat.
// All records carried over Backend are authenticated independently by
// internal/wire (Ed25519 / HMAC signatures), so Backend itself does not need
// to be trusted for integrity or authorization.
type Backend interface {
	// Name returns a short human-readable label for status display (e.g. "Honcho", "direct").
	Name() string
	// Peers returns every known peer and its signed metadata.
	Peers(ctx context.Context) ([]honcho.Peer, error)
	// SetPeerMetadata updates one metadata key on a peer without clobbering others.
	SetPeerMetadata(ctx context.Context, peer, key string, value any) error
	// Sessions lists conversations matching filter, newest first.
	Sessions(ctx context.Context, filter map[string]any) ([]honcho.Session, error)
	// CreateSession creates a new conversation session.
	CreateSession(ctx context.Context, id string, metadata any, peers ...string) error
	// SetSessionMetadata replaces a session's metadata.
	SetSessionMetadata(ctx context.Context, id string, metadata any) error
	// Messages returns every message of a session, oldest first.
	Messages(ctx context.Context, session string) ([]honcho.Message, error)
	// Latest returns the newest n messages of a session, oldest first.
	Latest(ctx context.Context, session string, n int) ([]honcho.Message, error)
	// Post appends a message to a session and returns the stored copy.
	Post(ctx context.Context, session, peer, content string, metadata any) (honcho.Message, error)
	// Ring optionally notifies a peer's bridge immediately (no-op for direct push transports).
	Ring(ctx context.Context, endpoint string) error
}

// HonchoBackend adapts *honcho.Client to Backend.
type HonchoBackend struct {
	Client *honcho.Client
}

// WrapHoncho returns a Backend backed by c.
func WrapHoncho(c *honcho.Client) *HonchoBackend {
	return &HonchoBackend{Client: c}
}

func (h *HonchoBackend) Name() string { return "Honcho" }

func (h *HonchoBackend) Peers(ctx context.Context) ([]honcho.Peer, error) {
	return h.Client.Peers(ctx)
}

func (h *HonchoBackend) SetPeerMetadata(ctx context.Context, peer, key string, value any) error {
	return h.Client.SetPeerMetadata(ctx, peer, key, value)
}

func (h *HonchoBackend) Sessions(ctx context.Context, filter map[string]any) ([]honcho.Session, error) {
	return h.Client.Sessions(ctx, filter)
}

func (h *HonchoBackend) CreateSession(ctx context.Context, id string, metadata any, peers ...string) error {
	return h.Client.CreateSession(ctx, id, metadata, peers...)
}

func (h *HonchoBackend) SetSessionMetadata(ctx context.Context, id string, metadata any) error {
	return h.Client.SetSessionMetadata(ctx, id, metadata)
}

func (h *HonchoBackend) Messages(ctx context.Context, session string) ([]honcho.Message, error) {
	return h.Client.Messages(ctx, session)
}

func (h *HonchoBackend) Latest(ctx context.Context, session string, n int) ([]honcho.Message, error) {
	return h.Client.Latest(ctx, session, n)
}

func (h *HonchoBackend) Post(ctx context.Context, session, peer, content string, metadata any) (honcho.Message, error) {
	return h.Client.Post(ctx, session, peer, content, metadata)
}

func (h *HonchoBackend) Ring(ctx context.Context, endpoint string) error {
	if endpoint == "" {
		return nil
	}
	var hc *http.Client
	if h.Client != nil {
		hc = h.Client.HTTP()
	}
	return wire.Ring(ctx, hc, endpoint)
}
