"""Pure geminise v1 values and character-capped wire operations.

No identities are generated and no membership or delivery policy is applied.
"""
from __future__ import annotations

from collections.abc import Iterable, Mapping
from dataclasses import dataclass, replace
import re

HEADER = "[geminise v1]"
FIELDS = ("id", "from", "to", "kind", "stage", "expects_reply", "notify",
          "status", "session", "ref")
MAX_PARTS = 999
_TOKEN = re.compile(r"@?[A-Za-z0-9][A-Za-z0-9_.-]*", re.ASCII)
_PART = re.compile(r"([1-9][0-9]{0,2})/([1-9][0-9]{0,2})", re.ASCII)


class ProtocolError(ValueError):
    """Invalid envelope, cap, or incomplete/conflicting logical message."""


def parse_addresses(value: str) -> tuple[str, ...]:
    """Parse bare/@ tokens; spaces after commas are separators, not repairs.

    Preserve token spelling and @ prefixes. 'all' is just a token here.
    """
    if not isinstance(value, str):
        raise ProtocolError("addresses must be a string")
    tokens = tuple(value.split(", ")) if ", " in value else (value,)
    tokens = tuple(token for group in tokens for token in group.split(","))
    if not tokens or any(_TOKEN.fullmatch(token) is None for token in tokens):
        raise ProtocolError("invalid address token")
    return tokens


@dataclass(frozen=True)
class Envelope:
    """Immutable ordered metadata and an optional explicit multipart identity."""

    metadata: tuple[tuple[str, str], ...]
    text: str
    part: int | None = None
    total: int | None = None

    def __post_init__(self) -> None:
        # Require an immutable canonical representation, including on direct use.
        if not isinstance(self.metadata, tuple) or any(
            not isinstance(item, tuple) or len(item) != 2 for item in self.metadata
        ):
            raise ProtocolError("metadata must be a tuple of field/value pairs")
        if tuple(key for key, _ in self.metadata) != FIELDS:
            raise ProtocolError("missing, duplicate, unknown, or unordered header fields")
        for key, value in self.metadata:
            if not isinstance(value, str) or "\r" in value or "\n" in value:
                raise ProtocolError(f"invalid header value: {key}")
            try:
                value.encode("utf-8")
            except UnicodeEncodeError as exc:
                raise ProtocolError(f"invalid UTF-8 header: {key}") from exc
        meta = dict(self.metadata)
        if not meta["id"] or not meta["ref"]:
            raise ProtocolError("id and ref must not be empty")
        if len(parse_addresses(meta["from"])) != 1:
            raise ProtocolError("from requires one address")
        parse_addresses(meta["to"])
        if not isinstance(self.text, str):
            raise ProtocolError("text must be a string")
        try:
            self.text.encode("utf-8")
        except UnicodeEncodeError as exc:
            raise ProtocolError("text must be UTF-8 encodable") from exc
        if self.part is None and self.total is None:
            return
        if (type(self.part) is not int or type(self.total) is not int
                or not 1 <= self.part <= self.total <= MAX_PARTS):
            raise ProtocolError("part must satisfy 1 <= part <= total <= 999")

    @property
    def id(self) -> str:
        return dict(self.metadata)["id"]

    @property
    def ref(self) -> str:
        return dict(self.metadata)["ref"]

    @property
    def recipients(self) -> tuple[str, ...]:
        return parse_addresses(dict(self.metadata)["to"])


def build_envelope(meta: Mapping[str, str], text: str, part: int | None = None,
                   total: int | None = None) -> Envelope:
    """Construct without defaults, randomness, normalization, or I/O."""
    if set(meta) != set(FIELDS):
        raise ProtocolError("metadata must contain exactly the v1 fields")
    return Envelope(tuple((key, meta[key]) for key in FIELDS), text, part, total)


