package chat

// "!cd" and "!pwd": moving around the persona's computer from the prompt.

import (
	"strings"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/wire"
)

func TestBangCdMovesFromTheDirectoryChosenSoFar(t *testing.T) {
	w := newWorld(t)
	w.homes = map[string]string{"sibyl": "/home/sibyl"}
	w.card("sibyl", now.Add(-40*time.Second), "hermes")
	w.talk([]string{"persona", "sibyl"},
		// Relative to where things stand, not to the bridge's own each time.
		[2]string{"!cd projekt", "directory: /home/sibyl/projekt on sibyl  (for the conversation the next prompt starts"},
		[2]string{"!cd go/geminus", "directory: /home/sibyl/projekt/go/geminus on sibyl"},
		[2]string{"!cd ../../../..", "directory: /home on sibyl"},
		// "-" is the one before; proven by where the next step lands.
		[2]string{"!cd -", ""},
		[2]string{"!cd internal/", "directory: /home/sibyl/projekt/go/geminus/internal on sibyl"},
		[2]string{"!cd ~/src", "directory: ~/src on sibyl"},
		[2]string{"!cd ..", "directory: ~ on sibyl"},
		[2]string{`!cd "my api"`, "directory: ~/my api on sibyl"},
		[2]string{"!cd /srv/./app//", "directory: /srv/app on sibyl"},
		[2]string{"!cd /tmp/a\x1b[2Jb", "a directory name cannot have line breaks, tabs or control characters"},
		// Alone: back to the bridge's own. /cwd text is read from there too.
		[2]string{"!cd", "directory: /home/sibyl (default) on sibyl"},
		[2]string{"/cwd work", "directory: work on sibyl"},
		[2]string{"!cd api", "directory: /home/sibyl/work/api on sibyl"},
		[2]string{"build", "echo: build"},
		// The conversation exists now and stays where it started.
		[2]string{"!cd /etc", "this conversation runs in /home/sibyl/work/api on sibyl and stays there; /new DIR starts another one in DIR"},
		[2]string{"!pwd", "directory: /home/sibyl/work/api on sibyl  (fixed for this conversation"},
		// Only !cd and !pwd are geminus's.
		[2]string{"!important: keep it", "echo: !important: keep it"},
		// A new conversation can be moved again.
		[2]string{"/new", "new conversation  in /home/sibyl/work/api"},
		[2]string{"!cd ../../other", "directory: /home/sibyl/other on sibyl"})
	w.expectNot("in /etc", "echo: !cd", "echo: !pwd")
	sessions := w.srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %+v", sessions)
	}
	info, _ := sessions[0].Metadata[wire.Kind].(map[string]any)
	if dir, _ := info["cwd"].(string); dir != "/home/sibyl/work/api" {
		t.Errorf("the conversation runs in %q", dir)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.ran) != 2 {
		t.Errorf("ran = %q", w.ran)
	}
}

func TestBangCdGoingBackToWhereTheBridgeStartsIsTheDefault(t *testing.T) {
	w := newWorld(t)
	w.homes = map[string]string{"sibyl": "/home/sibyl/"}
	w.card("sibyl", now.Add(-40*time.Second), "hermes")
	w.talk([]string{"persona", "sibyl"},
		[2]string{"!cd -", "no earlier directory to go back to"},
		[2]string{"!cd ..", "directory: /home on sibyl"},
		[2]string{"!cd sibyl", "directory: /home/sibyl/ (default) on sibyl"},
		[2]string{"!pwd now", "usage: !pwd"})
	if w.app.cwd != "" {
		t.Errorf("cwd = %q", w.app.cwd)
	}
}

func TestBangCdWithoutAReportedDirectoryStaysRelativeToTheBridge(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "chrome", "--harness", "claude"},
		[2]string{"!cd projekt", "directory: projekt on chrome"},
		[2]string{"!cd sub", "directory: projekt/sub on chrome"},
		[2]string{"!cd ../../..", "directory: .. on chrome"},
		[2]string{"!cd -", ""},
		[2]string{"!cd x", "directory: projekt/sub/x on chrome"},
		[2]string{"!cd ../../..", "directory: chrome's default directory on chrome"})
	if w.app.cwd != "" {
		t.Errorf("cwd = %q", w.app.cwd)
	}

	// Without a persona there is no computer to move around on.
	w.newApp()
	w.app.Out = NewPrinter(w.screen)
	for _, line := range []string{"!cd /srv", "!pwd"} {
		if err := w.app.handle(line); err == nil || !strings.Contains(err.Error(), "pick a persona first") {
			t.Errorf("%s with no persona: %v", line, err)
		}
	}
}

