package p2p

import (
	"context"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

const (
	wsAuthHeader   = "X-Geminise-Auth"
	wsTsHeader     = "X-Geminise-Ts"
	wsAuthSkewSecs = 120
)

func wsAuthMAC(secret []byte, ts string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("geminise-ws-relay:"))
	mac.Write([]byte(ts))
	return mac.Sum(nil)
}

func verifyWSAuth(secret []byte, r *http.Request) bool {
	tsStr := strings.TrimSpace(r.Header.Get(wsTsHeader))
	sigHex := strings.TrimSpace(r.Header.Get(wsAuthHeader))
	if tsStr == "" || sigHex == "" {
		return false
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}
	diff := time.Now().Unix() - ts
	if diff < -wsAuthSkewSecs || diff > wsAuthSkewSecs {
		return false
	}
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	want := wsAuthMAC(secret, tsStr)
	return hmac.Equal(got, want)
}

func sealWSFrame(aead cipher.AEAD, payload []byte) ([]byte, error) {
	out := make([]byte, obfsNonceSize, obfsNonceSize+len(payload)+aead.Overhead())
	if _, err := rand.Read(out[:obfsNonceSize]); err != nil {
		return nil, err
	}
	return aead.Seal(out, out[:obfsNonceSize], payload, nil), nil
}

func openWSFrame(aead cipher.AEAD, frame []byte) ([]byte, error) {
	if len(frame) < obfsNonceSize+aead.Overhead() {
		return nil, errors.New("p2p ws: frame too short")
	}
	return aead.Open(nil, frame[:obfsNonceSize], frame[obfsNonceSize:], nil)
}

type wsPeerConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	nodeID  [nodeIDSize]byte
	hasID   bool
}

func (c *wsPeerConn) send(frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return websocket.Message.Send(c.ws, frame)
}

// HTTPRelayServer is a blind encrypted WebSocket relay designed to sit behind
// an existing HTTPS reverse proxy or Cloudflare Tunnel on loopback without
// requiring open UDP firewall ports.
type HTTPRelayServer struct {
	ln      net.Listener
	srv     *http.Server
	secret  []byte
	aead    cipher.AEAD
	path    string
	dropped atomic.Uint64

	mu    sync.RWMutex
	peers map[[nodeIDSize]byte]*wsPeerConn
}

// ListenHTTPRelay starts a blind encrypted WebSocket relay on listenAddr at path.
func ListenHTTPRelay(listenAddr, path string, secret []byte) (*HTTPRelayServer, error) {
	if listenAddr == "" {
		listenAddr = "127.0.0.1:0"
	}
	if path == "" {
		path = "/geminise/relay"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	aead, err := deriveObfsAEAD(secret, "ws-relay")
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("p2p http relay listen: %w", err)
	}
	s := &HTTPRelayServer{
		ln:     ln,
		secret: append([]byte(nil), secret...),
		aead:   aead,
		path:   path,
		peers:  map[[nodeIDSize]byte]*wsPeerConn{},
	}
	mux := http.NewServeMux()
	wsServer := websocket.Server{
		Handler:   s.handleWS,
		Handshake: func(_ *websocket.Config, _ *http.Request) error { return nil },
	}
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if !verifyWSAuth(s.secret, r) {
			s.dropped.Add(1)
			http.NotFound(w, r)
			return
		}
		wsServer.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.dropped.Add(1)
		http.NotFound(w, r)
	})
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = s.srv.Serve(ln)
	}()
	return s, nil
}

// Addr returns the listening TCP address.
func (s *HTTPRelayServer) Addr() net.Addr { return s.ln.Addr() }

// Path returns the HTTP WebSocket relay path.
func (s *HTTPRelayServer) Path() string { return s.path }

// DroppedPackets returns the count of rejected HTTP requests or invalid frames.
func (s *HTTPRelayServer) DroppedPackets() uint64 { return s.dropped.Load() }

