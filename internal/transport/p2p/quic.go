package p2p

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/hauntedfail/geminus/internal/honcho"
	"github.com/hauntedfail/geminus/internal/transport"
)

// makeSelfSignedCert generates a minimal self-signed X.509 certificate backed
// by priv (or a freshly generated Ed25519 key if priv is nil).
func makeSelfSignedCert(priv ed25519.PrivateKey) (tls.Certificate, ed25519.PublicKey, error) {
	if priv == nil {
		var err error
		_, priv, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return tls.Certificate{}, nil, err
		}
	}
	pub := priv.Public().(ed25519.PublicKey)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "geminise-node"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}, pub, nil
}

func verifyPeerEd25519(expected ed25519.PublicKey) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("p2p: peer presented no certificate")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("p2p: invalid peer certificate: %w", err)
		}
		pub, ok := cert.PublicKey.(ed25519.PublicKey)
		if !ok {
			return errors.New("p2p: peer certificate key is not Ed25519")
		}
		if len(expected) > 0 && !pub.Equal(expected) {
			return errors.New("p2p: peer Ed25519 identity mismatch")
		}
		return nil
	}
}

func defaultQUICConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       60 * time.Second,
		KeepAlivePeriod:      15 * time.Second,
	}
}

// Server serves a transport.Backend (typically *LocalStore) over Obfuscated QUIC.
type Server struct {
	Backend transport.Backend
	tr      *quic.Transport
	ln      *quic.Listener
	obfs    *ObfsPacketConn
	pub     ed25519.PublicKey
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// ServerOptions configures ListenServer.
type ServerOptions struct {
	ListenAddr string             // e.g. "127.0.0.1:0" or ":4433" (ignored if PacketConn is set)
	PacketConn net.PacketConn     // optional pre-opened PacketConn (e.g. RelayPacketConn)
	Secret     []byte             // group secret for XChaCha20-Poly1305 whitening
	Identity   ed25519.PrivateKey // optional static Ed25519 identity key
	PadMax     int                // optional traffic-shaping padding upper bound
	Backend    transport.Backend  // backing store
}

// ListenServer starts an Obfuscated QUIC server backed by opts.Backend.
func ListenServer(opts ServerOptions) (*Server, error) {
	if opts.Backend == nil {
		return nil, errors.New("p2p: server requires a Backend")
	}
	pc := opts.PacketConn
	if pc == nil {
		addr := opts.ListenAddr
		if addr == "" {
			addr = ":0"
		}
		var err error
		pc, err = net.ListenPacket("udp", addr)
		if err != nil {
			return nil, fmt.Errorf("p2p listen udp: %w", err)
		}
	}
	obfs, err := NewObfsPacketConn(pc, opts.Secret, opts.PadMax)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	cert, pub, err := makeSelfSignedCert(opts.Identity)
	if err != nil {
		_ = obfs.Close()
		return nil, err
	}
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{alpnProto},
		MinVersion:   tls.VersionTLS13,
	}
	tr := &quic.Transport{
		Conn:                             obfs,
		DisableVersionNegotiationPackets: true,
	}
	ln, err := tr.Listen(tlsConf, defaultQUICConfig())
	if err != nil {
		_ = tr.Close()
		_ = obfs.Close()
		return nil, fmt.Errorf("p2p quic listen: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		Backend: opts.Backend,
		tr:      tr,
		ln:      ln,
		obfs:    obfs,
		pub:     pub,
		cancel:  cancel,
	}
	s.wg.Add(1)
	go s.serve(ctx)
	return s, nil
}

// Addr returns the local listening address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// PublicKey returns the server's Ed25519 public key.
func (s *Server) PublicKey() ed25519.PublicKey { return s.pub }

// DroppedPackets returns the number of unauthenticated UDP datagrams silently dropped.
func (s *Server) DroppedPackets() uint64 { return s.obfs.Dropped() }

// Close shuts down the listener and underlying UDP socket.
func (s *Server) Close() error {
	s.cancel()
	err1 := s.ln.Close()
	err2 := s.tr.Close()
	err3 := s.obfs.Close()
	s.wg.Wait()
	return errors.Join(err1, err2, err3)
}

func (s *Server) serve(ctx context.Context) {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept(ctx)
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func(c *quic.Conn) {
			defer s.wg.Done()
			s.handleConn(ctx, c)
		}(conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn *quic.Conn) {
	defer conn.CloseWithError(0, "")
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go s.handleStream(ctx, stream)
	}
}

func (s *Server) handleStream(ctx context.Context, stream *quic.Stream) {
	defer stream.Close()
	var req rpcRequest
	if err := readFrame(stream, &req); err != nil {
		return
	}
	resp := s.dispatch(ctx, req)
	_ = writeFrame(stream, resp)
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) rpcResponse {
	errResp := func(err error) rpcResponse {
		if errors.Is(err, honcho.ErrNotFound) {
			return rpcResponse{Error: err.Error(), Code: "not_found"}
		}
		return rpcResponse{Error: err.Error()}
	}
	switch req.Method {
	case "ping":
		return rpcResponse{}
	case "peers":
		peers, err := s.Backend.Peers(ctx)
		if err != nil {
			return errResp(err)
		}
		return rpcResponse{Peers: peers}
	case "set_peer_metadata":
		var val any
		if len(req.Value) > 0 {
			if err := json.Unmarshal(req.Value, &val); err != nil {
				return errResp(err)
			}
		}
		if err := s.Backend.SetPeerMetadata(ctx, req.Peer, req.Key, val); err != nil {
			return errResp(err)
		}
		return rpcResponse{}
	case "sessions":
		sessions, err := s.Backend.Sessions(ctx, req.Filter)
		if err != nil {
			return errResp(err)
		}
		return rpcResponse{Sessions: sessions}
	case "create_session":
		var meta any
		if len(req.Metadata) > 0 {
			if err := json.Unmarshal(req.Metadata, &meta); err != nil {
				return errResp(err)
			}
		}
		if err := s.Backend.CreateSession(ctx, req.Session, meta, req.Peers...); err != nil {
			return errResp(err)
		}
		return rpcResponse{}
	case "set_session_metadata":
		var meta any
		if len(req.Metadata) > 0 {
			if err := json.Unmarshal(req.Metadata, &meta); err != nil {
				return errResp(err)
			}
		}
		if err := s.Backend.SetSessionMetadata(ctx, req.Session, meta); err != nil {
			return errResp(err)
		}
		return rpcResponse{}
	case "messages":
		msgs, err := s.Backend.Messages(ctx, req.Session)
		if err != nil {
			return errResp(err)
		}
		return rpcResponse{Messages: msgs}
	case "latest":
		msgs, err := s.Backend.Latest(ctx, req.Session, req.Limit)
		if err != nil {
			return errResp(err)
		}
		return rpcResponse{Messages: msgs}
	case "post":
		var meta any
		if len(req.Metadata) > 0 {
			if err := json.Unmarshal(req.Metadata, &meta); err != nil {
				return errResp(err)
			}
		}
		msg, err := s.Backend.Post(ctx, req.Session, req.Peer, req.Content, meta)
		if err != nil {
			return errResp(err)
		}
		return rpcResponse{Message: &msg}
	case "ring":
		if err := s.Backend.Ring(ctx, req.Endpoint); err != nil {
			return errResp(err)
		}
		return rpcResponse{}
	default:
		return rpcResponse{Error: "unknown method: " + req.Method}
	}
}

// ClientOptions configures NewClient.
type ClientOptions struct {
	RemoteAddr    net.Addr          // direct UDP addr or RelayAddr
	RemoteAddrStr string            // resolved as UDP addr when RemoteAddr is nil
	PacketConn    net.PacketConn    // optional pre-opened PacketConn (e.g. RelayPacketConn)
	Secret        []byte            // group secret for XChaCha20-Poly1305 whitening
	ServerPubKey  ed25519.PublicKey // optional pinned server Ed25519 public key
	PadMax        int               // optional traffic-shaping padding upper bound
	Label         string            // display label ("direct" or "relay"); defaults to "direct"
}

// Client is a transport.Backend that talks to a remote p2p.Server over Obfuscated QUIC.
type Client struct {
	label   string
	remote  net.Addr
	tlsConf *tls.Config
	obfs    *ObfsPacketConn
	tr      *quic.Transport

	mu   sync.Mutex
	conn *quic.Conn
}

// NewClient prepares an Obfuscated QUIC client for opts.RemoteAddr.
func NewClient(opts ClientOptions) (*Client, error) {
	remote := opts.RemoteAddr
	if remote == nil {
		if opts.RemoteAddrStr == "" {
			return nil, errors.New("p2p: client requires RemoteAddr or RemoteAddrStr")
		}
		udpAddr, err := net.ResolveUDPAddr("udp", opts.RemoteAddrStr)
		if err != nil {
			return nil, fmt.Errorf("p2p resolve remote %q: %w", opts.RemoteAddrStr, err)
		}
		remote = udpAddr
	}
	pc := opts.PacketConn
	if pc == nil {
		var err error
		pc, err = net.ListenPacket("udp", ":0")
		if err != nil {
			return nil, fmt.Errorf("p2p client udp: %w", err)
		}
	}
	obfs, err := NewObfsPacketConn(pc, opts.Secret, opts.PadMax)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	tlsConf := &tls.Config{
		InsecureSkipVerify:    true, // verified via VerifyPeerCertificate Ed25519 check
		VerifyPeerCertificate: verifyPeerEd25519(opts.ServerPubKey),
		NextProtos:            []string{alpnProto},
		MinVersion:            tls.VersionTLS13,
	}
	tr := &quic.Transport{
		Conn: obfs,
	}
	label := opts.Label
	if label == "" {
		label = "direct"
	}
	return &Client{
		label:   label,
		remote:  remote,
		tlsConf: tlsConf,
		obfs:    obfs,
		tr:      tr,
	}, nil
}

// Close closes the active QUIC connection and underlying UDP socket.
func (c *Client) Close() error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	var err0 error
	if conn != nil {
		err0 = conn.CloseWithError(0, "closed")
	}
	err1 := c.tr.Close()
	err2 := c.obfs.Close()
	return errors.Join(err0, err1, err2)
}

