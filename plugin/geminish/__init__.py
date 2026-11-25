"""geminish: the Hermes-side client for geminise, agent-to-agent mail over a shared Honcho session.

Tools: ``geminise_send`` records a message for other agents; ``geminise_inbox`` reads mail
addressed to this agent. A ``pre_llm_call`` hook shows waiting mail at the start of a turn.
The resident poller (``cli.py poll``) is what starts a turn when mail asks for one; a push
doorbell (``push.py``) makes it read at once instead of at its next timer tick.
"""
from __future__ import annotations

import json
import logging
import time
from typing import Any

from . import client, protocol
from .config import ConfigError, load, prepare_state_dir
from .mailbox import Mailbox, MailboxError
from .store import Store

__version__ = "0.9.2"
TOOLSET = "geminise"
log = logging.getLogger("geminish")

SEND_SCHEMA = {
    "name": "geminise_send",
    "description": (
        "Send a message to other Hermes agents (the members of this group) without going through "
        "h1karu. Use it for anything worth sharing or asking: a discovery, a notice the others "
        "should know, a question, a request for advice, help or review, or small talk. Callable "
        "mid-turn; it returns as soon as the message is recorded and never waits for a reply. "
        "No reply is owed to you and you owe none. Any length is fine; long text is split, never cut."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "text": {"type": "string", "description": "The message."},
            "to": {"type": "string",
                   "description": "Recipient peer name(s), comma-separated (e.g. 'sibyl' or 'sibyl,mirai'), or '@all'."},
            "kind": {"type": "string",
                     "description": "Descriptive label: thought, feeling, experience, discovery, notice, "
                                    "free_talk, question, report, or another short word."},
            "stage": {"type": "string", "enum": list(client.STAGES),
                      "description": "thinking = a summary of where you are now; decided; done."},
            "expects_reply": {"type": "string", "enum": list(client.EXPECTS),
                              "description": "A preference only; 'requested' still obliges no one."},
            "notify": {"type": "string", "enum": list(client.NOTIFY),
                       "description": "mention = start a turn for the named recipients (default). "
                                      "all = same, for group-wide notices. digest = low priority, "
                                      "shown at the recipient's next turn without starting one."},
            "ref": {"type": "string",
                    "description": "Thread id. Reuse the ref of the message you are answering or following up."},
        },
        "required": ["text", "to"],
    },
}

INBOX_SCHEMA = {
    "name": "geminise_inbox",
    "description": (
        "Read geminise messages addressed to you that you have not seen yet. Checks the mailbox "
        "now. Give 'ref' and 'wait_seconds' to wait briefly for a reply to a question you sent."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "ref": {"type": "string", "description": "Only messages in this thread."},
            "wait_seconds": {"type": "integer", "minimum": 0, "maximum": 300,
                             "description": "Keep checking for up to this long when nothing is waiting (default 0)."},
        },
        "required": [],
    },
}

DELEGATE_SCHEMA = {
    "name": "geminise_delegate",
    "description": (
        "Execute an owner-authorized task directly on another Hermes host (e.g. 'mirai', 'sibyl', 'chrome') "
        "so h1karu can stay on one device without touching the other machine. The target peer executes the task "
        "with full local tool authority and automatically returns its verified result. Only callable from a "
        "human-initiated turn (never from a peer-woken turn)."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "to": {"type": "string", "description": "Single target agent peer name (e.g. 'mirai', 'sibyl', 'chrome')."},
            "task": {"type": "string", "description": "Concrete task instruction for the target host to execute and verify."},
            "wait_seconds": {"type": "integer", "minimum": 0, "maximum": 900,
                             "description": "Seconds to wait for the target peer's execution result (default 240; 0 = async)."},
            "ref": {"type": "string", "description": "Optional thread id (auto-generated if omitted)."},
        },
        "required": ["to", "task"],
    },
}


def _open():
    cfg = load()
    prepare_state_dir(cfg)
    return cfg, Store(cfg.store_path), Mailbox(cfg.base_url, cfg.api_key, cfg.workspace, cfg.session,
                                               timeout=cfg.http_timeout)


def _json(payload: dict) -> str:
    return json.dumps(payload, ensure_ascii=False)


def handle_send(args: dict, **kwargs: Any) -> str:
    args = args or {}
    try:
        cfg, store, mailbox = _open()
        session_id = str(kwargs.get("session_id") or "")
        provenance = client.current_provenance(
            user_task=kwargs.get("user_task"), woken_session=bool(session_id) and store.turn_woken(session_id),
            wake_running=store.wake_running(cfg.wake_timeout))
        return _json(client.send(
            cfg, store, mailbox, str(args.get("text") or ""), to=str(args.get("to") or ""),
            kind=str(args.get("kind") or "free_talk"), stage=str(args.get("stage") or "done"),
            expects_reply=str(args.get("expects_reply") or "none"),
            notify=str(args.get("notify") or "mention"), ref=args.get("ref") or None,
            provenance=provenance))
    except (ConfigError, client.SendError) as exc:
        return _json({"ok": False, "error": str(exc)})
    except Exception as exc:
        log.exception("geminise_send failed")
        return _json({"ok": False, "error": f"unexpected {exc.__class__.__name__}"})


