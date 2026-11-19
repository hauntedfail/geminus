// Package honcho is the small part of the Honcho v3 HTTP API that geminus
// uses: peers, sessions, messages and their metadata.
package honcho

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	pageSize = 50
	maxPages = 40
	timeout  = 20 * time.Second
)

// ErrNotFound means Honcho has no such object.
var ErrNotFound = errors.New("not found in Honcho")

// Client talks to one Honcho workspace.
type Client struct {
	base *url.URL
	key  string
	http *http.Client
}

// Peer is a participant of the workspace.
type Peer struct {
	ID       string                     `json:"id"`
	Metadata map[string]json.RawMessage `json:"metadata"`
}

// Session is one conversation.
type Session struct {
	ID        string                     `json:"id"`
	Metadata  map[string]json.RawMessage `json:"metadata"`
	CreatedAt string                     `json:"created_at"`
}

// Message is one message of a session.
type Message struct {
	ID        string                     `json:"id"`
	PeerID    string                     `json:"peer_id"`
	Content   string                     `json:"content"`
	Metadata  map[string]json.RawMessage `json:"metadata"`
	CreatedAt string                     `json:"created_at"`
}

// New returns a client for the workspace at baseURL.
func New(baseURL, workspace, apiKey string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid Honcho address %q", baseURL)
	}
	return &Client{
		base: u.JoinPath("v3", "workspaces", workspace),
		key:  apiKey,
		// A redirect would send the token somewhere else.
		http: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

// HTTP returns the client's HTTP client, for the doorbell.
func (c *Client) HTTP() *http.Client { return c.http }

func (c *Client) call(ctx context.Context, method string, path []string, query url.Values, body, out any) error {
	u := c.base.JoinPath(path...)
	u.RawQuery = query.Encode()
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "geminus")
	res, err := c.http.Do(req)
	if err != nil {
		// The error names the address but never the token.
		return fmt.Errorf("Honcho is not reachable: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("Honcho's answer was cut off: %w", err)
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case res.StatusCode < 200 || res.StatusCode > 299:
		return fmt.Errorf("Honcho answered HTTP %d", res.StatusCode)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unreadable answer from Honcho: %w", err)
	}
	return nil
}

// list reads every page of a list endpoint into out, a pointer to a slice.
func list[T any](ctx context.Context, c *Client, path []string, query url.Values, body any) ([]T, error) {
	var all []T
	if query == nil {
		query = url.Values{}
	}
	for page := 1; page <= maxPages; page++ {
		query.Set("page", fmt.Sprint(page))
		query.Set("size", fmt.Sprint(pageSize))
		var res struct {
			Items []T `json:"items"`
			Pages int `json:"pages"`
		}
		if err := c.call(ctx, http.MethodPost, path, query, body, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Items...)
		if len(res.Items) < pageSize || page >= res.Pages {
			break
		}
	}
	return all, nil
}

// Peers returns every peer of the workspace with its metadata.
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	return list[Peer](ctx, c, []string{"peers", "list"}, nil, map[string]any{})
}

// SetPeerMetadata sets one key of a peer's metadata and keeps the others.
func (c *Client) SetPeerMetadata(ctx context.Context, peer, key string, value any) error {
	peers, err := c.Peers(ctx)
	if err != nil {
		return err
	}
	for _, p := range peers {
		if p.ID != peer {
			continue
		}
		merged := map[string]any{}
		for k, v := range p.Metadata {
			merged[k] = v
		}
		merged[key] = value
		return c.call(ctx, http.MethodPut, []string{"peers", peer}, nil, map[string]any{"metadata": merged}, nil)
	}
	// Writing without the peer's current metadata would erase it.
	return fmt.Errorf("peer %s: %w", peer, ErrNotFound)
}

// Sessions returns the sessions whose metadata contains filter, newest first.
func (c *Client) Sessions(ctx context.Context, filter map[string]any) ([]Session, error) {
	query := url.Values{"reverse": {"true"}}
	return list[Session](ctx, c, []string{"sessions", "list"}, query, map[string]any{"filters": map[string]any{"metadata": filter}})
}

// CreateSession creates a session with the given peers, none of them
// observed: a geminus conversation is not material for Honcho's memory.
func (c *Client) CreateSession(ctx context.Context, id string, metadata any, peers ...string) error {
	members := map[string]any{}
	for _, p := range peers {
		members[p] = map[string]bool{"observe_me": false, "observe_others": false}
	}
	return c.call(ctx, http.MethodPost, []string{"sessions"}, nil,
		map[string]any{"id": id, "metadata": metadata, "peers": members}, nil)
}

// SetSessionMetadata replaces a session's metadata.
func (c *Client) SetSessionMetadata(ctx context.Context, id string, metadata any) error {
	return c.call(ctx, http.MethodPut, []string{"sessions", id}, nil, map[string]any{"metadata": metadata}, nil)
}

// Messages returns every message of a session, oldest first.
func (c *Client) Messages(ctx context.Context, session string) ([]Message, error) {
	return list[Message](ctx, c, []string{"sessions", session, "messages", "list"}, nil, map[string]any{})
}

// Latest returns the newest n messages of a session, oldest first. Watching
// a turn only needs these, however long the conversation is.
func (c *Client) Latest(ctx context.Context, session string, n int) ([]Message, error) {
	var res struct {
		Items []Message `json:"items"`
	}
	query := url.Values{"reverse": {"true"}, "page": {"1"}, "size": {fmt.Sprint(n)}}
	if err := c.call(ctx, http.MethodPost, []string{"sessions", session, "messages", "list"}, query, map[string]any{}, &res); err != nil {
		return nil, err
	}
	for i, j := 0, len(res.Items)-1; i < j; i, j = i+1, j-1 {
		res.Items[i], res.Items[j] = res.Items[j], res.Items[i]
	}
	return res.Items, nil
}

// Post stores one message and returns it as Honcho kept it.
func (c *Client) Post(ctx context.Context, session, peer, content string, metadata any) (Message, error) {
	var stored []Message
	body := map[string]any{"messages": []any{map[string]any{"peer_id": peer, "content": content, "metadata": metadata}}}
	if err := c.call(ctx, http.MethodPost, []string{"sessions", session, "messages"}, nil, body, &stored); err != nil {
		return Message{}, err
	}
	if len(stored) != 1 || stored[0].Content != content || stored[0].PeerID != peer {
		return Message{}, errors.New("Honcho did not store the message as written")
	}
	return stored[0], nil
}
