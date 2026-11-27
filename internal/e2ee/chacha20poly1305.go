// Package e2ee implements zero-dependency application-layer end-to-end
// encryption (GE2E v1) for tunneling Honcho HTTP requests and responses across
// untrusted intermediate networks while keeping Honcho storage raw.
package e2ee

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"math/bits"
)

const (
	// KeySize is the ChaCha20-Poly1305 key length in bytes (256 bits).
	KeySize = 32
	// NonceSize is the RFC 8439 ChaCha20-Poly1305 nonce length in bytes (96 bits).
	NonceSize = 12
	// TagSize is the Poly1305 authentication tag length in bytes (128 bits).
	TagSize = 16
)

var (
	// ErrInvalidKey is returned when a key does not match the required length.
	ErrInvalidKey = errors.New("e2ee: invalid key size")
	// ErrInvalidNonce is returned when a nonce does not match the required length.
	ErrInvalidNonce = errors.New("e2ee: invalid nonce size")
	// ErrAuthFailed is returned when AEAD authentication fails.
	ErrAuthFailed = errors.New("e2ee: message authentication failed")
)

const (
	c0 uint32 = 0x61707865
	c1 uint32 = 0x3320646e
	c2 uint32 = 0x79622d32
	c3 uint32 = 0x6b206574
)

func quarterRound(a, b, c, d uint32) (uint32, uint32, uint32, uint32) {
	a += b
	d ^= a
	d = bits.RotateLeft32(d, 16)
	c += d
	b ^= c
	b = bits.RotateLeft32(b, 12)
	a += b
	d ^= a
	d = bits.RotateLeft32(d, 8)
	c += d
	b ^= c
	b = bits.RotateLeft32(b, 7)
	return a, b, c, d
}

// chacha20Block computes one 64-byte RFC 8439 ChaCha20 keystream block.
func chacha20Block(key []byte, counter uint32, nonce []byte) [64]byte {
	var init [16]uint32
	init[0], init[1], init[2], init[3] = c0, c1, c2, c3
	for i := 0; i < 8; i++ {
		init[4+i] = binary.LittleEndian.Uint32(key[4*i:])
	}
	init[12] = counter
	init[13] = binary.LittleEndian.Uint32(nonce[0:])
	init[14] = binary.LittleEndian.Uint32(nonce[4:])
	init[15] = binary.LittleEndian.Uint32(nonce[8:])

	s := init
	for i := 0; i < 10; i++ {
		s[0], s[4], s[8], s[12] = quarterRound(s[0], s[4], s[8], s[12])
		s[1], s[5], s[9], s[13] = quarterRound(s[1], s[5], s[9], s[13])
		s[2], s[6], s[10], s[14] = quarterRound(s[2], s[6], s[10], s[14])
		s[3], s[7], s[11], s[15] = quarterRound(s[3], s[7], s[11], s[15])

		s[0], s[5], s[10], s[15] = quarterRound(s[0], s[5], s[10], s[15])
		s[1], s[6], s[11], s[12] = quarterRound(s[1], s[6], s[11], s[12])
		s[2], s[7], s[8], s[13] = quarterRound(s[2], s[7], s[8], s[13])
		s[3], s[4], s[9], s[14] = quarterRound(s[3], s[4], s[9], s[14])
	}

	var out [64]byte
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(out[4*i:], s[i]+init[i])
	}
	return out
}

// chacha20XOR XORs src with the ChaCha20 keystream starting at counter.
func chacha20XOR(key []byte, counter uint32, nonce, src []byte) []byte {
	dst := make([]byte, len(src))
	ctr := counter
	for offset := 0; offset < len(src); offset += 64 {
		block := chacha20Block(key, ctr, nonce)
		ctr++
		n := len(src) - offset
		if n > 64 {
			n = 64
		}
		for i := 0; i < n; i++ {
			dst[offset+i] = src[offset+i] ^ block[i]
		}
	}
	return dst
}

