package cli

import (
	"flag"
	"io"
	"strings"
)

// Globals holds flags shared by all commands. They may appear before or after the command
// name.
type Globals struct {
	// Home overrides GEMINUS_HOME. If empty, use the environment variable, falling back to
	// ~/.geminus.
	Home string
	// Agent selects the backend. If empty, use the configuration, falling back to claude.
	Agent string
	// JSON selects machine-readable output.
	JSON bool
	// Verbose enables diagnostics on stderr.
	Verbose bool
}

// RunOptions holds the flags for `geminus run`.
type RunOptions struct {
	Title    string
	Resume   string
	Fresh    bool
	NoRecord bool
	Force    bool
}

// AskOptions holds the flags for `geminus ask`.
type AskOptions struct {
	Session  string
	Continue bool
}

// LsOptions holds the flags for `geminus ls`.
type LsOptions struct {
	All bool
}

// LogOptions holds the flags for `geminus log`.
type LogOptions struct {
	Raw  bool
	Tail int
}

// RmOptions holds the flags for `geminus rm`.
type RmOptions struct {
	Force bool
}

// Options holds all parsed flags.
type Options struct {
	Globals
	Run RunOptions
	Ask AskOptions
	Ls  LsOptions
	Log LogOptions
	Rm  RmOptions
}

var globalFlagNames = map[string]bool{"home": true, "agent": true, "json": true, "v": true}

// bindGlobals registers common flags with their current values as defaults so parsing the
// subcommand preserves flags set before its name.
func bindGlobals(fs *flag.FlagSet, o *Options) {
	fs.StringVar(&o.Home, "home", o.Home, "keep state in `DIR` (default: $GEMINUS_HOME, else ~/.geminus)")
	fs.StringVar(&o.Agent, "agent", o.Agent, "use backend `NAME` (default: config, else claude)")
	fs.BoolVar(&o.JSON, "json", o.JSON, "machine-readable output")
	fs.BoolVar(&o.Verbose, "v", o.Verbose, "verbose diagnostics on stderr")
}

func newQuietFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	// cli.Run prints its own error messages and usage.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

func newGlobalFlagSet(o *Options) *flag.FlagSet {
	fs := newQuietFlagSet(programName)
	bindGlobals(fs, o)
	return fs
}

func newFlagSet(cmd command, o *Options) *flag.FlagSet {
	fs := newQuietFlagSet(programName + " " + cmd.name)
	if cmd.flags != nil {
		cmd.flags(fs, o)
	}
	bindGlobals(fs, o)
	return fs
}

// parseArgs parses flags and returns positional arguments. In interspersed mode it accepts
// flags after positional arguments, as in `show <id> --json`. Otherwise it stops at the
// first positional argument to pass run and ask prompts through unchanged. In both modes,
// everything after `--` is positional.
func parseArgs(fs *flag.FlagSet, args []string, interspersed bool) ([]string, error) {
	if !interspersed {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		return fs.Args(), nil
	}

	var positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(positionals, args[i+1:]...), nil
		}
		if !looksLikeFlag(arg) {
			positionals = append(positionals, arg)
			continue
		}
		tokens := []string{arg}
		if flagTakesNextArg(fs, arg) && i+1 < len(args) {
			i++
			tokens = append(tokens, args[i])
		}
		if err := fs.Parse(tokens); err != nil {
			return nil, err
		}
	}
	return positionals, nil
}

func looksLikeFlag(arg string) bool {
	return len(arg) >= 2 && arg[0] == '-'
}

// flagTakesNextArg reports whether arg consumes the next argument as its value. Unknown
// flags return false so fs.Parse can report the error.
func flagTakesNextArg(fs *flag.FlagSet, arg string) bool {
	name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	if strings.Contains(name, "=") {
		return false
	}
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return false
	}
	return true
}
