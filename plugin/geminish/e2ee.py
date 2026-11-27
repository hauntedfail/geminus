"""Pure-Python RFC 8439 ChaCha20-Poly1305 and GE2E v1 HTTP tunnel client.

Zero external dependencies (Python standard library only). Interoperates
byte-for-byte with Go's internal/e2ee and cmd/geminus-e2ee-proxy so Honcho
traffic stays encrypted across Cloudflare/intermediate hops while Honcho
continues storing raw records.
"""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
import struct
import time

MAGIC = b"GE2E"
VERSION = 1
DIR_REQUEST = 1
DIR_RESPONSE = 2
KEY_SIZE = 32
NONCE_SIZE = 12
TAG_SIZE = 16
SALT_SIZE = 24
HEADER_SIZE = 4 + 1 + 1 + 8 + SALT_SIZE
MIN_SECRET_BYTES = 32
MIN_PAD_BUCKET = 512
CONTENT_TYPE = "application/octet-stream"

_INFO_REQ_KEY = b"geminus-e2ee-v1:req"
_INFO_REQ_NONCE = b"geminus-e2ee-v1:req-nonce"
_INFO_RES_KEY = b"geminus-e2ee-v1:res"
_INFO_RES_NONCE = b"geminus-e2ee-v1:res-nonce"
_CHACHA_CONST = (0x61707865, 0x3320646E, 0x79622D32, 0x6B206574)
_MASK32 = 0xFFFFFFFF
_POLY_P = (1 << 130) - 5


class E2EEError(ValueError):
    """Raised on invalid GE2E v1 frames, keys, or authentication failures."""


def _rotl32(v: int, n: int) -> int:
    return ((v << n) & _MASK32) | (v >> (32 - n))


def _qround(s: list[int], a: int, b: int, c: int, d: int) -> None:
    s[a] = (s[a] + s[b]) & _MASK32
    s[d] = _rotl32(s[d] ^ s[a], 16)
    s[c] = (s[c] + s[d]) & _MASK32
    s[b] = _rotl32(s[b] ^ s[c], 12)
    s[a] = (s[a] + s[b]) & _MASK32
    s[d] = _rotl32(s[d] ^ s[a], 8)
    s[c] = (s[c] + s[d]) & _MASK32
    s[b] = _rotl32(s[b] ^ s[c], 7)


def chacha20_block(key: bytes, counter: int, nonce: bytes) -> bytes:
    """Compute one 64-byte RFC 8439 ChaCha20 keystream block."""
    init = list(_CHACHA_CONST)
    init.extend(struct.unpack("<8I", key))
    init.append(counter & _MASK32)
    init.extend(struct.unpack("<3I", nonce))
    s = list(init)
    for _ in range(10):
        _qround(s, 0, 4, 8, 12)
        _qround(s, 1, 5, 9, 13)
        _qround(s, 2, 6, 10, 14)
        _qround(s, 3, 7, 11, 15)
        _qround(s, 0, 5, 10, 15)
        _qround(s, 1, 6, 11, 12)
        _qround(s, 2, 7, 8, 13)
        _qround(s, 3, 4, 9, 14)
    return struct.pack("<16I", *((s[i] + init[i]) & _MASK32 for i in range(16)))


def chacha20_xor(key: bytes, counter: int, nonce: bytes, data: bytes) -> bytes:
    """XOR data with the ChaCha20 keystream starting at counter."""
    out = bytearray(len(data))
    ctr = counter
    for offset in range(0, len(data), 64):
        block = chacha20_block(key, ctr, nonce)
        ctr = (ctr + 1) & _MASK32
        chunk = data[offset:offset + 64]
        for i, b in enumerate(chunk):
            out[offset + i] = b ^ block[i]
    return bytes(out)


def poly1305_mac(key32: bytes, msg: bytes) -> bytes:
    """Compute the 16-byte RFC 8439 Poly1305 authenticator."""
    r = bytearray(key32[:16])
    r[3] &= 15
    r[7] &= 15
    r[11] &= 15
    r[15] &= 15
    r[4] &= 252
    r[8] &= 252
    r[12] &= 252
    r_int = int.from_bytes(r, "little")
    s_int = int.from_bytes(key32[16:32], "little")
    acc = 0
    for offset in range(0, len(msg), 16):
        block = msg[offset:offset + 16]
        n = int.from_bytes(block + b"\x01", "little")
        acc = ((acc + n) * r_int) % _POLY_P
    tag = (acc + s_int) & ((1 << 128) - 1)
    return tag.to_bytes(TAG_SIZE, "little")


