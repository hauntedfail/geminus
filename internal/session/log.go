package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

const (
	// DefaultMaxRawBytes は raw.log の既定の上限。超えたら記録だけを止める。
	DefaultMaxRawBytes int64 = 64 << 20
	// DefaultOutputWindow は連続した出力を1つの output イベントにまとめる時間幅。
	// TUI は毎秒何十回も書くので、読み出しごとにイベントを作ると transcript が膨らむ。
	DefaultOutputWindow = time.Second
)

var (
	// ErrClosed は Close 済みの Log を使ったことを表す。
	ErrClosed = errors.New("session log is closed")
	// ErrNoRawLog はセッションに raw.log が無いことを表す（--no-record など）。
	ErrNoRawLog = errors.New("session has no raw.log")
)

// LogOptions は OpenLog の設定。
type LogOptions struct {
	// Raw が true なら Write されたバイト列を raw.log に追記する。false なら捨てる（--no-record）。
	Raw bool
	// MaxRawBytes は raw.log の上限。0 以下なら DefaultMaxRawBytes。
	MaxRawBytes int64
	// OutputWindow は出力をまとめる時間幅。0 なら DefaultOutputWindow、負なら Write ごとに1イベント。
	OutputWindow time.Duration
}

// Log は1セッションの transcript.jsonl と raw.log への追記を受け持つ。
//
// io.Writer として proxy の記録先に渡せる。Write と AppendEvent は別々のゴルーチンから呼んでよい。
type Log struct {
	mu         sync.Mutex
	now        func() time.Time
	transcript *os.File
	raw        *os.File // 記録しない場合は nil
	seq        int64
	rawSize    int64
	maxRaw     int64
	window     time.Duration
	pending    outputSpan
	capped     bool // 上限に達して記録を止めた
	tornTail   bool // 既存 transcript の末尾に改行が無い
	closed     bool
}

// outputSpan は、まだ output イベントにしていない raw.log の範囲。
type outputSpan struct {
	offset int64
	size   int64
	start  time.Time
}

// OpenLog は id のセッションに追記するための Log を開く。
// seq と raw.log のオフセットは既存ファイルの続きから始まる（resume）。
func (s *Store) OpenLog(id string, opts LogOptions) (*Log, error) {
	paths, err := s.existingPaths(id)
	if err != nil {
		return nil, err
	}
	state, err := readTranscriptState(paths.Transcript)
	if err != nil {
		return nil, fmt.Errorf("session %s: read transcript: %w", id, err)
	}
	rawSize, err := fileSize(paths.Raw)
	if err != nil {
		return nil, fmt.Errorf("session %s: stat raw.log: %w", id, err)
	}

	transcript, err := openAppend(paths.Transcript)
	if err != nil {
		return nil, fmt.Errorf("session %s: open transcript: %w", id, err)
	}
	log := &Log{
		now:        s.timestamp,
		transcript: transcript,
		seq:        state.lastSeq,
		rawSize:    rawSize,
		maxRaw:     opts.MaxRawBytes,
		window:     opts.OutputWindow,
		tornTail:   state.tornTail,
	}
	if log.maxRaw <= 0 {
		log.maxRaw = DefaultMaxRawBytes
	}
	if log.window == 0 {
		log.window = DefaultOutputWindow
	}
	if opts.Raw {
		raw, err := openAppend(paths.Raw)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("session %s: open raw.log: %w", id, err), transcript.Close())
		}
		log.raw = raw
	}
	return log, nil
}

// OpenRaw は id の raw.log を読み取り用に開く。無ければ ErrNoRawLog、
// 通常ファイルでなければ（シンボリックリンク、FIFO など）ErrNotRegular を返す。
func (s *Store) OpenRaw(id string) (*os.File, error) {
	paths, err := s.existingPaths(id)
	if err != nil {
		return nil, err
	}
	f, err := openRegular(paths.Raw)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("session %s: %w", id, ErrNoRawLog)
		}
		return nil, fmt.Errorf("session %s: open raw.log: %w", id, err)
	}
	return f, nil
}

// AppendEvent は event に seq と（未設定なら）時刻を振って transcript.jsonl に追記し、
// 書いた内容を返す。
//
// Type が output で Offset が nil の場合は、raw.log の現在の長さを Offset に入れる。
func (l *Log) AppendEvent(event Event) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return Event{}, ErrClosed
	}
	// 先に溜めていた出力を書き出し、イベントの順序を実際の順序と合わせる。
	if err := l.flushPending(); err != nil {
		return Event{}, err
	}
	if event.Type == EventOutput && event.Offset == nil {
		offset := l.rawSize
		event.Offset = &offset
	}
	return l.appendLocked(event)
}

