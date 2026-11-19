// Package proxy は子プロセスに PTY を与え、利用者の端末との間でバイト列をそのまま中継する。
//
// 出力は解釈しない。端末（Terminal）と子プロセスの起動（Spawner）はインターフェースなので、
// テストではメモリ上の実装に差し替えられる。
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNoCommand は起動するコマンドが空であることを表す。
	ErrNoCommand = errors.New("proxy: no command given")
	// ErrPTY は PTY を確保できなかったことを表す。パイプへのフォールバックはしない。
	ErrPTY = errors.New("proxy: cannot allocate a pty")
	// ErrStart は PTY は確保できたが、コマンドを起動できなかったことを表す。
	ErrStart = errors.New("proxy: cannot start the command")
)

const (
	// 端末のサイズが取れないとき（パイプ実行など）に子へ渡すサイズ。
	DefaultCols = 80
	DefaultRows = 24

	// eot は Ctrl-D。非 TTY の入力が EOF になったら、子の TTY に1回だけ送る（eofSequence）。
	eot = 0x04

	outputBufSize = 32 << 10
	inputBufSize  = 4 << 10

	// 子の終了後、出力がこの時間だけ途絶えたら残りは無いと判断する。
	defaultDrainIdle = 100 * time.Millisecond
	// 出力が途絶えなくても、子の終了後にこれ以上は待たない。
	drainMax = 2 * time.Second
	// PTY を閉じた後、出力のゴルーチンが止まるのを待つ時間。
	closeGrace = 250 * time.Millisecond
)

// Options は Run の入力。
type Options struct {
	// Argv は起動するコマンド。Argv[0] は PATH から探す。
	Argv []string
	// Dir は子の作業ディレクトリ。空なら現在のディレクトリ。
	Dir string
	// Env は子の環境変数。nil なら geminus の環境をそのまま継承する。
	Env []string
	// Terminal は利用者の端末。nil なら標準入出力。
	Terminal Terminal
	// Record は子の出力の写しを受け取る（raw.log）。nil なら記録しない。
	// 書き込みに失敗したら記録だけを止め、中継は続ける。
	Record io.Writer
	// Spawner は子の起動方法。nil なら go-pty で PTY を確保する。
	Spawner Spawner
	// Signals は子へ転送するシグナルの供給元。nil なら OS のシグナル
	// （Unix は SIGINT / SIGTERM / SIGHUP / SIGQUIT）を受ける。
	// nil の場合、Run は raw mode に入る前から端末を復元するまでシグナルを受け続けるので、
	// その間に届いたシグナルで geminus が終了して端末が raw のまま残ることは無い。
	Signals <-chan os.Signal
	// Logf は中継を止めない程度の異常（記録やリサイズの失敗）を受け取る。nil なら捨てる。
	Logf func(format string, args ...any)
	// DrainIdle は子の終了後に出力の途絶を待つ時間。0 なら既定値。
	DrainIdle time.Duration
}

// Result は中継の結果。
type Result struct {
	// Started は子を起動できたことを表す。Run がエラーを返したとき、子が一度も動かなかった
	// （raw mode に入れない、PTY を確保できない、コマンドを起動できない）のか、
	// 動いた後で中継が壊れたのかを、呼び出し側が区別するために使う。
	Started bool
	// ExitCode は子の終了コード。Unix でシグナルにより終了した場合は 128+シグナル番号。
	ExitCode int
	// OutputBytes は子から読んだバイト数。
	OutputBytes int64
	// RecordErr は Record への最初の書き込みエラー。中継自体は成功している。
	RecordErr error
}

