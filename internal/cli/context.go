package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hauntedfail/geminus/internal/agent"
	"github.com/hauntedfail/geminus/internal/session"
)

const (
	configFileName = "config.json"
	// Maximum number of characters in a default title.
	maxTitleRunes = 60
)

// usageErr indicates a usage error, which report maps to ExitUsage.
type usageErr struct{ msg string }

func (e *usageErr) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageErr{msg: fmt.Sprintf(format, args...)}
}

// report writes err to stderr and returns the corresponding exit code.
func report(env Env, command string, err error) int {
	var usage *usageErr
	if errors.As(err, &usage) {
		return usageError(env, command, usage.msg)
	}
	return fail(env, command, err)
}

// warn reports a nonfatal problem on stderr.
func warn(env Env, command string, err error) {
	fmt.Fprintf(env.Stderr, "%s %s: warning: %v\n", programName, command, err)
}

// verboseLogf returns a function that writes to stderr when -v is set, or nil otherwise.
func verboseLogf(inv Invocation) func(format string, args ...any) {
	if !inv.Options.Verbose {
		return nil
	}
	return func(format string, args ...any) {
		fmt.Fprintf(inv.Env.Stderr, programName+": "+format+"\n", args...)
	}
}

// openStore opens the state directory selected by --home, GEMINUS_HOME, or ~/.geminus, in
// that order.
func openStore(inv Invocation) (*session.Store, error) {
	home, err := session.ResolveHome(inv.Options.Home)
	if err != nil {
		return nil, err
	}
	return session.Open(home)
}

// config holds the contents of the optional $GEMINUS_HOME/config.json file.
type config struct {
	// Agent selects the backend when --agent is omitted.
	Agent string `json:"agent"`
}

// loadConfig reads config.json from home. A missing file yields an empty config; unreadable
// or invalid content is an error.
func loadConfig(home string) (config, error) {
	path := filepath.Join(home, configFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return config{}, nil
		}
		return config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// defaultAgentName selects the backend for a new session. Precedence is --agent, then
// config.json, then claude.
func defaultAgentName(inv Invocation, home string) (string, error) {
	if inv.Options.Agent != "" {
		return inv.Options.Agent, nil
	}
	cfg, err := loadConfig(home)
	if err != nil {
		return "", err
	}
	if cfg.Agent != "" {
		return cfg.Agent, nil
	}
	return agent.DefaultName, nil
}

// lookupAdapter returns the adapter for name, treating an unknown name as a usage error.
func lookupAdapter(name string) (agent.Adapter, error) {
	adapter, err := agent.Lookup(name)
	if errors.Is(err, agent.ErrUnknownAgent) {
		return nil, &usageErr{msg: err.Error()}
	}
	return adapter, err
}

// writeJSON writes v to w as a single indented JSON value.
//
// encoding/json emits DEL and C1 controls (U+0080 through U+009F) literally. Escape them as
// \u00XX so terminals cannot interpret them as control sequences. They occur only inside
// JSON strings, so escaping preserves the decoded value.
func writeJSON(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	var out bytes.Buffer
	for _, r := range buf.String() {
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			fmt.Fprintf(&out, `\u%04x`, r)
			continue
		}
		out.WriteRune(r)
	}
	_, err := w.Write(out.Bytes())
	return err
}

// printable makes text from files or external programs safe for terminal display. It
// replaces controls (including ESC, newlines, and C1), bidirectional formatting characters,
// and invalid UTF-8 with "?" so fields like title and cwd in session.json cannot inject
// terminal escapes. Use it for human-readable output, not --json.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r):
			return '?'
		case (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			return '?'
		default:
			return r
		}
	}, s)
}

// oneArg requires exactly one positional argument and returns it.
func oneArg(args []string, what string) (string, error) {
	switch len(args) {
	case 0:
		return "", usagef("missing %s", what)
	case 1:
		return args[0], nil
	default:
		return "", usagef("unexpected argument %q", args[1])
	}
}

// noArgs checks that there are no positional arguments.
func noArgs(args []string) error {
	if len(args) > 0 {
		return usagef("unexpected argument %q", args[0])
	}
	return nil
}

// defaultTitle builds a title from args when --title is absent. For cmd, redact secrets
// before using the command line.
func defaultTitle(adapter agent.Adapter, args []string) string {
	if adapter.Name() == agent.GenericName {
		args = agent.RedactArgv(args)
	}
	return truncateTitle(strings.Join(args, " "))
}

// truncateTitle keeps only the first line and shortens it if necessary.
func truncateTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if utf8.RuneCountInString(s) <= maxTitleRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxTitleRunes-1]) + "…"
}

// sameDir reports whether a and b refer to the same directory, even through symlinks such
// as /tmp and /private/tmp.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}
