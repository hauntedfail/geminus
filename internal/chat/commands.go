package chat

// Slash commands.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/wire"
)

func (a *App) handle(line string) error {
	if !strings.HasPrefix(line, "/") {
		return a.say(line)
	}
	// "//text" sends "/text" to the agent even when geminus has a command of
	// that name.
	if strings.HasPrefix(line, "//") {
		return a.say(line[1:])
	}
	cmd, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "/help":
		a.Out.line(commandHelp)
	case "/status":
		if err := a.refresh(); err != nil {
			return err
		}
		a.Out.line(a.status())
	case "/persona", "/personas":
		if err := a.loadDirectory(); err != nil {
			return err
		}
		if arg == "" {
			picked, ok := a.pickPersona()
			if !ok {
				return nil
			}
			arg = picked
		}
		if err := a.start(arg, ""); err != nil {
			return err
		}
		a.Out.line(a.status())
	case "/harness":
		if a.cur == nil {
			return errors.New("pick a persona first (/persona)")
		}
		// Read again: what is installed there may have changed.
		if err := a.refresh(); err != nil {
			return err
		}
		if arg == "" && len(a.cur.Harnesses) > 1 {
			var opts []option
			for _, h := range a.cur.Harnesses {
				opts = append(opts, option{key: h.ID, text: h.Name})
			}
			picked, ok := a.choose("Harnesses on "+who(*a.cur)+" right now", "harness", opts, a.defaults.Personas[a.cur.Name].Harness)
			if !ok {
				return nil
			}
			arg = picked
		}
		if err := a.useHarness(arg); err != nil {
			return err
		}
		a.Out.line(a.status())
	case "/new":
		if a.cur == nil || a.harness == nil {
			return errors.New("pick a persona first (/persona)")
		}
		if err := validDir(arg); err != nil {
			return err
		}
		a.session, a.title = "", ""
		a.tuning = a.defaultTuning(a.cur.Name, *a.harness)
		a.shown, a.waiting, a.state = map[string]bool{}, map[string]bool{}, map[string]string{}
		a.shownTurn, a.turnPrompt = map[string]bool{}, map[string]string{}
		if a.live != nil {
			a.live.setPrompt("")
		}
		if arg != "" {
			a.cwd = arg
		}
		a.Out.line(a.status())
	case "/cwd":
		return a.chdir(arg)
	case "/sessions":
		return a.showSessions()
	case "/resume":
		if err := a.resume(arg); err != nil {
			return err
		}
		a.Out.line(a.status())
	case "/default":
		return a.setDefaults(arg)
	case "/ui":
		return a.setUI(arg)
	case "/update":
		return a.slashUpdate()
	case "/model", "/effort":
		return a.tune(cmd[1:], arg)
	default:
		// Not one of geminus's: the agent program has slash commands of its
		// own, so the line goes to it as typed.
		return a.say(line)
	}
	return nil
}

// tune shows or sets the model or the effort (kind) of the open conversation.
// The value travels with each prompt and the bridge passes it to the agent
// program as an option of that one run, so the program's own configuration
// and its other sessions are left as they are.
func (a *App) tune(kind, value string) error {
	if a.cur == nil || a.harness == nil {
		return errors.New("pick a persona first (/persona)")
	}
	current := &a.tuning.Model
	if kind == "effort" {
		current = &a.tuning.Effort
	}
	show := func() {
		if *current == "" {
			a.Out.line(fmt.Sprintf("%s: %s's default  (set one for this conversation with /%s NAME)", kind, oneLine(a.harness.Name), kind))
			return
		}
		a.Out.line(fmt.Sprintf("%s: %s  (this conversation only; /%s default goes back to %s's own)", kind, oneLine(*current), kind, oneLine(a.harness.Name)))
	}
	if value == "" {
		show()
		return nil
	}
	if value == "default" || value == "none" {
		value = ""
	}
	if value != "" {
		if len(a.harness.Options) == 0 {
			// A bridge from before settings existed lists none for any program.
			return fmt.Errorf("the bridge on %s's computer is too old to pass a %s: geminish 0.6 or later must be running there. Prompts without one still work", a.cur.Name, kind)
		}
		if !a.harness.Takes(kind) {
			return fmt.Errorf("%s takes no %s setting from geminus on %s's computer", oneLine(a.harness.Name), kind, a.cur.Name)
		}
		if !wire.ValidSetting(kind, value) {
			return fmt.Errorf("%q is not a usable %s name: it is passed to %s as an option, so it must be one word of letters, digits and . _ - (a model may also have : / @ # + [ ])",
				oneLine(value), kind, oneLine(a.harness.Name))
		}
	}
	previous := *current
	*current = value
	if a.session != "" {
		// Kept in the conversation's signed description, so reopening it
		// from any computer brings the setting back. Prompts still waiting
		// keep the mark the bridge looks for.
		ctx, cancel := a.call()
		defer cancel()
		if err := a.Honcho.SetSessionMetadata(ctx, a.session, wire.SessionMetadata(a.meta(a.session, a.title, len(a.waiting) > 0))); err != nil {
			*current = previous
			return fmt.Errorf("the %s was not changed: %w", kind, err)
		}
	}
	show()
	return nil
}

