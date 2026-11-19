//go:build !unix && !windows

package session

// ProcessAlive は pid のプロセスが存在するかを返す。
// この OS では確かめる手段が無いので、常に false を返す（起動を妨げない側に倒す）。
func ProcessAlive(pid int) bool {
	return false
}
