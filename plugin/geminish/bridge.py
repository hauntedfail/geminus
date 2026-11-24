"""The geminus bridge: run what the owner typed in geminus on this computer's agent programs.

geminus is a different product from geminise mail. It shares only the Honcho workspace and
this resident poller. A geminus record is told apart by its metadata (``kind: geminus`` on a
session, a ``geminus`` key on a message) and never enters the mail path: no wake policy, no
preamble, no inbox. The owner's prompt is handed to the agent program word for word, and what
the program prints is written back word for word.

The wire format is specified in the geminus repository, docs/protocol.md.
"""
from __future__ import annotations

import codecs
from dataclasses import dataclass
import hashlib
import json
import logging
import os
from pathlib import Path
import queue
import re
import shutil
import subprocess
import sys
import threading
import time
import urllib.parse
import uuid

from .ed25519_verify import verify_human_ed25519
from .mailbox import MailboxError, sign, verify

log = logging.getLogger("geminish.bridge")

KIND = "geminus"                 # session metadata "kind", and the metadata key on messages
NODE_KEY = "geminus_node"        # key in this peer's Honcho metadata
MAX_AGE = 7 * 86400              # seconds a signed prompt stays runnable
CLOCK_SKEW = 300
PIECE_CHARS = 6000               # under Honcho's 25,000-byte cap even at 4 bytes a character
FLUSH_SECONDS = 2.0
CARD_SECONDS = 300.0
DEFAULT_TIMEOUT = 3600
PAGE = 50
MAX_PAGES = 20
STDERR_TAIL = 600


# ----- signatures ------------------------------------------------------------------

def _digest(value: str) -> str:
    """A free-form value inside a label: hashed, so it cannot run into the fields around it."""
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def prompt_label(session: str, persona: str, harness: str, human: str, nonce: str, cwd: str,
                 model: str = "", effort: str = "") -> str:
    """Binds a prompt to its conversation, target, author, the directory it is to run in, and the
    model and effort it asks for. All of these are part of what the owner signed: metadata alone
    could be rewritten by anyone holding the Honcho token.

    The settings part is there only when one is set, so a prompt without settings is signed the
    way it was before settings existed and a 0.5 bridge still runs it. Adding a setting to such a
    prompt, or taking the settings off one that has them, changes the label and so fails."""
    label = f"geminus-prompt:{session}:{persona}:{harness}:{human}:{nonce}:{_digest(cwd)}"
    if model or effort:
        label += f":{_digest(model)}:{_digest(effort)}"
    return label


def output_label(session: str, turn: str, seq: int, final: bool, ok: bool, native: str) -> str:
    """Binds a piece of output to its turn, position and the agent program's session id."""
    return f"geminus-output:{session}:{turn}:{seq}:{int(final)}:{int(ok)}:{_digest(native)}"


def node_label(peer: str) -> str:
    return f"geminus-node:{peer}"


def options_label(peer: str) -> str:
    return f"geminus-node-options:{peer}"


def cwd_label(peer: str) -> str:
    return f"geminus-node-cwd:{peer}"


# ----- agent programs --------------------------------------------------------------

# A model or effort the owner set in geminus becomes an argument of the agent program, so it is
# only accepted in a form that no program can read as an option, a second argument, or (for the
# one that takes it inside a config value) as anything but a bare word.
_MODEL = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:/@#\[\]+-]{0,127}\Z")
_EFFORT = re.compile(r"[A-Za-z0-9][A-Za-z0-9_-]{0,31}\Z")


def valid_setting(kind: str, value: object) -> bool:
    """Whether value may be passed on as a model or an effort. The empty string means "not set"."""
    if not isinstance(value, str):
        return False
    return value == "" or bool((_MODEL if kind == "model" else _EFFORT).match(value))


class CannotGoBack(Exception):
    """The program cannot return this setting to its own default inside a session."""


