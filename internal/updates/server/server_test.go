package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hauntedfail/geminus/internal/updates"
)

var (
	secret = []byte("0123456789abcdef0123456789abcdef")
	// key stands in for the key of the computer that builds releases.
	key    = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	public = key.Public().(ed25519.PublicKey)
)

const base = "/geminus/updates/"

// site is a server over a temporary directory, with a clock the test moves
// and a stand-in for building from source.
type site struct {
	t   *testing.T
	dir string
	now time.Time
	srv *httptest.Server
	log bytes.Buffer

	mu sync.Mutex
	// programs is what a version builds into; make, when set, builds instead.
	programs map[string]map[string]string
	make     func(ctx context.Context, version, dir string) error
	built    []string
}

func (s *site) build(ctx context.Context, version, dir string) error {
	s.mu.Lock()
	s.built = append(s.built, version)
	programs, make := s.programs[version], s.make
	s.mu.Unlock()
	if make != nil {
		return make(ctx, version, dir)
	}
	for target, body := range programs {
		if err := os.WriteFile(filepath.Join(dir, target), []byte(body), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (s *site) builds() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.built)
}

func (s *site) set(make func(ctx context.Context, version, dir string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.make = make
}

func newSite(t *testing.T) *site {
	t.Helper()
	s := &site{t: t, dir: t.TempDir(), now: time.Unix(1795300000, 0), programs: map[string]map[string]string{}}
	s.start()
	return s
}

// start runs a server over the directory; a test may call it again to see
// what a server started later does.
func (s *site) start() *Server {
	s.t.Helper()
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		s.t.Fatal(err)
	}
	channel := New(Config{Root: root, Secret: secret, Key: key, Build: s.build,
		Now: func() time.Time { return s.now }, Log: slog.New(slog.NewTextHandler(&syncWriter{w: &s.log, mu: &s.mu}, nil))})
	s.srv = httptest.NewServer(channel)
	srv := s.srv
	s.t.Cleanup(func() { srv.Close(); channel.Close(); root.Close() })
	return channel
}

// syncWriter lets the build, which logs beside the requests, share a buffer.
type syncWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func (s *site) logged() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.log.String()
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

// state is what the server says about the build of version.
func (s *site) state(version string) updates.Build {
	s.t.Helper()
	var b updates.Build
	if err := json.Unmarshal([]byte(s.want(http.StatusOK, "GET", base+version+"/build", "")), &b); err != nil {
		s.t.Fatal(err)
	}
	return b
}

// settled waits until the build of version is over and returns how it ended.
func (s *site) settled(version string) updates.Build {
	s.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if b := s.state(version); b.State != updates.Building {
			return b
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("the build of %s did not end", version)
		}
	}
}

// release has the server build version into programs and waits for it.
func (s *site) release(version string, programs map[string]string) {
	s.t.Helper()
	s.mu.Lock()
	s.programs[version] = programs
	s.mu.Unlock()
	s.want(http.StatusAccepted, "POST", base+version, "")
	if b := s.settled(version); b.State != updates.Published {
		s.t.Fatalf("release %s: %+v", version, b)
	}
}

func (s *site) leftovers() {
	s.t.Helper()
	left, err := os.ReadDir(s.dir)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, e := range left {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			s.t.Errorf("left behind: %s", e.Name())
		}
	}
}

var first = map[string]string{"linux-amd64": "linux program one", "windows-amd64": "windows program one"}

