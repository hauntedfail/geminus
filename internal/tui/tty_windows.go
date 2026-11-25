package tui

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procCancelIoEx                 = kernel32.NewProc("CancelIoEx")
)

func setConsoleMode(handle syscall.Handle, mode uint32) error {
	if r, _, err := procSetConsoleMode.Call(uintptr(handle), uintptr(mode)); r == 0 {
		return err
	}
	return nil
}

// RawMode makes the console hand over each key as it is pressed, as the
// escape sequences other terminals send, and makes it obey such sequences on
// output. The returned function puts both back.
func RawMode(in, out *os.File) (restore func(), err error) {
	inHandle, outHandle := syscall.Handle(in.Fd()), syscall.Handle(out.Fd())
	var inMode, outMode uint32
	if err := syscall.GetConsoleMode(inHandle, &inMode); err != nil {
		return nil, err
	}
	if err := syscall.GetConsoleMode(outHandle, &outMode); err != nil {
		return nil, err
	}
	rawIn, rawOut := WindowsRawConsoleModes(inMode, outMode)
	if err := setConsoleMode(outHandle, rawOut); err != nil {
		return nil, err
	}
	if err := setConsoleMode(inHandle, rawIn); err != nil {
		_ = setConsoleMode(outHandle, outMode)
		return nil, err
	}
	return func() {
		_ = setConsoleMode(inHandle, inMode)
		_ = setConsoleMode(outHandle, outMode)
	}, nil
}

// TermSize returns the size of the console window, or zeros.
func TermSize(out *os.File) (cols, rows int) {
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

// CancelInputRead cancels any pending synchronous Read on the console input
// handle so the keyboard reader goroutine exits before a relaunched child
// process takes over the console.
func CancelInputRead(in *os.File) {
	if in == nil {
		return
	}
	_, _, _ = procCancelIoEx.Call(in.Fd(), 0)
}
