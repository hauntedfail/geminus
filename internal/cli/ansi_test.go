package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func stripString(t *testing.T, chunks ...string) string {
	t.Helper()
	var out bytes.Buffer
	s := newANSIStripper(&out)
	for _, chunk := range chunks {
		if _, err := s.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.String()
}

func TestANSIStripper(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "hello\n", "hello\n"},
		{"sgr colors", "\x1b[1;32mok\x1b[0m done\n", "ok done\n"},
		{"crlf becomes lf", "a\r\nb\r\n", "a\nb\n"},
		{"cursor and erase", "\x1b[2J\x1b[H\x1b[?25lhi\x1b[K\n", "hi\n"},
		{"osc title with bel", "\x1b]0;my title\x07prompt$ \n", "prompt$ \n"},
		{"osc with st", "\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\\n", "link\n"},
		{"dcs string", "a\x1bPq#0;2;0;0;0\x1b\\b\n", "ab\n"},
		{"charset selection", "\x1b(Bx\x1b)0y\n", "xy\n"},
		{"two byte escape", "\x1b=\x1b>\x1b7z\x1b8\n", "z\n"},
		{"backspace erases", "abc\b\bX\n", "aX\n"},
		{"macos eot echo", "^D\b\bbye\n", "bye\n"},
		{"backspace at line start", "\b\bx\n", "x\n"},
		{"backspace removes a whole rune", "日本\bX\n", "日X\n"},
		{"lone cr rewrites the line", "10%\r20%\rdone\n", "done\n"},
		{"cr before crlf", "x\r\r\ny\n", "x\ny\n"},
		{"tab is kept", "a\tb\n", "a\tb\n"},
		{"other controls are dropped", "a\x00\x07\x7fb\n", "ab\n"},
		{"utf8 is kept", "\x1b[31m日本語\x1b[0m\n", "日本語\n"},
		{"no trailing newline", "\x1b[1mtail\x1b[0m", "tail"},
		{"unterminated csi at end", "ok\x1b[3", "ok"},
		{"escape restarts inside csi", "\x1b[3\x1b[1mx\n", "x\n"},
		{"control character inside csi", "a\x1b[3\nb\n", "a\nb\n"},
		{"control character right after esc", "a\x1b\nb\n", "a\nb\n"},
		{"trailing cr keeps the line", "abc\r", "abc"},
		{"c1 controls encoded in utf8", "a\u009b31mb\u0090c\u009d\n", "a31mbc\n"},
		{"characters next to the c1 range are kept", "\u00a0\u00e9\u07ff\n", "\u00a0\u00e9\u07ff\n"},
		{"four byte characters are kept", "ok \U0001F600\n", "ok \U0001F600\n"},
		{"stray continuation bytes are dropped", "a\x9b31m\x80b\n", "a31mb\n"},
		{"truncated character before ascii", "a\xc2b\xe3\x81c\n", "ab\xe3\x81c\n"},
		{"c2 followed by another lead byte", "\xc2\u00e9\xc2\xc2\u0085x\n", "\u00e9x\n"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripString(t, tt.in); got != tt.want {
				t.Errorf("strip(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Splitting a sequence across writes must not change the result.
func TestANSIStripperSplitAcrossWrites(t *testing.T) {
	const in = "\x1b[1;32mgreen\x1b[0m\r\n\x1b]0;title\x07日本語\b!\u009b1m\u00e9\r\nend"
	want := stripString(t, in)
	for i := 0; i <= len(in); i++ {
		if got := stripString(t, in[:i], in[i:]); got != want {
			t.Fatalf("split at %d: got %q, want %q", i, got, want)
		}
	}
	var bytewise []string
	for i := 0; i < len(in); i++ {
		bytewise = append(bytewise, in[i:i+1])
	}
	if got := stripString(t, bytewise...); got != want {
		t.Errorf("bytewise: got %q, want %q", got, want)
	}
}

func TestANSIStripperLongLineIsFlushed(t *testing.T) {
	in := strings.Repeat("x", 3*maxPendingLine) + "\n"
	if got := stripString(t, in); got != in {
		t.Errorf("long line changed: got %d bytes, want %d", len(got), len(in))
	}
}

type brokenWriter struct{ err error }

func (w brokenWriter) Write([]byte) (int, error) { return 0, w.err }

func TestANSIStripperReportsWriteError(t *testing.T) {
	want := errors.New("pipe closed")
	s := newANSIStripper(brokenWriter{want})
	if _, err := s.Write([]byte("line\n")); !errors.Is(err, want) {
		t.Errorf("Write error = %v, want %v", err, want)
	}
	if err := s.Close(); !errors.Is(err, want) {
		t.Errorf("Close error = %v, want %v", err, want)
	}
}

func tailString(t *testing.T, n int, chunks ...string) string {
	t.Helper()
	var out bytes.Buffer
	tw := newTailWriter(&out, n)
	for _, chunk := range chunks {
		if _, err := tw.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.String()
}

func TestTailWriter(t *testing.T) {
	tests := []struct {
		name   string
		n      int
		chunks []string
		want   string
	}{
		{"fewer lines than n", 5, []string{"a\nb\n"}, "a\nb\n"},
		{"last two", 2, []string{"a\nb\nc\nd\n"}, "c\nd\n"},
		{"unterminated last line counts", 2, []string{"a\nb\nc"}, "b\nc"},
		{"split across writes", 2, []string{"a\nb", "b\nc", "c\n"}, "bb\ncc\n"},
		{"one line", 1, []string{"a\n\nb\n"}, "b\n"},
		{"blank lines count", 2, []string{"a\n\n\n"}, "\n\n"},
		{"empty input", 3, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tailString(t, tt.n, tt.chunks...); got != tt.want {
				t.Errorf("tail(%d, %q) = %q, want %q", tt.n, tt.chunks, got, tt.want)
			}
		})
	}
}