// poly1305MAC computes the 16-byte RFC 8439 Poly1305 authenticator using
// constant-time 26-bit radix arithmetic (modulo 2^130 - 5).
func poly1305MAC(key32 []byte, msg []byte) [TagSize]byte {
	// Clamp r per RFC 8439 Section 2.5.
	var rBytes [16]byte
	copy(rBytes[:], key32[:16])
	rBytes[3] &= 15
	rBytes[7] &= 15
	rBytes[11] &= 15
	rBytes[15] &= 15
	rBytes[4] &= 252
	rBytes[8] &= 252
	rBytes[12] &= 252

	t0 := binary.LittleEndian.Uint32(rBytes[0:4])
	t1 := binary.LittleEndian.Uint32(rBytes[4:8])
	t2 := binary.LittleEndian.Uint32(rBytes[8:12])
	t3 := binary.LittleEndian.Uint32(rBytes[12:16])

	r0 := uint64(t0 & 0x3ffffff)
	r1 := uint64(((t0 >> 26) | (t1 << 6)) & 0x3ffffff)
	r2 := uint64(((t1 >> 20) | (t2 << 12)) & 0x3ffffff)
	r3 := uint64(((t2 >> 14) | (t3 << 18)) & 0x3ffffff)
	r4 := uint64(t3 >> 8)

	s1 := r1 * 5
	s2 := r2 * 5
	s3 := r3 * 5
	s4 := r4 * 5

	var h0, h1, h2, h3, h4 uint64

	for len(msg) > 0 {
		var blk [16]byte
		n := len(msg)
		hibit := uint64(1 << 24)
		if n >= 16 {
			copy(blk[:], msg[:16])
			msg = msg[16:]
		} else {
			copy(blk[:], msg)
			blk[n] = 1
			hibit = 0
			msg = nil
		}

		b0 := binary.LittleEndian.Uint32(blk[0:4])
		b1 := binary.LittleEndian.Uint32(blk[4:8])
		b2 := binary.LittleEndian.Uint32(blk[8:12])
		b3 := binary.LittleEndian.Uint32(blk[12:16])

		h0 += uint64(b0 & 0x3ffffff)
		h1 += uint64(((b0 >> 26) | (b1 << 6)) & 0x3ffffff)
		h2 += uint64(((b1 >> 20) | (b2 << 12)) & 0x3ffffff)
		h3 += uint64(((b2 >> 14) | (b3 << 18)) & 0x3ffffff)
		h4 += uint64(b3>>8) | hibit

		d0 := h0*r0 + h1*s4 + h2*s3 + h3*s2 + h4*s1
		d1 := h0*r1 + h1*r0 + h2*s4 + h3*s3 + h4*s2
		d2 := h0*r2 + h1*r1 + h2*r0 + h3*s4 + h4*s3
		d3 := h0*r3 + h1*r2 + h2*r1 + h3*r0 + h4*s4
		d4 := h0*r4 + h1*r3 + h2*r2 + h3*r1 + h4*r0

		var c uint64
		c = d0 >> 26
		h0 = d0 & 0x3ffffff
		d1 += c
		c = d1 >> 26
		h1 = d1 & 0x3ffffff
		d2 += c
		c = d2 >> 26
		h2 = d2 & 0x3ffffff
		d3 += c
		c = d3 >> 26
		h3 = d3 & 0x3ffffff
		d4 += c
		c = d4 >> 26
		h4 = d4 & 0x3ffffff
		h0 += c * 5
		c = h0 >> 26
		h0 &= 0x3ffffff
		h1 += c
	}

	// Final carry propagation.
	var c uint64
	c = h1 >> 26
	h1 &= 0x3ffffff
	h2 += c
	c = h2 >> 26
	h2 &= 0x3ffffff
	h3 += c
	c = h3 >> 26
	h3 &= 0x3ffffff
	h4 += c
	c = h4 >> 26
	h4 &= 0x3ffffff
	h0 += c * 5
	c = h0 >> 26
	h0 &= 0x3ffffff
	h1 += c

	// Compute h + (-p) = h + 5 - 2^130; select in constant time.
	g0 := h0 + 5
	c = g0 >> 26
	g0 &= 0x3ffffff
	g1 := h1 + c
	c = g1 >> 26
	g1 &= 0x3ffffff
	g2 := h2 + c
	c = g2 >> 26
	g2 &= 0x3ffffff
	g3 := h3 + c
	c = g3 >> 26
	g3 &= 0x3ffffff
	g4 := h4 + c - (1 << 26)

	mask := (g4 >> 63) - 1
	g0 &= mask
	g1 &= mask
	g2 &= mask
	g3 &= mask
	g4 &= mask
	mask = ^mask
	h0 = (h0 & mask) | g0
	h1 = (h1 & mask) | g1
	h2 = (h2 & mask) | g2
	h3 = (h3 & mask) | g3
	h4 = (h4 & mask) | g4

	// Pack h into 4x32-bit words and add pad s = key32[16:32].
	f0 := uint64(uint32(h0 | (h1 << 26)))
	f1 := uint64(uint32((h1 >> 6) | (h2 << 20)))
	f2 := uint64(uint32((h2 >> 12) | (h3 << 14)))
	f3 := uint64(uint32((h3 >> 18) | (h4 << 8)))

	f0 += uint64(binary.LittleEndian.Uint32(key32[16:20]))
	f1 += uint64(binary.LittleEndian.Uint32(key32[20:24])) + (f0 >> 32)
	f2 += uint64(binary.LittleEndian.Uint32(key32[24:28])) + (f1 >> 32)
	f3 += uint64(binary.LittleEndian.Uint32(key32[28:32])) + (f2 >> 32)

	var tag [TagSize]byte
	binary.LittleEndian.PutUint32(tag[0:4], uint32(f0))
	binary.LittleEndian.PutUint32(tag[4:8], uint32(f1))
	binary.LittleEndian.PutUint32(tag[8:12], uint32(f2))
	binary.LittleEndian.PutUint32(tag[12:16], uint32(f3))
	return tag
}

