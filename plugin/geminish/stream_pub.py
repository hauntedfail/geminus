"""Ephemeral GE2E v1 real-time stream publisher for the geminus bridge.

Stdlib-only micro-batching publisher that signs each stream event with the
group HMAC secret and posts encrypted GE2E v1 frames to ``/_stream/publish``.
Failures never block or fail the agent turn; the bridge falls back cleanly to
standard Honcho output flushing.
"""
from __future__ import annotations

import hashlib
import json
import logging
import threading
import time
import urllib.error
import urllib.request

from . import e2ee
from .mailbox import USER_AGENT, _NoRedirect, sign

log = logging.getLogger("geminish.stream_pub")

STREAM_PATH_PUBLISH = "/_stream/publish"
KIND_DELTA = "delta"
KIND_TOOL_USE = "tool_use"
KIND_TOOL_RESULT = "tool_result"
KIND_DONE = "done"

DEFAULT_COALESCE_SECONDS = 0.05
DEFAULT_MAX_BATCH_CHARS = 1024
DEFAULT_TIMEOUT = 5.0
MAX_PENDING_EVENTS = 256


def _sha256_hex(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def stream_event_label(
    session: str,
    turn: str,
    seq: int,
    kind: str,
    tool: str = "",
    detail: str = "",
    ok: bool = False,
) -> str:
    """Domain-separated provenance label matching Go's e2ee.StreamEventLabel."""
    return (
        f"geminus-stream:{session}:{turn}:{int(seq)}:{kind}:"
        f"{_sha256_hex(tool)}:{_sha256_hex(detail)}:{int(bool(ok))}"
    )


def sign_stream_event(
    secret: bytes,
    *,
    session: str,
    turn: str,
    seq: int,
    kind: str,
    text: str = "",
    tool: str = "",
    detail: str = "",
    ok: bool = False,
    ts: int,
) -> str:
    """Compute the hex HMAC-SHA256 bridge signature matching Go's e2ee.SignStreamEvent."""
    label = stream_event_label(session, turn, seq, kind, tool=tool, detail=detail, ok=ok)
    return sign(secret, text, label, int(ts))


class StreamPublisher:
    """Async micro-batching publisher for one geminus turn over GE2E v1."""

    def __init__(
        self,
        e2ee_url: str,
        secret: bytes,
        session: str,
        turn: str,
        *,
        opener=None,
        clock=time.time,
        coalesce_s: float = DEFAULT_COALESCE_SECONDS,
        max_batch_chars: int = DEFAULT_MAX_BATCH_CHARS,
        timeout: float = DEFAULT_TIMEOUT,
    ):
        self._url = e2ee_url.strip()
        self._secret = bytes(secret)
        self._session = session
        self._turn = turn
        self._opener = opener or urllib.request.build_opener(_NoRedirect)
        self._clock = clock
        self._coalesce_s = max(0.0, float(coalesce_s))
        self._max_batch_chars = max(64, int(max_batch_chars))
        self._timeout = float(timeout)

        self._lock = threading.Lock()
        self._cond = threading.Condition(self._lock)
        self._seq = 0
        self._delta_buf: list[str] = []
        self._delta_chars = 0
        self._pending: list[dict] = []
        self._closed = False
        self._disabled = not bool(self._url and len(self._secret) >= e2ee.MIN_SECRET_BYTES)
        self._consecutive_errors = 0
        self.published_any = False

        self._worker: threading.Thread | None = None
        if not self._disabled:
            self._worker = threading.Thread(target=self._run, name=f"geminus-stream-{turn[:8]}", daemon=True)
            self._worker.start()

    @property
    def disabled(self) -> bool:
        with self._lock:
            return self._disabled

    def _make_event_locked(self, kind: str, *, text: str = "", tool: str = "", detail: str = "", ok: bool = False) -> dict:
        self._seq += 1
        ts = int(self._clock())
        sig = sign_stream_event(
            self._secret,
            session=self._session,
            turn=self._turn,
            seq=self._seq,
            kind=kind,
            text=text,
            tool=tool,
            detail=detail,
            ok=ok,
            ts=ts,
        )
        ev: dict = {
            "session": self._session,
            "turn": self._turn,
            "seq": self._seq,
            "kind": kind,
            "ts": ts,
            "sig": sig,
        }
        if text:
            ev["text"] = text
        if tool:
            ev["tool"] = tool
        if detail:
            ev["detail"] = detail
        if ok:
            ev["ok"] = True
        return ev

    def _flush_delta_locked(self) -> None:
        if not self._delta_buf:
            return
        joined = "".join(self._delta_buf)
        self._delta_buf.clear()
        self._delta_chars = 0
        if len(self._pending) < MAX_PENDING_EVENTS:
            self._pending.append(self._make_event_locked(KIND_DELTA, text=joined))

    def emit_delta(self, text: str) -> None:
        if not text:
            return
        with self._cond:
            if self._disabled or self._closed:
                return
            self._delta_buf.append(text)
            self._delta_chars += len(text)
            if self._delta_chars >= self._max_batch_chars:
                self._flush_delta_locked()
            self._cond.notify()

    def emit_tool_use(self, tool: str, detail: str = "") -> None:
        with self._cond:
            if self._disabled or self._closed:
                return
            self._flush_delta_locked()
            if len(self._pending) < MAX_PENDING_EVENTS:
                self._pending.append(self._make_event_locked(KIND_TOOL_USE, tool=tool, detail=detail))
            self._cond.notify()

    def emit_tool_result(self, tool: str, detail: str = "", ok: bool = True) -> None:
        with self._cond:
            if self._disabled or self._closed:
                return
            self._flush_delta_locked()
            if len(self._pending) < MAX_PENDING_EVENTS:
                self._pending.append(self._make_event_locked(KIND_TOOL_RESULT, tool=tool, detail=detail, ok=ok))
            self._cond.notify()

    def close(self, *, ok: bool = True, join_timeout: float = 3.0) -> None:
        with self._cond:
            if self._closed:
                return
            self._closed = True
            if not self._disabled:
                self._flush_delta_locked()
                self._pending.append(self._make_event_locked(KIND_DONE, ok=ok))
            self._cond.notify_all()
        if self._worker is not None:
            self._worker.join(join_timeout)

    def _run(self) -> None:
        while True:
            batch: list[dict] = []
            with self._cond:
                while not self._pending and not self._delta_buf and not self._closed and not self._disabled:
                    self._cond.wait(timeout=0.5)
                if self._disabled:
                    return
                if self._delta_buf and not self._pending and not self._closed and self._coalesce_s > 0:
                    self._cond.wait(timeout=self._coalesce_s)
                if self._disabled:
                    return
                self._flush_delta_locked()
                if self._pending:
                    batch = self._pending[:32]
                    del self._pending[:32]
                elif self._closed:
                    return
            if batch:
                self._post_batch(batch)

    def _post_batch(self, events: list[dict]) -> None:
        body = json.dumps({"events": events}, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        try:
            frame, ts, salt = e2ee.seal_request(
                self._secret,
                "POST",
                STREAM_PATH_PUBLISH,
                "",
                {"Content-Type": "application/json", "User-Agent": USER_AGENT},
                body,
                ts=int(self._clock()),
            )
            req = urllib.request.Request(
                self._url,
                data=frame,
                method="POST",
                headers={"Content-Type": e2ee.CONTENT_TYPE, "User-Agent": USER_AGENT},
            )
            with self._opener.open(req, timeout=self._timeout) as resp:
                out_frame = resp.read()
            status, _hdrs, _payload = e2ee.open_response(self._secret, out_frame, ts, salt)
            if 200 <= status <= 299:
                with self._lock:
                    self._consecutive_errors = 0
                    self.published_any = True
                return
            self._record_failure(f"inner HTTP {status}", fatal=status in (400, 401, 404))
        except urllib.error.HTTPError as exc:
            self._record_failure(f"HTTP {exc.code}", fatal=exc.code in (400, 401, 404))
        except (urllib.error.URLError, TimeoutError, OSError, e2ee.E2EEError) as exc:
            self._record_failure(exc.__class__.__name__, fatal=False)

    def _record_failure(self, reason: str, *, fatal: bool) -> None:
        with self._lock:
            self._consecutive_errors += 1
            if fatal or self._consecutive_errors >= 3:
                self._disabled = True
                log.debug("geminus: ephemeral stream disabled (%s); falling back to Honcho", reason)
