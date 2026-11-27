package build

import (
	"context"
	"debug/buildinfo"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const keyText = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// source is a small repository shaped like this one: the same module path
// and the two variables a release build sets.
type source struct {
	t   *testing.T
	dir string
}

var files = map[string]string{
	"go.mod":                      "module github.com/hauntedfail/geminus\n\ngo 1.25\n",
	"internal/version/version.go": "package version\n\nvar Version = \"dev\"\n",
	"internal/updates/key.go":     "package updates\n\nvar releaseKey = \"\"\n\nfunc Key() string { return releaseKey }\n",
	"cmd/geminus/main.go": `package main

import (
	"fmt"

	"github.com/hauntedfail/geminus/internal/updates"
	"github.com/hauntedfail/geminus/internal/version"
)

func main() { fmt.Println("geminus", version.Version, updates.Key()) }
`,
}

func newSource(t *testing.T) *source {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	s := &source{t: t, dir: t.TempDir()}
	s.git("init", "-q", "-b", "main")
	for name, content := range files {
		s.write(name, content)
	}
	s.commit("the first version")
	return s
}

func (s *source) write(name, content string) {
	s.t.Helper()
	path := filepath.Join(s.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *source) git(args ...string) {
	s.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", s.dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		s.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (s *source) commit(message string) {
	s.t.Helper()
	s.git("add", "-A")
	s.git("commit", "-q", "-m", message)
}

func (s *source) builder(targets ...string) Source {
	if len(targets) == 0 {
		targets = []string{runtime.GOOS + "-" + runtime.GOARCH}
	}
	return Source{Repo: s.dir, Git: "git", Go: "go", Targets: targets, Key: keyText}
}

func run(t *testing.T, program string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		// A program needs its extension to be started there.
		exe := program + ".exe"
		if err := os.Rename(program, exe); err != nil {
			t.Fatal(err)
		}
		program = exe
	}
	out, err := exec.Command(program).CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v\n%s", program, err, out)
	}
	return strings.TrimSpace(string(out))
}

var ctx = context.Background()

func TestTheTagOfAVersionIsBuiltWithItsVersionAndTheReleaseKey(t *testing.T) {
	s := newSource(t)
	s.git("tag", "v0.5.0")
	// What came after the tag is not part of the release.
	s.write("cmd/geminus/main.go", strings.Replace(files["cmd/geminus/main.go"], `"geminus"`, `"later"`, 1))
	s.commit("later work")
	s.git("tag", "-a", "-m", "the next release", "v0.6.0")

	out := t.TempDir()
	if err := s.builder().Build(ctx, "0.5.0", out); err != nil {
		t.Fatal(err)
	}
	host := runtime.GOOS + "-" + runtime.GOARCH
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 1 || entries[0].Name() != host {
		t.Fatalf("the build left %v (%v), want only %s", entries, err, host)
	}
	if got := run(t, filepath.Join(out, host)); got != "geminus 0.5.0 "+keyText {
		t.Errorf("the program says %q", got)
	}
	// An annotated tag is a release too.
	out = t.TempDir()
	if err := s.builder().Build(ctx, "0.6.0", out); err != nil {
		t.Fatal(err)
	}
	if got := run(t, filepath.Join(out, host)); got != "later 0.6.0 "+keyText {
		t.Errorf("the program of the later tag says %q", got)
	}
}

