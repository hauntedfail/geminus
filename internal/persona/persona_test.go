package persona

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/harness"
)

// honcho serves the peer-list endpoint for workspace "ws", two peers a page.
func honcho(t *testing.T, peers ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v3/workspaces/ws/peers/list" {
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var page int
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		const size = 2
		items := []map[string]string{}
		for i := (page - 1) * size; i < page*size && i < len(peers); i++ {
			items = append(items, map[string]string{"id": peers[i]})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items, "page": page, "pages": (len(peers) + size - 1) / size})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// setup writes the Hermes profile and the geminus config into temp homes.
func setup(t *testing.T, baseURL, geminusConfig string) {
	t.Helper()
	hermes, home := t.TempDir(), t.TempDir()
	t.Setenv(EnvHermesHome, hermes)
	t.Setenv(EnvHome, home)
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(hermes, "honcho.json"), fmt.Sprintf(
		`{"baseUrl":%q,"workspace":"ws","apiKey":"secret-key","aiPeer":"chrome","peerName":"h1karu"}`, baseURL))
	write(filepath.Join(hermes, "geminise.json"),
		`{"from":"chrome","members":{"h1karu":"human","chrome":"agent","sibyl":"agent","mirai":"agent"}}`)
	if geminusConfig != "" {
		write(filepath.Join(home, "config.json"), geminusConfig)
	}
}

func describe(d Directory) string {
	var parts []string
	for _, p := range d.Personas {
		route := "no route"
		if p.Node != nil {
			route = p.Node.Where()
		}
		parts = append(parts, fmt.Sprintf("%s [%s] %s", p.Name, p.Tag(), route))
	}
	return strings.Join(parts, "; ")
}

func TestLoadListsAgentsFromHonchoWithRoutes(t *testing.T) {
	srv := honcho(t, "h1karu", "sibyl", "vvx", "chrome", "mirai")
	setup(t, srv.URL, `{"personas":{"sibyl":{"ssh":"btcvps","cwd":"/home/hermes"}}}`)

	dir, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The human and the stray peer "vvx" are not personas; the local agent
	// needs no configured route; mirai has none.
	want := "chrome [THIS COMPUTER] this computer; mirai [REMOTE] no route; sibyl [REMOTE] ssh btcvps"
	if got := describe(dir); got != want {
		t.Errorf("personas:\n got %s\nwant %s", got, want)
	}
	if dir.Workspace != "ws" || dir.Warning != "" {
		t.Errorf("workspace = %q, warning = %q", dir.Workspace, dir.Warning)
	}
}

func TestLoadWithoutMembershipKeepsEveryPeerButTheHuman(t *testing.T) {
	srv := honcho(t, "h1karu", "sibyl", "chrome")
	setup(t, srv.URL, "")
	os.Remove(filepath.Join(os.Getenv(EnvHermesHome), "geminise.json"))

	dir, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := describe(dir); got != "chrome [THIS COMPUTER] this computer; sibyl [REMOTE] no route" {
		t.Errorf("personas = %s", got)
	}
}

func TestLoadFallsBackToConfiguredPersonasWhenHonchoIsDown(t *testing.T) {
	srv := honcho(t)
	srv.Close()
	setup(t, srv.URL, `{"personas":{"sibyl":{"ssh":"btcvps","cwd":"/home/hermes"}}}`)

	dir, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := describe(dir); got != "chrome [THIS COMPUTER] this computer; sibyl [REMOTE] ssh btcvps" {
		t.Errorf("personas = %s", got)
	}
	if !strings.Contains(dir.Warning, "Honcho directory unavailable") || strings.Contains(dir.Warning, "secret-key") {
		t.Errorf("warning = %q", dir.Warning)
	}
}

