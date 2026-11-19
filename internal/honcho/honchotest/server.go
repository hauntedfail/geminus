// Package honchotest is an in-memory Honcho for tests. It implements the
// endpoints geminus and the geminish bridge use, with the behaviour that was
// observed on Honcho 3.6: metadata filters match nested objects, lists are
// paged, message content is immutable and metadata is replaceable.
package honchotest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

type record struct {
	ID        string         `json:"id"`
	PeerID    string         `json:"peer_id,omitempty"`
	Content   string         `json:"content"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt string         `json:"created_at"`
	SessionID string         `json:"session_id,omitempty"`
}

// Server is a fake Honcho workspace.
type Server struct {
	*httptest.Server
	// Workspace and Key are what a client must present.
	Workspace, Key string

	mu       sync.Mutex
	peers    []*record
	sessions []*record
	messages map[string][]*record
	next     int
	// Requests counts calls by "METHOD path-kind", for tests about traffic.
	Requests map[string]int
}

// New starts a server with the given peers.
func New(peers ...string) *Server {
	s := &Server{Workspace: "ws", Key: "test-key", messages: map[string][]*record{}, Requests: map[string]int{}}
	for _, p := range peers {
		s.peers = append(s.peers, &record{ID: p, Metadata: map[string]any{}})
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// SetPeerMetadata puts a value into a peer's metadata directly.
func (s *Server) SetPeerMetadata(peer, key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.ID == peer {
			p.Metadata[key] = roundTrip(value)
		}
	}
}

// PeerMetadata returns one key of a peer's metadata as JSON.
func (s *Server) PeerMetadata(peer, key string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.ID == peer {
			data, _ := json.Marshal(p.Metadata[key])
			return data
		}
	}
	return nil
}

// Stored is a session or message as the server keeps it.
type Stored struct {
	ID, PeerID, Content string
	Metadata            map[string]any
}

func (r *record) stored() Stored {
	return Stored{ID: r.ID, PeerID: r.PeerID, Content: r.Content, Metadata: roundTrip(r.Metadata).(map[string]any)}
}

// Sessions returns every session, oldest first.
func (s *Server) Sessions() []Stored {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Stored, len(s.sessions))
	for i, r := range s.sessions {
		out[i] = r.stored()
	}
	return out
}

// Messages returns the messages of a session, oldest first.
func (s *Server) Messages(session string) []Stored {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Stored, len(s.messages[session]))
	for i, r := range s.messages[session] {
		out[i] = r.stored()
	}
	return out
}

// Append stores a message directly, as any holder of the token could.
func (s *Server) Append(session, peer, content string, metadata any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	r := &record{ID: fmt.Sprintf("msg-%04d", s.next), PeerID: peer, Content: content, SessionID: session,
		Metadata: roundTrip(metadata).(map[string]any), CreatedAt: time.Unix(1_795_000_000+int64(s.next), 0).UTC().Format(time.RFC3339)}
	s.messages[session] = append(s.messages[session], r)
	return r.ID
}

// SetMetadata replaces the metadata of a session (message == "") or of one
// of its messages.
func (s *Server) SetMetadata(session, message string, metadata any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.sessions
	id := session
	if message != "" {
		list, id = s.messages[session], message
	}
	for _, r := range list {
		if r.ID == id {
			r.Metadata = roundTrip(metadata).(map[string]any)
		}
	}
}

func roundTrip(v any) any {
	data, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}

// contains reports whether have includes everything in want, descending into
// nested objects.
func contains(have, want any) bool {
	w, ok := want.(map[string]any)
	if !ok {
		a, _ := json.Marshal(have)
		b, _ := json.Marshal(want)
		return string(a) == string(b)
	}
	h, ok := have.(map[string]any)
	if !ok {
		return false
	}
	for k, v := range w {
		if !contains(h[k], v) {
			return false
		}
	}
	return true
}

func page(w http.ResponseWriter, r *http.Request, items []*record) {
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if n < 1 {
		n = 1
	}
	if size < 1 {
		size = 50
	}
	if r.URL.Query().Get("reverse") == "true" {
		reversed := make([]*record, len(items))
		for i, item := range items {
			reversed[len(items)-1-i] = item
		}
		items = reversed
	}
	start, end := min((n-1)*size, len(items)), min(n*size, len(items))
	out := items[start:end]
	if out == nil {
		out = []*record{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": out, "page": n, "size": size,
		"total": len(items), "pages": (len(items) + size - 1) / size})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	prefix := "/v3/workspaces/" + s.Workspace + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "unknown workspace", http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.Key {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	var body struct {
		ID       string            `json:"id"`
		Metadata map[string]any    `json:"metadata"`
		Peers    map[string]any    `json:"peers"`
		Filters  map[string]any    `json:"filters"`
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	filter, _ := body.Filters["metadata"].(map[string]any)

	s.mu.Lock()
	defer s.mu.Unlock()
	kind := r.Method + " " + parts[0]
	if len(parts) > 2 {
		kind += "/" + parts[2]
	}
	if len(parts) > 1 && parts[len(parts)-1] == "list" {
		kind += " list"
	}
	s.Requests[kind]++
	stamp := func() string {
		s.next++
		return time.Unix(1_795_000_000+int64(s.next), 0).UTC().Format(time.RFC3339)
	}
	find := func(list []*record, id string) *record {
		for _, item := range list {
			if item.ID == id {
				return item
			}
		}
		return nil
	}

	switch {
	case r.Method == http.MethodPost && len(parts) == 2 && parts[0] == "peers" && parts[1] == "list":
		page(w, r, s.peers)
	case r.Method == http.MethodPut && len(parts) == 2 && parts[0] == "peers":
		p := find(s.peers, parts[1])
		if p == nil {
			http.Error(w, "no such peer", http.StatusNotFound)
			return
		}
		p.Metadata = body.Metadata
		_ = json.NewEncoder(w).Encode(p)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[0] == "sessions" && parts[1] == "list":
		var out []*record
		for _, item := range s.sessions {
			if filter == nil || contains(map[string]any(item.Metadata), filter) {
				out = append(out, item)
			}
		}
		page(w, r, out)
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "sessions":
		if existing := find(s.sessions, body.ID); existing != nil {
			_ = json.NewEncoder(w).Encode(existing) // get-or-create, as Honcho does
			return
		}
		item := &record{ID: body.ID, Metadata: body.Metadata, CreatedAt: stamp()}
		s.sessions = append(s.sessions, item)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(item)
	case r.Method == http.MethodPut && len(parts) == 2 && parts[0] == "sessions":
		item := find(s.sessions, parts[1])
		if item == nil {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		item.Metadata = body.Metadata
		_ = json.NewEncoder(w).Encode(item)
	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "sessions" && parts[2] == "messages" && parts[3] == "list":
		var out []*record
		for _, item := range s.messages[parts[1]] {
			if filter == nil || contains(map[string]any(item.Metadata), filter) {
				out = append(out, item)
			}
		}
		page(w, r, out)
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "sessions" && parts[2] == "messages":
		if find(s.sessions, parts[1]) == nil {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		var stored []*record
		for _, raw := range body.Messages {
			item := &record{}
			_ = json.Unmarshal(raw, item)
			s.next++
			item.ID, item.SessionID, item.CreatedAt = fmt.Sprintf("msg-%04d", s.next), parts[1], stamp()
			if item.Metadata == nil {
				item.Metadata = map[string]any{}
			}
			s.messages[parts[1]] = append(s.messages[parts[1]], item)
			stored = append(stored, item)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(stored)
	case r.Method == http.MethodPut && len(parts) == 4 && parts[0] == "sessions" && parts[2] == "messages":
		item := find(s.messages[parts[1]], parts[3])
		if item == nil {
			http.Error(w, "no such message", http.StatusNotFound)
			return
		}
		item.Metadata = body.Metadata // content is immutable
		_ = json.NewEncoder(w).Encode(item)
	default:
		http.Error(w, "not implemented in honchotest: "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
	}
}
