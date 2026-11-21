package chat

// Who can be talked to and with what: read from Honcho every time.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/wire"
)

const callTimeout = 30 * time.Second

// staleAfter is the age at which a bridge's last report is called out.
const staleAfter = 15 * time.Minute

// Persona is one agent of the Honcho workspace: a label, and what the bridge
// on its computer reported.
type Persona struct {
	Name string
	// Local means the persona is this computer's own agent.
	Local bool
	// Reported is false when no bridge has published a valid node card.
	Reported  bool
	Harnesses []wire.Harness
	// Seen is when the bridge last reported.
	Seen time.Time
	// Home is the directory the bridge runs a prompt in when the
	// conversation names none; "" when the bridge did not say.
	Home string
	// endpoint is the computer's doorbell, if it published one.
	endpoint string
}

// Tag returns the locality label shown next to the persona's name.
func (p Persona) Tag() string {
	if p.Local {
		return "THIS COMPUTER"
	}
	return "REMOTE"
}

// who is the persona as shown in the prompt and status line. The local
// persona always carries its tag: talking to this computer's own agent
// through geminus is possible, but it should never be mistaken for a remote.
func who(p Persona) string {
	return oneLine(p.Name) + " [" + p.Tag() + "]"
}

func (a *App) call() (context.Context, context.CancelFunc) {
	return context.WithTimeout(a.ctx, callTimeout)
}

// loadDirectory reads the personas, what their computers offer, and the
// person's defaults from Honcho.
func (a *App) loadDirectory() error {
	ctx, cancel := a.call()
	defer cancel()
	peers, err := a.Honcho.Peers(ctx)
	if err != nil {
		return err
	}
	a.personas, a.defaults, a.defaultsSaved = nil, wire.Defaults{}, 0
	for _, peer := range peers {
		if peer.ID == a.Profile.Human {
			// Defaults that this group did not sign are not used.
			a.defaults, a.defaultsSaved, _ = wire.OpenDefaults(a.Profile.Secret, a.Profile.Human, peer.Metadata[wire.DefaultsKey])
			continue
		}
		if !a.Profile.IsAgent(peer.ID) {
			continue
		}
		p := Persona{Name: peer.ID, Local: peer.ID == a.Profile.Local}
		if card, ok := wire.ParseNodeCard(a.Profile.Secret, peer.ID, peer.Metadata[wire.NodeKey]); ok {
			p.Reported, p.Harnesses, p.Seen, p.Home = true, card.Harnesses, time.Unix(card.TS, 0), card.CWD
		}
		p.endpoint, _ = wire.Endpoint(a.Profile.Secret, peer.ID, peer.Metadata[wire.PushKey])
		a.personas = append(a.personas, p)
	}
	sort.Slice(a.personas, func(i, j int) bool { return a.personas[i].Name < a.personas[j].Name })
	if a.defaults.Personas == nil {
		a.defaults.Personas = map[string]wire.PersonaDefaults{}
	}
	return nil
}

func (a *App) find(name string) (Persona, bool) {
	for _, p := range a.personas {
		if p.Name == name {
			return p, true
		}
	}
	return Persona{}, false
}

// ago describes how long ago t was.
func (a *App) ago(t time.Time) string { return since(a.Now().Sub(t)) }

// offers describes what a persona's computer reported.
func (a *App) offers(p Persona) string {
	if !p.Reported {
		return "no bridge has reported from this computer"
	}
	ids := make([]string, len(p.Harnesses))
	for i, h := range p.Harnesses {
		ids[i] = h.ID
	}
	list := strings.Join(ids, " ")
	if list == "" {
		list = "no agent program found"
	}
	return fmt.Sprintf("%s  (bridge seen %s)", list, a.ago(p.Seen))
}

func (a *App) personaOptions() (title string, opts []option) {
	title = "Personas (Honcho workspace " + oneLine(a.Profile.Workspace) + ")"
	for _, p := range a.personas {
		text := fmt.Sprintf("%-13s  %s", p.Tag(), a.offers(p))
		if p.Name == a.defaults.Persona {
			text += "  (default)"
		}
		opts = append(opts, option{key: p.Name, text: text})
	}
	return title, opts
}

func (a *App) showPersonas() {
	title, opts := a.personaOptions()
	a.Out.line(a.Out.bold(title))
	if len(opts) == 0 {
		a.Out.line("  none found")
	}
	a.Out.showOptions(opts)
}

// pickPersona asks who to talk to. It returns false if the input ended.
func (a *App) pickPersona() (string, bool) {
	title, opts := a.personaOptions()
	if len(opts) == 0 {
		a.Out.line(a.Out.fail("✗ Honcho lists no personas for this group"))
		return "", false
	}
	return a.choose(title, "persona", opts, a.defaults.Persona)
}

// selectPersona makes name the persona being talked to. No harness is chosen
// yet: a reopened conversation brings its own.
func (a *App) selectPersona(name string) error {
	p, ok := a.find(name)
	if !ok {
		return fmt.Errorf("no persona named %q in Honcho", name)
	}
	a.cur, a.harness, a.session, a.title, a.tuning = &p, nil, "", "", wire.Tuning{}
	a.cwd = a.defaults.Personas[p.Name].CWD
	a.shown, a.waiting, a.state = map[string]bool{}, map[string]bool{}, map[string]string{}
	return nil
}

