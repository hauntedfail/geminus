"""Pure-Python RFC 8032 Ed25519 signature verification (zero external dependencies).

Used by the geminish bridge to verify human-authored geminus prompts signed by
the owner's asymmetric Ed25519 key, breaking symmetric HMAC blast radius.
"""
from __future__ import annotations

import hashlib

_P = (1 << 255) - 19
_L = (1 << 252) + 27742317777372353535851937790883648493
_D = (-121665 * pow(121666, _P - 2, _P)) % _P
_I = pow(2, (_P - 1) // 4, _P)
_ED25519_DOMAIN = b"geminus-ed25519:"


def _xrecover(y: int) -> int:
    xx = (y * y - 1) * pow(_D * y * y + 1, _P - 2, _P) % _P
    x = pow(xx, (_P + 3) // 8, _P)
    if (x * x - xx) % _P != 0:
        x = (x * _I) % _P
    if x % 2 != 0:
        x = _P - x
    return x


_BY = (4 * pow(5, _P - 2, _P)) % _P
_BX = _xrecover(_BY)
_B = (_BX, _BY, 1, (_BX * _BY) % _P)
_ID = (0, 1, 1, 0)


def _point_add(p1: tuple[int, int, int, int], p2: tuple[int, int, int, int]) -> tuple[int, int, int, int]:
    x1, y1, z1, t1 = p1
    x2, y2, z2, t2 = p2
    a = ((y1 - x1) * (y2 - x2)) % _P
    b = ((y1 + x1) * (y2 + x2)) % _P
    c = (t1 * 2 * _D * t2) % _P
    d = (z1 * 2 * z2) % _P
    e = (b - a) % _P
    f = (d - c) % _P
    g = (d + c) % _P
    h = (b + a) % _P
    return ((e * f) % _P, (g * h) % _P, (f * g) % _P, (e * h) % _P)


def _point_mul(s: int, pt: tuple[int, int, int, int]) -> tuple[int, int, int, int]:
    q = _ID
    p = pt
    while s > 0:
        if s & 1:
            q = _point_add(q, p)
        p = _point_add(p, p)
        s >>= 1
    return q


def _point_equal(p1: tuple[int, int, int, int], p2: tuple[int, int, int, int]) -> bool:
    x1, y1, z1, _ = p1
    x2, y2, z2, _ = p2
    if (x1 * z2 - x2 * z1) % _P != 0:
        return False
    if (y1 * z2 - y2 * z1) % _P != 0:
        return False
    return True


def _decode_point(raw: bytes) -> tuple[int, int, int, int] | None:
    if len(raw) != 32:
        return None
    y = int.from_bytes(raw, "little")
    sign = (y >> 255) & 1
    y &= (1 << 255) - 1
    if y >= _P:
        return None
    u = (y * y - 1) % _P
    v = (_D * y * y + 1) % _P
    v3 = pow(v, 3, _P)
    v7 = (v3 * v3 * v) % _P
    x = (u * v3 * pow(u * v7, (_P - 5) // 8, _P)) % _P
    vx2 = (v * x * x) % _P
    if (vx2 - u) % _P != 0:
        if (vx2 + u) % _P != 0:
            return None
        x = (x * _I) % _P
    if x == 0 and sign == 1:
        return None
    if (x & 1) != sign:
        x = _P - x
    return (x, y, 1, (x * y) % _P)


def verify_ed25519(pubkey: bytes, message: bytes, signature: bytes) -> bool:
    """Return True iff signature is a valid RFC 8032 Ed25519 signature over message."""
    if not isinstance(pubkey, (bytes, bytearray)) or len(pubkey) != 32:
        return False
    if not isinstance(signature, (bytes, bytearray)) or len(signature) != 64:
        return False
    if not isinstance(message, (bytes, bytearray)):
        return False
    a_pt = _decode_point(bytes(pubkey))
    r_pt = _decode_point(bytes(signature[:32]))
    if a_pt is None or r_pt is None:
        return False
    s = int.from_bytes(signature[32:], "little")
    if s >= _L:
        return False
    h = int.from_bytes(hashlib.sha512(bytes(signature[:32]) + bytes(pubkey) + bytes(message)).digest(), "little") % _L
    sb = _point_mul(s, _B)
    r_ha = _point_add(r_pt, _point_mul(h, a_pt))
    return _point_equal(sb, r_ha)


def verify_human_ed25519(pubkey: bytes, content: str, label: str, ts: int, sig_hex: object) -> bool:
    """Verify a geminus Ed25519 human signature over (content, label, ts)."""
    if not isinstance(sig_hex, str) or len(sig_hex) != 128:
        return False
    try:
        sig = bytes.fromhex(sig_hex)
    except ValueError:
        return False
    payload = (
        _ED25519_DOMAIN
        + hashlib.sha256(content.encode("utf-8")).digest()
        + hashlib.sha256(label.encode("utf-8")).digest()
        + str(int(ts)).encode("ascii")
    )
    return verify_ed25519(pubkey, payload, sig)
