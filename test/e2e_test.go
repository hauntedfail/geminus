// Package e2e runs the built geminus binary against a scripted agent, a fake
// ssh, and a stub Honcho server.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var geminusBin, agentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "geminus-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	geminusBin = filepath.Join(dir, "geminus"+exe)
	agentBin = filepath.Join(dir, "fakeacp"+exe)
	for out, pkg := range map[string]string{geminusBin: "../cmd/geminus", agentBin: "./fakeacp"} {
		if msg, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", pkg, err, msg)
			os.Exit(1)
		}
	}
	// The fake ssh is the agent itself: it ignores the host and serves ACP.
	sshDir := filepath.Join(dir, "bin")
	data, err := os.ReadFile(agentBin)
	if err == nil {
		err = os.Mkdir(sshDir, 0o700)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(sshDir, "ssh"+exe), data, 0o700)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type world struct {
	t    *testing.T
	env  []string
	home string
	// args and probeArgs are where the fake ssh records how it was called.
	args, probeArgs string
}

// newWorld sets up a Hermes profile on "chrome", a Honcho workspace with
// three agents and a human, a route to sibyl over ssh (whose computer has
// Hermes), and a custom harness that is the scripted agent itself.
func newWorld(t *testing.T, honchoUp bool) *world {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/workspaces/ws/peers/list" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"pages": 1, "items": []map[string]string{
			{"id": "h1karu"}, {"id": "sibyl"}, {"id": "chrome"}, {"id": "mirai"}, {"id": "stray"},
		}})
	}))
	t.Cleanup(srv.Close)
	if !honchoUp {
		srv.Close()
	}
	hermes, home, state := t.TempDir(), t.TempDir(), t.TempDir()
	w := &world{t: t, home: home, args: filepath.Join(state, "ssh-args"), probeArgs: filepath.Join(state, "probe-args")}
	w.write(filepath.Join(hermes, "honcho.json"), map[string]string{
		"baseUrl": srv.URL, "workspace": "ws", "apiKey": "k", "aiPeer": "chrome", "peerName": "h1karu"})
	w.write(filepath.Join(hermes, "geminise.json"), map[string]any{
		"members": map[string]string{"h1karu": "human", "chrome": "agent", "sibyl": "agent", "mirai": "agent"}})
	w.config(map[string]any{
		"personas": map[string]any{
			"sibyl":  map[string]string{"ssh": "btcvps", "cwd": "/home/hermes"},
			"chrome": map[string]string{"harness": "fake"},
		},
		"harnesses": map[string]any{"fake": map[string]string{"name": "Fake Agent", "command": filepath.ToSlash(agentBin)}},
	})
	w.env = append(os.Environ(),
		"GEMINUS_HOME="+home, "HERMES_HOME="+hermes, "NO_COLOR=1",
		"FAKEACP_STATE="+filepath.Join(state, "sessions.json"),
		"FAKEACP_ARGS="+w.args, "FAKEACP_PROBE_ARGS="+w.probeArgs,
		"FAKEACP_PROBE=Last login: yesterday\nhome=/home/hermes\nhave=hermes\nhave=claude\n")
	return w
}

func (w *world) write(path string, v any) {
	w.t.Helper()
	data, _ := json.Marshal(v)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) config(v any) { w.write(filepath.Join(w.home, "config.json"), v) }

func (w *world) geminus(input string, args ...string) (stdout, stderr string, code int) {
	w.t.Helper()
	cmd := exec.Command(geminusBin, args...)
	cmd.Env = w.env
	cmd.Stdin = strings.NewReader(input)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		w.t.Fatalf("run geminus: %v", err)
	}
	return out.String(), errs.String(), code
}

