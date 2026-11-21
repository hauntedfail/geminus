package updates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// maxAnswer bounds a manifest or an error text read from the server.
	maxAnswer = 64 << 10
	// maxReason is how much of an error text is repeated.
	maxReason = 200
)

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
	Base   string
	Secret []byte
	HTTP   *http.Client
	Now    func() time.Time
	// Agent is what the client calls itself to the server; "geminus" if
	// empty.
	Agent string
}

// do sends one signed request. body may be nil; bodySHA is then EmptySHA.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, size int64, bodySHA string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	// Some front ends refuse a client that does not say what it is.
	agent := c.Agent
	if agent == "" {
		agent = "geminus"
	}
	req.Header.Set("User-Agent", agent)
	SignRequest(req, c.Secret, bodySHA, c.Now())
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
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError || r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 {
			return -1
		}
		return r
	}, text)
	if runes := []rune(text); len(runes) > maxReason {
		text = string(runes[:maxReason]) + "…"
	}
	return text
}

// Latest returns the manifest of the newest release, verified.
func (c *Client) Latest(ctx context.Context) (Manifest, error) {
	resp, err := c.do(ctx, http.MethodGet, "/latest", nil, 0, EmptySHA)
	var status *StatusError
	if errors.As(err, &status) && status.Status == http.StatusNotFound {
		return Manifest{}, ErrNoRelease
	}
	if err != nil {
		return Manifest{}, err
	}
	defer resp.Body.Close()
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("the update server's answer is not a manifest: %w", err)
	}
	if !m.Verify(c.Secret) {
		return Manifest{}, errors.New("the manifest of the latest release is not signed by this group")
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
	resp, err := c.do(ctx, http.MethodGet, "/"+m.Version+"/"+target, nil, 0, EmptySHA)
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

// Publish uploads the programs in files (target → path on this computer)
// as release version, then the manifest that makes it the latest. A program
// must not change while this runs: it is read once for its digest and once
// to send it, and the server refuses a body other than the signed one.
func (c *Client) Publish(ctx context.Context, version string, files map[string]string) (Manifest, error) {
	if !IsRelease(version) {
		return Manifest{}, fmt.Errorf("%q is not a release version: use X.Y.Z", version)
	}
	listed := make(map[string]File, len(files))
	for target, path := range files {
		f, err := c.upload(ctx, version, target, path)
		if err != nil {
			return Manifest{}, err
		}
		listed[target] = f
	}
	m := Seal(c.Secret, version, listed, c.Now().Unix())
	raw, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, err
	}
	sum := sha256.Sum256(raw)
	resp, err := c.do(ctx, http.MethodPut, "/"+version, bytes.NewReader(raw), int64(len(raw)), hex.EncodeToString(sum[:]))
	if err != nil {
		return Manifest{}, err
	}
	resp.Body.Close()
	return m, nil
}

func (c *Client) upload(ctx context.Context, version, target, path string) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	sum := sha256.New()
	size, err := io.Copy(sum, f)
	if err != nil {
		return File{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return File{}, err
	}
	file := File{SHA256: hex.EncodeToString(sum.Sum(nil)), Size: size}
	resp, err := c.do(ctx, http.MethodPut, "/"+version+"/"+target, f, size, file.SHA256)
	if err != nil {
		return File{}, fmt.Errorf("upload %s: %w", target, err)
	}
	resp.Body.Close()
	return file, nil
}