def _poly1305_aead_tag(otk: bytes, aad: bytes, ct: bytes) -> bytes:
    pad_aad = (16 - (len(aad) % 16)) % 16
    pad_ct = (16 - (len(ct) % 16)) % 16
    mac_data = (
        aad
        + (b"\x00" * pad_aad)
        + ct
        + (b"\x00" * pad_ct)
        + struct.pack("<QQ", len(aad), len(ct))
    )
    return poly1305_mac(otk, mac_data)


def seal_aead(key: bytes, nonce: bytes, plaintext: bytes, aad: bytes) -> bytes:
    """Encrypt and authenticate plaintext using RFC 8439 ChaCha20-Poly1305."""
    if len(key) != KEY_SIZE:
        raise E2EEError("invalid key size")
    if len(nonce) != NONCE_SIZE:
        raise E2EEError("invalid nonce size")
    otk = chacha20_block(key, 0, nonce)[:32]
    ct = chacha20_xor(key, 1, nonce, plaintext)
    tag = _poly1305_aead_tag(otk, aad, ct)
    return ct + tag


def open_aead(key: bytes, nonce: bytes, sealed: bytes, aad: bytes) -> bytes:
    """Verify and decrypt ciphertext||tag using RFC 8439 ChaCha20-Poly1305."""
    if len(key) != KEY_SIZE:
        raise E2EEError("invalid key size")
    if len(nonce) != NONCE_SIZE:
        raise E2EEError("invalid nonce size")
    if len(sealed) < TAG_SIZE:
        raise E2EEError("authentication failed")
    ct, got_tag = sealed[:-TAG_SIZE], sealed[-TAG_SIZE:]
    otk = chacha20_block(key, 0, nonce)[:32]
    want_tag = _poly1305_aead_tag(otk, aad, ct)
    if not hmac.compare_digest(got_tag, want_tag):
        raise E2EEError("authentication failed")
    return chacha20_xor(key, 1, nonce, ct)


def hkdf_sha256_extract(salt: bytes, ikm: bytes) -> bytes:
    return hmac.new(salt, ikm, hashlib.sha256).digest()


def hkdf_sha256_expand(prk: bytes, info: bytes, length: int) -> bytes:
    out = bytearray()
    prev = b""
    counter = 1
    while len(out) < length:
        prev = hmac.new(prk, prev + info + bytes([counter]), hashlib.sha256).digest()
        out.extend(prev)
        counter += 1
    return bytes(out[:length])


def pad_bucket(n: int) -> int:
    """Round n up to match Go's PadBucket (512B..32KiB powers of 2, then 4KiB steps)."""
    if n <= MIN_PAD_BUCKET:
        return MIN_PAD_BUCKET
    if n <= 32768:
        b = MIN_PAD_BUCKET
        while b < n:
            b <<= 1
        return b
    step = 4096
    rem = n % step
    return n if rem == 0 else n + (step - rem)


def _derive_key_nonce(secret: bytes, salt: bytes, direction: int) -> tuple[bytes, bytes]:
    if len(secret) < MIN_SECRET_BYTES:
        raise E2EEError("secret must be at least 32 bytes")
    if len(salt) != SALT_SIZE:
        raise E2EEError("invalid salt size")
    prk = hkdf_sha256_extract(salt, secret)
    if direction == DIR_REQUEST:
        return (
            hkdf_sha256_expand(prk, _INFO_REQ_KEY, KEY_SIZE),
            hkdf_sha256_expand(prk, _INFO_REQ_NONCE, NONCE_SIZE),
        )
    if direction == DIR_RESPONSE:
        return (
            hkdf_sha256_expand(prk, _INFO_RES_KEY, KEY_SIZE),
            hkdf_sha256_expand(prk, _INFO_RES_NONCE, NONCE_SIZE),
        )
    raise E2EEError("unknown direction")


def _encode_header(direction: int, ts: int, salt: bytes) -> bytes:
    return MAGIC + struct.pack(">BBq", VERSION, direction, int(ts)) + salt


def _decode_header(frame: bytes, want_dir: int) -> tuple[int, bytes]:
    if len(frame) < HEADER_SIZE + TAG_SIZE:
        raise E2EEError("frame too short")
    if frame[:4] != MAGIC:
        raise E2EEError("invalid magic")
    ver, direction, ts = struct.unpack(">BBq", frame[4:14])
    if ver != VERSION or direction != want_dir:
        raise E2EEError("version or direction mismatch")
    return ts, frame[14:HEADER_SIZE]


