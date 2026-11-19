//go:build !unix && !windows

package session

// ProcessAlive reports whether pid exists. This OS cannot check, so return false rather than
// prevent startup.
func ProcessAlive(pid int) bool {
	return false
}
