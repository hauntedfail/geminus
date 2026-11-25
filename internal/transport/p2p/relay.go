package p2p

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Blind UDP rendezvous and relay packet opcodes.
// All packets on the wire are wrapped by ObfsPacketConn so scanners get silent drop.
const (
	relayOpRegister   byte = 0x01 // [op][32-byte src NodeID]
	relayOpRegistered byte = 0x02 // [op][observed addr string]
	relayOpLookup     byte = 0x03 // [op][32-byte target NodeID]
	relayOpPeerAddr   byte = 0x04 // [op][32-byte target NodeID][observed addr string]
	relayOpForward    byte = 0x05 // [op][32-byte target NodeID][opaque payload]
	relayOpDeliver    byte = 0x06 // [op][32-byte src NodeID][opaque payload]
	relayOpPunch      byte = 0x07 // [op][32-byte src NodeID] (direct UDP hole-punch)
	relayOpPunchAck   byte = 0x08 // [op][32-byte src NodeID] (direct UDP hole-punch ack)
	relayOpDirect     byte = 0x09 // [op][32-byte src NodeID][opaque payload] (direct P2P data)

	nodeIDSize = ed25519.PublicKeySize // 32 bytes
)

// RelayAddr is a net.Addr identifying a remote peer reached through a blind relay.
type RelayAddr struct {
	NodeID [nodeIDSize]byte
}

func (a RelayAddr) Network() string { return "geminise-relay" }
func (a RelayAddr) String() string  { return hex.EncodeToString(a.NodeID[:]) }

// ParseNodeID decodes a 64-character hex NodeID into a 32-byte array.
func ParseNodeID(s string) ([nodeIDSize]byte, error) {
	var id [nodeIDSize]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != nodeIDSize {
		return id, fmt.Errorf("p2p: invalid node ID %q", s)
	}
	copy(id[:], b)
	return id, nil
}

type registeredNode struct {
	addr     net.Addr
	lastSeen time.Time
}

// RelayServer is a stateless-payload rendezvous and blind UDP relay.
// It only holds ephemeral NodeID -> UDP address mappings in memory and never
// decrypts the inner QUIC/wire payloads forwarded between peers.
type RelayServer struct {
	conn   *ObfsPacketConn
	ttl    time.Duration
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.RWMutex
	byID   map[[nodeIDSize]byte]registeredNode
	byAddr map[string][nodeIDSize]byte
}

// ListenRelay starts a blind UDP relay server listening on listenAddr.
func ListenRelay(listenAddr string, secret []byte) (*RelayServer, error) {
	if listenAddr == "" {
		listenAddr = ":0"
	}
	pc, err := net.ListenPacket("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("p2p relay listen: %w", err)
	}
	obfs, err := NewObfsPacketConn(pc, secret, 0)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	rs := &RelayServer{
		conn:   obfs,
		ttl:    5 * time.Minute,
		cancel: cancel,
		byID:   map[[nodeIDSize]byte]registeredNode{},
		byAddr: map[string][nodeIDSize]byte{},
	}
	rs.wg.Add(1)
	go rs.loop(ctx)
	return rs, nil
}

// Addr returns the relay's listening UDP address.
func (r *RelayServer) Addr() net.Addr { return r.conn.LocalAddr() }

// DroppedPackets returns the number of unauthenticated packets silently dropped.
func (r *RelayServer) DroppedPackets() uint64 { return r.conn.Dropped() }

// Close stops the relay server.
func (r *RelayServer) Close() error {
	r.cancel()
	err := r.conn.Close()
	r.wg.Wait()
	return err
}

func (r *RelayServer) loop(ctx context.Context) {
	defer r.wg.Done()
	buf := make([]byte, maxWireDatagram)
	for {
		n, addr, err := r.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		if n < 1 {
			continue
		}
		r.handlePacket(buf[:n], addr)
	}
}

