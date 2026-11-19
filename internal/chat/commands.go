package chat

// Slash commands and turns.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hauntedfail/geminus/internal/acp"
	"github.com/hauntedfail/geminus/internal/persona"
)

const commandHelp = `geminus commands
  /persona [NAME]        pick who to talk to
  /harness [ID]          pick the agent program on that persona's computer
  /new                   start a new session
  /sessions              list the sessions there
  /resume [ID|N]         reopen a session (default: the latest)
  /status                show persona, harness, connection and session
  /default               show the defaults; /default save keeps the current
                         persona and harness; /default persona|harness|session VALUE
  /disconnect            close the connection
  /quit                  leave geminus
  Ctrl-C stops the agent's turn; at the prompt it quits.`

// ownCommands are the slash commands geminus handles itself. Any other
// slash command belongs to the agent.
var ownCommands = map[string]bool{
	"/persona": true, "/personas": true, "/harness": true, "/new": true, "/sessions": true, "/resume": true,
	"/status": true, "/default": true, "/disconnect": true, "/help": true, "/quit": true, "/exit": true,
}

func (a *App) handle(line string) error {
	if !strings.HasPrefix(line, "/") {
		return a.say(line)
	}
	// "//name" always goes to the agent, for a command of its own that
	// shares a name with one of geminus's.
	if strings.HasPrefix(line, "//") {
		return a.say(line[1:])
	}
	cmd, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	if !ownCommands[cmd] {
		if a.agent != nil && a.agentTakes(cmd[1:]) {
			return a.say(line)
		}
		return fmt.Errorf("unknown command %s (see /help)", cmd)
	}
	switch cmd {
	case "/help":
		a.showHelp()
	case "/status":
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
		return a.start(arg, "", "", false)
	case "/harness":
		if a.cur == nil {
			return errors.New("pick a persona first: /persona")
		}
		// Look again: what is installed may have changed since the persona
		// was picked.
		if err := a.usePersona(a.cur.Name); err != nil {
			return err
		}
		if arg == "" {
			picked, ok := a.choose(fmt.Sprintf("Harnesses on %s", who(*a.cur)), "harness", a.harnessOptions(), a.preferredHarness())
			if !ok {
				return nil
			}
			arg = picked
		}
		if err := a.useHarness(arg); err != nil {
			return err
		}
		return a.connect("", false)
	case "/disconnect":
		a.disconnect()
		a.Out.line(a.status())
	case "/sessions":
		return a.showSessions()
	case "/default":
		return a.defaults(arg)
	case "/new", "/resume":
		if a.cur == nil || a.har == nil {
			return errors.New("pick a persona first: /persona")
		}
		resume := cmd == "/resume"
		if !resume {
			arg = ""
		}
		if a.agent == nil {
			// Reconnecting after a drop: /resume means the session that
			// was open, not whichever is newest.
			if resume && arg == "" {
				arg = a.last
			}
			return a.connect(arg, resume)
		}
		if resume {
			return a.open(arg, openExisting)
		}
		return a.open("", openNew)
	}
	return nil
}

// agentTakes reports whether a slash command that is not geminus's should be
// sent to the agent: when the agent announced it, or announced no list at
// all (some agents have commands and never say so). A command missing from
// an announced list is a typo, and typos are not sent anywhere.
func (a *App) agentTakes(name string) bool {
	cmds := a.agent.Commands(a.session)
	for _, c := range cmds {
		if c.Name == name {
			return true
		}
	}
	return len(cmds) == 0
}

func (a *App) showHelp() {
	a.Out.line(commandHelp)
	if a.agent == nil {
		return
	}
	cmds := a.agent.Commands(a.session)
	if len(cmds) == 0 {
		return
	}
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	a.Out.line(fmt.Sprintf("%s commands (sent to the agent as typed; //NAME for one geminus also has)", a.har.Name))
	for _, c := range cmds {
		a.Out.line(fmt.Sprintf("  /%-20s %s", oneLine(c.Name), clip(oneLine(c.Description), 72)))
	}
}

