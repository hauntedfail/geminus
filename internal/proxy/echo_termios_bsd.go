//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package proxy

import "golang.org/x/sys/unix"

// termios を読む ioctl。BSD 系は TIOCGETA（golang.org/x/term と同じ使い分け）。
const ioctlReadTermios = unix.TIOCGETA