func TestTaggedBuildIgnoresCachedPseudoVersion(t *testing.T) {
	// The caller built this commit before it was tagged. Its module cache
	// still maps the revision to that pre-release pseudo-version.
	t.Setenv("GIT_COMMITTER_DATE", "2025-01-02T03:04:05Z")
	s := newSource(t)
	s.git("tag", "v0.5.0")
	out, err := exec.Command("git", "-C", s.dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(string(out))
	pseudo := "v0.0.0-20250102030405-" + hash[:12]
	cache := t.TempDir()
	versions := filepath.Join(cache, "cache", "download", module, "@v")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	info := `{"Version":"` + pseudo + `","Time":"2025-01-02T03:04:05Z","Origin":{"VCS":"git","Hash":"` + hash + `"}}`
	if err := os.WriteFile(filepath.Join(versions, pseudo+".info"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOMODCACHE", cache)

	dir := t.TempDir()
	builder := s.builder()
	if err := builder.Build(ctx, "0.5.0", dir); err != nil {
		t.Fatal(err)
	}
	built, err := buildinfo.ReadFile(filepath.Join(dir, builder.Targets[0]))
	if err != nil {
		t.Fatal(err)
	}
	if got := built.Main.Version; got != "v0.5.0" {
		t.Errorf("built module version = %q, want v0.5.0", got)
	}
}

func TestEveryTargetIsBuiltUnderItsName(t *testing.T) {
	s := newSource(t)
	s.git("tag", "v0.5.0")
	out := t.TempDir()
	if err := s.builder("linux-amd64", "windows-arm64").Build(ctx, "0.5.0", out); err != nil {
		t.Fatal(err)
	}
	for target, magic := range map[string]string{"linux-amd64": "\x7fELF", "windows-arm64": "MZ"} {
		raw, err := os.ReadFile(filepath.Join(out, target))
		if err != nil || !strings.HasPrefix(string(raw), magic) {
			t.Errorf("%s: %v, begins %q", target, err, raw[:min(4, len(raw))])
		}
	}
}

func TestOnlyATagIsARelease(t *testing.T) {
	s := newSource(t)
	s.git("branch", "v0.7.0")
	for _, version := range []string{"0.5.0", "0.7.0"} {
		out := t.TempDir()
		err := s.builder().Build(ctx, version, out)
		if err == nil || !strings.Contains(err.Error(), "v"+version) {
			t.Errorf("%s: err = %v", version, err)
		}
		if left, _ := os.ReadDir(out); len(left) != 0 {
			t.Errorf("%s: the build left %v", version, left)
		}
	}
	// A version is only ever a release version: nothing else reaches git.
	for _, version := range []string{"", "0.5", "0.5.0-dev", "--upload-pack=x", "0.5.0 x", "../0.5.0"} {
		if err := s.builder().Build(ctx, version, t.TempDir()); err == nil || !strings.Contains(err.Error(), "X.Y.Z") {
			t.Errorf("Build(%q): %v", version, err)
		}
	}
}

func TestAProgramThatDoesNotCompileFailsWithWhatTheCompilerSaid(t *testing.T) {
	s := newSource(t)
	s.write("cmd/geminus/main.go", "package main\n\nfunc main() { undefinedThing() }\n")
	s.commit("a mistake")
	s.git("tag", "v0.5.0")
	err := s.builder().Build(ctx, "0.5.0", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "undefinedThing") {
		t.Errorf("err = %v", err)
	}
}

func TestAProgramThatWouldNotTrustItsSignerIsNotReleased(t *testing.T) {
	s := newSource(t)
	// The variable the key is linked into was renamed; the linker says
	// nothing about that.
	s.write("internal/updates/key.go", "package updates\n\nvar trustedKey = \"\"\n\nfunc Key() string { return trustedKey }\n")
	s.commit("the key moved")
	s.git("tag", "v0.5.0")
	err := s.builder().Build(ctx, "0.5.0", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "release key") {
		t.Errorf("err = %v", err)
	}
}

// What is set on the building computer does not reach the build.
func TestSettingsOfTheBuildingComputerDoNotChangeTheBuild(t *testing.T) {
	s := newSource(t)
	s.git("tag", "v0.5.0")
	config := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(config, []byte("[core]\n\thooksPath = /nonexistent\n[filter \"x\"]\n\tsmudge = false\n\trequired = true\n[url \"/nonexistent/\"]\n\tinsteadOf = /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GOFLAGS", "-mod=vendor")
	t.Setenv("GOOS", "plan9")
	t.Setenv("CGO_ENABLED", "1")
	out := t.TempDir()
	if err := s.builder().Build(ctx, "0.5.0", out); err != nil {
		t.Fatal(err)
	}
	host := runtime.GOOS + "-" + runtime.GOARCH
	if got := run(t, filepath.Join(out, host)); got != "geminus 0.5.0 "+keyText {
		t.Errorf("the program says %q", got)
	}
}

func TestABuildNeedsItsSettings(t *testing.T) {
	s := newSource(t)
	s.git("tag", "v0.5.0")
	for name, change := range map[string]func(*Source){
		"no targets":              func(b *Source) { b.Targets = nil },
		"a bad target":            func(b *Source) { b.Targets = []string{"linux/amd64"} },
		"no key":                  func(b *Source) { b.Key = "" },
		"a bad key":               func(b *Source) { b.Key = "abc -X main.x=y" },
		"no source":               func(b *Source) { b.Repo = "" },
		"an option as the source": func(b *Source) { b.Repo = "--upload-pack=touch /tmp/x" },
	} {
		b := s.builder()
		change(&b)
		if err := b.Build(ctx, "0.5.0", t.TempDir()); err == nil {
			t.Errorf("%s: the build went ahead", name)
		}
	}
}

func TestABuildStopsWithItsContext(t *testing.T) {
	s := newSource(t)
	s.git("tag", "v0.5.0")
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.builder().Build(stopped, "0.5.0", t.TempDir()); err == nil {
		t.Error("a build went ahead after its context ended")
	}
}

// The source is fetched into the directory the programs go to and nowhere
// else, and is gone when the build returns, however it ended.
func TestTheSourceIsRemovedAfterTheBuild(t *testing.T) {
	s := newSource(t)
	s.git("tag", "v0.5.0")
	s.write("cmd/geminus/main.go", "package main\n\nfunc main() { undefinedThing() }\n")
	s.commit("a mistake")
	s.git("tag", "v0.6.0")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	for version, programs := range map[string]int{"0.5.0": 1, "0.6.0": 0, "0.7.0": 0} {
		out := t.TempDir()
		err := s.builder().Build(ctx, version, out)
		if (err == nil) != (programs == 1) {
			t.Errorf("%s: err = %v", version, err)
		}
		if left, _ := os.ReadDir(out); len(left) != programs {
			t.Errorf("%s: the build left %v", version, left)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left in the temporary directory: %v", left)
	}
}

func TestAStoppedBuildSaysWhatWasRunning(t *testing.T) {
	s := newSource(t)
	s.git("tag", "v0.5.0")
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	err := s.builder().Build(stopped, "0.5.0", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "git was stopped") || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
