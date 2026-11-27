#!/usr/bin/env python3
"""geminish command line: the resident poller and a few operator commands.

    python3 cli.py init            create the mailbox session and mark existing mail as history
    python3 cli.py poll            run the poller (what the service unit starts)
    python3 cli.py poll --once     read the mailbox once and exit
    python3 cli.py status          local delivery state, no secrets
    python3 cli.py send --to sibyl "text"
    python3 cli.py inbox
"""
from __future__ import annotations

import argparse
import json
import logging
from logging.handlers import RotatingFileHandler
from pathlib import Path
import signal
import sys
import threading
import time

if __package__ in (None, ""):  # run as a script: make the package importable by its directory name
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
    __package__ = Path(__file__).resolve().parent.name
    import importlib
    importlib.import_module(__package__)  # relative imports need the parent package loaded

from . import __version__, bridge, client, ota, push  # noqa: E402
from .config import ConfigError, load, prepare_state_dir  # noqa: E402
from .mailbox import Mailbox, MailboxError  # noqa: E402
from .store import Store  # noqa: E402

log = logging.getLogger("geminish")
RING_FLOOR = 1.0  # seconds between two reads caused by rings, however many rings arrive


def _open():
    cfg = load()
    prepare_state_dir(cfg)
    mailbox = Mailbox(cfg.base_url, cfg.api_key, cfg.workspace, cfg.session,
                      timeout=cfg.http_timeout, e2ee_url=cfg.e2ee_url, e2ee_secret=cfg.secret)
    return cfg, Store(cfg.store_path), mailbox


def _logging(cfg, to_file: bool) -> None:
    handlers: list[logging.Handler] = [logging.StreamHandler()]
    if to_file:
        handlers = [RotatingFileHandler(cfg.state_dir / "poller.log", maxBytes=1_000_000, backupCount=3,
                                        encoding="utf-8")]
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s", handlers=handlers)


def cmd_init(_args) -> int:
    cfg, store, mailbox = _open()
    mailbox.ensure_session(tuple(member.peer for member in cfg.membership.members))
    client.sync(cfg, store, mailbox)
    print(json.dumps({"ok": True, "me": cfg.me, "session": cfg.session, **store.summary()}))
    return 0


def _wait(delay: float, stop: threading.Event, ring: threading.Event) -> bool:
    """Wait up to ``delay`` seconds for the next read. True means a doorbell ended the wait early.

    The ring is cleared here, before the read it causes, so a ring that arrives during that read
    is still set afterwards and causes one more."""
    deadline = time.monotonic() + delay
    while not stop.is_set():
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return False
        if ring.wait(min(1.0, remaining)):
            ring.clear()
            return True
    return False


def _ring_gap(last_read: float, now: float) -> float:
    """How long a read caused by a ring still has to wait, so a flood of rings cannot flood Honcho."""
    return max(0.0, RING_FLOOR - (now - last_read))


def _push_status(cfg, store, now: float | None = None) -> dict:
    """What `status` shows about the doorbell. Never the endpoint itself: it is a capability."""
    state = store.get_meta("push_state")
    idle = (time.time() if now is None else now) - store.last_sync()
    if state and idle > max(60.0, 3 * cfg.poll_seconds):
        state += f" (no mailbox read for {idle:.0f}s: the poller is stopped, or busy with a turn it started)"
    return {"enabled": cfg.push, "registered": cfg.push and bool(store.get_meta("push_endpoint")), "state": state}


def _prepare_push(cfg, store, stop: threading.Event, ring: threading.Event, *, once: bool) -> None:
    """A resident poller owns the push state; a one-shot read must not touch a running poller's."""
    if once:
        return
    store.set_meta("push_state", None)  # whatever an earlier poller left behind is no longer true
    if cfg.push:
        _start_listener(cfg, stop, ring)


def _start_listener(cfg, stop: threading.Event, ring: threading.Event) -> None:
    """The doorbell runs beside the timer and only ever makes the next read happen sooner."""
    listener = push.Listener(
        me=cfg.me, secret=cfg.secret, store=Store(cfg.store_path), on_ring=ring.set,
        directory=Mailbox(cfg.base_url, cfg.api_key, cfg.workspace, cfg.session,
                          timeout=cfg.http_timeout, e2ee_url=cfg.e2ee_url, e2ee_secret=cfg.secret))
    threading.Thread(target=listener.run, args=(stop,), name="geminish-push", daemon=True).start()


