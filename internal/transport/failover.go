package transport

import (
	"context"
	"errors"
	"sync"

	"github.com/hauntedfail/geminus/internal/honcho"
)

// FailoverBackend routes operations to Primary and automatically falls back
// to Fallback when Primary fails with a transport/reachability error (anything
// other than honcho.ErrNotFound or context cancellation).
type FailoverBackend struct {
	Primary  Backend
	Fallback Backend

	mu         sync.RWMutex
	onFallback bool
}

// NewFailover creates a FailoverBackend with primary and fallback transports.
func NewFailover(primary, fallback Backend) *FailoverBackend {
	return &FailoverBackend{Primary: primary, Fallback: fallback}
}

// UsingFallback reports whether the last completed call ran on Fallback.
func (f *FailoverBackend) UsingFallback() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.onFallback
}

func (f *FailoverBackend) Name() string {
	f.mu.RLock()
	fb := f.onFallback
	f.mu.RUnlock()
	if fb && f.Fallback != nil {
		return f.Fallback.Name()
	}
	if f.Primary != nil {
		return f.Primary.Name()
	}
	if f.Fallback != nil {
		return f.Fallback.Name()
	}
	return "none"
}

func shouldFailover(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, honcho.ErrNotFound) || errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

func (f *FailoverBackend) setFallback(v bool) {
	f.mu.Lock()
	f.onFallback = v
	f.mu.Unlock()
}

func callWithFailover[T any](f *FailoverBackend, fn func(Backend) (T, error)) (T, error) {
	if f.Primary != nil {
		v, err := fn(f.Primary)
		if !shouldFailover(err) || f.Fallback == nil {
			if err == nil {
				f.setFallback(false)
			}
			return v, err
		}
	}
	if f.Fallback != nil {
		v, err := fn(f.Fallback)
		if err == nil {
			f.setFallback(true)
		}
		return v, err
	}
	var zero T
	return zero, errors.New("no transport backend configured")
}

func (f *FailoverBackend) Peers(ctx context.Context) ([]honcho.Peer, error) {
	return callWithFailover(f, func(b Backend) ([]honcho.Peer, error) { return b.Peers(ctx) })
}

func (f *FailoverBackend) SetPeerMetadata(ctx context.Context, peer, key string, value any) error {
	_, err := callWithFailover(f, func(b Backend) (struct{}, error) {
		return struct{}{}, b.SetPeerMetadata(ctx, peer, key, value)
	})
	return err
}

func (f *FailoverBackend) Sessions(ctx context.Context, filter map[string]any) ([]honcho.Session, error) {
	return callWithFailover(f, func(b Backend) ([]honcho.Session, error) { return b.Sessions(ctx, filter) })
}

func (f *FailoverBackend) CreateSession(ctx context.Context, id string, metadata any, peers ...string) error {
	_, err := callWithFailover(f, func(b Backend) (struct{}, error) {
		return struct{}{}, b.CreateSession(ctx, id, metadata, peers...)
	})
	return err
}

func (f *FailoverBackend) SetSessionMetadata(ctx context.Context, id string, metadata any) error {
	_, err := callWithFailover(f, func(b Backend) (struct{}, error) {
		return struct{}{}, b.SetSessionMetadata(ctx, id, metadata)
	})
	return err
}

func (f *FailoverBackend) Messages(ctx context.Context, session string) ([]honcho.Message, error) {
	return callWithFailover(f, func(b Backend) ([]honcho.Message, error) { return b.Messages(ctx, session) })
}

func (f *FailoverBackend) Latest(ctx context.Context, session string, n int) ([]honcho.Message, error) {
	return callWithFailover(f, func(b Backend) ([]honcho.Message, error) { return b.Latest(ctx, session, n) })
}

func (f *FailoverBackend) Post(ctx context.Context, session, peer, content string, metadata any) (honcho.Message, error) {
	return callWithFailover(f, func(b Backend) (honcho.Message, error) {
		return b.Post(ctx, session, peer, content, metadata)
	})
}

func (f *FailoverBackend) Ring(ctx context.Context, endpoint string) error {
	if f.UsingFallback() && f.Fallback != nil {
		return f.Fallback.Ring(ctx, endpoint)
	}
	if f.Primary != nil {
		return f.Primary.Ring(ctx, endpoint)
	}
	if f.Fallback != nil {
		return f.Fallback.Ring(ctx, endpoint)
	}
	return nil
}
