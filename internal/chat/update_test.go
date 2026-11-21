package chat

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/updates"
	"github.com/hauntedfail/geminus/internal/updates/server"
	"github.com/hauntedfail/geminus/internal/version"
)

// releases is a release server beside a world, and the stand-in for the
// geminus program on disk that an update replaces.
type releases struct {
	t      *testing.T
	w      *world
	srv    *httptest.Server
	client *updates.Client
	exe    string
	// restarted holds the arguments geminus started itself again with.
	restarted chan []string
}

// running makes this build call itself version v for the test.
func running(t *testing.T, v string) {
	t.Helper()
	before := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = before })
}

func newReleases(t *testing.T, w *world) *releases {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	r := &releases{t: t, w: w, exe: filepath.Join(t.TempDir(), "geminus"), restarted: make(chan []string, 1)}
	r.srv = httptest.NewServer(server.New(root, w.prof.Secret, time.Now, slog.New(slog.DiscardHandler)))
	t.Cleanup(r.srv.Close)
	r.client = &updates.Client{Base: r.srv.URL + updates.Path, Secret: w.prof.Secret, HTTP: r.srv.Client(), Now: time.Now}
	if err := os.WriteFile(r.exe, []byte("the old program"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.attach()
	return r
}

// attach gives the world's current app the server and the program.
func (r *releases) attach() {
	r.w.app.Updates, r.w.app.Exe = r.client, r.exe
	r.w.app.Restart = func(exe string, args []string) (int, error) {
		r.restarted <- append([]string{exe}, args...)
		return 0, nil
	}
}

// publish releases version v for this computer, or for another kind of
// computer only.
func (r *releases) publish(v string, forThisComputer bool) {
	r.t.Helper()
	target := updates.Target(runtime.GOOS, runtime.GOARCH)
	if !forThisComputer {
		target = "plan9-mips"
	}
	built := filepath.Join(r.t.TempDir(), "built")
	if err := os.WriteFile(built, []byte("the program of "+v), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.client.Publish(context.Background(), v, map[string]string{target: built}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *releases) program() string {
	r.t.Helper()
	raw, err := os.ReadFile(r.exe)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(raw)
}

func TestUpdateInstallsANewerReleaseAndOnlyANewerOne(t *testing.T) {
	running(t, "0.4.0")
	w := newWorld(t)
	r := newReleases(t, w)
	r.publish("0.5.0", true)
	if code := w.run([]string{"update"}); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect("geminus 0.4.0 → 0.5.0")
	if got := r.program(); got != "the program of 0.5.0" {
		t.Errorf("program = %q", got)
	}

	// The new program asks again: nothing to do.
	version.Version = "0.5.0"
	w.newApp()
	r.attach()
	if code := w.run([]string{"update"}); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect("already the latest (0.5.0)")

	// A build ahead of the latest release is left alone.
	version.Version = "0.6.0-dev"
	w.newApp()
	r.attach()
	if code := w.run([]string{"update"}); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect("no release is newer than this build (0.6.0-dev); the latest is 0.5.0")
	if got := r.program(); got != "the program of 0.5.0" {
		t.Errorf("program = %q", got)
	}
	select {
	case args := <-r.restarted:
		t.Errorf("geminus update started the program again: %v", args)
	default:
	}
}

func TestABuildWithoutAVersionTakesAnyRelease(t *testing.T) {
	running(t, "dev")
	w := newWorld(t)
	r := newReleases(t, w)
	r.publish("0.5.0", true)
	if code := w.run([]string{"update"}); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect("geminus dev → 0.5.0")
}

func TestUpdateSaysWhyItCannotAndLeavesTheProgram(t *testing.T) {
	running(t, "0.4.0")
	for name, c := range map[string]struct {
		prepare func(*releases)
		want    string
	}{
		"nothing published":         {func(*releases) {}, "no release has been published"},
		"nothing for this computer": {func(r *releases) { r.publish("0.5.0", false) }, "has no program for " + updates.Target(runtime.GOOS, runtime.GOARCH)},
		"the server is away":        {func(r *releases) { r.publish("0.5.0", true); r.srv.Close() }, "geminus: update:"},
		"the program is unknown":    {func(r *releases) { r.publish("0.5.0", true); r.w.app.Exe = "" }, "where its own program is"},
	} {
		w := newWorld(t)
		r := newReleases(t, w)
		c.prepare(r)
		if code := w.run([]string{"update"}); code != 1 {
			t.Errorf("%s: exit %d", name, code)
		}
		if !strings.Contains(w.errs.String(), c.want) {
			t.Errorf("%s: stderr %q lacks %q", name, w.errs, c.want)
		}
		if got := r.program(); got != "the old program" {
			t.Errorf("%s: program = %q", name, got)
		}
		if left, _ := os.ReadDir(filepath.Dir(r.exe)); len(left) != 1 {
			t.Errorf("%s: %d files beside the program", name, len(left))
		}
	}
	// Without a server to ask, the command says so instead of failing oddly.
	w := newWorld(t)
	if code := w.run([]string{"update"}); code != 1 || !strings.Contains(w.errs.String(), "geminus: update:") {
		t.Errorf("no update server: exit %d, stderr %q", code, w.errs)
	}
}

func TestUpdateIsNotMixedWithOtherArguments(t *testing.T) {
	w := newWorld(t)
	newReleases(t, w)
	for _, args := range [][]string{{"update", "now"}, {"persona", "chrome", "update"}, {"update", "--continue"}} {
		if code := w.run(args); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestSlashUpdateWithoutATerminalInstallsAndSaysWhenItTakesEffect(t *testing.T) {
	running(t, "0.4.0")
	w := newWorld(t)
	r := newReleases(t, w)
	r.publish("0.5.0", true)
	w.talk([]string{"persona", "chrome", "--harness", "claude"},
		[2]string{"/update", "geminus 0.4.0 → 0.5.0"},
		[2]string{"still here", "echo: still here"})
	w.expect("the next time geminus starts")
	if got := r.program(); got != "the program of 0.5.0" {
		t.Errorf("program = %q", got)
	}
	select {
	case args := <-r.restarted:
		t.Errorf("geminus started itself again with piped input: %v", args)
	default:
	}
}

func TestSlashUpdateOnATerminalStartsTheNewProgramInTheSameConversation(t *testing.T) {
	running(t, "0.4.0")
	w := newWorld(t)
	r := newReleases(t, w)
	r.publish("0.5.0", true)
	term, typed, done := w.onTerminal(t, "persona", "chrome", "--harness", "claude")
	term.waitFor(t, "╭─ chrome · Claude Code ─", "update 0.5.0 · /update")
	typed("remember this\r")
	term.waitFor(t, "echo: remember this")
	typed("/update\r")
	var args []string
	select {
	case args = <-r.restarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("geminus did not start itself again:\n%s", term.text())
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("geminus did not leave after starting the new program")
	}
	sessions := w.srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("%d conversations", len(sessions))
	}
	want := []string{r.exe, "persona", "chrome", "--resume", sessions[0].ID}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("started again with %v, want %v", args, want)
	}
	if got := term.text(); !strings.Contains(got, "geminus 0.4.0 → 0.5.0") || strings.Contains(got, "╭─") {
		t.Errorf("the screen left behind:\n%s", got)
	}
	if got := r.program(); got != "the program of 0.5.0" {
		t.Errorf("program = %q", got)
	}
}

func TestSlashUpdateBeforeAConversationKeepsPersonaHarnessAndDirectory(t *testing.T) {
	running(t, "0.4.0")
	w := newWorld(t)
	r := newReleases(t, w)
	r.publish("0.5.0", true)
	term, typed, done := w.onTerminal(t, "persona", "chrome", "--harness", "grok", "--cwd", "~/work")
	term.waitFor(t, "╭─ chrome · Grok ─", "update 0.5.0 · /update")
	typed("/update\r")
	select {
	case args := <-r.restarted:
		want := []string{r.exe, "persona", "chrome", "--harness", "grok", "--cwd", "~/work"}
		if strings.Join(args, " ") != strings.Join(want, " ") {
			t.Errorf("started again with %v, want %v", args, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("geminus did not start itself again:\n%s", term.text())
	}
	<-done
}

func TestSlashUpdateWithNothingNewerStaysWhereItIs(t *testing.T) {
	running(t, "0.5.0")
	w := newWorld(t)
	r := newReleases(t, w)
	r.publish("0.5.0", true)
	term, typed, done := w.onTerminal(t, "persona", "chrome", "--harness", "claude")
	term.waitFor(t, "╭─ chrome · Claude Code ─")
	typed("/update\r")
	term.waitFor(t, "already the latest (0.5.0)")
	typed("still here\r")
	term.waitFor(t, "echo: still here")
	if strings.Contains(term.text(), "update 0.5.0 ·") {
		t.Errorf("a notice for the version that is running:\n%s", term.text())
	}
	select {
	case args := <-r.restarted:
		t.Errorf("started again: %v", args)
	default:
	}
	typed("\x03", "\x03")
	<-done
}

func TestAnUpdateServerThatIsAwayIsNotMentioned(t *testing.T) {
	running(t, "0.4.0")
	w := newWorld(t)
	r := newReleases(t, w)
	r.srv.Close()
	term, typed, done := w.onTerminal(t, "persona", "chrome", "--harness", "claude")
	term.waitFor(t, "╭─ chrome · Claude Code ─")
	typed("hello\r")
	term.waitFor(t, "echo: hello")
	if got := term.text(); strings.Contains(got, "update") {
		t.Errorf("the screen mentions updates:\n%s", got)
	}
	typed("\x03", "\x03")
	<-done
}

func TestStatusNamesANewerRelease(t *testing.T) {
	running(t, "0.4.0")
	w := newWorld(t)
	r := newReleases(t, w)
	r.publish("0.5.0", true)
	in, typist := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- w.app.Main([]string{"persona", "chrome", "--harness", "claude"}, in, w.errs) }()
	// The look for a release runs beside the start: ask until it is known.
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(w.screen.String(), "update 0.5.0 available (/update)"); {
		if time.Now().After(deadline) {
			t.Fatalf("/status never named the release:\n%s", w.screen.String())
		}
		io.WriteString(typist, "/status\n")
		time.Sleep(20 * time.Millisecond)
	}
	typist.Close()
	<-done
}

// A value that the new program would read as an option is not passed to it.
func TestSlashUpdateDoesNotStartAgainWithArgumentsThatLookLikeOptions(t *testing.T) {
	running(t, "0.4.0")
	w := newWorld(t)
	r := newReleases(t, w)
	r.publish("0.5.0", true)
	term, typed, done := w.onTerminal(t, "persona", "chrome", "--harness", "claude")
	term.waitFor(t, "╭─ chrome · Claude Code ─")
	typed("/cwd -rf\r")
	term.waitFor(t, "in -rf")
	typed("/update\r")
	term.waitFor(t, "geminus 0.4.0 → 0.5.0", "the next time geminus starts")
	select {
	case args := <-r.restarted:
		t.Errorf("started again with %v", args)
	default:
	}
	typed("\x03", "\x03")
	<-done
}

// After an /update line the keyboard is not read until geminus says it goes
// on here; when another program takes over, it is not read again at all.
func TestTheKeyboardIsLeftAloneAfterUpdateUntilGeminusGoesOn(t *testing.T) {
	_, lv, _ := newTestLive(60, 20)
	resume, closed := make(chan struct{}), make(chan struct{})
	lv.park = func(line string) bool {
		if !isUpdateLine(line) {
			return true
		}
		select {
		case <-resume:
			return true
		case <-closed:
			return false
		}
	}
	lines := make(chan string, 8)
	fed := make(chan bool, 1)
	go func() { fed <- lv.feed(keysOf(t, "hello\r", "/update\r", "typed ahead\r"), lines) }()
	if got := []string{<-lines, <-lines}; got[0] != "hello" || got[1] != "/update" {
		t.Fatalf("lines = %q", got)
	}
	select {
	case line := <-lines:
		t.Fatalf("read on after /update: %q", line)
	case <-time.After(50 * time.Millisecond):
	}
	resume <- struct{}{}
	if line := <-lines; line != "typed ahead" {
		t.Errorf("line = %q", line)
	}
	if stop := <-fed; stop {
		t.Error("the reader stopped although geminus went on")
	}
	// The other outcome: the box is closed for the new program.
	go func() { fed <- lv.feed(keysOf(t, "/update now\r", "for the new program\r"), lines) }()
	<-lines
	close(closed)
	if stop := <-fed; !stop {
		t.Error("the reader would read on beside the new program")
	}
	select {
	case line := <-lines:
		t.Errorf("a line meant for the new program was taken: %q", line)
	default:
	}
	for line, want := range map[string]bool{"/update": true, " /update ": true, "/update x": true, "//update": false, "/updates": false, "update": false} {
		if isUpdateLine(line) != want {
			t.Errorf("isUpdateLine(%q) = %v", line, !want)
		}
	}
}

func TestAServerOfferingAnOlderReleaseIsIgnored(t *testing.T) {
	running(t, "0.5.0")
	w := newWorld(t)
	r := newReleases(t, w)
	r.publish("0.4.9", true)
	if code := w.run([]string{"update"}); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect("no release is newer than this build (0.5.0); the latest is 0.4.9")
	if got := r.program(); got != "the old program" {
		t.Errorf("program = %q", got)
	}
}

func TestTheStartNeverWaitsForTheUpdateServer(t *testing.T) {
	running(t, "0.4.0")
	w := newWorld(t)
	r := newReleases(t, w)
	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	t.Cleanup(func() { close(hang); slow.Close() })
	r.client.Base, r.client.HTTP = slow.URL+updates.Path, slow.Client()
	began := time.Now()
	w.talk([]string{"persona", "chrome", "--harness", "claude"}, [2]string{"hello", "echo: hello"})
	if took := time.Since(began); took > 1500*time.Millisecond {
		t.Errorf("a conversation behind a silent update server took %s", took)
	}
}

func TestReleasePublishesTheProgramsOfABuild(t *testing.T) {
	w := newWorld(t)
	r := newReleases(t, w)
	dist := t.TempDir()
	for name, body := range map[string]string{
		"geminus-linux-amd64":                "linux",
		"geminus-windows-amd64.exe":          "windows",
		"geminus-latest-channel-linux-amd64": "the server, not a geminus",
		"notes.txt":                          "not a program",
	} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if code := w.run([]string{"release", "0.5.0", dist}); code != 0 {
		t.Fatalf("exit %d: %s", code, w.errs)
	}
	w.expect("published 0.5.0: 2 programs")
	m, err := r.client.Latest(context.Background())
	if err != nil || m.Version != "0.5.0" || len(m.Files) != 2 || m.Files["windows-amd64"].Size != 7 {
		t.Fatalf("latest = %+v, %v", m, err)
	}
	// The same version again with other programs, a version that is not a
	// release, and a directory without programs are refused.
	os.WriteFile(filepath.Join(dist, "geminus-linux-amd64"), []byte("another linux"), 0o755)
	for _, args := range [][]string{{"release", "0.5.0", dist}, {"release", "0.6.0-dev", dist}, {"release", "0.6.0", t.TempDir()}} {
		w.newApp()
		r.attach()
		if code := w.run(args); code != 1 || !strings.Contains(w.errs.String(), "geminus: release:") {
			t.Errorf("%v: exit %d, stderr %q", args, code, w.errs)
		}
	}
	if code := w.run([]string{"release", "0.6.0"}); code != 2 {
		t.Errorf("release without a directory: exit %d", code)
	}
}
