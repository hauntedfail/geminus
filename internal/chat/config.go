package chat

// Interactive /config menu modeled on Claude Code / harness settings panels.
// Users toggle booleans inline or open nested pickers for defaults; every
// change persists to Honcho immediately and the menu stays open until Esc or
// Done so multiple settings can be adjusted in one pass.

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hauntedfail/geminus/internal/wire"
)

func (a *App) slashConfig(arg string) error {
	if arg != "" {
		// Support direct single-key updates as a convenience, while keeping
		// bare `/config` as the primary interactive entry point.
		key, rest, _ := strings.Cut(arg, " ")
		key, rest = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(rest)
		switch key {
		case "auto_update", "autoupdate":
			return a.slashUpdateCmd("auto " + rest)
		case "post_human", "post-human", "posthuman", "helix":
			return a.slashPostHuman(rest)
		case "node", "model", "effort", "label", "reset":
			if rest == "" && key != "reset" {
				return fmt.Errorf("usage: /config (interactive) or /ui %s on|off", key)
			}
			return a.setUI(arg)
		case "mode":
			return a.setDefaults(arg)
		case "persona", "harness", "cwd", "save":
			return a.setDefaults(arg)
		default:
			return errors.New("usage: /config (opens interactive settings menu)")
		}
	}

	if a.live == nil {
		a.printConfigSummary()
		return nil
	}

	return a.runConfigMenu()
}

func (a *App) printConfigSummary() {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	show := func(v string) string {
		if v == "" {
			return "(none)"
		}
		return oneLine(v)
	}
	u := a.defaults.UI
	a.Out.line("geminus config (saved in Honcho)")
	a.Out.line(fmt.Sprintf("  auto_update      %s (%s)", onOff(a.autoUpdateEnabled()), a.autoUpdateSource()))
	a.Out.line("  post_human       " + onOff(a.defaults.PostHumanEnabled()))
	a.Out.line("  ui.node          " + onOff(u.NodeVisible()))
	a.Out.line("  ui.model         " + onOff(u.ModelVisible()))
	a.Out.line("  ui.effort        " + onOff(u.EffortVisible()))
	a.Out.line("  ui.mode          " + onOff(u.ModeVisible()))
	a.Out.line("  default.persona  " + show(a.defaults.Persona))
	if a.cur != nil {
		pd := a.defaults.Personas[a.cur.Name]
		a.Out.line(fmt.Sprintf("  default.harness  %s (on %s)", show(pd.Harness), oneLine(a.cur.Name)))
		a.Out.line(fmt.Sprintf("  default.model    %s (on %s)", show(pd.Model), oneLine(a.cur.Name)))
		a.Out.line(fmt.Sprintf("  default.effort   %s (on %s)", show(pd.Effort), oneLine(a.cur.Name)))
		a.Out.line(fmt.Sprintf("  default.mode     %s (on %s)", show(pd.Mode), oneLine(a.cur.Name)))
		a.Out.line(fmt.Sprintf("  default.cwd      %s (on %s)", show(pd.CWD), oneLine(a.cur.Name)))
	}
}

func (a *App) runConfigMenu() error {
	status := "Enter toggles or edits · changes save to Honcho · Esc closes"
	cursorKey := "auto_update"
	changed := 0

	for {
		cfg := menuConfig{
			title:             "Settings",
			subtitle:          status,
			searchPlaceholder: "Type to filter settings…",
			initialValue:      cursorKey,
			items:             a.configMenuItems(),
		}
		picked, ok := a.live.selectOption(a.ctx, cfg)
		if !ok || picked == "done" {
			if changed == 0 {
				a.Out.line(a.Out.dim("config closed (no changes)"))
			} else {
				a.Out.line(a.Out.dim(fmt.Sprintf("config closed (%d updated, saved in Honcho)", changed)))
			}
			return nil
		}
		cursorKey = picked

		note, updated, err := a.applyConfigChoice(picked)
		if err != nil {
			status = "✗ " + oneLine(err.Error())
			continue
		}
		if updated {
			changed++
			a.sync()
		}
		if note != "" {
			status = note
		}
		if a.restarting {
			return nil
		}
	}
}

