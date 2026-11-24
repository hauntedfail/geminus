"""A doorbell over Mozilla AutoPush, so a recipient reads the mailbox at once instead of at its next poll.

Honcho stays the only record. A push carries no content: it only tells a host "read the mailbox
now". Each host keeps one outbound WebSocket to Mozilla's push service (no inbound port) and
publishes the endpoint it was given, signed with the group secret, in its own Honcho peer
metadata. A sender POSTs an empty push to the recipient's endpoint after its mail is stored.

Everything here is optional. If the connection, the endpoint or the POST fails, the poller
still reads the mailbox on its timer; a ring never decides anything and is never retried.
Standard library only, so it runs under whatever Python runs the poller.
"""
from __future__ import annotations

import base64
import hashlib
import json
import logging
import os
import socket
import ssl
import struct
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from .mailbox import _NoRedirect, sign, verify

log = logging.getLogger("geminish")

SERVER_HOST = "push.services.mozilla.com"
ENDPOINT_HOST = "updates.push.services.mozilla.com"
ENDPOINT_PATH = "/wpush/"
META_KEY = "geminise_push"          # key in a peer's Honcho metadata
LABEL = "push-endpoint:"            # signing label; keeps an endpoint record from passing as mail
USER_AGENT = "geminish/0.3"
MAX_MESSAGE = 1 << 20               # bytes of one WebSocket message
RING_TTL = 60                       # seconds Mozilla keeps a ring for a host that is reconnecting
RING_TIMEOUT = 3.0                  # seconds for the directory read and for each POST
RING_DEADLINE = 4.0                 # seconds a send waits for its rings in total
PING_AFTER = 240.0                  # seconds of silence before asking the server whether it is there
PONG_WAIT = 30.0
REPLY_WAIT = 20.0
MIN_BACKOFF, MAX_BACKOFF = 5.0, 300.0
ROTATE_BACKOFF = 1.0                # short reconnect delay after a healthy server-side rotation
HEALTHY = 60.0                      # a connection that lasted this long resets the backoff
_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
_OPENER = urllib.request.build_opener(_NoRedirect)


class PushError(RuntimeError):
    """The push service refused something or sent something unusable."""


# ----- the published endpoint record ---------------------------------------------

def valid_endpoint(url: object) -> bool:
    """Only Mozilla's push endpoints are ever contacted, whatever a record says."""
    if not isinstance(url, str) or not url or len(url) > 2048 or any(ch <= " " for ch in url):
        return False
    try:
        parts = urllib.parse.urlsplit(url)
        port = parts.port
    except ValueError:
        return False
    return (parts.scheme == "https" and parts.hostname == ENDPOINT_HOST and port is None
            and parts.username is None and parts.password is None and parts.path.startswith(ENDPOINT_PATH))


def record(secret: bytes, peer: str, endpoint: str, now: float) -> dict:
    stamp = int(now)
    return {"v": 1, "endpoint": endpoint, "ts": stamp, "sig": sign(secret, endpoint, LABEL + peer, stamp)}


def endpoint_from(secret: bytes, peer: str, value: object) -> str | None:
    """The endpoint in a published record, if a member host signed it for exactly this peer."""
    if not isinstance(value, dict):
        return None
    endpoint, stamp = value.get("endpoint"), value.get("ts")
    if not isinstance(endpoint, str) or type(stamp) is not int:
        return None
    if not verify(secret, endpoint, LABEL + peer, stamp, value.get("sig")) or not valid_endpoint(endpoint):
        return None
    return endpoint


# ----- ringing -------------------------------------------------------------------

def http_post(url: str, timeout: float) -> int:
    """POST an empty push. Returns the HTTP status; a redirect is returned, never followed."""
    request = urllib.request.Request(url, data=b"", method="POST", headers={
        "TTL": str(RING_TTL), "Content-Length": "0", "User-Agent": USER_AGENT})
    try:
        with _OPENER.open(request, timeout=timeout) as response:
            return int(response.status)
    except urllib.error.HTTPError as exc:
        return int(exc.code)


