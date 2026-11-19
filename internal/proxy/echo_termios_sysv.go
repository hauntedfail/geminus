//go:build aix || linux || solaris || zos

package proxy

import "golang.org/x/sys/unix"

// termios を読む ioctl。Linux などは TCGETS（golang.org/x/term と同じ使い分け）。
const ioctlReadTermios = unix.TCGETS