// Close gracefully stops the HTTP relay server.
func (s *HTTPRelayServer) Close() error {
	s.mu.Lock()
	for id, p := range s.peers {
		_ = p.ws.Close()
		delete(s.peers, id)
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

func (s *HTTPRelayServer) handleWS(ws *websocket.Conn) {
	ws.PayloadType = websocket.BinaryFrame
	peer := &wsPeerConn{ws: ws}
	defer func() {
		if peer.hasID {
			s.mu.Lock()
			if cur, ok := s.peers[peer.nodeID]; ok && cur == peer {
				delete(s.peers, peer.nodeID)
			}
			s.mu.Unlock()
		}
		_ = ws.Close()
	}()

	for {
		var raw []byte
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			return
		}
		pkt, err := openWSFrame(s.aead, raw)
		if err != nil || len(pkt) < 1 {
			s.dropped.Add(1)
			continue
		}
		switch pkt[0] {
		case relayOpRegister:
			if len(pkt) < 1+nodeIDSize {
				s.dropped.Add(1)
				continue
			}
			var id [nodeIDSize]byte
			copy(id[:], pkt[1:1+nodeIDSize])
			s.mu.Lock()
			if peer.hasID && peer.nodeID != id {
				if cur, ok := s.peers[peer.nodeID]; ok && cur == peer {
					delete(s.peers, peer.nodeID)
				}
			}
			peer.nodeID = id
			peer.hasID = true
			s.peers[id] = peer
			s.mu.Unlock()

		case relayOpForward:
			if !peer.hasID || len(pkt) <= 1+nodeIDSize {
				s.dropped.Add(1)
				continue
			}
			var targetID [nodeIDSize]byte
			copy(targetID[:], pkt[1:1+nodeIDSize])
			payload := pkt[1+nodeIDSize:]

			s.mu.RLock()
			dst, ok := s.peers[targetID]
			s.mu.RUnlock()
			if !ok {
				continue
			}
			out := make([]byte, 1+nodeIDSize+len(payload))
			out[0] = relayOpDeliver
			copy(out[1:1+nodeIDSize], peer.nodeID[:])
			copy(out[1+nodeIDSize:], payload)
			frame, err := sealWSFrame(s.aead, out)
			if err != nil {
				continue
			}
			_ = dst.send(frame)
		default:
			s.dropped.Add(1)
		}
	}
}

type wsInboundPacket struct {
	src     [nodeIDSize]byte
	payload []byte
}

// WSRelayPacketConn implements net.PacketConn over a blind encrypted WebSocket
// relay so QUIC/E2E traffic can traverse HTTPS/443 (e.g. Cloudflare Tunnel)
// without opening UDP ports.
type WSRelayPacketConn struct {
	ws       *websocket.Conn
	aead     cipher.AEAD
	selfID   [nodeIDSize]byte
	writeMu  sync.Mutex
	recvCh   chan wsInboundPacket
	closedCh chan struct{}
	closeOnce sync.Once

	deadlineMu   sync.RWMutex
	readDeadline time.Time
	deadlineWake chan struct{}
}

// DialRelayPacketConn connects to either an HTTPS/WebSocket blind relay
// (when relayAddr starts with http://, https://, ws://, or wss://) or a UDP
// blind relay + hole-punching rendezvous server (host:port).
func DialRelayPacketConn(relayAddrStr string, secret []byte, selfID [nodeIDSize]byte) (net.PacketConn, error) {
	if isWSRelayURL(relayAddrStr) {
		return NewWSRelayPacketConn(relayAddrStr, secret, selfID)
	}
	return NewRelayPacketConn(relayAddrStr, secret, selfID)
}

func isWSRelayURL(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(lower, "ws://") ||
		strings.HasPrefix(lower, "wss://") ||
		strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://")
}

func normalizeWSURL(raw string) (wsURL, origin string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
		origin = "http://" + u.Host
	case "https":
		u.Scheme = "wss"
		origin = "https://" + u.Host
	case "ws":
		origin = "http://" + u.Host
	case "wss":
		origin = "https://" + u.Host
	default:
		return "", "", fmt.Errorf("p2p ws relay: unsupported scheme %q", u.Scheme)
	}
	if u.Path == "" {
		u.Path = "/geminise/relay"
	}
	return u.String(), origin, nil
}

// NewWSRelayPacketConn dials a blind encrypted WebSocket relay and registers selfID.
func NewWSRelayPacketConn(rawURL string, secret []byte, selfID [nodeIDSize]byte) (*WSRelayPacketConn, error) {
	wsURL, origin, err := normalizeWSURL(rawURL)
	if err != nil {
		return nil, err
	}
	aead, err := deriveObfsAEAD(secret, "ws-relay")
	if err != nil {
		return nil, err
	}
	cfg, err := websocket.NewConfig(wsURL, origin)
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	cfg.Header.Set(wsTsHeader, ts)
	cfg.Header.Set(wsAuthHeader, hex.EncodeToString(wsAuthMAC(secret, ts)))

	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("p2p dial ws relay %s: %w", wsURL, err)
	}
	ws.PayloadType = websocket.BinaryFrame

	rc := &WSRelayPacketConn{
		ws:           ws,
		aead:         aead,
		selfID:       selfID,
		recvCh:       make(chan wsInboundPacket, 256),
		closedCh:     make(chan struct{}),
		deadlineWake: make(chan struct{}),
	}
	if err := rc.register(); err != nil {
		_ = ws.Close()
		return nil, err
	}
	go rc.readLoop()
	go rc.keepalive()
	return rc, nil
}

