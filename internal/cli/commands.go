package cli

import (
	"flag"
	"fmt"

	"github.com/hauntedfail/geminus/internal/version"
)

// command defines a subcommand.
type command struct {
	name    string
	summary string
	// usage describes the arguments following "geminus <name>".
	usage string
	// interspersed enables flag parsing after positional arguments.
	interspersed bool
	// flags registers command-specific flags; newFlagSet adds the common flags.
	flags func(fs *flag.FlagSet, o *Options)
	run   func(inv Invocation) int
}

// commands lists subcommands in help order.
func commands() []command {
	return []command{
		{
			name:    "run",
			summary: "Start or resume an interactive agent session",
			usage:   "[--agent NAME] [--title T] [--resume ID] [--fresh] [--no-record] [--force] [--] [prompt...]",
			flags: func(fs *flag.FlagSet, o *Options) {
				fs.StringVar(&o.Run.Title, "title", "", "set the session `TITLE`")
				fs.StringVar(&o.Run.Resume, "resume", "", "resume the geminus session `ID`")
				fs.BoolVar(&o.Run.Fresh, "fresh", false, "with --resume: keep the geminus session but start a new agent-side session")
				fs.BoolVar(&o.Run.NoRecord, "no-record", false, "do not record output, piped input, arguments or a default title")
				fs.BoolVar(&o.Run.Force, "force", false, "open the session even if it looks like it is already running")
			},
			run: runRun,
		},
		{
			name:    "ask",
			summary: "Send one prompt and print the reply",
			usage:   "[--agent NAME] [--session ID] [--continue] [--json] \"prompt\"",
			flags: func(fs *flag.FlagSet, o *Options) {
				fs.StringVar(&o.Ask.Session, "session", "", "continue the geminus session `ID`")
				fs.BoolVar(&o.Ask.Continue, "continue", false, "continue the session most recently updated in this directory")
			},
			run: runAsk,
		},
		{
			name:         "ls",
			summary:      "List sessions",
			usage:        "[--json] [--all]",
			interspersed: true,
			flags: func(fs *flag.FlagSet, o *Options) {
				fs.BoolVar(&o.Ls.All, "all", false, "list every session (default: the 20 most recent)")
			},
			run: runLs,
		},
		{
			name:         "show",
			summary:      "Show session metadata",
			usage:        "<id> [--json]",
			interspersed: true,
			run:          runShow,
		},
		{
			name:         "log",
			summary:      "Print the recorded output of a session",
			usage:        "<id> [--raw] [--json] [--tail N]",
			interspersed: true,
			flags: func(fs *flag.FlagSet, o *Options) {
				fs.BoolVar(&o.Log.Raw, "raw", false, "keep ANSI escape sequences")
				fs.IntVar(&o.Log.Tail, "tail", 0, "print only the last `N` lines (0 = all)")
			},
			run: runLog,
		},
		{
			name:         "rm",
			summary:      "Delete a session",
			usage:        "<id> [--force]",
			interspersed: true,
			flags: func(fs *flag.FlagSet, o *Options) {
				fs.BoolVar(&o.Rm.Force, "force", false, "delete even if the session is running; do not fail if it does not exist")
			},
			run: runRm,
		},
		{
			name:         "agents",
			summary:      "List detected backends and their capabilities",
			usage:        "[--json]",
			interspersed: true,
			run:          runAgents,
		},
		{
			name:         "doctor",
			summary:      "Diagnose PTY, terminal and backends",
			usage:        "[--json]",
			interspersed: true,
			run:          runDoctor,
		},
		{
			name:         "version",
			summary:      "Print the version",
			usage:        "",
			interspersed: true,
			run:          runVersion,
		},
	}
}

func lookup(name string) (command, bool) {
	for _, cmd := range commands() {
		if cmd.name == name {
			return cmd, true
		}
	}
	return command{}, false
}

func runVersion(inv Invocation) int {
	if len(inv.Args) > 0 {
		return usageError(inv.Env, "version", fmt.Sprintf("unexpected argument %q", inv.Args[0]))
	}
	fmt.Fprintln(inv.Env.Stdout, version.String())
	return ExitOK
}
