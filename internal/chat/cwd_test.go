package chat

// The directory on the persona's computer that a conversation runs in.

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/wire"
)

func TestTheDirectoryCanBeChosenUntilTheConversationStarts(t *testing.T) {
	w := newWorld(t)
	w.homes = map[string]string{"sibyl": "/home/sibyl"}
	w.card("sibyl", now.Add(-40*time.Second), "hermes")
	w.talk([]string{"persona", "sibyl"},
		// None chosen: the bridge's own, which it reported.
		[2]string{"/cwd", "directory: /home/sibyl (default) on sibyl  (/cwd DIR sets another"},
		[2]string{"/cwd /srv/app", "directory: /srv/app on sibyl  (for the conversation the next prompt starts"},
		[2]string{"/cwd ~/my api", "directory: ~/my api on sibyl"},
		[2]string{"/status", "new conversation  in ~/my api"},
		[2]string{"build", "echo: build"},
		// The conversation exists now and stays where it started.
		[2]string{"/cwd /etc", "this conversation runs in ~/my api on sibyl and stays there; /new DIR starts another one in DIR"},
		[2]string{"/cwd", "directory: ~/my api on sibyl  (fixed for this conversation"},
		[2]string{"second", "echo: second"},
		// A new conversation can be put anywhere again.
		[2]string{"/new", "new conversation  in ~/my api"},
		[2]string{"/cwd default", "directory: /home/sibyl (default) on sibyl"},
		[2]string{"/status", "new conversation  in /home/sibyl (default)"},
		[2]string{"third", "echo: third"})
	w.expectNot("in /etc")
	sessions := w.srv.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("sessions = %+v", sessions)
	}
	var dirs []string
	for _, s := range sessions {
		info, _ := s.Metadata[wire.Kind].(map[string]any)
		dir, _ := info["cwd"].(string)
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	// The second conversation names no directory: the bridge uses its own.
	if dirs[0] != "" || dirs[1] != "~/my api" {
		t.Errorf("directories of the conversations = %q", dirs)
	}
	// Every prompt was signed for the directory of its conversation: the
	// bridge ran all three.
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.ran) != 3 {
		t.Errorf("ran = %q", w.ran)
	}
}

func TestADirectoryIsNamedEvenWhenTheBridgeDoesNotSayWhichItUses(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "chrome", "--harness", "claude"},
		[2]string{"/cwd", "directory: chrome's default directory on chrome"},
		[2]string{"/status", "new conversation  in chrome's default directory"},
		[2]string{"/cwd /tmp/a\x1b[2Jb", "a directory name cannot have line breaks, tabs or control characters"},
		[2]string{"/new /tmp/a\x07b", "a directory name cannot have"},
		[2]string{"/status", "new conversation  in chrome's default directory"})
	w.expectNot("in /tmp")

	// Without a persona there is no computer to have a directory on.
	w.newApp()
	w.app.Out = NewPrinter(w.screen)
	if err := w.app.chdir("/srv"); err == nil || !strings.Contains(err.Error(), "pick a persona first") {
		t.Errorf("no persona: %v", err)
	}
	if code := w.app.Main([]string{"persona", "chrome", "--cwd", "/tmp/a\x07b"}, strings.NewReader(""), w.errs); code != 1 ||
		!strings.Contains(w.errs.String(), "a directory name cannot have") {
		t.Errorf("--cwd with a control character: exit %d, %s", code, w.errs)
	}
}
