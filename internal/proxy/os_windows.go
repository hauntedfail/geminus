//go:build windows

package proxy

import (
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

const (
	enableVirtualTerminalProcessing = 0x0004
	disableNewlineAutoReturn        = 0x0008
	enableVirtualTerminalInput      = 0x0200

	// Windows には SIGWINCH が無いので、この間隔でサイズを見比べる。
	resizePollInterval = 250 * time.Millisecond
)

var procSetConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

// handledSignals は中継の間 geminus が自分で受けるシグナル。
func handledSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// forwardable は sig を子へ転送するかを返す。
func forwardable(os.Signal) bool {
	return true
}

// signalChild は Ctrl-C を ConPTY への入力として送る。それ以外は強制終了しか手段が無い。
func signalChild(proc *os.Process, input io.Writer, sig os.Signal) error {
	if sig == os.Interrupt {
		_, err := input.Write([]byte{0x03})
		return err
	}
	return proc.Kill()
}

func killChild(proc *os.Process) error {
	return proc.Kill()
}

func exitCode(state *os.ProcessState) int {
	return state.ExitCode()
}

// watchResize はコンソールのサイズを定期的に見て、変わったら notify を呼ぶ。
func watchResize(t *stdTerminal, notify func()) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(resizePollInterval)
		defer ticker.Stop()
		lastCols, lastRows, _ := t.Size()
		for {
			select {
			case <-ticker.C:
				cols, rows, err := t.Size()
				if err != nil || (cols == lastCols && rows == lastRows) {
					continue
				}
				lastCols, lastRows = cols, rows
				notify()
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// enableVT はコンソールに VT シーケンスをそのまま通させる。
// 入力側は矢印キーなどをエスケープシーケンスで受け、出力側は子の出すシーケンスを解釈する。
// コンソールでないハンドル（リダイレクト）は GetConsoleMode が失敗するので触らない。
// 入力側のモードは term.Restore が元に戻すので、ここで戻すのは出力側だけ。
func enableVT(in, out *os.File) func() {
	addConsoleMode(in, enableVirtualTerminalInput)
	return addConsoleMode(out, enableVirtualTerminalProcessing|disableNewlineAutoReturn)
}

func addConsoleMode(f *os.File, flags uint32) (restore func()) {
	restore = func() {}
	if f == nil {
		return restore
	}
	handle := syscall.Handle(f.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(handle, &mode); err != nil {
		return restore
	}
	if !setConsoleMode(handle, mode|flags) {
		// VT 非対応の古いコンソール。raw mode だけで続ける。
		return restore
	}
	return func() { setConsoleMode(handle, mode) }
}

func setConsoleMode(handle syscall.Handle, mode uint32) bool {
	ret, _, _ := procSetConsoleMode.Call(uintptr(handle), uintptr(mode))
	return ret != 0
}
