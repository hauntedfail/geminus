package e2ee

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	maxStreamSessions        = 1024
	maxStreamEventsPerSess   = 512
	maxStreamPublishBatch    = 64
	maxStreamEventTextBytes  = 32768
	maxStreamEventDetailByte = 2048
	streamSessionTTL         = 10 * time.Minute
	streamSubQueueSize       = 256
)

var (
	// ErrStreamUnauthorized is returned when a published stream event fails HMAC or skew checks.
	ErrStreamUnauthorized = errors.New("e2ee: unauthorized stream event")
	// ErrStreamBadRequest is returned when a publish batch or event violates bounds.
	ErrStreamBadRequest = errors.New("e2ee: invalid stream publish request")
)

type streamSubscriber struct {
	ch chan StreamEvent
}

type sessionStream struct {
	nextID      uint64
	events      []StreamEvent
	lastTouch   time.Time
	subscribers map[*streamSubscriber]struct{}
}

// StreamHub is a bounded, ephemeral, in-memory pub/sub broker keyed by Honcho session ID.
// Nothing stored in StreamHub is persisted to disk or forwarded to Honcho.
type StreamHub struct {
	mu       sync.Mutex
	sessions map[string]*sessionStream
	now      func() time.Time
}

// NewStreamHub creates a new bounded ephemeral stream hub.
func NewStreamHub(now func() time.Time) *StreamHub {
	if now == nil {
		now = time.Now
	}
	return &StreamHub{
		sessions: make(map[string]*sessionStream),
		now:      now,
	}
}

// VerifyAndPublish validates a batch of bridge-signed stream events and publishes
// them to their session ring buffers and active subscribers.
func (h *StreamHub) VerifyAndPublish(secret []byte, events []StreamEvent, maxSkew int64) (uint64, error) {
	if len(events) == 0 || len(events) > maxStreamPublishBatch {
		return 0, fmt.Errorf("%w: batch size %d out of bounds", ErrStreamBadRequest, len(events))
	}
	nowUnix := h.now().Unix()
	for i := range events {
		ev := events[i]
		if ev.Session == "" || len(ev.Session) > 128 || ev.Turn == "" || len(ev.Turn) > 128 {
			return 0, fmt.Errorf("%w: invalid session or turn", ErrStreamBadRequest)
		}
		if len(ev.Text) > maxStreamEventTextBytes || len(ev.Detail) > maxStreamEventDetailByte || len(ev.Tool) > 128 {
			return 0, fmt.Errorf("%w: event payload too large", ErrStreamBadRequest)
		}
		diff := nowUnix - ev.TS
		if diff < 0 {
			diff = -diff
		}
		if maxSkew > 0 && diff > maxSkew {
			return 0, fmt.Errorf("%w: timestamp outside allowed skew", ErrStreamUnauthorized)
		}
		if !VerifyStreamEvent(secret, ev) {
			return 0, fmt.Errorf("%w: invalid bridge signature", ErrStreamUnauthorized)
		}
	}

	var lastID uint64
	for _, ev := range events {
		lastID = h.Publish(ev)
	}
	return lastID, nil
}

// Publish appends ev to its session ring buffer, assigns a monotonic per-session ID,
// and fans it out non-blockingly to active subscribers.
func (h *StreamHub) Publish(ev StreamEvent) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := h.now()
	ss := h.ensureSessionLocked(ev.Session, now)
	ss.nextID++
	ev.ID = ss.nextID
	ss.lastTouch = now
	ss.events = append(ss.events, ev)
	if over := len(ss.events) - maxStreamEventsPerSess; over > 0 {
		ss.events = append([]StreamEvent(nil), ss.events[over:]...)
	}

	for sub := range ss.subscribers {
		select {
		case sub.ch <- ev:
		default:
			// Drop oldest queued event for a slow subscriber so publishers never stall.
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- ev:
			default:
			}
		}
	}
	return ev.ID
}

// Subscribe returns any buffered events for session with ID > after, a live channel
// for subsequent events, and an idempotent cancel function.
func (h *StreamHub) Subscribe(session string, after uint64) (backlog []StreamEvent, ch <-chan StreamEvent, cancel func()) {
	h.mu.Lock()
	now := h.now()
	ss := h.ensureSessionLocked(session, now)
	ss.lastTouch = now
	for _, ev := range ss.events {
		if ev.ID > after {
			backlog = append(backlog, ev)
		}
	}
	sub := &streamSubscriber{ch: make(chan StreamEvent, streamSubQueueSize)}
	ss.subscribers[sub] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel = func() {
		once.Do(func() {
			h.mu.Lock()
			if cur, ok := h.sessions[session]; ok {
				delete(cur.subscribers, sub)
				if len(cur.subscribers) == 0 && len(cur.events) == 0 {
					delete(h.sessions, session)
				}
			}
			h.mu.Unlock()
		})
	}
	return backlog, sub.ch, cancel
}

func (h *StreamHub) ensureSessionLocked(session string, now time.Time) *sessionStream {
	if ss, ok := h.sessions[session]; ok {
		return ss
	}
	h.pruneLocked(now)
	ss := &sessionStream{
		lastTouch:   now,
		subscribers: make(map[*streamSubscriber]struct{}),
	}
	h.sessions[session] = ss
	return ss
}

func (h *StreamHub) pruneLocked(now time.Time) {
	cutoff := now.Add(-streamSessionTTL)
	for id, ss := range h.sessions {
		if len(ss.subscribers) == 0 && ss.lastTouch.Before(cutoff) {
			delete(h.sessions, id)
		}
	}
	if len(h.sessions) < maxStreamSessions {
		return
	}
	// Evict oldest idle session if at capacity.
	var oldestID string
	var oldestTime time.Time
	for id, ss := range h.sessions {
		if len(ss.subscribers) > 0 {
			continue
		}
		if oldestID == "" || ss.lastTouch.Before(oldestTime) {
			oldestID = id
			oldestTime = ss.lastTouch
		}
	}
	if oldestID != "" {
		delete(h.sessions, oldestID)
	}
}
