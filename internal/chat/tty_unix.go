//go:build darwin || linux

package chat

import (
	"os"
	"syscall"
	"unsafe"
)

func ioctl(fd uintptr, request uint, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(request), uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// rawMode makes the terminal hand over each key as it is pressed, without
// echoing it and without turning Ctrl-C into a signal. Output is left as it
// is. The returned function puts the terminal back.
func rawMode(in, _ *os.File) (restore func(), err error) {
	fd := in.Fd()
	var old syscall.Termios
	if err := ioctl(fd, ioctlGetTermios, unsafe.Pointer(&old)); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.ICRNL | syscall.INLCR | syscall.IGNCR | syscall.IXON | syscall.ISTRIP
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := ioctl(fd, ioctlSetTermios, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return func() { _ = ioctl(fd, ioctlSetTermios, unsafe.Pointer(&old)) }, nil
}

// termSize returns the terminal's size, or zeros.
func termSize(out *os.File) (cols, rows int) {
	var size struct{ Row, Col, X, Y uint16 }
	if ioctl(out.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&size)) != nil {
		return 0, 0
	}
	return int(size.Col), int(size.Row)
}

func cancelInputRead(*os.File) {}
