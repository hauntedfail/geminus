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
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/e2ee"
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
	base       *url.URL
	key        string
	http       *http.Client
	e2eeURL    string
	e2eeSecret []byte
	now        func() time.Time
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

// WithE2EE enables app-layer E2EE tunneling for all Honcho API calls through
// the GE2E v1 terminating proxy at e2eeURL. Only https or loopback http URLs
// are accepted.
func (c *Client) WithE2EE(e2eeURL string, secret []byte) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(e2eeURL))
	if err != nil || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("invalid E2EE proxy address %q", e2eeURL)
	}
	host := u.Hostname()
	loopback := u.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1")
	if u.Scheme != "https" && !loopback {
		return nil, fmt.Errorf("E2EE proxy address must be https or loopback http: %q", e2eeURL)
	}
	if len(secret) < e2ee.MinSecretBytes {
		return nil, e2ee.ErrInvalidSecret
	}
	c.e2eeURL = u.String()
	c.e2eeSecret = append([]byte(nil), secret...)
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

func (c *Client) call(ctx context.Context, method string, path []string, query url.Values, body, out any) error {
	u := c.base.JoinPath(path...)
	u.RawQuery = query.Encode()
	var rawBody []byte
	if body != nil {
		var err error
		rawBody, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	var status int
	var data []byte
	var err error
	if c.e2eeURL != "" {
		reqPath := u.Path
		if !strings.HasPrefix(reqPath, "/") {
			reqPath = "/" + reqPath
		}
		status, data, err = c.doE2EE(ctx, method, reqPath, u.RawQuery, rawBody)
	} else {
		status, data, err = c.doDirect(ctx, method, u.String(), rawBody)
	}
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusNotFound:
		return ErrNotFound
	case status < 200 || status > 299:
		return fmt.Errorf("Honcho answered HTTP %d", status)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unreadable answer from Honcho: %w", err)
	}
	return nil
}

func (c *Client) doDirect(ctx context.Context, method, rawURL string, rawBody []byte) (int, []byte, error) {
	var payload io.Reader
	if len(rawBody) > 0 {
		payload = bytes.NewReader(rawBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "geminus")
	res, err := c.http.Do(req)
	if err != nil {
		// The error names the address but never the token.
		return 0, nil, fmt.Errorf("Honcho is not reachable: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("Honcho's answer was cut off: %w", err)
	}
	return res.StatusCode, data, nil
}

func (c *Client) doE2EE(ctx context.Context, method, reqPath, rawQuery string, rawBody []byte) (int, []byte, error) {
	inner := e2ee.Request{
		Method: method,
		Path:   reqPath,
		Query:  rawQuery,
		Headers: map[string]string{
			"Authorization": "Bearer " + c.key,
			"Content-Type":  "application/json",
			"User-Agent":    "geminus",
		},
	}
	inner.SetBody(rawBody)
	ts := c.now().Unix()
	frame, salt, err := e2ee.SealRequest(c.e2eeSecret, inner, ts, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("Honcho E2EE seal failed: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.e2eeURL, bytes.NewReader(frame))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", e2ee.ContentType)
	req.Header.Set("User-Agent", "geminus")
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("Honcho E2EE proxy is not reachable: %w", err)
	}
	defer res.Body.Close()
	outFrame, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("Honcho E2EE answer was cut off: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("Honcho E2EE proxy answered HTTP %d", res.StatusCode)
	}
	innerRes, err := e2ee.OpenResponse(c.e2eeSecret, outFrame, ts, salt)
	if err != nil {
		return 0, nil, fmt.Errorf("Honcho E2EE response failed verification: %w", err)
	}
	body, err := innerRes.BodyBytes()
	if err != nil {
		return 0, nil, fmt.Errorf("Honcho E2EE response body invalid: %w", err)
	}
	return innerRes.Status, body, nil
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
