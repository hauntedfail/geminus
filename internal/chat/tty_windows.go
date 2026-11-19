package chat

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableProcessedInput       = 0x0001
	enableLineInput            = 0x0002
	enableEchoInput            = 0x0004
	enableVirtualTerminalInput = 0x0200

	enableProcessedOutput       = 0x0001
	enableVirtualTerminalOutput = 0x0004
)

func setConsoleMode(handle syscall.Handle, mode uint32) error {
	if r, _, err := procSetConsoleMode.Call(uintptr(handle), uintptr(mode)); r == 0 {
		return err
	}
	return nil
}

// rawMode makes the console hand over each key as it is pressed, as the
// escape sequences other terminals send, and makes it obey such sequences on
// output. The returned function puts both back.
func rawMode(in, out *os.File) (restore func(), err error) {
	inHandle, outHandle := syscall.Handle(in.Fd()), syscall.Handle(out.Fd())
	var inMode, outMode uint32
	if err := syscall.GetConsoleMode(inHandle, &inMode); err != nil {
		return nil, err
	}
	if err := syscall.GetConsoleMode(outHandle, &outMode); err != nil {
		return nil, err
	}
	if err := setConsoleMode(outHandle, outMode|enableProcessedOutput|enableVirtualTerminalOutput); err != nil {
		return nil, err
	}
	raw := inMode&^(enableProcessedInput|enableLineInput|enableEchoInput) | enableVirtualTerminalInput
	if err := setConsoleMode(inHandle, raw); err != nil {
		_ = setConsoleMode(outHandle, outMode)
		return nil, err
	}
	return func() {
		_ = setConsoleMode(inHandle, inMode)
		_ = setConsoleMode(outHandle, outMode)
	}, nil
}

// termSize returns the size of the console window, or zeros.
func termSize(out *os.File) (cols, rows int) {
	var info struct {
		SizeX, SizeY, CursorX, CursorY int16
		Attributes                     uint16
		Left, Top, Right, Bottom       int16
		MaxX, MaxY                     int16
	}
	if r, _, _ := procGetConsoleScreenBufferInfo.Call(out.Fd(), uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0, 0
	}
	return int(info.Right-info.Left) + 1, int(info.Bottom-info.Top) + 1
}
