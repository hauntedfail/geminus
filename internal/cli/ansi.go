package cli

import (
	"bytes"
	"io"
	"unicode/utf8"
)

// maxPendingLine は、改行が来ないまま溜める1行の上限。超えた分は先に書き出す。
// 全画面 TUI の出力は改行を含まないことがあり、溜め続けるとメモリを使い切る。
const maxPendingLine = 64 << 10

type ansiState int

const (
	ansiGround ansiState = iota
	ansiEscape           // ESC の直後
	ansiEscapeIntermediate
	ansiCSI          // ESC [ ... 終端バイトまで
	ansiOSC          // ESC ] ... BEL か ST まで
	ansiOSCEscape    // OSC の中の ESC（ST の1バイト目かもしれない）
	ansiString       // DCS / SOS / PM / APC ... ST まで
	ansiStringEscape // 文字列の中の ESC
)

// ansiStripper は端末向けの出力から制御シーケンスを取り除き、読める文字だけを書く。
//
// 端末を再現はしない。色やカーソル移動のシーケンスを捨て、行の中だけで
// バックスペースと行頭復帰（CR）を反映する。全画面 TUI の出力はきれいには並ばない。
// シーケンスが Write の境界で切れていてもよい。最後に Close を呼ぶ。
//
// 出力は UTF-8 として扱う。C1 制御文字（U+0080〜U+009F）と、文字の途中でない
// 継続バイトは捨てる。端末によっては 8 ビットの制御シーケンスとして解釈されるため。
// それ以外の符号化（Shift_JIS など）の出力は、そのまま見たければ --raw を使う。
type ansiStripper struct {
	w     io.Writer
	state ansiState
	// line は現在の行のうち、まだ書いていない部分。バックスペースと CR で書き換えるために溜める。
	line []byte
	// pendingCR は CR を見た直後であることを表す。次が LF なら改行、文字なら行を書き直す。
	pendingCR bool
	// heldC2 は 0xC2 を見た直後であることを表す。次のバイトで C1 制御文字かどうかが決まる。
	heldC2 bool
	// contLeft は、今の UTF-8 文字であと何バイトの継続バイトを待っているか。
	contLeft int
	err      error
}

func newANSIStripper(w io.Writer) *ansiStripper {
	return &ansiStripper{w: w}
}

// Write は p を処理する。書き込み先がエラーを返したら、以後は何も書かずそのエラーを返す。
func (s *ansiStripper) Write(p []byte) (int, error) {
	for _, b := range p {
		if s.err != nil {
			return 0, s.err
		}
		s.step(b)
	}
	if s.err != nil {
		return 0, s.err
	}
	return len(p), nil
}

// Close は溜めていた行を書き出す。
func (s *ansiStripper) Close() error {
	s.flushLine()
	return s.err
}

func (s *ansiStripper) step(b byte) {
	switch s.state {
	case ansiGround:
		s.ground(b)
	case ansiEscape:
		s.escape(b)
	case ansiEscapeIntermediate:
		if b < 0x20 || b > 0x2f {
			s.state = ansiGround
		}
	case ansiCSI:
		s.csi(b)
	case ansiOSC:
		switch b {
		case 0x07:
			s.state = ansiGround
		case 0x1b:
			s.state = ansiOSCEscape
		}
	case ansiString:
		if b == 0x1b {
			s.state = ansiStringEscape
		}
	case ansiOSCEscape, ansiStringEscape:
		// ESC \ は ST（文字列の終端）。それ以外は新しいエスケープシーケンスの始まり。
		if b == '\\' {
			s.state = ansiGround
			return
		}
		s.state = ansiEscape
		s.escape(b)
	}
}

func (s *ansiStripper) ground(b byte) {
	if b >= 0x80 {
		s.highByte(b)
		return
	}
	// ASCII が来たら、途中だった UTF-8 文字（壊れている）は打ち切る。
	s.heldC2 = false
	s.contLeft = 0
	switch {
	case b == 0x1b:
		s.state = ansiEscape
	case b == '\n':
		s.pendingCR = false
		s.line = append(s.line, '\n')
		s.flushLine()
	case b == '\r':
		s.pendingCR = true
	case b == '\b':
		s.eraseLastRune()
	case b == '\t' || (b >= 0x20 && b != 0x7f):
		s.put(b)
	}
	// それ以外の制御文字（BEL、NUL、DEL など）は捨てる。
}

