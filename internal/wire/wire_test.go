package wire

import (
	"encoding/json"
	"testing"
)

func secret() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// The expected values were produced by the bridge's own code (geminish
// mailbox.sign with bridge.prompt_label, output_label and node_label), so a
// change on either side that breaks agreement fails here.
func TestSignaturesAgreeWithTheBridge(t *testing.T) {
	for _, tc := range []struct {
		content, label string
		ts             int64
		want           string
	}{
		{"hello 日本語", PromptLabel("sess", "mirai", "claude", "h1karu", "n1"), 1795000000,
			"a856fb25c392f505afceddbebd59d7cf5437b3188bc710455f72f4d643ebfc55"},
		{"out", OutputLabel("sess", "t1", 2, true, false), 1795000001,
			"ddc02e4938b08b6d4ecc38e9caef640d7080b6845aa75da3fe3381bfd598787b"},
		{"hermes,claude", NodeLabel("mirai"), 1795000002,
			"7e68ce915739878d1586115f55e92f545df07cf7a1451889c2ef40bd7e8e7a92"},
	} {
		if got := Sign(secret(), tc.content, tc.label, tc.ts); got != tc.want {
			t.Errorf("Sign(%q, %q) = %s, want %s", tc.content, tc.label, got, tc.want)
		}
		if !Verify(secret(), tc.content, tc.label, tc.ts, tc.want) {
			t.Errorf("Verify rejects its own signature for %q", tc.label)
		}
	}
}

func TestASignatureIsGoodForOnePurposeOnly(t *testing.T) {
	sig := Sign(secret(), "deploy", PromptLabel("s1", "mirai", "claude", "h1karu", "n"), 100)
	for name, ok := range map[string]bool{
		"other conversation": Verify(secret(), "deploy", PromptLabel("s2", "mirai", "claude", "h1karu", "n"), 100, sig),
		"other persona":      Verify(secret(), "deploy", PromptLabel("s1", "sibyl", "claude", "h1karu", "n"), 100, sig),
		"other harness":      Verify(secret(), "deploy", PromptLabel("s1", "mirai", "codex", "h1karu", "n"), 100, sig),
		"other nonce":        Verify(secret(), "deploy", PromptLabel("s1", "mirai", "claude", "h1karu", "m"), 100, sig),
		"other text":         Verify(secret(), "deploy!", PromptLabel("s1", "mirai", "claude", "h1karu", "n"), 100, sig),
		"other time":         Verify(secret(), "deploy", PromptLabel("s1", "mirai", "claude", "h1karu", "n"), 101, sig),
		"as output":          Verify(secret(), "deploy", OutputLabel("s1", "n", 1, true, true), 100, sig),
		"other secret":       Verify(make([]byte, 32), "deploy", PromptLabel("s1", "mirai", "claude", "h1karu", "n"), 100, sig),
	} {
		if ok {
			t.Errorf("signature accepted for %s", name)
		}
	}
}

func card(peer string, ids ...string) json.RawMessage {
	c := NodeCard{V: 1, TS: 500}
	joined := ""
	for i, id := range ids {
		c.Harnesses = append(c.Harnesses, Harness{ID: id, Name: "Name of " + id})
		if i > 0 {
			joined += ","
		}
		joined += id
	}
	c.Sig = Sign(secret(), joined, NodeLabel(peer), c.TS)
	raw, _ := json.Marshal(c)
	return raw
}

func TestNodeCardMustBeSignedForItsOwnPeer(t *testing.T) {
	got, ok := ParseNodeCard(secret(), "mirai", card("mirai", "claude", "codex"))
	if !ok || len(got.Harnesses) != 2 || got.Harnesses[0].Name != "Name of claude" || got.TS != 500 {
		t.Fatalf("card = %+v, %v", got, ok)
	}
	if _, ok := ParseNodeCard(secret(), "sibyl", card("mirai", "claude")); ok {
		t.Error("a card signed for mirai was accepted for sibyl")
	}
	// Someone with the Honcho token adds a harness to the list.
	var forged NodeCard
	_ = json.Unmarshal(card("mirai", "claude"), &forged)
	forged.Harnesses = append(forged.Harnesses, Harness{ID: "evil"})
	raw, _ := json.Marshal(forged)
	if _, ok := ParseNodeCard(secret(), "mirai", raw); ok {
		t.Error("a card with an added harness was accepted")
	}
	for _, raw := range []string{"", "null", "{}", `"x"`} {
		if _, ok := ParseNodeCard(secret(), "mirai", json.RawMessage(raw)); ok {
			t.Errorf("accepted %q as a card", raw)
		}
	}
}

func TestDoorbellEndpointMustBeSignedAndMozillas(t *testing.T) {
	record := func(peer, endpoint string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"v": 1, "endpoint": endpoint, "ts": 7,
			"sig": Sign(secret(), endpoint, "push-endpoint:"+peer, 7)})
		return raw
	}
	good := "https://updates.push.services.mozilla.com/wpush/v2/abc"
	if got, ok := Endpoint(secret(), "mirai", record("mirai", good)); !ok || got != good {
		t.Errorf("valid endpoint rejected: %q, %v", got, ok)
	}
	for name, raw := range map[string]json.RawMessage{
		"other peer":   record("sibyl", good),
		"other host":   record("mirai", "https://evil.example/wpush/v2/abc"),
		"http":         record("mirai", "http://updates.push.services.mozilla.com/wpush/v2/abc"),
		"other path":   record("mirai", "https://updates.push.services.mozilla.com/other"),
		"with user":    record("mirai", "https://u@updates.push.services.mozilla.com/wpush/x"),
		"with port":    record("mirai", "https://updates.push.services.mozilla.com:8443/wpush/x"),
		"not a record": json.RawMessage(`"x"`),
	} {
		if _, ok := Endpoint(secret(), "mirai", raw); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}
