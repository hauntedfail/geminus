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
		if p.Route != nil {
			route = p.Route.Describe()
		}
		parts = append(parts, fmt.Sprintf("%s [%s] %s", p.Name, p.Tag(), route))
	}
	return strings.Join(parts, "; ")
}

func TestLoadListsAgentsFromHonchoWithRoutes(t *testing.T) {
	srv := honcho(t, "h1karu", "sibyl", "vvx", "chrome", "mirai")
	setup(t, srv.URL, `{"personas":{"sibyl":{"ssh":"btcvps","runtime":"hermes","cwd":"/home/hermes"}}}`)

	dir, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The human and the stray peer "vvx" are not personas; the local agent
	// needs no configured route; mirai has none.
	want := "chrome [THIS COMPUTER] hermes (local); mirai [REMOTE] no route; sibyl [REMOTE] hermes via ssh btcvps"
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
	if got := describe(dir); got != "chrome [THIS COMPUTER] hermes (local); sibyl [REMOTE] no route" {
		t.Errorf("personas = %s", got)
	}
}

func TestLoadFallsBackToConfiguredPersonasWhenHonchoIsDown(t *testing.T) {
	srv := honcho(t)
	srv.Close()
	setup(t, srv.URL, `{"personas":{"sibyl":{"ssh":"btcvps","runtime":"hermes","cwd":"/home/hermes"}}}`)

	dir, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := describe(dir); got != "chrome [THIS COMPUTER] hermes (local); sibyl [REMOTE] hermes via ssh btcvps" {
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

func TestLoadRejectsInvalidRoutes(t *testing.T) {
	for name, cfg := range map[string]string{
		"unknown runtime": `{"personas":{"sibyl":{"ssh":"btcvps","runtime":"gpt","cwd":"/x"}}}`,
		"option as alias": `{"personas":{"sibyl":{"ssh":"-oProxyCommand=evil","runtime":"hermes","cwd":"/x"}}}`,
		"ssh without cwd": `{"personas":{"sibyl":{"ssh":"btcvps","runtime":"hermes"}}}`,
		"broken json":     `{"personas":`,
	} {
		srv := honcho(t, "sibyl")
		setup(t, srv.URL, cfg)
		if _, err := Load(context.Background()); err == nil {
			t.Errorf("%s: Load succeeded, want an error", name)
		}
	}
}

func TestRouteExec(t *testing.T) {
	for _, tc := range []struct {
		route Route
		want  string
	}{
		{Route{Runtime: "hermes"}, "hermes acp"},
		{Route{Runtime: "claude"}, "claude-agent-acp"},
		{Route{Runtime: "hermes", Command: "/opt/hermes/bin/hermes acp"}, "/opt/hermes/bin/hermes acp"},
		{Route{SSH: "btcvps", Runtime: "hermes", CWD: "/home/hermes"},
			"ssh -T -oBatchMode=yes -oConnectTimeout=10 -- btcvps sh -lc 'exec hermes acp'"},
		{Route{SSH: "mirai", Runtime: "hermes", CWD: `C:\Users\h`, Command: "hermes acp"},
			"ssh -T -oBatchMode=yes -oConnectTimeout=10 -- mirai hermes acp"},
	} {
		cmd, err := tc.route.Exec()
		if err != nil {
			t.Errorf("%+v: %v", tc.route, err)
			continue
		}
		if got := strings.Join(cmd.Args, " "); got != tc.want {
			t.Errorf("%+v:\n got %s\nwant %s", tc.route, got, tc.want)
		}
	}
}
