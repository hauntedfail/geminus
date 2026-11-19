package proxy

import (
	"errors"
	"os"
)

// ErrEchoUnsupported means this OS cannot report the terminal echo setting.
var ErrEchoUnsupported = errors.New("proxy: reading the terminal echo setting is not supported on this platform")

// TerminalEcho reports whether the terminal referred to by f echoes input. It does not change the
// setting.
//
// Raw mode disables echo, so a false result when no relay is active may indicate that a process
// exited without restoring the terminal. TerminalEcho returns ErrNotTTY for a nonterminal and
// ErrEchoUnsupported if the OS cannot report the setting.
func TerminalEcho(f *os.File) (bool, error) {
	if !isTerminalFile(f) {
		return false, ErrNotTTY
	}
	return echoEnabled(int(f.Fd()))
}