func (r *RelayServer) handlePacket(pkt []byte, src net.Addr) {
	now := time.Now()
	switch pkt[0] {
	case relayOpRegister:
		if len(pkt) < 1+nodeIDSize {
			return
		}
		var id [nodeIDSize]byte
		copy(id[:], pkt[1:1+nodeIDSize])
		r.mu.Lock()
		r.byID[id] = registeredNode{addr: src, lastSeen: now}
		r.byAddr[src.String()] = id
		r.mu.Unlock()
		resp := make([]byte, 1+len(src.String()))
		resp[0] = relayOpRegistered
		copy(resp[1:], src.String())
		_, _ = r.conn.WriteTo(resp, src)

	case relayOpLookup:
		if len(pkt) < 1+nodeIDSize {
			return
		}
		var target [nodeIDSize]byte
		copy(target[:], pkt[1:1+nodeIDSize])
		r.mu.RLock()
		node, ok := r.byID[target]
		r.mu.RUnlock()
		if !ok || now.Sub(node.lastSeen) > r.ttl {
			return
		}
		addrStr := node.addr.String()
		resp := make([]byte, 1+nodeIDSize+len(addrStr))
		resp[0] = relayOpPeerAddr
		copy(resp[1:1+nodeIDSize], target[:])
		copy(resp[1+nodeIDSize:], addrStr)
		_, _ = r.conn.WriteTo(resp, src)

	case relayOpForward:
		if len(pkt) <= 1+nodeIDSize {
			return
		}
		var target [nodeIDSize]byte
		copy(target[:], pkt[1:1+nodeIDSize])
		r.mu.RLock()
		srcID, hasSrc := r.byAddr[src.String()]
		dstNode, hasDst := r.byID[target]
		r.mu.RUnlock()
		if !hasSrc || !hasDst || now.Sub(dstNode.lastSeen) > r.ttl {
			return
		}
		payload := pkt[1+nodeIDSize:]
		out := make([]byte, 1+nodeIDSize+len(payload))
		out[0] = relayOpDeliver
		copy(out[1:1+nodeIDSize], srcID[:])
		copy(out[1+nodeIDSize:], payload)
		_, _ = r.conn.WriteTo(out, dstNode.addr)
	}
}

type directRoute struct {
	addr     net.Addr
	lastSeen time.Time
}

// RelayPacketConn is a net.PacketConn that tunnels datagrams through a
// RelayServer using RelayAddr endpoints and automatically upgrades to direct
// UDP hole-punching when peers can reach each other directly.
type RelayPacketConn struct {
	conn      *ObfsPacketConn
	relayAddr net.Addr
	selfID    [nodeIDSize]byte

	mu           sync.RWMutex
	observedAddr string
	peerAddrs    map[[nodeIDSize]byte]string
	directRoutes map[[nodeIDSize]byte]directRoute
	lastLookup   map[[nodeIDSize]byte]time.Time
	closed       chan struct{}
	closeOnce    sync.Once
}

// NewRelayPacketConn binds a local UDP socket, registers selfID with relayAddr,
// and keeps the NAT mapping alive in the background.
func NewRelayPacketConn(relayAddrStr string, secret []byte, selfID [nodeIDSize]byte) (*RelayPacketConn, error) {
	relayAddr, err := net.ResolveUDPAddr("udp", relayAddrStr)
	if err != nil {
		return nil, fmt.Errorf("p2p resolve relay %q: %w", relayAddrStr, err)
	}
	pc, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return nil, fmt.Errorf("p2p relay client udp: %w", err)
	}
	obfs, err := NewObfsPacketConn(pc, secret, 0)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	rc := &RelayPacketConn{
		conn:         obfs,
		relayAddr:    relayAddr,
		selfID:       selfID,
		peerAddrs:    map[[nodeIDSize]byte]string{},
		directRoutes: map[[nodeIDSize]byte]directRoute{},
		lastLookup:   map[[nodeIDSize]byte]time.Time{},
		closed:       make(chan struct{}),
	}
	if err := rc.register(); err != nil {
		_ = obfs.Close()
		return nil, err
	}
	go rc.keepalive()
	return rc, nil
}

// HasDirectRoute reports whether target currently has a confirmed direct UDP path.
func (rc *RelayPacketConn) HasDirectRoute(target [nodeIDSize]byte) bool {
	rc.mu.RLock()
	dr, ok := rc.directRoutes[target]
	rc.mu.RUnlock()
	return ok && time.Since(dr.lastSeen) < 30*time.Second
}

func (rc *RelayPacketConn) sendPunch(op byte, dst net.Addr) {
	pkt := make([]byte, 1+nodeIDSize)
	pkt[0] = op
	copy(pkt[1:], rc.selfID[:])
	_, _ = rc.conn.WriteTo(pkt, dst)
}

func (rc *RelayPacketConn) register() error {
	pkt := make([]byte, 1+nodeIDSize)
	pkt[0] = relayOpRegister
	copy(pkt[1:], rc.selfID[:])
	_, err := rc.conn.WriteTo(pkt, rc.relayAddr)
	return err
}

// Lookup asks the relay for the last-observed external UDP address of target
// (used for direct NAT hole-punching).
func (rc *RelayPacketConn) Lookup(target [nodeIDSize]byte) error {
	pkt := make([]byte, 1+nodeIDSize)
	pkt[0] = relayOpLookup
	copy(pkt[1:], target[:])
	_, err := rc.conn.WriteTo(pkt, rc.relayAddr)
	return err
}

// ObservedAddr returns the external UDP address reported by the relay, if any.
func (rc *RelayPacketConn) ObservedAddr() string {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.observedAddr
}

func (rc *RelayPacketConn) keepalive() {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-rc.closed:
			return
		case <-t.C:
			_ = rc.register()
		}
	}
}

