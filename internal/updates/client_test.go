package updates_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hauntedfail/geminus/internal/updates"
	"github.com/hauntedfail/geminus/internal/updates/server"
)

var (
	secret = []byte("0123456789abcdef0123456789abcdef")
	// key stands in for the key of the computer that builds releases.
	key    = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	public = key.Public().(ed25519.PublicKey)
)

// builds stands in for building from source: the test says what a version
// builds into, or why it fails.
type builds struct {
	mu       sync.Mutex
	programs map[string]map[string]string
	fail     error
	// hold, when set, keeps every build from finishing until it is closed.
	hold chan struct{}
}

func (b *builds) build(ctx context.Context, version, dir string) error {
	b.mu.Lock()
	programs, fail, hold := b.programs[version], b.fail, b.hold
	b.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail != nil {
		return fail
	}
	for target, body := range programs {
		if err := os.WriteFile(filepath.Join(dir, target), []byte(body), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// channel is a real server over a temporary directory and a client for it.
// wrap, when set, stands between the two.
func channel(t *testing.T, wrap func(http.Handler) http.Handler) (*updates.Client, *builds) {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &builds{programs: map[string]map[string]string{}}
	channel := server.New(server.Config{Root: root, Secret: secret, Key: key, Build: b.build,
		Now: time.Now, Log: slog.New(slog.DiscardHandler)})
	var h http.Handler = channel
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); channel.Close(); root.Close() })
	return &updates.Client{Base: srv.URL + updates.Path, Secret: secret, Key: public, HTTP: srv.Client(),
		Now: time.Now, Poll: 5 * time.Millisecond}, b
}

// release has the server build version into programs.
func release(t *testing.T, c *updates.Client, b *builds, version string, programs map[string]string) updates.Manifest {
	t.Helper()
	b.mu.Lock()
	b.programs[version] = programs
	b.mu.Unlock()
	m, err := c.Release(ctx, version)
	if err != nil {
		t.Fatalf("Release %s: %v", version, err)
	}
	return m
}

var ctx = context.Background()

func TestAReleaseIsBuiltFoundAndFetched(t *testing.T) {
	c, b := channel(t, nil)
	if _, err := c.Latest(ctx); !errors.Is(err, updates.ErrNoRelease) {
		t.Fatalf("Latest on an empty server: %v", err)
	}
	programs := map[string]string{"linux-amd64": "the linux program", "windows-amd64": "the windows program"}
	first := release(t, c, b, "0.5.0", programs)
	if first.Version != "0.5.0" || len(first.Files) != 2 || !first.Verify(public) {
		t.Fatalf("Release = %+v", first)
	}
	m, err := c.Latest(ctx)
	if err != nil || m.Version != "0.5.0" || len(m.Files) != 2 {
		t.Fatalf("Latest = %+v, %v", m, err)
	}
	var got bytes.Buffer
	if err := c.Fetch(ctx, m, "windows-amd64", &got); err != nil || got.String() != programs["windows-amd64"] {
		t.Fatalf("Fetch = %q, %v", got.String(), err)
	}
	if err := c.Fetch(ctx, m, "darwin-arm64", io.Discard); err == nil || !strings.Contains(err.Error(), "darwin-arm64") {
		t.Errorf("Fetch of a target the release lacks: %v", err)
	}
	// Asked for again, the release is the one already there, whatever the
	// source would build into now.
	again := release(t, c, b, "0.5.0", map[string]string{"linux-amd64": "another linux program"})
	if again.Sig != first.Sig {
		t.Errorf("a published release was built again: %+v", again)
	}
	if _, err := c.Release(ctx, "0.6.0-dev"); err == nil || !strings.Contains(err.Error(), "X.Y.Z") {
		t.Errorf("Release of a version that is not a release: %v", err)
	}
}

func TestAFailedBuildIsReportedWithItsReason(t *testing.T) {
	c, b := channel(t, nil)
	b.fail = errors.New("go build linux-amd64: exit status 1\n./main.go:3:1: \x1b[31mundefined: x")
	_, err := c.Release(ctx, "0.5.0")
	if err == nil {
		t.Fatal("Release succeeded")
	}
	if text := err.Error(); !strings.Contains(text, "0.5.0") || !strings.Contains(text, "undefined: x") || strings.ContainsAny(text, "\x1b\n") {
		t.Errorf("error = %q", text)
	}
	if _, err := c.Latest(ctx); !errors.Is(err, updates.ErrNoRelease) {
		t.Errorf("Latest after a failed build: %v", err)
	}
}

func TestReleaseWaitsNoLongerThanItsContext(t *testing.T) {
	c, b := channel(t, nil)
	b.hold = make(chan struct{})
	defer close(b.hold)
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := c.Release(short, "0.5.0"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Release while the build goes on: %v", err)
	}
}

