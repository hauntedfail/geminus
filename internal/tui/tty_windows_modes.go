package tui

// Windows ConPTY / PowerShell console mode bit flags (kernel32 SetConsoleMode).
// Kept OS-independent so bit transitions can be tested on any host.
const (
	WinEnableProcessedInput       uint32 = 0x0001
	WinEnableLineInput            uint32 = 0x0002
	WinEnableEchoInput            uint32 = 0x0004
	WinEnableMouseInput           uint32 = 0x0010
	WinEnableVirtualTerminalInput uint32 = 0x0200

	WinEnableProcessedOutput       uint32 = 0x0001
	WinEnableVirtualTerminalOutput uint32 = 0x0004
)

// WindowsRawConsoleModes computes the input and output console modes needed
// for raw virtual-terminal interaction on Windows ConPTY / PowerShell.
func WindowsRawConsoleModes(inMode, outMode uint32) (rawIn, rawOut uint32) {
	rawOut = outMode | WinEnableProcessedOutput | WinEnableVirtualTerminalOutput
	rawIn = inMode&^(WinEnableProcessedInput|WinEnableLineInput|WinEnableEchoInput) |
		WinEnableVirtualTerminalInput | WinEnableMouseInput
	return rawIn, rawOut
}
