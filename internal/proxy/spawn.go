package proxy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"

	gopty "github.com/aymanbagabas/go-pty"
)

// Spec describes the child process to start.
type Spec struct {
	Argv []string
	Dir  string
	// A nil Env inherits the parent's environment.
	Env []string
	// Cols and Rows specify the initial PTY size.
	Cols, Rows int
}

// Child is a process running on a PTY.
type Child interface {
	// Read reads child output from the master side of the PTY.
	io.Reader
	// Write writes to the child's input.
	io.Writer
	// Resize changes the PTY dimensions.
	Resize(cols, rows int) error
	// Wait waits for the child to exit and returns its exit code. Call it only once.
	Wait() (exitCode int, err error)
	// Signal sends a signal to the child's process group.
	Signal(sig os.Signal) error
	// Kill forcibly terminates the child.
	Kill() error
	// Close releases the PTY and unblocks any pending Read. It is safe to call more than once.
	Close() error
}

// Spawner starts a child process on a PTY.
type Spawner interface {
	Spawn(spec Spec) (Child, error)
}

// PTYSpawner returns a Spawner that uses go-pty (Unix PTY or Windows ConPTY). If allocation fails,
// it returns ErrPTY rather than falling back to pipes.
func PTYSpawner() Spawner { return ptySpawner{} }

type ptySpawner struct{}

func (ptySpawner) Spawn(spec Spec) (Child, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, ErrNoCommand
	}
	path, err := resolveCommand(spec.Argv[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStart, err)
	}

	pty, err := gopty.New()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPTY, err)
	}
	if spec.Cols > 0 && spec.Rows > 0 {
		if err := pty.Resize(spec.Cols, spec.Rows); err != nil {
			return nil, errors.Join(fmt.Errorf("%w: set initial size: %v", ErrPTY, err), pty.Close())
		}
	}

	cmd := pty.Command(path, spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: %s: %v", ErrStart, spec.Argv[0], err), pty.Close())
	}
	return &ptyChild{pty: pty, cmd: cmd}, nil
}

// CheckPTY allocates and immediately releases a PTY to check availability for `geminus doctor`. It
// does not start a child and returns ErrPTY if allocation fails.
func CheckPTY() error {
	pty, err := gopty.New()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPTY, err)
	}
	if err := pty.Close(); err != nil {
		return fmt.Errorf("proxy: release pty: %w", err)
	}
	return nil
}

// resolveCommand looks up bare command names on PATH. Windows go-pty does not search PATH, so
// resolve names up front on all platforms. Leave names containing path separators alone: relative
// paths are resolved against Spec.Dir, which may differ from the working directory of geminus.
func resolveCommand(name string) (string, error) {
	if filepath.Base(name) != name {
		return name, nil
	}
	return exec.LookPath(name)
}

type ptyChild struct {
	pty gopty.Pty
	cmd *gopty.Cmd

	exited    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (c *ptyChild) Read(p []byte) (int, error)  { return c.pty.Read(p) }
func (c *ptyChild) Write(p []byte) (int, error) { return c.pty.Write(p) }

func (c *ptyChild) Resize(cols, rows int) error { return c.pty.Resize(cols, rows) }

func (c *ptyChild) Wait() (int, error) {
	err := c.cmd.Wait()
	c.exited.Store(true)
	state := c.cmd.ProcessState
	if state == nil {
		return -1, err
	}
	// A nonzero exit yields an *exec.ExitError. Report the exit code rather than treating it as an
	// error. On Windows, go-pty v0.2.2 does not return an ExitError, so read ProcessState.
	c.releaseSlave()
	return exitCode(state), nil
}

// releaseSlave closes the parent's copy of the slave end.
//
// Keep the slave open in the parent while the child runs to avoid losing its final output. Closing
// it after the child exits lets the master drain the remaining bytes before Read returns EOF (EIO
// on Linux).
func (c *ptyChild) releaseSlave() {
	if unix, ok := c.pty.(gopty.UnixPty); ok {
		// Closing twice is harmless; there is nothing useful to do with the error.
		_ = unix.Slave().Close()
	}
}

func (c *ptyChild) Signal(sig os.Signal) error {
	if c.exited.Load() {
		return nil // Never signal a reaped PID.
	}
	return signalChild(c.cmd.Process, c.pty, sig)
}

func (c *ptyChild) Kill() error {
	if c.exited.Load() {
		return nil
	}
	if err := killChild(c.cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (c *ptyChild) Close() error {
	c.closeOnce.Do(func() {
		if unix, ok := c.pty.(gopty.UnixPty); ok {
			// Wait may already have closed the slave, so close each end separately.
			c.releaseSlave()
			c.closeErr = ignoreClosed(unix.Master().Close())
			return
		}
		c.closeErr = ignoreClosed(c.pty.Close())
	})
	return c.closeErr
}

func ignoreClosed(err error) error {
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}
