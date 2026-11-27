package e2ee

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
)

const (
	// DirStreamChunk marks an encrypted proxy -> client SSE stream event frame.
	DirStreamChunk byte = 3

	// StreamChunkHeaderSize is HeaderSize(38) + uint64 big-endian chunk sequence(8).
	StreamChunkHeaderSize = HeaderSize + 8

	// Inner GE2E paths intercepted locally by Proxy for ephemeral streaming
	// (never forwarded to upstream Honcho).
	StreamPathPublish   = "/_stream/publish"
	StreamPathSubscribe = "/_stream/subscribe"

	// Ephemeral stream event kinds emitted by the bridge.
	StreamKindDelta      = "delta"
	StreamKindToolUse    = "tool_use"
	StreamKindToolResult = "tool_result"
	StreamKindDone       = "done"

	infoStreamKey   = "geminus-e2ee-v1:stream"
	infoStreamNonce = "geminus-e2ee-v1:stream-nonce"
)

// StreamEvent is one ephemeral real-time event emitted by a bridge during a turn.
// Every event carries an HMAC-SHA256 bridge signature over its content and metadata.
type StreamEvent struct {
	ID      uint64 `json:"id,omitempty"`
	Session string `json:"session"`
	Turn    string `json:"turn"`
	Seq     int    `json:"seq"`
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Detail  string `json:"detail,omitempty"`
	OK      bool   `json:"ok,omitempty"`
	TS      int64  `json:"ts"`
	Sig     string `json:"sig"`
}

// StreamPublishRequest is the JSON payload sent inside POST /_stream/publish.
type StreamPublishRequest struct {
	Events []StreamEvent `json:"events"`
}

// StreamPublishResponse is the JSON response returned by POST /_stream/publish.
type StreamPublishResponse struct {
	OK     bool   `json:"ok"`
	LastID uint64 `json:"last_id"`
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ValidStreamKind reports whether kind is a recognized ephemeral stream event kind.
func ValidStreamKind(kind string) bool {
	switch kind {
	case StreamKindDelta, StreamKindToolUse, StreamKindToolResult, StreamKindDone:
		return true
	default:
		return false
	}
}

// StreamEventLabel builds the domain-separated provenance label signed by the bridge.
func StreamEventLabel(ev StreamEvent) string {
	okInt := 0
	if ev.OK {
		okInt = 1
	}
	return fmt.Sprintf("geminus-stream:%s:%s:%d:%s:%s:%s:%d",
		ev.Session, ev.Turn, ev.Seq, ev.Kind, sha256Hex(ev.Tool), sha256Hex(ev.Detail), okInt)
}

// SignStreamEvent produces the hex HMAC-SHA256 signature for ev using the exact
// SHA256(text) || SHA256(label) || decimal(ts) construction shared by geminus and geminish.
func SignStreamEvent(secret []byte, ev StreamEvent) string {
	contentHash := sha256.Sum256([]byte(ev.Text))
	labelHash := sha256.Sum256([]byte(StreamEventLabel(ev)))
	mac := hmac.New(sha256.New, secret)
	mac.Write(contentHash[:])
	mac.Write(labelHash[:])
	mac.Write([]byte(strconv.FormatInt(ev.TS, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyStreamEvent checks that ev has valid required fields and a matching bridge HMAC signature.
func VerifyStreamEvent(secret []byte, ev StreamEvent) bool {
	if len(secret) < MinSecretBytes || ev.Session == "" || ev.Turn == "" || ev.Seq <= 0 || !ValidStreamKind(ev.Kind) || ev.Sig == "" {
		return false
	}
	expected := SignStreamEvent(secret, ev)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(ev.Sig)) == 1
}

func deriveStreamKeyNonce(secret, salt []byte, seq uint64) (key, nonce []byte, err error) {
	if len(secret) < MinSecretBytes {
		return nil, nil, ErrInvalidSecret
	}
	if len(salt) != SaltSize {
		return nil, nil, fmt.Errorf("%w: bad salt size %d", ErrInvalidFrame, len(salt))
	}
	if seq == 0 {
		return nil, nil, fmt.Errorf("%w: stream chunk seq must be >= 1", ErrInvalidFrame)
	}
	prk, err := hkdf.Extract(sha256.New, secret, salt)
	if err != nil {
		return nil, nil, err
	}
	key, err = hkdf.Expand(sha256.New, prk, infoStreamKey, KeySize)
	if err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, infoStreamNonce, NonceSize)
	if err != nil {
		return nil, nil, err
	}
	var seqBuf [8]byte
	binary.BigEndian.PutUint64(seqBuf[:], seq)
	for i := range seqBuf {
		nonce[4+i] ^= seqBuf[i]
	}
	return key, nonce, nil
}

// SealStreamChunk encrypts one SSE stream payload bound to (ts, salt, seq).
func SealStreamChunk(secret []byte, ts int64, salt []byte, seq uint64, payload []byte) ([]byte, error) {
	key, nonce, err := deriveStreamKeyNonce(secret, salt, seq)
	if err != nil {
		return nil, err
	}
	baseHdr := encodeHeader(DirStreamChunk, ts, salt)
	var hdr [StreamChunkHeaderSize]byte
	copy(hdr[:HeaderSize], baseHdr[:])
	binary.BigEndian.PutUint64(hdr[HeaderSize:], seq)
	sealed, err := SealAEAD(key, nonce, payload, hdr[:])
	if err != nil {
		return nil, err
	}
	frame := make([]byte, StreamChunkHeaderSize+len(sealed))
	copy(frame[:StreamChunkHeaderSize], hdr[:])
	copy(frame[StreamChunkHeaderSize:], sealed)
	return frame, nil
}

// OpenStreamChunk decrypts and authenticates one SSE stream chunk, verifying
// that it matches the originating subscribe request's (ts, salt) and monotonic seq.
func OpenStreamChunk(secret, frame []byte, expectedTS int64, expectedSalt []byte, expectedSeq uint64) ([]byte, error) {
	if len(expectedSalt) != SaltSize || expectedSeq == 0 {
		return nil, ErrInvalidFrame
	}
	if len(frame) < StreamChunkHeaderSize+TagSize {
		return nil, fmt.Errorf("%w: stream chunk too short", ErrInvalidFrame)
	}
	ts, salt, err := decodeHeader(frame, DirStreamChunk)
	if err != nil {
		return nil, err
	}
	seq := binary.BigEndian.Uint64(frame[HeaderSize:StreamChunkHeaderSize])
	if ts != expectedTS || subtle.ConstantTimeCompare(salt, expectedSalt) != 1 || seq != expectedSeq {
		return nil, ErrAuthFailed
	}
	key, nonce, err := deriveStreamKeyNonce(secret, salt, seq)
	if err != nil {
		return nil, err
	}
	return OpenAEAD(key, nonce, frame[StreamChunkHeaderSize:], frame[:StreamChunkHeaderSize])
}
