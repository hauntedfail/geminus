// Package harness knows the agent programs geminus can talk to and finds out
// which of them a computer actually has.
//
// A harness is anything that serves the Agent Client Protocol on its stdin
// and stdout: Hermes, Claude Code, Codex, pi and so on. Some do it natively
// (`hermes acp`); some need a separate bridge program (`claude-agent-acp`).
package harness

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Harness describes one agent program.
type Harness struct {
	// ID is what the person types and what config files store.
	ID string `json:"-"`
	// Name is the display name.
	Name string `json:"name"`
	// Command starts the ACP server. Its first word is the program that
	// must be installed for the harness to be usable.
	Command string `json:"command"`
	// CLI is the agent's own program, when that differs from Command's.
	// Finding CLI but not Command means the agent is installed without its
	// ACP bridge.
	CLI string `json:"cli,omitempty"`
	// Install says how to get the bridge.
	Install string `json:"install,omitempty"`
}

// builtin lists the harnesses geminus knows without configuration. Each
// command was checked against the program's own help or package metadata.
var builtin = []Harness{
	{ID: "hermes", Name: "Hermes Agent", Command: "hermes acp"},
	{ID: "claude", Name: "Claude Code", Command: "claude-agent-acp", CLI: "claude", Install: "npm install -g @zed-industries/claude-agent-acp"},
	{ID: "codex", Name: "Codex", Command: "codex-acp", CLI: "codex", Install: "npm install -g @zed-industries/codex-acp"},
	{ID: "pi", Name: "pi", Command: "pi-acp", CLI: "pi", Install: "npm install -g pi-acp"},
	{ID: "opencode", Name: "OpenCode", Command: "opencode acp"},
	{ID: "gemini", Name: "Gemini CLI", Command: "gemini --acp"},
	{ID: "grok", Name: "Grok", Command: "grok agent stdio"},
}

// A program name ends up in a shell script that runs on the agent's machine,
// so only plain names and paths are accepted.
var (
	programName = regexp.MustCompile(`^[A-Za-z0-9/][A-Za-z0-9._+/-]*$`)
	idPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	// Command arguments are passed through a remote shell as well.
	commandPattern = regexp.MustCompile(`^[A-Za-z0-9._+=:/@ -]+$`)
)

// Program returns the executable a command line starts with.
func Program(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func (h Harness) validate() error {
	switch {
	case !idPattern.MatchString(h.ID):
		return fmt.Errorf("harness id %q: use lower-case letters, digits, '.', '_' and '-'", h.ID)
	case !commandPattern.MatchString(h.Command) || !programName.MatchString(Program(h.Command)):
		return fmt.Errorf("harness %q: \"command\" must be a program name followed by plain arguments, got %q", h.ID, h.Command)
	case h.CLI != "" && !programName.MatchString(h.CLI):
		return fmt.Errorf("harness %q: \"cli\" must be a program name, got %q", h.ID, h.CLI)
	}
	return nil
}

// All returns the built-in harnesses followed by the custom ones, sorted by
// ID. A custom harness with a built-in's ID replaces it in place.
func All(custom map[string]Harness) ([]Harness, error) {
	out := append([]Harness(nil), builtin...)
	ids := make([]string, 0, len(custom))
	for id := range custom {
		ids = append(ids, id)
	}
	sort.Strings(ids)
next:
	for _, id := range ids {
		h := custom[id]
		h.ID = id
		if h.Name == "" {
			h.Name = id
		}
		if err := h.validate(); err != nil {
			return nil, err
		}
		for i := range out {
			if out[i].ID == id {
				out[i] = h
				continue next
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// Programs returns every executable worth looking for, without duplicates.
func Programs(all []Harness) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range all {
		for _, name := range []string{Program(h.Command), h.CLI} {
			if name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// Probe is what a computer reported about itself.
type Probe struct {
	// Home is the home directory there.
	Home string
	// Have holds the programs found on its PATH.
	Have map[string]bool
}

// probeMarker lets the script be recognized (the end-to-end tests' fake ssh
// answers it instead of starting an agent).
const probeMarker = "geminus-probe"

// Script returns a POSIX shell script that prints the home directory and
// which of the programs are installed.
func Script(all []Harness) string {
	return fmt.Sprintf(`: %s; printf 'home=%%s\n' "$HOME"; for c in %s; do command -v "$c" >/dev/null 2>&1 && printf 'have=%%s\n' "$c"; done; true`,
		probeMarker, strings.Join(Programs(all), " "))
}

// ParseProbe reads the output of Script. Lines it does not recognize are
// ignored: a login shell may print a greeting first.
func ParseProbe(output string) Probe {
	p := Probe{Have: map[string]bool{}}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if home, ok := strings.CutPrefix(line, "home="); ok {
			p.Home = home
		} else if name, ok := strings.CutPrefix(line, "have="); ok && programName.MatchString(name) {
			p.Have[name] = true
		}
	}
	return p
}

// Choice is a harness as it stands on one computer.
type Choice struct {
	Harness
	// Available is false when the agent is installed but its ACP bridge is
	// not, so it can be shown with the fix but not selected.
	Available bool
}

// Choices returns the harnesses the probed computer has: the usable ones
// first, then those that only lack their bridge. Harnesses with nothing
// installed are left out.
func (p Probe) Choices(all []Harness) []Choice {
	var usable, blocked []Choice
	for _, h := range all {
		switch {
		case p.Have[Program(h.Command)]:
			usable = append(usable, Choice{Harness: h, Available: true})
		case h.CLI != "" && p.Have[h.CLI]:
			blocked = append(blocked, Choice{Harness: h})
		}
	}
	return append(usable, blocked...)
}
