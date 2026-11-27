"""The shared Honcho session used as the geminise mailbox.

Honcho is the record. It does not push, so every host reads this session on a
timer. The workspace token proves nothing about which peer wrote a message
(Honcho 3.6 has no per-peer credential), so each message also carries an HMAC
made with a secret that never leaves the member hosts; see ``sign``.
"""
from __future__ import annotations

import hashlib
import hmac
import json
import urllib.error
import urllib.parse
import urllib.request

from . import e2ee

USER_AGENT = "geminish/0.5"
CHAR_CAP = 25_000
BYTE_CAP = 25_000
PEER_PAGE_SIZE = 100
PEER_PAGES = 20


class MailboxError(RuntimeError):
    """A Honcho call failed. ``ambiguous`` means the write may have been stored."""

    def __init__(self, message: str, *, ambiguous: bool = False):
        super().__init__(message)
        self.ambiguous = ambiguous


def sign(secret: bytes, wire: str, provenance: str, sent_at: int) -> str:
    """HMAC-SHA256 over the exact stored content, its provenance label and its send time.
    Each field is hashed on its own first, so no field can be shifted into its neighbour."""
    body = (hashlib.sha256(wire.encode("utf-8")).digest()
            + hashlib.sha256(provenance.encode("utf-8")).digest()
            + str(int(sent_at)).encode("ascii"))
    return hmac.new(secret, body, hashlib.sha256).hexdigest()


def verify(secret: bytes, wire: str, provenance: str, sent_at: int, signature: object) -> bool:
    if not isinstance(signature, str):
        return False
    expected = sign(secret, wire, provenance, sent_at).encode("ascii")
    return hmac.compare_digest(expected, signature.encode("utf-8", "replace"))


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):  # a redirect would resend the bearer token elsewhere
        return None


