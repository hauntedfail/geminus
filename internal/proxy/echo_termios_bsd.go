//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package proxy

import "golang.org/x/sys/unix"

// BSD systems read termios with TIOCGETA, the same ioctl used by golang.org/x/term.
const ioctlReadTermios = unix.TIOCGETA
