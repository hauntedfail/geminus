package persona

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/harness"
)

// probeTimeout bounds finding out what a computer has installed.
const probeTimeout = 30 * time.Second

// An ssh alias is passed to ssh as an argument. Restricting it keeps it from
// being read as an option and from carrying anything a shell would interpret.
var sshAlias = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]*$`)

// Node is the computer a persona's agent runs on.
type Node struct {
	// SSH is an ssh host alias. Empty means this computer.
	SSH string `json:"ssh,omitempty"`
	// CWD is the directory sessions run in there. Empty means the home
	// directory on a remote computer and the current directory on this one.
	CWD string `json:"cwd,omitempty"`
	// Harness is the agent program to use on this persona by default.
	Harness string `json:"harness,omitempty"`
}

// Where returns a short description of the location, such as "ssh btcvps".
func (n Node) Where() string {
	if n.SSH == "" {
		return "this computer"
	}
	return "ssh " + n.SSH
}

func (n Node) validate() error {
	if n.SSH != "" && !sshAlias.MatchString(n.SSH) {
		return fmt.Errorf("invalid ssh host alias %q", n.SSH)
	}
	if strings.ContainsAny(n.CWD, "\x00\r\n") {
		return errors.New("invalid cwd")
	}
	return nil
}

// sshArgs is how geminus always calls ssh. BatchMode fails instead of asking
// for a password: the terminal belongs to the conversation, not to ssh.
func (n Node) sshArgs(remote string) []string {
	return []string{"-T", "-oBatchMode=yes", "-oConnectTimeout=10", "--", n.SSH, remote}
}

// loginShell wraps a command for the remote side. A non-interactive ssh
// command gets a bare PATH; a login shell reads the profile that puts agents
// (~/.local/bin, node version managers) on it.
func loginShell(script string) string {
	return "sh -lc " + shellQuote(script)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Probe finds out which of the harnesses' programs the node has, and its
// home directory.
func (n Node) Probe(ctx context.Context, all []harness.Harness) (harness.Probe, error) {
	if err := n.validate(); err != nil {
		return harness.Probe{}, err
	}
	if n.SSH == "" {
		probe := harness.Probe{Have: map[string]bool{}}
		probe.Home, _ = os.UserHomeDir()
		for _, name := range harness.Programs(all) {
			if _, err := exec.LookPath(name); err == nil {
				probe.Have[name] = true
			}
		}
		return probe, nil
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", n.sshArgs(loginShell(harness.Script(all)))...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		if len(reason) > 300 {
			reason = reason[len(reason)-300:]
		}
		return harness.Probe{}, errors.New(strings.Join(strings.Fields(reason), " "))
	}
	return harness.ParseProbe(string(out)), nil
}

// SessionCWD returns the directory sessions run in on the node.
func (n Node) SessionCWD(probe harness.Probe) (string, error) {
	switch {
	case n.CWD != "":
		return n.CWD, nil
	case n.SSH == "":
		return os.Getwd()
	case probe.Home != "":
		return probe.Home, nil
	}
	return "", errors.New("the remote home directory is unknown; set \"cwd\" for this persona")
}

// Exec returns the process that connects to a harness on the node: the
// harness's ACP server itself on this computer, ssh to it otherwise.
func (n Node) Exec(h harness.Harness) (*exec.Cmd, error) {
	if err := n.validate(); err != nil {
		return nil, err
	}
	argv := strings.Fields(h.Command)
	if len(argv) == 0 {
		return nil, fmt.Errorf("harness %q has no command", h.ID)
	}
	if n.SSH == "" {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = n.CWD
		return cmd, nil
	}
	return exec.Command("ssh", n.sshArgs(loginShell("exec "+h.Command))...), nil
}
