//go:build unix

package chat

import (
	"os"
	"syscall"
)

// restart puts the program at exe in the place of this process. It only
// returns if that fails.
func restart(exe string, args []string) (int, error) {
	return 0, syscall.Exec(exe, append([]string{exe}, args...), os.Environ())
}
