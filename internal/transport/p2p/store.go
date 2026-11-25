package p2p

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hauntedfail/geminus/internal/honcho"
)

// LocalStore is a bridge-owned, file-backed (or in-memory when dir == "")
// implementation of transport.Backend. Every peer metadata entry, session
// descriptor, and message retains its original internal/wire signature.
type LocalStore struct {
	dir string

	mu       sync.RWMutex
	peers    map[string]*honcho.Peer
	sessions map[string]*storedSession

	// OnMessage is invoked asynchronously (if non-nil) after Post appends a message.
	OnMessage func(sessionID string, msg honcho.Message)
}

type storedSession struct {
	Session  honcho.Session   `json:"session"`
	Peers    []string         `json:"peers,omitempty"`
	Messages []honcho.Message `json:"messages,omitempty"`
}

type persistedState struct {
	Peers    map[string]*honcho.Peer   `json:"peers"`
	Sessions map[string]*storedSession `json:"sessions"`
}

// NewLocalStore opens or initializes a LocalStore at dir. When dir is empty,
// the store is kept in memory only (useful for tests).
func NewLocalStore(dir string) (*LocalStore, error) {
	s := &LocalStore{
		dir:      dir,
		peers:    map[string]*honcho.Peer{},
		sessions: map[string]*storedSession{},
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("p2p store mkdir: %w", err)
		}
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *LocalStore) statePath() string {
	return filepath.Join(s.dir, "state.json")
}

func (s *LocalStore) load() error {
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("p2p store read: %w", err)
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("p2p store parse: %w", err)
	}
	if st.Peers != nil {
		s.peers = st.Peers
	}
	if st.Sessions != nil {
		s.sessions = st.Sessions
	}
	return nil
}

func (s *LocalStore) saveLocked() error {
	if s.dir == "" {
		return nil
	}
	data, err := json.MarshalIndent(persistedState{
		Peers:    s.peers,
		Sessions: s.sessions,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath())
}

func (s *LocalStore) Name() string { return "local" }

func (s *LocalStore) Peers(_ context.Context) ([]honcho.Peer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]honcho.Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, clonePeer(*p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *LocalStore) SetPeerMetadata(_ context.Context, peer, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[peer]
	if !ok {
		p = &honcho.Peer{ID: peer, Metadata: map[string]json.RawMessage{}}
		s.peers[peer] = p
	}
	if p.Metadata == nil {
		p.Metadata = map[string]json.RawMessage{}
	}
	p.Metadata[key] = raw
	return s.saveLocked()
}

func (s *LocalStore) Sessions(_ context.Context, filter map[string]any) ([]honcho.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []honcho.Session
	for _, ss := range s.sessions {
		if matchesFilter(ss.Session.Metadata, filter) {
			out = append(out, cloneSession(ss.Session))
		}
	}
	// Newest first by CreatedAt, matching Honcho.
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

func (s *LocalStore) CreateSession(_ context.Context, id string, metadata any, peers ...string) error {
	metaMap, err := toRawMap(metadata)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[id]; exists {
		return nil
	}
	for _, peer := range peers {
		if peer != "" {
			if _, ok := s.peers[peer]; !ok {
				s.peers[peer] = &honcho.Peer{ID: peer, Metadata: map[string]json.RawMessage{}}
			}
		}
	}
	s.sessions[id] = &storedSession{
		Session: honcho.Session{
			ID:        id,
			Metadata:  metaMap,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
		Peers: append([]string(nil), peers...),
	}
	return s.saveLocked()
}

func (s *LocalStore) SetSessionMetadata(_ context.Context, id string, metadata any) error {
	metaMap, err := toRawMap(metadata)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[id]
	if !ok {
		return honcho.ErrNotFound
	}
	ss.Session.Metadata = metaMap
	return s.saveLocked()
}

func (s *LocalStore) Messages(_ context.Context, session string) ([]honcho.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ss, ok := s.sessions[session]
	if !ok {
		return nil, honcho.ErrNotFound
	}
	out := make([]honcho.Message, len(ss.Messages))
	for i, m := range ss.Messages {
		out[i] = cloneMessage(m)
	}
	return out, nil
}

func (s *LocalStore) Latest(_ context.Context, session string, n int) ([]honcho.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ss, ok := s.sessions[session]
	if !ok {
		return nil, honcho.ErrNotFound
	}
	if n <= 0 || len(ss.Messages) == 0 {
		return nil, nil
	}
	start := 0
	if len(ss.Messages) > n {
		start = len(ss.Messages) - n
	}
	slice := ss.Messages[start:]
	out := make([]honcho.Message, len(slice))
	for i, m := range slice {
		out[i] = cloneMessage(m)
	}
	return out, nil
}

func (s *LocalStore) Post(_ context.Context, session, peer, content string, metadata any) (honcho.Message, error) {
	metaMap, err := toRawMap(metadata)
	if err != nil {
		return honcho.Message{}, err
	}
	var idBytes [12]byte
	_, _ = rand.Read(idBytes[:])
	msg := honcho.Message{
		ID:        "msg-" + hex.EncodeToString(idBytes[:]),
		PeerID:    peer,
		Content:   content,
		Metadata:  metaMap,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	s.mu.Lock()
	ss, ok := s.sessions[session]
	if !ok {
		s.mu.Unlock()
		return honcho.Message{}, honcho.ErrNotFound
	}
	ss.Messages = append(ss.Messages, msg)
	err = s.saveLocked()
	hook := s.OnMessage
	s.mu.Unlock()

	if err != nil {
		return honcho.Message{}, err
	}
	if hook != nil {
		go hook(session, cloneMessage(msg))
	}
	return cloneMessage(msg), nil
}

func (s *LocalStore) Ring(_ context.Context, _ string) error {
	return nil
}

func toRawMap(v any) (map[string]json.RawMessage, error) {
	if v == nil {
		return map[string]json.RawMessage{}, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

func matchesFilter(meta map[string]json.RawMessage, filter map[string]any) bool {
	if len(filter) == 0 {
		return true
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return false
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	return matchObject(doc, filter)
}

func matchObject(doc, filter map[string]any) bool {
	for k, want := range filter {
		got, ok := doc[k]
		if !ok {
			return false
		}
		if wantSub, ok := want.(map[string]any); ok {
			gotSub, ok := got.(map[string]any)
			if !ok || !matchObject(gotSub, wantSub) {
				return false
			}
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return false
		}
	}
	return true
}

func cloneRawMap(m map[string]json.RawMessage) map[string]json.RawMessage {
	if m == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(m))
	maps.Copy(out, m)
	return out
}

func clonePeer(p honcho.Peer) honcho.Peer {
	p.Metadata = cloneRawMap(p.Metadata)
	return p
}

func cloneSession(s honcho.Session) honcho.Session {
	s.Metadata = cloneRawMap(s.Metadata)
	return s
}

func cloneMessage(m honcho.Message) honcho.Message {
	m.Metadata = cloneRawMap(m.Metadata)
	return m
}
