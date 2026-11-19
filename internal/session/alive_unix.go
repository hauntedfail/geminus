//go:build unix

package session

import (
	"errors"
	"syscall"
)

// ProcessAlive は pid のプロセスが存在するかを返す。
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// シグナル 0 は配送せず、宛先の存在と権限だけを確かめる。
	err := syscall.Kill(pid, 0)
	// EPERM は「存在するが他ユーザのプロセス」。
	return err == nil || errors.Is(err, syscall.EPERM)
}
