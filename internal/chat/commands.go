package chat

// Slash commands.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/wire"
)

func (a *App) handle(line string) error {
	if a.expirePendingAsk() {
		return errors.New("permission question expired; no answer sent")
	}
	if a.pendingAsk != nil {
		if choice, ok := parseAskAnswerFor(line, a.pendingAsk.Choices); ok {
			return a.sendAnswer(choice)
		}
		if !isGeminusCommand(line) {
			return fmt.Errorf("permission question pending: choose %s, a number, or /ask", strings.Join(askChoices(a.pendingAsk), " / "))
		}
	}
	if cmd, arg, ok := shellLine(line); ok {
		return a.shell(cmd, arg)
	}
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
		a.notifyResetTurnState()
		a.notifyBoundary()
		a.printTransitionStatus("Ready")
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
		a.notifyResetTurnState()
		a.notifyBoundary()
		a.printTransitionStatus("Ready")
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
		a.resetAskState()
		a.notifyResetTurnState()
		if a.live != nil {
			a.live.setPrompt("")
		}
		if arg != "" {
			a.cwd = arg
		} else {
			a.cwd = a.preferredCWD()
		}
		a.notifyBoundary()
		a.printTransitionStatus("New session")
	case "/cwd":
		return a.chdir(arg)
	case "/sessions":
		return a.showSessions()
	case "/resume":
		if err := a.resume(arg); err != nil {
			return err
		}
		a.notifySyncState()
		a.printTransitionStatus("Resumed session")
	case "/default":
		return a.setDefaults(arg)
	case "/config":
		return a.slashConfig(arg)
	case "/post-human", "/posthuman", "/helix":
		return a.slashPostHuman(arg)
	case "/clawd":
		return a.slashClawd(arg)
	case "/notify":
		return a.slashNotify(arg)
	case "/ui":
		return a.setUI(arg)
	case "/update":
		return a.slashUpdateCmd(arg)
	case "/model", "/effort", "/mode":
		return a.tune(cmd[1:], arg)
	case "/ask":
		if a.pendingAsk == nil {
			return errors.New("no permission question is waiting for an answer")
		}
		a.showBridgeAsk(a.pendingAsk.Question, wire.Message{Tool: a.pendingAsk.Tool, Detail: a.pendingAsk.Detail, Choices: a.pendingAsk.Choices})
		a.pendingAsk.Prompted = false
		a.promptPendingAsk()
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
	} else if kind == "mode" {
		current = &a.tuning.Mode
	}
	show := func() {
		if *current == "" {
			resolved := a.harness.DefaultFor(kind)
			if resolved == "" {
				resolved = "unknown; host has not reported a value"
			}
			a.Out.line(fmt.Sprintf("%s: %s's default (%s)  (set one for this conversation with /%s NAME)", kind, oneLine(a.harness.Name), oneLine(resolved), kind))
			return
		}
		a.Out.line(fmt.Sprintf("%s: %s  (this conversation only; /%s default goes back to %s's own)", kind, oneLine(*current), kind, oneLine(a.harness.Name)))
	}
	if value == "" {
		if a.live != nil {
			if len(a.harness.Options) == 0 {
				return fmt.Errorf("the bridge on %s's computer is too old to pass a %s: geminish 0.6 or later must be running there. Prompts without one still work", a.cur.Name, kind)
			}
			if !a.harness.Takes(kind) {
				return fmt.Errorf("%s takes no %s setting from geminus on %s's computer", oneLine(a.harness.Name), kind, a.cur.Name)
			}
			disp := a.harnessDisplay(*a.harness)
			var cfg menuConfig
			if kind == "mode" {
				cfg = menuConfig{title: "Permission mode", subtitle: "This conversation only; host-native rules still apply",
					items: modeMenuItems(a.tuning.Mode, a.defaults.Personas[a.cur.Name].Mode, a.harness.Name, a.harness.DefaultFor("mode"))}
			} else if kind == "model" {
				cfg = menuConfig{
					title:             "Select model",
					subtitle:          fmt.Sprintf("Model for this conversation · %s", disp),
					searchPlaceholder: "Type to search models…",
					customTitle:       "Custom model",
					customSubtitle:    fmt.Sprintf("Enter any model name for %s (or \"default\" to reset)", disp),
					customPlaceholder: "e.g. group/lvmh, opus, sonnet",
					customEmptyErr:    "enter a model name, or press Esc to go back",
					items:             a.modelMenuItems(),
					validate: func(v string) error {
						if !wire.ValidSetting("model", v) {
							return fmt.Errorf("%q is not a usable model name (letters, digits and . _ - : / @ # + [ ])", oneLine(v))
						}
						return nil
					},
				}
			} else {
				saved := ""
				if a.cur != nil {
					saved = a.defaults.Personas[a.cur.Name].Effort
				}
				cfg = menuConfig{
					title:             "Select effort",
					subtitle:          fmt.Sprintf("Reasoning effort for this conversation · %s", disp),
					searchPlaceholder: "Type to search effort levels…",
					customTitle:       "Custom effort",
					customSubtitle:    fmt.Sprintf("Enter any effort level for %s (or \"default\" to reset)", disp),
					customPlaceholder: "e.g. low, medium, high, max",
					customEmptyErr:    "enter an effort level, or press Esc to go back",
					items:             effortMenuItemsWithDefault(a.tuning.Effort, saved, a.harness.Name, a.harness.DefaultFor("effort")),
					validate: func(v string) error {
						if !wire.ValidSetting("effort", v) {
							return fmt.Errorf("%q is not a usable effort level (letters, digits and . _ -)", oneLine(v))
						}
						return nil
					},
				}
			}
			picked, ok := a.live.selectOption(a.ctx, cfg)
			if !ok {
				a.Out.line(a.Out.dim(kind + " unchanged"))
				return nil
			}
			value = picked
		} else {
			show()
			return nil
		}
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
	if kind == "model" && value != "" {
		a.rememberModel(a.harness.ID, value)
	}
	show()
	return nil
}

