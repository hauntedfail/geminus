// Command geminus-latest-channel builds, signs and hands out geminus
// releases. It listens on this computer only; the tunnel in front of it
// gives it its public address, https://sibyl.haunted.fail/geminus/updates.
// See internal/updates/server.
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hauntedfail/geminus/internal/profile"
	"github.com/hauntedfail/geminus/internal/updates"
	"github.com/hauntedfail/geminus/internal/updates/build"
	"github.com/hauntedfail/geminus/internal/updates/server"
	"github.com/hauntedfail/geminus/internal/version"
)

const (
	defaultListen = "127.0.0.1:8651"
	defaultRepo   = "git@github.com:hauntedfail/geminus.git"
	// A program is at most 32 MiB and may cross a slow link.
	transferTimeout = 5 * time.Minute
	headerTimeout   = 10 * time.Second
	idleTimeout     = time.Minute
	stopTimeout     = 10 * time.Second
	maxHeaderBytes  = 16 << 10
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// inHome is a path below the home directory, or "" if there is none.
func inHome(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home}, parts...)...)
}

// settings is what the command line asked for.
type settings struct {
	listen, dir, key, repo, goBin, gitBin string
	targets                               []string
}

func run(ctx context.Context, args []string, out, errw io.Writer) int {
	flags := flag.NewFlagSet("geminus-latest-channel", flag.ContinueOnError)
	flags.SetOutput(errw)
	var s settings
	flags.StringVar(&s.listen, "listen", defaultListen, "address to listen on")
	flags.StringVar(&s.dir, "dir", inHome(".local", "share", "geminus", "updates"), "directory the releases are kept in")
	flags.StringVar(&s.key, "key", inHome(".config", "geminus", "release.key"), "file of the key that signs releases")
	flags.StringVar(&s.repo, "repo", defaultRepo, "source repository a release is built from, at the tag vX.Y.Z")
	flags.StringVar(&s.goBin, "go", "go", "the go program a release is built with")
	flags.StringVar(&s.gitBin, "git", "git", "the git program the source is fetched with")
	targets := flags.String("targets", build.Targets, "targets a release is built for, separated by commas")
	keygen := flags.Bool("keygen", false, "make the key that signs releases if there is none, print its public half, and stop")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || s.dir == "" || s.key == "" {
		fmt.Fprintln(errw, "usage: geminus-latest-channel [-listen ADDRESS] [-dir DIRECTORY] [-key FILE] [-repo URL] [-go PROGRAM] [-git PROGRAM] [-targets LIST] [-keygen]")
		return 2
	}
	s.targets = strings.Split(*targets, ",")
	if *keygen {
		return makeKey(s.key, out, errw)
	}
	log := slog.New(slog.NewTextHandler(errw, nil))
	if err := serve(ctx, s, log); err != nil {
		log.Error("stopped", "err", err)
		return 1
	}
	return 0
}

// makeKey prints the public half of the key at path, making the key first
// if there is none. The public half is what internal/updates/key.go holds.
func makeKey(path string, out, errw io.Writer) int {
	load := server.LoadKey
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		load = server.CreateKey
	}
	key, err := load(path)
	if err != nil {
		fmt.Fprintln(errw, "geminus-latest-channel:", err)
		return 1
	}
	fmt.Fprintln(out, updates.PublicKeyText(key.Public().(ed25519.PublicKey)))
	return 0
}

func serve(ctx context.Context, s settings, log *slog.Logger) error {
	prof, err := profile.Load()
	if err != nil {
		return err
	}
	key, err := server.LoadKey(s.key)
	if err != nil {
		return err
	}
	public := updates.PublicKeyText(key.Public().(ed25519.PublicKey))
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return err
	}
	source := build.Source{Repo: s.repo, Git: s.gitBin, Go: s.goBin, Targets: s.targets, Key: public}
	channel := server.New(server.Config{Root: root, Secret: prof.Secret, Key: key, Build: source.Build, Now: time.Now, Log: log})
	// A build in progress is stopped with the server; the release is asked
	// for again.
	defer channel.Close()
	srv := &http.Server{
		Handler:           channel,
		ReadHeaderTimeout: headerTimeout,
		ReadTimeout:       transferTimeout,
		WriteTimeout:      transferTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	log.Info("listening", "address", ln.Addr().String(), "dir", s.dir, "repo", s.repo, "key", public, "version", version.Version)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if err := srv.Shutdown(stopCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
