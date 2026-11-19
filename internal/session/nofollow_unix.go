//go:build unix

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// appendOpenFlags adds flags for appending. O_NOFOLLOW rejects a symlink in the final path
// component. O_NONBLOCK fails immediately on a FIFO without a reader and does not affect regular
// file writes.
const appendOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// openNoFollow opens path for reading. O_NOFOLLOW prevents following a symlink, and O_NONBLOCK
// avoids waiting for a FIFO writer without affecting regular file reads.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, symlinkError(path, err)
	}
	return f, nil
}

// symlinkError maps errors from O_NOFOLLOW rejecting a symlink to ErrNotRegular. Linux and macOS
// return ELOOP; FreeBSD returns EMLINK.
func symlinkError(path string, err error) error {
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
		return fmt.Errorf("%w: %s is a symbolic link", ErrNotRegular, filepath.Base(path))
	}
	return err
}
