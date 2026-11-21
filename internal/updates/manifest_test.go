package updates

import (
	"encoding/json"
	"strings"
	"testing"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func hash(c byte) string { return strings.Repeat(string(c), 64) }

func sealed() Manifest {
	return Seal(secret, "0.5.0", map[string]File{
		"windows-amd64": {SHA256: hash('a'), Size: 7},
		"linux-amd64":   {SHA256: hash('b'), Size: 9},
	}, 1795300000)
}

func TestASealedManifestVerifiesAndSurvivesJSON(t *testing.T) {
	m := sealed()
	if !m.Verify(secret) {
		t.Fatal("a manifest just sealed does not verify")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"version":"0.5.0"`, `"ts":1795300000`, `"files":{`, `"sha256":"` + hash('a') + `"`, `"size":7`, `"sig":"`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("the JSON lacks %s: %s", field, raw)
		}
	}
	var back Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Verify(secret) {
		t.Error("the manifest read back does not verify")
	}
}

func TestAManifestChangedAnywhereDoesNotVerify(t *testing.T) {
	change := map[string]func(*Manifest){
		"another secret":  nil,
		"version":         func(m *Manifest) { m.Version = "0.5.1" },
		"time":            func(m *Manifest) { m.TS++ },
		"a hash":          func(m *Manifest) { m.Files["linux-amd64"] = File{SHA256: hash('c'), Size: 9} },
		"a size":          func(m *Manifest) { m.Files["linux-amd64"] = File{SHA256: hash('b'), Size: 10} },
		"a file added":    func(m *Manifest) { m.Files["darwin-arm64"] = File{SHA256: hash('d'), Size: 1} },
		"a file removed":  func(m *Manifest) { delete(m.Files, "linux-amd64") },
		"a file renamed":  func(m *Manifest) { m.Files["linux-arm64"] = m.Files["linux-amd64"]; delete(m.Files, "linux-amd64") },
		"no signature":    func(m *Manifest) { m.Sig = "" },
		"wrong signature": func(m *Manifest) { m.Sig = hash('0') },
	}
	for name, f := range change {
		m := sealed()
		key := secret
		if f == nil {
			key = []byte("ffffffffffffffffffffffffffffffff")
		} else {
			f(&m)
		}
		if m.Verify(key) {
			t.Errorf("%s: still verifies", name)
		}
	}
}

func TestAManifestThatIsNotAReleaseNeverVerifies(t *testing.T) {
	for name, m := range map[string]Manifest{
		"a build version": Seal(secret, "0.5.0-dev", map[string]File{"linux-amd64": {SHA256: hash('a'), Size: 1}}, 1),
		"a bad target":    Seal(secret, "0.5.0", map[string]File{"../etc": {SHA256: hash('a'), Size: 1}}, 1),
		"a short hash":    Seal(secret, "0.5.0", map[string]File{"linux-amd64": {SHA256: "abcd", Size: 1}}, 1),
		"an upper hash":   Seal(secret, "0.5.0", map[string]File{"linux-amd64": {SHA256: strings.Repeat("A", 64), Size: 1}}, 1),
		"an empty file":   Seal(secret, "0.5.0", map[string]File{"linux-amd64": {SHA256: hash('a'), Size: 0}}, 1),
		"a huge file":     Seal(secret, "0.5.0", map[string]File{"linux-amd64": {SHA256: hash('a'), Size: MaxProgram + 1}}, 1),
		"an endless file": Seal(secret, "0.5.0", map[string]File{"linux-amd64": {SHA256: hash('a'), Size: 1<<63 - 1}}, 1),
		"no files":        Seal(secret, "0.5.0", nil, 1),
	} {
		if m.Verify(secret) {
			t.Errorf("%s: verifies", name)
		}
	}
}
