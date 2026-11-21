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

const versionFlag = "-X github.com/hauntedfail/geminus/internal/version.Version="

func buildAs(t *testing.T, out, pkg, version string) {
	t.Helper()
	msg, err := exec.Command("go", "build", "-ldflags", versionFlag+version, "-o", out, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, msg)
	}
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

// TestAReleaseIsPublishedAndInstalledByTheBuiltPrograms runs the server and
// geminus as they are shipped: a release goes up with `geminus release`, and
// an older geminus replaces itself with it by `geminus update`.
func TestAReleaseIsPublishedAndInstalledByTheBuiltPrograms(t *testing.T) {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	bin, dist, installed := t.TempDir(), t.TempDir(), t.TempDir()
	server := filepath.Join(bin, "geminus-latest-channel"+exe)
	buildAs(t, server, "../cmd/geminus-latest-channel", "0.0.2")
	fresh := filepath.Join(dist, "geminus-"+runtime.GOOS+"-"+runtime.GOARCH+exe)
	buildAs(t, fresh, "../cmd/geminus", "0.0.2")
	program := filepath.Join(installed, "geminus"+exe)
	buildAs(t, program, "../cmd/geminus", "0.0.1")
	// Something a real build directory also holds.
	if err := os.WriteFile(filepath.Join(dist, "geminus-latest-channel-linux-amd64"), []byte("the server"), 0o755); err != nil {
		t.Fatal(err)
	}

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
	srv := exec.Command(server, "-listen", address, "-dir", filepath.Join(t.TempDir(), "releases"))
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
	if out := run(true, fresh, "release", "0.0.2", dist); !strings.Contains(out, "published 0.0.2: 1 programs") {
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
	want, err := os.ReadFile(fresh)
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
