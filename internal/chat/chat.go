// Package chat is geminus's terminal interface. It is a proxy screen: the
// person picks a persona (who) and a harness (which agent program on that
// persona's computer) and types. geminus writes each prompt into Honcho word
// for word; the bridge on the persona's computer runs it there, and whatever
// that agent prints appears here when it arrives.
//
// geminus keeps nothing of its own. Personas, what each computer offers,
// conversations, and the person's defaults all live in Honcho.
package chat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/profile"
	"github.com/hauntedfail/geminus/internal/version"
	"github.com/hauntedfail/geminus/internal/wire"
)

const usage = `geminus talks to the agents on your other computers, through Honcho.

Usage:
  geminus                  talk to the default persona, or pick one
  geminus persona          list personas and exit
  geminus persona NAME     talk to NAME
  geminus version | help

Options:
  --harness ID             use this agent program instead of the default
  --cwd DIR                directory on that computer for a new conversation
  --continue, -c           reopen the latest conversation
  --resume ID              reopen a conversation by ID

Everything else is chosen inside geminus (type /help there). geminus keeps
no files: personas, conversations and your defaults live in Honcho.
`

const commandHelp = `geminus commands
  /persona [NAME]        pick who to talk to
  /harness [ID]          pick the agent program on that persona's computer
  /new [DIR]             start a new conversation (optionally in DIR there)
  /sessions              list your conversations with this persona
  /resume [ID|N]         reopen a conversation (default: the latest)
  /status                show persona, harness, bridge and conversation
  /default               show your defaults; /default save keeps the current
                         persona, harness and directory;
                         /default persona|harness|cwd VALUE (or none)
  /quit                  leave geminus
  //TEXT                 send a line that starts with a slash to the agent
  While an answer is arriving, Ctrl-C stops watching it; the agent keeps
  working and the rest appears when it arrives. At the prompt, Ctrl-C quits.`

const (
	// watchEvery is how often Honcho is read while an answer is expected.
	watchEvery = time.Second
	// idleEvery is how often it is read at the prompt.
	idleEvery = 4 * time.Second
	// slowAfter is when geminus says that nothing has picked the prompt up.
	slowAfter = 20 * time.Second
)

// App is one run of the interface. The zero value is not usable; see Run.
type App struct {
	Out     *Printer
	Profile profile.Profile
	Honcho  *honcho.Client
	// Now and the two intervals are replaceable for tests.
	Now       func() time.Time
	Watch     time.Duration
	Idle      time.Duration
	SlowAfter time.Duration

	ctx   context.Context
	lines <-chan string
	// echo repeats each input line on the screen. A terminal already shows
	// what is typed; piped input would otherwise be invisible.
	echo bool
	// interrupt delivers Ctrl-C.
	interrupt <-chan os.Signal

	personas []Persona
	defaults wire.Defaults
	// defaultsSaved is when the defaults in Honcho were signed; 0 if none.
	defaultsSaved int64
	cur           *Persona
	harness       *wire.Harness
	// cwd is the directory for the next new conversation; session is the
	// open conversation, or "" before the first prompt creates one.
	cwd, session string
	// title is the open conversation's title.
	title string
	// shown holds the messages of the open conversation already on screen.
	shown map[string]bool
	// waiting holds the prompts whose answer has not ended yet, and state
	// the last state seen for each.
	waiting map[string]bool
	state   map[string]string
}

// Run is the geminus command. It returns the process exit code.
func Run(ctx context.Context, args []string, in io.Reader, out, errw io.Writer) int {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	req := parseArgs(args)
	app := &App{Out: newPrinter(out, isTerminal(out) && os.Getenv("NO_COLOR") == ""), ctx: ctx, interrupt: interrupt}
	if !req.badUsage && !req.version && !req.help {
		prof, err := profile.Load()
		if err != nil {
			fmt.Fprintln(errw, "geminus:", sanitize(err.Error()))
			return 1
		}
		client, err := honcho.New(prof.BaseURL, prof.Workspace, prof.APIKey)
		if err != nil {
			fmt.Fprintln(errw, "geminus:", sanitize(err.Error()))
			return 1
		}
		app.Profile, app.Honcho = prof, client
	}
	return app.main(req, in, errw)
}

// request is what the command line asked for.
type request struct {
	list                    bool
	persona, harness, cwd   string
	resume                  string
	latest                  bool
	version, help, badUsage bool
}

