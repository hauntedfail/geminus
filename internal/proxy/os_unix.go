//go:build unix

package proxy

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// handledSignals は中継の間 geminus が自分で受けるシグナル。
//
// SIGINT / SIGTERM / SIGHUP / SIGQUIT は子へ転送する。raw mode 中の Ctrl-C はバイトとして
// PTY を通るので、ここに来るのは外から送られたもの。
// SIGPIPE は転送しない。受けておくと、閉じた標準出力（`geminus run | head`）への書き込みが
// プロセスの即死ではなく EPIPE になり、端末を復元してから終われる。
func handledSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGPIPE}
}

// forwardable は sig を子へ転送するかを返す。
func forwardable(sig os.Signal) bool {
	return sig != syscall.SIGPIPE
}

// signalChild は子のプロセスグループへ sig を送る。
// go-pty は Setsid で起動するので、子はセッションリーダーで、pgid は pid に等しい。
//
// 呼び出し側（ptyChild）は Wait が返った後は送らないが、カーネルが子を回収してから
// その印が立つまでの一瞬は防げない。その間に同じ pgid が再利用される可能性は、
// PID が一巡する必要があるので無視している。
func signalChild(proc *os.Process, _ io.Writer, sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return proc.Signal(sig)
	}
	err := syscall.Kill(-proc.Pid, s)
	if errors.Is(err, syscall.ESRCH) {
		return nil // 既に終了している
	}
	return err
}

func killChild(proc *os.Process) error {
	return signalChild(proc, nil, syscall.SIGKILL)
}

// exitCode は終了コードを返す。シグナルで終了した場合はシェルと同じ 128+番号にする。
func exitCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}

// watchResize は SIGWINCH のたびに notify を呼ぶ。
func watchResize(_ *stdTerminal, notify func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ch:
				notify()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
		wg.Wait()
	}
}

// enableVT は Windows のコンソール専用。Unix の端末は最初から VT を解釈する。
func enableVT(_, _ *os.File) func() {
	return func() {}
}
