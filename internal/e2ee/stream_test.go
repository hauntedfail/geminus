package e2ee

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestStreamChunkSealOpenAndTamper(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, 32)
	salt := bytes.Repeat([]byte{0x11}, SaltSize)
	ts := int64(1795680000)
	payload := []byte(`{"session":"s1","turn":"t1","seq":1,"kind":"delta","text":"hello"}`)

	frame, err := SealStreamChunk(secret, ts, salt, 1, payload)
	if err != nil {
		t.Fatalf("SealStreamChunk: %v", err)
	}
	got, err := OpenStreamChunk(secret, frame, ts, salt, 1)
	if err != nil {
		t.Fatalf("OpenStreamChunk: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %q want %q", got, payload)
	}

	// Wrong sequence number must fail (anti-replay / ordering binding).
	if _, err := OpenStreamChunk(secret, frame, ts, salt, 2); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed on seq mismatch, got %v", err)
	}
	// Wrong timestamp must fail.
	if _, err := OpenStreamChunk(secret, frame, ts+1, salt, 1); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed on ts mismatch, got %v", err)
	}
	// Wrong salt must fail.
	badSalt := append([]byte(nil), salt...)
	badSalt[0] ^= 0xff
	if _, err := OpenStreamChunk(secret, frame, ts, badSalt, 1); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed on salt mismatch, got %v", err)
	}
	// Bit-flip in ciphertext must fail AEAD authentication.
	tampered := append([]byte(nil), frame...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := OpenStreamChunk(secret, tampered, ts, salt, 1); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed on tampered frame, got %v", err)
	}
}

func TestStreamEventSignVerify(t *testing.T) {
	secret := bytes.Repeat([]byte{0x77}, 32)
	ev := StreamEvent{
		Session: "geminus-sibyl-20261127-abc123",
		Turn:    "msg-1",
		Seq:     1,
		Kind:    StreamKindToolUse,
		Text:    "",
		Tool:    "terminal",
		Detail:  "go test ./...",
		OK:      true,
		TS:      1795680000,
	}
	ev.Sig = SignStreamEvent(secret, ev)
	if !VerifyStreamEvent(secret, ev) {
		t.Fatal("VerifyStreamEvent failed for valid event")
	}

	tampered := ev
	tampered.Detail = "rm -rf /"
	if VerifyStreamEvent(secret, tampered) {
		t.Fatal("VerifyStreamEvent accepted tampered detail")
	}
	wrongKind := ev
	wrongKind.Kind = "bogus"
	if VerifyStreamEvent(secret, wrongKind) {
		t.Fatal("VerifyStreamEvent accepted unknown kind")
	}
}

