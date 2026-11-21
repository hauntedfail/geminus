// Package build makes the programs of a geminus release from the tag of its
// version in the source repository. It runs on the computer that signs
// releases; nothing but git and the Go toolchain is started, and nothing
// from the source is run.
package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/updates"
)

const (
	module = "github.com/hauntedfail/geminus"
	// Targets is every kind of computer a release has a program for.
	Targets = "darwin-amd64,darwin-arm64,linux-amd64,linux-arm64,windows-amd64,windows-arm64"
	// maxSaid is how much of what a failed command printed is repeated: its
	// end, where it says what is wrong.
	maxSaid = 1500
	// killDelay is how long a command that was told to stop may take.
	killDelay = 5 * time.Second
	// fetchLimit bounds fetching the source: a repository that does not
	// answer must not hold up every other release.
	fetchLimit = 2 * time.Minute
)

// Source says where a release is built from and with what.
type Source struct {
	// Repo is the address of the source, as git takes it.
	Repo string
	// Git and Go are the two programs a build starts.
	Git, Go string
	// Targets are the kinds of computer to build for, each "<os>-<arch>".
	Targets []string
	// Key is the public half of the key the release will be signed with,
	// in hex. It is linked into the programs, so each trusts its signer.
	Key string
}

// Build makes the programs of release version from the tag v<version> and
// leaves them in dir, one file per target, named after it.
func (s Source) Build(ctx context.Context, version, dir string) error {
	if !updates.IsRelease(version) {
		return fmt.Errorf("%q is not a release version: use X.Y.Z", version)
	}
	if err := s.valid(); err != nil {
		return err
	}
	// The source goes beside the programs, so that whoever removes dir after
	// a build that was cut off removes it too. It is gone when Build returns.
	src, err := os.MkdirTemp(dir, ".source-*")
	if err != nil {
		return err
	}
	if err := s.checkout(ctx, version, src); err != nil {
		return errors.Join(err, os.RemoveAll(src))
	}
	if err := s.compile(ctx, version, src, dir); err != nil {
		return errors.Join(err, os.RemoveAll(src))
	}
	return os.RemoveAll(src)
}

// compile builds every target from the source in src into dir.
func (s Source) compile(ctx context.Context, version, src, dir string) error {
	ldflags := fmt.Sprintf("-s -w -X %s/internal/version.Version=%s -X %s/internal/updates.releaseKey=%s", module, version, module, s.Key)
	for _, target := range s.Targets {
		goos, goarch, _ := strings.Cut(target, "-")
		// The source decides nothing about how it is built: no toolchain it
		// names is fetched, and no module comes from the network.
		// Nor does a setting left on this computer: no go env file, no
		// program standing in for the build cache.
		env := []string{"GOOS=" + goos, "GOARCH=" + goarch, "CGO_ENABLED=0", "GOTOOLCHAIN=local",
			"GOFLAGS=-mod=readonly", "GOPROXY=off", "GOWORK=off", "GOENV=off", "GOCACHEPROG="}
		out, err := filepath.Abs(filepath.Join(dir, target))
		if err != nil {
			return err
		}
		if err := s.run(ctx, src, env, s.Go, "build", "-trimpath", "-ldflags", ldflags, "-o", out, "./cmd/geminus"); err != nil {
			return fmt.Errorf("build %s: %w", target, err)
		}
		if err := s.trusts(out); err != nil {
			return fmt.Errorf("build %s: %w", target, err)
		}
	}
	return nil
}

// trusts checks that the program at path holds the release key. The linker
// says nothing when the variable it was told to set is not in the source;
// a program released like that would refuse every later release.
func (s Source) trusts(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Contains(raw, []byte(s.Key)) {
		return errors.New("the program does not hold the release key: the source at this tag has no internal/updates.releaseKey variable for it")
	}
	return nil
}

func (s Source) valid() error {
	switch {
	case s.Repo == "" || strings.HasPrefix(s.Repo, "-"):
		return errors.New("the build has no source repository")
	case s.Git == "" || s.Go == "":
		return errors.New("the build needs git and go")
	case len(s.Targets) == 0:
		return errors.New("the build has no target")
	}
	for _, target := range s.Targets {
		if !updates.ValidTarget(target) {
			return fmt.Errorf("%q is not a target: a target is <os>-<arch>", target)
		}
	}
	if _, err := updates.ParsePublicKey(s.Key); err != nil || strings.TrimSpace(s.Key) != s.Key {
		return errors.New("the build has no release key to link into the programs")
	}
	return nil
}

// checkout puts the source of the tag v<version> into src. Only a tag is a
// release: a branch of the same name is not fetched.
func (s Source) checkout(ctx context.Context, version, src string) error {
	ctx, cancel := context.WithTimeout(ctx, fetchLimit)
	defer cancel()
	ref := "refs/tags/v" + version
	// No prompt may wait for an answer nobody gives, and no git setting of
	// this computer (a filter, a hook directory) applies to the source.
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"}
	steps := [][]string{
		{"init", "--quiet", src},
		{"-C", src, "fetch", "--quiet", "--depth", "1", "--no-tags", "--", s.Repo, ref + ":" + ref},
		{"-C", src, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", ref},
	}
	for _, args := range steps {
		if err := s.run(ctx, "", env, s.Git, args...); err != nil {
			return fmt.Errorf("fetch the tag v%s: %w", version, err)
		}
	}
	return nil
}

// run runs one command and returns what it printed as part of the error.
func (s Source) run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = killDelay
	// go and git start programs of their own; they stop with it.
	stopWithChildren(cmd)
	said, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s was stopped: %w", filepath.Base(name), ctx.Err())
	}
	text := strings.TrimSpace(string(said))
	if len(text) > maxSaid {
		text = "…" + strings.ToValidUTF8(text[len(text)-maxSaid:], "")
	}
	return fmt.Errorf("%s: %w\n%s", filepath.Base(name), err, text)
}
