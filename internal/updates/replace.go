package updates

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// newSuffix marks the download beside the program until it is whole.
	newSuffix = ".new"
	// oldSuffix marks the program a Windows computer was running when it was
	// replaced.
	oldSuffix = ".old"
)

// rename is os.Rename, replaceable in tests.
var rename = os.Rename

// Executable is the path of the running program, with links followed, so the
// file itself is replaced and not a link to it.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// Replace puts what write produces in the place of the program exe. The new
// file is written beside it, flushed to disk and renamed over it, so exe is
// at every moment the old program or the new one. A running program cannot
// be overwritten on Windows but can be renamed: there the old one is moved
// aside first (exe+".old"), for Sweep to remove later. If anything fails,
// exe is the old program and nothing else is left.
func Replace(exe, goos string, write func(io.Writer) error) (err error) {
	fresh := exe + newSuffix
	mode := os.FileMode(0o755)
	if info, statErr := os.Stat(exe); statErr == nil {
		mode = info.Mode().Perm()
	}
	// A leftover of an interrupted update is not a reason to stop.
	if rmErr := os.Remove(fresh); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return fmt.Errorf("cannot write to %s: %w", filepath.Dir(exe), rmErr)
	}
	f, err := os.OpenFile(fresh, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", filepath.Dir(exe), err)
	}
	defer func() {
		if err != nil {
			os.Remove(fresh)
		}
	}()
	if err = write(f); err == nil {
		// Without this a power cut after the rename could leave an empty
		// file under the program's name.
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if goos != "windows" {
		if err = rename(fresh, exe); err == nil {
			syncDir(filepath.Dir(exe))
		}
		return err
	}
	old := aside(exe)
	if err = rename(exe, old); err != nil {
		return err
	}
	if err = rename(fresh, exe); err != nil {
		// Put the running program back under its name.
		if backErr := rename(old, exe); backErr != nil {
			return fmt.Errorf("%w; and the program is now at %s: %w", err, old, backErr)
		}
		return err
	}
	return nil
}

// aside is a free name to move the running program to. exe+".old" may still
// be there and in use: after /update on Windows the process before this one
// stays behind, running from it.
func aside(exe string) string {
	old := exe + oldSuffix
	if err := os.Remove(old); err == nil || errors.Is(err, os.ErrNotExist) {
		return old
	}
	return fmt.Sprintf("%s.%d", old, os.Getpid())
}

// syncDir makes a rename in dir last through a power cut, where the system
// lets a directory be flushed.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// Sweep removes the programs earlier updates on Windows moved aside. One
// that is still running cannot be removed and is left for a later start.
func Sweep(exe string) {
	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(exe)+oldSuffix) {
			os.Remove(filepath.Join(filepath.Dir(exe), e.Name()))
		}
	}
}
