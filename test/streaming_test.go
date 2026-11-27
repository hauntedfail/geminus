package e2e

import (
	"bufio"
	"bytes"
	"context"
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
	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/honcho/honchotest"
	"github.com/hauntedfail/geminus/internal/wire"
)

// fakeStreamHermes mimics `hermes chat --format stream-json`, emitting an early
// token and tool progress, sleeping 1.5s, and then emitting the final token.
const fakeStreamHermes = `#!/bin/sh
if [ "$1" = "chat" ] && [ "$2" = "--help" ]; then
  echo "usage: hermes chat [--format {text,stream-json}]"
  exit 0
fi
cat >/dev/null
printf '{"type":"system","subtype":"init","session_id":"stream-sess-1"}\n'
printf '{"type":"tool_use","name":"terminal","input":{"command":"echo check"}}\n'
printf '{"type":"tool_result","name":"terminal","duration_ms":12,"is_error":false}\n'
printf '{"type":"text","text":"EARLY-STREAM-TOKEN-1 "}\n'
sleep 1.5
printf '{"type":"text","text":"FINAL-STREAM-TOKEN-2\\n"}\n'
printf '{"type":"result","session_id":"stream-sess-1","text":"EARLY-STREAM-TOKEN-1 FINAL-STREAM-TOKEN-2\\n","exit_code":0}\n'
`

// TestGeminusRealTimeStreamingOverE2EE proves end-to-end across the built Go
// geminus binary, Go e2ee.Proxy + StreamHub, real Python geminish bridge, and
// honchotest that:
//  1. Early tokens arrive at geminus BEFORE the agent finishes the turn.
//  2. Tool execution progress is surfaced live.
//  3. Final Honcho reconciliation produces zero duplicated output.
//  4. Honcho stores only a single clean final RoleOutput message (no chunk bloat).
//  5. Wire traffic carries zero plaintext prompts, tokens, or Honcho paths.
func TestGeminusRealTimeStreamingOverE2EE(t *testing.T) {
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
	)
	const secretPrompt = "stream-secret-prompt-7744"
	spyProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		wireCount++
		if r.Header.Get("Authorization") != "" {
			outerAuths++
		}
		for _, tok := range []string{"/v3/workspaces", "/_stream/", srv.Key, secretPrompt, "EARLY-STREAM-TOKEN-1", "FINAL-STREAM-TOKEN-2"} {
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
	if err := os.WriteFile(agent, []byte(fakeStreamHermes), 0o700); err != nil {
		t.Fatal(err)
	}

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
		"poll_seconds": 1,
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

	// Wait until bridge publishes its node card into Honcho.
	honchoCli, err := honcho.New(srv.URL, srv.Workspace, srv.Key)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		peers, err := honchoCli.Peers(context.Background())
		if err == nil {
			for _, p := range peers {
				if p.ID == "mirai" && len(p.Metadata[wire.NodeKey]) > 0 {
					goto ready
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("bridge did not publish node card in time")

ready:
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, geminus, "mirai", "hermes")
	cmd.Env = append(os.Environ(),
		"HERMES_HOME="+here,
		"GEMINISE_CONFIG="+filepath.Join(here, "geminise.json"),
		"HOME="+here,
		"GEMINUS_UPDATES_URL=http://127.0.0.1:1",
		"NO_COLOR=1",
		"TERM=dumb",
	)
	cmd.Stdin = strings.NewReader(secretPrompt + "\n/quit\n")
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var (
		outMu      sync.Mutex
		fullOut    strings.Builder
		earlyAt    time.Time
		finalAt    time.Time
		readerDone = make(chan struct{})
	)
	go func() {
		defer close(readerDone)
		r := bufio.NewReader(stdoutPipe)
		buf := make([]byte, 256)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				outMu.Lock()
				fullOut.Write(buf[:n])
				cur := fullOut.String()
				if earlyAt.IsZero() && strings.Contains(cur, "EARLY-STREAM-TOKEN-1") {
					earlyAt = time.Now()
				}
				if finalAt.IsZero() && strings.Contains(cur, "FINAL-STREAM-TOKEN-2") {
					finalAt = time.Now()
				}
				outMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		<-readerDone
		t.Fatalf("geminus failed: %v\nstdout:\n%s\nstderr:\n%s", err, fullOut.String(), stderrBuf.String())
	}
	<-readerDone

	screen := fullOut.String()
	if earlyAt.IsZero() || finalAt.IsZero() {
		t.Fatalf("missing streamed tokens in output:\n%s", screen)
	}
	lead := finalAt.Sub(earlyAt)
	if lead < 500*time.Millisecond {
		t.Fatalf("early token did not stream before turn completion (delta=%v, expected >= 500ms)\noutput:\n%s", lead, screen)
	}
	if !strings.Contains(screen, "terminal: echo check") {
		t.Fatalf("missing live tool progress in output:\n%s", screen)
	}
	if c := strings.Count(screen, "EARLY-STREAM-TOKEN-1"); c != 1 {
		t.Fatalf("expected EARLY-STREAM-TOKEN-1 exactly once (no duplication), got %d\n%s", c, screen)
	}
	if c := strings.Count(screen, "FINAL-STREAM-TOKEN-2"); c != 1 {
		t.Fatalf("expected FINAL-STREAM-TOKEN-2 exactly once (no duplication), got %d\n%s", c, screen)
	}

	// Verify Honcho stored only ONE clean final RoleOutput message (no intermediate chunk spam).
	sessions, err := honchoCli.Sessions(context.Background(), map[string]any{"kind": wire.Kind})
	if err != nil || len(sessions) == 0 {
		t.Fatalf("expected geminus session in Honcho, err=%v len=%d", err, len(sessions))
	}
	msgs, err := honchoCli.Messages(context.Background(), sessions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	outputs := 0
	for _, m := range msgs {
		var meta wire.Message
		if raw := m.Metadata[wire.Kind]; len(raw) > 0 && json.Unmarshal(raw, &meta) == nil && meta.Role == wire.RoleOutput {
			outputs++
			if !meta.Final || !strings.Contains(m.Content, "EARLY-STREAM-TOKEN-1 FINAL-STREAM-TOKEN-2") {
				t.Fatalf("unexpected Honcho RoleOutput: meta=%+v content=%q", meta, m.Content)
			}
		}
	}
	if outputs != 1 {
		t.Fatalf("expected exactly 1 final RoleOutput in Honcho (clean history), got %d", outputs)
	}

	mu.Lock()
	defer mu.Unlock()
	if wireCount == 0 || outerAuths != 0 || len(wireLeaks) != 0 {
		t.Fatalf("wire security check failed: count=%d outerAuths=%d leaks=%v", wireCount, outerAuths, wireLeaks)
	}
}
