package session

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func openTestLog(t *testing.T, store *Store, id string, opts LogOptions) *Log {
	t.Helper()
	log, err := store.OpenLog(id, opts)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

func mustReadEvents(t *testing.T, store *Store, id string) []Event {
	t.Helper()
	events, err := store.ReadEvents(id)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	return events
}

func eventTypes(events []Event) string {
	types := make([]string, 0, len(events))
	for _, e := range events {
		types = append(types, e.Type)
	}
	return strings.Join(types, ",")
}

func TestAppendEventAssignsSeqAndTimestamp(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	log := openTestLog(t, store, meta.ID, LogOptions{Raw: true})

	inputs := []Event{
		StartEvent("claude", []string{"claude", "--session-id", "u"}),
		InputEvent("hello"),
		NoteEvent("resume unsupported"),
		ExitEvent(0),
	}
	var last int64
	for i, in := range inputs {
		got, err := log.AppendEvent(in)
		if err != nil {
			t.Fatalf("AppendEvent #%d: %v", i, err)
		}
		if got.Seq != last+1 {
			t.Errorf("seq = %d, want %d", got.Seq, last+1)
		}
		if got.TS.IsZero() {
			t.Errorf("event #%d has no timestamp", i)
		}
		last = got.Seq
	}

	events := mustReadEvents(t, store, meta.ID)
	if got := eventTypes(events); got != "session.start,input,note,session.exit" {
		t.Fatalf("types = %s", got)
	}
	if events[0].Agent != "claude" || strings.Join(events[0].Argv, " ") != "claude --session-id u" {
		t.Errorf("start event = %+v", events[0])
	}
	if events[1].Text != "hello" || events[2].Text != "resume unsupported" {
		t.Errorf("text events = %+v, %+v", events[1], events[2])
	}
	if events[3].Code == nil || *events[3].Code != 0 {
		t.Errorf("exit event code = %v, want 0", events[3].Code)
	}

	paths, _ := store.Paths(meta.ID)
	assertPerm(t, paths.Transcript, 0o600)
	assertPerm(t, paths.Raw, 0o600)
}

func TestExitEventKeepsZeroCodeInJSON(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	log := openTestLog(t, store, meta.ID, LogOptions{Raw: true})
	if _, err := log.AppendEvent(ExitEvent(0)); err != nil {
		t.Fatal(err)
	}
	paths, _ := store.Paths(meta.ID)
	data, err := os.ReadFile(paths.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"type":"session.exit","code":0`) {
		t.Errorf("transcript = %s", data)
	}
	if strings.Contains(string(data), "offset") || strings.Contains(string(data), "argv") {
		t.Errorf("exit event carries unrelated fields: %s", data)
	}
}

// 出力イベントの offset は、そのイベントを追記した時点の raw.log の長さになる。
func TestAppendEventRecordsRawOffset(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	paths, _ := store.Paths(meta.ID)

	// 以前の実行が残した raw.log がある状態から始める（resume）。
	if err := os.WriteFile(paths.Raw, bytes.Repeat([]byte("p"), 812), 0o600); err != nil {
		t.Fatal(err)
	}
	log := openTestLog(t, store, meta.ID, LogOptions{Raw: true})

	first, err := log.AppendEvent(Event{Type: EventOutput})
	if err != nil {
		t.Fatal(err)
	}
	if first.Offset == nil || *first.Offset != 812 {
		t.Fatalf("offset = %v, want 812", first.Offset)
	}

	if _, err := log.Write(bytes.Repeat([]byte("q"), 100)); err != nil {
		t.Fatal(err)
	}
	second, err := log.AppendEvent(Event{Type: EventOutput})
	if err != nil {
		t.Fatal(err)
	}
	if second.Offset == nil || *second.Offset != 912 {
		t.Fatalf("offset = %v, want 912", second.Offset)
	}
	if got := log.RawBytes(); got != 912 {
		t.Errorf("RawBytes = %d, want 912", got)
	}

	// 出力以外のイベントには offset を付けない。
	note, err := log.AppendEvent(NoteEvent("n"))
	if err != nil {
		t.Fatal(err)
	}
	if note.Offset != nil {
		t.Errorf("note event has offset %d", *note.Offset)
	}
}

// Write で書いたバイト列は、output イベントの offset/bytes で raw.log から正確に取り出せる。
func TestWriteRecordsOutputSpans(t *testing.T) {
	store, clock := newTestStore(t)
	clock.step = 0
	meta := mustCreate(t, store, "x")
	log := openTestLog(t, store, meta.ID, LogOptions{Raw: true, OutputWindow: time.Second})

	write := func(s string) {
		t.Helper()
		n, err := log.Write([]byte(s))
		if err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}

	write("hello ")
	clock.Advance(500 * time.Millisecond)
	write("world") // 同じ窓の中なので1つのイベントにまとまる
	clock.Advance(2 * time.Second)
	write("\x1b[31mred\x1b[0m") // 窓を越えたので別イベント
	if _, err := log.AppendEvent(NoteEvent("between")); err != nil {
		t.Fatal(err)
	}
	write("tail")
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := mustReadEvents(t, store, meta.ID)
	if got := eventTypes(events); got != "output,output,note,output" {
		t.Fatalf("types = %s", got)
	}
	paths, _ := store.Paths(meta.ID)
	raw, err := os.ReadFile(paths.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "hello world\x1b[31mred\x1b[0mtail" {
		t.Fatalf("raw.log = %q", raw)
	}

	wantChunks := []string{"hello world", "\x1b[31mred\x1b[0m", "", "tail"}
	var nextOffset int64
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Errorf("event #%d seq = %d", i, e.Seq)
		}
		if e.Type != EventOutput {
			continue
		}
		if e.Offset == nil || e.Bytes == nil {
			t.Fatalf("output event #%d lacks offset/bytes: %+v", i, e)
		}
		if *e.Offset != nextOffset {
			t.Errorf("event #%d offset = %d, want %d", i, *e.Offset, nextOffset)
		}
		if got := string(raw[*e.Offset : *e.Offset+*e.Bytes]); got != wantChunks[i] {
			t.Errorf("event #%d chunk = %q, want %q", i, got, wantChunks[i])
		}
		nextOffset = *e.Offset + *e.Bytes
	}
	if nextOffset != int64(len(raw)) {
		t.Errorf("events cover %d bytes, raw.log has %d", nextOffset, len(raw))
	}
}

func TestSeqContinuesAcrossReopen(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")

	log, err := store.OpenLog(meta.ID, LogOptions{Raw: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := log.AppendEvent(NoteEvent("a")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// クラッシュで途中まで書かれた行が残っていても、続きの seq と行の区切りを保つ。
	paths, _ := store.Paths(meta.ID)
	f, err := os.OpenFile(paths.Transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":5,"ts":"2026-11-1`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	reopened := openTestLog(t, store, meta.ID, LogOptions{Raw: true})
	got, err := reopened.AppendEvent(NoteEvent("b"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 5 { // note×3 + output×1 の次
		t.Errorf("seq after reopen = %d, want 5", got.Seq)
	}
	if reopened.RawBytes() != 3 {
		t.Errorf("RawBytes after reopen = %d, want 3", reopened.RawBytes())
	}

	events, err := store.ReadEvents(meta.ID)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadEvents err = %v, want ErrCorrupt for the torn line", err)
	}
	if len(events) != 5 || events[4].Text != "b" {
		t.Errorf("events = %+v, want 5 valid events ending with note b", events)
	}
}

func TestRawLogLimit(t *testing.T) {
	store, clock := newTestStore(t)
	clock.step = 0 // 2回の書き込みを同じ出力窓に入れる
	meta := mustCreate(t, store, "x")
	log := openTestLog(t, store, meta.ID, LogOptions{Raw: true, MaxRawBytes: 10})

	for _, chunk := range []string{"12345678", "abcdefgh", "dropped"} {
		n, err := log.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil (recording stops, relay continues)", chunk, n, err, len(chunk))
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	paths, _ := store.Paths(meta.ID)
	raw, err := os.ReadFile(paths.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "12345678ab" {
		t.Errorf("raw.log = %q, want the first 10 bytes", raw)
	}
	if log.RawBytes() != 10 {
		t.Errorf("RawBytes = %d, want 10", log.RawBytes())
	}

	events := mustReadEvents(t, store, meta.ID)
	if got := eventTypes(events); got != "output,note" {
		t.Fatalf("types = %s, want output,note (a single note)", got)
	}
	if *events[0].Offset != 0 || *events[0].Bytes != 10 {
		t.Errorf("output event = offset %d bytes %d, want 0/10", *events[0].Offset, *events[0].Bytes)
	}
	if !strings.Contains(events[1].Text, "raw.log") {
		t.Errorf("note = %q, want it to mention raw.log", events[1].Text)
	}
}

func TestDefaultRawLimitIs64MiB(t *testing.T) {
	if DefaultMaxRawBytes != 64<<20 {
		t.Errorf("DefaultMaxRawBytes = %d, want %d", DefaultMaxRawBytes, 64<<20)
	}
}

// --no-record: raw.log を作らず、出力イベントも残さない。それ以外のイベントは残す。
func TestLogWithoutRaw(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	log := openTestLog(t, store, meta.ID, LogOptions{Raw: false})

	if n, err := log.Write([]byte("secret output")); err != nil || n != 13 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if _, err := log.AppendEvent(ExitEvent(3)); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	paths, _ := store.Paths(meta.ID)
	if _, err := os.Stat(paths.Raw); !os.IsNotExist(err) {
		t.Errorf("raw.log exists with Raw=false (stat err = %v)", err)
	}
	if got := eventTypes(mustReadEvents(t, store, meta.ID)); got != "session.exit" {
		t.Errorf("types = %s, want session.exit", got)
	}
}

func TestLogAfterClose(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	log, err := store.OpenLog(meta.ID, LogOptions{Raw: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := log.AppendEvent(NoteEvent("late")); !errors.Is(err, ErrClosed) {
		t.Errorf("AppendEvent after Close err = %v, want ErrClosed", err)
	}
	if _, err := log.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close err = %v, want ErrClosed", err)
	}
}

func TestOpenLogMissingSession(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.OpenLog("20200101-000000-0000", LogOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := store.ReadEvents("20200101-000000-0000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadEvents err = %v, want ErrNotFound", err)
	}
	if _, err := store.OpenRaw("20200101-000000-0000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenRaw err = %v, want ErrNotFound", err)
	}
}

func TestReadEventsWithoutTranscript(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	events, err := store.ReadEvents(meta.ID)
	if err != nil || len(events) != 0 {
		t.Errorf("ReadEvents = %v, %v; want empty, nil", events, err)
	}
	if _, err := store.OpenRaw(meta.ID); !errors.Is(err, ErrNoRawLog) {
		t.Errorf("OpenRaw err = %v, want ErrNoRawLog", err)
	}
}

func TestReadEventsHandlesLongLines(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	log := openTestLog(t, store, meta.ID, LogOptions{})
	long := strings.Repeat("あ", 100_000) // bufio.Scanner の既定上限を超える長さ
	if _, err := log.AppendEvent(InputEvent(long)); err != nil {
		t.Fatal(err)
	}
	events := mustReadEvents(t, store, meta.ID)
	if len(events) != 1 || events[0].Text != long {
		t.Errorf("long input event did not round-trip (got %d events)", len(events))
	}
}

func TestOpenRaw(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	log := openTestLog(t, store, meta.ID, LogOptions{Raw: true})
	if _, err := log.Write([]byte("raw bytes")); err != nil {
		t.Fatal(err)
	}
	f, err := store.OpenRaw(meta.ID)
	if err != nil {
		t.Fatalf("OpenRaw: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 32)
	n, _ := f.Read(buf)
	if string(buf[:n]) != "raw bytes" {
		t.Errorf("raw = %q", buf[:n])
	}
}

func TestProcessAlive(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Error("ProcessAlive(self) = false")
	}
	for _, pid := range []int{0, -1} {
		if ProcessAlive(pid) {
			t.Errorf("ProcessAlive(%d) = true", pid)
		}
	}

	// 終了して回収済みの子プロセスは生きていない。
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper process: %v", err)
	}
	if ProcessAlive(cmd.Process.Pid) {
		t.Errorf("ProcessAlive(%d) = true for an exited process", cmd.Process.Pid)
	}
}

func TestMetaActive(t *testing.T) {
	tests := []struct {
		name string
		meta Meta
		want bool
	}{
		{"running with live pid", Meta{Status: StatusRunning, PID: os.Getpid()}, true},
		{"running without pid", Meta{Status: StatusRunning}, false},
		{"exited with live pid", Meta{Status: StatusExited, PID: os.Getpid()}, false},
		{"created", Meta{Status: StatusCreated, PID: os.Getpid()}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.meta.Active(); got != tt.want {
				t.Errorf("Active() = %v, want %v", got, tt.want)
			}
		})
	}
}

// raw.log や transcript.jsonl がシンボリックリンクにすり替わっていたら、辿らずに拒否する。
func TestOpenLogRejectsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	for _, name := range []string{"raw.log", "transcript.jsonl"} {
		t.Run(name, func(t *testing.T) {
			store, _ := newTestStore(t)
			meta := mustCreate(t, store, "x")
			paths, _ := store.Paths(meta.ID)
			target := filepath.Join(t.TempDir(), "outside.txt")
			if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(paths.Dir, name)); err != nil {
				t.Fatal(err)
			}

			if log, err := store.OpenLog(meta.ID, LogOptions{Raw: true}); err == nil {
				log.Close()
				t.Fatal("OpenLog followed a symlink")
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "keep" {
				t.Errorf("target = %q, %v; want it untouched", data, err)
			}
			assertPerm(t, target, 0o644)
		})
	}
}
