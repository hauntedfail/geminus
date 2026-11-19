// Package acp is a small Agent Client Protocol client.
//
// ACP is JSON-RPC 2.0 with one message per line over the agent's stdin and
// stdout. geminus keeps one agent process per connection and talks to it for
// the whole conversation; it never starts a process per turn.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

var (
	// ErrSessionNotFound means the agent has no session with the requested ID.
	ErrSessionNotFound = errors.New("the agent has no session with that ID")
	// ErrClosed means the agent's output ended, so no reply can arrive.
	ErrClosed = errors.New("connection to the agent closed")
)

const (
	protocolVersion = 1
	// How long Prompt keeps waiting for the agent to acknowledge a cancel.
	cancelGrace  = 10 * time.Second
	maxListPages = 5
)

// RPCError is an error reply from the agent.
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("%s failed: %s (code %d)", e.Method, e.Message, e.Code)
}

// Info is what the agent reports about itself during the handshake.
type Info struct {
	Name, Version string
	// CanLoad means sessions can be reopened with LoadSession.
	CanLoad bool
	// CanList means ListSessions is supported.
	CanList bool
}

// SessionInfo describes one session the agent knows about.
type SessionInfo struct {
	ID        string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"`
}

// Command is a slash command the agent itself understands. It is sent as an
// ordinary prompt whose text starts with "/" and the command's name.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ToolEvent reports the start or a status change of a tool call.
type ToolEvent struct {
	ID, Title, Status string
	// Update is false for the first event of a tool call.
	Update bool
}

// PermissionOption is one answer the agent offers for a permission request.
type PermissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	// Kind is allow_once, allow_always, reject_once or reject_always.
	Kind string `json:"kind"`
}

// PermissionRequest asks the person whether a tool call may run.
type PermissionRequest struct {
	Title   string
	Options []PermissionOption
}

// Handler receives what the agent sends while a call is in flight. Functions
// are called one at a time, in the order the agent sent the events. Any of
// them may be nil.
type Handler struct {
	// Text is a piece of the agent's reply.
	Text func(string)
	// Thought is a piece of the agent's reasoning.
	Thought func(string)
	// UserText is a piece of an earlier user message, replayed by LoadSession.
	UserText func(string)
	Tool     func(ToolEvent)
	// Permission returns the ID of the chosen option, or "" to decline.
	Permission func(PermissionRequest) string
}

// Client is one connection to an agent.
type Client struct {
	w    io.WriteCloser
	wmu  sync.Mutex
	done chan struct{}
	// diag returns recent diagnostic output of the agent process, for errors.
	diag func() string

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan frame
	session string
	handler *Handler
	// commands holds each session's latest list of agent commands.
	commands map[string][]Command

	// trace, when set, receives every protocol line in both directions.
	trace   io.Writer
	traceMu sync.Mutex
}

// traceLineBytes bounds one traced line; replies can be very long.
const traceLineBytes = 4000

// Trace makes the client log every protocol line it sends ("> ") and
// receives ("< ") to w, for diagnosing an agent that misbehaves. The log
// contains the conversation. Call it before the first request.
func (c *Client) Trace(w io.Writer) { c.trace = w }

func (c *Client) traced(direction string, line []byte) {
	if c.trace == nil {
		return
	}
	if len(line) > traceLineBytes {
		line = append(append([]byte(nil), line[:traceLineBytes]...), "…"...)
	}
	c.traceMu.Lock()
	defer c.traceMu.Unlock()
	fmt.Fprintf(c.trace, "%s %s %s\n", time.Now().Format("15:04:05.000"), direction, line)
}

type frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// New returns a client that writes requests to w and reads the agent's
// messages from r. diag may be nil.
func New(w io.WriteCloser, r io.Reader, diag func() string) *Client {
	if diag == nil {
		diag = func() string { return "" }
	}
	c := &Client{w: w, done: make(chan struct{}), diag: diag, pending: map[int64]chan frame{}, commands: map[string][]Command{}}
	go c.read(r)
	return c
}

// Done is closed when the agent's output ends.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) read(r io.Reader) {
	defer close(c.done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		c.traced("<", line)
		var f frame
		// A login shell may print a banner before the agent starts. Skip
		// anything that is not a JSON-RPC message.
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &f) != nil {
			continue
		}
		switch {
		case f.Method != "" && len(f.ID) > 0:
			c.serve(f)
		case f.Method != "":
			c.notify(f)
		case len(f.ID) > 0:
			c.resolve(f)
		}
	}
}

