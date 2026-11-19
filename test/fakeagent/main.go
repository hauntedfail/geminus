// Command fakeagent provides a stand-in agent for end-to-end tests.
//
// geminus starts it in place of claude, hermes or a custom command to get predictable output.
//
//	fakeagent --version                         print the version and exit
//	fakeagent -p ... <prompt>                   simulate a Claude one-shot invocation
//	fakeagent chat ... -q <prompt> --oneshot    simulate a Hermes one-shot invocation
//	fakeagent [args...]                         echo interactive input line by line
//
// In interactive mode, print a colored banner, the arguments and whether stdin is a terminal.
// Reply to each input line with "echo:<line>" until a line containing "quit" or EOF.
//
// Environment variables:
//
//	FAKE_EXIT=<n>       exit code (default: 0)
//	FAKE_RECORD=<path>  write interactive input lines verbatim to this file so tests can
//	                    check what arrived without parsing output mixed with PTY echo.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

const (
	versionLine = "fakeagent 0.0.1"
	// banner includes ANSI color so tests can verify both recording and escape stripping by `log`.
	banner    = "\x1b[1;32mfakeagent\x1b[0m ready"
	farewell  = "\x1b[2mbye\x1b[0m"
	quitLine  = "quit"
	envExit   = "FAKE_EXIT"
	envRecord = "FAKE_RECORD"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	code, err := exitCode(os.Getenv(envExit))
	if err != nil {
		fmt.Fprintf(stderr, "fakeagent: %v\n", err)
		return 2
	}
	if len(args) == 1 && args[0] == "--version" {
		say(stdout, versionLine)
		return 0
	}

	say(stdout, "argv:"+strings.Join(args, " "))
	say(stdout, "tty:"+strconv.FormatBool(term.IsTerminal(int(stdin.Fd()))))
	if prompt, ok := oneshotPrompt(args); ok {
		say(stdout, "reply:"+prompt)
		return code
	}

	received, closeRecord, err := openRecord(os.Getenv(envRecord))
	if err != nil {
		fmt.Fprintf(stderr, "fakeagent: %v\n", err)
		return 2
	}
	say(stdout, banner)
	err = echoLines(stdin, stdout, received)
	if closeErr := closeRecord(); err == nil {
		err = closeErr
	}
	if err != nil {
		fmt.Fprintf(stderr, "fakeagent: %v\n", err)
		return 2
	}
	say(stdout, farewell)
	return code
}

// openRecord opens the destination for received lines. An empty path discards them.
func openRecord(path string) (io.Writer, func() error, error) {
	if path == "" {
		return io.Discard, func() error { return nil }, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

// exitCode reads FAKE_EXIT, defaulting to zero when empty.
func exitCode(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	code, err := strconv.Atoi(value)
	if err != nil || code < 0 || code > 255 {
		return 0, fmt.Errorf("%s=%q is not an exit code", envExit, value)
	}
	return code, nil
}

// say emits each line in a single write so PTY echo cannot interleave within it.
func say(w io.Writer, line string) {
	// There is no point continuing if output is closed, but this test helper leaves error reporting to
	// the exit code.
	_, _ = io.WriteString(w, line+"\n")
}

// oneshotPrompt returns the prompt if args specifies a one-shot invocation. Claude uses `-p ...
// <prompt>` (the final argument); Hermes uses `-q <prompt> --oneshot`.
func oneshotPrompt(args []string) (string, bool) {
	hasFlag := func(name string) bool {
		for _, arg := range args {
			if arg == name {
				return true
			}
		}
		return false
	}
	switch {
	case hasFlag("--oneshot"):
		for i, arg := range args {
			if arg == "-q" && i+1 < len(args) {
				return args[i+1], true
			}
		}
		return "", true
	case hasFlag("-p") && len(args) > 0:
		return args[len(args)-1], true
	default:
		return "", false
	}
}

// echoLines replies to each line from r with "echo:<line>" on w and writes the line itself to
// received. It stops on a line containing quit or at EOF.
func echoLines(r io.Reader, w, received io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == quitLine {
			return nil
		}
		if _, err := io.WriteString(received, line+"\n"); err != nil {
			return err
		}
		say(w, "echo:"+line)
	}
	return scanner.Err()
}
