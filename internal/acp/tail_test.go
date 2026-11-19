package acp

import (
	"strings"
	"testing"
)

// A process that floods stderr must not grow geminus's memory without bound.
func TestTailStaysBoundedOnEndlessLine(t *testing.T) {
	var tl tail
	chunk := []byte(strings.Repeat("x", 64<<10))
	for i := 0; i < 64; i++ { // 4 MiB with no newline
		tl.Write(chunk)
	}
	if len(tl.part) > tailLineBytes {
		t.Errorf("unfinished line holds %d bytes, want at most %d", len(tl.part), tailLineBytes)
	}
	if got := len(tl.String()); got > 3*(tailLineBytes+8) {
		t.Errorf("String is %d bytes", got)
	}
}

func TestTailCapsLongLinesAndKeepsTheLastOnes(t *testing.T) {
	var tl tail
	tl.Write([]byte(strings.Repeat("y", 1<<20) + "\n"))
	for i := 0; i < 200; i++ {
		tl.Write([]byte("routine [INFO] line\n"))
	}
	tl.Write([]byte("ssh: connect to host example port 22: Connection refused\n"))
	if len(tl.lines) > tailLines {
		t.Errorf("kept %d lines, want at most %d", len(tl.lines), tailLines)
	}
	for _, line := range tl.lines {
		if len(line) > tailLineBytes {
			t.Fatalf("kept a %d-byte line", len(line))
		}
	}
	if got := tl.String(); got != "ssh: connect to host example port 22: Connection refused" {
		t.Errorf("String = %q", got)
	}
}
