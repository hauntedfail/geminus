//go:build !darwin && !linux && !windows

package chat

import (
	"errors"
	"os"
)

// On other systems geminus stays line by line.
func rawMode(_, _ *os.File) (func(), error) { return nil, errors.New("no raw mode on this system") }

func termSize(*os.File) (cols, rows int) { return 0, 0 }

func cancelInputRead(*os.File) {}
