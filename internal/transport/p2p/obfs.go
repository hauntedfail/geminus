// Package p2p implements geminise Transport v2: obfuscated QUIC over UDP with
// group-keyed XChaCha20-Poly1305 whitening, silent drop on unauthenticated
// packets, local bridge session persistence, LAN discovery, and blind UDP relay.
package p2p

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	obfsNonceSize  = chacha20poly1305.NonceSizeX // 24 bytes
	obfsOverhead   = obfsNonceSize + chacha20poly1305.Overhead + 2
	maxWireDatagram = 65535
)

// deriveObfsAEAD derives an XChaCha20-Poly1305 AEAD from the group secret using
// HKDF-SHA256 with a domain-separated salt and info label.
func deriveObfsAEAD(secret []byte, info string) (cipher.AEAD, error) {
	if len(secret) == 0 {
		return nil, errors.New("p2p: empty group secret")
	}
	r := hkdf.New(sha256.New, secret, []byte("geminise-v2-obfs"), []byte(info))
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("p2p: derive obfs key: %w", err)
	}
	return chacha20poly1305.NewX(key)
}

// ObfsPacketConn wraps a net.PacketConn so every UDP datagram is encrypted and
// authenticated with XChaCha20-Poly1305 using a random 24-byte nonce.
//
// Wire layout: [24-byte random nonce][AEAD(uint16_be(len(payload)) || payload || padding)]
//
// Unauthenticated, truncated, or tampered packets are silently dropped without
// returning an error or sending any bytes back to the sender.
type ObfsPacketConn struct {
	inner   net.PacketConn
	aead    cipher.AEAD
	padMax  int // optional upper bound for random padding (0 disables padding)
	dropped atomic.Uint64
}

// NewObfsPacketConn wraps inner with group-keyed whitening and silent drop.
// If padMax > 0, outgoing packets shorter than padMax are padded with random
// bytes up to a random length in [len, padMax] (capped to avoid exceeding MTU).
func NewObfsPacketConn(inner net.PacketConn, secret []byte, padMax int) (*ObfsPacketConn, error) {
	aead, err := deriveObfsAEAD(secret, "udp-envelope")
	if err != nil {
		return nil, err
	}
	if padMax > 1180 {
		padMax = 1180
	}
	return &ObfsPacketConn{
		inner:  inner,
		aead:   aead,
		padMax: padMax,
	}, nil
}

// Dropped returns the number of unauthenticated/malformed datagrams silently
// discarded by ReadFrom.
func (c *ObfsPacketConn) Dropped() uint64 {
	return c.dropped.Load()
}

// SealPacket encrypts and authenticates a single datagram.
func (c *ObfsPacketConn) SealPacket(payload []byte) ([]byte, error) {
	if len(payload) > 0xffff {
		return nil, errors.New("p2p: datagram too large")
	}
	padLen := 0
	if c.padMax > len(payload) {
		diff := c.padMax - len(payload)
		var b [2]byte
		if _, err := rand.Read(b[:]); err == nil {
			padLen = int(binary.BigEndian.Uint16(b[:])) % (diff + 1)
		}
	}
	plain := make([]byte, 2+len(payload)+padLen)
	binary.BigEndian.PutUint16(plain[:2], uint16(len(payload)))
	copy(plain[2:], payload)
	if padLen > 0 {
		_, _ = rand.Read(plain[2+len(payload):])
	}

	out := make([]byte, obfsNonceSize+len(plain)+c.aead.Overhead())
	if _, err := io.ReadFull(rand.Reader, out[:obfsNonceSize]); err != nil {
		return nil, err
	}
	sealed := c.aead.Seal(out[:obfsNonceSize], out[:obfsNonceSize], plain, nil)
	return sealed, nil
}

// OpenPacket authenticates and decrypts a single wire datagram.
func (c *ObfsPacketConn) OpenPacket(wire []byte) ([]byte, bool) {
	if len(wire) < obfsOverhead {
		return nil, false
	}
	nonce := wire[:obfsNonceSize]
	ct := wire[obfsNonceSize:]
	plain, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil || len(plain) < 2 {
		return nil, false
	}
	n := int(binary.BigEndian.Uint16(plain[:2]))
	if n > len(plain)-2 {
		return nil, false
	}
	return plain[2 : 2+n], true
}

func (c *ObfsPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	buf := make([]byte, len(p)+obfsOverhead+1280)
	if len(buf) > maxWireDatagram {
		buf = make([]byte, maxWireDatagram)
	}
	for {
		n, addr, err := c.inner.ReadFrom(buf)
		if err != nil {
			return 0, nil, err
		}
		plain, ok := c.OpenPacket(buf[:n])
		if !ok {
			c.dropped.Add(1)
			continue
		}
		if len(plain) > len(p) {
			c.dropped.Add(1)
			continue
		}
		copied := copy(p, plain)
		return copied, addr, nil
	}
}

func (c *ObfsPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	wire, err := c.SealPacket(p)
	if err != nil {
		return 0, err
	}
	if _, err := c.inner.WriteTo(wire, addr); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *ObfsPacketConn) Close() error                       { return c.inner.Close() }
func (c *ObfsPacketConn) LocalAddr() net.Addr                { return c.inner.LocalAddr() }
func (c *ObfsPacketConn) SetDeadline(t time.Time) error      { return c.inner.SetDeadline(t) }
func (c *ObfsPacketConn) SetReadDeadline(t time.Time) error  { return c.inner.SetReadDeadline(t) }
func (c *ObfsPacketConn) SetWriteDeadline(t time.Time) error { return c.inner.SetWriteDeadline(t) }
