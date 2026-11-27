package e2ee

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrStreamUnsupported is returned when the configured GE2E proxy does not
// implement ephemeral /_stream/* endpoints (for example an older proxy binary).
var ErrStreamUnsupported = errors.New("e2ee: proxy does not support ephemeral streaming")

const maxStreamLineBytes = 1 << 20

// StreamClient publishes and subscribes to ephemeral GE2E v1 stream events
// through a geminus-e2ee-proxy endpoint.
type StreamClient struct {
	e2eeURL string
	secret  []byte
	http    *http.Client
	pubHTTP *http.Client
	now     func() time.Time
}

// NewStreamClient creates a validated GE2E v1 ephemeral stream client.
func NewStreamClient(e2eeURL string, secret []byte) (*StreamClient, error) {
	u, err := url.Parse(strings.TrimSpace(e2eeURL))
	if err != nil || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("e2ee: invalid stream endpoint %q", e2eeURL)
	}
	host := u.Hostname()
	loopback := u.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1")
	if u.Scheme != "https" && !loopback {
		return nil, fmt.Errorf("e2ee: stream endpoint must be https or loopback http: %q", e2eeURL)
	}
	if len(secret) < MinSecretBytes {
		return nil, ErrInvalidSecret
	}
	noRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &StreamClient{
		e2eeURL: u.String(),
		secret:  append([]byte(nil), secret...),
		// Streaming subscribe relies on ctx for cancellation rather than a short client timeout.
		http:    &http.Client{Timeout: 0, CheckRedirect: noRedirect},
		pubHTTP: &http.Client{Timeout: 10 * time.Second, CheckRedirect: noRedirect},
		now:     time.Now,
	}, nil
}

// Publish sends one or more bridge-signed StreamEvents over GE2E v1 to /_stream/publish.
func (c *StreamClient) Publish(ctx context.Context, events []StreamEvent) (uint64, error) {
	body, err := json.Marshal(StreamPublishRequest{Events: events})
	if err != nil {
		return 0, err
	}
	ts := c.now().Unix()
	inner := Request{
		Method:  http.MethodPost,
		Path:    StreamPathPublish,
		Headers: map[string]string{"Content-Type": "application/json"},
	}
	inner.SetBody(body)
	frame, salt, err := SealRequest(c.secret, inner, ts, nil)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.e2eeURL, bytes.NewReader(frame))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", ContentType)
	res, err := c.pubHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusNotFound {
		return 0, ErrStreamUnsupported
	}
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("e2ee stream publish: HTTP %d", res.StatusCode)
	}
	outFrame, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	innerRes, err := OpenResponse(c.secret, outFrame, ts, salt)
	if err != nil {
		return 0, err
	}
	if innerRes.Status < 200 || innerRes.Status > 299 {
		return 0, fmt.Errorf("e2ee stream publish: inner HTTP %d", innerRes.Status)
	}
	resBody, err := innerRes.BodyBytes()
	if err != nil {
		return 0, err
	}
	var pubRes StreamPublishResponse
	if len(resBody) > 0 && json.Unmarshal(resBody, &pubRes) == nil {
		return pubRes.LastID, nil
	}
	return 0, nil
}

// Subscribe opens one encrypted SSE stream for session starting strictly after cursor `after`,
// verifying both GE2E chunk encryption (ts, salt, monotonic seq) and bridge HMAC signatures.
// It returns the highest delivered event ID when the stream ends cleanly or encounters an error.
func (c *StreamClient) Subscribe(ctx context.Context, session string, after uint64, onEvent func(StreamEvent)) (uint64, error) {
	if strings.TrimSpace(session) == "" {
		return after, errors.New("e2ee: empty session for stream subscribe")
	}
	q := url.Values{}
	q.Set("session", session)
	if after > 0 {
		q.Set("after", strconv.FormatUint(after, 10))
	}
	ts := c.now().Unix()
	inner := Request{
		Method:  http.MethodGet,
		Path:    StreamPathSubscribe,
		Query:   q.Encode(),
		Headers: map[string]string{"Accept": "text/event-stream"},
	}
	frame, salt, err := SealRequest(c.secret, inner, ts, nil)
	if err != nil {
		return after, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.e2eeURL, bytes.NewReader(frame))
	if err != nil {
		return after, err
	}
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("Accept", "text/event-stream")

	res, err := c.http.Do(req)
	if err != nil {
		return after, err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusNotFound {
		return after, ErrStreamUnsupported
	}
	if res.StatusCode != http.StatusOK {
		return after, fmt.Errorf("e2ee stream subscribe: HTTP %d", res.StatusCode)
	}

	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxStreamLineBytes)
	lastID := after
	var expectedSeq uint64
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		b64 := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if b64 == "" {
			continue
		}
		rawChunk, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return lastID, fmt.Errorf("e2ee stream decode: %w", err)
		}
		expectedSeq++
		plain, err := OpenStreamChunk(c.secret, rawChunk, ts, salt, expectedSeq)
		if err != nil {
			return lastID, err
		}
		var ev StreamEvent
		if err := json.Unmarshal(plain, &ev); err != nil {
			return lastID, fmt.Errorf("e2ee stream event json: %w", err)
		}
		if ev.Session != session || !VerifyStreamEvent(c.secret, ev) {
			continue
		}
		if ev.ID > lastID {
			lastID = ev.ID
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
	if err := scanner.Err(); err != nil {
		return lastID, err
	}
	return lastID, nil
}