def ring(secret: bytes, records: dict, targets, *, timeout: float = RING_TIMEOUT,
         rang: list | None = None) -> list[str]:
    """Ring each target that published a valid endpoint. Returns the peers Mozilla accepted a ring
    for; ``rang`` is filled as each ring is accepted, for a caller that stops waiting early."""
    rang = [] if rang is None else rang
    for peer in targets:
        meta = records.get(peer)
        endpoint = endpoint_from(secret, peer, meta.get(META_KEY) if isinstance(meta, dict) else None)
        if not endpoint:
            continue
        try:
            status = http_post(endpoint, timeout)
        except Exception as exc:
            log.debug("push: ring for %s failed: %s", peer, exc.__class__.__name__)
            continue
        if 200 <= status < 300:
            rang.append(peer)
        else:
            log.debug("push: ring for %s answered HTTP %s", peer, status)
    return rang


def ring_peers(cfg, mailbox, targets) -> list[str]:
    """Best effort, after the mail is stored. Never raises, and never holds the sender longer than
    RING_DEADLINE: rings still on their way then are left to finish or fail on their own, and a
    host that was not rung reads at its next poll."""
    rang: list[str] = []

    def work() -> None:
        try:
            ring(cfg.secret, mailbox.peer_metadata(timeout=RING_TIMEOUT), targets, rang=rang)
        except Exception as exc:
            log.debug("push: directory not readable: %s", exc.__class__.__name__)

    worker = threading.Thread(target=work, name="geminish-ring", daemon=True)
    worker.start()
    worker.join(RING_DEADLINE)
    done = list(rang)
    return [peer for peer in targets if peer in done]


# ----- a small WebSocket client (RFC 6455, text messages only) --------------------