def cmd_poll(args) -> int:
    cfg, store, mailbox = _open()
    _logging(cfg, to_file=not args.once and not args.foreground)
    stop, ring = threading.Event(), threading.Event()

    def _stop(*_):
        stop.set()
    signal.signal(signal.SIGTERM, _stop)
    signal.signal(signal.SIGINT, _stop)
    log.info("geminish %s polling as %s every %.0fs (wake=%s, max_hops=%d, push=%s)", __version__, cfg.me,
             cfg.poll_seconds, cfg.wake, cfg.max_hops, cfg.push)
    _prepare_push(cfg, store, stop, ring, once=args.once)
    owner = bridge.Bridge(cfg, store, bridge.Api(mailbox))
    ota_cfg = ota.ota_settings(cfg)
    ota_interval = float(ota_cfg.get("interval_seconds", ota.DEFAULT_INTERVAL_SECONDS))
    next_ota = time.monotonic() + 15.0 if ota_cfg.get("enabled") and not args.once else float("inf")
    failures = 0
    while True:
        last_read = time.monotonic()
        try:
            released = store.recover(cfg.wake_timeout)
            if released:
                log.warning("%d unfinished wake(s) moved to the inbox", released)
            wakes = client.sync(cfg, store, mailbox)
            # The owner's prompts start before any mail turn: a mail turn holds this loop until
            # it ends, a geminus conversation and delegated tasks run in workers of their own.
            started = owner.poll()
            if started:
                log.info("geminus: %d conversation(s) picked up", started)
            for result in client.deliver(cfg, store, wakes, mailbox=mailbox, background=not args.once):
                log.info("wake %s from %s: %s", result["id"], result["from"], result["detail"])
            if failures:
                log.info("mailbox reachable again after %d failed reads", failures)
            failures = 0
        except MailboxError as exc:
            failures += 1
            if failures in (1, 10) or failures % 100 == 0:
                log.warning("mailbox read failed (%d in a row): %s", failures, exc)
        except Exception:
            failures += 1
            log.exception("poll cycle failed")
        if time.monotonic() >= next_ota:
            next_ota = time.monotonic() + ota_interval
            try:
                res = ota.update_geminish(cfg, __version__)
                if res.get("updated"):
                    owner.wait(30.0)
                    ota.restart_poller()
            except Exception as exc:
                log.warning("ota: geminish check failed: %s", exc)
            try:
                ota.update_geminus_cli(cfg)
            except Exception as exc:
                log.warning("ota: geminus check failed: %s", exc)
        if args.once:
            owner.wait(cfg.wake_timeout)
            return 0 if failures == 0 else 1
        # Back off while Honcho is unreachable, up to five minutes; mail waits there meanwhile.
        delay = min(300.0, cfg.poll_seconds * (2 ** min(failures, 5))) if failures else cfg.poll_seconds
        if _wait(delay, stop, ring):
            stop.wait(_ring_gap(last_read, time.monotonic()))
            log.info("push: ring, reading the mailbox now")
        if stop.is_set():
            log.info("stopping")
            return 0


def cmd_status(_args) -> int:
    cfg, store, _ = _open()
    print(json.dumps({"version": __version__, "me": cfg.me, "session": cfg.session,
                      "members": list(cfg.membership.authorized), "honcho": cfg.base_url,
                      "wake": cfg.wake, "max_hops": cfg.max_hops, "hermes_bin": cfg.hermes_bin or None,
                      "state": str(cfg.store_path),
                      "push": _push_status(cfg, store),
                      "geminus": {"enabled": (cfg.geminus or {}).get("enabled", True) is not False,
                                  "harnesses": [h.id for h, _ in bridge.available(cfg)]},
                      **store.summary()}, indent=2))
    return 0


def cmd_send(args) -> int:
    cfg, store, mailbox = _open()
    text = args.text if args.text is not None else sys.stdin.read()
    result = client.send(cfg, store, mailbox, text, to=args.to, kind=args.kind, stage=args.stage,
                         expects_reply=args.expects_reply, notify=args.notify, ref=args.ref)
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0 if result.get("ok") else 1


def cmd_inbox(args) -> int:
    cfg, store, mailbox = _open()
    client.sync(cfg, store, mailbox, inline=True)
    print(client.render_inbox(store.take_inbox(ref=args.ref)) or "(no new geminise mail)")
    return 0


def cmd_update(_args) -> int:
    cfg, _, _ = _open()
    plugin_res = ota.update_geminish(cfg, __version__)
    try:
        cli_res = ota.update_geminus_cli(cfg)
    except ota.OTAError as exc:
        cli_res = {"ok": False, "error": str(exc)}
    print(json.dumps({"geminish": plugin_res, "geminus": cli_res}, ensure_ascii=False, indent=2))
    return 0


def cmd_release(args) -> int:
    cfg, _, _ = _open()
    manifest = ota.trigger_plugin_release(cfg, args.version)
    print(json.dumps({"ok": True, "version": manifest["version"], "files": manifest["files"]},
                     ensure_ascii=False, indent=2))
    return 0


def cmd_delegate(args) -> int:
    from . import handle_delegate
    task = args.task if args.task is not None else sys.stdin.read()
    raw = handle_delegate({"to": args.to, "task": task, "wait_seconds": args.wait, "ref": args.ref})
    print(raw)
    try:
        return 0 if json.loads(raw).get("ok") else 1
    except ValueError:
        return 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="geminish", description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("init").set_defaults(func=cmd_init)
    poll = sub.add_parser("poll")
    poll.add_argument("--once", action="store_true")
    poll.add_argument("--foreground", action="store_true", help="log to stderr instead of poller.log")
    poll.set_defaults(func=cmd_poll)
    sub.add_parser("status").set_defaults(func=cmd_status)
    send = sub.add_parser("send")
    send.add_argument("text", nargs="?")
    send.add_argument("--to", required=True)
    send.add_argument("--kind", default="free_talk")
    send.add_argument("--stage", default="done", choices=client.STAGES)
    send.add_argument("--expects-reply", default="none", choices=client.EXPECTS)
    send.add_argument("--notify", default="mention", choices=client.NOTIFY)
    send.add_argument("--ref", default=None)
    send.set_defaults(func=cmd_send)
    inbox = sub.add_parser("inbox")
    inbox.add_argument("--ref", default=None)
    inbox.set_defaults(func=cmd_inbox)
    sub.add_parser("update").set_defaults(func=cmd_update)
    rel = sub.add_parser("release")
    rel.add_argument("version")
    rel.set_defaults(func=cmd_release)
    deleg = sub.add_parser("delegate")
    deleg.add_argument("task", nargs="?")
    deleg.add_argument("--to", required=True)
    deleg.add_argument("--wait", type=int, default=240)
    deleg.add_argument("--ref", default=None)
    deleg.set_defaults(func=cmd_delegate)
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except (ConfigError, client.SendError, MailboxError, ota.OTAError) as exc:
        print(f"geminish: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
