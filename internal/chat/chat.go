// Package chat is geminus's terminal interface: pick a persona, connect to
// its agent, and talk. The persona and the connection state are always on
// screen, in the prompt and in the status line.
package chat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/acp"
	"github.com/hauntedfail/geminus/internal/persona"
	"github.com/hauntedfail/geminus/internal/version"
)

const usage = `geminus talks to your agents on other computers.

Usage:
  geminus                          list personas, pick one, and talk
  geminus persona                  list personas and exit
  geminus persona NAME             connect to NAME and start a new session
  geminus persona NAME --continue  connect and reopen NAME's latest session
  geminus persona NAME --resume ID connect and reopen a session by ID
  geminus version | help

Personas are the agents of your Honcho workspace. How to reach each one is
set in ~/.geminus/config.json:

  {"personas": {"sibyl": {"ssh": "btcvps", "runtime": "hermes", "cwd": "/home/hermes"}}}
`

const commandHelp = `  /persona [NAME]   list personas, or switch to NAME
  /new              start a new session with the current persona
  /sessions         list the persona's sessions
  /resume [ID|N]    reopen a session (default: the latest)
  /status           show who you are connected to
  /disconnect       close the connection
  /quit             leave geminus
  Ctrl-C stops the agent's turn; at the prompt it quits.`

// connectTimeout bounds the handshake, including ssh and agent startup.
const connectTimeout = 90 * time.Second

// Agent is the part of an ACP connection the interface uses.
type Agent interface {
	Initialize(ctx context.Context, clientVersion string) (acp.Info, error)
	NewSession(ctx context.Context, cwd string) (string, error)
	LoadSession(ctx context.Context, cwd, sessionID string, h *acp.Handler) error
	ListSessions(ctx context.Context, cwd string) ([]acp.SessionInfo, error)
	Prompt(ctx context.Context, sessionID, text string, h *acp.Handler) (string, error)
	Done() <-chan struct{}
	Err() error
	Close() error
}

// App is one run of the interface. The zero value is not usable; see Run.
type App struct {
	Out *Printer
	// Load reads the persona directory.
	Load func(context.Context) (persona.Directory, error)
	// Dial starts the connection for a route.
	Dial func(persona.Route) (Agent, error)

	ctx   context.Context
	lines <-chan string
	// echo repeats each input line on the screen. A terminal already shows
	// what is typed; piped input would otherwise be invisible.
	echo bool
	// interrupt delivers Ctrl-C.
	interrupt <-chan os.Signal

	dir   persona.Directory
	cur   *persona.Persona
	agent Agent
	info  acp.Info
	cwd   string
	// session is the open session; last is the one to offer after a drop.
	session, last string
}

// Run is the geminus command. It returns the process exit code.
func Run(ctx context.Context, args []string, in io.Reader, out, errw io.Writer) int {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	app := &App{
		Out:  newPrinter(out, isTerminal(out) && os.Getenv("NO_COLOR") == ""),
		Load: persona.Load,
		Dial: func(route persona.Route) (Agent, error) {
			cmd, err := route.Exec()
			if err != nil {
				return nil, err
			}
			return acp.Start(cmd)
		},
		ctx:       ctx,
		interrupt: interrupt,
	}
	return app.Main(args, in, errw)
}

