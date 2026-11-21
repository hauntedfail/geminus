package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/updates"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

const base = "/geminus/updates/"

// site is a server over a temporary directory, with a clock the test moves.
type site struct {
	t   *testing.T
	dir string
	now time.Time
	srv *httptest.Server
}

func newSite(t *testing.T) *site {
	t.Helper()
	s := &site{t: t, dir: t.TempDir(), now: time.Unix(1795300000, 0)}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	s.srv = httptest.NewServer(New(root, secret, func() time.Time { return s.now }, slog.New(slog.DiscardHandler)))
	t.Cleanup(s.srv.Close)
	return s
}

func sum(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

// call sends a signed request and returns the status and the body.
func (s *site) call(method, path, body string, change ...func(*http.Request)) (int, string) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.srv.URL+path, strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	updates.SignRequest(req, secret, sum(body), s.now)
	for _, f := range change {
		f(req)
	}
	// Redirects are answers too: a cleaned path must not be followed blindly.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func (s *site) want(status int, method, path, body string, change ...func(*http.Request)) string {
	s.t.Helper()
	got, text := s.call(method, path, body, change...)
	if got != status {
		s.t.Fatalf("%s %s = %d %q, want %d", method, path, got, strings.TrimSpace(text), status)
	}
	return text
}

func manifest(version string, ts int64, files map[string]string) string {
	list := map[string]updates.File{}
	for target, body := range files {
		list[target] = updates.File{SHA256: sum(body), Size: int64(len(body))}
	}
	raw, _ := json.Marshal(updates.Seal(secret, version, list, ts))
	return string(raw)
}

// publish uploads the files of a release and its manifest.
func (s *site) publish(version string, files map[string]string) {
	s.t.Helper()
	for target, body := range files {
		s.want(http.StatusCreated, "PUT", base+version+"/"+target, body)
	}
	s.want(http.StatusCreated, "PUT", base+version, manifest(version, s.now.Unix(), files))
}

var first = map[string]string{"linux-amd64": "linux program one", "windows-amd64": "windows program one"}

func TestAReleaseIsPublishedThenServedAsTheLatest(t *testing.T) {
	s := newSite(t)
	s.want(http.StatusNotFound, "GET", base+"latest", "")
	s.publish("0.5.0", first)

	var m updates.Manifest
	if err := json.Unmarshal([]byte(s.want(http.StatusOK, "GET", base+"latest", "")), &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "0.5.0" || !m.Verify(secret) || m.Files["windows-amd64"].Size != int64(len(first["windows-amd64"])) {
		t.Errorf("latest = %+v", m)
	}
	if got := s.want(http.StatusOK, "GET", base+"0.5.0/windows-amd64", ""); got != first["windows-amd64"] {
		t.Errorf("program = %q", got)
	}
	if got := s.want(http.StatusOK, "GET", base+"0.5.0", ""); !strings.Contains(got, `"version":"0.5.0"`) {
		t.Errorf("manifest = %q", got)
	}

	s.publish("0.5.1", map[string]string{"linux-amd64": "linux program two"})
	if got := s.want(http.StatusOK, "GET", base+"latest", ""); !strings.Contains(got, `"version":"0.5.1"`) {
		t.Errorf("latest = %q", got)
	}
	// The earlier release is still there.
	s.want(http.StatusOK, "GET", base+"0.5.0/linux-amd64", "")
	s.want(http.StatusNotFound, "GET", base+"0.5.1/windows-amd64", "")
}

func TestNothingIsServedBeforeItsManifestIsPublished(t *testing.T) {
	s := newSite(t)
	s.want(http.StatusCreated, "PUT", base+"0.5.0/linux-amd64", "half a release")
	s.want(http.StatusNotFound, "GET", base+"0.5.0/linux-amd64", "")
	s.want(http.StatusNotFound, "GET", base+"0.5.0", "")
	s.want(http.StatusNotFound, "GET", base+"latest", "")
	// Until then an upload may be replaced, so a failed release can be run again.
	s.want(http.StatusCreated, "PUT", base+"0.5.0/linux-amd64", "the whole release")
	s.want(http.StatusCreated, "PUT", base+"0.5.0", manifest("0.5.0", s.now.Unix(), map[string]string{"linux-amd64": "the whole release"}))
	if got := s.want(http.StatusOK, "GET", base+"0.5.0/linux-amd64", ""); got != "the whole release" {
		t.Errorf("program = %q", got)
	}
}

func TestEveryRequestMustBeSignedByTheGroup(t *testing.T) {
	s := newSite(t)
	s.publish("0.5.0", first)
	for name, change := range map[string]func(*http.Request){
		"unsigned": func(r *http.Request) { r.Header.Del(updates.HeaderSignature) },
		"another secret": func(r *http.Request) {
			updates.SignRequest(r, []byte("ffffffffffffffffffffffffffffffff"), updates.EmptySHA, s.now)
		},
		"an old request": func(r *http.Request) { updates.SignRequest(r, secret, updates.EmptySHA, s.now.Add(-6*time.Minute)) },
	} {
		for _, path := range []string{"latest", "0.5.0", "0.5.0/linux-amd64"} {
			if got, text := s.call("GET", base+path, "", change); got != http.StatusUnauthorized {
				t.Errorf("%s: GET %s = %d %q", name, path, got, text)
			}
		}
		if got, _ := s.call("PUT", base+"0.6.0/linux-amd64", "x", func(r *http.Request) {
			change(r)
			if name == "another secret" {
				updates.SignRequest(r, []byte("ffffffffffffffffffffffffffffffff"), sum("x"), s.now)
			}
		}); got != http.StatusUnauthorized && name != "an old request" {
			t.Errorf("%s: PUT = %d", name, got)
		}
	}
	// A clock that is off is said to be so.
	_, text := s.call("GET", base+"latest", "", func(r *http.Request) {
		updates.SignRequest(r, secret, updates.EmptySHA, s.now.Add(6*time.Minute))
	})
	if !strings.Contains(text, "clock") {
		t.Errorf("the answer to a request from another time does not mention clocks: %q", text)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "0.6.0")); err == nil {
		t.Error("an unsigned upload left something behind")
	}
}