func (c *Client) resolve(f frame) {
	var id int64
	if json.Unmarshal(f.ID, &id) != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- f
	}
}

func (c *Client) active(sessionID string) *Handler {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handler == nil || sessionID != c.session {
		return nil
	}
	return c.handler
}

func (c *Client) notify(f frame) {
	if f.Method != "session/update" {
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind    string          `json:"sessionUpdate"`
			Content json.RawMessage `json:"content"`
			ID      string          `json:"toolCallId"`
			Title   string          `json:"title"`
			Status  string          `json:"status"`
			// Commands is sent whenever the agent's command list changes,
			// which may be outside any call.
			Commands []Command `json:"availableCommands"`
		} `json:"update"`
	}
	if json.Unmarshal(f.Params, &p) != nil {
		return
	}
	if p.Update.Kind == "available_commands_update" {
		c.mu.Lock()
		c.commands[p.SessionID] = p.Update.Commands
		c.mu.Unlock()
		return
	}
	h := c.active(p.SessionID)
	if h == nil {
		return
	}
	u := p.Update
	text := func(deliver func(string)) {
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if deliver != nil && json.Unmarshal(u.Content, &content) == nil && content.Type == "text" && content.Text != "" {
			deliver(content.Text)
		}
	}
	switch u.Kind {
	case "agent_message_chunk":
		text(h.Text)
	case "agent_thought_chunk":
		text(h.Thought)
	case "user_message_chunk":
		text(h.UserText)
	case "tool_call", "tool_call_update":
		if h.Tool != nil {
			h.Tool(ToolEvent{ID: u.ID, Title: u.Title, Status: u.Status, Update: u.Kind == "tool_call_update"})
		}
	}
}

// serve answers a request from the agent. Only permission requests are
// supported; geminus offers the agent no file system or terminal.
func (c *Client) serve(f frame) {
	reply := map[string]any{"jsonrpc": "2.0", "id": f.ID}
	if f.Method != "session/request_permission" {
		reply["error"] = map[string]any{"code": -32601, "message": "method not supported by geminus"}
		_ = c.send(reply)
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			Title string `json:"title"`
		} `json:"toolCall"`
		Options []PermissionOption `json:"options"`
	}
	outcome := map[string]any{"outcome": "cancelled"}
	if json.Unmarshal(f.Params, &p) == nil {
		if h := c.active(p.SessionID); h != nil && h.Permission != nil {
			chosen := h.Permission(PermissionRequest{Title: p.ToolCall.Title, Options: p.Options})
			for _, o := range p.Options {
				if chosen != "" && o.ID == chosen {
					outcome = map[string]any{"outcome": "selected", "optionId": chosen}
				}
			}
		}
	}
	reply["result"] = map[string]any{"outcome": outcome}
	_ = c.send(reply)
}

func (c *Client) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.traced(">", data)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

// Commands returns the slash commands the agent has announced for a session.
func (c *Client) Commands(sessionID string) []Command {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Command(nil), c.commands[sessionID]...)
}

// Err returns why the connection ended, or nil while it is open.
func (c *Client) Err() error {
	select {
	case <-c.done:
		return c.closed()
	default:
		return nil
	}
}

// closed describes a lost connection, with the agent's last words if any.
func (c *Client) closed() error {
	if d := strings.TrimSpace(c.diag()); d != "" {
		return fmt.Errorf("%w: %s", ErrClosed, d)
	}
	return ErrClosed
}

// start sends a request. The reply arrives on the returned channel; forget
// must be called once the caller stops waiting for it.
func (c *Client) start(method string, params any) (reply <-chan frame, forget func(), err error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan frame, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	forget = func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		forget()
		return nil, nil, c.closed()
	}
	return ch, forget, nil
}

