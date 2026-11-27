package e2ee

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "\n", ""))
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

// TestRFC8439Section282 verifies our ChaCha20-Poly1305 AEAD matches the exact
// official test vector from RFC 8439 Section 2.8.2.
func TestRFC8439Section282(t *testing.T) {
	t.Parallel()
	key := mustHex(t, "808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f")
	nonce := mustHex(t, "070000004041424344454647")
	aad := mustHex(t, "50515253c0c1c2c3c4c5c6c7")
	plaintext := []byte("Ladies and Gentlemen of the class of '99: If I could offer you only one tip for the future, sunscreen would be it.")

	wantCT := mustHex(t,
		"d31a8d34648e60db7b86afbc53ef7ec2a4aded51296e08fea9e2b5a736ee62d6"+
			"3dbea45e8ca9671282fafb69da92728b1a71de0a9e060b2905d6a5b67ecd3b36"+
			"92ddbd7f2d778b8c9803aee328091b58fab324e4fad675945585808b4831d7bc"+
			"3ff4def08e4b7a9de576d26586cec64b6116")
	wantTag := mustHex(t, "1ae10b594f09e26a7e902ecbd0600691")

	sealed, err := SealAEAD(key, nonce, plaintext, aad)
	if err != nil {
		t.Fatalf("SealAEAD: %v", err)
	}
	want := append(append([]byte(nil), wantCT...), wantTag...)
	if !bytes.Equal(sealed, want) {
		t.Fatalf("sealed mismatch:\ngot  %x\nwant %x", sealed, want)
	}

	got, err := OpenAEAD(key, nonce, sealed, aad)
	if err != nil {
		t.Fatalf("OpenAEAD: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext mismatch: got %q", got)
	}

	// Flipping a bit in AAD or ciphertext must fail authentication.
	badAAD := append([]byte(nil), aad...)
	badAAD[0] ^= 1
	if _, err := OpenAEAD(key, nonce, sealed, badAAD); err == nil {
		t.Fatal("expected auth failure on modified AAD")
	}
	badSealed := append([]byte(nil), sealed...)
	badSealed[len(badSealed)-1] ^= 1
	if _, err := OpenAEAD(key, nonce, badSealed, aad); err == nil {
		t.Fatal("expected auth failure on modified tag")
	}
}

// TestRFC8439Poly1305Section252 verifies the standalone Poly1305 test vector.
func TestRFC8439Poly1305Section252(t *testing.T) {
	t.Parallel()
	key := mustHex(t, "85d6be7857556d337f4452fe42d506a80103808afb0db2fd4abff6af4149f51b")
	msg := []byte("Cryptographic Forum Research Group")
	want := mustHex(t, "a8061dc1305136c6c22b8baf0c0127a9")
	got := poly1305MAC(key, msg)
	if !bytes.Equal(got[:], want) {
		t.Fatalf("poly1305 mismatch: got %x want %x", got, want)
	}
}

func testSecret() []byte {
	return bytes.Repeat([]byte{0x42}, 32)
}

func TestEnvelopeRoundTripAndTampering(t *testing.T) {
	t.Parallel()
	secret := testSecret()
	ts := int64(1795600000)

	req := Request{
		Method:  http.MethodPost,
		Path:    "/v3/workspaces/sibyl/sessions/s1/messages",
		Query:   "reverse=true",
		Headers: map[string]string{"Authorization": "Bearer secret-tok", "Content-Type": "application/json"},
	}
	req.SetBody([]byte(`{"messages":[{"peer_id":"h1karu","content":"hello raw honcho"}]}`))

	frame, salt, err := SealRequest(secret, req, ts, nil)
	if err != nil {
		t.Fatalf("SealRequest: %v", err)
	}
	if len(salt) != SaltSize {
		t.Fatalf("salt len = %d", len(salt))
	}
	// Wire frame must never contain plaintext URL, token, or message content.
	for _, leak := range []string{"/v3/workspaces", "secret-tok", "hello raw honcho"} {
		if bytes.Contains(frame, []byte(leak)) {
			t.Fatalf("encrypted frame leaked %q", leak)
		}
	}
	// Inner JSON is padded to >= MinPadBucket.
	if len(frame) < HeaderSize+MinPadBucket+TagSize {
		t.Fatalf("frame too small for padded bucket: %d", len(frame))
	}

	gotReq, gotTS, gotSalt, err := OpenRequest(secret, frame)
	if err != nil {
		t.Fatalf("OpenRequest: %v", err)
	}
	if gotTS != ts || !bytes.Equal(gotSalt, salt) {
		t.Fatalf("ts/salt mismatch")
	}
	if gotReq.Method != req.Method || gotReq.Path != req.Path || gotReq.Query != req.Query {
		t.Fatalf("decoded request mismatch: %+v", gotReq)
	}
	body, err := gotReq.BodyBytes()
	if err != nil || !strings.Contains(string(body), "hello raw honcho") {
		t.Fatalf("body mismatch: %q %v", body, err)
	}

	// Direction confusion: request frame must not open as response.
	if _, err := OpenResponse(secret, frame, ts, salt); err == nil {
		t.Fatal("request frame accepted as response")
	}
	// Timestamp tampering in header must break AAD authentication.
	tampered := append([]byte(nil), frame...)
	tampered[13] ^= 1
	if _, _, _, err := OpenRequest(secret, tampered); err == nil {
		t.Fatal("tampered timestamp accepted")
	}

	// Response round-trip and salt binding check.
	res := Response{Status: 200, Headers: map[string]string{"Content-Type": "application/json"}}
	res.SetBody([]byte(`{"ok":true}`))
	resFrame, err := SealResponse(secret, res, ts, salt)
	if err != nil {
		t.Fatalf("SealResponse: %v", err)
	}
	gotRes, err := OpenResponse(secret, resFrame, ts, salt)
	if err != nil {
		t.Fatalf("OpenResponse: %v", err)
	}
	if gotRes.Status != 200 {
		t.Fatalf("status = %d", gotRes.Status)
	}
	wrongSalt := append([]byte(nil), salt...)
	wrongSalt[0] ^= 0xff
	if _, err := OpenResponse(secret, resFrame, ts, wrongSalt); err == nil {
		t.Fatal("response accepted with wrong salt")
	}
}