// Main runs the command line in args against the input in.
func (a *App) Main(args []string, in io.Reader, errw io.Writer) int {
	name, resume, latest, list := "", "", false, false
	switch {
	case len(args) == 0:
	case len(args) == 1 && (args[0] == "version" || args[0] == "--version" || args[0] == "-v"):
		a.Out.line(version.String())
		return 0
	case len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h"):
		a.Out.write(usage)
		return 0
	case args[0] == "persona" && len(args) == 1:
		list = true
	case args[0] == "persona" && len(args) == 2:
		name = args[1]
	case args[0] == "persona" && len(args) == 3 && (args[2] == "--continue" || args[2] == "-c"):
		name, latest = args[1], true
	case args[0] == "persona" && len(args) == 4 && args[2] == "--resume":
		name, resume = args[1], args[3]
	default:
		fmt.Fprint(errw, usage)
		return 2
	}
	if strings.HasPrefix(name, "-") {
		fmt.Fprint(errw, usage)
		return 2
	}

	if err := a.loadDirectory(); err != nil {
		fmt.Fprintln(errw, "geminus:", sanitize(err.Error()))
		return 1
	}
	if list {
		a.showPersonas()
		return 0
	}
	defer a.disconnect()
	a.lines = readLines(in)
	a.echo = !isTerminal(in)

	if name == "" {
		a.showPersonas()
		a.Out.line(a.Out.dim("Type a persona's name or number to connect. /help lists commands."))
	} else if err := a.connect(name, resume, latest); err != nil {
		// Asked for one persona on the command line: either that
		// conversation opens or the command fails.
		fmt.Fprintln(errw, "geminus:", sanitize(err.Error()))
		return 1
	}
	a.loop()
	return 0
}

func readLines(in io.Reader) <-chan string {
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

func (a *App) loadDirectory() error {
	dir, err := a.Load(a.ctx)
	if err != nil {
		return err
	}
	a.dir = dir
	return nil
}

func (a *App) showPersonas() {
	title := "Personas"
	if a.dir.Workspace != "" {
		title += " (Honcho workspace " + oneLine(a.dir.Workspace) + ")"
	}
	a.Out.line(a.Out.bold(title))
	if a.dir.Warning != "" {
		a.Out.line(a.Out.warn("! " + oneLine(a.dir.Warning)))
	}
	if len(a.dir.Personas) == 0 {
		a.Out.line("  none found")
		return
	}
	width := 0
	for _, p := range a.dir.Personas {
		width = max(width, len(oneLine(p.Name)))
	}
	for i, p := range a.dir.Personas {
		name := oneLine(p.Name)
		how := "no route: add \"" + name + "\" to " + a.dir.ConfigPath
		if p.Route != nil {
			how = p.Route.Describe()
		}
		mark := " "
		if a.cur != nil && a.cur.Name == p.Name && a.agent != nil {
			mark = "●"
		}
		a.Out.line(fmt.Sprintf("%s %d  %-*s  %-13s  %s", mark, i+1, width, name, p.Tag(), how))
	}
}

// who is the persona as shown in the prompt and status line. The local
// persona always carries its tag: talking to this computer's own agent
// through geminus is possible, but it should never be mistaken for a remote.
func who(p persona.Persona) string {
	return oneLine(p.Name) + " [" + p.Tag() + "]"
}

// short abbreviates a session ID for display. /resume accepts any unique
// prefix, so the short form is enough to name a session.
func short(id string) string {
	const keep = 14
	id = oneLine(id)
	if len(id) > keep+2 {
		return id[:keep] + "…"
	}
	return id
}

// when trims an ISO timestamp to minutes.
func when(stamp string) string {
	stamp = oneLine(stamp)
	if len(stamp) >= 16 && stamp[10] == 'T' {
		return stamp[:10] + " " + stamp[11:16]
	}
	return stamp
}

func (a *App) status() string {
	if a.agent == nil || a.cur == nil {
		s := "○ disconnected"
		if a.cur != nil && a.last != "" {
			s += fmt.Sprintf(" (last: %s, session %s; /resume reconnects)", who(*a.cur), short(a.last))
		}
		return s
	}
	agent := oneLine(a.info.Name + " " + a.info.Version)
	route := "local"
	if a.cur.Route.SSH != "" {
		route = "via ssh " + a.cur.Route.SSH
	}
	return fmt.Sprintf("● connected  %s  %s %s  session %s  cwd %s", who(*a.cur), agent, route, short(a.session), oneLine(a.cwd))
}

func (a *App) prompt() string {
	if a.agent == nil || a.cur == nil {
		return a.Out.dim("geminus (disconnected)") + " ❯ "
	}
	return a.Out.bold(who(*a.cur)) + " ❯ "
}

func (a *App) disconnect() {
	if a.agent != nil {
		_ = a.agent.Close()
	}
	a.agent, a.session = nil, ""
}

// dropped records that the connection ended on its own.
func (a *App) dropped(err error) {
	a.Out.line(a.Out.fail(fmt.Sprintf("✗ connection to %s lost: %v", a.cur.Name, err)))
	a.disconnect()
	a.Out.line(a.status())
}

// connect opens a connection to the named persona and a session on it:
// a new one, the latest (latest), or the one named by resume.
func (a *App) connect(name, resume string, latest bool) error {
	p, ok := a.dir.Find(name)
	if !ok {
		return fmt.Errorf("no persona named %q in the directory", name)
	}
	if p.Route == nil {
		return fmt.Errorf("no route to %s: add it to %s, for example\n  {\"personas\": {%q: {\"ssh\": \"HOST\", \"runtime\": \"hermes\", \"cwd\": \"/home/USER\"}}}",
			p.Name, a.dir.ConfigPath, p.Name)
	}
	cwd, err := p.Route.SessionCWD()
	if err != nil {
		return err
	}
	a.disconnect()
	a.cur, a.last = &p, ""
	a.Out.line(a.Out.dim(fmt.Sprintf("connecting to %s (%s)…", who(p), p.Route.Describe())))

	agent, err := a.Dial(*p.Route)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", p.Name, err)
	}
	ctx, cancel := context.WithTimeout(a.ctx, connectTimeout)
	defer cancel()
	info, err := agent.Initialize(ctx, version.Version)
	if err != nil {
		_ = agent.Close()
		return fmt.Errorf("cannot reach %s (%s): %w", p.Name, p.Route.Describe(), err)
	}
	a.agent, a.info, a.cwd = agent, info, cwd
	if err := a.open(resume, latest); err != nil {
		a.disconnect()
		return err
	}
	return nil
}