func expect(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestPersonaListComesFromHoncho(t *testing.T) {
	out, errs, code := newWorld(t, true).geminus("", "persona")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	expect(t, out,
		"Personas (Honcho workspace ws)",
		"chrome  THIS COMPUTER  this computer",
		"mirai   REMOTE         no route yet",
		"sibyl   REMOTE         ssh btcvps",
	)
	if strings.Contains(out, "stray") || strings.Contains(out, "h1karu") {
		t.Errorf("non-agents listed:\n%s", out)
	}
}

func TestBareGeminusPicksPersonaFindsHarnessAndTalksOverSSH(t *testing.T) {
	w := newWorld(t, true)
	out, errs, code := w.geminus("sibyl\nremember plum\nsecond turn\n")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	expect(t, out,
		"persona ❯ sibyl",
		"looking at sibyl [REMOTE] (ssh btcvps)…",
		// The remote has Hermes and Claude Code without its bridge, so
		// Hermes is the only choice and is taken without asking.
		"● connected  sibyl [REMOTE]  Hermes Agent (fake-agent 1.2.3)  via ssh btcvps  session sess-0001  cwd /home/hermes",
		"sibyl [REMOTE] · hermes ❯ remember plum", "echo: remember plum", "echo: second turn",
	)
	// One ssh call asks what the computer has; one more carries the whole
	// conversation, starting the agent through a login shell.
	probe, err := os.ReadFile(w.probeArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(probe), "-T\n-oBatchMode=yes\n-oConnectTimeout=10\n--\nbtcvps\nsh -lc ") || !strings.Contains(string(probe), "hermes claude-agent-acp claude codex-acp") {
		t.Errorf("probe arguments: %q", probe)
	}
	args, err := os.ReadFile(w.args)
	if err != nil {
		t.Fatal(err)
	}
	if want := "-T\n-oBatchMode=yes\n-oConnectTimeout=10\n--\nbtcvps\nsh -lc 'exec hermes acp'"; string(args) != want {
		t.Errorf("ssh arguments:\n got %q\nwant %q", args, want)
	}

	out, errs, code = w.geminus("what did I say?\n", "persona", "sibyl", "--continue")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	expect(t, out, "── conversation so far ──", "remember plum", "echo: second turn", "session sess-0001", "echo: what did I say?")
}

func TestDefaultSavedInsideGeminusIsUsedByTheNextBareStart(t *testing.T) {
	w := newWorld(t, true)
	out, errs, code := w.geminus("/default save\n/default session continue\nfirst\n", "persona", "sibyl")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	expect(t, out, "geminus now starts with sibyl [REMOTE] · Hermes Agent", "default session: continue")

	data, err := os.ReadFile(filepath.Join(w.home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The custom harness written before is still there.
	expect(t, string(data), `"persona": "sibyl"`, `"session": "continue"`, `"harness": "hermes"`, `"Fake Agent"`)

	out, errs, code = w.geminus("second\n")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	expect(t, out, "sibyl [REMOTE] · hermes ❯ second", "echo: first", "session sess-0001")
	if strings.Contains(out, "persona ❯") {
		t.Errorf("asked for a persona although a default is set:\n%s", out)
	}
}

func TestLocalPersonaRunsACustomHarnessDirectly(t *testing.T) {
	w := newWorld(t, true)
	out, errs, code := w.geminus("hi\n/model fast\n", "persona", "chrome")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	expect(t, out,
		"● connected  chrome [THIS COMPUTER]  Fake Agent (fake-agent 1.2.3)  on this computer",
		"chrome [THIS COMPUTER] · fake ❯ hi", "echo: hi",
		"model set to fast",
	)
}

func TestAgentCrashShowsItsLastWords(t *testing.T) {
	out, _, _ := newWorld(t, true).geminus("die\nafter\n", "persona", "sibyl")
	expect(t, out,
		"✗ connection to sibyl lost", "fake-agent: fatal: asked to die",
		"○ disconnected  sibyl [REMOTE]  Hermes Agent", "not connected, so nothing was sent",
	)
}

func TestComputerWithoutAnyHarnessFailsBeforeAnyPrompt(t *testing.T) {
	w := newWorld(t, true)
	for i, kv := range w.env {
		if strings.HasPrefix(kv, "FAKEACP_PROBE=") {
			w.env[i] = "FAKEACP_PROBE=home=/home/hermes\nhave=codex\n"
		}
	}
	out, errs, code := w.geminus("hello\n", "persona", "sibyl")
	if code != 1 || !strings.Contains(errs, "has no agent program geminus can talk to") ||
		!strings.Contains(errs, "Codex is installed, but its ACP bridge is missing: npm install -g @zed-industries/codex-acp") ||
		strings.Contains(out, "❯") {
		t.Errorf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
}

func TestHonchoDownStillOffersConfiguredPersonas(t *testing.T) {
	out, errs, code := newWorld(t, false).geminus("", "persona")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	expect(t, out, "! Honcho directory unavailable", "chrome  THIS COMPUTER", "sibyl   REMOTE")
	if strings.Contains(out, "mirai") {
		t.Errorf("a persona known only to Honcho was listed while Honcho was down:\n%s", out)
	}
}
