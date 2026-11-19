//go:build unix

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// appendOpenFlags は追記用に開くときに足すフラグ。
// O_NOFOLLOW で、最後の要素がシンボリックリンクなら開かない。
// O_NONBLOCK で、読み手のいない FIFO を待たずに失敗する。通常ファイルへの書き込みには影響しない。
const appendOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// openNoFollow は path を読み取り用に開く。
// O_NOFOLLOW でシンボリックリンクを辿らず、O_NONBLOCK で FIFO の書き手を待たない。
// 通常ファイルの読み取りは O_NONBLOCK の影響を受けない。
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, symlinkError(path, err)
	}
	return f, nil
}

// symlinkError は O_NOFOLLOW がシンボリックリンクを拒否したエラーを ErrNotRegular にする。
// Linux と macOS は ELOOP、FreeBSD は EMLINK を返す。
func symlinkError(path string, err error) error {
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
		return fmt.Errorf("%w: %s is a symbolic link", ErrNotRegular, filepath.Base(path))
	}
	return err
}
