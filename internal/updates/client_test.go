package updates_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hauntedfail/geminus/internal/updates"
	"github.com/hauntedfail/geminus/internal/updates/server"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

// channel is a real server over a temporary directory and a client for it.
// wrap, when set, stands between the two.
func channel(t *testing.T, wrap func(http.Handler) http.Handler) *updates.Client {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	var h http.Handler = server.New(root, secret, time.Now, slog.New(slog.DiscardHandler))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &updates.Client{Base: srv.URL + updates.Path, Secret: secret, HTTP: srv.Client(), Now: time.Now}
}

// built writes stand-ins for the programs of a build and returns target → path.
func built(t *testing.T, programs map[string]string) map[string]string {
	t.Helper()
	dir, files := t.TempDir(), map[string]string{}
	for target, body := range programs {
		files[target] = filepath.Join(dir, "geminus-"+target)
		if err := os.WriteFile(files[target], []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

var ctx = context.Background()

func TestAReleaseIsPublishedFoundAndFetched(t *testing.T) {
	c := channel(t, nil)
	if _, err := c.Latest(ctx); !errors.Is(err, updates.ErrNoRelease) {
		t.Fatalf("Latest on an empty server: %v", err)
	}
	programs := map[string]string{"linux-amd64": "the linux program", "windows-amd64": "the windows program"}
	if _, err := c.Publish(ctx, "0.5.0", built(t, programs)); err != nil {
		t.Fatalf("Publish: %v", err)
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
	// A published release cannot be given other programs.
	_, err = c.Publish(ctx, "0.5.0", built(t, map[string]string{"linux-amd64": "another linux program"}))
	if err == nil || !strings.Contains(err.Error(), "cannot be changed") {
		t.Errorf("a release was published again with other bytes: %v", err)
	}
}

func TestAManifestNotSignedByTheGroupIsNotBelieved(t *testing.T) {
	c := channel(t, nil)
	if _, err := c.Publish(ctx, "0.5.0", built(t, map[string]string{"linux-amd64": "the linux program"})); err != nil {
		t.Fatal(err)
	}
	// The same server, read by someone who trusts another secret: the
	// request is refused before any manifest is seen.
	other := *c
	other.Secret = []byte("ffffffffffffffffffffffffffffffff")
	if _, err := other.Latest(ctx); err == nil {
		t.Error("a stranger read the latest release")
	}
	// A server that answers with a manifest of its own making.
	forged := updates.Seal([]byte("ffffffffffffffffffffffffffffffff"), "9.9.9",
		map[string]updates.File{"linux-amd64": {SHA256: strings.Repeat("a", 64), Size: 3}}, 1)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"version":"9.9.9","ts":1,"files":{"linux-amd64":{"sha256":"` + forged.Files["linux-amd64"].SHA256 + `","size":3}},"sig":"` + forged.Sig + `"}`))
	}))
	defer fake.Close()
	c.Base, c.HTTP = fake.URL+updates.Path, fake.Client()
	if _, err := c.Latest(ctx); err == nil || !strings.Contains(err.Error(), "signed") {
		t.Errorf("Latest believed a forged manifest: %v", err)
	}
}

func TestWhatIsFetchedMustBeTheFileTheManifestNames(t *testing.T) {
	const program = "the linux program, whole"
	for name, damage := range map[string]func(body []byte) []byte{
		"cut short":   func(b []byte) []byte { return b[:len(b)-5] },
		"other bytes": func(b []byte) []byte { return bytes.ToUpper(b) },
		"too long":    func(b []byte) []byte { return append(b, "and more"...) },
	} {
		c := channel(t, func(next http.Handler) http.Handler {
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
		m, err := c.Publish(ctx, "0.5.0", built(t, map[string]string{"linux-amd64": program}))
		if err != nil {
			t.Fatal(err)
		}
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
