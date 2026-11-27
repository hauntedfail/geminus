// Command geminus-e2ee-proxy terminates app-layer GE2E v1 encrypted HTTP
// tunnels on loopback and forwards the decrypted raw HTTP requests to Honcho.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hauntedfail/geminus/internal/e2ee"
	"github.com/hauntedfail/geminus/internal/profile"
	"github.com/hauntedfail/geminus/internal/version"
)

const (
	defaultListen   = "127.0.0.1:8653"
	defaultPath     = "/geminise/e2ee"
	defaultUpstream = "http://127.0.0.1:8000"
	headerTimeout   = 10 * time.Second
	readTimeout     = 30 * time.Second
	writeTimeout    = 60 * time.Second
	idleTimeout     = time.Minute
	stopTimeout     = 10 * time.Second
	maxHeaderBytes  = 16 << 10
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

type settings struct {
	listen     string
	path       string
	upstream   string
	secretFile string
	maxSkew    time.Duration
}

func run(ctx context.Context, args []string, out, errw io.Writer) int {
	flags := flag.NewFlagSet("geminus-e2ee-proxy", flag.ContinueOnError)
	flags.SetOutput(errw)
	var s settings
	flags.StringVar(&s.listen, "listen", defaultListen, "address to listen on (loopback recommended)")
	flags.StringVar(&s.path, "path", defaultPath, "HTTP POST path for encrypted GE2E v1 frames")
	flags.StringVar(&s.upstream, "upstream", defaultUpstream, "raw Honcho upstream base URL")
	flags.StringVar(&s.secretFile, "secret-file", "", "optional file containing hex group secret (defaults to Hermes profile geminise.json)")
	flags.DurationVar(&s.maxSkew, "max-skew", e2ee.DefaultMaxSkew, "maximum allowed client clock skew")
	showVer := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVer {
		fmt.Fprintln(out, "geminus-e2ee-proxy", version.Version)
		return 0
	}
	if flags.NArg() > 0 || !strings.HasPrefix(s.path, "/") {
		fmt.Fprintln(errw, "usage: geminus-e2ee-proxy [-listen ADDR] [-path /PATH] [-upstream URL] [-secret-file FILE] [-max-skew DUR]")
		return 2
	}
	log := slog.New(slog.NewTextHandler(errw, nil))
	if err := serve(ctx, s, log); err != nil {
		log.Error("stopped", "err", err)
		return 1
	}
	return 0
}

func loadSecret(secretFile string) ([]byte, error) {
	if secretFile != "" {
		raw, err := os.ReadFile(secretFile)
		if err != nil {
			return nil, fmt.Errorf("read secret file: %w", err)
		}
		sec, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(sec) < e2ee.MinSecretBytes {
			return nil, e2ee.ErrInvalidSecret
		}
		return sec, nil
	}
	prof, err := profile.Load()
	if err != nil {
		return nil, err
	}
	return prof.Secret, nil
}

func serve(ctx context.Context, s settings, log *slog.Logger) error {
	secret, err := loadSecret(s.secretFile)
	if err != nil {
		return err
	}
	proxy, err := e2ee.NewProxy(e2ee.ProxyConfig{
		Secret:   secret,
		Upstream: s.upstream,
		MaxSkew:  s.maxSkew,
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle(s.path, proxy)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})

	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: headerTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", ln.Addr().String(), "path", s.path, "upstream", s.upstream, "version", version.Version)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}
