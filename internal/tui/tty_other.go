//go:build !darwin && !linux && !windows

package tui

import (
	"errors"
	"os"
)

// On other systems geminus stays line by line.
func RawMode(_, _ *os.File) (func(), error) { return nil, errors.New("no raw mode on this system") }

func TermSize(*os.File) (cols, rows int) { return 0, 0 }

// CancelInputRead is a no-op on non-Windows platforms.
func CancelInputRead(*os.File) {}