// defaults shows or changes what geminus does when nothing is chosen.
func (a *App) defaults(arg string) error {
	d := a.dir.Defaults
	key, value, _ := strings.Cut(arg, " ")
	value = strings.TrimSpace(value)
	if value == "none" {
		value = ""
	} else if value == "" && key != "" && key != "save" {
		return fmt.Errorf("usage: /default %s VALUE (or none)", key)
	}
	show := func(v string) string {
		if v == "" {
			return "(none)"
		}
		return v
	}
	switch key {
	case "":
		session := d.Session
		if session == "" {
			session = persona.SessionNew
		}
		a.Out.line(fmt.Sprintf("defaults (%s)\n  persona  %s\n  harness  %s\n  session  %s", a.dir.ConfigPath, show(d.Persona), show(d.Harness), session))
		for _, p := range a.dir.Personas {
			if p.Node != nil && p.Node.Harness != "" {
				a.Out.line(fmt.Sprintf("  harness on %s  %s", oneLine(p.Name), p.Node.Harness))
			}
		}
		return nil
	case "save":
		if a.cur == nil || a.har == nil {
			return errors.New("nothing to save: pick a persona and a harness first")
		}
		// The harness is remembered for this persona; another persona's
		// computer may not have it.
		node := a.node
		node.Harness = a.har.ID
		if err := a.SaveNode(a.cur.Name, node); err != nil {
			return err
		}
		a.node = node
		for i := range a.dir.Personas {
			if a.dir.Personas[i].Name == a.cur.Name {
				a.dir.Personas[i].Node = &node
			}
		}
		d.Persona = a.cur.Name
	case "persona":
		if _, ok := a.dir.Find(value); value != "" && !ok {
			return fmt.Errorf("no persona named %q in the directory", value)
		}
		d.Persona = value
	case "harness":
		known := value == ""
		for _, h := range a.dir.Harnesses {
			known = known || h.ID == value
		}
		if !known {
			return fmt.Errorf("no harness named %q", value)
		}
		d.Harness = value
	case "session":
		if value != persona.SessionNew && value != persona.SessionContinue {
			return fmt.Errorf("session is %q or %q", persona.SessionNew, persona.SessionContinue)
		}
		d.Session = value
	default:
		return errors.New("usage: /default [save | persona NAME | harness ID | session new|continue]")
	}
	if err := a.SaveDefaults(d); err != nil {
		return err
	}
	a.dir.Defaults = d
	if key == "save" {
		a.Out.line(fmt.Sprintf("geminus now starts with %s · %s (saved in %s)", who(*a.cur), a.har.Name, a.dir.ConfigPath))
		return nil
	}
	a.Out.line(fmt.Sprintf("default %s: %s (saved in %s)", key, show(value), a.dir.ConfigPath))
	return nil
}

// reason is an error as shown to the person. When the agent itself answered
// with an error, only its own words matter, not the protocol method.
func reason(err error) string {
	var refused *acp.RPCError
	if errors.As(err, &refused) {
		return refused.Message
	}
	return err.Error()
}

// say sends a line to the agent, or explains why it cannot.
func (a *App) say(text string) error {
	if a.agent == nil {
		return errors.New("not connected, so nothing was sent. /persona picks who to talk to; /resume reconnects")
	}
	return a.turn(text)
}

// turn sends one message and shows the agent's reply as it arrives.
func (a *App) turn(text string) error {
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-a.interrupt:
			cancel()
		case <-finished:
		}
	}()

	view := newTurnView(a.Out, a.cur.Name)
	h := &acp.Handler{
		Text:    view.text,
		Thought: view.thought,
		Tool:    view.tool,
		Permission: func(req acp.PermissionRequest) string {
			return view.permission(req, a.lines, ctx.Done(), a.entered)
		},
	}
	stop, err := a.agent.Prompt(ctx, a.session, text, h)
	view.end()
	switch {
	case errors.Is(err, acp.ErrClosed):
		a.dropped(err)
		return errors.New("the message may not have been delivered; it was not sent again")
	case errors.Is(err, context.Canceled):
		a.Out.line(a.Out.dim("(stopped; the agent did not confirm)"))
	case err != nil:
		// The agent answered with an error of its own (no credit, a model
		// that is not available, ...). The connection is still up.
		return fmt.Errorf("%s did not take the message: %s", a.har.Name, reason(err))
	case stop == "cancelled":
		a.Out.line(a.Out.dim("(stopped)"))
	case stop != "" && stop != "end_turn":
		a.Out.line(a.Out.dim("(turn ended: " + stop + ")"))
	}
	return nil
}
