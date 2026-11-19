//go:build !unix

package session

import "os"

// appendOpenFlags は Unix 専用（O_NOFOLLOW | O_NONBLOCK）。
// この OS では Lstat と、開いた後の確認に頼る。
const appendOpenFlags = 0

// openNoFollow は path を読み取り用に開く。
// この OS には O_NOFOLLOW が無いので、呼び出し側の Lstat と、開いた後の確認で弾く。
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

func symlinkError(_ string, err error) error {
	return err
}
