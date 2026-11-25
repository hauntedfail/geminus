package tui

import "os"

// IsTerminal reports whether w is an *os.File attached to a character device.
func IsTerminal(w any) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