// start selects the persona and the harness for a new conversation.
func (a *App) start(name, harnessID string) error {
	if err := a.selectPersona(name); err != nil {
		return err
	}
	return a.useHarness(harnessID)
}

// useHarness selects the agent program on the current persona's computer.
// With no id it uses the default, the only one there is, or asks.
func (a *App) useHarness(id string) error {
	p := *a.cur
	switch {
	case !p.Reported:
		return fmt.Errorf("no bridge has reported from %s's computer, so there is nothing to talk to yet; geminish 0.5 or later must be running there", p.Name)
	case len(p.Harnesses) == 0:
		return fmt.Errorf("the bridge on %s's computer found no agent program there", p.Name)
	}
	has := func(id string) bool {
		for _, h := range p.Harnesses {
			if h.ID == id {
				return true
			}
		}
		return false
	}
	if id == "" && has(a.defaults.Personas[p.Name].Harness) {
		id = a.defaults.Personas[p.Name].Harness
	}
	if id == "" && len(p.Harnesses) == 1 {
		id = p.Harnesses[0].ID
	}
	if id == "" {
		var opts []option
		for _, h := range p.Harnesses {
			opts = append(opts, option{key: h.ID, text: h.Name})
		}
		picked, ok := a.choose("Harnesses on "+who(p)+" right now", "harness", opts, "")
		if !ok {
			return errors.New("no harness chosen")
		}
		id = picked
	}
	for _, h := range p.Harnesses {
		if h.ID == id {
			h := h
			a.harness, a.session, a.title, a.tuning = &h, "", "", wire.Tuning{}
			a.shown, a.waiting, a.state = map[string]bool{}, map[string]bool{}, map[string]string{}
			return nil
		}
	}
	ids := make([]string, len(p.Harnesses))
	for i, h := range p.Harnesses {
		ids[i] = h.ID
	}
	return fmt.Errorf("%s's computer has no harness %q (it has: %s)", p.Name, id, strings.Join(ids, ", "))
}

func (a *App) status() string {
	if a.cur == nil {
		return "○ no persona chosen (/persona)"
	}
	p := *a.cur
	if a.harness == nil {
		return "○ " + who(p) + "  " + a.offers(p)
	}
	bridge := "bridge seen " + a.ago(p.Seen)
	if a.Now().Sub(p.Seen) > staleAfter {
		bridge += " (it may be off; prompts wait in Honcho until it is back)"
	}
	conversation := "new conversation"
	if a.session != "" {
		conversation = "conversation " + oneLine(a.session)
	}
	where := "  in " + a.where()
	tuned := ""
	if a.tuning.Model != "" {
		tuned += "  model " + oneLine(a.tuning.Model)
	}
	if a.tuning.Effort != "" {
		tuned += "  effort " + oneLine(a.tuning.Effort)
	}
	update := ""
	if v := a.newerRelease(); v != "" {
		update = "  update " + v + " available (/update)"
	}
	return fmt.Sprintf("● %s  %s%s  %s  %s%s%s", who(p), oneLine(a.harness.Name), tuned, bridge, conversation, where, update)
}

// where names the directory on the persona's computer that the conversation
// runs in: the one chosen, or the one the bridge there uses when none is.
func (a *App) where() string {
	switch {
	case a.cwd != "":
		return oneLine(a.cwd)
	case a.cur != nil && a.cur.Home != "":
		return oneLine(a.cur.Home) + " (default)"
	case a.cur != nil:
		return oneLine(a.cur.Name) + "'s default directory"
	}
	return "the default directory"
}

// validDir refuses a directory name that could not be shown as it is.
// Whether it exists is for the computer it is on to say.
func validDir(dir string) error {
	if dir != oneLine(dir) || strings.ContainsAny(dir, "\n\t") {
		return errors.New("a directory name cannot have line breaks, tabs or control characters")
	}
	return nil
}

// chdir shows or sets the directory the next conversation runs in. A
// conversation stays in the directory it started in: the agent program's
// session there belongs to it.
func (a *App) chdir(dir string) error {
	if a.cur == nil {
		return errors.New("pick a persona first (/persona)")
	}
	name := oneLine(a.cur.Name)
	if dir == "" {
		a.Out.line("directory: " + a.where() + " on " + name + a.dirNote())
		return nil
	}
	if a.session != "" {
		return fmt.Errorf("this conversation runs in %s on %s and stays there; /new DIR starts another one in DIR", a.where(), name)
	}
	if dir == "default" || dir == "none" {
		dir = ""
	}
	if err := validDir(dir); err != nil {
		return err
	}
	a.cwd = dir
	a.Out.line("directory: " + a.where() + " on " + name + a.dirNote())
	return nil
}

// dirNote says what can still be done about the directory.
func (a *App) dirNote() string {
	switch {
	case a.session != "":
		return "  (fixed for this conversation; /new DIR starts another one in DIR)"
	case a.cwd == "":
		return "  (/cwd DIR sets another for the conversation the next prompt starts)"
	}
	return "  (for the conversation the next prompt starts; /cwd default goes back to the bridge's own)"
}
