package proxy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"

	gopty "github.com/aymanbagabas/go-pty"
)

// Spec は起動する子プロセスの指定。
type Spec struct {
	Argv []string
	Dir  string
	// Env が nil なら親の環境を継承する。
	Env []string
	// Cols と Rows は PTY の初期サイズ。
	Cols, Rows int
}

// Child は PTY 上で動いている子プロセス。
type Child interface {
	// Read は子の出力（PTY の master 側）を読む。
	io.Reader
	// Write は子の入力へ書く。
	io.Writer
	// Resize は PTY のサイズを変える。
	Resize(cols, rows int) error
	// Wait は子の終了を待ち、終了コードを返す。1回だけ呼ぶ。
	Wait() (exitCode int, err error)
	// Signal は子のプロセスグループへシグナルを送る。
	Signal(sig os.Signal) error
	// Kill は子を強制終了する。
	Kill() error
	// Close は PTY を解放する。ブロック中の Read は解ける。何度呼んでもよい。
	Close() error
}

// Spawner は子プロセスを PTY 上で起動する。
type Spawner interface {
	Spawn(spec Spec) (Child, error)
}

// PTYSpawner は go-pty（Unix PTY / Windows ConPTY）で起動する Spawner を返す。
// PTY を確保できないときは ErrPTY を返し、パイプでは起動しない。
func PTYSpawner() Spawner { return ptySpawner{} }

type ptySpawner struct{}

func (ptySpawner) Spawn(spec Spec) (Child, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, ErrNoCommand
	}
	path, err := resolveCommand(spec.Argv[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStart, err)
	}

	pty, err := gopty.New()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPTY, err)
	}
	if spec.Cols > 0 && spec.Rows > 0 {
		if err := pty.Resize(spec.Cols, spec.Rows); err != nil {
			return nil, errors.Join(fmt.Errorf("%w: set initial size: %v", ErrPTY, err), pty.Close())
		}
	}

	cmd := pty.Command(path, spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: %s: %v", ErrStart, spec.Argv[0], err), pty.Close())
	}
	return &ptyChild{pty: pty, cmd: cmd}, nil
}

// CheckPTY は PTY を1つ確保してすぐ解放し、この環境で PTY を使えるかを確かめる。
// 子プロセスは起動しない（`geminus doctor` 用）。確保できなければ ErrPTY を返す。
func CheckPTY() error {
	pty, err := gopty.New()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPTY, err)
	}
	if err := pty.Close(); err != nil {
		return fmt.Errorf("proxy: release pty: %w", err)
	}
	return nil
}

// resolveCommand は名前だけのコマンドを PATH から探す。
// Windows の go-pty は PATH を探さないので、どの OS でも先に解決しておく。
// パス区切りを含む名前はそのまま返す。相対パスは子の作業ディレクトリ（Spec.Dir）から
// 解決されるので、geminus の作業ディレクトリで確かめると食い違う。
func resolveCommand(name string) (string, error) {
	if filepath.Base(name) != name {
		return name, nil
	}
	return exec.LookPath(name)
}

type ptyChild struct {
	pty gopty.Pty
	cmd *gopty.Cmd

	exited    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (c *ptyChild) Read(p []byte) (int, error)  { return c.pty.Read(p) }
func (c *ptyChild) Write(p []byte) (int, error) { return c.pty.Write(p) }

func (c *ptyChild) Resize(cols, rows int) error { return c.pty.Resize(cols, rows) }

func (c *ptyChild) Wait() (int, error) {
	err := c.cmd.Wait()
	c.exited.Store(true)
	state := c.cmd.ProcessState
	if state == nil {
		return -1, err
	}
	// 0 以外の終了は *exec.ExitError になるが、終了コードとして返すのでエラーにしない。
	// （Windows の go-pty v0.2.2 は ExitError を返さないので、ProcessState から読む。）
	c.releaseSlave()
	return exitCode(state), nil
}

// releaseSlave は親が持つ slave 側を閉じる。
//
// 子の実行中は親も slave を開いたままにし、子が最後に書いた出力が捨てられないようにする。
// 子の終了後に閉じると、master の Read が残りを読み切った後で EOF（Linux は EIO）になる。
func (c *ptyChild) releaseSlave() {
	if unix, ok := c.pty.(gopty.UnixPty); ok {
		// 二重に閉じても害は無く、エラーにも使い道が無い。
		_ = unix.Slave().Close()
	}
}

func (c *ptyChild) Signal(sig os.Signal) error {
	if c.exited.Load() {
		return nil // 回収済みの PID に送らない
	}
	return signalChild(c.cmd.Process, c.pty, sig)
}

func (c *ptyChild) Kill() error {
	if c.exited.Load() {
		return nil
	}
	if err := killChild(c.cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (c *ptyChild) Close() error {
	c.closeOnce.Do(func() {
		if unix, ok := c.pty.(gopty.UnixPty); ok {
			// slave は Wait で閉じている場合があるので、ここでは両方を個別に閉じる。
			c.releaseSlave()
			c.closeErr = ignoreClosed(unix.Master().Close())
			return
		}
		c.closeErr = ignoreClosed(c.pty.Close())
	})
	return c.closeErr
}

func ignoreClosed(err error) error {
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}