func (rc *WSRelayPacketConn) register() error {
	pkt := make([]byte, 1+nodeIDSize)
	pkt[0] = relayOpRegister
	copy(pkt[1:], rc.selfID[:])
	frame, err := sealWSFrame(rc.aead, pkt)
	if err != nil {
		return err
	}
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	return websocket.Message.Send(rc.ws, frame)
}

func (rc *WSRelayPacketConn) keepalive() {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-rc.closedCh:
			return
		case <-t.C:
			if err := rc.register(); err != nil {
				_ = rc.Close()
				return
			}
		}
	}
}

func (rc *WSRelayPacketConn) readLoop() {
	defer rc.Close()
	for {
		var raw []byte
		if err := websocket.Message.Receive(rc.ws, &raw); err != nil {
			return
		}
		pkt, err := openWSFrame(rc.aead, raw)
		if err != nil || len(pkt) <= 1+nodeIDSize || pkt[0] != relayOpDeliver {
			continue
		}
		var src [nodeIDSize]byte
		copy(src[:], pkt[1:1+nodeIDSize])
		payload := append([]byte(nil), pkt[1+nodeIDSize:]...)
		select {
		case rc.recvCh <- wsInboundPacket{src: src, payload: payload}:
		case <-rc.closedCh:
			return
		default:
			// Drop oldest-style overflow instead of blocking reader loop.
		}
	}
}

func (rc *WSRelayPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		rc.deadlineMu.RLock()
		dl := rc.readDeadline
		wake := rc.deadlineWake
		rc.deadlineMu.RUnlock()

		var timer *time.Timer
		var timeoutCh <-chan time.Time
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, nil, &net.OpError{Op: "read", Net: "geminise-relay", Err: wsTimeoutErr{}}
			}
			timer = time.NewTimer(d)
			timeoutCh = timer.C
		}

		select {
		case pkt := <-rc.recvCh:
			if timer != nil {
				timer.Stop()
			}
			n := copy(p, pkt.payload)
			return n, RelayAddr{NodeID: pkt.src}, nil
		case <-timeoutCh:
			return 0, nil, &net.OpError{Op: "read", Net: "geminise-relay", Err: wsTimeoutErr{}}
		case <-wake:
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-rc.closedCh:
			if timer != nil {
				timer.Stop()
			}
			return 0, nil, net.ErrClosed
		}
	}
}

func (rc *WSRelayPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	ra, ok := addr.(RelayAddr)
	if !ok {
		return 0, errors.New("p2p: WSRelayPacketConn requires RelayAddr destination")
	}
	select {
	case <-rc.closedCh:
		return 0, net.ErrClosed
	default:
	}
	pkt := make([]byte, 1+nodeIDSize+len(p))
	pkt[0] = relayOpForward
	copy(pkt[1:1+nodeIDSize], ra.NodeID[:])
	copy(pkt[1+nodeIDSize:], p)
	frame, err := sealWSFrame(rc.aead, pkt)
	if err != nil {
		return 0, err
	}
	rc.writeMu.Lock()
	err = websocket.Message.Send(rc.ws, frame)
	rc.writeMu.Unlock()
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (rc *WSRelayPacketConn) Close() error {
	var err error
	rc.closeOnce.Do(func() {
		close(rc.closedCh)
		err = rc.ws.Close()
	})
	return err
}

func (rc *WSRelayPacketConn) LocalAddr() net.Addr { return RelayAddr{NodeID: rc.selfID} }

func (rc *WSRelayPacketConn) SetDeadline(t time.Time) error {
	_ = rc.SetReadDeadline(t)
	return rc.SetWriteDeadline(t)
}

func (rc *WSRelayPacketConn) SetReadDeadline(t time.Time) error {
	rc.deadlineMu.Lock()
	rc.readDeadline = t
	close(rc.deadlineWake)
	rc.deadlineWake = make(chan struct{})
	rc.deadlineMu.Unlock()
	return nil
}

func (rc *WSRelayPacketConn) SetWriteDeadline(t time.Time) error {
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	return rc.ws.SetWriteDeadline(t)
}

type wsTimeoutErr struct{}

func (wsTimeoutErr) Error() string   { return "i/o timeout" }
func (wsTimeoutErr) Timeout() bool   { return true }
func (wsTimeoutErr) Temporary() bool { return true }
