"""C2–C5 pure membership, visibility and ordered wake policy.

Inputs are authenticated facts supplied by an adapter, not assertions trusted
from an envelope. This module neither admits turns nor creates notifications.
"""
from __future__ import annotations

from dataclasses import dataclass
import math
import re

from .protocol import Envelope

_PEER = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]*", re.ASCII)
HOP_CEILING = 8  # the deepest wake chain any host accepts or signs, whatever its own setting says


class PolicyError(ValueError):
    """Rejected policy input; no message or credential data in diagnostics."""


@dataclass(frozen=True)
class Member:
    peer: str
    kind: str
    status: str = "active"

    def __post_init__(self):
        if (not isinstance(self.peer, str) or _PEER.fullmatch(self.peer) is None
                or self.peer == "all" or self.kind not in ("agent", "human")
                or self.status not in ("active", "revoked", "unknown")):
            raise PolicyError("invalid membership binding")


@dataclass(frozen=True)
class Membership:
    members: tuple[Member, ...]

    def __post_init__(self):
        if (not isinstance(self.members, tuple)
                or any(not isinstance(m, Member) for m in self.members)
                or len({m.peer for m in self.members}) != len(self.members)):
            raise PolicyError("explicit unique membership required")

    def get(self, peer: str) -> Member | None:
        return next((m for m in self.members if m.peer == peer), None)

    def active(self, peer: str) -> bool:
        member = self.get(peer)
        return member is not None and member.status == "active"

    @property
    def authorized(self) -> tuple[str, ...]:
        return tuple(m.peer for m in self.members if m.status == "active")


def recipients(value: str, membership: Membership) -> tuple[str, ...]:
    """Strict C2 grammar layered over the more permissive protocol syntax."""
    if not isinstance(value, str) or not value:
        raise PolicyError("invalid recipient field")
    if value in ("@all", "all"):
        if not membership.authorized:
            raise PolicyError("no authorized recipients")
        return membership.authorized
    result = []
    for token in value.split(","):
        peer = token[1:] if token.startswith("@") else token
        if (_PEER.fullmatch(peer) is None or peer == "all"
                or not membership.active(peer)):
            raise PolicyError("invalid or unauthorized recipient")
        if peer not in result:
            result.append(peer)
    return tuple(result)


@dataclass(frozen=True)
class WakeContext:
    authenticated_author: str
    binding_valid: bool
    channel_valid: bool
    canonical_complete: bool
    session_status: str
    prior_admission: str
    provenance: str  # initiated, agent_reply, unknown; follows response across ids/refs
    human_route_authorized: bool = False
    hop: int | None = None  # turns geminise started between a turn it did not start and this mail
    max_hops: int = 0       # this host's budget: the deepest reply that may still start a turn here


@dataclass(frozen=True)
class Decision:
    readable: bool
    action: str  # none, wake, notice, digest
    reason: str


def decide(envelope: Envelope, recipient: str, membership: Membership,
           context: WakeContext) -> Decision:
    meta = dict(envelope.metadata)
    author = meta["from"]
    author = author[1:] if author.startswith("@") else author
    # Guard 1 precedes every other guard, including dedupe and notify selection.
    if (context.binding_valid is not True or context.channel_valid is not True
            or context.canonical_complete is not True
            or author != context.authenticated_author
            or not membership.active(author) or not membership.active(recipient)):
        return Decision(False, "none", "authorization")
    try:
        mentioned = recipients(meta["to"], membership)
    except PolicyError:
        return Decision(True, "none", "recipients")
    # Guard 2. Unknown admission is never a new message opportunity.
    if recipient == author:
        return Decision(True, "none", "self")
    if context.session_status != "open" or meta["status"] != "open":
        return Decision(True, "none", "closed_or_unknown_session")
    if context.prior_admission != "fresh":
        return Decision(True, "none", "prior_admission")
    # Guard 3. Provenance must survive follow-up id/ref changes. A reply starts a turn only while
    # its hop count is inside this host's budget; the count follows the chain, never the ref.
    if context.provenance != "initiated" and not _in_budget(context):
        return Decision(True, "none", "reply_chain_or_unknown")
    # Guard 4. Visibility does not depend on notify or explicit mentions.
    notify = meta["notify"]
    if notify == "digest":
        return Decision(True, "digest", "next_existing_turn")
    if notify not in ("mention", "all"):
        return Decision(True, "none", "unknown_notify")
    if notify == "mention" and recipient not in mentioned:
        return Decision(True, "none", "not_mentioned")
    member = membership.get(recipient)
    if member is not None and member.kind == "human":
        if context.human_route_authorized is True:
            return Decision(True, "notice", "identifier_only")
        return Decision(True, "none", "human_route_unconfigured")
    return Decision(True, "wake", "eligible")


def _in_budget(context: WakeContext) -> bool:
    """A missing, malformed or over-budget hop is "unknown", and unknown never starts a turn."""
    hop, budget = context.hop, context.max_hops
    return (context.provenance == "agent_reply" and type(hop) is int and type(budget) is int
            and 1 <= hop <= min(budget, HOP_CEILING))


def retry_at(*, outcome: str, attempts: int, now: float, expires_at: float | None,
             delay: float | None, retry_after: float | None = None,
             prewrite_proven: bool = False) -> float | None:
    """Validate an explicit sampled backoff, not choose a jitter distribution.

    delay comes from the caller's configured jittered exponential scheduler.
    No missing timing/TTL or ambiguous result can authorize another attempt.
    retry_after is an already parsed duration (seconds or HTTP-date adapter).
    """
    def finite(value):
        return type(value) in (int, float) and math.isfinite(value)

    if (type(attempts) is not int or not 1 <= attempts < 3
            or not finite(now) or expires_at is None or not finite(expires_at)
            or delay is None or not finite(delay) or not 0 <= delay <= 600):
        return None
    if outcome not in ("push_5xx", "push_429", "prewrite_failure"):
        return None
    if outcome == "prewrite_failure" and prewrite_proven is not True:
        return None
    if outcome == "push_429":
        if retry_after is None or not finite(retry_after) or retry_after < 0:
            return None
        delay = max(delay, retry_after)
    if delay > 600 or now + delay >= expires_at:
        return None
    return now + delay
