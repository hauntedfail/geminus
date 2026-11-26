package wire

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
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
		{"hello 日本語", PromptLabel("sess", "mirai", "claude", "h1karu", "n1", "/home/me/project", Tuning{Model: "opus", Effort: "high"}), 1795000000,
			"5a1781f2c08ee07237079aa3deb30d7d816241fe97dac7ac0b6eb903af2ef2f1"},
		{"hello 日本語", PromptLabel("sess", "mirai", "claude", "h1karu", "n1", "/home/me/project", Tuning{}), 1795000000,
			"dd4d6b5ee2e6b602decc820fc986e36b87e22079f181ac4da53ae686d3eef4cc"}, // as geminish 0.5 signs it
		{"out", OutputLabel("sess", "t1", 2, true, false, "native-7"), 1795000001,
			"e58aa5fc372066fa1e61ee6976f21f846df5c9ae0a78b98ddeb32aaeef7c6eba"},
		{"out", OutputLabel("sess", "t1", 1, false, true, ""), 1795000001,
			"feb5333a3f863f03101a38f5c1ec535f300961a6e3222db2dd0bcab431b68127"},
		{"hermes,claude", NodeLabel("mirai"), 1795000002,
			"7e68ce915739878d1586115f55e92f545df07cf7a1451889c2ef40bd7e8e7a92"},
		{CardText([]Harness{{ID: "hermes", Options: []string{"model", "effort"}}, {ID: "claude", Options: []string{"model", "effort"}},
			{ID: "opencode", Options: []string{"model"}}}), OptionsLabel("mirai"), 1795000002,
			"5cbeccf080d31c381eab5d0fb27ea0096b2ae772026ec78924ceea150cdad14b"},
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
	tuned := Tuning{Model: "opus", Effort: "high"}
	label := func(session, persona, harness, nonce, cwd string) string {
		return PromptLabel(session, persona, harness, "h1karu", nonce, cwd, tuned)
	}
	with := func(t Tuning) string { return PromptLabel("s1", "mirai", "claude", "h1karu", "n", "/work", t) }
	sig := Sign(secret(), "deploy", label("s1", "mirai", "claude", "n", "/work"), 100)
	for name, ok := range map[string]bool{
		"other conversation": Verify(secret(), "deploy", label("s2", "mirai", "claude", "n", "/work"), 100, sig),
		"other persona":      Verify(secret(), "deploy", label("s1", "sibyl", "claude", "n", "/work"), 100, sig),
		"other harness":      Verify(secret(), "deploy", label("s1", "mirai", "codex", "n", "/work"), 100, sig),
		"other nonce":        Verify(secret(), "deploy", label("s1", "mirai", "claude", "m", "/work"), 100, sig),
		"other directory":    Verify(secret(), "deploy", label("s1", "mirai", "claude", "n", "/"), 100, sig),
		"other model":        Verify(secret(), "deploy", with(Tuning{Model: "haiku", Effort: "high"}), 100, sig),
		"other effort":       Verify(secret(), "deploy", with(Tuning{Model: "opus", Effort: "max"}), 100, sig),
		"no model":           Verify(secret(), "deploy", with(Tuning{Effort: "high"}), 100, sig),
		"settings swapped":   Verify(secret(), "deploy", with(Tuning{Model: "high", Effort: "opus"}), 100, sig),
		"settings taken off": Verify(secret(), "deploy", with(Tuning{}), 100, sig),
		"other text":         Verify(secret(), "deploy!", label("s1", "mirai", "claude", "n", "/work"), 100, sig),
		"other time":         Verify(secret(), "deploy", label("s1", "mirai", "claude", "n", "/work"), 101, sig),
		"as output":          Verify(secret(), "deploy", OutputLabel("s1", "n", 1, true, true, ""), 100, sig),
		"other secret":       Verify(make([]byte, 32), "deploy", label("s1", "mirai", "claude", "n", "/work"), 100, sig),
	} {
		if ok {
			t.Errorf("signature accepted for %s", name)
		}
	}
	// A directory cannot be made to look like the fields beside it.
	if label("s1", "mirai", "claude", "n:x", "") == label("s1", "mirai", "claude", "n", "x") {
		t.Error("label fields can run into each other")
	}
	out := Sign(secret(), "done", OutputLabel("s1", "t", 1, true, true, "native-a"), 100)
	if Verify(secret(), "done", OutputLabel("s1", "t", 1, true, true, "native-b"), 100, out) {
		t.Error("an output signature was accepted for another program session")
	}
}