func poly1305AEADTag(otk, aad, ciphertext []byte) [TagSize]byte {
	padAAD := (16 - (len(aad) % 16)) % 16
	padCT := (16 - (len(ciphertext) % 16)) % 16
	buf := make([]byte, 0, len(aad)+padAAD+len(ciphertext)+padCT+16)
	buf = append(buf, aad...)
	buf = append(buf, make([]byte, padAAD)...)
	buf = append(buf, ciphertext...)
	buf = append(buf, make([]byte, padCT)...)
	var lens [16]byte
	binary.LittleEndian.PutUint64(lens[0:8], uint64(len(aad)))
	binary.LittleEndian.PutUint64(lens[8:16], uint64(len(ciphertext)))
	buf = append(buf, lens[:]...)
	return poly1305MAC(otk, buf)
}

// SealAEAD encrypts and authenticates plaintext using RFC 8439 ChaCha20-Poly1305.
// It returns ciphertext || 16-byte tag.
func SealAEAD(key, nonce, plaintext, aad []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKey
	}
	if len(nonce) != NonceSize {
		return nil, ErrInvalidNonce
	}
	otkBlock := chacha20Block(key, 0, nonce)
	ct := chacha20XOR(key, 1, nonce, plaintext)
	tag := poly1305AEADTag(otkBlock[:32], aad, ct)
	out := make([]byte, len(ct)+TagSize)
	copy(out, ct)
	copy(out[len(ct):], tag[:])
	return out, nil
}

// OpenAEAD verifies and decrypts ciphertext||tag using RFC 8439 ChaCha20-Poly1305.
func OpenAEAD(key, nonce, sealed, aad []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKey
	}
	if len(nonce) != NonceSize {
		return nil, ErrInvalidNonce
	}
	if len(sealed) < TagSize {
		return nil, ErrAuthFailed
	}
	ct := sealed[:len(sealed)-TagSize]
	gotTag := sealed[len(sealed)-TagSize:]
	otkBlock := chacha20Block(key, 0, nonce)
	wantTag := poly1305AEADTag(otkBlock[:32], aad, ct)
	if subtle.ConstantTimeCompare(gotTag, wantTag[:]) != 1 {
		return nil, ErrAuthFailed
	}
	return chacha20XOR(key, 1, nonce, ct), nil
}
