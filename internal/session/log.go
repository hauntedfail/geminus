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
	// DefaultMaxRawBytes is the default size limit for raw.log. Reaching the limit stops only the
	// recording.
	DefaultMaxRawBytes int64 = 64 << 20
	// DefaultOutputWindow is the interval over which consecutive writes are combined into one output
	// event. A TUI may write dozens of times per second; an event per read would inflate the
	// transcript.
	DefaultOutputWindow = time.Second
)

var (
	// ErrClosed means the Log has already been closed.
	ErrClosed = errors.New("session log is closed")
	// ErrNoRawLog means the session has no raw.log, for example when recording was disabled with
	// --no-record.
	ErrNoRawLog = errors.New("session has no raw.log")
)

// LogOptions configures OpenLog.
type LogOptions struct {
	// Raw enables appending bytes passed to Write to raw.log. When false (--no-record), those bytes
	// are discarded.
	Raw bool
	// MaxRawBytes limits the size of raw.log. Nonpositive values use DefaultMaxRawBytes.
	MaxRawBytes int64
	// OutputWindow is the interval for grouping output. Zero uses DefaultOutputWindow; a negative
	// value produces one event per Write.
	OutputWindow time.Duration
}

// Log appends to transcript.jsonl and raw.log for one session.
//
// A Log implements io.Writer for use as the proxy's recording destination. Write and AppendEvent
// may be called from separate goroutines.
type Log struct {
	mu         sync.Mutex
	now        func() time.Time
	transcript *os.File
	raw        *os.File // nil when recording is disabled
	seq        int64
	rawSize    int64
	maxRaw     int64
	window     time.Duration
	pending    outputSpan
	capped     bool // recording stopped at the size limit
	tornTail   bool // the existing transcript lacks a trailing newline
	closed     bool
}

// outputSpan is the range in raw.log not yet recorded in an output event.
type outputSpan struct {
	offset int64
	size   int64
	start  time.Time
}

// OpenLog opens a Log for appending to session id. On resume, sequence numbers and raw.log offsets
// continue from the existing files.
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

// OpenRaw opens the session's raw.log for reading. It returns ErrNoRawLog if the file is missing,
// or ErrNotRegular for a symlink, FIFO or other nonregular file.
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

// AppendEvent assigns a sequence number and, if missing, a timestamp to event, appends it to
// transcript.jsonl and returns the event it wrote.
//
// For an output event with a nil Offset, use the current length of raw.log.
func (l *Log) AppendEvent(event Event) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return Event{}, ErrClosed
	}
	// Flush pending output first to preserve event order.
	if err := l.flushPending(); err != nil {
		return Event{}, err
	}
	if event.Type == EventOutput && event.Offset == nil {
		offset := l.rawSize
		event.Offset = &offset
	}
	return l.appendLocked(event)
}

// Write appends p to raw.log, grouping output events by OutputWindow.
//
// At the size limit, stop recording and add a single note event. Return len(p) without an error so
// the relay continues. With Raw set to false, nothing is written.
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

// RawBytes returns the current length of raw.log.
func (l *Log) RawBytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rawSize
}

// Close flushes pending output events and closes the files. Subsequent calls do nothing.
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
		// Separate the new record from any partial preceding line with a newline.
		line = append([]byte{'\n'}, line...)
	}
	// Write each line in a single write so O_APPEND prevents other writes from interleaving within the
	// line.
	if _, err := l.transcript.Write(line); err != nil {
		// The write may have left a partial line. Insert a newline before the next record.
		l.tornTail = true
		return Event{}, fmt.Errorf("append transcript: %w", err)
	}
	l.tornTail = false
	l.seq = event.Seq
	return event, nil
}

func openAppend(path string) (*os.File, error) {
	// Do not follow symlinks and inadvertently write to or chmod files outside the session.
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, notRegular(path, info.Mode())
	}
	return openAppendChecked(path)
}

// openAppendChecked opens path for appending, creating it if necessary. In case the file was
// replaced after Lstat, use O_NOFOLLOW and O_NONBLOCK on Unix and verify the open file is regular
// before setting its permissions.
func openAppendChecked(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|appendOpenFlags, filePerm)
	if err != nil {
		return nil, symlinkError(path, err)
	}
	if err := requireRegular(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	// Restore mode 0600 even if the existing file had looser permissions.
	if err := f.Chmod(filePerm); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// fileSize returns the length of the regular file at path, or zero if it does not exist.
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