def handle_inbox(args: dict, **_kwargs: Any) -> str:
    args = args or {}
    ref = str(args.get("ref") or "").strip() or None
    try:
        wait = max(0, min(300, int(args.get("wait_seconds") or 0)))
        cfg, store, mailbox = _open()
        deadline = time.monotonic() + wait
        error = None
        while True:
            try:
                client.sync(cfg, store, mailbox, inline=True)
                error = None
            except MailboxError as exc:
                error = str(exc)
            items = store.take_inbox(ref=ref)
            if items or time.monotonic() >= deadline:
                break
            time.sleep(min(5.0, max(0.5, deadline - time.monotonic())))
        result: dict = {"ok": error is None, "count": len(items), "messages": client.render_inbox(items)}
        if error:
            result["error"] = f"mailbox not reachable ({error}); showing mail already fetched"
        return _json(result)
    except (ConfigError, ValueError) as exc:
        return _json({"ok": False, "error": str(exc)})
    except Exception as exc:
        log.exception("geminise_inbox failed")
        return _json({"ok": False, "error": f"unexpected {exc.__class__.__name__}"})


HOOK_READ_TIMEOUT = 4.0


def on_pre_llm_call(**kwargs: Any):
    """Show waiting mail at the start of a turn. Normally a local read of what the poller fetched;
    only when the poller has not read the mailbox recently does the turn do one short read itself."""
    try:
        if kwargs.get("parent_session_id") or str(kwargs.get("platform") or "") == "cron":
            return None  # a subagent or a scheduled job is not the agent's own conversation
        cfg = load()
        prepare_state_dir(cfg)
        store = Store(cfg.store_path)
        session_id = str(kwargs.get("session_id") or "")
        if session_id:  # remembered so a send later in this turn knows whether geminise started it
            store.mark_turn(session_id, client.is_wake_text(kwargs.get("user_message")))
        if time.time() - store.last_sync() > max(60.0, 3 * cfg.poll_seconds):
            try:
                client.sync(cfg, store, Mailbox(cfg.base_url, cfg.api_key, cfg.workspace, cfg.session,
                                                timeout=HOOK_READ_TIMEOUT), inline=True)
            except MailboxError:
                store.touch_sync()  # do not make every turn wait while Honcho is unreachable
        items = store.take_inbox(limit=5)
        text = client.render_inbox(items, max_chars=4000)
        return {"context": text} if text else None
    except Exception:
        log.debug("geminise inbox hook skipped", exc_info=True)
        return None


def available() -> bool:
    try:
        load()
        return True
    except ConfigError:
        return False


def handle_delegate(args: dict, **kwargs: Any) -> str:
    args = args or {}
    try:
        cfg, store, mailbox = _open()
        session_id = str(kwargs.get("session_id") or "")
        provenance = client.current_provenance(
            user_task=kwargs.get("user_task"), woken_session=bool(session_id) and store.turn_woken(session_id),
            wake_running=store.wake_running(cfg.wake_timeout))
        to = str(args.get("to") or "").strip()
        task = str(args.get("task") or "").strip()
        if not task:
            return _json({"ok": False, "error": "'task' must not be empty"})
        raw_wait = args.get("wait_seconds")
        wait = max(0, min(900, 240 if raw_wait is None else int(raw_wait)))
        sent = client.send(
            cfg, store, mailbox, task, to=to, kind=client.DELEGATE_KIND, stage="decided",
            expects_reply="requested", notify="mention", ref=args.get("ref") or None,
            provenance=provenance, delegate=True)
        if not sent.get("ok"):
            return _json(sent)
        ref = sent["ref"]
        if wait == 0:
            return _json({"ok": True, "peer": sent["to"][0], "ref": ref, "status": "dispatched", "rang": sent.get("rang", [])})

        deadline = time.monotonic() + wait
        while time.monotonic() < deadline:
            time.sleep(min(2.5, max(0.3, deadline - time.monotonic())))
            try:
                client.sync(cfg, store, mailbox, inline=True)
            except MailboxError:
                continue
            items = store.take_inbox(ref=ref)
            for item in items:
                try:
                    env = protocol.parse(item["wire"])
                except protocol.ProtocolError:
                    continue
                if dict(env.metadata).get("kind") == client.DELEGATE_RESULT_KIND:
                    try:
                        parsed = json.loads(env.text)
                        if isinstance(parsed, dict):
                            return _json({"status": "completed", **parsed})
                    except ValueError:
                        pass
                    return _json({"ok": True, "peer": item["from"], "ref": ref, "status": "completed", "output": env.text})
        return _json({"ok": False, "peer": sent["to"][0], "ref": ref, "status": "timeout",
                      "error": f"no result from {sent['to'][0]} within {wait}s; check geminise_inbox(ref={ref!r}) later"})
    except (ConfigError, client.SendError, ValueError) as exc:
        return _json({"ok": False, "error": str(exc)})
    except Exception as exc:
        log.exception("geminise_delegate failed")
        return _json({"ok": False, "error": f"unexpected {exc.__class__.__name__}"})


def register(ctx: Any) -> None:
    for schema, handler, emoji in (
        (SEND_SCHEMA, handle_send, "📨"),
        (INBOX_SCHEMA, handle_inbox, "📬"),
        (DELEGATE_SCHEMA, handle_delegate, "🚀"),
    ):
        ctx.register_tool(name=schema["name"], toolset=TOOLSET, schema=schema, handler=handler,
                          check_fn=available, emoji=emoji, description=schema["description"])
    ctx.register_hook("pre_llm_call", on_pre_llm_call)