class WebSocket:
    def __init__(self, sock, host: str, path: str = "/", *, timeout: float = 20.0):
        self._sock = sock
        self._buf = b""
        self._parts: list[bytes] = []
        self._size = 0
        self._handshake(host, path, timeout)

    @classmethod
    def connect(cls, host: str = SERVER_HOST, port: int = 443, path: str = "/", *, timeout: float = 20.0):
        raw = socket.create_connection((host, port), timeout=timeout)
        try:
            sock = ssl.create_default_context().wrap_socket(raw, server_hostname=host)
        except BaseException:
            raw.close()
            raise
        try:
            return cls(sock, host, path, timeout=timeout)
        except BaseException:
            sock.close()
            raise

    def _handshake(self, host: str, path: str, timeout: float) -> None:
        key = base64.b64encode(os.urandom(16)).decode("ascii")
        self._sock.settimeout(timeout)
        self._sock.sendall((
            f"GET {path} HTTP/1.1\r\nHost: {host}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
            f"Sec-WebSocket-Protocol: push-notification\r\nUser-Agent: {USER_AGENT}\r\n\r\n").encode("ascii"))
        head = b""
        while b"\r\n\r\n" not in head:
            if len(head) > 16384:
                raise PushError("handshake refused: response too long")
            try:
                chunk = self._sock.recv(4096)
            except socket.timeout:
                raise PushError("handshake timed out") from None
            if not chunk:
                raise PushError("handshake refused: connection closed")
            head += chunk
        head, _, self._buf = head.partition(b"\r\n\r\n")
        lines = head.decode("latin-1").split("\r\n")
        headers = {name.strip().lower(): value.strip()
                   for name, value in (line.split(":", 1) for line in lines[1:] if ":" in line)}
        expected = base64.b64encode(hashlib.sha1((key + _GUID).encode("ascii")).digest()).decode("ascii")
        status = lines[0].split(" ")
        if len(status) < 2 or status[1] != "101" or headers.get("sec-websocket-accept") != expected:
            raise PushError(f"handshake refused: {lines[0][:80]}")

    def _send_frame(self, opcode: int, payload: bytes, timeout: float = REPLY_WAIT) -> None:
        mask, size = os.urandom(4), len(payload)
        head = bytes([0x80 | opcode])
        if size < 126:
            head += bytes([0x80 | size])
        elif size < 65536:
            head += bytes([0x80 | 126]) + struct.pack(">H", size)
        else:
            head += bytes([0x80 | 127]) + struct.pack(">Q", size)
        self._sock.settimeout(timeout)
        self._sock.sendall(head + mask + bytes(byte ^ mask[i % 4] for i, byte in enumerate(payload)))

    def send(self, text: str) -> None:
        self._send_frame(0x1, text.encode("utf-8"))

    def _take_frame(self):
        """One complete frame from the buffer, or None while bytes are still missing."""
        buf = self._buf
        if len(buf) < 2:
            return None
        first, second = buf[0], buf[1]
        if first & 0x70:
            raise PushError("frame uses an extension that was not negotiated")
        if second & 0x80:
            raise PushError("masked frame from the server")
        size, offset = second & 0x7F, 2
        if size == 126:
            if len(buf) < 4:
                return None
            size, offset = struct.unpack(">H", buf[2:4])[0], 4
        elif size == 127:
            if len(buf) < 10:
                return None
            size, offset = struct.unpack(">Q", buf[2:10])[0], 10
        if size > MAX_MESSAGE:
            raise PushError("frame too large")
        if len(buf) < offset + size:
            return None
        self._buf = buf[offset + size:]
        return bool(first & 0x80), first & 0x0F, buf[offset:offset + size]

    def recv(self, timeout: float) -> str:
        """The next text message. Raises socket.timeout when none completes in time; whatever
        arrived so far is kept, so the same call can simply be made again."""
        deadline = time.monotonic() + timeout
        while True:
            frame = self._take_frame()
            if frame is None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise socket.timeout("no message")
                self._sock.settimeout(remaining)
                chunk = self._sock.recv(65536)
                if not chunk:
                    raise PushError("connection closed by the server")
                self._buf += chunk
                continue
            final, opcode, payload = frame
            if opcode == 0x9:
                self._send_frame(0xA, payload)
            elif opcode == 0xA:
                pass
            elif opcode == 0x8:
                raise PushError("connection closed by the server")
            elif opcode in (0x0, 0x1, 0x2):
                if (opcode == 0x0) != bool(self._parts):
                    raise PushError("message fragments out of order")
                self._parts.append(payload)
                self._size += len(payload)
                if self._size > MAX_MESSAGE:
                    raise PushError("message too large")
                if final:
                    data, self._parts, self._size = b"".join(self._parts), [], 0
                    try:
                        return data.decode("utf-8")
                    except UnicodeDecodeError:
                        raise PushError("message is not UTF-8") from None
            else:
                raise PushError("unknown frame type")

    def close(self) -> None:
        try:
            self._send_frame(0x8, struct.pack(">H", 1000), timeout=2.0)
        except (OSError, ValueError):
            pass
        try:
            self._sock.close()
        except OSError:
            pass


# ----- the listener --------------------------------------------------------------

