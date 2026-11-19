//go:build unix

package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	gopty "github.com/aymanbagabas/go-pty"
	"golang.org/x/term"
)

// openSlave allocates a real PTY and returns its terminal (slave) end.
func openSlave(t *testing.T) *os.File {
	t.Helper()
	pty, err := gopty.New()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { pty.Close() })
	unix, ok := pty.(gopty.UnixPty)
	if !ok {
		t.Skip("not a unix pty")
	}
	return unix.Slave()
}

// Raw mode disables echo; restoring the terminal re-enables it. The doctor command uses this
// difference to detect a missed restore.
func TestTerminalEchoFollowsRawMode(t *testing.T) {
	slave := openSlave(t)

	if on, err := TerminalEcho(slave); err != nil || !on {
		t.Fatalf("fresh pty: TerminalEcho = %v, %v; want true", on, err)
	}
	state, err := term.MakeRaw(int(slave.Fd()))
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if on, err := TerminalEcho(slave); err != nil || on {
		t.Errorf("raw mode: TerminalEcho = %v, %v; want false", on, err)
	}
	if err := term.Restore(int(slave.Fd()), state); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if on, err := TerminalEcho(slave); err != nil || !on {
		t.Errorf("restored: TerminalEcho = %v, %v; want true", on, err)
	}
}

func TestTerminalEchoNeedsATerminal(t *testing.T) {
	if _, err := TerminalEcho(nil); !errors.Is(err, ErrNotTTY) {
		t.Errorf("nil file: err = %v, want ErrNotTTY", err)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "plain"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := TerminalEcho(f); !errors.Is(err, ErrNotTTY) {
		t.Errorf("plain file: err = %v, want ErrNotTTY", err)
	}
}