// open starts or reopens a session on the current connection.
func (a *App) open(resume string, latest bool) error {
	ctx, cancel := context.WithTimeout(a.ctx, connectTimeout)
	defer cancel()
	if resume == "" && !latest {
		id, err := a.agent.NewSession(ctx, a.cwd)
		if err != nil {
			return fmt.Errorf("%s could not start a session: %w", a.cur.Name, err)
		}
		a.session, a.last = id, id
		a.Out.line(a.status())
		return nil
	}
	if !a.info.CanLoad {
		return fmt.Errorf("%s's agent cannot reopen sessions", a.cur.Name)
	}
	id, err := a.resolve(ctx, resume)
	if err != nil {
		return err
	}
	replay := newReplay(a.Out)
	if err := a.agent.LoadSession(ctx, a.cwd, id, replay.handler()); err != nil {
		return fmt.Errorf("%s could not reopen session %s: %w", a.cur.Name, id, err)
	}
	replay.end()
	a.session, a.last = id, id
	a.Out.line(a.status())
	return nil
}

// resolve turns what the person typed (nothing, a list number, an ID or the
// start of one) into a session ID the agent knows.
func (a *App) resolve(ctx context.Context, want string) (string, error) {
	if !a.info.CanList {
		if want == "" {
			return "", fmt.Errorf("%s's agent cannot list sessions; give a session ID", a.cur.Name)
		}
		return want, nil
	}
	sessions, err := a.agent.ListSessions(ctx, a.cwd)
	if err != nil {
		return "", fmt.Errorf("%s could not list sessions: %w", a.cur.Name, err)
	}
	if len(sessions) == 0 {
		return "", fmt.Errorf("%s has no sessions in %s yet", a.cur.Name, a.cwd)
	}
	if want == "" {
		return sessions[0].ID, nil
	}
	var matches []string
	for _, s := range sessions {
		if s.ID == want {
			return s.ID, nil
		}
		if strings.HasPrefix(s.ID, want) {
			matches = append(matches, s.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if n, err := strconv.Atoi(want); err == nil && len(matches) == 0 && n >= 1 && n <= len(sessions) {
		return sessions[n-1].ID, nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("%q matches %d sessions; type more of the ID", want, len(matches))
	}
	return "", fmt.Errorf("%s has no session %q (see /sessions)", a.cur.Name, want)
}

func (a *App) showSessions() error {
	if a.agent == nil {
		return errors.New("not connected")
	}
	if !a.info.CanList {
		return fmt.Errorf("%s's agent cannot list sessions", a.cur.Name)
	}
	ctx, cancel := context.WithTimeout(a.ctx, connectTimeout)
	defer cancel()
	sessions, err := a.agent.ListSessions(ctx, a.cwd)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		a.Out.line("  no sessions yet")
	}
	for i, s := range sessions {
		mark := " "
		if s.ID == a.session {
			mark = "●"
		}
		title := oneLine(s.Title)
		if title == "" {
			title = "(untitled)"
		}
		a.Out.line(fmt.Sprintf("%s %2d  %s  %s  %s", mark, i+1, short(s.ID), a.Out.dim(when(s.UpdatedAt)), title))
	}
	return nil
}

// loop reads lines until the input ends, /quit, or Ctrl-C at the prompt.
func (a *App) loop() {
	for {
		a.Out.write(a.prompt())
		var done <-chan struct{}
		if a.agent != nil {
			done = a.agent.Done()
		}
		select {
		case line, ok := <-a.lines:
			if !ok {
				a.Out.line("")
				return
			}
			a.entered(line)
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if line == "/quit" || line == "/exit" {
				return
			}
			if err := a.handle(line); err != nil {
				a.Out.line(a.Out.fail("✗ " + err.Error()))
			}
		case <-done:
			a.Out.line("")
			a.dropped(a.agent.Err())
		case <-a.interrupt:
			a.Out.line("")
			return
		}
	}
}

// entered accounts for a line of input on the screen.
func (a *App) entered(line string) {
	if a.echo {
		a.Out.write(line + "\n")
		return
	}
	a.Out.typed()
}

func (a *App) handle(line string) error {
	if !strings.HasPrefix(line, "/") {
		if a.agent == nil {
			return a.pick(line)
		}
		return a.turn(line)
	}
	cmd, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "/help":
		a.Out.line(commandHelp)
	case "/status":
		a.Out.line(a.status())
	case "/persona", "/personas":
		if arg != "" {
			return a.connect(arg, "", false)
		}
		if err := a.loadDirectory(); err != nil {
			return err
		}
		a.showPersonas()
	case "/disconnect":
		a.disconnect()
		a.Out.line(a.status())
	case "/sessions":
		return a.showSessions()
	case "/new", "/resume":
		if a.cur == nil {
			return errors.New("pick a persona first: /persona NAME")
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
			return a.connect(a.cur.Name, arg, resume && arg == "")
		}
		return a.open(arg, resume && arg == "")
	default:
		return fmt.Errorf("unknown command %s (see /help)", cmd)
	}
	return nil
}

// pick connects to the persona named by a bare line typed while disconnected.
func (a *App) pick(line string) error {
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(a.dir.Personas) {
		return a.connect(a.dir.Personas[n-1].Name, "", false)
	}
	if _, ok := a.dir.Find(line); ok {
		return a.connect(line, "", false)
	}
	return errors.New("not connected, so nothing was sent. Type a persona's name or number, or /help")
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
		return err
	case stop == "cancelled":
		a.Out.line(a.Out.dim("(stopped)"))
	case stop != "" && stop != "end_turn":
		a.Out.line(a.Out.dim("(turn ended: " + stop + ")"))
	}
	return nil
}