@dataclass(frozen=True)
class Harness:
    """One agent program and how to give it a prompt in its own non-interactive mode."""
    id: str
    name: str
    binary: str
    output: str = "text"   # how stdout is read: text, codex (JSON lines) or grok (one JSON object)
    # How a model and an effort are passed for one run; "{}" stands for the value. Empty means the
    # program has no such option. Nothing here writes the program's own configuration.
    model: tuple = ()
    effort: tuple = ()
    # For a program that takes a setting inside the session instead: (setting, the line that sets
    # it, the line that goes back to the program's own or "" if it has none). The line is sent as a
    # turn of its own before the prompt and must change that session only.
    inline: tuple = ()

    @property
    def options(self) -> tuple:
        """The settings geminus may pass to this program."""
        said = {name for name, _, _ in self.inline}
        return tuple(name for name, how in (("model", self.model), ("effort", self.effort)) if how or name in said)

    def preludes(self, asked: dict, ever: dict) -> list:
        """The lines to send in the session before a prompt: (setting, line). ``asked`` is what the
        prompt carries; ``ever`` says which settings an earlier prompt of the conversation carried.

        A value is said before every prompt that carries it, since the program may have forgotten
        (Hermes keeps it in the memory of its gateway process). Once a conversation has had a
        value, a prompt without one says the line that goes back. Raises CannotGoBack if the
        program has no such line."""
        lines = []
        for name, set_line, reset_line in self.inline:
            if asked.get(name):
                lines.append((name, set_line.replace("{}", asked[name])))
            elif ever.get(name):
                if not reset_line:
                    raise CannotGoBack(name)
                lines.append((name, reset_line))
        return lines

    def tuning(self, model: str, effort: str) -> list[str]:
        """The arguments that select model and effort for one run. Unset values add nothing, so
        the program's own default applies."""
        args: list[str] = []
        for value, how in ((model, self.model), (effort, self.effort)):
            if value and how:
                args += [part.replace("{}", value) for part in how]
        return args

    def command(self, session: str, native: str | None, prompt: str, tune=()) -> tuple[list[str], str | None]:
        """(arguments after the program, text for stdin or None). ``native`` is the program's own
        session id from an earlier turn of this conversation, if there was one. ``tune`` is what
        ``tuning`` returned; it goes before anything positional."""
        if self.id == "hermes":
            return ["chat", *tune, "-Q", "--continue", session, "--create-if-missing", "--source", "tool",
                    "--query-file", "-"], prompt
        if self.id == "claude":
            # Claude Code takes the id of a new session from the caller, so it is derived from the
            # Honcho session and is the same on every turn without being stored anywhere.
            own = str(uuid.uuid5(uuid.NAMESPACE_URL, "geminus:" + session))
            return ["-p", *tune, "--resume" if native else "--session-id", own], prompt
        if self.id == "codex":
            head = ["exec", "resume", native] if native else ["exec"]
            return [*head, *tune, "--json", "--skip-git-repo-check", "-"], prompt
        if self.id == "grok":
            return [*tune, "-p", prompt, "--output-format", "json", *(["-r", native] if native else [])], None
        if self.id == "pi":
            return ["-p", *tune, "--session-id", session, prompt], None
        if self.id == "opencode":
            return ["run", *tune, "-s", session, prompt], None
        raise ValueError(f"no command for harness {self.id}")

    def native_id(self, session: str, reported: str | None) -> str:
        """The program's session id to remember after a turn."""
        if self.id == "claude":
            return str(uuid.uuid5(uuid.NAMESPACE_URL, "geminus:" + session))
        if self.output in ("codex", "grok"):
            return reported or ""
        return session


HARNESSES = (
    # Hermes fixes model and reasoning when a session is created and refuses them as options on
    # every later run; its own slash commands change them for that session only.
    Harness("hermes", "Hermes Agent", "hermes",
            inline=(("model", "/model {}", ""), ("effort", "/reasoning {}", "/reasoning reset"))),
    Harness("claude", "Claude Code", "claude", model=("--model", "{}"), effort=("--effort", "{}")),
    Harness("codex", "Codex", "codex", output="codex", model=("-m", "{}"),
            effort=("-c", 'model_reasoning_effort="{}"')),
    Harness("grok", "Grok", "grok", output="grok", model=("-m", "{}"), effort=("--reasoning-effort", "{}")),
    Harness("pi", "pi", "pi", model=("--model", "{}"), effort=("--thinking", "{}")),
    # OpenCode has no effort option; its model name can carry a variant (provider/model#variant).
    Harness("opencode", "OpenCode", "opencode", model=("-m", "{}")),
)


# ----- the owner's environment -----------------------------------------------------