func (a *App) configMenuItems() []menuItem {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	show := func(v string) string {
		if v == "" {
			return "(none)"
		}
		return oneLine(v)
	}

	u := a.defaults.UI
	autoOn := a.autoUpdateEnabled()
	postHumanOn := a.defaults.PostHumanEnabled()
	items := []menuItem{
		{
			value:   "auto_update",
			label:   "Auto-update",
			desc:    fmt.Sprintf("%s (%s) · install & relaunch on new release", onOff(autoOn), a.autoUpdateSource()),
			current: autoOn,
		},
		{
			value:   "ui.node",
			label:   "Show node badge",
			desc:    fmt.Sprintf("%s · persona badge on prompt box border", onOff(u.NodeVisible())),
			current: u.NodeVisible(),
		},
		{
			value:   "ui.model",
			label:   "Show model badge",
			desc:    fmt.Sprintf("%s · model indicator in prompt box header", onOff(u.ModelVisible())),
			current: u.ModelVisible(),
		},
		{
			value:   "ui.effort",
			label:   "Show effort badge",
			desc:    fmt.Sprintf("%s · effort indicator in prompt box header", onOff(u.EffortVisible())),
			current: u.EffortVisible(),
		},
		{
			value:   "post_human",
			label:   "Post-Human mode",
			desc:    fmt.Sprintf("%s · Helix modal editing (Normal/Insert/Select)", onOff(postHumanOn)),
			current: postHumanOn,
		},
		{
			value: "default.persona",
			label: "Default persona",
			desc:  fmt.Sprintf("%s · preselected on startup", show(a.defaults.Persona)),
		},
	}

	if a.cur != nil {
		pd := a.defaults.Personas[a.cur.Name]
		who := oneLine(a.cur.Name)
		items = append(items,
			menuItem{value: "default.mode", label: "Default permission mode", desc: fmt.Sprintf("%s · on %s", show(pd.Mode), who)},
			menuItem{
				value: "default.harness",
				label: "Default harness",
				desc:  fmt.Sprintf("%s · on %s", show(pd.Harness), who),
			},
			menuItem{
				value: "default.model",
				label: "Default model",
				desc:  fmt.Sprintf("%s · on %s", show(pd.Model), who),
			},
			menuItem{
				value: "default.effort",
				label: "Default effort",
				desc:  fmt.Sprintf("%s · on %s", show(pd.Effort), who),
			},
			menuItem{
				value: "default.cwd",
				label: "Default directory",
				desc:  fmt.Sprintf("%s · on %s", show(pd.CWD), who),
			},
		)
	}

	items = append(items,
		menuItem{value: "ui.mode", label: "Show permission mode badge", desc: onOff(u.ModeVisible()), current: u.ModeVisible()},
		menuItem{
			value: "ui.reset",
			label: "Reset UI settings",
			desc:  "Restore default prompt box layout",
		},
		menuItem{
			value: "done",
			label: "Done",
			desc:  "Close settings menu",
		},
	)
	return items
}

func (a *App) applyConfigChoice(choice string) (note string, updated bool, err error) {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}

	switch choice {
	case "auto_update":
		d := a.cloneDefaults()
		next := !a.autoUpdateEnabled()
		d.AutoUpdate = &next
		if err := a.saveDefaults(d); err != nil {
			return "", false, err
		}
		if next {
			a.checkAndAutoUpdateNow()
		}
		return fmt.Sprintf("Saved: auto_update = %s (Honcho)", onOff(next)), true, nil

	case "post_human":
		d := a.cloneDefaults()
		next := !a.defaults.PostHumanEnabled()
		d.PostHuman = &next
		if err := a.saveDefaults(d); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Saved: post_human = %s (Honcho)", onOff(next)), true, nil

	case "ui.node":
		d := a.cloneDefaults()
		if d.UI == nil {
			d.UI = &wire.UIDefaults{}
		}
		next := !a.defaults.UI.NodeVisible()
		d.UI.ShowNode = &next
		if err := a.saveDefaults(d); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Saved: ui.node = %s (Honcho)", onOff(next)), true, nil

	case "ui.model":
		d := a.cloneDefaults()
		if d.UI == nil {
			d.UI = &wire.UIDefaults{}
		}
		next := !a.defaults.UI.ModelVisible()
		d.UI.ShowModel = &next
		if err := a.saveDefaults(d); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Saved: ui.model = %s (Honcho)", onOff(next)), true, nil

	case "ui.effort":
		d := a.cloneDefaults()
		if d.UI == nil {
			d.UI = &wire.UIDefaults{}
		}
		next := !a.defaults.UI.EffortVisible()
		d.UI.ShowEffort = &next
		if err := a.saveDefaults(d); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("Saved: ui.effort = %s (Honcho)", onOff(next)), true, nil

	case "ui.reset":
		d := a.cloneDefaults()
		d.UI = nil
		if err := a.saveDefaults(d); err != nil {
			return "", false, err
		}
		return "Saved: UI settings reset to defaults (Honcho)", true, nil

	case "default.persona":
		return a.configPickDefaultPersona()

	case "default.harness":
		return a.configPickDefaultHarness()

	case "default.model":
		return a.configPickDefaultModel()

	case "default.effort":
		return a.configPickDefaultEffort()

	case "ui.mode":
		if err := a.setUI("mode " + onOff(!a.defaults.UI.ModeVisible())); err != nil {
			return "", false, err
		}
		return "Saved: ui.mode (Honcho)", true, nil
	case "default.mode":
		if a.cur == nil || a.harness == nil || !a.harness.Takes("mode") {
			return "", false, errors.New("host harness does not advertise mode support")
		}
		pd := a.defaults.Personas[a.cur.Name]
		picked, ok := a.live.selectOption(a.ctx, menuConfig{title: "Default permission mode", subtitle: "Saved per persona; host-native rules still apply",
			items: modeMenuItems(pd.Mode, pd.Mode, a.harness.Name, a.harness.DefaultFor("mode"))})
		if !ok {
			return "Mode unchanged", false, nil
		}
		value := picked
		if value == "" {
			value = "none"
		}
		if err := a.setDefaults("mode " + value); err != nil {
			return "", false, err
		}
		return "Saved: default.mode (Honcho)", true, nil
	case "default.cwd":
		return a.configPickDefaultCWD()

	default:
		return "", false, nil
	}
}

