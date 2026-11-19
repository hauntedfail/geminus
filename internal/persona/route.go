package persona

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// An ssh alias is passed to ssh as an argument. Restricting it keeps it from
// being read as an option and from carrying anything a shell would interpret.
var sshAlias = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]*$`)

// agentCommands are the default ACP entry points of each runtime.
var agentCommands = map[string]string{
	"hermes": "hermes acp",
	// Claude Code speaks ACP through Zed's adapter, which must be installed
	// on the agent's machine.
	"claude": "claude-agent-acp",
}

func (r Route) validate() error {
	if _, ok := agentCommands[r.Runtime]; !ok {
		return fmt.Errorf("runtime must be \"hermes\" or \"claude\", not %q", r.Runtime)
	}
	if r.SSH == "" {
		return nil
	}
	if !sshAlias.MatchString(r.SSH) {
		return fmt.Errorf("invalid ssh host alias %q", r.SSH)
	}
	if !strings.HasPrefix(r.CWD, "/") && !regexp.MustCompile(`^[A-Za-z]:[\\/]`).MatchString(r.CWD) {
		return errors.New("an ssh route needs \"cwd\": an absolute directory on the remote machine")
	}
	return nil
}

// AgentCommand returns the command line that starts the agent's ACP server.
func (r Route) AgentCommand() string {
	if r.Command != "" {
		return r.Command
	}
	return agentCommands[r.Runtime]
}

// SessionCWD returns the directory sessions run in on the agent's machine.
func (r Route) SessionCWD() (string, error) {
	if r.CWD != "" || r.SSH != "" {
		return r.CWD, nil
	}
	return os.Getwd()
}

// Exec returns the process that connects to the agent: the agent itself
// for a local route, ssh for a remote one.
func (r Route) Exec() (*exec.Cmd, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	agent := r.AgentCommand()
	if r.SSH == "" {
		argv := strings.Fields(agent)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = r.CWD
		return cmd, nil
	}
	remote := agent
	if r.Command == "" {
		// A non-interactive ssh command gets a bare PATH. A login shell
		// reads the profile that puts the agent (~/.local/bin) on PATH.
		remote = "sh -lc " + shellQuote("exec "+agent)
	}
	// BatchMode fails instead of asking for a password: the terminal belongs
	// to the conversation, not to ssh.
	return exec.Command("ssh", "-T", "-oBatchMode=yes", "-oConnectTimeout=10", "--", r.SSH, remote), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
