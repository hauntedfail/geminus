package wire

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// Only Mozilla's push service is ever contacted, whatever a record says.
	endpointHost = "updates.push.services.mozilla.com"
	endpointPath = "/wpush/"
	pushLabel    = "push-endpoint:"
	ringTimeout  = 4 * time.Second
)

// Endpoint returns the doorbell address a member computer signed for exactly
// this peer in its geminish record, or false.
func Endpoint(secret []byte, peer string, raw json.RawMessage) (string, bool) {
	var record struct {
		Endpoint string `json:"endpoint"`
		TS       int64  `json:"ts"`
		Sig      string `json:"sig"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &record) != nil {
		return "", false
	}
	if !Verify(secret, record.Endpoint, pushLabel+peer, record.TS, record.Sig) {
		return "", false
	}
	u, err := url.Parse(record.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host != endpointHost || u.User != nil || !strings.HasPrefix(u.Path, endpointPath) {
		return "", false
	}
	return record.Endpoint, true
}

// Ring makes the computer behind endpoint read Honcho now instead of at its
// next timer tick. The ring carries nothing; a lost one only costs time.
func Ring(ctx context.Context, client *http.Client, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, ringTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("TTL", "60")
	req.Header.Set("User-Agent", "geminus")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("the push service answered HTTP %d", res.StatusCode)
	}
	return nil
}