func (a *App) configPickDefaultPersona() (string, bool, error) {
	items := []menuItem{
		{
			value:   "",
			label:   "(none)",
			desc:    "Ask on startup",
			current: a.defaults.Persona == "",
		},
	}
	peers := make([]Persona, len(a.personas))
	copy(peers, a.personas)
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	for _, p := range peers {
		desc := p.Tag()
		if len(p.Harnesses) > 0 {
			desc = fmt.Sprintf("%s · %d harnesses", desc, len(p.Harnesses))
		}
		items = append(items, menuItem{
			value:   p.Name,
			label:   p.Name,
			desc:    desc,
			current: a.defaults.Persona == p.Name,
		})
	}
	picked, ok := a.live.selectOption(a.ctx, menuConfig{
		title:             "Default persona",
		subtitle:          "Persona preselected when geminus starts",
		searchPlaceholder: "Type to search personas…",
		items:             items,
	})
	if !ok {
		return "Default persona unchanged", false, nil
	}
	d := a.cloneDefaults()
	d.Persona = picked
	if err := a.saveDefaults(d); err != nil {
		return "", false, err
	}
	shown := picked
	if shown == "" {
		shown = "(none)"
	}
	return fmt.Sprintf("Saved: default.persona = %s (Honcho)", oneLine(shown)), true, nil
}

func (a *App) configPickDefaultHarness() (string, bool, error) {
	if a.cur == nil {
		return "", false, errors.New("pick a persona first (/persona)")
	}
	curSaved := a.defaults.Personas[a.cur.Name].Harness
	items := []menuItem{
		{
			value:   "",
			label:   "(none)",
			desc:    "Use first available harness",
			current: curSaved == "",
		},
	}
	for _, h := range a.cur.Harnesses {
		items = append(items, menuItem{
			value:   h.ID,
			label:   h.ID,
			desc:    oneLine(h.Name),
			current: curSaved == h.ID,
		})
	}
	picked, ok := a.live.selectOption(a.ctx, menuConfig{
		title:             "Default harness",
		subtitle:          "Harness preselected on " + oneLine(a.cur.Name),
		searchPlaceholder: "Type to search harnesses…",
		items:             items,
	})
	if !ok {
		return "Default harness unchanged", false, nil
	}
	d := a.cloneDefaults()
	pd := d.Personas[a.cur.Name]
	pd.Harness = picked
	d.Personas[a.cur.Name] = pd
	if err := a.saveDefaults(d); err != nil {
		return "", false, err
	}
	shown := picked
	if shown == "" {
		shown = "(none)"
	}
	return fmt.Sprintf("Saved: default.harness = %s on %s (Honcho)", oneLine(shown), oneLine(a.cur.Name)), true, nil
}

