package session

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrNotRegular は、セッションのファイルが通常ファイルでない（シンボリックリンク、
// 名前付きパイプ、デバイス、ディレクトリなど）か、セッションのディレクトリが
// 実ディレクトリでないことを表す。そういうものは辿らず、読まず、書かない。
var ErrNotRegular = errors.New("not a regular file")

// openRegular は path を読み取り用に開く。通常ファイルでなければ ErrNotRegular を返す。
//
// シンボリックリンクを辿るとセッションの外のファイルを表示してしまい、FIFO は開くだけで
// 書き手を待って止まる。Lstat で先に弾き、Unix では O_NOFOLLOW と O_NONBLOCK を付けて開き、
// 開いた後のファイルも確かめる（Lstat と open の間のすり替えに備える）。
func openRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, notRegular(path, info.Mode())
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	if err := requireRegular(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// readRegular は通常ファイル path の内容を読む。
func readRegular(path string) (data []byte, err error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, f.Close())
	}()
	return io.ReadAll(f)
}

// requireRegular は開いたファイルが通常ファイルであることを確かめる。
func requireRegular(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return notRegular(f.Name(), info.Mode())
	}
	return nil
}

// requireSessionDir は dir が実ディレクトリであることを確かめる。
// 無ければ fs.ErrNotExist を含むエラー、リンクなどなら ErrNotRegular を返す。
func requireSessionDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: the session directory is a %s", ErrNotRegular, fileKind(info.Mode()))
	}
	return nil
}

func notRegular(path string, mode fs.FileMode) error {
	return fmt.Errorf("%w: %s is a %s", ErrNotRegular, filepath.Base(path), fileKind(mode))
}

func fileKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "symbolic link"
	case mode.IsDir():
		return "directory"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeDevice != 0:
		return "device"
	default:
		return "special file"
	}
}
