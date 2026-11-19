package chat

// Choosing who to talk to (a persona) and with what (a harness).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hauntedfail/geminus/internal/harness"
	"github.com/hauntedfail/geminus/internal/persona"
)

// where describes a persona's location for lists.
func where(p persona.Persona) string {
	if p.Node == nil {
		return "no route yet"
	}
	return p.Node.Where()
}

func (a *App) personaOptions() (title string, opts []option) {
	title = "Personas"
	if a.dir.Workspace != "" {
		title += " (Honcho workspace " + oneLine(a.dir.Workspace) + ")"
	}
	for _, p := range a.dir.Personas {
		text := fmt.Sprintf("%-13s  %s", p.Tag(), where(p))
		if p.Name == a.dir.Defaults.Persona {
			text += "  (default)"
		}
		opts = append(opts, option{key: p.Name, text: text})
	}
	return title, opts
}

func (a *App) showPersonas() {
	title, opts := a.personaOptions()
	a.Out.line(a.Out.bold(title))
	if a.dir.Warning != "" {
		a.Out.line(a.Out.warn("! " + oneLine(a.dir.Warning)))
	}
	a.Out.showOptions(opts)
}

// pickPersona asks who to talk to. It returns false if the input ended.
func (a *App) pickPersona() (string, bool) {
	title, opts := a.personaOptions()
	if a.dir.Warning != "" {
		a.Out.line(a.Out.warn("! " + oneLine(a.dir.Warning)))
	}
	if len(opts) == 0 {
		a.Out.line(a.Out.fail("✗ the directory lists no personas"))
		return "", false
	}
	return a.choose(title, "persona", opts, a.dir.Defaults.Persona)
}

// start selects the persona and the harness and opens a session.
func (a *App) start(name, harnessID, resume string, latest bool) error {
	if err := a.usePersona(name); err != nil {
		return err
	}
	if err := a.useHarness(harnessID); err != nil {
		return err
	}
	return a.connect(resume, latest)
}

// usePersona selects who to talk to and finds out what their computer has.
func (a *App) usePersona(name string) error {
	p, ok := a.dir.Find(name)
	if !ok {
		return fmt.Errorf("no persona named %q in the directory", name)
	}
	var node persona.Node
	if p.Node != nil {
		node = *p.Node
	} else {
		asked, err := a.askNode(p)
		if err != nil {
			return err
		}
		node = asked
		p.Node = &node
	}
	a.Out.line(a.Out.dim(fmt.Sprintf("looking at %s (%s)…", who(p), node.Where())))
	ctx, cancel := context.WithTimeout(a.ctx, connectTimeout)
	defer cancel()
	probe, err := a.Probe(ctx, node, a.dir.Harnesses)
	if err != nil {
		return fmt.Errorf("cannot reach %s (%s): %w", p.Name, node.Where(), err)
	}
	a.disconnect()
	a.cur, a.node, a.probe, a.har, a.last = &p, node, probe, nil, ""
	a.choices = probe.Choices(a.dir.Harnesses)
	return nil
}

// askNode asks where a persona without a route runs, and remembers it.
func (a *App) askNode(p persona.Persona) (persona.Node, error) {
	a.Out.line(fmt.Sprintf("%s has no route yet. geminus reaches a persona's computer over ssh.", who(p)))
	a.Out.write(fmt.Sprintf("ssh host alias for %s (empty cancels) ❯ ", oneLine(p.Name)))
	alias, ok := a.readLine()
	if !ok || alias == "" {
		return persona.Node{}, fmt.Errorf("no route to %s; set one with /persona %s or in %s", p.Name, p.Name, a.dir.ConfigPath)
	}
	node := persona.Node{SSH: alias}
	if err := a.SaveNode(p.Name, node); err != nil {
		return persona.Node{}, err
	}
	a.Out.line(a.Out.dim("saved in " + a.dir.ConfigPath))
	for i := range a.dir.Personas {
		if a.dir.Personas[i].Name == p.Name {
			a.dir.Personas[i].Node = &node
		}
	}
	return node, nil
}

// readLine reads one line of input outside a turn.
func (a *App) readLine() (string, bool) {
	select {
	case line, ok := <-a.lines:
		if !ok {
			a.Out.line("")
			return "", false
		}
		a.entered(line)
		return strings.TrimSpace(line), true
	case <-a.interrupt:
		a.Out.line("")
		return "", false
	}
}

func (a *App) harnessOptions() []option {
	var opts []option
	for _, c := range a.choices {
		text := c.Name
		if !c.Available {
			text += ": installed, but its ACP bridge is missing"
			if c.Install != "" {
				text += " (" + c.Install + ")"
			}
		}
		opts = append(opts, option{key: c.ID, text: text, disabled: !c.Available})
	}
	return opts
}

// preferredHarness is the harness to use when none was asked for: the
// persona's own default, then the general one, if this computer has it.
func (a *App) preferredHarness() string {
	for _, id := range []string{a.node.Harness, a.dir.Defaults.Harness} {
		for _, c := range a.choices {
			if id != "" && c.ID == id && c.Available {
				return id
			}
		}
	}
	return ""
}

// useHarness selects the agent program on the current persona's computer.
// With no id it uses the default, the only one there is, or asks.
func (a *App) useHarness(id string) error {
	var usable []harness.Choice
	for _, c := range a.choices {
		if c.Available {
			usable = append(usable, c)
		}
	}
	if len(usable) == 0 {
		msg := fmt.Sprintf("%s (%s) has no agent program geminus can talk to", a.cur.Name, a.node.Where())
		for _, c := range a.choices {
			msg += fmt.Sprintf("\n  %s is installed, but its ACP bridge is missing", c.Name)
			if c.Install != "" {
				msg += ": " + c.Install
			}
		}
		return errors.New(msg)
	}
	if id == "" {
		id = a.preferredHarness()
	}
	if id == "" && len(usable) == 1 {
		id = usable[0].ID
	}
	if id == "" {
		picked, ok := a.choose(fmt.Sprintf("Harnesses on %s", who(*a.cur)), "harness", a.harnessOptions(), "")
		if !ok {
			return errors.New("no harness chosen")
		}
		id = picked
	}
	for _, c := range a.choices {
		if c.ID != id {
			continue
		}
		if !c.Available {
			return fmt.Errorf("%s is installed on %s, but its ACP bridge is missing: %s", c.Name, a.cur.Name, c.Install)
		}
		h := c.Harness
		a.har = &h
		return nil
	}
	var ids []string
	for _, c := range usable {
		ids = append(ids, c.ID)
	}
	return fmt.Errorf("%s has no harness %q (it has: %s)", a.cur.Name, id, strings.Join(ids, ", "))
}