func parseArgs(args []string) request {
	var r request
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() string {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				r.badUsage = true
				return ""
			}
			return args[i]
		}
		switch {
		case len(args) == 1 && (arg == "version" || arg == "--version" || arg == "-v"):
			r.version = true
		case len(args) == 1 && (arg == "help" || arg == "--help" || arg == "-h"):
			r.help = true
		case arg == "persona" && i == 0:
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				r.persona = args[i]
			} else {
				r.list = true
			}
		case arg == "--harness" || arg == "-H":
			r.harness = value()
		case arg == "--cwd":
			r.cwd = value()
		case arg == "--resume":
			r.resume = value()
		case arg == "--continue" || arg == "-c":
			r.latest = true
		default:
			r.badUsage = true
		}
	}
	reopen := r.resume != "" || r.latest
	if r.list && (r.harness != "" || reopen || r.cwd != "") || r.resume != "" && r.latest ||
		reopen && (r.harness != "" || r.cwd != "") {
		r.badUsage = true
	}
	return r
}

// Main runs a command line against the input in, for tests.
func (a *App) Main(args []string, in io.Reader, errw io.Writer) int {
	return a.main(parseArgs(args), in, errw)
}

func (a *App) main(req request, in io.Reader, errw io.Writer) int {
	switch {
	case req.badUsage:
		fmt.Fprint(errw, usage)
		return 2
	case req.version:
		a.Out.line(version.String())
		return 0
	case req.help:
		a.Out.write(usage)
		return 0
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.Watch == 0 {
		a.Watch, a.Idle, a.SlowAfter = watchEvery, idleEvery, slowAfter
	}
	if err := a.loadDirectory(); err != nil {
		fmt.Fprintln(errw, "geminus:", sanitize(err.Error()))
		return 1
	}
	if req.list {
		a.showPersonas()
		return 0
	}
	a.lines = readLines(in)
	a.echo = !isTerminal(in)

	name := req.persona
	explicit := name != ""
	if name == "" {
		name = a.defaults.Persona
	}
	if name == "" {
		// A persona must be chosen before anything else can happen.
		picked, ok := a.pickPersona()
		if !ok {
			return 0
		}
		name = picked
	}
	var err error
	if req.resume != "" || req.latest {
		// A reopened conversation keeps the harness and directory it was
		// started with, so neither is asked for.
		if err = a.selectPersona(name); err == nil {
			err = a.resume(req.resume)
		}
	} else if err = a.start(name, req.harness); err == nil && req.cwd != "" {
		a.cwd = req.cwd
	}
	if err != nil {
		if explicit {
			// Asked for one persona on the command line: either that
			// conversation opens or the command fails.
			fmt.Fprintln(errw, "geminus:", sanitize(err.Error()))
			return 1
		}
		a.Out.line(a.Out.fail("✗ " + err.Error()))
	}
	a.Out.line(a.status())
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

// readLine reads one line of input outside the main loop.
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

// entered accounts for a line of input on the screen.
func (a *App) entered(line string) {
	if a.echo {
		a.Out.write(line + "\n")
		return
	}
	a.Out.typed()
}

func (a *App) prompt() string {
	switch {
	case a.cur == nil:
		return a.Out.dim("geminus") + " ❯ "
	case a.harness == nil:
		return a.Out.bold(who(*a.cur)) + " ❯ "
	}
	return a.Out.bold(who(*a.cur)) + a.Out.dim(" · "+a.harness.ID) + " ❯ "
}

// loop reads lines until the input ends, /quit, or Ctrl-C at the prompt.
// While it waits it shows answers that arrive for the open conversation.
func (a *App) loop() {
	idle := time.NewTicker(a.Idle)
	defer idle.Stop()
	for {
		a.Out.write(a.prompt())
	wait:
		for {
			select {
			case line, ok := <-a.lines:
				if !ok {
					a.Out.line("")
					return
				}
				a.entered(line)
				line = strings.TrimSpace(line)
				if line == "/quit" || line == "/exit" {
					return
				}
				if line != "" {
					if err := a.handle(line); err != nil {
						a.Out.line(a.Out.fail("✗ " + err.Error()))
					}
				}
				break wait
			case <-idle.C:
				// Results are asynchronous: show what arrived while idle.
				if len(a.waiting) > 0 && a.session != "" && a.arrivals() {
					break wait
				}
			case <-a.interrupt:
				a.Out.line("")
				return
			}
		}
	}
}
