package session

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrNotRegular means a session file is not a regular file (for example, a symlink, FIFO, device
// or directory), or the session directory is not a real directory. Do not follow, read or write
// such entries.
var ErrNotRegular = errors.New("not a regular file")

// openRegular opens path for reading and returns ErrNotRegular if it is not a regular file.
//
// Following a symlink could expose a file outside the session; opening a FIFO could block waiting
// for a writer. Reject these with Lstat first. On Unix, open with O_NOFOLLOW and O_NONBLOCK, and
// check the open file in case it was replaced between Lstat and open.
func openRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, notRegular(path, info.Mode())
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	if err := requireRegular(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// readRegular reads the contents of the regular file at path.
func readRegular(path string) (data []byte, err error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, f.Close())
	}()
	return io.ReadAll(f)
}

// requireRegular checks that the open file is a regular file.
func requireRegular(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return notRegular(f.Name(), info.Mode())
	}
	return nil
}

// requireSessionDir checks that dir is a real directory. It returns an error wrapping
// fs.ErrNotExist if absent, or ErrNotRegular for a symlink or other nondirectory entry.
func requireSessionDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: the session directory is a %s", ErrNotRegular, fileKind(info.Mode()))
	}
	return nil
}

func notRegular(path string, mode fs.FileMode) error {
	return fmt.Errorf("%w: %s is a %s", ErrNotRegular, filepath.Base(path), fileKind(mode))
}

func fileKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "symbolic link"
	case mode.IsDir():
		return "directory"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeDevice != 0:
		return "device"
	default:
		return "special file"
	}
}
