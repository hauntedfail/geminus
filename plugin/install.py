#!/usr/bin/env python3
"""Install geminish into a Hermes home (any OS; plain Python, no PowerShell script).

    python install.py --upgrade                                          # newer files, same identity
    python install.py --from mirai --secret-file secret.txt
    python install.py --from mirai --secret-file secret.txt --force     # replace an older install
    python install.py --from node1 --members '{"owner":"human","node1":"agent"}' --secret-file secret.txt
    python install.py --from mirai --secret-file secret.txt --dry-run

--upgrade only replaces the plugin files. It keeps geminise.json, needs no secret, and is the
way to move a host that already runs geminish 0.2 or later to this version.

The secret is the group's signing secret (64+ hex characters). It is the same on every member
host and is copied by hand: it must never be stored in Honcho, a repository or a chat.
A fresh install copies files, writes geminise.json, enables the plugin and reads the mailbox once.
Neither mode registers or restarts the poller service; plugin/README.md has the command for each OS.
"""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

PKG = Path(__file__).resolve().parent
SRC = PKG / "geminish"
DEFAULT_MEMBERS = {"h1karu": "human", "chrome": "agent", "sibyl": "agent", "mirai": "agent"}
IGNORE = shutil.ignore_patterns("__pycache__", "*.pyc", "tests", "geminise.json")
STAGING, PREVIOUS = ".geminish-installing", ".geminish-previous"  # beside plugins/, never inside it


def hermes_home(arg: str) -> Path:
    if arg:
        return Path(arg)
    if os.environ.get("HERMES_HOME"):
        return Path(os.environ["HERMES_HOME"])
    local = os.environ.get("LOCALAPPDATA")
    if local and (Path(local) / "hermes").is_dir():
        return Path(local) / "hermes"
    return Path.home() / ".hermes"


def find_hermes(home: Path) -> str:
    local = Path(os.environ.get("LOCALAPPDATA", Path.home() / "AppData" / "Local"))
    for candidate in (os.environ.get("GEMINISH_HERMES_BIN"), shutil.which("hermes"),
                      local / "hermes" / "bin" / "hermes.exe", Path.home() / ".local" / "bin" / "hermes",
                      home / "bin" / "hermes"):
        if candidate and Path(candidate).exists():
            return str(candidate)
    return ""


def _remove(path: Path) -> None:
    if path.is_dir() and not path.is_symlink():
        shutil.rmtree(path, ignore_errors=True)
    elif path.exists() or path.is_symlink():
        path.unlink()


def parse_members(raw: str) -> dict[str, str]:
    if not raw:
        return dict(DEFAULT_MEMBERS)
    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ValueError(f"invalid --members JSON: {exc}") from exc
    if not isinstance(parsed, dict) or not parsed:
        raise ValueError("--members must be a non-empty JSON object of {peer: 'agent'|'human'}")
    out: dict[str, str] = {}
    for key, role in parsed.items():
        if not isinstance(key, str) or not key.strip():
            raise ValueError("member names must be non-empty strings")
        if role not in ("agent", "human"):
            raise ValueError(f"member {key!r} role must be 'agent' or 'human', got {role!r}")
        out[key] = role
    return out


def replace_plugin(dest: Path) -> None:
    """Swap the plugin directory by renaming, so the old install is either fully in place or fully
    replaced. Raises OSError with the old install untouched when the swap cannot be made (on
    Windows, typically a file of the old install that is still open)."""
    base = dest.parent.parent
    staged, previous = base / STAGING, base / PREVIOUS
    if previous.is_dir() and not dest.exists():
        previous.rename(dest)  # an earlier run stopped between its two renames: put the old install back
    _remove(staged)
    _remove(previous)
    dest.parent.mkdir(parents=True, exist_ok=True)
    try:
        shutil.copytree(SRC, staged, ignore=IGNORE)
        if (dest / "geminise.json").is_file():  # an identity kept inside the plugin moves with it
            shutil.copy2(dest / "geminise.json", staged / "geminise.json")
        had_old = dest.exists()
        if had_old:
            dest.rename(previous)
        try:
            staged.rename(dest)
        except OSError:
            if had_old:
                previous.rename(dest)
            raise
    except OSError:
        _remove(staged)
        raise
    _remove(previous)


