package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNodeCardCapabilitiesSignedAndVerified(t *testing.T) {
	secret := []byte("test-group-secret")
	peer := "chrome"
	ts := int64(1795590000)
	harnesses := []Harness{{ID: "hermes", Name: "Hermes", Options: []string{"model"}}}
	caps := []string{CapModelMenu, CapEd25519, CapMultiPubkey}

	card := NodeCard{
		V:             1,
		Harnesses:     harnesses,
		TS:            ts,
		Sig:           Sign(secret, "hermes", NodeLabel(peer), ts),
		OptionsSig:    Sign(secret, CardText(harnesses), OptionsLabel(peer), ts),
		Proto:         ProtocolVersion,
		BridgeVersion: "0.9.2",
		Capabilities:  caps,
		CapsSig:       Sign(secret, CapsText(ProtocolVersion, "0.9.2", caps), CapsLabel(peer), ts),
	}
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ParseNodeCard(secret, peer, raw)
	if !ok {
		t.Fatal("ParseNodeCard returned false for valid card with capabilities")
	}
	if !got.CapabilitiesVerified() {
		t.Fatal("CapabilitiesVerified = false, want true")
	}
	if got.Proto != ProtocolVersion || got.BridgeVersion != "0.9.2" {
		t.Fatalf("got proto=%d ver=%q", got.Proto, got.BridgeVersion)
	}
	if !got.HasCapability(CapEd25519) || !got.HasCapability(CapModelMenu) || got.HasCapability("nonexistent") {
		t.Fatalf("unexpected capabilities: %+v", got.Capabilities)
	}

	// Tampering with capabilities strips them while keeping the base card valid.
	card.Capabilities = append(card.Capabilities, CapProviderModel)
	rawTampered, _ := json.Marshal(card)
	tampered, ok := ParseNodeCard(secret, peer, rawTampered)
	if !ok {
		t.Fatal("ParseNodeCard rejected valid base card when caps were tampered")
	}
	if tampered.CapabilitiesVerified() || tampered.HasCapability(CapEd25519) || tampered.Proto != 0 {
		t.Fatalf("tampered capabilities were not stripped: %+v", tampered)
	}
}

func TestBridgeCompatibilityAndRefusalHints(t *testing.T) {
	newer := NodeCard{Proto: 2, BridgeVersion: "1.0.0", Capabilities: []string{CapEd25519}, CapsSig: "ok"}
	warns := CheckBridgeCompatibility("sibyl", newer, true)
	if len(warns) != 1 || !strings.Contains(warns[0], "wire v2") || !strings.Contains(warns[0], "/update") {
		t.Fatalf("newer bridge warnings = %v", warns)
	}

	noEd := NodeCard{Proto: 1, BridgeVersion: "0.8.0", Capabilities: []string{CapModelMenu}, CapsSig: "ok"}
	warns = CheckBridgeCompatibility("mirai", noEd, true)
	if len(warns) != 1 || !strings.Contains(warns[0], CapEd25519) || !strings.Contains(warns[0], MinEd25519BridgeVersion) {
		t.Fatalf("missing ed25519 warnings = %v", warns)
	}

	legacy := NodeCard{} // no CapsSig
	if got := CheckBridgeCompatibility("mirai", legacy, true); len(got) != 0 {
		t.Fatalf("legacy card should not emit proactive warning: %v", got)
	}
	hint := RefusalHint("mirai", legacy, "the signature does not verify", true)
	if !strings.Contains(hint, "did not advertise capabilities") || !strings.Contains(hint, MinEd25519BridgeVersion) {
		t.Fatalf("legacy refusal hint = %q", hint)
	}

	hintNoEd := RefusalHint("mirai", noEd, "unsupported signature algorithm", true)
	if !strings.Contains(hintNoEd, "0.8.0") || !strings.Contains(hintNoEd, MinEd25519BridgeVersion) {
		t.Fatalf("noEd refusal hint = %q", hintNoEd)
	}

	okCard := NodeCard{Proto: 1, BridgeVersion: "0.9.2", Capabilities: []string{CapEd25519}, CapsSig: "ok"}
	if got := RefusalHint("mirai", okCard, "the signature does not verify", true); got != "" {
		t.Fatalf("compatible bridge should not emit upgrade hint: %q", got)
	}
}
