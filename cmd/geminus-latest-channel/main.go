// Command geminus-latest-channel hands out geminus releases. It listens on
// this computer only; the tunnel in front of it gives it its public address,
// https://sibyl.haunted.fail/geminus/updates. See internal/updates/server.
package main

import (
	"context"
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
	"syscall"
	"time"

	"github.com/hauntedfail/geminus/internal/profile"
	"github.com/hauntedfail/geminus/internal/updates/server"
	"github.com/hauntedfail/geminus/internal/version"
)

const (
	defaultListen = "127.0.0.1:8651"
	// An upload is at most 32 MiB and may cross a slow link.
	transferTimeout = 5 * time.Minute
	headerTimeout   = 10 * time.Second
	idleTimeout     = time.Minute
	stopTimeout     = 10 * time.Second
	maxHeaderBytes  = 16 << 10
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

func defaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "geminus", "updates")
}

func run(ctx context.Context, args []string, errw io.Writer) int {
	flags := flag.NewFlagSet("geminus-latest-channel", flag.ContinueOnError)
	flags.SetOutput(errw)
	listen := flags.String("listen", defaultListen, "address to listen on")
	dir := flags.String("dir", defaultDir(), "directory the releases are kept in")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || *dir == "" {
		fmt.Fprintln(errw, "usage: geminus-latest-channel [-listen ADDRESS] [-dir DIRECTORY]")
		return 2
	}
	log := slog.New(slog.NewTextHandler(errw, nil))
	if err := serve(ctx, *listen, *dir, log); err != nil {
		log.Error("stopped", "err", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, listen, dir string, log *slog.Logger) error {
	prof, err := profile.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           server.New(root, prof.Secret, time.Now, log),
		ReadHeaderTimeout: headerTimeout,
		ReadTimeout:       transferTimeout,
		WriteTimeout:      transferTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	log.Info("listening", "address", ln.Addr().String(), "dir", dir, "version", version.Version)
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
