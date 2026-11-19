package acp

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// How long Close waits for the agent to exit after its input is closed.
const exitGrace = 3 * time.Second

// Process is a Client backed by a running command (a local agent, or ssh to
// a remote one).
type Process struct {
	*Client
	cmd    *exec.Cmd
	stderr *tail
	once   sync.Once
	exited chan struct{}
}

// Start runs cmd and returns a client connected to its stdin and stdout.
// The command's stderr is kept for error messages and is not shown otherwise.
func Start(cmd *exec.Cmd) (*Process, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errTail := &tail{}
	cmd.Stderr = errTail
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{cmd: cmd, stderr: errTail, exited: make(chan struct{})}
	p.Client = New(stdin, stdout, func() string {
		// stderr is complete once the process has been reaped; give it a
		// moment so the error names the cause rather than nothing.
		select {
		case <-p.exited:
		case <-time.After(time.Second):
		}
		return errTail.String()
	})
	go func() {
		// Wait must not run before stdout is drained: it closes the pipe.
		<-p.Client.Done()
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// Close closes the agent's input and waits for it to exit, killing it if it
// does not.
func (p *Process) Close() error {
	p.once.Do(func() {
		_ = p.Client.Close()
		select {
		case <-p.exited:
		case <-time.After(exitGrace):
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
	})
	return nil
}

// tail keeps the last lines a process wrote to stderr.
type tail struct {
	mu    sync.Mutex
	lines []string
	part  string
}

const (
	tailLines = 40
	// tailLineBytes bounds one kept line, and the unfinished line being
	// collected, so a process flooding stderr cannot grow memory.
	tailLineBytes = 1024
)

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for rest := p; len(rest) > 0; {
		line, after, ended := bytes.Cut(rest, []byte{'\n'})
		rest = after
		// Keep the start of an overlong line; the rest is dropped.
		if room := tailLineBytes - len(t.part); room > 0 {
			t.part += string(line[:min(room, len(line))])
		}
		if !ended {
			break
		}
		t.lines = append(t.lines, t.part)
		t.part = ""
		if len(t.lines) > tailLines {
			t.lines = t.lines[len(t.lines)-tailLines:]
		}
	}
	return len(p), nil
}

// String returns the most useful recent lines: routine log lines are dropped
// when anything else was written, because agents log their startup at INFO.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := append([]string(nil), t.lines...)
	if strings.TrimSpace(t.part) != "" {
		lines = append(lines, t.part)
	}
	var notable []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.Contains(line, "[INFO]") && !strings.Contains(line, "[DEBUG]") {
			notable = append(notable, line)
		}
	}
	if len(notable) > 3 {
		notable = notable[len(notable)-3:]
	}
	return strings.Join(notable, " | ")
}

var _ io.Writer = (*tail)(nil)