func (a *App) configPickDefaultModel() (string, bool, error) {
	if a.cur == nil || a.harness == nil {
		return "", false, errors.New("pick a persona and harness first")
	}
	curSaved := a.defaults.Personas[a.cur.Name].Model
	disp := a.harnessDisplay(*a.harness)
	picked, ok := a.live.selectOption(a.ctx, menuConfig{
		title:             "Default model",
		subtitle:          fmt.Sprintf("Saved default model on %s · %s", oneLine(a.cur.Name), disp),
		searchPlaceholder: "Type to search models…",
		customTitle:       "Custom default model",
		customSubtitle:    fmt.Sprintf("Enter any model name for %s (or \"default\" to reset)", disp),
		customPlaceholder: "e.g. group/lvmh, opus, sonnet",
		customEmptyErr:    "enter a model name, or press Esc to go back",
		items:             a.modelMenuItemsFor(curSaved),
		validate: func(v string) error {
			if !wire.ValidSetting("model", v) {
				return fmt.Errorf("%q is not a usable model name (letters, digits and . _ - : / @ # + [ ])", oneLine(v))
			}
			return nil
		},
	})
	if !ok {
		return "Default model unchanged", false, nil
	}
	d := a.cloneDefaults()
	pd := d.Personas[a.cur.Name]
	pd.Model = picked
	d.Personas[a.cur.Name] = pd
	if err := a.saveDefaults(d); err != nil {
		return "", false, err
	}
	shown := picked
	if shown == "" {
		shown = "default"
	}
	return fmt.Sprintf("Saved: default.model = %s on %s (Honcho)", oneLine(shown), oneLine(a.cur.Name)), true, nil
}

func (a *App) configPickDefaultEffort() (string, bool, error) {
	if a.cur == nil {
		return "", false, errors.New("pick a persona first (/persona)")
	}
	curSaved := a.defaults.Personas[a.cur.Name].Effort
	harnessName := ""
	if a.harness != nil {
		harnessName = a.harness.Name
	}
	picked, ok := a.live.selectOption(a.ctx, menuConfig{
		title:             "Default effort",
		subtitle:          "Saved default effort on " + oneLine(a.cur.Name),
		searchPlaceholder: "Type to search effort levels…",
		customTitle:       "Custom default effort",
		customSubtitle:    "Enter any effort level (or \"default\" to reset)",
		customPlaceholder: "e.g. low, medium, high, max",
		customEmptyErr:    "enter an effort level, or press Esc to go back",
		items:             effortMenuItems(curSaved, curSaved, harnessName),
		validate: func(v string) error {
			if !wire.ValidSetting("effort", v) {
				return fmt.Errorf("%q is not a usable effort level (letters, digits and . _ -)", oneLine(v))
			}
			return nil
		},
	})
	if !ok {
		return "Default effort unchanged", false, nil
	}
	d := a.cloneDefaults()
	pd := d.Personas[a.cur.Name]
	pd.Effort = picked
	d.Personas[a.cur.Name] = pd
	if err := a.saveDefaults(d); err != nil {
		return "", false, err
	}
	shown := picked
	if shown == "" {
		shown = "default"
	}
	return fmt.Sprintf("Saved: default.effort = %s on %s (Honcho)", oneLine(shown), oneLine(a.cur.Name)), true, nil
}

func (a *App) configPickDefaultCWD() (string, bool, error) {
	if a.cur == nil {
		return "", false, errors.New("pick a persona first (/persona)")
	}
	curSaved := a.defaults.Personas[a.cur.Name].CWD
	items := []menuItem{
		{
			value:   "",
			label:   "(none)",
			desc:    "Use remote persona default directory",
			current: curSaved == "",
		},
	}
	if a.cwd != "" && a.cwd != curSaved {
		items = append(items, menuItem{
			value: a.cwd,
			label: a.cwd,
			desc:  "Current working directory",
		})
	}
	if curSaved != "" {
		items = append(items, menuItem{
			value:   curSaved,
			label:   curSaved,
			desc:    "Saved default directory",
			current: true,
		})
	}
	items = append(items, menuItem{
		label:  "Custom directory…",
		desc:   "Enter a directory path on " + oneLine(a.cur.Name),
		custom: true,
	})
	picked, ok := a.live.selectOption(a.ctx, menuConfig{
		title:             "Default directory",
		subtitle:          "Working directory preselected on " + oneLine(a.cur.Name),
		searchPlaceholder: "Type to search directories…",
		customTitle:       "Custom default directory",
		customSubtitle:    "Enter directory path on " + oneLine(a.cur.Name) + " (or \"none\" to clear)",
		customPlaceholder: "a usable directory path on this persona's computer",
		customEmptyErr:    "enter a directory path, or press Esc to go back",
		items:             items,
		validate:          validDir,
	})
	if !ok {
		return "Default directory unchanged", false, nil
	}
	d := a.cloneDefaults()
	pd := d.Personas[a.cur.Name]
	pd.CWD = picked
	d.Personas[a.cur.Name] = pd
	if err := a.saveDefaults(d); err != nil {
		return "", false, err
	}
	shown := picked
	if shown == "" {
		shown = "(none)"
	}
	return fmt.Sprintf("Saved: default.cwd = %s on %s (Honcho)", oneLine(shown), oneLine(a.cur.Name)), true, nil
}