// await waits for the reply to a started request and decodes it into out,
// which may be nil.
func (c *Client) await(ctx context.Context, method string, reply <-chan frame, out any) error {
	select {
	case f := <-reply:
		if f.Error != nil {
			return &RPCError{Method: method, Code: f.Error.Code, Message: f.Error.Message}
		}
		if out == nil {
			return nil
		}
		if len(f.Result) == 0 || string(f.Result) == "null" {
			return fmt.Errorf("%s: the agent sent an empty result", method)
		}
		if err := json.Unmarshal(f.Result, out); err != nil {
			return fmt.Errorf("%s: unreadable result: %w", method, err)
		}
		return nil
	case <-c.done:
		return c.closed()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// call sends a request and waits for its reply. out may be nil.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	reply, forget, err := c.start(method, params)
	if err != nil {
		return err
	}
	defer forget()
	return c.await(ctx, method, reply, out)
}

func (c *Client) setHandler(sessionID string, h *Handler) {
	c.mu.Lock()
	c.session, c.handler = sessionID, h
	c.mu.Unlock()
}

// Initialize performs the handshake. Call it once, before anything else.
func (c *Client) Initialize(ctx context.Context, clientVersion string) (Info, error) {
	var res struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
		AgentCapabilities struct {
			LoadSession         bool                       `json:"loadSession"`
			SessionCapabilities map[string]json.RawMessage `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	params := map[string]any{
		"protocolVersion":    protocolVersion,
		"clientCapabilities": map[string]any{},
		"clientInfo":         map[string]any{"name": "geminus", "version": clientVersion},
	}
	if err := c.call(ctx, "initialize", params, &res); err != nil {
		return Info{}, err
	}
	if res.ProtocolVersion != protocolVersion {
		return Info{}, fmt.Errorf("the agent speaks ACP version %d; geminus supports version %d", res.ProtocolVersion, protocolVersion)
	}
	_, canList := res.AgentCapabilities.SessionCapabilities["list"]
	return Info{
		Name:    res.AgentInfo.Name,
		Version: res.AgentInfo.Version,
		CanLoad: res.AgentCapabilities.LoadSession,
		CanList: canList,
	}, nil
}

func sessionParams(cwd string) map[string]any {
	return map[string]any{"cwd": cwd, "mcpServers": []any{}}
}

// NewSession starts a conversation in cwd on the agent's machine.
func (c *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "session/new", sessionParams(cwd), &res); err != nil {
		return "", err
	}
	if res.SessionID == "" {
		return "", errors.New("session/new: the agent returned no session ID")
	}
	return res.SessionID, nil
}

// LoadSession reopens an earlier conversation. The agent replays the
// conversation so far through h before LoadSession returns.
func (c *Client) LoadSession(ctx context.Context, cwd, sessionID string, h *Handler) error {
	c.setHandler(sessionID, h)
	defer c.setHandler("", nil)
	params := sessionParams(cwd)
	params["sessionId"] = sessionID
	// The protocol allows a null result here, so there is nothing to decode.
	// Hermes also answers null for an unknown ID; callers that can list
	// sessions should check the ID first.
	err := c.call(ctx, "session/load", params, nil)
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return fmt.Errorf("%w (%s): %s", ErrSessionNotFound, sessionID, rpcErr.Message)
	}
	return err
}

// ListSessions returns the agent's sessions for cwd, most recent first as the
// agent orders them. An empty cwd lists sessions for every directory.
func (c *Client) ListSessions(ctx context.Context, cwd string) ([]SessionInfo, error) {
	var all []SessionInfo
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		params := map[string]any{}
		if cwd != "" {
			params["cwd"] = cwd
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Sessions   []SessionInfo `json:"sessions"`
			NextCursor string        `json:"nextCursor"`
		}
		if err := c.call(ctx, "session/list", params, &res); err != nil {
			return all, err
		}
		all = append(all, res.Sessions...)
		if res.NextCursor == "" || len(res.Sessions) == 0 {
			break
		}
		cursor = res.NextCursor
	}
	return all, nil
}

// Prompt sends one user message and returns the agent's stop reason once the
// turn ends. Cancelling ctx asks the agent to stop the turn; Prompt then
// returns the stop reason "cancelled", or ctx's error if the agent does not
// acknowledge in time.
func (c *Client) Prompt(ctx context.Context, sessionID, text string, h *Handler) (string, error) {
	c.setHandler(sessionID, h)
	defer c.setHandler("", nil)

	params := map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": text}},
	}
	reply, forget, err := c.start("session/prompt", params)
	if err != nil {
		return "", err
	}
	defer forget()

	// The cancel is registered only now, so it can never overtake the prompt
	// it is meant to stop.
	wait, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	stop := context.AfterFunc(ctx, func() {
		_ = c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sessionID}})
		timer := time.AfterFunc(cancelGrace, giveUp)
		<-wait.Done()
		timer.Stop()
	})
	defer stop()

	var res struct {
		StopReason string `json:"stopReason"`
	}
	if err := c.await(wait, "session/prompt", reply, &res); err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	return res.StopReason, nil
}

// Close ends the connection by closing the agent's input.
func (c *Client) Close() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.w.Close()
}