// Run は opts.Argv を PTY 上で起動し、子が終了するまで端末との間を中継する。
//
// 入力が TTY なら raw mode に入り、戻る前に必ず復元する。
// 入力が TTY でなければ raw mode に入らず、EOF で EOT を1回だけ子に送る
// （入力が改行で終わっていなければ、先に改行を1つ補う）。
// 子が終了コード 0 以外で終わってもエラーにはしない（Result.ExitCode を見る）。
// ctx がキャンセルされたら子を kill する。
//
// 端末の Read は中断できないので、入力を読むゴルーチンは Run が返った後も Read の中に残る。
// 標準入力を渡した場合、Run の後に同じプロセスで標準入力を読むと最初の入力を取られる。
// Run はプロセスの最後に1回だけ呼ぶこと。
func Run(ctx context.Context, opts Options) (res Result, err error) {
	if len(opts.Argv) == 0 || opts.Argv[0] == "" {
		return Result{}, ErrNoCommand
	}
	term := opts.Terminal
	if term == nil {
		term = OSTerminal()
	}
	spawner := opts.Spawner
	if spawner == nil {
		spawner = PTYSpawner()
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// シグナルは raw mode に入る前に受け始め、復元の後で手放す（defer は逆順に走る）。
	// 受けていない間に SIGTERM などが届くと、既定動作で終了して端末が raw のまま残る。
	signals := opts.Signals
	if signals == nil {
		ch := make(chan os.Signal, 16)
		signal.Notify(ch, handledSignals()...)
		defer signal.Stop(ch)
		signals = ch
	}

	isTTY := term.IsTTY()
	if isTTY {
		restore, rawErr := term.MakeRaw()
		if rawErr != nil {
			return Result{}, fmt.Errorf("proxy: enter raw mode: %w", rawErr)
		}
		defer func() {
			if restoreErr := restore(); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("proxy: restore terminal: %w", restoreErr))
			}
		}()
	}

	cols, rows, sizeErr := term.Size()
	if sizeErr != nil || cols <= 0 || rows <= 0 {
		cols, rows = DefaultCols, DefaultRows
	}
	child, err := spawner.Spawn(Spec{Argv: opts.Argv, Dir: opts.Dir, Env: opts.Env, Cols: cols, Rows: rows})
	if err != nil {
		return Result{}, err
	}

	r := &relay{term: term, child: child, record: opts.Record, logf: logf}
	r.touch()

	outputDone := make(chan struct{})
	go r.copyOutput(outputDone)
	go r.copyInput(isTTY)
	stopResize := term.WatchResize(r.resize)
	// Size を読んでから監視を始めるまでの間に変わっていたら、ここで追いつく。
	if sizeErr == nil {
		r.syncSize(cols, rows)
	}
	stopSignals := r.forwardSignals(signals)

	waitDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if killErr := child.Kill(); killErr != nil {
				logf("proxy: kill child: %v", killErr)
			}
		case <-waitDone:
		}
	}()

	code, waitErr := child.Wait()
	close(waitDone)
	stopSignals()
	stopResize()

	drainIdle := opts.DrainIdle
	if drainIdle <= 0 {
		drainIdle = defaultDrainIdle
	}
	r.drain(outputDone, drainIdle)
	if closeErr := child.Close(); closeErr != nil {
		logf("proxy: close pty: %v", closeErr)
	}
	select {
	case <-outputDone:
	case <-time.After(closeGrace):
	}

	res, termErr := r.finish(code)
	if waitErr != nil {
		err = errors.Join(err, fmt.Errorf("proxy: wait for child: %w", waitErr))
	}
	if termErr != nil {
		err = errors.Join(err, fmt.Errorf("proxy: write to terminal: %w", termErr))
	}
	return res, err
}

// relay は1回の中継の状態。
type relay struct {
	term   Terminal
	child  Child
	record io.Writer
	logf   func(format string, args ...any)

	// mu は端末と記録先への書き込み、および下の状態を守る。
	mu        sync.Mutex
	stopped   bool
	outBytes  int64
	recordErr error
	termErr   error

	lastOutput atomic.Int64 // UnixNano
}

func (r *relay) touch() { r.lastOutput.Store(time.Now().UnixNano()) }

// copyOutput は子の出力を端末と記録先へ流す。
func (r *relay) copyOutput(done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, outputBufSize)
	for {
		n, err := r.child.Read(buf)
		if n > 0 {
			// 中継を止めた後も読み捨て続ける。読む側がいなくなると、子の書き込みや
			// ConPTY の終了処理が詰まる。
			r.deliver(buf[:n])
		}
		if err != nil {
			// EOF（macOS）、EIO（Linux）、Close 済みのいずれも「出力はここまで」を意味する。
			return
		}
	}
}