func TestBangCdOnAWindowsComputer(t *testing.T) {
	w := newWorld(t)
	w.homes = map[string]string{"mirai": `C:\Users\h.ota`}
	w.card("mirai", now.Add(-5*time.Second), "hermes")
	w.talk([]string{"persona", "mirai"},
		[2]string{`!cd projects\miracle`, `directory: C:\Users\h.ota\projects\miracle on mirai`},
		[2]string{"!cd ../web/src", `directory: C:\Users\h.ota\projects\web\src on mirai`},
		[2]string{`!cd ..\..\..\..\..\..`, `directory: C:\ on mirai`},
		[2]string{"!cd users/H.OTA", `directory: C:\Users\h.ota (default) on mirai`},
		[2]string{"!cd D:/work", `directory: D:\work on mirai`},
		[2]string{`!cd ~\Desktop`, `directory: ~\Desktop on mirai`})
}

func TestRemotePaths(t *testing.T) {
	for _, c := range []struct {
		base, dir string
		windows   bool
		want      string
	}{
		{"/home/me", "projekt", false, "/home/me/projekt"},
		{"/home/me", "", false, "/home/me"},
		{"/home/me", ".", false, "/home/me"},
		{"/home/me", "..", false, "/home"},
		{"/home/me", "../../../..", false, "/"},
		{"/home/me", "/srv//app/", false, "/srv/app"},
		{"/home/me", "~", false, "~"},
		{"/home/me", "~sibyl/x/..", false, "~sibyl"},
		{"~/a", "../..", false, "~/.."},
		{"~/a", "b/./c", false, "~/a/b/c"},
		{"", "a/../..", false, ".."},
		{"", "a/..", false, ""},
		{"a", "b", false, "a/b"},
		// A backslash is part of a name anywhere but on Windows.
		{"/home/me", `a\b`, false, `/home/me/a\b`},
		{"/home/me", "日本語/api", false, "/home/me/日本語/api"},
		{`C:\Users\me`, "projekt", true, `C:\Users\me\projekt`},
		{`C:\Users\me`, `..\..\..`, true, `C:\`},
		{`C:\Users\me`, `\Windows`, true, `\Windows`},
		{`C:\Users\me`, "D:", true, `D:\`},
		{`C:/Users/me`, "a/b", true, `C:\Users\me\a\b`},
		{`\\nas\share\x`, `..\..\y`, true, `\\nas\share\y`},
		{`~\a`, "..", true, "~"},
	} {
		if got := joinRemote(c.base, c.dir, c.windows); got != c.want {
			t.Errorf("joinRemote(%q, %q, windows=%v) = %q, want %q", c.base, c.dir, c.windows, got, c.want)
		}
	}
	for path, want := range map[string]bool{`C:\Users`: true, "c:/x": true, "D:": true, `\\nas\share`: true,
		"/home/me": false, "~/x": false, "a:b": false, "": false, "projekt": false} {
		if got := windowsPath("", path); got != want {
			t.Errorf("windowsPath(%q) = %v", path, got)
		}
	}
}

func TestShellLinesAndTheirHint(t *testing.T) {
	for line, want := range map[string][2]string{"!cd": {"cd", ""}, "!cd  a b ": {"cd", "a b"}, "! cd x": {"cd", "x"},
		"!pwd": {"pwd", ""}, `!cd 'a b'`: {"cd", "a b"}, `!cd "a`: {"cd", `"a`}} {
		cmd, arg, ok := shellLine(strings.TrimSpace(line))
		if !ok || cmd != want[0] || arg != want[1] {
			t.Errorf("shellLine(%q) = %q, %q, %v", line, cmd, arg, ok)
		}
	}
	for _, line := range []string{"!", "!cdx", "!ls", "!important", "cd x", "/cd x", "!!cd x"} {
		if _, _, ok := shellLine(line); ok {
			t.Errorf("shellLine(%q) is taken as geminus's", line)
		}
		if isGeminusCommand(line) {
			t.Errorf("%q is taken as a geminus command", line)
		}
	}
	if !isGeminusCommand("!cd x") || !isGeminusCommand("!pwd") {
		t.Error("!cd and !pwd are not taken as geminus commands")
	}

	v := view{persona: "sibyl", harness: "hermes", home: "/home/sibyl"}
	for text, want := range map[string]string{"!c": "from /home/sibyl", "!cd": "from /home/sibyl", "!cd pro": "!cd DIR moves on sibyl",
		"!pw": "!pwd shows"} {
		if got, ok := shellHint(text, v); !ok || !strings.Contains(got, want) {
			t.Errorf("shellHint(%q) = %q, %v", text, got, ok)
		}
	}
	for _, text := range []string{"!", "!x", "!c x", "!cdx", "hello", "/cwd", "!cd a\nb"} {
		if got, ok := shellHint(text, v); ok {
			t.Errorf("shellHint(%q) = %q", text, got)
		}
	}
	v.cwd = "~/src"
	if got, _ := shellHint("!cd", v); !strings.Contains(got, "from ~/src") {
		t.Errorf("hint with a chosen directory: %q", got)
	}
	v.session = "geminus-sibyl-1"
	if got, _ := shellHint("!cd x", v); !strings.Contains(got, "stays in its directory") {
		t.Errorf("hint in a started conversation: %q", got)
	}
	if got, _ := shellHint("!cd x", view{}); !strings.Contains(got, "pick a persona first") {
		t.Errorf("hint with no persona: %q", got)
	}
}
