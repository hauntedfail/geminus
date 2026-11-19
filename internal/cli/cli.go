// Package cli parses geminus subcommands and determines their exit codes.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// Exit codes used by geminus itself. run and ask pass through the child's exit code.
const (
	ExitOK    = 0 // Success
	ExitError = 1 // Internal geminus error, such as I/O or PTY allocation failure
	ExitUsage = 2 // Usage error
)

const programName = "geminus"

// Env holds the command's I/O streams. Tests can substitute buffers.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Invocation holds the parsed inputs for one command execution.
type Invocation struct {
	Env     Env
	Options Options
	// Args contains the positional arguments after flag parsing.
	Args []string
}

// Run parses and executes args (excluding the program name) and returns the exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	env := Env{Stdin: stdin, Stdout: stdout, Stderr: stderr}

	var opts Options
	global := newGlobalFlagSet(&opts)
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(env.Stdout)
			return ExitOK
		}
		return usageError(env, "", err.Error())
	}

	rest := global.Args()
	if len(rest) == 0 {
		printUsage(env.Stderr)
		return ExitUsage
	}

	name, cmdArgs := rest[0], rest[1:]
	if name == "help" {
		return runHelp(env, cmdArgs)
	}
	cmd, ok := lookup(name)
	if !ok {
		return usageError(env, "", fmt.Sprintf("unknown command %q", name))
	}

	fs := newFlagSet(cmd, &opts)
	positionals, err := parseArgs(fs, cmdArgs, cmd.interspersed)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandHelp(env.Stdout, cmd)
			return ExitOK
		}
		return usageError(env, cmd.name, err.Error())
	}
	return cmd.run(Invocation{Env: env, Options: opts, Args: positionals})
}

func runHelp(env Env, args []string) int {
	switch len(args) {
	case 0:
		printUsage(env.Stdout)
		return ExitOK
	case 1:
		cmd, ok := lookup(args[0])
		if !ok {
			return usageError(env, "", fmt.Sprintf("unknown command %q", args[0]))
		}
		printCommandHelp(env.Stdout, cmd)
		return ExitOK
	default:
		return usageError(env, "", "help takes at most one command name")
	}
}

// usageError prints a usage error to stderr and returns ExitUsage. An empty command denotes
// a top-level error.
func usageError(env Env, command, msg string) int {
	if command == "" {
		fmt.Fprintf(env.Stderr, "%s: %s\nRun '%s --help' for usage.\n", programName, msg, programName)
		return ExitUsage
	}
	fmt.Fprintf(env.Stderr, "%s %s: %s\nRun '%s help %s' for usage.\n", programName, command, msg, programName, command)
	return ExitUsage
}

// fail prints an internal geminus error to stderr and returns ExitError.
func fail(env Env, command string, err error) int {
	fmt.Fprintf(env.Stderr, "%s %s: %v\n", programName, command, err)
	return ExitError
}