// refresh re-reads Honcho and keeps the current persona selected.
func (a *App) refresh() error {
	if err := a.loadDirectory(); err != nil {
		return err
	}
	if a.cur == nil {
		return nil
	}
	p, ok := a.find(a.cur.Name)
	if !ok {
		return fmt.Errorf("%s is no longer listed in Honcho", a.cur.Name)
	}
	a.cur = &p
	return nil
}

// setDefaults shows or changes what geminus preselects. The defaults live in
// the owner's Honcho peer, so they are the same on every computer.
func (a *App) setDefaults(arg string) error {
	key, value, _ := strings.Cut(arg, " ")
	value = strings.TrimSpace(value)
	show := func(v string) string {
		if v == "" {
			return "(none)"
		}
		return oneLine(v)
	}
	if key == "" {
		head := "defaults (kept in Honcho, on " + oneLine(a.Profile.Human)
		if a.defaultsSaved > 0 {
			// An older signed copy can be put back by whoever holds the
			// Honcho token; the age makes that noticeable.
			head += ", saved " + a.ago(time.Unix(a.defaultsSaved, 0))
		}
		a.Out.line(head + ")")
		a.Out.line("  persona  " + show(a.defaults.Persona))
		names := make([]string, 0, len(a.defaults.Personas))
		for name := range a.defaults.Personas {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			d := a.defaults.Personas[name]
			line := fmt.Sprintf("  on %s  harness %s  cwd %s", oneLine(name), show(d.Harness), show(d.CWD))
			if d.Model != "" {
				line += "  model " + oneLine(d.Model)
			}
			if d.Effort != "" {
				line += "  effort " + oneLine(d.Effort)
			}
			a.Out.line(line)
		}
		return nil
	}
	if value == "none" {
		value = ""
	} else if value == "" && key != "save" {
		return fmt.Errorf("usage: /default %s VALUE (or none)", key)
	}
	d := a.cloneDefaults()
	current := func() (wire.PersonaDefaults, error) {
		if a.cur == nil {
			return wire.PersonaDefaults{}, errors.New("pick a persona first (/persona)")
		}
		return d.Personas[a.cur.Name], nil
	}
	said := ""
	switch key {
	case "save":
		if a.cur == nil || a.harness == nil {
			return errors.New("nothing to save: pick a persona and a harness first")
		}
		d.Persona = a.cur.Name
		d.Personas[a.cur.Name] = wire.PersonaDefaults{
			Harness: a.harness.ID,
			CWD:     a.cwd,
			Model:   a.tuning.Model,
			Effort:  a.tuning.Effort,
		}
		said = fmt.Sprintf("geminus now starts with %s · %s", who(*a.cur), oneLine(a.harness.Name))
	case "persona":
		if _, ok := a.find(value); value != "" && !ok {
			return fmt.Errorf("no persona named %q in Honcho", value)
		}
		d.Persona = value
	case "harness":
		pd, err := current()
		if err != nil {
			return err
		}
		pd.Harness = value
		d.Personas[a.cur.Name] = pd
	case "cwd":
		pd, err := current()
		if err != nil {
			return err
		}
		pd.CWD = value
		d.Personas[a.cur.Name] = pd
	case "model":
		if value != "" && !wire.ValidSetting("model", value) {
			return fmt.Errorf("%q is not a usable model name", oneLine(value))
		}
		pd, err := current()
		if err != nil {
			return err
		}
		pd.Model = value
		d.Personas[a.cur.Name] = pd
	case "effort":
		if value != "" && !wire.ValidSetting("effort", value) {
			return fmt.Errorf("%q is not a usable effort name", oneLine(value))
		}
		pd, err := current()
		if err != nil {
			return err
		}
		pd.Effort = value
		d.Personas[a.cur.Name] = pd
	default:
		return errors.New("usage: /default [save | persona NAME | harness ID | cwd DIR | model NAME | effort LEVEL]")
	}
	if err := a.saveDefaults(d); err != nil {
		return err
	}
	if said == "" {
		said = fmt.Sprintf("default %s: %s", key, show(value))
	}
	a.Out.line(said + " (saved in Honcho)")
	return nil
}

