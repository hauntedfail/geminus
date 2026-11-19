//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

package proxy

import "golang.org/x/sys/unix"

// echoEnabled reads the termios ECHO flag.
func echoEnabled(fd int) (bool, error) {
	termios, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return false, err
	}
	return termios.Lflag&unix.ECHO != 0, nil
}
