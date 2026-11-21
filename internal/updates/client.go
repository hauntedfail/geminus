package updates

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// maxAnswer bounds a manifest or an error text read from the server.
	maxAnswer = 64 << 10
	// maxReason is how much of an error text is repeated, and maxBuildReason
	// how much of what a failed build said.
	maxReason      = 200
	maxBuildReason = 1000
	// defaultPoll is how often Release asks how a build is going, and
	// pollTries how many questions in a row may go unanswered (a tunnel
	// that hiccups, a server being restarted) before it gives up.
	defaultPoll = 2 * time.Second
	pollTries   = 5
)

// The states of a release that was asked for.
const (
	Building  = "building"
	Published = "published"
	Failed    = "failed"
)

// Build is how the server answers a request to release a version, and a
// question about it.
type Build struct {
	Version string `json:"version"`
	State   string `json:"state"`
	// Error is why the build failed.
	Error string `json:"error,omitempty"`
}

// ErrNoRelease means the server has no release yet.
var ErrNoRelease = errors.New("no release has been published")

// Base is the address of the releases: DefaultBase unless the environment
// names another.
func Base(getenv func(string) string) string {
	if base := getenv(EnvBase); base != "" {
		return strings.TrimRight(base, "/")
	}
	return DefaultBase
}

// Client talks to the server that hands out releases.
type Client struct {
	// Base is the address of the releases, without a trailing slash.
	Base string
	// Secret is the group's secret; every request is signed with it.
	Secret []byte
	// Key is the key a release must be signed with.
	Key  ed25519.PublicKey
	HTTP *http.Client
	Now  func() time.Time
	// Poll is how often Release asks how a build is going; two seconds if
	// zero.
	Poll time.Duration
	// Agent is what the client calls itself to the server; "geminus" if
	// empty.
	Agent string
}

// do sends one signed request. No request has a body.
func (c *Client) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	// Some front ends refuse a client that does not say what it is.
	agent := c.Agent
	if agent == "" {
		agent = "geminus"
	}
	req.Header.Set("User-Agent", agent)
	SignRequest(req, c.Secret, EmptySHA, c.Now())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	return nil, &StatusError{Status: resp.StatusCode, Reason: oneLine(string(raw))}
}

// StatusError is an answer of the server that is not a success.
type StatusError struct {
	Status int
	Reason string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("the update server answered %d: %s", e.Status, e.Reason)
}

// oneLine makes text from outside safe to print: its first line, without
// control characters or characters that reorder text, cut to maxReason.
func oneLine(text string) string {
	text, _, _ = strings.Cut(strings.TrimSpace(text), "\n")
	return printable(text, maxReason)
}

// printable is text without control characters or characters that reorder
// text, cut to limit characters.
func printable(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError || r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 {
			return -1
		}
		return r
	}, text)
	if runes := []rune(text); len(runes) > limit {
		text = string(runes[:limit]) + "…"
	}
	return text
}

// buildReason is what a failed build said, on one line: its last lines are
// the ones that say what went wrong.
func buildReason(text string) string {
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	text = strings.Join(lines, " | ")
	if runes := []rune(text); len(runes) > maxBuildReason {
		text = "…" + string(runes[len(runes)-maxBuildReason:])
	}
	return printable(text, maxBuildReason+1)
}

// Latest returns the manifest of the newest release, verified.
func (c *Client) Latest(ctx context.Context) (Manifest, error) {
	m, err := c.manifest(ctx, "/latest")
	var status *StatusError
	if errors.As(err, &status) && status.Status == http.StatusNotFound {
		return Manifest{}, ErrNoRelease
	}
	return m, err
}

// manifest reads the manifest at path and verifies it.
func (c *Client) manifest(ctx context.Context, path string) (Manifest, error) {
	resp, err := c.do(ctx, http.MethodGet, path)
	if err != nil {
		return Manifest{}, err
	}
	defer resp.Body.Close()
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("the update server's answer is not a manifest: %w", err)
	}
	if !m.Verify(c.Key) {
		return Manifest{}, errors.New("the manifest the update server sent is not signed with the release key")
	}
	return m, nil
}

// Fetch writes the program of m for target to w. It fails unless what
// arrived has the size and the SHA-256 the manifest names; w may then have
// received bytes that must be thrown away.
func (c *Client) Fetch(ctx context.Context, m Manifest, target string, w io.Writer) error {
	want, listed := m.Files[target]
	if !listed {
		return fmt.Errorf("release %s has no program for %s", m.Version, target)
	}
	resp, err := c.do(ctx, http.MethodGet, "/"+m.Version+"/"+target)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sum := sha256.New()
	// One byte more than expected shows a body that is too long.
	n, err := io.Copy(io.MultiWriter(w, sum), io.LimitReader(resp.Body, want.Size+1))
	if err != nil {
		return fmt.Errorf("download %s %s: %w", m.Version, target, err)
	}
	if n != want.Size || hex.EncodeToString(sum.Sum(nil)) != want.SHA256 {
		return fmt.Errorf("the download of %s %s is not the file the release names (%d of %d bytes, or other bytes)", m.Version, target, n, want.Size)
	}
	return nil
}

// Release asks the server to build version from its tag in the source, sign
// it and publish it, and waits until that is done or has failed. A version
// that is published already is returned as it is.
func (c *Client) Release(ctx context.Context, version string) (Manifest, error) {
	if !IsRelease(version) {
		return Manifest{}, fmt.Errorf("%q is not a release version: use X.Y.Z", version)
	}
	b, err := c.build(ctx, http.MethodPost, "/"+version)
	if err != nil {
		return Manifest{}, err
	}
	poll := c.Poll
	if poll <= 0 {
		poll = defaultPoll
	}
	for unanswered := 0; b.State == Building; {
		select {
		case <-ctx.Done():
			return Manifest{}, fmt.Errorf("release %s is still being built: %w", version, ctx.Err())
		case <-time.After(poll):
		}
		next, err := c.build(ctx, http.MethodGet, "/"+version+"/build")
		var status *StatusError
		switch {
		case err == nil:
			b, unanswered = next, 0
		case ctx.Err() != nil:
			return Manifest{}, fmt.Errorf("release %s is still being built: %w", version, ctx.Err())
		case errors.As(err, &status) && status.Status == http.StatusNotFound:
			// The server keeps a build in memory only.
			return Manifest{}, fmt.Errorf("the update server no longer knows the build of release %s (it was restarted?); run the release again", version)
		case errors.As(err, &status) && status.Status < 500:
			return Manifest{}, err
		default:
			if unanswered++; unanswered >= pollTries {
				return Manifest{}, fmt.Errorf("lost touch with the build of release %s; it may still finish: %w", version, err)
			}
		}
	}
	switch {
	case b.State == Failed:
		return Manifest{}, fmt.Errorf("the build of release %s failed: %s", version, buildReason(b.Error))
	case b.State != Published:
		return Manifest{}, fmt.Errorf("the update server says release %s is %q", version, printable(b.State, maxReason))
	}
	m, err := c.manifest(ctx, "/"+version)
	if err == nil && m.Version != version {
		err = fmt.Errorf("the update server sent release %s for %s", m.Version, version)
	}
	return m, err
}

// build sends one request about a build and reads the answer.
func (c *Client) build(ctx context.Context, method, path string) (Build, error) {
	resp, err := c.do(ctx, method, path)
	if err != nil {
		return Build{}, err
	}
	defer resp.Body.Close()
	var b Build
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&b); err != nil {
		return Build{}, fmt.Errorf("the update server's answer is not about a build: %w", err)
	}
	return b, nil
}
