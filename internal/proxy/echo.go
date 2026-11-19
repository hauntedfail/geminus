package proxy

import (
	"errors"
	"os"
)

// ErrEchoUnsupported は、この OS では端末の echo 設定を読めないことを表す。
var ErrEchoUnsupported = errors.New("proxy: reading the terminal echo setting is not supported on this platform")

// TerminalEcho は、f の指す端末が入力を echo する設定かを返す。設定は読むだけで変えない。
//
// raw mode は echo を切るので、中継していないのに false なら、端末を raw mode にした
// プロセスが復元せずに終わった兆候になる。
// f が端末でなければ ErrNotTTY、この OS で読めなければ ErrEchoUnsupported を返す。
func TerminalEcho(f *os.File) (bool, error) {
	if !isTerminalFile(f) {
		return false, ErrNotTTY
	}
	return echoEnabled(int(f.Fd()))
}
