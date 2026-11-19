// Package chat is geminus's terminal interface. It is a proxy screen: the
// person picks a persona (who) and a harness (which agent program on that
// persona's computer), and talks. Everything behind the screen (the model,
// tools, sessions, history) belongs to the harness; geminus only relays.
//
// The persona, the harness and the connection state are always on screen, in
// the prompt and in the status line.
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

	"github.com/hauntedfail/geminus/internal/acp"
	"github.com/hauntedfail/geminus/internal/harness"
	"github.com/hauntedfail/geminus/internal/persona"
	"github.com/hauntedfail/geminus/internal/version"
)

const usage = `geminus talks to your agents on other computers.

Usage:
  geminus                  connect to the default persona, or pick one
  geminus persona          list personas and exit
  geminus persona NAME     connect to NAME
  geminus version | help

Options when connecting:
  --harness ID             use this agent program instead of the default
  --continue, -c           reopen the latest session instead of starting one
  --resume ID              reopen a session by ID

Everything else is chosen inside geminus (type /help there) and remembered
with /default. Settings live in ~/.geminus/config.json.
`

// EnvTrace names the environment variable that turns on protocol tracing.
const EnvTrace = "GEMINUS_TRACE"

// connectTimeout bounds the handshake, including ssh and agent startup.
const connectTimeout = 90 * time.Second

// Agent is the part of an ACP connection the interface uses.
type Agent interface {
	Initialize(ctx context.Context, clientVersion string) (acp.Info, error)
	NewSession(ctx context.Context, cwd string) (string, error)
	LoadSession(ctx context.Context, cwd, sessionID string, h *acp.Handler) error
	ListSessions(ctx context.Context, cwd string) ([]acp.SessionInfo, error)
	Prompt(ctx context.Context, sessionID, text string, h *acp.Handler) (string, error)
	Commands(sessionID string) []acp.Command
	Done() <-chan struct{}
	Err() error
	Close() error
}

// App is one run of the interface. The zero value is not usable; see Run.
type App struct {
	Out *Printer
	// Load reads the persona directory and the settings.
	Load func(context.Context) (persona.Directory, error)
	// Probe finds out which harnesses a node has.
	Probe func(context.Context, persona.Node, []harness.Harness) (harness.Probe, error)
	// Dial starts the connection to a harness on a node.
	Dial func(persona.Node, harness.Harness) (Agent, error)
	// SaveDefaults and SaveNode write settings.
	SaveDefaults func(persona.Defaults) error
	SaveNode     func(string, persona.Node) error

	ctx   context.Context
	lines <-chan string
	// echo repeats each input line on the screen. A terminal already shows
	// what is typed; piped input would otherwise be invisible.
	echo bool
	// interrupt delivers Ctrl-C.
	interrupt <-chan os.Signal

	dir persona.Directory
	// cur, node, probe and choices describe the selected persona; har is
	// the selected harness. They outlive a lost connection.
	cur     *persona.Persona
	node    persona.Node
	probe   harness.Probe
	choices []harness.Choice
	har     *harness.Harness

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

	// GEMINUS_TRACE names a file that receives every protocol line. It holds
	// the conversation, so it is created private.
	var trace *os.File
	if path := os.Getenv(EnvTrace); path != "" {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(errw, "geminus:", err)
			return 1
		}
		defer f.Close()
		trace = f
	}

	app := &App{
		Out:  newPrinter(out, isTerminal(out) && os.Getenv("NO_COLOR") == ""),
		Load: persona.Load,
		Probe: func(ctx context.Context, node persona.Node, all []harness.Harness) (harness.Probe, error) {
			return node.Probe(ctx, all)
		},
		Dial: func(node persona.Node, h harness.Harness) (Agent, error) {
			cmd, err := node.Exec(h)
			if err != nil {
				return nil, err
			}
			proc, err := acp.Start(cmd)
			if err != nil {
				return nil, err
			}
			if trace != nil {
				proc.Trace(trace)
			}
			return proc, nil
		},
		SaveDefaults: persona.SaveDefaults,
		SaveNode:     persona.SaveNode,
		ctx:          ctx,
		interrupt:    interrupt,
	}
	return app.Main(args, in, errw)
}

// request is what the command line asked for.
type request struct {
	list                    bool
	persona, harness        string
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
		case arg == "--resume":
			r.resume = value()
		case arg == "--continue" || arg == "-c":
			r.latest = true
		default:
			r.badUsage = true
		}
	}
	if r.list && (r.harness != "" || r.resume != "" || r.latest) || r.resume != "" && r.latest {
		r.badUsage = true
	}
	return r
}

// Main runs the command line in args against the input in.
func (a *App) Main(args []string, in io.Reader, errw io.Writer) int {
	req := parseArgs(args)
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
	if err := a.loadDirectory(); err != nil {
		fmt.Fprintln(errw, "geminus:", sanitize(err.Error()))
		return 1
	}
	if req.list {
		a.showPersonas()
		return 0
	}
	defer a.disconnect()
	a.lines = readLines(in)
	a.echo = !isTerminal(in)

	name := req.persona
	explicit := name != ""
	if name == "" {
		name = a.dir.Defaults.Persona
	}
	if name == "" {
		// A persona must be chosen before anything else can happen.
		picked, ok := a.pickPersona()
		if !ok {
			return 0
		}
		name = picked
	}
	if err := a.start(name, req.harness, req.resume, req.latest); err != nil {
		if explicit {
			// Asked for one persona on the command line: either that
			// conversation opens or the command fails.
			fmt.Fprintln(errw, "geminus:", sanitize(err.Error()))
			return 1
		}
		a.Out.line(a.Out.fail("✗ " + err.Error()))
		a.Out.line(a.status())
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
