package wire

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"
)

func testModeEdAuth() HumanAuth {
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	return HumanAuth{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}
}

func TestModeSignaturesBindSettingsWithoutChangingLegacyLabels(t *testing.T) {
	for _, auth := range []HumanAuth{{Secret: secret()}, testModeEdAuth()} {
		for _, tuning := range []Tuning{{}, {Model: "m", Effort: "high"}, {Mode: "manual"}, {Model: "m", Mode: "bypass"}} {
			label := PromptLabel("session", "peer", "claude", "human", "nonce", "/project", tuning)
			sig, alg := auth.Sign("prompt", label, 100)
			if !auth.Verify("prompt", label, 100, sig, alg) {
				t.Fatal("valid prompt rejected")
			}
			changed := tuning
			changed.Mode = "read-only"
			if auth.Verify("prompt", PromptLabel("session", "peer", "claude", "human", "nonce", "/project", changed), 100, sig, alg) {
				t.Fatal("mode tampering accepted")
			}
			s := SealSessionWith(auth, "session", Session{V: 1, Human: "human", Persona: "peer", Harness: "claude", CWD: "/project", Mode: tuning.Mode, Model: tuning.Model, Effort: tuning.Effort})
			if !VerifySessionWith(auth, "session", s) {
				t.Fatal("session rejected")
			}
			s.Mode = "read-only"
			if VerifySessionWith(auth, "session", s) {
				t.Fatal("session mode tampering accepted")
			}
		}
	}
}

func TestHostDefaultsRequireSeparateSignature(t *testing.T) {
	c := NodeCard{V: 1, TS: 100, Harnesses: []Harness{{ID: "claude", Options: []string{"model", "effort", "mode"}, Defaults: map[string]string{"model": "host-model", "effort": "high", "mode": "manual"}}}}
	c.Sig = Sign(secret(), "claude", NodeLabel("peer"), c.TS)
	c.OptionsSig = Sign(secret(), CardText(c.Harnesses), OptionsLabel("peer"), c.TS)
	parse := func() NodeCard {
		t.Helper()
		raw, _ := json.Marshal(c)
		got, ok := ParseNodeCard(secret(), "peer", raw)
		if !ok {
			t.Fatal("card rejected")
		}
		return got
	}
	if got := parse(); got.Harnesses[0].DefaultFor("model") != "" {
		t.Fatal("unsigned defaults trusted")
	}
	c.DefaultsSig = Sign(secret(), HostDefaultsText(c.Harnesses), HostDefaultsLabel("peer"), c.TS)
	if got := parse(); got.Harnesses[0].DefaultFor("model") != "host-model" {
		t.Fatal("signed host default lost")
	}
	c.Harnesses[0].Defaults["model"] = "forged-model"
	got := parse()
	if got.Harnesses[0].DefaultFor("model") != "" || !got.Harnesses[0].Takes("mode") {
		t.Fatal("tampering not isolated from legacy options")
	}
}
