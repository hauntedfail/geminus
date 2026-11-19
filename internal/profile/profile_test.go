package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T, honcho, group string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv(EnvHermesHome, home)
	for name, content := range map[string]string{"honcho.json": honcho, "geminise.json": group} {
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	goodHoncho = `{"baseUrl":"https://honcho.example","workspace":"ws","apiKey":"k","aiPeer":"chrome","peerName":"h1karu","other":1}`
	secretHex  = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	goodGroup  = `{"from":"chrome","members":{"h1karu":"human","chrome":"agent","mirai":"agent"},"secret":"` + secretHex + `","poll_seconds":10}`
)

func TestLoadReadsTheExistingFiles(t *testing.T) {
	setup(t, goodHoncho, goodGroup)
	p, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.BaseURL != "https://honcho.example" || p.Workspace != "ws" || p.APIKey != "k" || p.Human != "h1karu" || p.Local != "chrome" {
		t.Errorf("profile = %+v", p)
	}
	if len(p.Secret) != 32 || p.Secret[31] != 0x1f {
		t.Errorf("secret has %d bytes", len(p.Secret))
	}
	if !p.IsAgent("mirai") || p.IsAgent("h1karu") || p.IsAgent("stranger") {
		t.Error("IsAgent is wrong")
	}
}

func TestLoadSaysWhatIsMissingAndNeverPrintsSecrets(t *testing.T) {
	for name, files := range map[string][2]string{
		"no honcho.json":   {"", goodGroup},
		"no geminise.json": {goodHoncho, ""},
		"short secret":     {goodHoncho, `{"members":{"h1karu":"human"},"secret":"abcd"}`},
		"owner not human":  {goodHoncho, strings.Replace(goodGroup, `"h1karu":"human"`, `"h1karu":"agent"`, 1)},
		"no token":         {`{"baseUrl":"https://h","workspace":"ws","peerName":"h1karu"}`, goodGroup},
		"broken json":      {`{"baseUrl":`, goodGroup},
	} {
		setup(t, files[0], files[1])
		_, err := Load()
		if err == nil {
			t.Errorf("%s: Load succeeded", name)
			continue
		}
		if strings.Contains(err.Error(), secretHex) || strings.Contains(err.Error(), "abcd") {
			t.Errorf("%s: the error shows the secret: %v", name, err)
		}
	}
}