def serialize(envelope: Envelope) -> str:
    """Emit the installed builder's field order and literal text delimiter."""
    lines = [HEADER, *(f"{key}: {value}" for key, value in envelope.metadata)]
    if envelope.part is not None:
        lines.append(f"part: {envelope.part}/{envelope.total}")
    return "\n".join([*lines, "text:", envelope.text])


def parse(wire: str) -> Envelope:
    """Read only header lines; the entire remainder after text:\n is opaque."""
    if not isinstance(wire, str):
        raise ProtocolError("wire must be a string")
    header, separator, text = wire.partition("\ntext:\n")
    if not separator:
        raise ProtocolError("missing literal text delimiter")
    lines = header.split("\n")
    if not lines or lines[0] != HEADER:
        raise ProtocolError("invalid v1 header")
    meta: dict[str, str] = {}
    part = total = None
    seen: set[str] = set()
    for line in lines[1:]:
        key, sep, value = line.partition(": ")
        if not sep or key in seen or key not in (*FIELDS, "part"):
            raise ProtocolError("invalid or duplicate header field")
        seen.add(key)
        if key == "part":
            match = _PART.fullmatch(value)
            if match is None:
                raise ProtocolError("invalid part header")
            part, total = map(int, match.groups())
        else:
            meta[key] = value
    return build_envelope(meta, text, part, total)


def _positive_limit(limit: int) -> None:
    if type(limit) is not int or limit <= 0:
        raise ProtocolError("limit must be a positive integer")


def split_text(text: str, limit: int) -> list[str]:
    """Lossless character splitting, preferring the last available newline."""
    _positive_limit(limit)
    if not isinstance(text, str):
        raise ProtocolError("text must be a string")
    result: list[str] = []
    start = 0
    while start < len(text):
        end = min(start + limit, len(text))
        if end < len(text):
            newline = text.rfind("\n", start, end)
            if newline >= start:
                end = newline + 1
        result.append(text[start:end])
        if len(result) > MAX_PARTS:
            raise ProtocolError("message requires more than 999 parts")
        start = end
    return result or [""]


def split(envelope: Envelope, limit: int = 25000) -> tuple[Envelope, ...]:
    """Cap the full serialized envelope in Unicode characters, as legacy v1.

    Try each possible part-count width, accounting for actual header overhead.
    The bounded search never truncates and returns all parts or raises.
    """
    _positive_limit(limit)
    if envelope.part is not None:
        raise ProtocolError("cannot split an already numbered part")
    if len(serialize(envelope)) <= limit:
        return (envelope,)
    for widest in (9, 99, 999):
        overhead = len(serialize(replace(envelope, text="", part=widest, total=widest)))
        capacity = limit - overhead
        if capacity <= 0:
            raise ProtocolError("headers leave no text capacity within limit")
        chunks = split_text(envelope.text, capacity)
        if len(chunks) <= widest:
            return tuple(replace(envelope, text=text, part=i, total=len(chunks))
                         for i, text in enumerate(chunks, 1))
    raise ProtocolError("message requires more than 999 parts")


def reassemble(parts: Iterable[Envelope]) -> Envelope:
    """Return a complete logical value; missing/duplicate/conflicting parts fail."""
    values = tuple(parts)
    if not values:
        raise ProtocolError("no parts")
    first = values[0]
    if first.part is None:
        if len(values) != 1:
            raise ProtocolError("multiple unnumbered messages")
        return first
    assert first.total is not None  # Enforced by Envelope validation.
    indexed: dict[int, Envelope] = {}
    for value in values:
        if value.metadata != first.metadata or value.total != first.total:
            raise ProtocolError("conflicting logical metadata or part totals")
        if value.part is None or value.part in indexed:
            raise ProtocolError("unnumbered or duplicate part")
        indexed[value.part] = value
    if set(indexed) != set(range(1, first.total + 1)):
        raise ProtocolError("incomplete logical message")
    return replace(first, text="".join(indexed[i].text for i in range(1, first.total + 1)),
                   part=None, total=None)