func TestABodyOtherThanTheSignedOneIsRefusedAndLeavesNothing(t *testing.T) {
	s := newSite(t)
	got, _ := s.call("PUT", base+"0.5.0/linux-amd64", "what arrives", func(r *http.Request) {
		updates.SignRequest(r, secret, sum("what was signed"), s.now)
	})
	if got != http.StatusBadRequest {
		t.Errorf("PUT = %d", got)
	}
	left, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		if e.Name() != "0.5.0" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	s.want(http.StatusBadRequest, "GET", base+"latest", "", func(r *http.Request) {
		updates.SignRequest(r, secret, sum("a body on a read"), s.now)
	})
}

func TestAPublishedReleaseCannotBeChanged(t *testing.T) {
	s := newSite(t)
	s.publish("0.5.0", first)
	// The same bytes again are fine: a release script may be run twice.
	s.want(http.StatusOK, "PUT", base+"0.5.0/linux-amd64", first["linux-amd64"])
	s.want(http.StatusOK, "PUT", base+"0.5.0", manifest("0.5.0", s.now.Unix(), first))
	before := s.want(http.StatusOK, "GET", base+"0.5.0", "")
	// Signed again later, with the same programs: accepted, and the manifest
	// first published stays.
	s.want(http.StatusOK, "PUT", base+"0.5.0", manifest("0.5.0", s.now.Unix()+60, first))
	if after := s.want(http.StatusOK, "GET", base+"0.5.0", ""); after != before {
		t.Errorf("the manifest changed:\n%s\n%s", before, after)
	}
	s.want(http.StatusConflict, "PUT", base+"0.5.0/linux-amd64", "something else")
	s.want(http.StatusConflict, "PUT", base+"0.5.0/darwin-arm64", "a late addition")
	s.want(http.StatusConflict, "PUT", base+"0.5.0", manifest("0.5.0", s.now.Unix(), map[string]string{"linux-amd64": first["linux-amd64"]}))
	s.want(http.StatusConflict, "PUT", base+"0.5.0", manifest("0.5.0", s.now.Unix(),
		map[string]string{"linux-amd64": "something else", "windows-amd64": first["windows-amd64"]}))
	if got := s.want(http.StatusOK, "GET", base+"0.5.0/linux-amd64", ""); got != first["linux-amd64"] {
		t.Errorf("program = %q", got)
	}
}

func TestAReleaseRunAgainFinishesWhatAnInterruptedOneLeft(t *testing.T) {
	s := newSite(t)
	s.publish("0.5.0", first)
	second := map[string]string{"linux-amd64": "linux program two"}
	s.publish("0.5.1", second)
	// As if the server had stopped between storing the manifest of 0.5.1
	// and making it the latest.
	if err := os.WriteFile(filepath.Join(s.dir, "latest"), []byte("0.5.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.want(http.StatusOK, "PUT", base+"0.5.1/linux-amd64", second["linux-amd64"])
	s.want(http.StatusOK, "PUT", base+"0.5.1", manifest("0.5.1", s.now.Unix()+60, second))
	if got := s.want(http.StatusOK, "GET", base+"latest", ""); !strings.Contains(got, `"version":"0.5.1"`) {
		t.Errorf("latest = %q", got)
	}
	// Running an old release again does not move the latest back.
	s.want(http.StatusOK, "PUT", base+"0.5.0", manifest("0.5.0", s.now.Unix()+120, first))
	if got := s.want(http.StatusOK, "GET", base+"latest", ""); !strings.Contains(got, `"version":"0.5.1"`) {
		t.Errorf("latest after the old release was run again = %q", got)
	}
}

func TestTheLogSaysHowEachRequestWasAnswered(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var log bytes.Buffer
	s := &site{t: t, dir: dir, now: time.Unix(1795300000, 0)}
	s.srv = httptest.NewServer(New(root, secret, func() time.Time { return s.now }, slog.New(slog.NewTextHandler(&log, nil))))
	defer s.srv.Close()
	s.publish("0.5.0", map[string]string{"linux-amd64": "linux program one"})
	s.want(http.StatusPartialContent, "GET", base+"0.5.0/linux-amd64", "", func(r *http.Request) { r.Header.Set("Range", "bytes=0-4") })
	s.want(http.StatusNotFound, "GET", base+"0.9.9", "")
	for _, want := range []string{"method=PUT path=/geminus/updates/0.5.0/linux-amd64 status=201", "status=206", "status=404 reason=\"no such release\""} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, log.String())
		}
	}
}