SHELL_ENV_SECONDS = 300.0
_MARK = "__geminus_env__"
_SH_LIKE = ("sh", "bash", "zsh", "dash", "ksh")
_shell_env: dict = {"at": 0.0, "env": None}
_shell_lock = threading.Lock()


def read_shell_env(environ=os.environ, run=subprocess.run) -> dict | None:
    """The environment of the owner's interactive login shell, or None if it cannot be read.

    The poller is a service: its PATH is short and none of the owner's profile has run. An agent
    program has to be found and started the way it is when the owner sits at this computer,
    or another copy of it (older, differently configured) is picked up instead."""
    if os.name != "posix":
        return None  # a Windows scheduled task already runs with the user's environment
    shell = environ.get("SHELL") or ""
    if not shell:
        try:
            import pwd
            shell = pwd.getpwuid(os.getuid()).pw_shell
        except (ImportError, KeyError):
            shell = ""
    if os.path.basename(shell) not in _SH_LIKE:
        return None
    try:
        done = run([shell, "-lic", f"printf '%s' {_MARK}; env -0; printf '%s' {_MARK}"],
                   stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=30)
    except (OSError, subprocess.SubprocessError):
        return None
    parts = (done.stdout or b"").split(_MARK.encode())
    if len(parts) < 3:
        return None
    env = {}
    for item in parts[1].split(b"\0"):
        key, sep, value = item.partition(b"=")
        if sep and key:
            env[key.decode("utf-8", "replace")] = value.decode("utf-8", "replace")
    for transient in ("PWD", "OLDPWD", "SHLVL", "_"):
        env.pop(transient, None)
    return env if env.get("PATH") else None


def shell_env(now: float | None = None) -> dict:
    """The owner's environment, re-read every few minutes; this process's own as a fallback."""
    now = time.monotonic() if now is None else now
    with _shell_lock:
        if _shell_env["env"] is None or now - _shell_env["at"] > SHELL_ENV_SECONDS:
            _shell_env["env"], _shell_env["at"] = read_shell_env() or dict(os.environ), now
        return dict(_shell_env["env"])


def find_binary(name: str, cfg=None, env: dict | None = None) -> str:
    """The program the owner's shell would run for this name, or ""."""
    if name == "hermes" and cfg is not None and getattr(cfg, "hermes_bin", ""):
        return cfg.hermes_bin
    env = shell_env() if env is None else env
    return shutil.which(name, path=env.get("PATH", "")) or ""


def available(cfg=None, harnesses=HARNESSES, find=find_binary) -> list[tuple[Harness, str]]:
    """The agent programs installed on this computer right now, with their paths."""
    found = []
    for harness in harnesses:
        path = find(harness.binary, cfg)
        if path:
            found.append((harness, path))
    return found


# ----- Honcho ----------------------------------------------------------------------

class Api:
    """The few Honcho calls the bridge makes, over the poller's own connection."""

    def __init__(self, mailbox):
        self._mailbox = mailbox

    def _call(self, method: str, path: str, body: object = None, *, write: bool = False):
        return self._mailbox.request(method, path, body, write=write)

    @staticmethod
    def _q(value: str) -> str:
        return urllib.parse.quote(value, safe="")

    def pending_sessions(self, persona: str) -> list[dict]:
        found: list[dict] = []
        for page in range(1, MAX_PAGES + 1):
            result = self._call("POST", f"/sessions/list?page={page}&size={PAGE}", {
                "filters": {"metadata": {"kind": KIND, KIND: {"persona": persona, "pending": True}}}})
            items = result.get("items") if isinstance(result, dict) else None
            if not isinstance(items, list):
                raise MailboxError("Honcho session list has no items")
            found += [item for item in items if isinstance(item, dict) and item.get("id")]
            if len(items) < PAGE:
                break
        return found

    def set_session_metadata(self, session: str, metadata: dict) -> None:
        self._call("PUT", f"/sessions/{self._q(session)}", {"metadata": metadata}, write=True)

    def messages(self, session: str) -> list[dict]:
        """Every message of the session, oldest first."""
        found: list[dict] = []
        for page in range(1, MAX_PAGES + 1):
            result = self._call("POST", f"/sessions/{self._q(session)}/messages/list?page={page}&size={PAGE}", {})
            items = result.get("items") if isinstance(result, dict) else None
            if not isinstance(items, list):
                raise MailboxError("Honcho message list has no items")
            found += [item for item in items if isinstance(item, dict)]
            if len(items) < PAGE:
                break
        return found

    def post(self, session: str, peer: str, content: str, metadata: dict) -> dict:
        stored = self._call("POST", f"/sessions/{self._q(session)}/messages", {
            "messages": [{"peer_id": peer, "content": content, "metadata": metadata}]}, write=True)
        if not isinstance(stored, list) or len(stored) != 1 or not isinstance(stored[0], dict):
            raise MailboxError("Honcho did not echo the message", ambiguous=True)
        return stored[0]

    def set_message_metadata(self, session: str, message: str, metadata: dict) -> None:
        self._call("PUT", f"/sessions/{self._q(session)}/messages/{self._q(message)}",
                   {"metadata": metadata}, write=True)

    def set_peer_metadata(self, peer: str, key: str, value: object) -> None:
        self._mailbox.set_peer_metadata(peer, key, value)