// between answers every question about a build with status until it has
// been asked times times; times < 0 means always.
func between(status int, times int) (wrap func(http.Handler) http.Handler, asked func() int) {
	var mu sync.Mutex
	n := 0
	wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/build") {
				mu.Lock()
				n++
				fail := times < 0 || n <= times
				mu.Unlock()
				if fail {
					http.Error(w, "the tunnel has a moment", status)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	return wrap, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func TestReleaseOutlastsATunnelThatHiccups(t *testing.T) {
	wrap, asked := between(http.StatusBadGateway, 3)
	c, b := channel(t, wrap)
	if m := release(t, c, b, "0.5.0", map[string]string{"linux-amd64": "the linux program"}); m.Version != "0.5.0" {
		t.Errorf("Release = %+v", m)
	}
	if asked() < 4 {
		t.Errorf("the build was asked about %d times", asked())
	}
}

func TestReleaseGivesUpOnAServerThatStaysAway(t *testing.T) {
	wrap, asked := between(http.StatusBadGateway, -1)
	c, b := channel(t, wrap)
	b.programs["0.5.0"] = map[string]string{"linux-amd64": "the linux program"}
	_, err := c.Release(ctx, "0.5.0")
	if err == nil || !strings.Contains(err.Error(), "lost touch") || !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v", err)
	}
	if asked() != 5 {
		t.Errorf("the build was asked about %d times, want 5", asked())
	}
}

func TestReleaseSaysWhenTheServerForgotTheBuild(t *testing.T) {
	// As after a restart of the server in the middle of a build.
	wrap, asked := between(http.StatusNotFound, -1)
	c, b := channel(t, wrap)
	b.hold = make(chan struct{})
	defer close(b.hold)
	_, err := c.Release(ctx, "0.5.0")
	if err == nil || !strings.Contains(err.Error(), "run the release again") {
		t.Errorf("err = %v", err)
	}
	if asked() != 1 {
		t.Errorf("the build was asked about %d times, want 1", asked())
	}
	// A refusal that asking again cannot change ends the wait at once.
	wrap, asked = between(http.StatusUnauthorized, -1)
	c, b = channel(t, wrap)
	b.hold = make(chan struct{})
	defer close(b.hold)
	if _, err := c.Release(ctx, "0.5.0"); err == nil || !strings.Contains(err.Error(), "401") || asked() != 1 {
		t.Errorf("err = %v after %d questions", err, asked())
	}
}

func TestAManifestNotSignedWithTheReleaseKeyIsNotBelieved(t *testing.T) {
	c, b := channel(t, nil)
	release(t, c, b, "0.5.0", map[string]string{"linux-amd64": "the linux program"})
	// The same server, read by someone who trusts another secret: the
	// request is refused before any manifest is seen.
	other := *c
	other.Secret = []byte("ffffffffffffffffffffffffffffffff")
	if _, err := other.Latest(ctx); err == nil {
		t.Error("a stranger read the latest release")
	}
	// A geminus that trusts another key does not take sibyl's release.
	other = *c
	other.Key = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if _, err := other.Latest(ctx); err == nil || !strings.Contains(err.Error(), "signed") {
		t.Errorf("Latest with another release key: %v", err)
	}
	// A server that answers with a manifest of its own making. It holds the
	// group's secret, as every member does, and still cannot sign a release.
	forged, err := json.Marshal(updates.Seal(ed25519.NewKeyFromSeed(secret), "9.9.9",
		map[string]updates.File{"linux-amd64": {SHA256: strings.Repeat("a", 64), Size: 3}}, 1))
	if err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/build") || r.Method == http.MethodPost {
			w.Write([]byte(`{"version":"9.9.9","state":"published"}`))
			return
		}
		w.Write(forged)
	}))
	defer fake.Close()
	c.Base, c.HTTP = fake.URL+updates.Path, fake.Client()
	if _, err := c.Latest(ctx); err == nil || !strings.Contains(err.Error(), "signed") {
		t.Errorf("Latest believed a forged manifest: %v", err)
	}
	if _, err := c.Release(ctx, "9.9.9"); err == nil || !strings.Contains(err.Error(), "signed") {
		t.Errorf("Release believed a forged manifest: %v", err)
	}
}

func TestWhatIsFetchedMustBeTheFileTheManifestNames(t *testing.T) {
	const program = "the linux program, whole"
	for name, damage := range map[string]func(body []byte) []byte{
		"cut short":   func(b []byte) []byte { return b[:len(b)-5] },
		"other bytes": func(b []byte) []byte { return bytes.ToUpper(b) },
		"too long":    func(b []byte) []byte { return append(b, "and more"...) },
	} {
		c, b := channel(t, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/linux-amd64") {
					next.ServeHTTP(w, r)
					return
				}
				rec := httptest.NewRecorder()
				next.ServeHTTP(rec, r)
				w.Write(damage(rec.Body.Bytes()))
			})
		})
		m := release(t, c, b, "0.5.0", map[string]string{"linux-amd64": program})
		if err := c.Fetch(ctx, m, "linux-amd64", io.Discard); err == nil {
			t.Errorf("%s: Fetch succeeded", name)
		}
	}
}