func (c *Client) getConn(ctx context.Context) (*quic.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		select {
		case <-c.conn.Context().Done():
			c.conn = nil
		default:
			return c.conn, nil
		}
	}
	conn, err := c.tr.Dial(ctx, c.remote, c.tlsConf, defaultQUICConfig())
	if err != nil {
		return nil, fmt.Errorf("p2p dial %s: %w", c.remote, err)
	}
	c.conn = conn
	return conn, nil
}

func (c *Client) resetConn(failed *quic.Conn) {
	c.mu.Lock()
	if c.conn == failed {
		c.conn = nil
	}
	c.mu.Unlock()
}

func (c *Client) call(ctx context.Context, req rpcRequest) (rpcResponse, error) {
	conn, err := c.getConn(ctx)
	if err != nil {
		return rpcResponse{}, err
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		c.resetConn(conn)
		return rpcResponse{}, fmt.Errorf("p2p open stream: %w", err)
	}
	defer stream.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	if err := writeFrame(stream, req); err != nil {
		c.resetConn(conn)
		return rpcResponse{}, fmt.Errorf("p2p write request: %w", err)
	}
	var resp rpcResponse
	if err := readFrame(stream, &resp); err != nil {
		c.resetConn(conn)
		return rpcResponse{}, fmt.Errorf("p2p read response: %w", err)
	}
	if resp.Error != "" {
		if resp.Code == "not_found" {
			return rpcResponse{}, honcho.ErrNotFound
		}
		return rpcResponse{}, errors.New(resp.Error)
	}
	return resp, nil
}

