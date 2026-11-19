package proxy

import (
	"errors"
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

var (
	// ErrNoSize means neither input nor output is a terminal that can report its size.
	ErrNoSize = errors.New("proxy: terminal size is not available")
	// ErrNotTTY means the input cannot be put into raw mode because it is not a terminal.
	ErrNotTTY = errors.New("proxy: input is not a terminal")
)

// Terminal represents the user's terminal.
type Terminal interface {
	// Read reads input from the user.
	io.Reader
	// Write writes to the user's screen.
	io.Writer
	// IsTTY reports whether the input is an interactive terminal. If not, Run skips raw mode.
	IsTTY() bool
	// MakeRaw puts the terminal in raw mode and returns a function that restores it.
	MakeRaw() (restore func() error, err error)
	// Size returns the number of columns and rows, or ErrNoSize if unavailable.
	Size() (cols, rows int, err error)
	// WatchResize calls notify whenever the terminal size changes. No callbacks occur after stop
	// returns.
	WatchResize(notify func()) (stop func())
}

// OSTerminal returns a terminal backed by standard input and output.
func OSTerminal() Terminal {
	return NewTerminal(os.Stdin, os.Stdout)
}

// NewTerminal returns a terminal backed by in and out.
//
// An *os.File that refers to a terminal supports TTY operations (raw mode, size queries and resize
// notifications). Pipes, regular files and in-memory buffers are not TTYs.
func NewTerminal(in io.Reader, out io.Writer) Terminal {
	t := &stdTerminal{in: in, out: out}
	if f, ok := in.(*os.File); ok {
		t.inFile = f
	}
	if f, ok := out.(*os.File); ok {
		t.outFile = f
	}
	return t
}

type stdTerminal struct {
	in      io.Reader
	out     io.Writer
	inFile  *os.File // nil unless in is an *os.File
	outFile *os.File // nil unless out is an *os.File
}

func (t *stdTerminal) Read(p []byte) (int, error)  { return t.in.Read(p) }
func (t *stdTerminal) Write(p []byte) (int, error) { return t.out.Write(p) }

func isTerminalFile(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

func (t *stdTerminal) IsTTY() bool { return isTerminalFile(t.inFile) }

func (t *stdTerminal) MakeRaw() (func() error, error) {
	if !t.IsTTY() {
		return nil, ErrNotTTY
	}
	fd := int(t.inFile.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	restoreVT := enableVT(t.inFile, t.outFile)

	var once sync.Once
	var restoreErr error
	return func() error {
		once.Do(func() {
			restoreVT()
			restoreErr = term.Restore(fd, state)
		})
		return restoreErr
	}, nil
}

// sizeFile returns the file to query for the terminal size. It prefers an output terminal so
// `geminus run < file` still uses the screen dimensions.
func (t *stdTerminal) sizeFile() *os.File {
	for _, f := range []*os.File{t.outFile, t.inFile} {
		if isTerminalFile(f) {
			return f
		}
	}
	return nil
}

func (t *stdTerminal) Size() (int, int, error) {
	f := t.sizeFile()
	if f == nil {
		return 0, 0, ErrNoSize
	}
	cols, rows, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0, 0, err
	}
	return cols, rows, nil
}

func (t *stdTerminal) WatchResize(notify func()) func() {
	if t.sizeFile() == nil {
		return func() {}
	}
	return watchResize(t, notify)
}
