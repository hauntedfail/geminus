package e2ee

import (
	"bytes"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	// Magic identifies a GE2E binary frame.
	Magic = "GE2E"
	// Version is the current GE2E wire protocol version.
	Version byte = 1
	// DirRequest marks an encrypted client -> proxy HTTP request.
	DirRequest byte = 1
	// DirResponse marks an encrypted proxy -> client HTTP response.
	DirResponse byte = 2

	// SaltSize is the per-request random salt size in bytes (192 bits).
	SaltSize = 24
	// HeaderSize is magic(4) + version(1) + dir(1) + ts(8) + salt(24).
	HeaderSize = 4 + 1 + 1 + 8 + SaltSize
	// MinSecretBytes matches geminise.json's minimum group secret length.
	MinSecretBytes = 32
	// MinPadBucket is the smallest padded inner JSON size in bytes.
	MinPadBucket = 512

	infoReqKey   = "geminus-e2ee-v1:req"
	infoReqNonce = "geminus-e2ee-v1:req-nonce"
	infoResKey   = "geminus-e2ee-v1:res"
	infoResNonce = "geminus-e2ee-v1:res-nonce"
)

var (
	// ErrInvalidFrame is returned when a GE2E binary frame is malformed.
	ErrInvalidFrame = errors.New("e2ee: invalid frame")
	// ErrInvalidSecret is returned when the shared secret is shorter than MinSecretBytes.
	ErrInvalidSecret = errors.New("e2ee: secret must be at least 32 bytes")
)

// Request is the inner HTTP request encrypted inside a GE2E v1 request frame.
type Request struct {
	Method  string            `json:"m"`
	Path    string            `json:"p"`
	Query   string            `json:"q,omitempty"`
	Headers map[string]string `json:"h,omitempty"`
	BodyB64 string            `json:"b,omitempty"`
	Pad     string            `json:"pad,omitempty"`
}

// BodyBytes decodes the inner request body bytes.
func (r Request) BodyBytes() ([]byte, error) {
	if r.BodyB64 == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(r.BodyB64)
}

// SetBody encodes body into BodyB64.
func (r *Request) SetBody(body []byte) {
	if len(body) == 0 {
		r.BodyB64 = ""
		return
	}
	r.BodyB64 = base64.StdEncoding.EncodeToString(body)
}

// Response is the inner HTTP response encrypted inside a GE2E v1 response frame.
type Response struct {
	Status  int               `json:"s"`
	Headers map[string]string `json:"h,omitempty"`
	BodyB64 string            `json:"b,omitempty"`
	Pad     string            `json:"pad,omitempty"`
}

// BodyBytes decodes the inner response body bytes.
func (r Response) BodyBytes() ([]byte, error) {
	if r.BodyB64 == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(r.BodyB64)
}

// SetBody encodes body into BodyB64.
func (r *Response) SetBody(body []byte) {
	if len(body) == 0 {
		r.BodyB64 = ""
		return
	}
	r.BodyB64 = base64.StdEncoding.EncodeToString(body)
}

// PadBucket rounds n up to a coarse size bucket to obscure small payload sizes:
// powers of two from 512B up to 32KiB, then 4KiB steps above 32KiB.
func PadBucket(n int) int {
	if n <= MinPadBucket {
		return MinPadBucket
	}
	if n <= 32768 {
		b := MinPadBucket
		for b < n {
			b <<= 1
		}
		return b
	}
	const step = 4096
	rem := n % step
	if rem == 0 {
		return n
	}
	return n + (step - rem)
}

func deriveKeyNonce(secret, salt []byte, dir byte) (key, nonce []byte, err error) {
	if len(secret) < MinSecretBytes {
		return nil, nil, ErrInvalidSecret
	}
	if len(salt) != SaltSize {
		return nil, nil, fmt.Errorf("%w: bad salt size %d", ErrInvalidFrame, len(salt))
	}
	prk, err := hkdf.Extract(sha256.New, secret, salt)
	if err != nil {
		return nil, nil, err
	}
	var keyInfo, nonceInfo string
	switch dir {
	case DirRequest:
		keyInfo, nonceInfo = infoReqKey, infoReqNonce
	case DirResponse:
		keyInfo, nonceInfo = infoResKey, infoResNonce
	default:
		return nil, nil, fmt.Errorf("%w: unknown direction %d", ErrInvalidFrame, dir)
	}
	key, err = hkdf.Expand(sha256.New, prk, keyInfo, KeySize)
	if err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, nonceInfo, NonceSize)
	if err != nil {
		return nil, nil, err
	}
	return key, nonce, nil
}

func encodeHeader(dir byte, ts int64, salt []byte) [HeaderSize]byte {
	var hdr [HeaderSize]byte
	copy(hdr[0:4], Magic)
	hdr[4] = Version
	hdr[5] = dir
	binary.BigEndian.PutUint64(hdr[6:14], uint64(ts))
	copy(hdr[14:HeaderSize], salt)
	return hdr
}

