package honcho_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hauntedfail/geminus/internal/e2ee"
	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/honcho/honchotest"
)

func TestClientWithE2EEEncryptsWireAndStoresRawInHoncho(t *testing.T) {
	t.Parallel()
	secret := bytes.Repeat([]byte{0x7a}, 32)
	srv := honchotest.New("h1karu", "sibyl")
	defer srv.Close()

	proxyHandler, err := e2ee.NewProxy(e2ee.ProxyConfig{
		Secret:   secret,
		Upstream: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	var (
		mu          sync.Mutex
		wireBodies  [][]byte
		wirePaths   []string
		wireHeaders []string
	)
	spyProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		wireBodies = append(wireBodies, raw)
		wirePaths = append(wirePaths, r.URL.Path)
		wireHeaders = append(wireHeaders, r.Header.Get("Authorization"))
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		proxyHandler.ServeHTTP(w, r)
	}))
	defer spyProxy.Close()

	c, err := honcho.New(srv.URL, srv.Workspace, srv.Key)
	if err != nil {
		t.Fatalf("honcho.New: %v", err)
	}
	if _, err := c.WithE2EE(spyProxy.URL+"/geminise/e2ee", secret); err != nil {
		t.Fatalf("WithE2EE: %v", err)
	}

	ctx := context.Background()
	if err := c.CreateSession(ctx, "e2ee-sess", map[string]any{"kind": "geminus"}, "h1karu", "sibyl"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	const secretPrompt = "confidential prompt content 日本語テスト"
	if _, err := c.Post(ctx, "e2ee-sess", "h1karu", secretPrompt, map[string]any{"geminus": map[string]any{"role": "prompt"}}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	msgs, err := c.Messages(ctx, "e2ee-sess")
	if err != nil || len(msgs) != 1 || msgs[0].Content != secretPrompt {
		t.Fatalf("Messages via E2EE = %+v, %v", msgs, err)
	}

	// Verify Honcho stored the message completely raw.
	rawStored := srv.Messages("e2ee-sess")
	if len(rawStored) != 1 || rawStored[0].Content != secretPrompt {
		t.Fatalf("Honcho raw storage mismatch: %+v", rawStored)
	}

	// Verify the wire carried only fixed path, no Authorization header, and opaque GE2E ciphertext.
	mu.Lock()
	defer mu.Unlock()
	if len(wireBodies) < 3 {
		t.Fatalf("expected >= 3 proxied calls, got %d", len(wireBodies))
	}
	for i, b := range wireBodies {
		if wirePaths[i] != "/geminise/e2ee" {
			t.Errorf("wire path[%d] = %q, want /geminise/e2ee", i, wirePaths[i])
		}
		if wireHeaders[i] != "" {
			t.Errorf("outer Authorization header leaked: %q", wireHeaders[i])
		}
		if !bytes.HasPrefix(b, []byte(e2ee.Magic)) {
			t.Errorf("wire frame[%d] missing GE2E magic", i)
		}
		for _, forbidden := range []string{"/v3/workspaces", "e2ee-sess", secretPrompt, srv.Key} {
			if bytes.Contains(b, []byte(forbidden)) {
				t.Errorf("wire frame[%d] leaked %q", i, forbidden)
			}
		}
	}
}

func TestWithE2EERejectsInsecureHTTP(t *testing.T) {
	t.Parallel()
	c, err := honcho.New("https://honcho.example", "sibyl", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WithE2EE("http://example.com/e2ee", bytes.Repeat([]byte{1}, 32)); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected insecure http rejection, got %v", err)
	}
}
