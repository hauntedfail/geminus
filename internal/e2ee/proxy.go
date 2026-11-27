package e2ee

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMaxSkew is the allowed clock difference between client and proxy.
	DefaultMaxSkew = 300 * time.Second
	// DefaultMaxRequestBytes caps outer encrypted request frame size.
	DefaultMaxRequestBytes int64 = 4 << 20
	// DefaultMaxResponseBytes caps upstream Honcho response body size.
	DefaultMaxResponseBytes int64 = 64 << 20
	// ContentType is the outer HTTP content type for GE2E binary frames.
	ContentType = "application/octet-stream"

	maxReplayEntries = 65536
)

var allowedMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

var allowedReqHeaders = map[string]string{
	"authorization":   "Authorization",
	"content-type":    "Content-Type",
	"accept":          "Accept",
	"user-agent":      "User-Agent",
	"idempotency-key": "Idempotency-Key",
}

// ValidateMethod reports whether method is permitted for inner Honcho calls.
func ValidateMethod(method string) bool {
	return allowedMethods[method]
}

// ValidatePath verifies that rawPath targets only Honcho's /v3/... or /health
// endpoints without path traversal, scheme injection, or control characters.
func ValidatePath(rawPath string) (string, error) {
	if rawPath == "" || !strings.HasPrefix(rawPath, "/") {
		return "", errors.New("e2ee: inner path must start with /")
	}
	if strings.ContainsAny(rawPath, "\\\r\n\x00?#") {
		return "", errors.New("e2ee: inner path contains forbidden characters")
	}
	for _, seg := range strings.Split(rawPath, "/") {
		if seg == ".." {
			return "", errors.New("e2ee: path traversal not allowed")
		}
	}
	clean := path.Clean(rawPath)
	if clean == "/health" || clean == "/v3" || strings.HasPrefix(clean, "/v3/") {
		return clean, nil
	}
	return "", fmt.Errorf("e2ee: path %q is outside /v3 and /health", clean)
}

type replayCache struct {
	mu      sync.Mutex
	entries map[[SaltSize]byte]int64
	ttl     int64
}

func newReplayCache(ttl time.Duration) *replayCache {
	secs := int64(ttl / time.Second)
	if secs < 60 {
		secs = 60
	}
	return &replayCache{
		entries: make(map[[SaltSize]byte]int64),
		ttl:     secs,
	}
}

// claim returns true if salt has not been seen within ttl, and records it.
func (rc *replayCache) claim(salt []byte, now int64) bool {
	var key [SaltSize]byte
	copy(key[:], salt)

	rc.mu.Lock()
	defer rc.mu.Unlock()

	if exp, ok := rc.entries[key]; ok && exp > now {
		return false
	}
	if len(rc.entries) >= maxReplayEntries {
		for k, exp := range rc.entries {
			if exp <= now {
				delete(rc.entries, k)
			}
		}
		if len(rc.entries) >= maxReplayEntries {
			// Evict oldest half if still full under heavy load.
			n := len(rc.entries) / 2
			for k := range rc.entries {
				delete(rc.entries, k)
				n--
				if n <= 0 {
					break
				}
			}
		}
	}
	rc.entries[key] = now + rc.ttl
	return true
}

// ProxyConfig configures the BTCVPS loopback terminating E2EE proxy.
type ProxyConfig struct {
	Secret           []byte
	Upstream         string
	MaxSkew          time.Duration
	MaxRequestBytes  int64
	MaxResponseBytes int64
	HTTPClient       *http.Client
	Now              func() time.Time
}

// Proxy terminates GE2E v1 frames and forwards raw HTTP requests to Honcho.
type Proxy struct {
	secret           []byte
	upstream         *url.URL
	maxSkew          int64
	maxRequestBytes  int64
	maxResponseBytes int64
	httpClient       *http.Client
	now              func() time.Time
	replay           *replayCache
}

// NewProxy constructs a validated GE2E v1 terminating proxy.
func NewProxy(cfg ProxyConfig) (*Proxy, error) {
	if len(cfg.Secret) < MinSecretBytes {
		return nil, ErrInvalidSecret
	}
	u, err := url.Parse(strings.TrimRight(cfg.Upstream, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("e2ee: invalid upstream %q", cfg.Upstream)
	}
	skew := cfg.MaxSkew
	if skew <= 0 {
		skew = DefaultMaxSkew
	}
	maxReq := cfg.MaxRequestBytes
	if maxReq <= 0 {
		maxReq = DefaultMaxRequestBytes
	}
	maxRes := cfg.MaxResponseBytes
	if maxRes <= 0 {
		maxRes = DefaultMaxResponseBytes
	}
	cli := cfg.HTTPClient
	if cli == nil {
		cli = &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Proxy{
		secret:           append([]byte(nil), cfg.Secret...),
		upstream:         u,
		maxSkew:          int64(skew / time.Second),
		maxRequestBytes:  maxReq,
		maxResponseBytes: maxRes,
		httpClient:       cli,
		now:              nowFn,
		replay:           newReplayCache(2 * skew),
	}, nil
}

// ServeHTTP handles a single GE2E v1 encrypted request and returns an encrypted response.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limited := io.LimitReader(r.Body, p.maxRequestBytes+1)
	frame, err := io.ReadAll(limited)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if int64(len(frame)) > p.maxRequestBytes {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}

	innerReq, ts, salt, err := OpenRequest(p.secret, frame)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	now := p.now().Unix()
	diff := now - ts
	if diff < 0 {
		diff = -diff
	}
	if diff > p.maxSkew {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !p.replay.claim(salt, now) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if !ValidateMethod(innerReq.Method) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cleanPath, err := ValidatePath(innerReq.Path)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	bodyBytes, err := innerReq.BodyBytes()
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	target := *p.upstream
	target.Path = strings.TrimRight(p.upstream.Path, "/") + cleanPath
	target.RawPath = ""
	target.RawQuery = innerReq.Query

	var bodyReader io.Reader
	if len(bodyBytes) > 0 {
		bodyReader = bytes.NewReader(bodyBytes)
	}
	upReq, err := http.NewRequestWithContext(r.Context(), innerReq.Method, target.String(), bodyReader)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for k, v := range innerReq.Headers {
		if canon, ok := allowedReqHeaders[strings.ToLower(strings.TrimSpace(k))]; ok {
			if !strings.ContainsAny(v, "\r\n") {
				upReq.Header.Set(canon, v)
			}
		}
	}

	upRes, err := p.httpClient.Do(upReq)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer upRes.Body.Close()

	resBody, err := io.ReadAll(io.LimitReader(upRes.Body, p.maxResponseBytes+1))
	if err != nil || int64(len(resBody)) > p.maxResponseBytes {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	innerRes := Response{
		Status:  upRes.StatusCode,
		Headers: map[string]string{},
	}
	if ct := upRes.Header.Get("Content-Type"); ct != "" {
		innerRes.Headers["Content-Type"] = ct
	}
	innerRes.SetBody(resBody)

	outFrame, err := SealResponse(p.secret, innerRes, ts, salt)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(outFrame)
}