# ----- the node card ---------------------------------------------------------------

def card_text(harnesses) -> str:
    """What the options signature of a node card covers: each program's id followed by its options."""
    return ",".join(":".join((h.id, *h.options)) for h in harnesses)


def node_card(secret: bytes, peer: str, harnesses, now: float, cwd: str = "") -> dict:
    """``sig`` covers the program ids, as it always has, so a geminus from before options existed
    still reads the card. ``options_sig`` covers the ids with their options, and ``cwd_sig`` the
    directory a prompt runs in when its conversation names none, which geminus shows."""
    stamp = int(now)
    card = {"v": 1, "harnesses": [{"id": h.id, "name": h.name, "options": list(h.options)} for h in harnesses],
            "ts": stamp, "sig": sign(secret, ",".join(h.id for h in harnesses), node_label(peer), stamp),
            "options_sig": sign(secret, card_text(harnesses), options_label(peer), stamp)}
    if cwd:
        card.update(cwd=cwd, cwd_sig=sign(secret, cwd, cwd_label(peer), stamp))
    return card


# ----- running one prompt ----------------------------------------------------------

@dataclass
class Result:
    ok: bool
    native: str = ""
    detail: str = ""


def _decode_codex(line: str, state: dict) -> str:
    """Text to show for one line of `codex exec --json`, and the thread id as a side effect."""
    try:
        event = json.loads(line)
    except ValueError:
        return ""
    if not isinstance(event, dict):
        return ""
    if event.get("type") == "thread.started" and isinstance(event.get("thread_id"), str):
        state["native"] = event["thread_id"]
    item = event.get("item")
    if event.get("type") == "item.completed" and isinstance(item, dict) and item.get("type") == "agent_message":
        return str(item.get("text") or "") + "\n"
    if event.get("type") in ("turn.failed", "error"):
        state["error"] = json.dumps(event.get("error") or event.get("message") or event, ensure_ascii=False)[:STDERR_TAIL]
    return ""


def _decode_grok(text: str, state: dict) -> str:
    """Text to show for the single JSON object `grok --output-format json` prints at the end."""
    try:
        result = json.loads(text)
    except ValueError:
        return text  # not JSON after all: show what it printed
    if not isinstance(result, dict):
        return text
    if isinstance(result.get("sessionId"), str):
        state["native"] = result["sessionId"]
    return str(result.get("text") or "")


