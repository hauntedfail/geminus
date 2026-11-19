// Package wire is the geminus protocol: the records geminus and the bridges
// exchange through Honcho, and how they are signed. docs/protocol.md is the
// specification; the bridge (a part of geminish) implements the other side.
package wire

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	// Kind marks a geminus session, and is the metadata key of a geminus
	// message. geminish mail uses "geminise" for both.
	Kind = "geminus"
	// NodeKey is the peer-metadata key of a computer's node card.
	NodeKey = "geminus_node"
	// PushKey is the peer-metadata key of geminish's doorbell record.
	PushKey = "geminise_push"
	// DefaultsKey is the key of the owner's defaults in their peer metadata.
	DefaultsKey = "geminus"

	RolePrompt = "prompt"
	RoleOutput = "output"

	StateQueued  = "queued"
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
	StateRefused = "refused"
)

// Sign returns the signature of content for one purpose (label) at time ts.
// Each field is hashed on its own first, so none can run into its neighbour.
func Sign(secret []byte, content, label string, ts int64) string {
	c, l := sha256.Sum256([]byte(content)), sha256.Sum256([]byte(label))
	mac := hmac.New(sha256.New, secret)
	mac.Write(c[:])
	mac.Write(l[:])
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether sig is the signature of content for label at ts.
func Verify(secret []byte, content, label string, ts int64, sig string) bool {
	return hmac.Equal([]byte(Sign(secret, content, label, ts)), []byte(sig))
}

func bit(b bool) int {
	if b {
		return 1
	}
	return 0
}

// PromptLabel binds a prompt to its conversation, target and author.
func PromptLabel(session, persona, harness, human, nonce string) string {
	return fmt.Sprintf("geminus-prompt:%s:%s:%s:%s:%s", session, persona, harness, human, nonce)
}

// OutputLabel binds a piece of output to its turn and position.
func OutputLabel(session, turn string, seq int, final, ok bool) string {
	return fmt.Sprintf("geminus-output:%s:%s:%d:%d:%d", session, turn, seq, bit(final), bit(ok))
}

// NodeLabel binds a node card to the peer that published it.
func NodeLabel(peer string) string { return "geminus-node:" + peer }

// Session is the "geminus" object in a session's metadata.
type Session struct {
	V       int    `json:"v"`
	Human   string `json:"human"`
	Persona string `json:"persona"`
	Harness string `json:"harness"`
	CWD     string `json:"cwd,omitempty"`
	Title   string `json:"title,omitempty"`
	Pending bool   `json:"pending"`
	// Updated is when the last prompt was written, for "the latest
	// conversation": Honcho only records when a session was created.
	Updated int64 `json:"updated,omitempty"`
}

// SessionMetadata is a geminus session's whole metadata.
func SessionMetadata(s Session) map[string]any {
	return map[string]any{"kind": Kind, Kind: s}
}

// Message is the "geminus" object in a message's metadata.
type Message struct {
	V    int    `json:"v"`
	Role string `json:"role"`
	TS   int64  `json:"ts"`
	Sig  string `json:"sig"`

	// Prompt fields.
	Nonce  string `json:"nonce,omitempty"`
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`

	// Output fields.
	Turn   string `json:"turn,omitempty"`
	Seq    int    `json:"seq,omitempty"`
	Final  bool   `json:"final,omitempty"`
	OK     bool   `json:"ok,omitempty"`
	Native string `json:"native,omitempty"`
}

// Harness is one agent program a computer offers.
type Harness struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// NodeCard is what a bridge published about its computer.
type NodeCard struct {
	V         int       `json:"v"`
	Harnesses []Harness `json:"harnesses"`
	TS        int64     `json:"ts"`
	Sig       string    `json:"sig"`
}

// ParseNodeCard returns the card a member computer signed for exactly this
// peer, or false.
func ParseNodeCard(secret []byte, peer string, raw json.RawMessage) (NodeCard, bool) {
	var card NodeCard
	if len(raw) == 0 || json.Unmarshal(raw, &card) != nil || card.TS == 0 {
		return NodeCard{}, false
	}
	ids := make([]string, len(card.Harnesses))
	for i, h := range card.Harnesses {
		ids[i] = h.ID
	}
	if !Verify(secret, strings.Join(ids, ","), NodeLabel(peer), card.TS, card.Sig) {
		return NodeCard{}, false
	}
	return card, true
}

// Defaults are the owner's preselected choices, kept in their peer metadata.
type Defaults struct {
	V int `json:"v"`
	// Persona is who geminus talks to when started without a name.
	Persona string `json:"persona,omitempty"`
	// Personas holds the choices made for each persona.
	Personas map[string]PersonaDefaults `json:"personas,omitempty"`
}

// PersonaDefaults are the preselected harness and directory on one persona.
type PersonaDefaults struct {
	Harness string `json:"harness,omitempty"`
	CWD     string `json:"cwd,omitempty"`
}