def _marshal_padded(obj: dict) -> bytes:
    raw = json.dumps(obj, separators=(",", ":")).encode("utf-8")
    overhead = len(b',"pad":""')
    target = pad_bucket(len(raw) + overhead)
    pad_len = target - len(raw) - overhead
    if pad_len > 0:
        padded = dict(obj)
        padded["pad"] = "a" * pad_len
        return json.dumps(padded, separators=(",", ":")).encode("utf-8")
    return raw


def seal_request(
    secret: bytes,
    method: str,
    path: str,
    query: str = "",
    headers: dict[str, str] | None = None,
    body: bytes | None = None,
    *,
    ts: int | None = None,
    salt: bytes | None = None,
) -> tuple[bytes, int, bytes]:
    """Seal an inner HTTP request into a GE2E v1 frame. Returns (frame, ts, salt)."""
    stamp = int(time.time()) if ts is None else int(ts)
    used_salt = os.urandom(SALT_SIZE) if salt is None else bytes(salt)
    key, nonce = _derive_key_nonce(secret, used_salt, DIR_REQUEST)
    payload: dict[str, object] = {"m": method, "p": path}
    if query:
        payload["q"] = query
    if headers:
        payload["h"] = dict(headers)
    if body:
        payload["b"] = base64.b64encode(body).decode("ascii")
    plain = _marshal_padded(payload)
    hdr = _encode_header(DIR_REQUEST, stamp, used_salt)
    return hdr + seal_aead(key, nonce, plain, hdr), stamp, used_salt


def open_request(secret: bytes, frame: bytes) -> tuple[dict, int, bytes]:
    """Decrypt and authenticate a GE2E v1 request frame."""
    ts, salt = _decode_header(frame, DIR_REQUEST)
    key, nonce = _derive_key_nonce(secret, salt, DIR_REQUEST)
    plain = open_aead(key, nonce, frame[HEADER_SIZE:], frame[:HEADER_SIZE])
    try:
        obj = json.loads(plain.decode("utf-8"))
    except ValueError as exc:
        raise E2EEError("invalid request json") from exc
    if not isinstance(obj, dict):
        raise E2EEError("invalid request object")
    obj.pop("pad", None)
    return obj, ts, salt


def seal_response(
    secret: bytes,
    status: int,
    headers: dict[str, str] | None,
    body: bytes | None,
    ts: int,
    salt: bytes,
) -> bytes:
    """Seal an inner HTTP response bound to (ts, salt)."""
    key, nonce = _derive_key_nonce(secret, salt, DIR_RESPONSE)
    payload: dict[str, object] = {"s": int(status)}
    if headers:
        payload["h"] = dict(headers)
    if body:
        payload["b"] = base64.b64encode(body).decode("ascii")
    plain = _marshal_padded(payload)
    hdr = _encode_header(DIR_RESPONSE, ts, salt)
    return hdr + seal_aead(key, nonce, plain, hdr)


def open_response(secret: bytes, frame: bytes, expected_ts: int, expected_salt: bytes) -> tuple[int, dict[str, str], bytes]:
    """Decrypt and verify a GE2E v1 response frame against the originating (ts, salt)."""
    if len(expected_salt) != SALT_SIZE:
        raise E2EEError("invalid expected salt")
    ts, salt = _decode_header(frame, DIR_RESPONSE)
    if ts != int(expected_ts) or not hmac.compare_digest(salt, expected_salt):
        raise E2EEError("response does not match request")
    key, nonce = _derive_key_nonce(secret, salt, DIR_RESPONSE)
    plain = open_aead(key, nonce, frame[HEADER_SIZE:], frame[:HEADER_SIZE])
    try:
        obj = json.loads(plain.decode("utf-8"))
    except ValueError as exc:
        raise E2EEError("invalid response json") from exc
    if not isinstance(obj, dict) or not isinstance(obj.get("s"), int):
        raise E2EEError("invalid response object")
    raw_b64 = obj.get("b") or ""
    body = base64.b64decode(raw_b64) if raw_b64 else b""
    hdrs = obj.get("h") if isinstance(obj.get("h"), dict) else {}
    return int(obj["s"]), {str(k): str(v) for k, v in hdrs.items()}, body
