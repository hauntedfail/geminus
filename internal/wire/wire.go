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
	"regexp"
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

// digest stands for a free-form value inside a label: hashed, it cannot run
// into the fields around it.
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Tuning is what the owner set for a conversation's runs of the agent
// program. An empty value means the program's own default.
type Tuning struct {
	Model  string
	Effort string
}

var (
	modelForm  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@#\[\]+-]{0,127}$`)
	effortForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
)

// ValidSetting reports whether value can be a model or an effort (kind). The
// bridge turns it into an argument of the agent program, so it must be a
// bare name that no program can read as an option or as two arguments. The
// empty string means "not set".
func ValidSetting(kind, value string) bool {
	if value == "" {
		return true
	}
	if strings.ContainsAny(value, "\r\n") {
		return false
	}
	if kind == "model" {
		return modelForm.MatchString(value)
	}
	return effortForm.MatchString(value)
}

// part is how the settings appear in a label: not at all when none is set.
// A record without settings is therefore signed the way it was before
// settings existed, and an older bridge still accepts it. Adding a setting
// to such a record, or taking the settings off one that has them, changes
// the label, so neither can be done behind the signer's back.
func (t Tuning) part() string {
	if t.Model == "" && t.Effort == "" {
		return ""
	}
	return ":" + digest(t.Model) + ":" + digest(t.Effort)
}

// PromptLabel binds a prompt to its conversation, target, author, the
// directory it is to run in, and the model and effort it asks for. All of
// these are part of what the owner signs: metadata alone could be rewritten
// by anyone holding the Honcho token.
func PromptLabel(session, persona, harness, human, nonce, cwd string, t Tuning) string {
	return fmt.Sprintf("geminus-prompt:%s:%s:%s:%s:%s:%s", session, persona, harness, human, nonce, digest(cwd)) + t.part()
}

// OutputLabel binds a piece of output to its turn, position and the agent
// program's own session ID.
func OutputLabel(session, turn string, seq int, final, ok bool, native string) string {
	return fmt.Sprintf("geminus-output:%s:%s:%d:%d:%d:%s", session, turn, seq, bit(final), bit(ok), digest(native))
}

// DefaultsLabel binds stored defaults to the person they belong to.
func DefaultsLabel(human string) string { return "geminus-defaults:" + human }

// NodeLabel binds a node card to the peer that published it.
func NodeLabel(peer string) string { return "geminus-node:" + peer }

// OptionsLabel binds the options in a node card to the peer that published
// it.
func OptionsLabel(peer string) string { return "geminus-node-options:" + peer }

// Session is the "geminus" object in a session's metadata.
type Session struct {
	V       int    `json:"v"`
	Human   string `json:"human"`
	Persona string `json:"persona"`
	Harness string `json:"harness"`
	CWD     string `json:"cwd,omitempty"`
	// Model and Effort are the owner's settings for this conversation; the
	// next prompt carries them. Empty means the agent program's default.
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Title   string `json:"title,omitempty"`
	Pending bool   `json:"pending"`
	// Updated is when the last prompt was written, for "the latest
	// conversation": Honcho only records when a session was created.
	Updated int64 `json:"updated,omitempty"`
	// Sig signs the description: everything above except Pending.
	Sig string `json:"sig,omitempty"`
}

// SessionLabel binds a session's description to its ID, target, owner,
// directory and settings.
func SessionLabel(session, persona, harness, human, cwd string, t Tuning) string {
	return fmt.Sprintf("geminus-session:%s:%s:%s:%s:%s", session, persona, harness, human, digest(cwd)) + t.part()
}

func (s Session) label(id string) string {
	return SessionLabel(id, s.Persona, s.Harness, s.Human, s.CWD, Tuning{Model: s.Model, Effort: s.Effort})
}

// SealSession signs the description of the session with the given ID. The
// harness and directory of a conversation decide where the owner's next
// prompt runs, so a description is only believed with this signature.
// Pending is left out: the bridge flips it, and it only causes a read.
func SealSession(secret []byte, id string, s Session) Session {
	s.Sig = Sign(secret, s.Title, s.label(id), s.Updated)
	return s
}