func TestAManifestMustBeSignedMatchItsFilesAndBeNewer(t *testing.T) {
	s := newSite(t)
	s.publish("0.5.0", first)
	s.want(http.StatusCreated, "PUT", base+"0.6.0/linux-amd64", "linux program two")
	good := map[string]string{"linux-amd64": "linux program two"}
	forged, _ := json.Marshal(updates.Seal([]byte("ffffffffffffffffffffffffffffffff"), "0.6.0",
		map[string]updates.File{"linux-amd64": {SHA256: sum(good["linux-amd64"]), Size: 17}}, s.now.Unix()))
	for name, body := range map[string]string{
		"not JSON":             "{",
		"another secret":       string(forged),
		"another version":      manifest("0.6.1", s.now.Unix(), good),
		"a file not uploaded":  manifest("0.6.0", s.now.Unix(), map[string]string{"linux-amd64": good["linux-amd64"], "windows-amd64": "missing"}),
		"a file that differs":  manifest("0.6.0", s.now.Unix(), map[string]string{"linux-amd64": "linux program 2.0"}),
		"a file of other size": manifest("0.6.0", s.now.Unix(), map[string]string{"linux-amd64": "short"}),
	} {
		if got, text := s.call("PUT", base+"0.6.0", body); got != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d %q", name, got, text)
		}
	}
	if got := s.want(http.StatusOK, "GET", base+"latest", ""); !strings.Contains(got, `"version":"0.5.0"`) {
		t.Errorf("latest = %q", got)
	}
	// An older version cannot become the latest.
	s.want(http.StatusCreated, "PUT", base+"0.4.9/linux-amd64", "an old program")
	s.want(http.StatusConflict, "PUT", base+"0.4.9", manifest("0.4.9", s.now.Unix(), map[string]string{"linux-amd64": "an old program"}))
	s.want(http.StatusCreated, "PUT", base+"0.6.0", manifest("0.6.0", s.now.Unix(), good))
}

func TestNamesThatAreNotAReleaseOrATargetAreRefused(t *testing.T) {
	s := newSite(t)
	s.publish("0.5.0", first)
	outside := filepath.Join(filepath.Dir(s.dir), "outside")
	for _, path := range []string{
		"latest/linux-amd64", "0.5.0-dev/linux-amd64", "v0.5.0/linux-amd64", "0.5.0/linux", "0.5.0/linux-amd64.exe",
		"0.5.0/manifest.json", "0.5.0/..%2f..%2foutside", "..%2foutside/linux-amd64", "0.5.0/linux-amd64/more", "%2e%2e/linux-amd64",
	} {
		for _, method := range []string{"GET", "PUT"} {
			body := ""
			if method == "PUT" {
				body = "x"
			}
			if got, text := s.call(method, base+path, body); got < 400 || got >= 500 {
				t.Errorf("%s %s = %d %q", method, path, got, text)
			}
		}
	}
	for _, path := range []string{"../outside", "0.5.0/../../outside", "latest"} {
		if got, _ := s.call("PUT", base+path, "x"); got == http.StatusCreated || got == http.StatusOK {
			t.Errorf("PUT %s = %d", path, got)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("something was written outside the directory")
	}
	for _, method := range []string{"POST", "DELETE", "PATCH"} {
		if got, _ := s.call(method, base+"0.5.0/linux-amd64", ""); got != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d", method, got)
		}
	}
	if got, _ := s.call("GET", "/", ""); got != http.StatusNotFound {
		t.Errorf("GET / = %d", got)
	}
}

func TestUploadsHaveALimit(t *testing.T) {
	s := newSite(t)
	big := strings.Repeat("m", maxManifest+1)
	if got, _ := s.call("PUT", base+"0.5.0", big); got != http.StatusRequestEntityTooLarge {
		t.Errorf("a manifest over the limit: %d", got)
	}
	req, _ := http.NewRequest("PUT", s.srv.URL+base+"0.5.0/linux-amd64", io.LimitReader(zeros{}, maxProgram+1))
	updates.SignRequest(req, secret, sum("not what is sent"), s.now)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("a program over the limit: %d", resp.StatusCode)
		}
	}
	left, _ := os.ReadDir(s.dir)
	for _, e := range left {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestLeftoversOfAnInterruptedUploadAreRemovedAtStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".tmp-123"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var log bytes.Buffer
	New(root, secret, time.Now, slog.New(slog.NewTextHandler(&log, nil)))
	if _, err := os.Stat(filepath.Join(dir, ".tmp-123")); err == nil {
		t.Error("the leftover is still there")
	}
}