func TestValidatePath(t *testing.T) {
	t.Parallel()
	good := []string{"/v3/workspaces/sibyl/sessions", "/v3/keys", "/health"}
	for _, p := range good {
		if _, err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) unexpected err: %v", p, err)
		}
	}
	bad := []string{
		"",
		"v3/workspaces",
		"/v3/../admin",
		"/etc/passwd",
		"/v2/workspaces",
		"/v3/foo?bar=1",
		"/v3/foo\r\nHost: evil",
		"/v3/foo\\bar",
	}
	for _, p := range bad {
		if _, err := ValidatePath(p); err == nil {
			t.Errorf("ValidatePath(%q) should fail", p)
		}
	}
}

func TestProxyRoundTripReplayAndSkew(t *testing.T) {
	t.Parallel()
	secret := testSecret()
	now := time.Unix(1795600000, 0)

	var seenPath, seenQuery, seenAuth, seenBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenQuery = r.URL.RawQuery
		seenAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"stored":"raw-ok"}`))
	}))
	defer upstream.Close()

	proxy, err := NewProxy(ProxyConfig{
		Secret:   secret,
		Upstream: upstream.URL,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	srv := httptest.NewServer(proxy)
	defer srv.Close()

	req := Request{
		Method:  http.MethodPost,
		Path:    "/v3/workspaces/sibyl/sessions/s1/messages",
		Query:   "page=1",
		Headers: map[string]string{"Authorization": "Bearer test-bearer", "Content-Type": "application/json"},
	}
	req.SetBody([]byte(`{"content":"plaintext-for-honcho"}`))
	frame, salt, err := SealRequest(secret, req, now.Unix(), nil)
	if err != nil {
		t.Fatalf("SealRequest: %v", err)
	}

	httpRes, err := http.Post(srv.URL, ContentType, bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("POST proxy: %v", err)
	}
	outFrame, _ := io.ReadAll(httpRes.Body)
	httpRes.Body.Close()
	if httpRes.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d", httpRes.StatusCode)
	}
	if bytes.Contains(outFrame, []byte("raw-ok")) {
		t.Fatal("proxy response was not encrypted")
	}

	decRes, err := OpenResponse(secret, outFrame, now.Unix(), salt)
	if err != nil {
		t.Fatalf("OpenResponse: %v", err)
	}
	decBody, _ := decRes.BodyBytes()
	if decRes.Status != http.StatusCreated || string(decBody) != `{"stored":"raw-ok"}` {
		t.Fatalf("unexpected decoded response: status=%d body=%q", decRes.Status, decBody)
	}
	if seenPath != req.Path || seenQuery != req.Query || seenAuth != "Bearer test-bearer" || seenBody != `{"content":"plaintext-for-honcho"}` {
		t.Fatalf("upstream did not receive raw request: path=%q query=%q auth=%q body=%q", seenPath, seenQuery, seenAuth, seenBody)
	}

	// Replaying the identical frame must be rejected with 401.
	replayRes, err := http.Post(srv.URL, ContentType, bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("replay POST: %v", err)
	}
	replayRes.Body.Close()
	if replayRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", replayRes.StatusCode)
	}

	// Stale timestamp must be rejected with 401.
	staleFrame, _, _ := SealRequest(secret, req, now.Add(-10*time.Minute).Unix(), nil)
	staleRes, err := http.Post(srv.URL, ContentType, bytes.NewReader(staleFrame))
	if err != nil {
		t.Fatalf("stale POST: %v", err)
	}
	staleRes.Body.Close()
	if staleRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale status = %d, want 401", staleRes.StatusCode)
	}
}