func TestDefaultsMustBeSignedForTheirOwner(t *testing.T) {
	d := Defaults{Persona: "mirai", Personas: map[string]PersonaDefaults{"mirai": {Harness: "claude", CWD: "/work"}}}
	stored, err := SealDefaults(secret(), "h1karu", d, 42)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stored)
	got, saved, ok := OpenDefaults(secret(), "h1karu", raw)
	if !ok || saved != 42 || got.Persona != "mirai" || got.Personas["mirai"] != d.Personas["mirai"] {
		t.Fatalf("defaults = %+v, saved %d, %v", got, saved, ok)
	}
	if _, _, ok := OpenDefaults(secret(), "someone", raw); ok {
		t.Error("defaults sealed for h1karu opened for someone else")
	}
	// What a token holder could write: plain defaults, or sealed ones with the data changed.
	plain, _ := json.Marshal(d)
	changed := []byte(strings.Replace(string(raw), "mirai", "sibyl", 1))
	for name, forged := range map[string][]byte{"plain": plain, "changed": changed, "empty": nil, "junk": []byte(`"x"`)} {
		if _, _, ok := OpenDefaults(secret(), "h1karu", forged); ok {
			t.Errorf("%s defaults were accepted", name)
		}
	}
}

func TestSessionDescriptionMustBeSigned(t *testing.T) {
	s := SealSession(secret(), "geminus-mirai-1", Session{V: 1, Human: "h1karu", Persona: "mirai", Harness: "claude",
		CWD: "/work", Model: "opus", Effort: "high", Title: "fix the build", Updated: 500})
	if !VerifySession(secret(), "geminus-mirai-1", s) {
		t.Fatal("a sealed description does not verify")
	}
	// The bridge flips pending; that must not break the seal.
	flipped := s
	flipped.Pending = !flipped.Pending
	if !VerifySession(secret(), "geminus-mirai-1", flipped) {
		t.Error("pending is part of the seal")
	}
	change := func(f func(*Session)) Session { c := s; f(&c); return c }
	for name, bad := range map[string]Session{
		"other directory": change(func(c *Session) { c.CWD = "/" }),
		"other harness":   change(func(c *Session) { c.Harness = "codex" }),
		"other model":     change(func(c *Session) { c.Model = "haiku" }),
		"other effort":    change(func(c *Session) { c.Effort = "" }),
		"other persona":   change(func(c *Session) { c.Persona = "sibyl" }),
		"other owner":     change(func(c *Session) { c.Human = "someone" }),
		"other title":     change(func(c *Session) { c.Title = "x" }),
		"made newer":      change(func(c *Session) { c.Updated = 9999999999 }),
		"no signature":    change(func(c *Session) { c.Sig = "" }),
	} {
		if VerifySession(secret(), "geminus-mirai-1", bad) {
			t.Errorf("%s: accepted", name)
		}
	}
	if VerifySession(secret(), "geminus-mirai-2", s) {
		t.Error("a description sealed for one session was accepted for another")
	}
}