func TestProxyStreamPublishSubscribeEndToEnd(t *testing.T) {
	secret := bytes.Repeat([]byte{0x5a}, 32)
	now := time.Unix(1795681000, 0)

	var upstreamHits int
	var upMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upMu.Lock()
		upstreamHits++
		upMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy, err := NewProxy(ProxyConfig{
		Secret:            secret,
		Upstream:          upstream.URL,
		StreamKeepalive:   50 * time.Millisecond,
		MaxStreamDuration: 3 * time.Second,
		Now:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	var wireMu sync.Mutex
	var wireLeaks []string
	const secretToken = "super-secret-delta-token-9876"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		wireMu.Lock()
		if bytes.Contains(raw, []byte(secretToken)) || bytes.Contains(raw, []byte("_stream")) {
			wireLeaks = append(wireLeaks, string(raw))
		}
		wireMu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()

	cli, err := NewStreamClient(srv.URL+"/geminise/e2ee", secret)
	if err != nil {
		t.Fatalf("NewStreamClient: %v", err)
	}
	cli.now = func() time.Time { return now }

	// Publish one backlog event before subscribing.
	ev1 := StreamEvent{
		Session: "sess-1",
		Turn:    "turn-1",
		Seq:     1,
		Kind:    StreamKindDelta,
		Text:    "first ",
		TS:      now.Unix(),
	}
	ev1.Sig = SignStreamEvent(secret, ev1)
	if lastID, err := cli.Publish(context.Background(), []StreamEvent{ev1}); err != nil || lastID != 1 {
		t.Fatalf("Publish ev1: lastID=%d err=%v", lastID, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gotCh := make(chan StreamEvent, 8)
	errCh := make(chan error, 1)
	go func() {
		_, err := cli.Subscribe(ctx, "sess-1", 0, func(ev StreamEvent) {
			gotCh <- ev
		})
		errCh <- err
	}()

	// Receive backlog event first.
	select {
	case got := <-gotCh:
		if got.Text != "first " || got.ID != 1 {
			t.Fatalf("unexpected backlog event: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backlog event")
	}

	// Publish live delta + tool_use + done while subscribed.
	ev2 := StreamEvent{Session: "sess-1", Turn: "turn-1", Seq: 2, Kind: StreamKindToolUse, Tool: "terminal", Detail: "ls", TS: now.Unix()}
	ev2.Sig = SignStreamEvent(secret, ev2)
	ev3 := StreamEvent{Session: "sess-1", Turn: "turn-1", Seq: 3, Kind: StreamKindDelta, Text: secretToken, TS: now.Unix()}
	ev3.Sig = SignStreamEvent(secret, ev3)
	ev4 := StreamEvent{Session: "sess-1", Turn: "turn-1", Seq: 4, Kind: StreamKindDone, OK: true, TS: now.Unix()}
	ev4.Sig = SignStreamEvent(secret, ev4)

	start := time.Now()
	if lastID, err := cli.Publish(context.Background(), []StreamEvent{ev2, ev3, ev4}); err != nil || lastID != 4 {
		t.Fatalf("Publish batch: lastID=%d err=%v", lastID, err)
	}

	var received []StreamEvent
	for len(received) < 3 {
		select {
		case got := <-gotCh:
			received = append(received, got)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for live events (got %d)", len(received))
		}
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("live stream delivery too slow: %v", elapsed)
	}
	if received[0].Kind != StreamKindToolUse || received[0].Tool != "terminal" {
		t.Fatalf("unexpected ev2: %+v", received[0])
	}
	if received[1].Kind != StreamKindDelta || received[1].Text != secretToken {
		t.Fatalf("unexpected ev3: %+v", received[1])
	}
	if received[2].Kind != StreamKindDone || !received[2].OK {
		t.Fatalf("unexpected ev4: %+v", received[2])
	}

	// Unsigned publish must be rejected with HTTP 401.
	bad := StreamEvent{Session: "sess-1", Turn: "turn-1", Seq: 5, Kind: StreamKindDelta, Text: "forged", TS: now.Unix(), Sig: "deadbeef"}
	if _, err := cli.Publish(context.Background(), []StreamEvent{bad}); err == nil {
		t.Fatal("expected unsigned stream publish to fail")
	}

	cancel()
	<-errCh

	upMu.Lock()
	hits := upstreamHits
	upMu.Unlock()
	if hits != 0 {
		t.Fatalf("stream endpoints must never hit upstream Honcho, got %d hits", hits)
	}
	wireMu.Lock()
	leaks := len(wireLeaks)
	wireMu.Unlock()
	if leaks != 0 {
		t.Fatalf("wire traffic leaked plaintext: %d", leaks)
	}
}

func TestStreamClientGracefulUnsupportedFallback(t *testing.T) {
	secret := bytes.Repeat([]byte{0x33}, 32)
	oldProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer oldProxy.Close()

	cli, err := NewStreamClient(oldProxy.URL+"/geminise/e2ee", secret)
	if err != nil {
		t.Fatalf("NewStreamClient: %v", err)
	}
	if _, err := cli.Subscribe(context.Background(), "s1", 0, nil); !errors.Is(err, ErrStreamUnsupported) {
		t.Fatalf("expected ErrStreamUnsupported on 400, got %v", err)
	}
}
