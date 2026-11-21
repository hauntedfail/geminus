package e2e

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	versionFlag = "-X github.com/hauntedfail/geminus/internal/version.Version="
	keyFlag     = "-X github.com/hauntedfail/geminus/internal/updates.releaseKey="
)

// buildAs builds pkg as version, trusting the release key given (the one in
// the source if it is "").
func buildAs(t *testing.T, out, pkg, version, key string) {
	t.Helper()
	ldflags := versionFlag + version
	if key != "" {
		ldflags += " " + keyFlag + key
	}
	msg, err := exec.Command("go", "build", "-ldflags", ldflags, "-o", out, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, msg)
	}
}

// tagged is a repository holding this source as it is now, committed and
// tagged v<version>: what the server builds a release from.
func tagged(t *testing.T, version string) string {
	t.Helper()
	repo := t.TempDir()
	for _, dir := range []string{"cmd", "internal"} {
		if err := os.CopyFS(filepath.Join(repo, dir), os.DirFS(filepath.Join("..", dir))); err != nil {
			t.Fatal(err)
		}
	}
	mod, err := os.ReadFile(filepath.Join("..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), mod, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "the source of " + version}, {"tag", "v" + version},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

// freeAddress is a local address nothing listens on at the moment.
func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestAReleaseIsBuiltByTheServerAndInstalledByTheBuiltPrograms runs the
// server and geminus as they are shipped: `geminus release` has the server
// build the tagged source and sign it with its own key, and an older geminus
// replaces itself with that by `geminus update`.
func TestAReleaseIsBuiltByTheServerAndInstalledByTheBuiltPrograms(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	bin, installed, releases := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "releases")
	server := filepath.Join(bin, "geminus-latest-channel"+exe)
	buildAs(t, server, "../cmd/geminus-latest-channel", "0.0.2", "")
	// The server makes its key; its public half is what a geminus trusts.
	keyFile := filepath.Join(t.TempDir(), "keys", "release.key")
	made, err := exec.Command(server, "-key", keyFile, "-keygen").Output()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	public := strings.TrimSpace(string(made))
	if again, err := exec.Command(server, "-key", keyFile, "-keygen").Output(); err != nil || strings.TrimSpace(string(again)) != public {
		t.Fatalf("a second keygen printed %q (%v), want the same key %q", again, err, public)
	}
	program := filepath.Join(installed, "geminus"+exe)
	buildAs(t, program, "../cmd/geminus", "0.0.1", public)
	target := runtime.GOOS + "-" + runtime.GOARCH

	// One Hermes profile for all three: on a real computer each has its own
	// copy of the same group secret.
	home := t.TempDir()
	for name, content := range map[string]any{
		"honcho.json":   map[string]string{"baseUrl": "http://127.0.0.1:1", "workspace": "ws", "apiKey": "k", "aiPeer": "chrome", "peerName": "h1karu"},
		"geminise.json": map[string]any{"from": "chrome", "members": map[string]string{"h1karu": "human", "chrome": "agent"}, "secret": secretHex},
	} {
		raw, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	address := freeAddress(t)
	env := append(os.Environ(), "HERMES_HOME="+home, "HOME="+t.TempDir(), "NO_COLOR=1",
		"GEMINUS_UPDATES_URL=http://"+address+"/geminus/updates")

	var serverLog bytes.Buffer
	srv := exec.Command(server, "-listen", address, "-dir", releases, "-key", keyFile,
		"-repo", tagged(t, "0.0.2"), "-targets", target)
	srv.Env, srv.Stdout, srv.Stderr = env, &serverLog, &serverLog
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = srv.Process.Kill()
		_ = srv.Wait()
		if t.Failed() {
			t.Logf("server log:\n%s", serverLog.String())
		}
	}()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if conn, err := net.Dial("tcp", address); err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server did not start listening")
		}
	}

	run := func(wantOK bool, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantOK {
			t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, out)
		}
		return string(out)
	}

	if out := run(false, program, "update"); !strings.Contains(out, "no release has been published") {
		t.Errorf("update before any release:\n%s", out)
	}
	// No tag, no release.
	if out := run(false, program, "release", "0.0.3"); !strings.Contains(out, "v0.0.3") {
		t.Errorf("release of a version without a tag:\n%s", out)
	}
	if out := run(true, program, "release", "0.0.2"); !strings.Contains(out, "released 0.0.2: 1 programs") {
		t.Errorf("release:\n%s", out)
	}
	if out := run(true, program, "version"); !strings.Contains(out, "geminus 0.0.1 ") {
		t.Fatalf("the installed program before the update:\n%s", out)
	}
	if out := run(true, program, "update"); !strings.Contains(out, "geminus 0.0.1 → 0.0.2") {
		t.Errorf("update:\n%s", out)
	}
	if out := run(true, program, "version"); !strings.Contains(out, "geminus 0.0.2 ") {
		t.Errorf("the installed program after the update:\n%s", out)
	}
	if out := run(true, program, "update"); !strings.Contains(out, "already the latest (0.0.2)") {
		t.Errorf("a second update:\n%s", out)
	}
	want, err := os.ReadFile(filepath.Join(releases, "0.0.2", target))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(program); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the installed program is not the released one (%v)", err)
	}
	left, err := os.ReadDir(installed)
	if err != nil {
		t.Fatal(err)
	}
	// The second start on Windows removes what the update moved aside.
	if len(left) != 1 {
		t.Errorf("%d files where the program is installed", len(left))
	}

	// A geminus that trusts another key does not take this server's release.
	other := filepath.Join(t.TempDir(), "geminus"+exe)
	buildAs(t, other, "../cmd/geminus", "0.0.1", strings.Repeat("ab", 32))
	if out := run(false, other, "update"); !strings.Contains(out, "not signed with the release key") {
		t.Errorf("update by a geminus that trusts another key:\n%s", out)
	}
	if out := run(true, other, "version"); !strings.Contains(out, "geminus 0.0.1 ") {
		t.Errorf("it replaced itself all the same:\n%s", out)
	}

	// Someone outside the group gets nothing.
	stranger := t.TempDir()
	for _, name := range []string{"honcho.json", "geminise.json"} {
		raw, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.ReplaceAll(raw, []byte(secretHex), []byte(strings.Repeat("f", 64)))
		if err := os.WriteFile(filepath.Join(stranger, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env = append(env, "HERMES_HOME="+stranger)
	if out := run(false, program, "update"); !strings.Contains(out, "401") {
		t.Errorf("update with another group's secret:\n%s", out)
	}
}