func (rc *RelayPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	buf := make([]byte, len(p)+1+nodeIDSize+64)
	if len(buf) > maxWireDatagram {
		buf = make([]byte, maxWireDatagram)
	}
	for {
		n, srcAddr, err := rc.conn.ReadFrom(buf)
		if err != nil {
			return 0, nil, err
		}
		if n < 1 {
			continue
		}
		switch buf[0] {
		case relayOpRegistered:
			rc.mu.Lock()
			rc.observedAddr = string(buf[1:n])
			rc.mu.Unlock()
		case relayOpPeerAddr:
			if n > 1+nodeIDSize {
				var id [nodeIDSize]byte
				copy(id[:], buf[1:1+nodeIDSize])
				addrStr := string(buf[1+nodeIDSize : n])
				rc.mu.Lock()
				rc.peerAddrs[id] = addrStr
				rc.mu.Unlock()
				if udpAddr, err := net.ResolveUDPAddr("udp", addrStr); err == nil {
					rc.sendPunch(relayOpPunch, udpAddr)
				}
			}
		case relayOpPunch:
			if n >= 1+nodeIDSize {
				var srcID [nodeIDSize]byte
				copy(srcID[:], buf[1:1+nodeIDSize])
				rc.mu.Lock()
				rc.directRoutes[srcID] = directRoute{addr: srcAddr, lastSeen: time.Now()}
				rc.mu.Unlock()
				rc.sendPunch(relayOpPunchAck, srcAddr)
			}
		case relayOpPunchAck:
			if n >= 1+nodeIDSize {
				var srcID [nodeIDSize]byte
				copy(srcID[:], buf[1:1+nodeIDSize])
				rc.mu.Lock()
				rc.directRoutes[srcID] = directRoute{addr: srcAddr, lastSeen: time.Now()}
				rc.mu.Unlock()
			}
		case relayOpDirect:
			if n <= 1+nodeIDSize {
				continue
			}
			var srcID [nodeIDSize]byte
			copy(srcID[:], buf[1:1+nodeIDSize])
			rc.mu.Lock()
			rc.directRoutes[srcID] = directRoute{addr: srcAddr, lastSeen: time.Now()}
			rc.mu.Unlock()
			payload := buf[1+nodeIDSize : n]
			if len(payload) > len(p) {
				continue
			}
			copied := copy(p, payload)
			return copied, RelayAddr{NodeID: srcID}, nil
		case relayOpDeliver:
			if n <= 1+nodeIDSize {
				continue
			}
			var srcID [nodeIDSize]byte
			copy(srcID[:], buf[1:1+nodeIDSize])
			payload := buf[1+nodeIDSize : n]
			if len(payload) > len(p) {
				continue
			}
			copied := copy(p, payload)
			return copied, RelayAddr{NodeID: srcID}, nil
		}
	}
}

func (rc *RelayPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	ra, ok := addr.(RelayAddr)
	if !ok {
		return 0, errors.New("p2p: RelayPacketConn requires RelayAddr destination")
	}
	rc.mu.Lock()
	dr, hasDirect := rc.directRoutes[ra.NodeID]
	directValid := hasDirect && time.Since(dr.lastSeen) < 30*time.Second
	needLookup := !directValid && time.Since(rc.lastLookup[ra.NodeID]) > 5*time.Second
	if needLookup {
		rc.lastLookup[ra.NodeID] = time.Now()
	}
	rc.mu.Unlock()

	if directValid {
		pkt := make([]byte, 1+nodeIDSize+len(p))
		pkt[0] = relayOpDirect
		copy(pkt[1:1+nodeIDSize], rc.selfID[:])
		copy(pkt[1+nodeIDSize:], p)
		if _, err := rc.conn.WriteTo(pkt, dr.addr); err == nil {
			return len(p), nil
		}
	}

	if needLookup {
		_ = rc.Lookup(ra.NodeID)
	}

	pkt := make([]byte, 1+nodeIDSize+len(p))
	pkt[0] = relayOpForward
	copy(pkt[1:1+nodeIDSize], ra.NodeID[:])
	copy(pkt[1+nodeIDSize:], p)
	if _, err := rc.conn.WriteTo(pkt, rc.relayAddr); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (rc *RelayPacketConn) Close() error {
	rc.closeOnce.Do(func() { close(rc.closed) })
	return rc.conn.Close()
}
func (rc *RelayPacketConn) LocalAddr() net.Addr                { return RelayAddr{NodeID: rc.selfID} }
func (rc *RelayPacketConn) SetDeadline(t time.Time) error      { return rc.conn.SetDeadline(t) }
func (rc *RelayPacketConn) SetReadDeadline(t time.Time) error  { return rc.conn.SetReadDeadline(t) }
func (rc *RelayPacketConn) SetWriteDeadline(t time.Time) error { return rc.conn.SetWriteDeadline(t) }