// deliver は p を端末へ書き、同じバイト列を記録先へ写す。中継を止めた後は何もしない。
func (r *relay) deliver(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	// 書き込みの前後で時刻を更新し、遅い端末への書き込み中を「出力が途絶えた」と見なさない。
	r.touch()
	defer r.touch()
	r.outBytes += int64(len(p))

	if _, err := r.term.Write(p); err != nil {
		// 利用者に見えないままエージェントを動かし続けない。
		r.termErr = err
		r.stopped = true
		go func() {
			if killErr := r.child.Kill(); killErr != nil {
				r.logf("proxy: kill child: %v", killErr)
			}
		}()
		return
	}
	if r.record != nil && r.recordErr == nil {
		if _, err := r.record.Write(p); err != nil {
			// 記録の失敗で会話を止めない。以後は記録だけを止める。
			r.recordErr = err
			r.logf("proxy: recording stopped: %v", err)
		}
	}
}

// copyInput は端末の入力を子へ流す。
// 標準入力の Read は中断できないので、Run はこのゴルーチンの終了を待たない。
func (r *relay) copyInput(isTTY bool) {
	buf := make([]byte, inputBufSize)
	// 入力が無いうちは行頭にいるので、改行済みとして扱う。
	endsWithNewline := true
	for {
		n, err := r.term.Read(buf)
		if n > 0 {
			if _, werr := r.child.Write(buf[:n]); werr != nil {
				return // 子が終了した
			}
			endsWithNewline = buf[n-1] == '\n'
		}
		if err != nil {
			if errors.Is(err, io.EOF) && !isTTY {
				// パイプ入力の終わりを、子の TTY に EOF として伝える。失敗は子の終了を意味する。
				_, _ = r.child.Write(eofSequence(endsWithNewline))
			}
			return
		}
	}
}

// eofSequence は非 TTY の入力が終わったときに子の TTY へ送るバイト列を返す。
//
// canonical mode の EOT は行頭でだけ EOF になる。行の途中で送ると、溜まっている行を
// 読み手に渡すだけで EOF にはならない（`printf abc | geminus run -- cat` が終わらない）。
// 入力が改行で終わっていなければ、改行を1つ補ってから EOT を送る。
func eofSequence(endsWithNewline bool) []byte {
	if endsWithNewline {
		return []byte{eot}
	}
	return []byte{'\n', eot}
}

// resize は端末の現在のサイズを子の PTY に反映する。
func (r *relay) resize() {
	cols, rows, err := r.term.Size()
	if err != nil {
		r.logf("proxy: read terminal size: %v", err)
		return
	}
	if err := r.child.Resize(cols, rows); err != nil {
		r.logf("proxy: resize pty: %v", err)
	}
}

// syncSize は端末のサイズが spawn 時の cols×rows から変わっていれば子に反映する。
func (r *relay) syncSize(cols, rows int) {
	if nowCols, nowRows, err := r.term.Size(); err == nil && (nowCols != cols || nowRows != rows) {
		r.resize()
	}
}

// forwardSignals は src に届いたシグナルを子へ転送する。
// 返す関数は転送を止め、転送中の呼び出しが終わるまで待つ。
func (r *relay) forwardSignals(src <-chan os.Signal) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case sig := <-src:
				if !forwardable(sig) {
					continue
				}
				if err := r.child.Signal(sig); err != nil {
					r.logf("proxy: forward %v: %v", sig, err)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// drain は子の終了後、残った出力が流れ切るのを待つ。
//
// 通常は PTY の EOF で done が閉じてすぐ戻る。孫プロセスが PTY を握っている場合や
// Windows の ConPTY では EOF が来ないので、出力が idle だけ途絶えた時点で戻る。
func (r *relay) drain(done <-chan struct{}, idle time.Duration) {
	r.touch()
	deadline := time.NewTimer(drainMax)
	defer deadline.Stop()
	tick := time.NewTicker(max(idle/4, time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-deadline.C:
			return
		case <-tick.C:
			if time.Since(time.Unix(0, r.lastOutput.Load())) >= idle {
				return
			}
		}
	}
}

// finish は以後の書き込みを止め、結果をまとめる。
// これ以降、出力のゴルーチンが残っていても端末と記録先には触れない。
func (r *relay) finish(code int) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	return Result{Started: true, ExitCode: code, OutputBytes: r.outBytes, RecordErr: r.recordErr}, r.termErr
}
