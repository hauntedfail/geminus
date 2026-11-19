//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/session"
)

// outsideSecret is the content of a file outside the session that must never be read.
const outsideSecret = "OUTSIDE-FILE-CONTENT"

// plantSymlink replaces file in the session with a symlink to an external file. Use content
// that would be readable if the link were followed.
func plantSymlink(t *testing.T, s *sandbox, id, file, content string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, sessionsDirName, id, file)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// runWithin runs the command and checks that it does not hang.
func runWithin(t *testing.T, s *sandbox, args ...string) result {
	t.Helper()
	done := make(chan result, 1)
	go func() { done <- s.run("", args...) }()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("geminus %s did not return", strings.Join(args, " "))
		return result{}
	}
}

func expectRefused(t *testing.T, r result) {
	t.Helper()
	if r.code != ExitError || !strings.Contains(r.stderr, "not a regular file") {
		t.Errorf("exit = %d, stderr = %q; want %d and a refusal", r.code, r.stderr, ExitError)
	}
	if strings.Contains(r.stdout, outsideSecret) {
		t.Errorf("stdout shows the file behind the link: %q", r.stdout)
	}
}

// log and show must not display the target of a substituted symlink.
func TestLogAndShowDoNotFollowSymlinks(t *testing.T) {
	t.Run("raw.log", func(t *testing.T) {
		s := newSandbox(t)
		id := s.seed("cmd", nil).ID
		s.seedLog(id, sampleRaw)
		plantSymlink(t, s, id, "raw.log", outsideSecret+"\n")
		expectRefused(t, runWithin(t, s, "log", id))
		expectRefused(t, runWithin(t, s, "log", id, "--raw"))
	})

	t.Run("transcript.jsonl", func(t *testing.T) {
		s := newSandbox(t)
		id := s.seed("cmd", nil).ID
		s.seedLog(id, sampleRaw)
		plantSymlink(t, s, id, "transcript.jsonl", `{"seq":1,"type":"note","text":"`+outsideSecret+`"}`+"\n")
		expectRefused(t, runWithin(t, s, "log", id, "--json"))
	})

	t.Run("session.json", func(t *testing.T) {
		s := newSandbox(t)
		id := s.seed("cmd", nil).ID
		plantSymlink(t, s, id, "session.json", `{"version":1,"id":"`+id+`","title":"`+outsideSecret+`"}`)
		expectRefused(t, runWithin(t, s, "show", id))
		expectRefused(t, runWithin(t, s, "show", id, "--json"))

		// ls displays the remaining sessions and reports the unreadable session on stderr.
		r := runWithin(t, s, "ls", "--all")
		if strings.Contains(r.stdout, outsideSecret) || !strings.Contains(r.stderr, "not a regular file") {
			t.Errorf("ls: stdout = %q, stderr = %q", r.stdout, r.stderr)
		}
		// rm can still remove a session containing a substituted symlink.
		s.ok("", "rm", id)
	})
}

// Opening a FIFO can block waiting for a writer. log must return an error without hanging.
func TestLogDoesNotBlockOnAFIFO(t *testing.T) {
	for _, tt := range []struct {
		file string
		args []string
	}{
		{"raw.log", nil},
		{"transcript.jsonl", []string{"--json"}},
	} {
		t.Run(tt.file, func(t *testing.T) {
			s := newSandbox(t)
			id := s.seed("cmd", nil).ID
			s.seedLog(id, sampleRaw)
			path := filepath.Join(s.home, sessionsDirName, id, tt.file)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			expectRefused(t, runWithin(t, s, append([]string{"log", id}, tt.args...)...))
		})
	}
}

// run --resume must neither read a substituted session.json symlink nor write to its
// target.
func TestResumeRefusesASymlinkedSession(t *testing.T) {
	needShell(t)
	s := newSandbox(t)
	id := s.seed("cmd", nil).ID
	content := `{"version":1,"id":"` + id + `","agent":"cmd","status":"exited"}`
	plantSymlink(t, s, id, "session.json", content)

	r := runWithin(t, s, "run", "--agent", "cmd", "--resume", id, "--", "sh", "-c", "exit 0")
	if r.code != ExitError || !strings.Contains(r.stderr, session.ErrNotRegular.Error()) {
		t.Errorf("exit = %d, stderr = %q; want a refusal", r.code, r.stderr)
	}
}