class Listener:
    """Keeps this host registered with the push service and calls ``on_ring`` for every push.

    The identity Mozilla gave this host (uaid, channel, endpoint) is kept in the local store, so a
    reconnect keeps the same endpoint and nothing has to be published again."""

    def __init__(self, *, me: str, secret: bytes, store, directory, on_ring, connect=None,
                 clock=time.time, new_id=None):
        self._me, self._secret, self._store, self._directory = me, secret, store, directory
        self._on_ring = on_ring
        self._connect = connect or WebSocket.connect
        self._clock = clock
        self._new_id = new_id or (lambda: str(uuid.uuid4()))

    def session(self, stop) -> None:
        """One connection, from hello until it fails or ``stop`` is set."""
        wire = self._connect()
        try:
            self._serve(wire, stop)
        finally:
            wire.close()

    def _serve(self, wire, stop) -> None:
        store = self._store
        uaid, channel, endpoint = (store.get_meta(key) for key in ("push_uaid", "push_channel", "push_endpoint"))
        known = bool(uaid and channel and endpoint)
        hello = {"messageType": "hello", "use_webpush": True, "channelIDs": [channel] if known else []}
        if known:
            hello["uaid"] = uaid
        wire.send(json.dumps(hello))
        reply = self._await(wire, "hello")
        granted = reply.get("uaid")
        if reply.get("status") != 200 or not isinstance(granted, str) or not granted:
            raise PushError("hello refused")
        fresh = not known or granted != uaid
        if fresh:  # first contact, or the service no longer knows the old identity
            channel = self._new_id()
            wire.send(json.dumps({"messageType": "register", "channelID": channel}))
            reply = self._await(wire, "register")
            endpoint = reply.get("pushEndpoint")
            if reply.get("status") != 200 or reply.get("channelID") != channel or not valid_endpoint(endpoint):
                raise PushError("register refused, or the endpoint is not a Mozilla push endpoint")
            for key, value in (("push_uaid", granted), ("push_channel", channel), ("push_endpoint", endpoint)):
                store.set_meta(key, value)
        self._publish(endpoint)
        store.set_meta("push_state", f"connected since {int(self._clock())}")
        log.info("push: connected (%s endpoint)", "new" if fresh else "same")
        while not stop.is_set():
            try:
                raw = wire.recv(PING_AFTER)
            except socket.timeout:
                wire.send("{}")  # the service answers a ping with {}; silence means the line is dead
                raw = wire.recv(PONG_WAIT)
            self._handle(wire, _parse(raw))

    def _publish(self, endpoint: str) -> None:
        meta = self._directory.peer_metadata().get(self._me)
        published = endpoint_from(self._secret, self._me, meta.get(META_KEY) if isinstance(meta, dict) else None)
        if published != endpoint:
            self._directory.set_peer_metadata(self._me, META_KEY,
                                              record(self._secret, self._me, endpoint, self._clock()))

    def _await(self, wire, kind: str) -> dict:
        for _ in range(50):
            message = _parse(wire.recv(REPLY_WAIT))
            if message.get("messageType") == kind:
                return message
            self._handle(wire, message)  # a push stored while this host was away may come first
        raise PushError(f"no {kind} reply")

    def _handle(self, wire, message: dict) -> None:
        if message.get("messageType") != "notification":
            return
        channel, version = message.get("channelID"), message.get("version")
        if not isinstance(channel, str) or not isinstance(version, str):
            return
        wire.send(json.dumps({"messageType": "ack",
                              "updates": [{"channelID": channel, "version": version, "code": 100}]}))
        self._on_ring()

    def run(self, stop, wait=None) -> None:
        """Reconnect for as long as the poller runs. Never raises."""
        wait = wait or stop.wait
        delay, failures = MIN_BACKOFF, 0
        while not stop.is_set():
            started = self._clock()
            exc_obj: Exception | None = None
            try:
                self.session(stop)
                reason = "stopped"
            except Exception as exc:
                exc_obj = exc
                reason = exc.__class__.__name__ + (f": {exc}" if isinstance(exc, PushError) else "")
            if stop.is_set():
                break
            duration = self._clock() - started
            if duration >= HEALTHY:
                delay, failures = MIN_BACKOFF, 0
                if isinstance(exc_obj, PushError) and "connection closed by the server" in str(exc_obj):
                    log.info("push: connection rotated by server after %.0fs; reconnecting in %.0fs",
                             duration, ROTATE_BACKOFF)
                    wait(ROTATE_BACKOFF)
                    continue
            failures += 1
            try:
                self._store.set_meta("push_state", f"down since {int(self._clock())}: {reason}"[:200])
            except Exception:
                pass
            if failures in (1, 5) or failures % 50 == 0:
                log.warning("push: not connected (%s); retrying in %.0fs, the mailbox is still read on the timer",
                            reason, delay)
            wait(delay)
            delay = min(MAX_BACKOFF, delay * 2)


def _parse(raw: str) -> dict:
    try:
        message = json.loads(raw)
    except ValueError:
        raise PushError("push service sent something that is not JSON") from None
    if not isinstance(message, dict):
        raise PushError("push service sent something that is not an object")
    return message
