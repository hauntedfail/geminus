package proxy

import (
	"errors"
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

var (
	// ErrNoSize は端末のサイズが取れないことを表す（入出力がどちらも端末でない）。
	ErrNoSize = errors.New("proxy: terminal size is not available")
	// ErrNotTTY は端末でない入力を raw mode にしようとしたことを表す。
	ErrNotTTY = errors.New("proxy: input is not a terminal")
)

// Terminal は利用者側の端末。
type Terminal interface {
	// Read は利用者の入力を読む。
	io.Reader
	// Write は利用者の画面へ書く。
	io.Writer
	// IsTTY は入力が対話端末かを返す。false なら Run は raw mode に入らない。
	IsTTY() bool
	// MakeRaw は端末を raw mode にし、元に戻す関数を返す。
	MakeRaw() (restore func() error, err error)
	// Size は端末の桁数と行数を返す。取れなければ ErrNoSize。
	Size() (cols, rows int, err error)
	// WatchResize はサイズが変わるたびに notify を呼ぶ。
	// stop は監視を止め、stop が返った後は notify を呼ばない。
	WatchResize(notify func()) (stop func())
}

// OSTerminal は標準入力と標準出力による端末を返す。
func OSTerminal() Terminal {
	return NewTerminal(os.Stdin, os.Stdout)
}

// NewTerminal は in と out による端末を返す。
//
// *os.File が端末を指していれば TTY として扱う（raw mode、サイズ、リサイズ通知）。
// それ以外（パイプ、ファイル、メモリ上のバッファ）は非 TTY になる。
func NewTerminal(in io.Reader, out io.Writer) Terminal {
	t := &stdTerminal{in: in, out: out}
	if f, ok := in.(*os.File); ok {
		t.inFile = f
	}
	if f, ok := out.(*os.File); ok {
		t.outFile = f
	}
	return t
}

type stdTerminal struct {
	in      io.Reader
	out     io.Writer
	inFile  *os.File // in が *os.File でなければ nil
	outFile *os.File // out が *os.File でなければ nil
}

func (t *stdTerminal) Read(p []byte) (int, error)  { return t.in.Read(p) }
func (t *stdTerminal) Write(p []byte) (int, error) { return t.out.Write(p) }

func isTerminalFile(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

func (t *stdTerminal) IsTTY() bool { return isTerminalFile(t.inFile) }

func (t *stdTerminal) MakeRaw() (func() error, error) {
	if !t.IsTTY() {
		return nil, ErrNotTTY
	}
	fd := int(t.inFile.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	restoreVT := enableVT(t.inFile, t.outFile)

	var once sync.Once
	var restoreErr error
	return func() error {
		once.Do(func() {
			restoreVT()
			restoreErr = term.Restore(fd, state)
		})
		return restoreErr
	}, nil
}

// sizeFile はサイズの問い合わせ先を返す。出力が端末ならそちらを優先する
// （`geminus run < file` でも画面のサイズを使うため）。
func (t *stdTerminal) sizeFile() *os.File {
	for _, f := range []*os.File{t.outFile, t.inFile} {
		if isTerminalFile(f) {
			return f
		}
	}
	return nil
}

func (t *stdTerminal) Size() (int, int, error) {
	f := t.sizeFile()
	if f == nil {
		return 0, 0, ErrNoSize
	}
	cols, rows, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0, 0, err
	}
	return cols, rows, nil
}

func (t *stdTerminal) WatchResize(notify func()) func() {
	if t.sizeFile() == nil {
		return func() {}
	}
	return watchResize(t, notify)
}