func (a *App) cloneDefaults() wire.Defaults {
	d := a.defaults
	d.V = 1
	d.Personas = map[string]wire.PersonaDefaults{}
	for name, v := range a.defaults.Personas {
		d.Personas[name] = v
	}
	if a.defaults.UI != nil {
		u := *a.defaults.UI
		if u.ShowNode != nil {
			v := *u.ShowNode
			u.ShowNode = &v
		}
		if u.ShowModel != nil {
			v := *u.ShowModel
			u.ShowModel = &v
		}
		if u.ShowEffort != nil {
			v := *u.ShowEffort
			u.ShowEffort = &v
		}
		if len(a.defaults.UI.HarnessNames) > 0 {
			u.HarnessNames = map[string]string{}
			for k, v := range a.defaults.UI.HarnessNames {
				u.HarnessNames[k] = v
			}
		}
		d.UI = &u
	}
	return d
}

func (a *App) saveDefaults(d wire.Defaults) error {
	for name, v := range d.Personas {
		if v == (wire.PersonaDefaults{}) {
			delete(d.Personas, name)
		}
	}
	if d.UI.IsZero() {
		d.UI = nil
	}
	ctx, cancel := a.call()
	defer cancel()
	stored, err := wire.SealDefaultsWith(a.humanAuth(), a.Profile.Human, d, a.Now().Unix())
	if err != nil {
		return err
	}
	if err := a.Honcho.SetPeerMetadata(ctx, a.Profile.Human, wire.DefaultsKey, stored); err != nil {
		return fmt.Errorf("the default was not saved: %w", err)
	}
	a.defaults, a.defaultsSaved = d, a.Now().Unix()
	return nil
}

// setUI shows or updates prompt-box display preferences, stored in the
// owner's signed Honcho defaults so every computer shares the same look.
func (a *App) setUI(arg string) error {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	if arg == "" {
		u := a.defaults.UI
		a.Out.line("prompt box UI (saved in Honcho)")
		a.Out.line("  node    " + onOff(u.NodeVisible()))
		a.Out.line("  model   " + onOff(u.ModelVisible()))
		a.Out.line("  effort  " + onOff(u.EffortVisible()))
		if u != nil && len(u.HarnessNames) > 0 {
			ids := make([]string, 0, len(u.HarnessNames))
			for id := range u.HarnessNames {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				a.Out.line(fmt.Sprintf("  label   %s -> %s", oneLine(id), oneLine(u.HarnessNames[id])))
			}
		} else {
			a.Out.line("  label   hermes -> hermes (default)")
		}
		return nil
	}
	key, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	d := a.cloneDefaults()
	if d.UI == nil {
		d.UI = &wire.UIDefaults{}
	}
	parseToggle := func(s string) (bool, error) {
		switch strings.ToLower(s) {
		case "on", "true", "show", "yes", "1":
			return true, nil
		case "off", "false", "hide", "no", "0":
			return false, nil
		default:
			return false, fmt.Errorf("usage: /ui %s on|off", key)
		}
	}
	said := ""
	switch key {
	case "node":
		v, err := parseToggle(rest)
		if err != nil {
			return err
		}
		d.UI.ShowNode = &v
		said = "ui node: " + onOff(v)
	case "model":
		v, err := parseToggle(rest)
		if err != nil {
			return err
		}
		d.UI.ShowModel = &v
		said = "ui model: " + onOff(v)
	case "effort":
		v, err := parseToggle(rest)
		if err != nil {
			return err
		}
		d.UI.ShowEffort = &v
		said = "ui effort: " + onOff(v)
	case "label":
		id, label, _ := strings.Cut(rest, " ")
		id, label = strings.TrimSpace(id), strings.TrimSpace(label)
		if id == "" || label == "" {
			return errors.New("usage: /ui label HARNESS_ID NAME (or default)")
		}
		if d.UI.HarnessNames == nil {
			d.UI.HarnessNames = map[string]string{}
		}
		if label == "default" || label == "none" {
			delete(d.UI.HarnessNames, id)
			said = fmt.Sprintf("ui label %s: default", oneLine(id))
		} else {
			d.UI.HarnessNames[id] = oneLine(label)
			said = fmt.Sprintf("ui label %s: %s", oneLine(id), oneLine(label))
		}
	case "reset":
		d.UI = nil
		said = "ui settings reset to defaults"
	default:
		return errors.New("usage: /ui [node|model|effort on|off | label ID NAME | reset]")
	}
	if err := a.saveDefaults(d); err != nil {
		return err
	}
	a.Out.line(said + " (saved in Honcho)")
	return nil
}