func TestLoadReportsRejectedCredential(t *testing.T) {
	srv := honcho(t, "sibyl")
	setup(t, srv.URL, `{"honcho":{"apiKeyEnv":"GEMINUS_TEST_KEY"}}`)
	t.Setenv("GEMINUS_TEST_KEY", "wrong")

	dir, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(dir.Warning, "HTTP 401") {
		t.Errorf("warning = %q, want the HTTP status", dir.Warning)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	for name, cfg := range map[string]string{
		"option as alias":  `{"personas":{"sibyl":{"ssh":"-oProxyCommand=evil"}}}`,
		"shell in harness": `{"harnesses":{"x":{"command":"agent; rm -rf ~"}}}`,
		"session mode":     `{"defaults":{"session":"sometimes"}}`,
		"broken json":      `{"personas":`,
	} {
		srv := honcho(t, "sibyl")
		setup(t, srv.URL, cfg)
		if _, err := Load(context.Background()); err == nil {
			t.Errorf("%s: Load succeeded, want an error", name)
		}
	}
}

func TestLoadReadsDefaultsAndCustomHarnesses(t *testing.T) {
	srv := honcho(t, "sibyl", "chrome")
	setup(t, srv.URL, `{"defaults":{"persona":"sibyl","harness":"hermes","session":"continue"},
		"personas":{"sibyl":{"ssh":"btcvps","harness":"aider"}},
		"harnesses":{"aider":{"name":"Aider","command":"aider-acp","cli":"aider"}}}`)
	dir, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if dir.Defaults != (Defaults{Persona: "sibyl", Harness: "hermes", Session: SessionContinue}) {
		t.Errorf("defaults = %+v", dir.Defaults)
	}
	sibyl, _ := dir.Find("sibyl")
	if sibyl.Node == nil || sibyl.Node.Harness != "aider" {
		t.Errorf("sibyl node = %+v", sibyl.Node)
	}
	last := dir.Harnesses[len(dir.Harnesses)-1]
	if last.ID != "aider" || dir.Harnesses[0].ID != "hermes" {
		t.Errorf("harnesses = %+v", dir.Harnesses)
	}
}

func TestSavingKeepsTheRestOfTheConfig(t *testing.T) {
	srv := honcho(t, "sibyl", "mirai", "chrome")
	setup(t, srv.URL, `{"future":{"keep":true},"personas":{"sibyl":{"ssh":"btcvps"}}}`)
	if err := SaveNode("mirai", Node{SSH: "mirai-box", CWD: "/home/m"}); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}
	if err := SaveDefaults(Defaults{Persona: "mirai", Harness: "codex"}); err != nil {
		t.Fatalf("SaveDefaults: %v", err)
	}
	if err := SaveNode("bad", Node{SSH: "-oEvil"}); err == nil {
		t.Error("SaveNode accepted an option as the alias")
	}
	path := filepath.Join(os.Getenv(EnvHome), "config.json")
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"keep": true`) {
		t.Errorf("unknown key lost:\n%s", data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", info.Mode().Perm())
	}
	dir, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load after saving: %v", err)
	}
	if got := describe(dir); got != "chrome [THIS COMPUTER] this computer; mirai [REMOTE] ssh mirai-box; sibyl [REMOTE] ssh btcvps" {
		t.Errorf("personas = %s", got)
	}
	if dir.Defaults.Persona != "mirai" || dir.Defaults.Harness != "codex" {
		t.Errorf("defaults = %+v", dir.Defaults)
	}
}

func TestSavingCreatesTheConfig(t *testing.T) {
	t.Setenv(EnvHome, filepath.Join(t.TempDir(), "fresh"))
	if err := SaveDefaults(Defaults{Persona: "sibyl"}); err != nil {
		t.Fatalf("SaveDefaults: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv(EnvHome), "config.json"))
	if err != nil || !strings.Contains(string(data), `"persona": "sibyl"`) {
		t.Errorf("config = %q, %v", data, err)
	}
}

func TestNodeExec(t *testing.T) {
	hermes := harness.Harness{ID: "hermes", Command: "hermes acp"}
	for _, tc := range []struct {
		node Node
		want string
	}{
		{Node{}, "hermes acp"},
		{Node{SSH: "btcvps", CWD: "/home/hermes"}, "ssh -T -oBatchMode=yes -oConnectTimeout=10 -- btcvps sh -lc 'exec hermes acp'"},
	} {
		cmd, err := tc.node.Exec(hermes)
		if err != nil {
			t.Errorf("%+v: %v", tc.node, err)
			continue
		}
		if got := strings.Join(cmd.Args, " "); got != tc.want {
			t.Errorf("%+v:\n got %s\nwant %s", tc.node, got, tc.want)
		}
	}
	if _, err := (Node{SSH: "-oEvil"}).Exec(hermes); err == nil {
		t.Error("Exec accepted an option as the alias")
	}
}

func TestLocalProbeAndSessionDirectory(t *testing.T) {
	all, _ := harness.All(map[string]harness.Harness{"shell": {Command: "sh -c true"}, "none": {Command: "geminus-no-such-program acp"}})
	probe, err := (Node{}).Probe(context.Background(), all)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !probe.Have["sh"] || probe.Have["geminus-no-such-program"] || probe.Home == "" {
		t.Errorf("probe = %+v", probe)
	}
	here, _ := os.Getwd()
	for _, tc := range []struct {
		node  Node
		probe harness.Probe
		want  string
	}{
		{Node{}, probe, here},
		{Node{CWD: "/srv"}, probe, "/srv"},
		{Node{SSH: "h"}, harness.Probe{Home: "/home/hermes"}, "/home/hermes"},
		{Node{SSH: "h", CWD: "/work"}, harness.Probe{Home: "/home/hermes"}, "/work"},
	} {
		if got, err := tc.node.SessionCWD(tc.probe); err != nil || got != tc.want {
			t.Errorf("%+v: cwd = %q, %v; want %q", tc.node, got, err, tc.want)
		}
	}
	if _, err := (Node{SSH: "h"}).SessionCWD(harness.Probe{}); err == nil {
		t.Error("remote node without a known home returned a directory")
	}
}

func TestLoadSkipsPeersWithUnsafeNames(t *testing.T) {
	srv := honcho(t, "sibyl", "evil\x1b[2J", "two words", "-flag", strings.Repeat("x", 65))
	setup(t, srv.URL, "")
	os.Remove(filepath.Join(os.Getenv(EnvHermesHome), "geminise.json"))

	dir, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := describe(dir); got != "sibyl [REMOTE] no route" {
		t.Errorf("personas = %q", got)
	}
}
