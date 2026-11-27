package e2e

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/e2ee"
	"github.com/hauntedfail/geminus/internal/honcho/honchotest"
)

// TestGeminusAndBridgeCommunicateEndToEndOverE2EE proves that:
//   1. Both the Go geminus binary and the Python geminish bridge communicate
//      exclusively via GE2E v1 encrypted binary frames over the proxy endpoint
//      (direct Honcho baseUrl is unreachable, proving zero plaintext fallback).
//   2. Wire traffic reveals no Honcho path, Bearer token, prompt, or output.
//   3. Honcho stores the conversation completely raw.
func TestGeminusAndBridgeCommunicateEndToEndOverE2EE(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake agent program is a shell script")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	source := bridgeSource(t)
	geminus := build(t)

	secret, err := hex.DecodeString(secretHex)
	if err != nil {
		t.Fatal(err)
	}

	srv := honchotest.New("h1karu", "chrome", "mirai")
	defer srv.Close()

	proxyHandler, err := e2ee.NewProxy(e2ee.ProxyConfig{
		Secret:   secret,
		Upstream: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	var (
		mu         sync.Mutex
		wireCount  int
		wireLeaks  []string
		outerAuths int
		badPaths   int
	)
	const secretPrompt = "top-secret-prompt-alpha-9911"
	spyProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		wireCount++
		if r.URL.Path != "/geminise/e2ee" {
			badPaths++
		}
		if r.Header.Get("Authorization") != "" {
			outerAuths++
		}
		for _, tok := range []string{"/v3/workspaces", srv.Key, secretPrompt, "in geminus-mirai"} {
			if bytes.Contains(raw, []byte(tok)) {
				wireLeaks = append(wireLeaks, tok)
			}
		}
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		proxyHandler.ServeHTTP(w, r)
	}))
	defer spyProxy.Close()

	e2eeEndpoint := spyProxy.URL + "/geminise/e2ee"

	write := func(dir, name string, v any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	members := map[string]string{"h1karu": "human", "chrome": "agent", "mirai": "agent"}
	here, there, work := t.TempDir(), t.TempDir(), t.TempDir()
	agent := filepath.Join(there, "hermes")
	if err := os.WriteFile(agent, []byte(fakeHermes), 0o700); err != nil {
		t.Fatal(err)
	}

	// Point honcho.json baseUrl at an unreachable loopback port so any
	// non-E2EE direct request would fail immediately.
	for dir, self := range map[string]string{here: "chrome", there: "mirai"} {
		write(dir, "honcho.json", map[string]string{
			"baseUrl":   "http://127.0.0.1:1",
			"workspace": srv.Workspace,
			"apiKey":    srv.Key,
			"aiPeer":    self,
			"peerName":  "h1karu",
		})
	}
	write(here, "geminise.json", map[string]any{
		"from":     "chrome",
		"members":  members,
		"secret":   secretHex,
		"e2ee_url": e2eeEndpoint,
	})
	write(there, "geminise.json", map[string]any{
		"from":         "mirai",
		"members":      members,
		"secret":       secretHex,
		"e2ee_url":     e2eeEndpoint,
		"hermes_bin":   agent,
		"poll_seconds": 2,
		"push":         false,
		"wake":         false,
		"state_dir":    filepath.Join(there, "state"),
		"geminus":      map[string]any{"cwd": work},
	})

	var bridgeLog bytes.Buffer
	bridge := exec.Command(python, filepath.Join(source, "cli.py"), "poll", "--foreground")
	bridge.Env = append(os.Environ(),
		"HERMES_HOME="+there,
		"GEMINISE_CONFIG="+filepath.Join(there, "geminise.json"),
		"HOME="+there,
		"GEMINUS_UPDATES_URL=http://127.0.0.1:1",
		"SHELL=/bin/sh",
		"PYTHONDONTWRITEBYTECODE=1",
	)
	bridge.Stdout, bridge.Stderr = &bridgeLog, &bridgeLog
	if err := bridge.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = bridge.Process.Kill()
		_ = bridge.Wait()
		if t.Failed() {
			t.Logf("bridge log:\n%s", bridgeLog.String())
		}
	}()

	deadline := time.Now().Add(30 * time.Second)
	for len(srv.PeerMetadata("mirai", "geminus_node")) < 10 {
		if time.Now().After(deadline) {
			t.Fatalf("the E2EE bridge never published its node card\n%s", bridgeLog.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	cmd := exec.Command(geminus, "persona", "mirai", "--harness", "hermes")
	cmd.Env = append(os.Environ(),
		"HERMES_HOME="+here,
		"HOME="+t.TempDir(),
		"NO_COLOR=1",
		"GEMINUS_UPDATES_URL=http://127.0.0.1:1",
	)
	cmd.Stdin = strings.NewReader(secretPrompt + "\n")
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		t.Fatalf("geminus over E2EE failed: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errs.String())
	}
	if !strings.Contains(out.String(), secretPrompt) {
		t.Fatalf("geminus output missing echoed prompt:\n%s", out.String())
	}

	// Verify Honcho stored both the prompt and the output completely raw.
	sessions := srv.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 Honcho session, got %d", len(sessions))
	}
	msgs := srv.Messages(sessions[0].ID)
	foundPrompt, foundOutput := false, false
	for _, m := range msgs {
		if m.PeerID == "h1karu" && m.Content == secretPrompt {
			foundPrompt = true
		}
		if m.PeerID == "mirai" && strings.Contains(m.Content, secretPrompt) {
			foundOutput = true
		}
	}
	if !foundPrompt || !foundOutput {
		t.Fatalf("Honcho raw messages missing expected records: prompt=%v output=%v msgs=%+v", foundPrompt, foundOutput, msgs)
	}

	// Verify all wire frames stayed encrypted on the fixed endpoint.
	mu.Lock()
	defer mu.Unlock()
	if wireCount == 0 {
		t.Fatal("expected encrypted traffic through E2EE proxy, saw 0 requests")
	}
	if badPaths != 0 || outerAuths != 0 || len(wireLeaks) != 0 {
		t.Fatalf("wire confidentiality violated: badPaths=%d outerAuths=%d leaks=%v", badPaths, outerAuths, wireLeaks)
	}
}
