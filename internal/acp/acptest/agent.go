// Package acptest is a scripted ACP agent for tests. It never calls a model.
//
// What it does with a prompt depends on the prompt text:
//
//	"tool ..."  asks permission for a tool call, then reports the answer
//	"slow ..."  streams nothing until the turn is cancelled
//	"die ..."   writes to stderr and exits in the middle of the turn
//	anything    replies "echo: <text>", preceded by a thought
package acptest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// AgentName is the name the agent reports in the handshake.
const AgentName = "fake-agent"

type turn struct {
	User  string `json:"user"`
	Agent string `json:"agent"`
}

type state struct {
	Order    []string          `json:"order"`
	Sessions map[string][]turn `json:"sessions"`
}

type agent struct {
	out       *json.Encoder
	outMu     sync.Mutex
	statePath string
	state     state
	stderr    io.Writer

	mu       sync.Mutex
	nextID   int
	replies  map[int]chan json.RawMessage
	cancels  map[string]chan struct{}
	inFlight sync.WaitGroup
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Params struct {
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
		Prompt    []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	} `json:"params"`
}

// Serve speaks ACP on r and w until r ends. Sessions are kept in the JSON
// file at statePath so that a later Serve can load them; an empty statePath
// keeps them in memory only. It returns the process exit code.
func Serve(r io.Reader, w, stderr io.Writer, statePath string) int {
	a := &agent{
		out:       json.NewEncoder(w),
		statePath: statePath,
		stderr:    stderr,
		state:     state{Sessions: map[string][]turn{}},
		replies:   map[int]chan json.RawMessage{},
		cancels:   map[string]chan struct{}{},
	}
	if data, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(data, &a.state)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		var req request
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		if code, exit := a.handle(req); exit {
			return code
		}
	}
	a.inFlight.Wait()
	return 0
}

func (a *agent) send(v any) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	_ = a.out.Encode(v)
}

func (a *agent) result(id json.RawMessage, v any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": v})
}

func (a *agent) fail(id json.RawMessage, message string) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32002, "message": message}})
}

func (a *agent) update(sessionID string, update map[string]any) {
	a.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": update}})
}

func (a *agent) chunk(sessionID, kind, text string) {
	a.update(sessionID, map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}})
}

// turns returns a copy of a session's history.
func (a *agent) turns(sid string) ([]turn, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	turns, ok := a.state.Sessions[sid]
	return append([]turn(nil), turns...), ok
}

// save writes the state file. The caller holds a.mu.
func (a *agent) save() {
	if a.statePath == "" {
		return
	}
	if data, err := json.Marshal(a.state); err == nil {
		_ = os.WriteFile(a.statePath, data, 0o600)
	}
}

func (a *agent) handle(req request) (code int, exit bool) {
	sid := req.Params.SessionID
	switch req.Method {
	case "":
		// A reply to one of our permission requests.
		var id int
		if json.Unmarshal(req.ID, &id) == nil {
			a.mu.Lock()
			ch := a.replies[id]
			delete(a.replies, id)
			a.mu.Unlock()
			if ch != nil {
				ch <- req.Result
			}
		}
	case "initialize":
		a.result(req.ID, map[string]any{
			"protocolVersion": 1,
			"agentInfo":       map[string]any{"name": AgentName, "version": "1.2.3"},
			"agentCapabilities": map[string]any{
				"loadSession":         true,
				"sessionCapabilities": map[string]any{"list": map[string]any{}},
			},
		})
	case "session/new":
		a.mu.Lock()
		id := fmt.Sprintf("sess-%04d", len(a.state.Order)+1)
		a.state.Order = append(a.state.Order, id)
		a.state.Sessions[id] = nil
		a.save()
		a.mu.Unlock()
		a.result(req.ID, map[string]any{"sessionId": id})
	case "session/load":
		turns, ok := a.turns(sid)
		if !ok {
			a.fail(req.ID, "session not found")
			break
		}
		for _, t := range turns {
			a.chunk(sid, "user_message_chunk", t.User)
			a.chunk(sid, "agent_message_chunk", t.Agent)
		}
		a.result(req.ID, nil)
	case "session/list":
		sessions := []map[string]any{}
		a.mu.Lock()
		defer a.mu.Unlock()
		for i := len(a.state.Order) - 1; i >= 0; i-- {
			id := a.state.Order[i]
			title := ""
			if turns := a.state.Sessions[id]; len(turns) > 0 {
				title = turns[0].User
			}
			sessions = append(sessions, map[string]any{"sessionId": id, "cwd": "/work", "title": title})
		}
		a.result(req.ID, map[string]any{"sessions": sessions})
	case "session/cancel":
		a.mu.Lock()
		if ch := a.cancels[sid]; ch != nil {
			close(ch)
			delete(a.cancels, sid)
		}
		a.mu.Unlock()
	case "session/prompt":
		if _, ok := a.turns(sid); !ok {
			a.fail(req.ID, "session not found")
			break
		}
		text := ""
		if len(req.Params.Prompt) > 0 {
			text = req.Params.Prompt[0].Text
		}
		if strings.HasPrefix(text, "die") {
			fmt.Fprintln(a.stderr, "fake-agent: fatal: asked to die")
			return 3, true
		}
		cancelled := make(chan struct{})
		a.mu.Lock()
		a.cancels[sid] = cancelled
		a.mu.Unlock()
		a.inFlight.Add(1)
		// Run the turn off the read loop so cancels and permission replies
		// can arrive while it is in flight.
		go func() {
			defer a.inFlight.Done()
			a.prompt(req.ID, sid, text, cancelled)
		}()
	default:
		a.fail(req.ID, "method not found")
	}
	return 0, false
}

func (a *agent) prompt(id json.RawMessage, sid, text string, cancelled <-chan struct{}) {
	reply := "echo: " + text
	switch {
	case strings.HasPrefix(text, "slow"):
		<-cancelled
		a.result(id, map[string]any{"stopReason": "cancelled"})
		return
	case strings.HasPrefix(text, "tool"):
		a.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "terminal: ls", "status": "pending"})
		a.mu.Lock()
		a.nextID++
		reqID := a.nextID
		answer := make(chan json.RawMessage, 1)
		a.replies[reqID] = answer
		a.mu.Unlock()
		a.send(map[string]any{"jsonrpc": "2.0", "id": reqID, "method": "session/request_permission", "params": map[string]any{
			"sessionId": sid,
			"toolCall":  map[string]any{"toolCallId": "t1", "title": "terminal: ls"},
			"options": []map[string]any{
				{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
				{"optionId": "always", "name": "Always allow", "kind": "allow_always"},
				{"optionId": "no", "name": "Reject", "kind": "reject_once"},
			},
		}})
		var res struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		_ = json.Unmarshal(<-answer, &res)
		status := "failed"
		if res.Outcome.OptionID == "yes" || res.Outcome.OptionID == "always" {
			status = "completed"
		}
		a.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": status})
		reply = "permission: " + res.Outcome.Outcome + " " + res.Outcome.OptionID
	default:
		a.chunk(sid, "agent_thought_chunk", "thinking about it")
	}
	half := len(reply) / 2
	a.chunk(sid, "agent_message_chunk", reply[:half])
	a.chunk(sid, "agent_message_chunk", reply[half:])
	a.mu.Lock()
	delete(a.cancels, sid)
	a.state.Sessions[sid] = append(a.state.Sessions[sid], turn{User: text, Agent: reply})
	a.save()
	a.mu.Unlock()
	a.result(id, map[string]any{"stopReason": "end_turn"})
}
