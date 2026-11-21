//go:build !unix

package chat

import (
	"errors"
	"os"
	"os/exec"
)

// restart runs the program at exe on this console and waits for it: where a
// process cannot be replaced (Windows), this one stays behind only to pass
// on the exit code.
func restart(exe string, args []string) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}