func TestAnErrorPageIsReportedInOneLineWithoutControlCharacters(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<html>\x1b[31mbad gateway\n"+strings.Repeat("x", 5000), http.StatusBadGateway)
	}))
	defer fake.Close()
	c := &updates.Client{Base: fake.URL + updates.Path, Secret: secret, HTTP: fake.Client(), Now: time.Now}
	_, err := c.Latest(ctx)
	if err == nil {
		t.Fatal("Latest succeeded")
	}
	if text := err.Error(); !strings.Contains(text, "502") || strings.ContainsAny(text, "\x1b\n") || len(text) > 400 {
		t.Errorf("error = %q", text)
	}
	// Text that would reorder the line, and a long line of wide characters.
	fake.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "safe\u202egnp.exe "+strings.Repeat("語", 5000), http.StatusBadGateway)
	})
	_, err = c.Latest(ctx)
	if text := err.Error(); strings.ContainsRune(text, 0x202e) || !utf8.ValidString(text) || utf8.RuneCountInString(text) > 300 {
		t.Errorf("error = %q", text)
	}
}

func TestASignedRequestIsNotCarriedToAnotherAddress(t *testing.T) {
	var carried http.Header
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { carried = r.Header }))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirecting.Close()
	c := &updates.Client{Base: redirecting.URL + updates.Path, Secret: secret, Now: time.Now,
		HTTP: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if _, err := c.Latest(ctx); err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("Latest through a redirect: %v", err)
	}
	if carried != nil {
		t.Errorf("the signature went to another address: %v", carried)
	}
}

func TestBaseComesFromTheEnvironmentOrIsSibyl(t *testing.T) {
	if got := updates.Base(func(string) string { return "" }); got != "https://sibyl.haunted.fail/geminus/updates" {
		t.Errorf("Base = %q", got)
	}
	env := map[string]string{"GEMINUS_UPDATES_URL": "http://127.0.0.1:1/geminus/updates/"}
	if got := updates.Base(func(k string) string { return env[k] }); got != "http://127.0.0.1:1/geminus/updates" {
		t.Errorf("Base = %q", got)
	}
}

// program is a stand-in for the running program on disk.
func program(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "geminus")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func content(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func write(text string) func(io.Writer) error {
	return func(w io.Writer) error { _, err := io.WriteString(w, text); return err }
}

func only(t *testing.T, exe string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("the directory holds %v, want %v", got, names)
	}
}

func TestReplaceSwapsTheProgramInPlace(t *testing.T) {
	exe := program(t)
	if err := updates.Replace(exe, "linux", write("new")); err != nil {
		t.Fatal(err)
	}
	if got := content(t, exe); got != "new" {
		t.Errorf("program = %q", got)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(exe); info.Mode().Perm()&0o111 == 0 {
			t.Errorf("the new program is not executable: %v", info.Mode())
		}
	}
	only(t, exe, "geminus")
}

func TestReplaceOnWindowsMovesTheRunningProgramAside(t *testing.T) {
	exe := program(t)
	// An .old from an earlier update is in the way.
	if err := os.WriteFile(exe+".old", []byte("older"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := updates.Replace(exe, "windows", write("new")); err != nil {
		t.Fatal(err)
	}
	if content(t, exe) != "new" || content(t, exe+".old") != "old" {
		t.Errorf("program = %q, .old = %q", content(t, exe), content(t, exe+".old"))
	}
	updates.Sweep(exe)
	only(t, exe, "geminus")
}

// After /update on Windows the process before this one stays behind, running
// from the .old file, which then cannot be removed.
func TestReplaceOnWindowsWorksWhileTheEarlierProgramIsStillThere(t *testing.T) {
	exe := program(t)
	// A directory with something in it stands in for a file that is in use:
	// neither can be removed.
	if err := os.MkdirAll(filepath.Join(exe+".old", "in use"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := updates.Replace(exe, "windows", write("new")); err != nil {
		t.Fatal(err)
	}
	if got := content(t, exe); got != "new" {
		t.Errorf("program = %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	if len(entries) != 3 {
		t.Fatalf("%d files, want the program, the one in use and the one just moved aside", len(entries))
	}
	updates.Sweep(exe)
	only(t, exe, "geminus", "geminus.old")
}

func TestAFailedDownloadLeavesTheProgramAsItWas(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		exe := program(t)
		err := updates.Replace(exe, goos, func(w io.Writer) error {
			io.WriteString(w, "half of the n")
			return errors.New("the connection broke")
		})
		if err == nil || !strings.Contains(err.Error(), "the connection broke") {
			t.Errorf("%s: err = %v", goos, err)
		}
		if got := content(t, exe); got != "old" {
			t.Errorf("%s: program = %q", goos, got)
		}
		only(t, exe, "geminus")
	}
}

func TestReplaceSaysWhichDirectoryItMayNotWriteTo(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory that chmod can close")
	}
	exe := program(t)
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	err := updates.Replace(exe, "linux", write("new"))
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("err = %v", err)
	}
	if got := content(t, exe); got != "old" {
		t.Errorf("program = %q", got)
	}
}
