// Command geminised is the standalone Transport v2 daemon for the geminise
// family. It operates in two modes:
//
//   - relay: stateless rendezvous and blind encrypted UDP relay (for sibyl / VPS)
//   - node:  edge P2P listener backed by a local session store (for chrome / mirai)
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hauntedfail/geminus/internal/profile"
	"github.com/hauntedfail/geminus/internal/transport/p2p"
	"github.com/hauntedfail/geminus/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printUsage()
		return 0
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		fmt.Println("geminised", version.Version)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "relay":
		if err := runRelay(ctx, args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "geminised relay:", err)
			return 1
		}
		return 0
	case "node":
		if err := runNode(ctx, args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "geminised node:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "geminised: unknown subcommand %q\n", args[0])
		printUsage()
		return 2
	}
}

func printUsage() {
	fmt.Print(`geminised — Transport v2 relay and edge node daemon

Usage:
  geminised relay [--listen :4433] [--secret <text>] [--secret-file <path>]
  geminised node  [--listen :4433] [--relay <addr>] [--store <dir>] [--peer <name>] [--key-file <path>] [--lan] [--pad <bytes>]
  geminised version
`)
}

func decodeSecret(s string) []byte {
	s = strings.TrimSpace(s)
	if len(s) >= 64 && len(s)%2 == 0 {
		if b, err := hex.DecodeString(s); err == nil {
			return b
		}
	}
	return []byte(s)
}

func resolveSecret(flagVal, flagFile string) ([]byte, error) {
	if flagVal != "" {
		return decodeSecret(flagVal), nil
	}
	if flagFile != "" {
		data, err := os.ReadFile(flagFile)
		if err != nil {
			return nil, err
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			return nil, errors.New("secret file is empty")
		}
		return decodeSecret(s), nil
	}
	if env := os.Getenv("GEMINISE_SECRET"); env != "" {
		return decodeSecret(env), nil
	}
	prof, err := profile.Load()
	if err != nil {
		return nil, fmt.Errorf("load secret from profile (or pass --secret / GEMINISE_SECRET): %w", err)
	}
	return prof.Secret, nil
}

func loadOrCreateNodeKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		return priv, err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("invalid Ed25519 seed in %s", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}

func runRelay(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	listen := fs.String("listen", ":4433", "UDP listen address")
	secretVal := fs.String("secret", "", "group secret (defaults to GEMINISE_SECRET or Hermes profile)")
	secretFile := fs.String("secret-file", "", "path to file containing group secret")
	if err := fs.Parse(args); err != nil {
		return err
	}
	secret, err := resolveSecret(*secretVal, *secretFile)
	if err != nil {
		return err
	}
	rs, err := p2p.ListenRelay(*listen, secret)
	if err != nil {
		return err
	}
	defer rs.Close()
	fmt.Printf("geminised relay listening on udp://%s (blind encrypted mode, silent drop active)\n", rs.Addr())
	<-ctx.Done()
	fmt.Printf("geminised relay stopping (silently dropped %d unauthenticated packets)\n", rs.DroppedPackets())
	return nil
}

func runNode(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	listen := fs.String("listen", ":4433", "UDP listen address for direct connections")
	relayAddr := fs.String("relay", "", "optional blind relay UDP address (host:port)")
	storeDir := fs.String("store", "", "directory for local session store (empty = in-memory)")
	peerName := fs.String("peer", "", "peer name for LAN discovery beacon")
	keyFile := fs.String("key-file", "", "path to persistent Ed25519 node identity key")
	lan := fs.Bool("lan", false, "broadcast authenticated LAN discovery beacon")
	padMax := fs.Int("pad", 0, "optional random UDP padding upper bound in bytes")
	secretVal := fs.String("secret", "", "group secret (defaults to GEMINISE_SECRET or Hermes profile)")
	secretFile := fs.String("secret-file", "", "path to file containing group secret")
	if err := fs.Parse(args); err != nil {
		return err
	}
	secret, err := resolveSecret(*secretVal, *secretFile)
	if err != nil {
		return err
	}
	priv, err := loadOrCreateNodeKey(*keyFile)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	nodeIDHex := hex.EncodeToString(pub)

	store, err := p2p.NewLocalStore(*storeDir)
	if err != nil {
		return err
	}

	var pc net.PacketConn
	if *relayAddr != "" {
		var id [ed25519.PublicKeySize]byte
		copy(id[:], pub)
		pc, err = p2p.NewRelayPacketConn(*relayAddr, secret, id)
		if err != nil {
			return err
		}
	}

	srv, err := p2p.ListenServer(p2p.ServerOptions{
		ListenAddr: *listen,
		PacketConn: pc,
		Secret:     secret,
		Identity:   priv,
		PadMax:     *padMax,
		Backend:    store,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	if *lan && *relayAddr == "" && *peerName != "" {
		if udpAddr, ok := srv.Addr().(*net.UDPAddr); ok {
			_ = p2p.StartLANBeacon(ctx, secret, *peerName, nodeIDHex, udpAddr.Port, p2p.DefaultDiscoveryPort)
		}
	}

	mode := "direct udp://" + srv.Addr().String()
	if *relayAddr != "" {
		mode = "relay via " + *relayAddr
	}
	fmt.Printf("geminised node online (%s)\nnode_id=%s\n", mode, nodeIDHex)
	<-ctx.Done()
	fmt.Printf("geminised node stopping (silently dropped %d unauthenticated packets)\n", srv.DroppedPackets())
	return nil
}