func card(peer string, ids ...string) json.RawMessage {
	c := NodeCard{V: 1, TS: 500}
	for _, id := range ids {
		c.Harnesses = append(c.Harnesses, Harness{ID: id, Name: "Name of " + id, Options: []string{"model"}})
	}
	c.Sig = Sign(secret(), strings.Join(ids, ","), NodeLabel(peer), c.TS)
	c.OptionsSig = Sign(secret(), CardText(c.Harnesses), OptionsLabel(peer), c.TS)
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
	// Or claims that a program takes a setting its computer did not list:
	// the programs are still believed, the options are not.
	_ = json.Unmarshal(card("mirai", "claude"), &forged)
	forged.Harnesses[0].Options = []string{"model", "effort"}
	raw, _ = json.Marshal(forged)
	if c, ok := ParseNodeCard(secret(), "mirai", raw); !ok || len(c.Harnesses[0].Options) != 0 {
		t.Errorf("a card with an added option: %+v, %v", c, ok)
	}
	// A card from a bridge older than options (geminish 0.5) is read as before.
	old := NodeCard{V: 1, TS: 500, Harnesses: []Harness{{ID: "hermes", Name: "Hermes Agent"}}}
	old.Sig = Sign(secret(), "hermes", NodeLabel("mirai"), old.TS)
	raw, _ = json.Marshal(old)
	if c, ok := ParseNodeCard(secret(), "mirai", raw); !ok || c.Harnesses[0].ID != "hermes" || c.Harnesses[0].Takes("model") {
		t.Errorf("an older card: %+v, %v", c, ok)
	}
	// An option cannot be folded into an ID.
	_ = json.Unmarshal(card("mirai", "claude"), &forged)
	forged.Harnesses[0] = Harness{ID: "claude:model"}
	raw, _ = json.Marshal(forged)
	if _, ok := ParseNodeCard(secret(), "mirai", raw); ok {
		t.Error("a card with an option folded into the ID was accepted")
	}
	if !got.Harnesses[0].Takes("model") || got.Harnesses[0].Takes("effort") {
		t.Errorf("options of %+v", got.Harnesses[0])
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

// The same values the bridge accepts and refuses (geminish tests/test_bridge.py).
func TestASettingMustBeABareName(t *testing.T) {
	for _, good := range []string{"", "opus", "claude-opus-5-5", "provider/some-model:v1.5", "a/b#high", "sonnet[1m]", "gpt-5.2"} {
		if !ValidSetting("model", good) {
			t.Errorf("model %q refused", good)
		}
	}
	for _, bad := range []string{"--dangerously-skip-permissions", "-x", "a b", "opus\n--yolo", strings.Repeat("x", 200), "モデル", "a\"b"} {
		if ValidSetting("model", bad) {
			t.Errorf("model %q accepted", bad)
		}
	}
	for _, good := range []string{"", "low", "xhigh", "none", "very_high", "max-1"} {
		if !ValidSetting("effort", good) {
			t.Errorf("effort %q refused", good)
		}
	}
	for _, bad := range []string{"--help", "high\"\nsandbox_mode=\"danger-full-access", "a=b", "a/b", "a.b", strings.Repeat("x", 40)} {
		if ValidSetting("effort", bad) {
			t.Errorf("effort %q accepted", bad)
		}
	}
}

func TestNodeCardSaysWhereAPromptRunsWhenNoDirectoryIsChosen(t *testing.T) {
	signed := func(peer, dir string) json.RawMessage {
		var c NodeCard
		_ = json.Unmarshal(card("mirai", "claude"), &c)
		c.CWD, c.CWDSig = dir, Sign(secret(), dir, CWDLabel(peer), c.TS)
		raw, _ := json.Marshal(c)
		return raw
	}
	if c, ok := ParseNodeCard(secret(), "mirai", signed("mirai", `C:\Users\me`)); !ok || c.CWD != `C:\Users\me` {
		t.Errorf("card = %+v, %v", c, ok)
	}
	// A directory that computer did not sign is not shown; the rest of the
	// card still stands.
	var forged NodeCard
	_ = json.Unmarshal(signed("mirai", "/home/me"), &forged)
	forged.CWD = "/somewhere/else"
	raw, _ := json.Marshal(forged)
	if c, ok := ParseNodeCard(secret(), "mirai", raw); !ok || c.CWD != "" || !c.Harnesses[0].Takes("model") {
		t.Errorf("a card with a changed directory: %+v, %v", c, ok)
	}
	if c, ok := ParseNodeCard(secret(), "mirai", signed("sibyl", "/home/me")); !ok || c.CWD != "" {
		t.Errorf("a directory signed for another peer: %+v, %v", c, ok)
	}
	// The signature is for this purpose only.
	_ = json.Unmarshal(card("mirai", "claude"), &forged)
	forged.CWD, forged.CWDSig = "claude", forged.Sig
	raw, _ = json.Marshal(forged)
	if c, ok := ParseNodeCard(secret(), "mirai", raw); !ok || c.CWD != "" {
		t.Errorf("the list signature passed for a directory: %+v, %v", c, ok)
	}
	// A card from a bridge that does not say (geminish 0.6 and older).
	if c, ok := ParseNodeCard(secret(), "mirai", card("mirai", "claude")); !ok || c.CWD != "" {
		t.Errorf("an older card: %+v, %v", c, ok)
	}
}

func TestEd25519HumanAuthSeparatesTrustFromGroupSecret(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := HumanAuth{Secret: secret(), Priv: priv, Pub: pub, RequireEd25519: true}
	verifier := HumanAuth{Secret: secret(), Pub: pub, RequireEd25519: true}
	legacy := HumanAuth{Secret: secret()}

	label := PromptLabel("s1", "chrome", "claude", "h1karu", "n1", "/work", Tuning{Model: "opus", Effort: "high"})
	sig, alg := signer.Sign("run tests", label, 1795000000)
	if alg != AlgEd25519 || len(sig) != 128 {
		t.Fatalf("unexpected ed25519 signature: alg=%q len=%d", alg, len(sig))
	}
	if !verifier.Verify("run tests", label, 1795000000, sig, alg) {
		t.Fatal("valid ed25519 signature rejected")
	}
	// Wrong key, wrong content, wrong label, wrong ts, or stripped sig_alg must fail.
	if verifier.Verify("run tests!", label, 1795000000, sig, alg) {
		t.Error("tampered content accepted")
	}
	if verifier.Verify("run tests", label+"x", 1795000000, sig, alg) {
		t.Error("tampered label accepted")
	}
	if verifier.Verify("run tests", label, 1795000001, sig, alg) {
		t.Error("tampered ts accepted")
	}
	if verifier.Verify("run tests", label, 1795000000, sig, "") {
		t.Error("ed25519 signature accepted as HMAC")
	}
	otherSig, _ := (HumanAuth{Priv: otherPriv, Pub: otherPub}).Sign("run tests", label, 1795000000)
	if verifier.Verify("run tests", label, 1795000000, otherSig, AlgEd25519) {
		t.Error("signature from another Ed25519 key accepted")
	}

	// A peer holding only the shared HMAC secret cannot forge a prompt when RequireEd25519 is true.
	hmacSig, hmacAlg := legacy.Sign("rm -rf ~", label, 1795000000)
	if verifier.Verify("rm -rf ~", label, 1795000000, hmacSig, hmacAlg) {
		t.Error("HMAC signature accepted under RequireEd25519")
	}
	dual := HumanAuth{Secret: secret(), Pub: pub, RequireEd25519: false}
	if !dual.Verify("rm -rf ~", label, 1795000000, hmacSig, hmacAlg) {
		t.Error("HMAC signature rejected under dual mode")
	}

	// Session and defaults round-trip with Ed25519.
	sess := SealSessionWith(signer, "s1", Session{V: 1, Human: "h1karu", Persona: "chrome", Harness: "claude", Title: "hi", Updated: 42})
	if sess.SigAlg != AlgEd25519 || !VerifySessionWith(verifier, "s1", sess) {
		t.Fatalf("ed25519 session failed verification: %+v", sess)
	}
	hmacSess := SealSession(secret(), "s1", Session{V: 1, Human: "h1karu", Persona: "chrome", Harness: "claude", Title: "hi", Updated: 42})
	if VerifySessionWith(verifier, "s1", hmacSess) {
		t.Error("HMAC session accepted under RequireEd25519")
	}

	rawBox, err := SealDefaultsWith(signer, "h1karu", Defaults{Persona: "chrome"}, 99)
	if err != nil {
		t.Fatal(err)
	}
	rawJSON, _ := json.Marshal(rawBox)
	gotD, saved, ok := OpenDefaultsWith(verifier, "h1karu", rawJSON)
	if !ok || saved != 99 || gotD.Persona != "chrome" {
		t.Fatalf("ed25519 defaults failed: %+v %d %v", gotD, saved, ok)
	}
}
