package updates

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
)

var (
	// secret stands in for what the group shares.
	secret = []byte("0123456789abcdef0123456789abcdef")
	// key stands in for the key of the computer that builds releases.
	key = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
)

func public(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

func hash(c byte) string { return strings.Repeat(string(c), 64) }

func sealed() Manifest {
	return Seal(key, "0.5.0", map[string]File{
		"windows-amd64": {SHA256: hash('a'), Size: 7},
		"linux-amd64":   {SHA256: hash('b'), Size: 9},
	}, 1795300000)
}

func TestASealedManifestVerifiesAndSurvivesJSON(t *testing.T) {
	m := sealed()
	if !m.Verify(public(key)) {
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
	if !back.Verify(public(key)) {
		t.Error("the manifest read back does not verify")
	}
}

func TestAManifestChangedAnywhereDoesNotVerify(t *testing.T) {
	change := map[string]func(*Manifest){
		"another key":     nil,
		"version":         func(m *Manifest) { m.Version = "0.5.1" },
		"time":            func(m *Manifest) { m.TS++ },
		"a hash":          func(m *Manifest) { m.Files["linux-amd64"] = File{SHA256: hash('c'), Size: 9} },
		"a size":          func(m *Manifest) { m.Files["linux-amd64"] = File{SHA256: hash('b'), Size: 10} },
		"a file added":    func(m *Manifest) { m.Files["darwin-arm64"] = File{SHA256: hash('d'), Size: 1} },
		"a file removed":  func(m *Manifest) { delete(m.Files, "linux-amd64") },
		"a file renamed":  func(m *Manifest) { m.Files["linux-arm64"] = m.Files["linux-amd64"]; delete(m.Files, "linux-amd64") },
		"no signature":    func(m *Manifest) { m.Sig = "" },
		"wrong signature": func(m *Manifest) { m.Sig = hash('0') + hash('0') },
		"not hex":         func(m *Manifest) { m.Sig = strings.Repeat("z", 128) },
	}
	for name, f := range change {
		m := sealed()
		with := public(key)
		if f == nil {
			with = public(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize)))
		} else {
			f(&m)
		}
		if m.Verify(with) {
			t.Errorf("%s: still verifies", name)
		}
	}
	if sealed().Verify(nil) || sealed().Verify(ed25519.PublicKey("short")) {
		t.Error("a manifest verifies without a key")
	}
}

// What the group shares cannot make a release: the signature is not an HMAC
// with a secret every member holds.
func TestTheGroupSecretCannotSignARelease(t *testing.T) {
	m := sealed()
	for _, forged := range []ed25519.PrivateKey{
		ed25519.NewKeyFromSeed(secret),
		ed25519.NewKeyFromSeed(public(key)),
	} {
		m.Sig = Seal(forged, m.Version, m.Files, m.TS).Sig
		if m.Verify(public(key)) {
			t.Error("a manifest signed with something every member knows verifies")
		}
	}
}

func TestAManifestThatIsNotAReleaseNeverVerifies(t *testing.T) {
	for name, m := range map[string]Manifest{
		"a build version": Seal(key, "0.5.0-dev", map[string]File{"linux-amd64": {SHA256: hash('a'), Size: 1}}, 1),
		"a bad target":    Seal(key, "0.5.0", map[string]File{"../etc": {SHA256: hash('a'), Size: 1}}, 1),
		"a short hash":    Seal(key, "0.5.0", map[string]File{"linux-amd64": {SHA256: "abcd", Size: 1}}, 1),
		"an upper hash":   Seal(key, "0.5.0", map[string]File{"linux-amd64": {SHA256: strings.Repeat("A", 64), Size: 1}}, 1),
		"an empty file":   Seal(key, "0.5.0", map[string]File{"linux-amd64": {SHA256: hash('a'), Size: 0}}, 1),
		"a huge file":     Seal(key, "0.5.0", map[string]File{"linux-amd64": {SHA256: hash('a'), Size: MaxProgram + 1}}, 1),
		"an endless file": Seal(key, "0.5.0", map[string]File{"linux-amd64": {SHA256: hash('a'), Size: 1<<63 - 1}}, 1),
		"no files":        Seal(key, "0.5.0", nil, 1),
	} {
		if m.Verify(public(key)) {
			t.Errorf("%s: verifies", name)
		}
	}
}

func TestAPublicKeyIsWrittenAndReadAsHex(t *testing.T) {
	text := PublicKeyText(public(key))
	if len(text) != 64 || strings.ToLower(text) != text {
		t.Errorf("text = %q", text)
	}
	back, err := ParsePublicKey(text + "\n")
	if err != nil || !back.Equal(public(key)) {
		t.Errorf("ParsePublicKey = %x, %v", back, err)
	}
	for _, bad := range []string{"", "abcd", strings.Repeat("z", 64), text + "00"} {
		if k, err := ParsePublicKey(bad); err == nil {
			t.Errorf("ParsePublicKey(%q) = %x", bad, k)
		}
	}
}

// Every geminus built from this source trusts this key and no other.
func TestTheReleaseKeyBuiltInIsAKey(t *testing.T) {
	if k := ReleaseKey(); len(k) != ed25519.PublicKeySize {
		t.Fatalf("the built-in release key %q is not a public key", releaseKey)
	}
	before := releaseKey
	defer func() { releaseKey = before }()
	releaseKey = "not a key"
	if k := ReleaseKey(); k != nil {
		t.Errorf("ReleaseKey of a broken value = %x", k)
	}
}
