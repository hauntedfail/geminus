//go:build !unix

package session

import "os"

// appendOpenFlags is only needed on Unix (O_NOFOLLOW | O_NONBLOCK). On this OS, rely on Lstat and
// the post-open check.
const appendOpenFlags = 0

// openNoFollow opens path for reading. Without O_NOFOLLOW, the caller relies on Lstat and the
// post-open check to reject nonregular files.
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

func symlinkError(_ string, err error) error {
	return err
}
