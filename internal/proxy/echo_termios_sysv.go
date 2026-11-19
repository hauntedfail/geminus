//go:build aix || linux || solaris || zos

package proxy

import "golang.org/x/sys/unix"

// Linux and similar systems read termios with TCGETS, the same ioctl used by golang.org/x/term.
const ioctlReadTermios = unix.TCGETS