func (c *Client) Name() string { return c.label }

// Ping verifies live end-to-end QUIC reachability.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.call(ctx, rpcRequest{Method: "ping"})
	return err
}

func (c *Client) Peers(ctx context.Context) ([]honcho.Peer, error) {
	resp, err := c.call(ctx, rpcRequest{Method: "peers"})
	return resp.Peers, err
}

func (c *Client) SetPeerMetadata(ctx context.Context, peer, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, rpcRequest{Method: "set_peer_metadata", Peer: peer, Key: key, Value: raw})
	return err
}

func (c *Client) Sessions(ctx context.Context, filter map[string]any) ([]honcho.Session, error) {
	resp, err := c.call(ctx, rpcRequest{Method: "sessions", Filter: filter})
	return resp.Sessions, err
}

func (c *Client) CreateSession(ctx context.Context, id string, metadata any, peers ...string) error {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, rpcRequest{Method: "create_session", Session: id, Metadata: raw, Peers: peers})
	return err
}

func (c *Client) SetSessionMetadata(ctx context.Context, id string, metadata any) error {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, rpcRequest{Method: "set_session_metadata", Session: id, Metadata: raw})
	return err
}

func (c *Client) Messages(ctx context.Context, session string) ([]honcho.Message, error) {
	resp, err := c.call(ctx, rpcRequest{Method: "messages", Session: session})
	return resp.Messages, err
}

func (c *Client) Latest(ctx context.Context, session string, n int) ([]honcho.Message, error) {
	resp, err := c.call(ctx, rpcRequest{Method: "latest", Session: session, Limit: n})
	return resp.Messages, err
}

func (c *Client) Post(ctx context.Context, session, peer, content string, metadata any) (honcho.Message, error) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return honcho.Message{}, err
	}
	resp, err := c.call(ctx, rpcRequest{Method: "post", Session: session, Peer: peer, Content: content, Metadata: raw})
	if err != nil {
		return honcho.Message{}, err
	}
	if resp.Message == nil {
		return honcho.Message{}, errors.New("p2p: empty post response")
	}
	return *resp.Message, nil
}

func (c *Client) Ring(ctx context.Context, endpoint string) error {
	_, err := c.call(ctx, rpcRequest{Method: "ring", Endpoint: endpoint})
	return err
}