func TestAReleaseIsBuiltSignedAndServedAsTheLatest(t *testing.T) {
	s := newSite(t)
	s.want(http.StatusNotFound, "GET", base+"latest", "")
	s.want(http.StatusNotFound, "GET", base+"0.5.0/build", "")
	s.release("0.5.0", first)

	var m updates.Manifest
	if err := json.Unmarshal([]byte(s.want(http.StatusOK, "GET", base+"latest", "")), &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "0.5.0" || !m.Verify(public) || m.TS != s.now.Unix() || len(m.Files) != 2 ||
		m.Files["windows-amd64"] != (updates.File{SHA256: sum(first["windows-amd64"]), Size: int64(len(first["windows-amd64"]))}) {
		t.Errorf("latest = %+v", m)
	}
	if got := s.want(http.StatusOK, "GET", base+"0.5.0/windows-amd64", ""); got != first["windows-amd64"] {
		t.Errorf("program = %q", got)
	}
	if got := s.want(http.StatusOK, "GET", base+"0.5.0", ""); !strings.Contains(got, `"version":"0.5.0"`) {
		t.Errorf("manifest = %q", got)
	}

	s.release("0.5.1", map[string]string{"linux-amd64": "linux program two"})
	if got := s.want(http.StatusOK, "GET", base+"latest", ""); !strings.Contains(got, `"version":"0.5.1"`) {
		t.Errorf("latest = %q", got)
	}
	// The earlier release is still there.
	s.want(http.StatusOK, "GET", base+"0.5.0/linux-amd64", "")
	s.want(http.StatusNotFound, "GET", base+"0.5.1/windows-amd64", "")
	s.leftovers()
}

func TestNothingIsServedWhileAReleaseIsBeingBuilt(t *testing.T) {
	s := newSite(t)
	finish := make(chan struct{})
	s.set(func(_ context.Context, _, dir string) error {
		<-finish
		return os.WriteFile(filepath.Join(dir, "linux-amd64"), []byte("the whole release"), 0o755)
	})
	if got := s.want(http.StatusAccepted, "POST", base+"0.5.0", ""); !strings.Contains(got, `"state":"building"`) {
		t.Errorf("answer = %q", got)
	}
	if b := s.state("0.5.0"); b.State != updates.Building || b.Version != "0.5.0" {
		t.Errorf("state = %+v", b)
	}
	s.want(http.StatusNotFound, "GET", base+"0.5.0/linux-amd64", "")
	s.want(http.StatusNotFound, "GET", base+"0.5.0", "")
	s.want(http.StatusNotFound, "GET", base+"latest", "")
	// Asked for again, it is the same build; another release waits its turn.
	s.want(http.StatusAccepted, "POST", base+"0.5.0", "")
	if got := s.want(http.StatusConflict, "POST", base+"0.5.1", ""); !strings.Contains(got, "0.5.0 is being built") {
		t.Errorf("answer = %q", got)
	}
	close(finish)
	if b := s.settled("0.5.0"); b.State != updates.Published {
		t.Fatalf("state = %+v", b)
	}
	if n := s.builds(); n != 1 {
		t.Errorf("%d builds, want 1", n)
	}
	if got := s.want(http.StatusOK, "GET", base+"0.5.0/linux-amd64", ""); got != "the whole release" {
		t.Errorf("program = %q", got)
	}
}

func TestEveryRequestMustBeSignedByTheGroup(t *testing.T) {
	s := newSite(t)
	s.release("0.5.0", first)
	for name, change := range map[string]func(*http.Request){
		"unsigned": func(r *http.Request) { r.Header.Del(updates.HeaderSignature) },
		"another secret": func(r *http.Request) {
			updates.SignRequest(r, []byte("ffffffffffffffffffffffffffffffff"), updates.EmptySHA, s.now)
		},
		"an old request": func(r *http.Request) { updates.SignRequest(r, secret, updates.EmptySHA, s.now.Add(-6*time.Minute)) },
	} {
		for _, path := range []string{"latest", "0.5.0", "0.5.0/linux-amd64", "0.5.0/build"} {
			if got, text := s.call("GET", base+path, "", change); got != http.StatusUnauthorized {
				t.Errorf("%s: GET %s = %d %q", name, path, got, text)
			}
		}
		if got, text := s.call("POST", base+"0.6.0", "", change); got != http.StatusUnauthorized {
			t.Errorf("%s: POST = %d %q", name, got, text)
		}
	}
	// A clock that is off is said to be so.
	_, text := s.call("GET", base+"latest", "", func(r *http.Request) {
		updates.SignRequest(r, secret, updates.EmptySHA, s.now.Add(6*time.Minute))
	})
	if !strings.Contains(text, "clock") {
		t.Errorf("the answer to a request from another time does not mention clocks: %q", text)
	}
	if n := s.builds(); n != 1 {
		t.Errorf("a request that was refused started a build: %d builds", n)
	}
	if strings.Contains(s.logged(), "status=401") {
		t.Errorf("requests from outside the group fill the log:\n%s", s.logged())
	}
}

func TestNoRequestCarriesABody(t *testing.T) {
	s := newSite(t)
	for _, method := range []string{"GET", "POST"} {
		path := base + "latest"
		if method == "POST" {
			path = base + "0.5.0"
		}
		if got, text := s.call(method, path, "a body"); got != http.StatusBadRequest {
			t.Errorf("%s with a body = %d %q", method, got, text)
		}
	}
	if n := s.builds(); n != 0 {
		t.Errorf("%d builds", n)
	}
}

func TestAFailedBuildSaysWhyAndCanBeRunAgain(t *testing.T) {
	s := newSite(t)
	s.set(func(_ context.Context, _, dir string) error {
		// What a build wrote before it failed is not a release.
		os.WriteFile(filepath.Join(dir, "linux-amd64"), []byte("half built"), 0o755)
		return errors.New("go build windows-amd64: exit status 1\n./main.go:3:1: undefined: x")
	})
	s.want(http.StatusAccepted, "POST", base+"0.5.0", "")
	b := s.settled("0.5.0")
	if b.State != updates.Failed || !strings.Contains(b.Error, "undefined: x") {
		t.Fatalf("state = %+v", b)
	}
	s.want(http.StatusNotFound, "GET", base+"0.5.0", "")
	s.want(http.StatusNotFound, "GET", base+"0.5.0/linux-amd64", "")
	s.want(http.StatusNotFound, "GET", base+"latest", "")
	if !strings.Contains(s.logged(), "undefined: x") {
		t.Errorf("the log does not say why the build failed:\n%s", s.logged())
	}
	s.leftovers()

	s.set(nil)
	s.release("0.5.0", first)
	if got := s.want(http.StatusOK, "GET", base+"0.5.0/linux-amd64", ""); got != first["linux-amd64"] {
		t.Errorf("program = %q", got)
	}
}

func TestAPublishedReleaseIsNeverBuiltAgain(t *testing.T) {
	s := newSite(t)
	s.release("0.5.0", first)
	before := s.want(http.StatusOK, "GET", base+"0.5.0", "")
	s.mu.Lock()
	s.programs["0.5.0"] = map[string]string{"linux-amd64": "something else"}
	s.mu.Unlock()
	if got := s.want(http.StatusOK, "POST", base+"0.5.0", ""); !strings.Contains(got, `"state":"published"`) {
		t.Errorf("answer = %q", got)
	}
	if n := s.builds(); n != 1 {
		t.Errorf("%d builds, want 1", n)
	}
	if after := s.want(http.StatusOK, "GET", base+"0.5.0", ""); after != before {
		t.Errorf("the manifest changed:\n%s\n%s", before, after)
	}
	if got := s.want(http.StatusOK, "GET", base+"0.5.0/linux-amd64", ""); got != first["linux-amd64"] {
		t.Errorf("program = %q", got)
	}
	// A server started later knows the release too.
	s.start()
	if b := s.state("0.5.0"); b.State != updates.Published {
		t.Errorf("state after a restart = %+v", b)
	}
}

func TestAReleaseAskedForAgainFinishesWhatAnInterruptedOneLeft(t *testing.T) {
	s := newSite(t)
	s.release("0.5.0", first)
	s.release("0.5.1", map[string]string{"linux-amd64": "linux program two"})
	// As if the server had stopped between storing the manifest of 0.5.1
	// and making it the latest.
	if err := os.WriteFile(filepath.Join(s.dir, "latest"), []byte("0.5.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.want(http.StatusOK, "POST", base+"0.5.1", "")
	if got := s.want(http.StatusOK, "GET", base+"latest", ""); !strings.Contains(got, `"version":"0.5.1"`) {
		t.Errorf("latest = %q", got)
	}
	// Asking for an old release again does not move the latest back.
	s.want(http.StatusOK, "POST", base+"0.5.0", "")
	if got := s.want(http.StatusOK, "GET", base+"latest", ""); !strings.Contains(got, `"version":"0.5.1"`) {
		t.Errorf("latest after the old release was asked for again = %q", got)
	}
}

func TestAReleaseMustBeNewerThanTheLatest(t *testing.T) {
	s := newSite(t)
	s.release("0.5.0", first)
	for _, version := range []string{"0.4.9", "0.0.1"} {
		if got := s.want(http.StatusConflict, "POST", base+version, ""); !strings.Contains(got, "not newer than the latest, 0.5.0") {
			t.Errorf("answer = %q", got)
		}
	}
	if n := s.builds(); n != 1 {
		t.Errorf("%d builds, want 1", n)
	}
}

func TestWhatABuildLeavesMustBePrograms(t *testing.T) {
	for name, leave := range map[string]func(dir string) error{
		"nothing": func(string) error { return nil },
		"another file": func(dir string) error {
			os.WriteFile(filepath.Join(dir, "linux-amd64"), []byte("a program"), 0o755)
			return os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a program"), 0o644)
		},
		"an empty program": func(dir string) error { return os.WriteFile(filepath.Join(dir, "linux-amd64"), nil, 0o755) },
		"a directory":      func(dir string) error { return os.Mkdir(filepath.Join(dir, "linux-amd64"), 0o755) },
		"a link": func(dir string) error {
			return os.Symlink(filepath.Join(dir, "..", "elsewhere"), filepath.Join(dir, "linux-amd64"))
		},
		"a huge program": func(dir string) error {
			f, err := os.Create(filepath.Join(dir, "linux-amd64"))
			if err != nil {
				return err
			}
			defer f.Close()
			return f.Truncate(updates.MaxProgram + 1)
		},
	} {
		s := newSite(t)
		s.set(func(_ context.Context, _, dir string) error { return leave(dir) })
		s.want(http.StatusAccepted, "POST", base+"0.5.0", "")
		if b := s.settled("0.5.0"); b.State != updates.Failed || b.Error == "" {
			t.Errorf("%s: state = %+v", name, b)
		}
		s.want(http.StatusNotFound, "GET", base+"latest", "")
		s.want(http.StatusNotFound, "GET", base+"0.5.0/linux-amd64", "")
		s.leftovers()
	}
}

func TestClosingTheServerStopsABuild(t *testing.T) {
	s := newSite(t)
	stopped := make(chan error, 1)
	s.set(func(ctx context.Context, _, _ string) error {
		<-ctx.Done()
		stopped <- ctx.Err()
		return ctx.Err()
	})
	channel := s.start()
	s.want(http.StatusAccepted, "POST", base+"0.5.0", "")
	channel.Close()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the build ended with %v", err)
		}
	default:
		t.Fatal("Close returned while the build was running")
	}
	if got, text := s.call("POST", base+"0.5.1", ""); got != http.StatusServiceUnavailable {
		t.Errorf("POST after Close = %d %q", got, text)
	}
}

func TestABuildHasATimeLimit(t *testing.T) {
	s := newSite(t)
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	channel := New(Config{Root: root, Secret: secret, Key: key, Limit: 20 * time.Millisecond,
		Build: func(ctx context.Context, _, _ string) error { <-ctx.Done(); return ctx.Err() },
		Now:   func() time.Time { return s.now }, Log: slog.New(slog.DiscardHandler)})
	s.srv = httptest.NewServer(channel)
	t.Cleanup(func() { s.srv.Close(); channel.Close(); root.Close() })
	s.want(http.StatusAccepted, "POST", base+"0.5.0", "")
	if b := s.settled("0.5.0"); b.State != updates.Failed || !strings.Contains(b.Error, "deadline") {
		t.Errorf("state = %+v", b)
	}
}

func TestTheLogSaysHowEachRequestWasAnswered(t *testing.T) {
	s := newSite(t)
	s.release("0.5.0", map[string]string{"linux-amd64": "linux program one"})
	s.want(http.StatusPartialContent, "GET", base+"0.5.0/linux-amd64", "", func(r *http.Request) { r.Header.Set("Range", "bytes=0-4") })
	s.want(http.StatusNotFound, "GET", base+"0.9.9", "")
	for _, want := range []string{"method=POST path=/geminus/updates/0.5.0 status=202", "status=206",
		"status=404 reason=\"no such release\"", "msg=published version=0.5.0 files=1"} {
		if !strings.Contains(s.logged(), want) {
			t.Errorf("the log lacks %q:\n%s", want, s.logged())
		}
	}
}

func TestNamesThatAreNotAReleaseOrATargetAreRefused(t *testing.T) {
	s := newSite(t)
	s.release("0.5.0", first)
	outside := filepath.Join(filepath.Dir(s.dir), "outside")
	for _, path := range []string{
		"latest/linux-amd64", "0.5.0-dev/linux-amd64", "v0.5.0/linux-amd64", "0.5.0/linux", "0.5.0/linux-amd64.exe",
		"0.5.0/manifest.json", "0.5.0/..%2f..%2foutside", "..%2foutside/linux-amd64", "0.5.0/linux-amd64/more", "%2e%2e/linux-amd64",
		"latest/build", "0.6.0-dev", "v0.6.0", "..%2foutside", "0.6", "0.06.0",
	} {
		for _, method := range []string{"GET", "POST"} {
			if got, text := s.call(method, base+path, ""); got < 400 || got >= 500 {
				t.Errorf("%s %s = %d %q", method, path, got, text)
			}
		}
	}
	if got, _ := s.call("POST", base+"latest", ""); got != http.StatusNotFound {
		t.Errorf("POST latest = %d", got)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("something was written outside the directory")
	}
	// Nothing can be uploaded, changed or removed from outside.
	for _, method := range []string{"PUT", "DELETE", "PATCH"} {
		for _, path := range []string{"0.5.0/linux-amd64", "0.5.0", "0.6.0", "latest"} {
			if got, _ := s.call(method, base+path, ""); got != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d", method, path, got)
			}
		}
	}
	if got, _ := s.call("POST", base+"0.5.0/linux-amd64", ""); got != http.StatusMethodNotAllowed {
		t.Errorf("POST of a program = %d", got)
	}
	if got, _ := s.call("GET", "/", ""); got != http.StatusNotFound {
		t.Errorf("GET / = %d", got)
	}
	if n := s.builds(); n != 1 {
		t.Errorf("%d builds, want 1", n)
	}
}

func TestLeftoversOfAnInterruptedBuildAreRemovedAtStart(t *testing.T) {
	s := newSite(t)
	if err := os.WriteFile(filepath.Join(s.dir, ".tmp-123"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.start()
	if _, err := os.Stat(filepath.Join(s.dir, ".tmp-123")); err == nil {
		t.Error("the leftover is still there")
	}
}