// VerifySession reports whether s is a description the group signed for the
// session with this ID.
func VerifySession(secret []byte, id string, s Session) bool {
	return s.Sig != "" && Verify(secret, s.Title, s.label(id), s.Updated, s.Sig)
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

	// Prompt fields. Model and Effort are what the owner set for this run;
	// empty means the agent program's default.
	Nonce  string `json:"nonce,omitempty"`
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`

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
	// Options are the settings the bridge can pass to the program for one
	// run: "model", "effort".
	Options []string `json:"options,omitempty"`
}

// Takes reports whether the program can be given the setting.
func (h Harness) Takes(option string) bool {
	for _, o := range h.Options {
		if o == option {
			return true
		}
	}
	return false
}

// CardText is what a node card's options signature covers: each program's
// ID followed by its options.
func CardText(harnesses []Harness) string {
	entries := make([]string, len(harnesses))
	for i, h := range harnesses {
		entries[i] = strings.Join(append([]string{h.ID}, h.Options...), ":")
	}
	return strings.Join(entries, ",")
}

// NodeCard is what a bridge published about its computer.
type NodeCard struct {
	V         int       `json:"v"`
	Harnesses []Harness `json:"harnesses"`
	TS        int64     `json:"ts"`
	// Sig signs the program IDs, as it has since before options existed, so
	// cards and readers of different ages still agree on which programs
	// there are. OptionsSig signs the IDs with their options.
	Sig        string `json:"sig"`
	OptionsSig string `json:"options_sig,omitempty"`
}

// ParseNodeCard returns the card a member computer signed for exactly this
// peer, or false.
func ParseNodeCard(secret []byte, peer string, raw json.RawMessage) (NodeCard, bool) {
	var card NodeCard
	if len(raw) == 0 || json.Unmarshal(raw, &card) != nil || card.TS == 0 {
		return NodeCard{}, false
	}
	// The signed text joins IDs and options with ":" and ",", so neither may
	// contain them: otherwise an option could be moved into an ID.
	for _, h := range card.Harnesses {
		for _, part := range append([]string{h.ID}, h.Options...) {
			if part == "" || strings.ContainsAny(part, ":,") {
				return NodeCard{}, false
			}
		}
	}
	ids := make([]string, len(card.Harnesses))
	for i, h := range card.Harnesses {
		ids[i] = h.ID
	}
	if !Verify(secret, strings.Join(ids, ","), NodeLabel(peer), card.TS, card.Sig) {
		return NodeCard{}, false
	}
	// Options nobody signed are not options: a program is then offered
	// without settings, as on a bridge from before they existed.
	if !Verify(secret, CardText(card.Harnesses), OptionsLabel(peer), card.TS, card.OptionsSig) {
		plain := make([]Harness, len(card.Harnesses))
		for i, h := range card.Harnesses {
			plain[i] = Harness{ID: h.ID, Name: h.Name}
		}
		card.Harnesses, card.OptionsSig = plain, ""
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

// sealed is how defaults are stored: the defaults as JSON text, signed.
// Unsigned defaults would let anyone holding the Honcho token choose which
// computer, program and directory the owner's next prompt goes to.
type sealed struct {
	V    int    `json:"v"`
	Data string `json:"data"`
	TS   int64  `json:"ts"`
	Sig  string `json:"sig"`
}

// SealDefaults returns the form in which d is stored in the owner's peer
// metadata.
func SealDefaults(secret []byte, human string, d Defaults, ts int64) (any, error) {
	d.V = 1
	data, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	return sealed{V: 1, Data: string(data), TS: ts, Sig: Sign(secret, string(data), DefaultsLabel(human), ts)}, nil
}

// OpenDefaults returns the defaults a member computer signed for this
// person and when they were saved, or false.
//
// The signature does not prove these are the newest: someone holding the
// Honcho token can put back an older signed copy. Telling them apart would
// need something remembered on this computer, and geminus keeps nothing; the
// time saved is returned so that it can be shown.
func OpenDefaults(secret []byte, human string, raw json.RawMessage) (d Defaults, saved int64, ok bool) {
	var box sealed
	if len(raw) == 0 || json.Unmarshal(raw, &box) != nil || box.Data == "" {
		return Defaults{}, 0, false
	}
	if !Verify(secret, box.Data, DefaultsLabel(human), box.TS, box.Sig) {
		return Defaults{}, 0, false
	}
	if json.Unmarshal([]byte(box.Data), &d) != nil {
		return Defaults{}, 0, false
	}
	return d, box.TS, true
}