// highByte は 0x80 以上のバイト（UTF-8 の2バイト目以降か、先頭バイト）を処理する。
func (s *ansiStripper) highByte(b byte) {
	continuation := b&0xc0 == 0x80
	switch {
	case s.heldC2:
		s.heldC2 = false
		if !continuation {
			// 0xC2 の後に継続バイトが来なかった。0xC2 は捨てて、このバイトから読み直す。
			s.highByte(b)
			return
		}
		if b <= 0x9f {
			return // U+0080〜U+009F: C1 制御文字
		}
		s.put(0xc2)
		s.put(b)
	case continuation:
		if s.contLeft == 0 {
			return // 文字の途中でない継続バイト。8 ビットの C1 制御文字かもしれない。
		}
		s.contLeft--
		s.put(b)
	case b == 0xc2:
		s.heldC2 = true
		s.contLeft = 0
	default:
		s.contLeft = continuationBytes(b)
		s.put(b)
	}
}

// continuationBytes は、UTF-8 の先頭バイト lead に続く継続バイトの数を返す。
func continuationBytes(lead byte) int {
	switch {
	case lead >= 0xc3 && lead <= 0xdf:
		return 1
	case lead >= 0xe0 && lead <= 0xef:
		return 2
	case lead >= 0xf0 && lead <= 0xf4:
		return 3
	default:
		return 0
	}
}

// put は表示する1バイトを現在の行に足す。
func (s *ansiStripper) put(b byte) {
	if s.pendingCR {
		// 行頭に戻ってからの出力は、それまでの行を上書きする（進捗表示など）。
		s.pendingCR = false
		s.line = s.line[:0]
	}
	s.line = append(s.line, b)
	if len(s.line) >= maxPendingLine {
		s.flushLine()
	}
}

func (s *ansiStripper) escape(b byte) {
	switch {
	case b == '[':
		s.state = ansiCSI
	case b == ']':
		s.state = ansiOSC
	case b == 'P' || b == 'X' || b == '^' || b == '_':
		s.state = ansiString
	case b >= 0x20 && b <= 0x2f:
		s.state = ansiEscapeIntermediate
	case b == 0x1b:
		// ESC が続いた。後ろの ESC から読み直す。
	case b < 0x20:
		// シーケンスの途中に入った制御文字（改行など）は、端末と同じく実行する。
		s.state = ansiGround
		s.ground(b)
	default:
		// ESC 7 や ESC = のような2バイトのシーケンス。
		s.state = ansiGround
	}
}

func (s *ansiStripper) csi(b byte) {
	switch {
	case b >= 0x20 && b <= 0x3f:
		// パラメータと中間バイト。
	case b == 0x1b:
		s.state = ansiEscape
	case b < 0x20:
		// 壊れたシーケンス。制御文字として扱い直す。
		s.state = ansiGround
		s.ground(b)
	default:
		s.state = ansiGround
	}
}

// eraseLastRune は現在の行の最後の1文字を消す。既に書き出した分は消せない。
func (s *ansiStripper) eraseLastRune() {
	if len(s.line) == 0 {
		return
	}
	_, size := utf8.DecodeLastRune(s.line)
	s.line = s.line[:len(s.line)-size]
}

func (s *ansiStripper) flushLine() {
	if len(s.line) == 0 || s.err != nil {
		return
	}
	_, s.err = s.w.Write(s.line)
	s.line = s.line[:0]
}

// tailWriter は書き込まれた内容の最後の n 行だけを覚え、Close で w に書く。
// 改行で終わっていない最後の行も1行と数える。
type tailWriter struct {
	w       io.Writer
	n       int
	lines   [][]byte // 改行で終わった行。古い順
	partial []byte   // まだ改行が来ていない行
}

func newTailWriter(w io.Writer, n int) *tailWriter {
	return &tailWriter{w: w, n: n}
}

func (t *tailWriter) Write(p []byte) (int, error) {
	rest := p
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			t.partial = append(t.partial, rest...)
			return len(p), nil
		}
		line := append(t.partial, rest[:i+1]...)
		t.partial = nil
		t.push(line)
		rest = rest[i+1:]
	}
}

func (t *tailWriter) push(line []byte) {
	t.lines = append(t.lines, line)
	if len(t.lines) > t.n {
		// 先頭の要素を手放して、覚えている行数を n に保つ。
		t.lines[0] = nil
		t.lines = t.lines[1:]
	}
}

// Close は覚えている行を w に書く。
func (t *tailWriter) Close() error {
	if len(t.partial) > 0 {
		t.push(t.partial)
		t.partial = nil
	}
	for _, line := range t.lines {
		if _, err := t.w.Write(line); err != nil {
			return err
		}
	}
	t.lines = nil
	return nil
}