class Mailbox:
    def __init__(self, base_url: str, api_key: str, workspace: str, session: str, *,
                 timeout: float = 20.0, opener=None, e2ee_url: str = "", e2ee_secret: bytes = b""):
        self._base = base_url.rstrip("/")
        self._key = api_key
        self._root = f"/v3/workspaces/{urllib.parse.quote(workspace, safe='')}"
        self._session = urllib.parse.quote(session, safe="")
        self._timeout = timeout
        self._opener = opener or urllib.request.build_opener(_NoRedirect)
        self._e2ee_url = e2ee_url.strip()
        self._e2ee_secret = bytes(e2ee_secret)

    def _call(self, method: str, path: str, body: object = None, *, write: bool = False,
              timeout: float | None = None):
        if self._e2ee_url:
            return self._call_e2ee(method, path, body, write=write, timeout=timeout)
        headers = {"Content-Type": "application/json", "User-Agent": USER_AGENT}
        if self._key:
            headers["Authorization"] = f"Bearer {self._key}"
        data = json.dumps(body).encode("utf-8") if body is not None else None
        request = urllib.request.Request(self._base + path, data=data, method=method, headers=headers)
        try:
            with self._opener.open(request, timeout=timeout or self._timeout) as response:
                payload = response.read()
        except urllib.error.HTTPError as exc:
            # The server answered: a 4xx/5xx is a definite refusal, not an unknown outcome.
            raise MailboxError(f"Honcho {method} answered HTTP {exc.code}") from None
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise MailboxError(f"Honcho {method} failed: {exc.__class__.__name__}", ambiguous=write) from None
        try:
            return json.loads(payload) if payload else None
        except ValueError:
            raise MailboxError(f"Honcho {method} returned non-JSON", ambiguous=write) from None

    def _call_e2ee(self, method: str, path: str, body: object = None, *, write: bool = False,
                   timeout: float | None = None):
        headers = {"Content-Type": "application/json", "User-Agent": USER_AGENT}
        if self._key:
            headers["Authorization"] = f"Bearer {self._key}"
        data = json.dumps(body).encode("utf-8") if body is not None else None
        parts = urllib.parse.urlsplit(path)
        req_path = parts.path if parts.path.startswith("/") else "/" + parts.path
        try:
            frame, ts, salt = e2ee.seal_request(
                self._e2ee_secret, method, req_path, parts.query, headers, data,
            )
        except e2ee.E2EEError as exc:
            raise MailboxError(f"Honcho E2EE seal failed: {exc}") from None
        outer = urllib.request.Request(
            self._e2ee_url, data=frame, method="POST",
            headers={"Content-Type": e2ee.CONTENT_TYPE, "User-Agent": USER_AGENT},
        )
        try:
            with self._opener.open(outer, timeout=timeout or self._timeout) as response:
                out_frame = response.read()
        except urllib.error.HTTPError as exc:
            raise MailboxError(f"Honcho E2EE proxy answered HTTP {exc.code}") from None
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise MailboxError(f"Honcho E2EE {method} failed: {exc.__class__.__name__}", ambiguous=write) from None
        try:
            status, _res_headers, payload = e2ee.open_response(self._e2ee_secret, out_frame, ts, salt)
        except e2ee.E2EEError:
            raise MailboxError(f"Honcho E2EE {method} response failed verification", ambiguous=write) from None
        if status < 200 or status > 299:
            raise MailboxError(f"Honcho {method} answered HTTP {status}")
        try:
            return json.loads(payload) if payload else None
        except ValueError:
            raise MailboxError(f"Honcho {method} returned non-JSON", ambiguous=write) from None

    def request(self, method: str, path: str, body: object = None, *, write: bool = False):
        """One call on a path below this workspace, for records that are not in the mailbox session."""
        return self._call(method, self._root + path, body, write=write)

    def ensure_session(self, peers: tuple[str, ...]) -> None:
        """Get or create the mailbox session. Members are not observed: mail is not memory."""
        self._call("POST", f"{self._root}/sessions", {
            "id": urllib.parse.unquote(self._session),
            "metadata": {"kind": "geminise"},
            "peers": {peer: {"observe_me": False, "observe_others": False} for peer in peers},
        })

    def post(self, peer_id: str, content: str, metadata: dict) -> dict:
        """Store one message as ``peer_id`` and confirm Honcho kept it unchanged."""
        if len(content) > CHAR_CAP or len(content.encode("utf-8")) > BYTE_CAP:
            raise MailboxError("message exceeds the Honcho content cap")
        stored = self._call("POST", f"{self._root}/sessions/{self._session}/messages", {
            "messages": [{"peer_id": peer_id, "content": content, "metadata": metadata}],
        }, write=True)
        if (not isinstance(stored, list) or len(stored) != 1 or not isinstance(stored[0], dict)
                or stored[0].get("peer_id") != peer_id or stored[0].get("content") != content):
            raise MailboxError("Honcho did not echo the message as written", ambiguous=True)
        return stored[0]

    def recent(self, *, page: int = 1, size: int = 50) -> list[dict]:
        """Newest first. An empty list also means "no such page"."""
        result = self._call(
            "POST", f"{self._root}/sessions/{self._session}/messages/list?page={page}&size={size}&reverse=true", {})
        items = result.get("items") if isinstance(result, dict) else None
        if not isinstance(items, list):
            raise MailboxError("Honcho list response has no items")
        return [item for item in items if isinstance(item, dict)]

    def peer_metadata(self, timeout: float | None = None) -> dict[str, dict]:
        """Metadata of every peer in the workspace, by peer id."""
        extra = {} if timeout is None else {"timeout": timeout}
        found: dict[str, dict] = {}
        for page in range(1, PEER_PAGES + 1):
            result = self._call("POST", f"{self._root}/peers/list?page={page}&size={PEER_PAGE_SIZE}", {}, **extra)
            items = result.get("items") if isinstance(result, dict) else None
            if not isinstance(items, list):
                raise MailboxError("Honcho peer list has no items")
            for item in items:
                if isinstance(item, dict) and item.get("id"):
                    found[str(item["id"])] = item["metadata"] if isinstance(item.get("metadata"), dict) else {}
            pages = result.get("pages")
            if len(items) < PEER_PAGE_SIZE or (isinstance(pages, int) and page >= pages):
                break
        return found

    def set_peer_metadata(self, peer_id: str, key: str, value: object) -> None:
        """Set one key in a peer's metadata and keep the others. A host only writes its own peer.

        A peer missing from the directory is refused: writing without its current metadata
        would erase whatever else is stored on it."""
        directory = self.peer_metadata()
        if peer_id not in directory:
            raise MailboxError(f"peer {peer_id} not found in the workspace; nothing written")
        self._call("PUT", f"{self._root}/peers/{urllib.parse.quote(peer_id, safe='')}",
                   {"metadata": {**directory[peer_id], key: value}}, write=True)
