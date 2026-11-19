package cli

import (
	"bytes"
	"strings"
	"testing"
)

type result struct {
	code   int
	stdout string
	stderr string
}

func run(t *testing.T, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, strings.NewReader(""), &out, &errOut)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

var allCommands = []string{"run", "ask", "ls", "show", "log", "rm", "agents", "doctor", "version"}

func TestVersion(t *testing.T) {
	r := run(t, "version")
	if r.code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %q)", r.code, ExitOK, r.stderr)
	}
	if !strings.HasPrefix(r.stdout, "geminus ") {
		t.Errorf("stdout = %q, want prefix %q", r.stdout, "geminus ")
	}
	if strings.Count(r.stdout, "\n") != 1 {
		t.Errorf("stdout = %q, want exactly one line", r.stdout)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty", r.stderr)
	}
}

func TestHelpListsAllCommands(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := run(t, args...)
			if r.code != ExitOK {
				t.Fatalf("exit = %d, want %d", r.code, ExitOK)
			}
			for _, name := range allCommands {
				if !strings.Contains(r.stdout, "\n  "+name+" ") {
					t.Errorf("help does not list %q:\n%s", name, r.stdout)
				}
			}
			if r.stderr != "" {
				t.Errorf("stderr = %q, want empty", r.stderr)
			}
		})
	}
}

func TestHelpRunShowsFlags(t *testing.T) {
	for _, args := range [][]string{{"help", "run"}, {"run", "--help"}, {"run", "-h"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := run(t, args...)
			if r.code != ExitOK {
				t.Fatalf("exit = %d, want %d (stderr: %q)", r.code, ExitOK, r.stderr)
			}
			for _, flagName := range []string{"--agent", "--title", "--resume", "--fresh", "--no-record", "--force", "--home"} {
				if !strings.Contains(r.stdout, flagName) {
					t.Errorf("help run does not mention %s:\n%s", flagName, r.stdout)
				}
			}
			if !strings.Contains(r.stdout, "geminus run") {
				t.Errorf("help run has no usage line:\n%s", r.stdout)
			}
		})
	}
}

func TestEveryCommandHasHelp(t *testing.T) {
	for _, name := range allCommands {
		t.Run(name, func(t *testing.T) {
			r := run(t, "help", name)
			if r.code != ExitOK {
				t.Fatalf("exit = %d, want %d", r.code, ExitOK)
			}
			if !strings.Contains(r.stdout, "geminus "+name) {
				t.Errorf("usage line missing:\n%s", r.stdout)
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{"unknown command", []string{"bogus"}, `unknown command "bogus"`},
		{"no arguments", nil, "Usage:"},
		{"unknown global flag", []string{"--nope", "version"}, "nope"},
		{"unknown command flag", []string{"ls", "--nope"}, "nope"},
		{"help for unknown command", []string{"help", "bogus"}, `unknown command "bogus"`},
		{"missing flag value", []string{"run", "--agent"}, "agent"},
		{"version takes no arguments", []string{"version", "extra"}, "extra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := run(t, tt.args...)
			if r.code != ExitUsage {
				t.Fatalf("exit = %d, want %d", r.code, ExitUsage)
			}
			if !strings.Contains(r.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", r.stderr, tt.wantStderr)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want empty", r.stdout)
			}
		})
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name         string
		command      string
		args         []string
		wantPos      []string
		wantJSON     bool
		wantAgent    string
		wantHome     string
		wantErr      bool
		checkGlobals bool
	}{
		{
			name:    "flags after positional are parsed when interspersed",
			command: "show", args: []string{"20261119-105700-a1b2", "--json"},
			wantPos: []string{"20261119-105700-a1b2"}, wantJSON: true, checkGlobals: true,
		},
		{
			name:    "flag value form with equals",
			command: "log", args: []string{"--tail=5", "abc", "--raw"},
			wantPos: []string{"abc"}, checkGlobals: true,
		},
		{
			name:    "flag with separate value between positionals",
			command: "log", args: []string{"abc", "--tail", "5", "def"},
			wantPos: []string{"abc", "def"}, checkGlobals: true,
		},
		{
			name:    "double dash ends flag parsing",
			command: "rm", args: []string{"--", "--force"},
			wantPos: []string{"--force"}, checkGlobals: true,
		},
		{
			name:    "run stops at first positional",
			command: "run", args: []string{"--agent", "cmd", "fix", "--fresh"},
			wantPos: []string{"fix", "--fresh"}, wantAgent: "cmd", checkGlobals: true,
		},
		{
			name:    "run passes everything after double dash",
			command: "run", args: []string{"--agent", "cmd", "--", "sh", "-c", "exit 7"},
			wantPos: []string{"sh", "-c", "exit 7"}, wantAgent: "cmd", checkGlobals: true,
		},
		{
			name:    "global flags are accepted after the command",
			command: "ls", args: []string{"--home", "/tmp/x", "--json"},
			wantHome: "/tmp/x", wantJSON: true, checkGlobals: true,
		},
		{
			name:    "missing value",
			command: "log", args: []string{"abc", "--tail"},
			wantErr: true,
		},
		{
			name:    "non numeric value",
			command: "log", args: []string{"--tail", "x"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, ok := lookup(tt.command)
			if !ok {
				t.Fatalf("command %q not registered", tt.command)
			}
			var g Options
			fs := newFlagSet(cmd, &g)
			pos, err := parseArgs(fs, tt.args, cmd.interspersed)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseArgs succeeded, want error (pos=%v)", pos)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if strings.Join(pos, "\x00") != strings.Join(tt.wantPos, "\x00") {
				t.Errorf("positionals = %q, want %q", pos, tt.wantPos)
			}
			if !tt.checkGlobals {
				return
			}
			if g.JSON != tt.wantJSON {
				t.Errorf("JSON = %v, want %v", g.JSON, tt.wantJSON)
			}
			if g.Agent != tt.wantAgent {
				t.Errorf("Agent = %q, want %q", g.Agent, tt.wantAgent)
			}
			if g.Home != tt.wantHome {
				t.Errorf("Home = %q, want %q", g.Home, tt.wantHome)
			}
		})
	}
}

func TestGlobalFlagsBeforeCommand(t *testing.T) {
	r := run(t, "--home", t.TempDir(), "-v", "version")
	if r.code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %q)", r.code, ExitOK, r.stderr)
	}
	if !strings.HasPrefix(r.stdout, "geminus ") {
		t.Errorf("stdout = %q", r.stdout)
	}
}