// rememberModel keeps up to 8 recently selected models per harness for the
// /model select menu.
func (a *App) rememberModel(harnessID, model string) {
	if harnessID == "" || model == "" {
		return
	}
	if a.recentModels == nil {
		a.recentModels = map[string][]string{}
	}
	prev := a.recentModels[harnessID]
	next := make([]string, 0, min(len(prev)+1, 8))
	next = append(next, model)
	for _, m := range prev {
		if m != model && len(next) < 8 {
			next = append(next, m)
		}
	}
	a.recentModels[harnessID] = next
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
	if a.harness != nil {
		current := wire.Harness{ID: a.harness.ID, Name: a.harness.Name}
		for _, h := range p.Harnesses {
			if h.ID == current.ID {
				current = h
				break
			}
		}
		a.harness = &current
	}
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
		autoStatus := "off"
		if a.autoUpdateEnabled() {
			autoStatus = "on"
		}
		if src := a.autoUpdateSource(); src != "" {
			autoStatus += " (" + src + ")"
		}
		a.Out.line("  auto_update  " + autoStatus)
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
			if d.Mode != "" {
				line += "  mode " + oneLine(d.Mode)
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
			Mode:    a.tuning.Mode,
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
	case "mode":
		if !wire.ValidSetting("mode", value) {
			return fmt.Errorf("unknown permission mode %q", value)
		}
		if value != "" && (a.harness == nil || !a.harness.Takes("mode")) {
			return errors.New("host harness does not advertise mode support")
		}
		pd, err := current()
		if err != nil {
			return err
		}
		pd.Mode = value
		d.Personas[a.cur.Name] = pd
	case "auto_update", "autoupdate":
		if value == "" {
			d.AutoUpdate = nil
			said = "default auto_update: (none)"
		} else {
			on, err := parseBoolToggle(value)
			if err != nil {
				return errors.New("usage: /default auto_update on|off (or none)")
			}
			d.AutoUpdate = &on
			if on {
				said = "default auto_update: on"
			} else {
				said = "default auto_update: off"
			}
		}
	default:
		return errors.New("usage: /default [save | persona NAME | harness ID | cwd DIR | model NAME | effort LEVEL | mode MODE | auto_update on|off]")
	}
	if err := a.saveDefaults(d); err != nil {
		return err
	}
	if said == "" {
		said = fmt.Sprintf("default %s: %s", key, show(value))
	}
	a.Out.line(said + " (saved in Honcho)")
	if (key == "auto_update" || key == "autoupdate") && a.autoUpdateEnabled() {
		a.checkAndAutoUpdateNow()
	}
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
		if u.ShowMode != nil {
			v := *u.ShowMode
			u.ShowMode = &v
		}
		if len(a.defaults.UI.HarnessNames) > 0 {
			u.HarnessNames = map[string]string{}
			for k, v := range a.defaults.UI.HarnessNames {
				u.HarnessNames[k] = v
			}
		}
		d.UI = &u
	}
	if a.defaults.AutoUpdate != nil {
		v := *a.defaults.AutoUpdate
		d.AutoUpdate = &v
	}
	if a.defaults.PostHuman != nil {
		v := *a.defaults.PostHuman
		d.PostHuman = &v
	}
	if a.defaults.Clawd != nil {
		v := *a.defaults.Clawd
		d.Clawd = &v
	}
	return d
}

// slashPostHuman toggles or sets Post-Human (Helix) modal editing, saved in
// the owner's signed Honcho defaults.
func (a *App) slashPostHuman(arg string) error {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	var next bool
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "":
		next = !a.defaults.PostHumanEnabled()
	case "on", "true", "yes", "1":
		next = true
	case "off", "false", "no", "0":
		next = false
	case "status":
		a.Out.line("post-human mode: " + onOff(a.defaults.PostHumanEnabled()) + " (Helix modal editing)")
		return nil
	default:
		return errors.New("usage: /post-human [on|off|status]")
	}
	d := a.cloneDefaults()
	d.PostHuman = &next
	if err := a.saveDefaults(d); err != nil {
		return err
	}
	a.sync()
	if next {
		a.Out.line("Post-Human mode: on (Helix modal editing · Esc normal, i/a insert, v select · saved in Honcho)")
	} else {
		a.Out.line("Post-Human mode: off (saved in Honcho)")
	}
	return nil
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
	peers, err := a.Honcho.Peers(ctx)
	if err != nil {
		return fmt.Errorf("default write not confirmed: %w", err)
	}
	confirmed := false
	want, _ := json.Marshal(d)
	for _, peer := range peers {
		if peer.ID != a.Profile.Human {
			continue
		}
		got, _, ok := wire.OpenDefaultsWith(a.humanAuth(), a.Profile.Human, peer.Metadata[wire.DefaultsKey])
		encoded, _ := json.Marshal(got)
		confirmed = ok && string(encoded) == string(want)
	}
	if !confirmed {
		return errors.New("default write not confirmed by Honcho readback")
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
		a.Out.line("  mode    " + onOff(u.ModeVisible()))
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
	case "mode":
		v, err := parseToggle(rest)
		if err != nil {
			return err
		}
		d.UI.ShowMode = &v
		said = "ui mode: " + onOff(v)
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