// Write は p を raw.log に追記する。output イベントは OutputWindow ごとにまとめて書く。
//
// 上限に達したら記録だけを止め、note イベントを1つ残す。その場合もエラーは返さず
// len(p) を返すので、中継は続く。記録しない設定（Raw=false）では何も書かない。
func (l *Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, ErrClosed
	}
	if l.raw == nil || l.capped || len(p) == 0 {
		return len(p), nil
	}

	now := l.now()
	if l.pending.size > 0 && (l.window < 0 || now.Sub(l.pending.start) >= l.window) {
		if err := l.flushPending(); err != nil {
			return 0, err
		}
	}

	chunk := p
	if room := l.maxRaw - l.rawSize; int64(len(chunk)) > room {
		chunk = p[:max(room, 0)]
	}
	if len(chunk) > 0 {
		n, err := l.raw.Write(chunk)
		l.extendPending(int64(n), now)
		if err != nil {
			return n, fmt.Errorf("write raw.log: %w", err)
		}
	}
	if len(chunk) < len(p) {
		if err := l.stopRecording(); err != nil {
			return len(chunk), err
		}
	}
	return len(p), nil
}

// RawBytes は raw.log の現在の長さを返す。
func (l *Log) RawBytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rawSize
}

// Close は溜めていた出力イベントを書き出してファイルを閉じる。2回目以降は何もしない。
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	errs := []error{l.flushPending()}
	l.closed = true
	if l.raw != nil {
		errs = append(errs, l.raw.Close())
	}
	errs = append(errs, l.transcript.Close())
	return errors.Join(errs...)
}

func (l *Log) extendPending(n int64, now time.Time) {
	if n <= 0 {
		return
	}
	if l.pending.size == 0 {
		l.pending = outputSpan{offset: l.rawSize, start: now}
	}
	l.pending.size += n
	l.rawSize += n
}

func (l *Log) flushPending() error {
	if l.pending.size == 0 {
		return nil
	}
	span := l.pending
	if _, err := l.appendLocked(outputEvent(span.offset, span.size, span.start)); err != nil {
		return err
	}
	l.pending = outputSpan{}
	return nil
}

func (l *Log) stopRecording() error {
	l.capped = true
	if err := l.flushPending(); err != nil {
		return err
	}
	note := NoteEvent(fmt.Sprintf("raw.log reached the %d byte limit; recording stopped", l.maxRaw))
	_, err := l.appendLocked(note)
	return err
}

func (l *Log) appendLocked(event Event) (Event, error) {
	event.Seq = l.seq + 1
	if event.TS.IsZero() {
		event.TS = l.now()
	}
	line, err := json.Marshal(event)
	if err != nil {
		return Event{}, fmt.Errorf("encode event: %w", err)
	}
	line = append(line, '\n')
	if l.tornTail {
		// 途中で切れた前の行と混ざらないよう、改行で区切ってから書く。
		line = append([]byte{'\n'}, line...)
	}
	// 1行を1回の write で書く。O_APPEND なので行の途中に他の書き込みが挟まらない。
	if _, err := l.transcript.Write(line); err != nil {
		// 行の途中まで書けた可能性がある。次の行は改行で区切ってから書く。
		l.tornTail = true
		return Event{}, fmt.Errorf("append transcript: %w", err)
	}
	l.tornTail = false
	l.seq = event.Seq
	return event, nil
}

func openAppend(path string) (*os.File, error) {
	// シンボリックリンクなどを辿って、セッションの外のファイルに書いたり chmod したりしない。
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, notRegular(path, info.Mode())
	}
	return openAppendChecked(path)
}

// openAppendChecked は path を追記用に開く（無ければ作る）。
// Lstat の後でファイルがすり替わった場合に備え、Unix では O_NOFOLLOW と O_NONBLOCK を付け、
// 開いたものが通常ファイルであることを確かめてから権限を揃える。
func openAppendChecked(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|appendOpenFlags, filePerm)
	if err != nil {
		return nil, symlinkError(path, err)
	}
	if err := requireRegular(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	// 既存ファイルの権限が緩んでいても 0600 に戻す。
	if err := f.Chmod(filePerm); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// fileSize は通常ファイル path の長さを返す。無ければ 0。
func fileSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, notRegular(path, info.Mode())
	}
	return info.Size(), nil
}