def run_prompt(harness: Harness, binary: str, session: str, native: str | None, prompt: str, *,
               cwd: str, emit, extra_args=(), timeout: float = DEFAULT_TIMEOUT, popen=subprocess.Popen,
               environ: dict | None = None, tune=()) -> Result:
    """Run one prompt and pass what the program prints to ``emit(text)`` as it arrives."""
    args, stdin_text = harness.command(session, native, prompt, tune)
    command = [binary, *args, *extra_args]
    kwargs: dict = {}
    if sys.platform == "win32":
        kwargs["creationflags"] = getattr(subprocess, "CREATE_NO_WINDOW", 0)
    # The owner's environment: a program started through `env node` needs the owner's PATH too.
    env = dict(shell_env() if environ is None else environ, PYTHONIOENCODING="utf-8")
    try:
        proc = popen(command, stdin=subprocess.PIPE if stdin_text is not None else subprocess.DEVNULL,
                     stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=cwd, env=env, **kwargs)
    except OSError as exc:
        return Result(False, detail=f"could not start {harness.name}: {exc.__class__.__name__}")

    chunks: "queue.Queue[bytes | None]" = queue.Queue()
    errors: list[bytes] = []

    def feed() -> None:
        try:
            proc.stdin.write(stdin_text.encode("utf-8"))
            proc.stdin.close()
        except OSError:
            pass  # the program exited before reading everything

    def read_out() -> None:
        while True:
            data = proc.stdout.read1(65536) if hasattr(proc.stdout, "read1") else proc.stdout.read(65536)
            if not data:
                break
            chunks.put(data)
        chunks.put(None)

    def read_err() -> None:
        for line in proc.stderr:
            errors.append(line)
            del errors[:-20]

    threads = [threading.Thread(target=read_out, daemon=True), threading.Thread(target=read_err, daemon=True)]
    if stdin_text is not None:
        threads.append(threading.Thread(target=feed, daemon=True))
    for thread in threads:
        thread.start()

    state: dict = {}
    decoder = codecs.getincrementaldecoder("utf-8")(errors="replace")
    pending = b""       # codex: an unfinished line; grok: everything printed so far
    deadline = time.monotonic() + timeout
    timed_out = False
    while True:
        try:
            data = chunks.get(timeout=1.0)
        except queue.Empty:
            if time.monotonic() > deadline:
                timed_out = True
                proc.kill()
                break
            continue
        if data is None:
            break
        if harness.output == "text":
            # A multi-byte character may be split across two reads; the decoder holds the start.
            text = decoder.decode(data)
            if text:
                emit(text)
        elif harness.output == "codex":
            pending += data
            *lines, pending = pending.split(b"\n")
            for line in lines:
                text = _decode_codex(line.decode("utf-8", "replace"), state)
                if text:
                    emit(text)
        else:
            pending += data
    for thread in threads:
        thread.join(5.0)
    code = proc.wait()

    if harness.output == "text":
        text = decoder.decode(b"", final=True)
        if text:
            emit(text)
    elif harness.output == "codex" and pending:
        text = _decode_codex(pending.decode("utf-8", "replace"), state)
        if text:
            emit(text)
    elif harness.output == "grok" and pending:
        text = _decode_grok(pending.decode("utf-8", "replace"), state)
        if text:
            emit(text)

    native_id = harness.native_id(session, state.get("native"))
    if timed_out:
        return Result(False, native_id, f"{harness.name} did not finish within {int(timeout)}s and was stopped")
    if code != 0 or state.get("error"):
        tail = b"".join(errors).decode("utf-8", "replace").strip()[-STDERR_TAIL:]
        detail = state.get("error") or tail or "no error output"
        return Result(False, native_id, f"{harness.name} exited with status {code}: {detail}")
    return Result(True, native_id)


# ----- the bridge ------------------------------------------------------------------

def _pieces(text: str, limit: int = PIECE_CHARS) -> list[str]:
    """Cut text into pieces of at most ``limit`` characters, at a line end where there is one."""
    pieces = []
    while len(text) > limit:
        cut = text.rfind("\n", 0, limit)
        cut = cut + 1 if cut > limit // 2 else limit
        pieces.append(text[:cut])
        text = text[cut:]
    return pieces + [text]