func decodeHeader(frame []byte, wantDir byte) (ts int64, salt []byte, err error) {
	if len(frame) < HeaderSize+TagSize {
		return 0, nil, ErrInvalidFrame
	}
	if !bytes.Equal(frame[0:4], []byte(Magic)) {
		return 0, nil, ErrInvalidFrame
	}
	if frame[4] != Version || frame[5] != wantDir {
		return 0, nil, ErrInvalidFrame
	}
	ts = int64(binary.BigEndian.Uint64(frame[6:14]))
	salt = make([]byte, SaltSize)
	copy(salt, frame[14:HeaderSize])
	return ts, salt, nil
}

func marshalPaddedRequest(req Request) ([]byte, error) {
	req.Pad = ""
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	const overhead = len(`,"pad":""`)
	target := PadBucket(len(raw) + overhead)
	padLen := target - len(raw) - overhead
	if padLen > 0 {
		req.Pad = strings.Repeat("a", padLen)
		return json.Marshal(req)
	}
	return raw, nil
}

func marshalPaddedResponse(res Response) ([]byte, error) {
	res.Pad = ""
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	const overhead = len(`,"pad":""`)
	target := PadBucket(len(raw) + overhead)
	padLen := target - len(raw) - overhead
	if padLen > 0 {
		res.Pad = strings.Repeat("a", padLen)
		return json.Marshal(res)
	}
	return raw, nil
}

// SealRequest encrypts an inner HTTP request into a GE2E v1 binary frame.
// If salt is nil, a fresh 24-byte CSPRNG salt is generated and returned.
func SealRequest(secret []byte, req Request, ts int64, salt []byte) (frame []byte, usedSalt []byte, err error) {
	if salt == nil {
		usedSalt = make([]byte, SaltSize)
		if _, err := rand.Read(usedSalt); err != nil {
			return nil, nil, err
		}
	} else {
		if len(salt) != SaltSize {
			return nil, nil, fmt.Errorf("%w: salt must be %d bytes", ErrInvalidFrame, SaltSize)
		}
		usedSalt = append([]byte(nil), salt...)
	}
	key, nonce, err := deriveKeyNonce(secret, usedSalt, DirRequest)
	if err != nil {
		return nil, nil, err
	}
	plain, err := marshalPaddedRequest(req)
	if err != nil {
		return nil, nil, err
	}
	hdr := encodeHeader(DirRequest, ts, usedSalt)
	sealed, err := SealAEAD(key, nonce, plain, hdr[:])
	if err != nil {
		return nil, nil, err
	}
	frame = make([]byte, HeaderSize+len(sealed))
	copy(frame[:HeaderSize], hdr[:])
	copy(frame[HeaderSize:], sealed)
	return frame, usedSalt, nil
}

// OpenRequest authenticates and decrypts a GE2E v1 request frame.
func OpenRequest(secret, frame []byte) (req Request, ts int64, salt []byte, err error) {
	ts, salt, err = decodeHeader(frame, DirRequest)
	if err != nil {
		return Request{}, 0, nil, err
	}
	key, nonce, err := deriveKeyNonce(secret, salt, DirRequest)
	if err != nil {
		return Request{}, 0, nil, err
	}
	plain, err := OpenAEAD(key, nonce, frame[HeaderSize:], frame[:HeaderSize])
	if err != nil {
		return Request{}, 0, nil, err
	}
	if err := json.Unmarshal(plain, &req); err != nil {
		return Request{}, 0, nil, fmt.Errorf("%w: %v", ErrInvalidFrame, err)
	}
	req.Pad = ""
	return req, ts, salt, nil
}

// SealResponse encrypts an inner HTTP response bound to the originating
// request's timestamp and salt.
func SealResponse(secret []byte, res Response, ts int64, salt []byte) ([]byte, error) {
	key, nonce, err := deriveKeyNonce(secret, salt, DirResponse)
	if err != nil {
		return nil, err
	}
	plain, err := marshalPaddedResponse(res)
	if err != nil {
		return nil, err
	}
	hdr := encodeHeader(DirResponse, ts, salt)
	sealed, err := SealAEAD(key, nonce, plain, hdr[:])
	if err != nil {
		return nil, err
	}
	frame := make([]byte, HeaderSize+len(sealed))
	copy(frame[:HeaderSize], hdr[:])
	copy(frame[HeaderSize:], sealed)
	return frame, nil
}

// OpenResponse authenticates and decrypts a GE2E v1 response frame and ensures
// it matches the originating request's timestamp and salt.
func OpenResponse(secret, frame []byte, expectedTS int64, expectedSalt []byte) (Response, error) {
	if len(expectedSalt) != SaltSize {
		return Response{}, ErrInvalidFrame
	}
	ts, salt, err := decodeHeader(frame, DirResponse)
	if err != nil {
		return Response{}, err
	}
	if ts != expectedTS || subtle.ConstantTimeCompare(salt, expectedSalt) != 1 {
		return Response{}, ErrAuthFailed
	}
	key, nonce, err := deriveKeyNonce(secret, salt, DirResponse)
	if err != nil {
		return Response{}, err
	}
	plain, err := OpenAEAD(key, nonce, frame[HeaderSize:], frame[:HeaderSize])
	if err != nil {
		return Response{}, err
	}
	var res Response
	if err := json.Unmarshal(plain, &res); err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrInvalidFrame, err)
	}
	res.Pad = ""
	return res, nil
}
