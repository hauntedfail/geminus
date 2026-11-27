package profile

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProfileFixture(t *testing.T, dir string, group map[string]any) {
	t.Helper()
	honchoCfg := map[string]any{
		"baseUrl":   "https://honcho.haunted.fail",
		"workspace": "sibyl",
		"apiKey":    "tok-test",
		"aiPeer":    "sibyl",
		"peerName":  "h1karu",
	}
	hb, _ := json.Marshal(honchoCfg)
	if err := os.WriteFile(filepath.Join(dir, "honcho.json"), hb, 0o600); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(group)
	if err := os.WriteFile(filepath.Join(dir, "geminise.json"), gb, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadE2EEURLPrecedenceAndValidation(t *testing.T) {
	t.Setenv(EnvE2EEURL, "")
	secHex := hex.EncodeToString(make([]byte, 32))

	dir := t.TempDir()
	writeProfileFixture(t, dir, map[string]any{
		"from":    "sibyl",
		"members": map[string]string{"h1karu": "human", "sibyl": "agent"},
		"secret":  secHex,
		"e2ee_url": "https://sibyl.haunted.fail/geminise/e2ee",
	})
	p, err := loadFrom(dir)
	if err != nil {
		t.Fatalf("loadFrom top-level e2ee_url: %v", err)
	}
	if p.E2EEURL != "https://sibyl.haunted.fail/geminise/e2ee" {
		t.Fatalf("E2EEURL = %q", p.E2EEURL)
	}

	// geminus.e2ee_url overrides top-level e2ee_url.
	writeProfileFixture(t, dir, map[string]any{
		"from":     "sibyl",
		"members":  map[string]string{"h1karu": "human", "sibyl": "agent"},
		"secret":   secHex,
		"e2ee_url": "https://sibyl.haunted.fail/old",
		"geminus":  map[string]any{"e2ee_url": "https://honcho.haunted.fail/v3/e2ee"},
	})
	p, err = loadFrom(dir)
	if err != nil {
		t.Fatalf("loadFrom geminus.e2ee_url: %v", err)
	}
	if p.E2EEURL != "https://honcho.haunted.fail/v3/e2ee" {
		t.Fatalf("E2EEURL = %q", p.E2EEURL)
	}

	// GEMINUS_E2EE_URL env overrides config file.
	t.Setenv(EnvE2EEURL, "http://127.0.0.1:8653/geminise/e2ee")
	p, err = loadFrom(dir)
	if err != nil {
		t.Fatalf("loadFrom env override: %v", err)
	}
	if p.E2EEURL != "http://127.0.0.1:8653/geminise/e2ee" {
		t.Fatalf("E2EEURL = %q", p.E2EEURL)
	}

	// Non-loopback plain http must be rejected.
	t.Setenv(EnvE2EEURL, "http://evil.example/e2ee")
	if _, err := loadFrom(dir); err == nil || !strings.Contains(err.Error(), "e2ee_url") {
		t.Fatalf("expected invalid e2ee_url error, got %v", err)
	}
}