def upgrade(home: Path, dry_run: bool) -> int:
    dest = home / "plugins" / "geminish"
    if not dest.is_dir() or not ((home / "geminise.json").is_file() or (dest / "geminise.json").is_file()):
        print(f"error: no geminish install with a geminise.json under {home}; do a fresh install instead",
              file=sys.stderr)
        return 1
    print(f"source: {SRC}\ntarget: {dest}\nidentity: unchanged")
    if dry_run:
        print("dry-run: nothing written")
        return 0
    try:
        replace_plugin(dest)
    except OSError as exc:
        print(f"error: could not replace {dest} ({exc.__class__.__name__}); the old install is unchanged. "
              "Stop the poller and try again.", file=sys.stderr)
        return 1
    print("upgraded. Restart the poller service, then open a new Hermes session to load the new tools.")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Install the geminish plugin for Hermes.")
    parser.add_argument("--upgrade", action="store_true", help="replace the plugin files only; keep geminise.json")
    parser.add_argument("--from", dest="me", help="this host's own peer name")
    parser.add_argument("--members", default="", help="JSON map of {peer: 'agent'|'human'} for fresh installs")
    parser.add_argument("--secret-file", help="file holding the group signing secret (hex)")
    parser.add_argument("--hermes-home", default="")
    parser.add_argument("--no-wake", action="store_true", help="never start turns here; mail waits for the next turn")
    parser.add_argument("--force", action="store_true", help="replace an existing plugins/geminish")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)

    if args.upgrade:
        home = hermes_home(args.hermes_home)
        if not (home / "honcho.json").is_file():
            print(f"error: {home / 'honcho.json'} not found; is this the right Hermes home?", file=sys.stderr)
            return 1
        return upgrade(home, args.dry_run)
    if not args.me or not args.secret_file:
        parser.error("a fresh install needs --from and --secret-file (or use --upgrade)")
    try:
        members = parse_members(args.members)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    if args.me not in members or members[args.me] != "agent":
        print(f"error: --from must be one of {[m for m, k in members.items() if k == 'agent']}", file=sys.stderr)
        return 1
    try:
        secret = Path(args.secret_file).read_text(encoding="utf-8-sig").strip()
        if len(bytes.fromhex(secret)) < 32:
            raise ValueError
    except (OSError, ValueError):
        print("error: --secret-file must hold at least 64 hex characters", file=sys.stderr)
        return 1
    home = hermes_home(args.hermes_home)
    dest, config = home / "plugins" / "geminish", home / "geminise.json"
    if not (home / "honcho.json").is_file():
        print(f"error: {home / 'honcho.json'} not found; set up Honcho memory for this Hermes first", file=sys.stderr)
        return 1
    if dest.exists() and not args.force:
        print(f"error: {dest} exists; re-run with --force to replace it", file=sys.stderr)
        return 1
    print(f"source: {SRC}\ntarget: {dest}\nconfig: {config}\nidentity: {args.me}")
    if args.dry_run:
        print("dry-run: nothing written")
        return 0

    try:
        replace_plugin(dest)
    except OSError as exc:
        print(f"error: could not install into {dest} ({exc.__class__.__name__}); nothing was changed",
              file=sys.stderr)
        return 1
    if config.exists() and not args.force:
        print(f"kept the existing {config} (use --force to rewrite it)")
    else:
        fd = os.open(config, os.O_WRONLY | os.O_CREAT, 0o600)
        if hasattr(os, "fchmod"):
            os.fchmod(fd, 0o600)  # before the secret is written, even if the file existed with a wider mode
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
            handle.truncate(0)
            json.dump({"from": args.me, "members": members, "session": "geminise-main", "poll_seconds": 10,
                       "wake": not args.no_wake, "secret": secret}, handle, indent=2)
            handle.write("\n")
    print("installed. Delete the secret file now; geminise.json holds it.")

    binary = find_hermes(home)
    def run(command: list[str], timeout: int) -> int:
        try:
            return subprocess.run(command, check=False, timeout=timeout,
                                  env={**os.environ, "HERMES_HOME": str(home)}).returncode
        except (OSError, subprocess.TimeoutExpired) as exc:
            print(f"warning: {command[0]} did not finish: {exc.__class__.__name__}", file=sys.stderr)
            return 1

    if not binary or run([binary, "plugins", "enable", "geminish"], 180) != 0:
        print("warning: run `hermes plugins enable geminish` yourself", file=sys.stderr)
    if run([sys.executable, str(dest / "cli.py"), "init"], 120) != 0:
        print("warning: the first mailbox read failed; check `python cli.py status` once Honcho is reachable",
              file=sys.stderr)
    print("next: register the poller (plugin/README.md), then open a new Hermes session to load the tools.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