class Bridge:
    """Finds the owner's prompts in Honcho and runs each once. One worker per conversation, so a
    long turn in one does not hold back another, or the mail poller."""

    def __init__(self, cfg, store, api: Api, *, harnesses=HARNESSES, find=find_binary, run=run_prompt,
                 clock=time.time):
        self.cfg, self.store, self.api = cfg, store, api
        self._harnesses, self._find, self._run, self._clock = harnesses, find, run, clock
        self._workers: dict[str, threading.Thread] = {}
        self._lock = threading.Lock()
        self._card_at = 0.0
        self._card_ids: str | None = None
        settings = getattr(cfg, "geminus", None) or {}
        self.enabled = settings.get("enabled", True) is not False
        self.default_cwd = str(settings.get("cwd") or Path.home())
        self.extra_args = settings.get("args") if isinstance(settings.get("args"), dict) else {}
        self.timeout = float(settings.get("timeout") or DEFAULT_TIMEOUT)
        self.human_auth = str(settings.get("human_auth") or "dual").lower()

    def directory(self, asked: str) -> str:
        """Where a conversation runs: the directory its owner named, read as they would type it on
        this computer. ``~`` is their home, a relative path starts at the default directory, and no
        name means the default directory itself."""
        if not asked:
            return self.default_cwd
        return os.path.normpath(os.path.join(self.default_cwd, os.path.expanduser(asked)))

    # -- node card --

    def publish_card(self, *, force: bool = False) -> bool:
        """Tell geminus which agent programs this computer has. Returns whether a card was written."""
        now = self._clock()
        found = [h for h, _ in available(self.cfg, self._harnesses, self._find)]
        ids = card_text(found)
        if not force and ids == self._card_ids and now - self._card_at < CARD_SECONDS:
            return False
        self.api.set_peer_metadata(self.cfg.me, NODE_KEY,
                                   node_card(self.cfg.secret, self.cfg.me, found, now, self.default_cwd))
        self._card_at, self._card_ids = now, ids
        return True

    # -- finding work --

    def poll(self) -> int:
        """Start a worker for every conversation with something to run. Returns how many started."""
        if not self.enabled:
            return 0
        try:
            self.publish_card()
        except MailboxError as exc:
            log.warning("geminus: node card not published: %s", exc)
        started = 0
        for session in self.api.pending_sessions(self.cfg.me):
            sid = str(session["id"])
            with self._lock:
                worker = self._workers.get(sid)
                if worker is not None and worker.is_alive():
                    continue  # it re-reads the conversation before it stops
                worker = threading.Thread(target=self._serve, args=(session,), name=f"geminus-{sid}", daemon=True)
                self._workers[sid] = worker
            worker.start()
            started += 1
        return started

    def wait(self, timeout: float = 30.0) -> None:
        """Wait for the running workers; for tests and for a one-shot poll."""
        with self._lock:
            workers = list(self._workers.values())
        for worker in workers:
            worker.join(timeout)

    def _serve(self, session: dict) -> None:
        sid = str(session["id"])
        try:
            while self._serve_once(session):
                pass
        except MailboxError as exc:
            log.warning("geminus: conversation %s stopped: %s", sid, exc)
        except Exception:
            log.exception("geminus: conversation %s failed", sid)

    def _serve_once(self, session: dict) -> bool:
        """Clear the pending mark, then run every prompt that is waiting. True means "look again"."""
        sid = str(session["id"])
        metadata = session.get("metadata") if isinstance(session.get("metadata"), dict) else {}
        info = metadata.get(KIND) if isinstance(metadata.get(KIND), dict) else {}
        # Cleared before the read: a prompt written after this is either in the read below or sets
        # the mark again.
        self.api.set_session_metadata(sid, {**metadata, KIND: {**info, "pending": False}})
        messages = self.api.messages(sid)
        ran = False
        for message in messages:
            if self._runnable(sid, info, message):
                self._turn(sid, info, message, messages)
                ran = True
        return ran

    def _runnable(self, sid: str, info: dict, message: dict) -> bool:
        meta = (message.get("metadata") or {}).get(KIND)
        if not isinstance(meta, dict) or meta.get("role") != "prompt":
            return False
        if meta.get("state") not in (None, "queued"):
            return False
        author, nonce, sent_at = message.get("peer_id"), meta.get("nonce"), meta.get("ts")
        content = message.get("content")
        if not isinstance(author, str) or not isinstance(nonce, str) or type(sent_at) is not int \
                or not isinstance(content, str) or not message.get("id"):
            return False
        member = self.cfg.membership.get(author)
        if member is None or member.kind != "human" or member.status != "active" or info.get("human") != author:
            self._refuse(sid, message, "the author is not a human member of this conversation")
            return False
        if info.get("persona") != self.cfg.me:
            return False
        model, effort = meta.get("model", ""), meta.get("effort", "")
        if not valid_setting("model", model) or not valid_setting("effort", effort):
            self._refuse(sid, message, "the model or effort is not a usable name")
            return False
        label = prompt_label(sid, self.cfg.me, str(info.get("harness") or ""), author, nonce,
                             str(info.get("cwd") or ""), model, effort)
        ok_sig, sig_err = self._verify_human_prompt(author, content, label, sent_at, meta)
        if not ok_sig:
            self._refuse(sid, message, sig_err)
            return False
        now = self._clock()
        if sent_at < now - MAX_AGE or sent_at > now + CLOCK_SKEW:
            self._refuse(sid, message, "the prompt is too old, or dated in the future")
            return False
        if self.store.is_seen("geminus:" + nonce):
            return False  # already run here; a copy, or its state was reset
        return True

    def _verify_human_prompt(self, author: str, content: str, label: str, sent_at: int, meta: dict) -> tuple[bool, str]:
        sig_alg = meta.get("sig_alg") or ""
        sig = meta.get("sig")
        if sig_alg == "ed25519":
            pubkeys = getattr(self.cfg, "human_pubkeys", None) or {}
            pub = pubkeys.get(author)
            if not pub:
                return False, "no Ed25519 public key configured for human author"
            if not verify_human_ed25519(pub, content, label, sent_at, sig):
                return False, "the Ed25519 signature does not verify"
            return True, ""
        if sig_alg in ("", "hmac-sha256"):
            if self.human_auth == "ed25519_only":
                return False, "this host requires an Ed25519 human signature"
            if not verify(self.cfg.secret, content, label, sent_at, sig):
                return False, "the signature does not verify"
            return True, ""
        return False, "unsupported signature algorithm"

    def _state(self, sid: str, message: dict, state: str, detail: str = "") -> None:
        metadata = dict(message.get("metadata") or {})
        meta = dict(metadata.get(KIND) or {})
        meta["state"] = state
        if detail:
            meta["detail"] = detail
        else:
            meta.pop("detail", None)
        metadata[KIND] = meta
        message["metadata"] = metadata
        try:
            self.api.set_message_metadata(sid, str(message["id"]), metadata)
        except MailboxError as exc:
            log.warning("geminus: state %s of prompt %s not recorded: %s", state, message.get("id"), exc)

    def _refuse(self, sid: str, message: dict, why: str) -> None:
        log.warning("geminus: prompt %s in %s refused: %s", message.get("id"), sid, why)
        self._state(sid, message, "refused", why)

    def _native(self, sid: str, messages: list[dict]) -> str | None:
        """The agent program's own session id, from the newest output of this conversation that
        this computer signed. An unsigned one is ignored: anyone holding the Honcho token could
        otherwise point the next prompt at a different session of the program."""
        for message in reversed(messages):
            meta = (message.get("metadata") or {}).get(KIND)
            if not isinstance(meta, dict) or meta.get("role") != "output" or message.get("peer_id") != self.cfg.me:
                continue
            native, content, stamp = meta.get("native"), message.get("content"), meta.get("ts")
            if not isinstance(native, str) or not native or not isinstance(content, str) or type(stamp) is not int:
                continue
            label = output_label(sid, str(meta.get("turn")), meta.get("seq") if type(meta.get("seq")) is int else 0,
                                 meta.get("final") is True, meta.get("ok") is True, native)
            if verify(self.cfg.secret, content, label, stamp, meta.get("sig")):
                return native
        return None

    def _turn(self, sid: str, info: dict, message: dict, messages: list[dict]) -> None:
        turn, nonce = str(message["id"]), message["metadata"][KIND]["nonce"]
        # Remembered before the program starts: a prompt is run at most once, even if this
        # computer goes down in the middle of it.
        self.store.mark_seen("geminus:" + nonce)
        harness = next((h for h in self._harnesses if h.id == info.get("harness")), None)
        binary = self._find(harness.binary, self.cfg) if harness else ""
        seq = 0
        buffer: list[str] = []
        flushed_at = time.monotonic()
        native_box: list[str] = []

        def post(text: str, *, final: bool, ok: bool = True) -> None:
            nonlocal seq
            seq += 1
            stamp = int(self._clock())
            native = native_box[0] if native_box else ""
            meta = {"v": 1, "role": "output", "turn": turn, "seq": seq, "final": final, "ok": ok, "ts": stamp,
                    "sig": sign(self.cfg.secret, text, output_label(sid, turn, seq, final, ok, native), stamp)}
            if native:
                meta["native"] = native
            posted = self.api.post(sid, self.cfg.me, text, {KIND: meta})
            messages.append(posted)

        def flush(*, force: bool = False) -> None:
            nonlocal flushed_at
            text = "".join(buffer)
            if not text or (not force and len(text) < PIECE_CHARS and time.monotonic() - flushed_at < FLUSH_SECONDS):
                return
            buffer.clear()
            for piece in _pieces(text):
                post(piece, final=False)
            flushed_at = time.monotonic()

        def emit(text: str) -> None:
            buffer.append(text)
            flush()

        if harness is None or not binary:
            name = harness.name if harness else str(info.get("harness"))
            self._state(sid, message, "failed", f"{name} is not installed on {self.cfg.me}")
            post(f"{name} is not installed on {self.cfg.me}.", final=True, ok=False)
            return
        asked_cwd = str(info.get("cwd") or "")
        cwd = self.directory(asked_cwd)
        if not os.path.isdir(cwd):
            named = cwd if cwd == asked_cwd else f"{asked_cwd} ({cwd})"
            self._state(sid, message, "failed", "the working directory does not exist")
            post(f"The working directory {named} does not exist on {self.cfg.me}.", final=True, ok=False)
            return

        # The owner's model and effort for this prompt; _runnable checked their form and signature.
        asked = {name: message["metadata"][KIND].get(name, "") for name in ("model", "effort")}
        for name, value in asked.items():
            if value and name not in harness.options:
                # Running it anyway would be a different run from the one the owner asked for.
                why = f"{harness.name} has no {name} setting that geminus can pass"
                self._state(sid, message, "failed", why)
                post(f"{why}. Clear it with /{name} default and send the prompt again.", final=True, ok=False)
                return

        # What the conversation's earlier prompts carried, for a program that is told its settings
        # inside the session.
        earlier = []
        for other in messages:
            if other.get("id") == message["id"]:
                break
            meta = (other.get("metadata") or {}).get(KIND)
            if isinstance(meta, dict) and meta.get("role") == "prompt" and other.get("peer_id") == message.get("peer_id"):
                earlier.append(meta)
        ever = {name: any(isinstance(m.get(name), str) and m.get(name) for m in earlier) for name in asked}
        last = earlier[-1] if earlier else {}
        try:
            preludes = harness.preludes(asked, ever)
        except CannotGoBack as exc:
            why = (f"{harness.name} cannot go back to its own {exc} inside a conversation that has had one set; "
                   f"set a {exc} again or start a new conversation")
            self._state(sid, message, "failed", why)
            post(why + ".", final=True, ok=False)
            return

        self._state(sid, message, "running")
        log.info("geminus: running prompt %s of %s on %s", turn, sid, harness.id)
        extra = tuple(str(a) for a in self.extra_args.get(harness.id) or ())
        tune = tuple(harness.tuning(asked["model"], asked["effort"]))
        native = self._native(sid, messages)

        def run(known, text=message["content"], sink=emit):
            return self._run(harness, binary, sid, known, text, cwd=cwd, emit=sink, extra_args=extra,
                             timeout=self.timeout, tune=tune)

        result = None
        for name, line in preludes:
            # What the program says to a setting is shown when the owner has just changed it.
            changed = asked[name] != (last.get(name) or "")
            took = run(native, line, emit if changed else (lambda _text: None))
            if not took.ok:
                result = Result(False, took.native, f"{harness.name} did not take the {name} setting: {took.detail}")
                break
            native = took.native or native
        if result is None:
            result = run(native)
            if not result.ok and native is None and seq == 0 and not buffer and "already in use" in result.detail:
                # An earlier turn created the program's session but its output never reached Honcho.
                result = run(harness.native_id(sid, None))
        if result.native:
            native_box.append(result.native)
        rest = "".join(buffer)
        buffer.clear()
        pieces = _pieces(rest) if rest else [""]
        if result.ok:
            for piece in pieces[:-1]:
                post(piece, final=False)
            post(pieces[-1], final=True)
            self._state(sid, message, "done")
        else:
            for piece in pieces:
                if piece:
                    post(piece, final=False)
            post(result.detail, final=True, ok=False)
            self._state(sid, message, "failed", result.detail)
        log.info("geminus: prompt %s of %s: %s", turn, sid, "done" if result.ok else result.detail)
