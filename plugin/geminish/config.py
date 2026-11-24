"""Local identity and connection settings for one geminish host.

Nothing here is taken from a message. The peer name, the member list and the
signing secret come from ``geminise.json`` on this machine; the Honcho
connection comes from the Hermes ``honcho.json`` unless overridden.
"""
from __future__ import annotations

from dataclasses import dataclass, field
import json
import os
from pathlib import Path
import shutil
import stat
import sys
import urllib.parse

from .policy import HOP_CEILING, Member, Membership

DEFAULT_SESSION = "geminise-main"
MIN_SECRET_BYTES = 32


class ConfigError(ValueError):
    """The local configuration is missing or unusable. Never includes secrets."""


@dataclass(frozen=True)
class Config:
    me: str
    membership: Membership
    session: str
    base_url: str
    api_key: str
    workspace: str
    secret: bytes
    state_dir: Path
    hermes_bin: str
    poll_seconds: float = 10.0
    wake: bool = True
    wake_timeout: int = 900
    max_wakes_per_hour: int = 6
    max_hops: int = 1
    http_timeout: float = 20.0
    push: bool = True
    # Settings of the geminus bridge (see bridge.py): enabled, cwd, args, timeout, human_auth. None = defaults.
    geminus: object = None
    # Optional Ed25519 public keys for human peers: {peer_name: tuple of 32-byte raw pubkeys}.
    human_pubkeys: dict[str, tuple[bytes, ...]] = field(default_factory=dict)
    # Settings of automatic OTA updates (see ota.py): enabled, base_url, interval_seconds, update_geminus.
    ota: object = None

    @property
    def store_path(self) -> Path:
        return self.state_dir / "state.db"


def plugin_dir() -> Path:
    return Path(__file__).resolve().parent


def hermes_home() -> Path:
    """$HERMES_HOME, else the home this plugin is installed under, else ~/.hermes."""
    env = os.environ.get("HERMES_HOME")
    if env:
        return Path(env)
    installed = plugin_dir().parent.parent
    if (installed / "honcho.json").is_file() or (installed / "config.yaml").is_file():
        return installed
    return Path.home() / ".hermes"


def config_path() -> Path:
    """``GEMINISE_CONFIG`` is the only source when set, so tests never read a host identity."""
    override = os.environ.get("GEMINISE_CONFIG")
    if override:
        return Path(override)
    beside = plugin_dir() / "geminise.json"
    return beside if beside.is_file() else hermes_home() / "geminise.json"


def _read_json(path: Path, what: str) -> dict:
    try:
        data = json.loads(path.read_text(encoding="utf-8-sig"))
    except OSError as exc:
        raise ConfigError(f"{what} not readable: {path} ({exc.__class__.__name__})") from None
    except ValueError:
        raise ConfigError(f"{what} is not valid JSON: {path}") from None
    if not isinstance(data, dict):
        raise ConfigError(f"{what} must be a JSON object: {path}")
    return data


def _private(path: Path) -> None:
    """A file holding the signing secret must not be readable by other users (POSIX only)."""
    if os.name != "posix":
        return
    mode = stat.S_IMODE(path.stat().st_mode)
    if mode & 0o077:
        raise ConfigError(f"{path} holds a secret and must be mode 0600 (is {mode:04o})")


def find_hermes_bin(configured: str = "") -> str:
    candidates = [configured, os.environ.get("GEMINISH_HERMES_BIN", ""), shutil.which("hermes") or ""]
    local = Path(os.environ.get("LOCALAPPDATA", Path.home() / "AppData" / "Local"))
    candidates += [
        str(Path.home() / ".local" / "bin" / "hermes"),
        str(hermes_home() / "hermes-agent" / ".hermes" / "bin" / "hermes"),
        str(local / "hermes" / "bin" / "hermes.exe"),
    ]
    for candidate in candidates:
        if candidate and (Path(candidate).is_file() or shutil.which(candidate)):
            return candidate
    return ""


def load(path: Path | None = None) -> Config:
    path = path or config_path()
    raw = _read_json(path, "geminise.json")
    me = str(raw.get("from") or "").strip()
    if not me:
        raise ConfigError("geminise.json needs 'from': this host's own peer name")

    members_raw = raw.get("members")
    if not isinstance(members_raw, dict) or not members_raw:
        raise ConfigError("geminise.json needs 'members': {peer: 'agent'|'human'}")
    try:
        membership = Membership(tuple(Member(str(p), str(k)) for p, k in members_raw.items()))
    except ValueError:
        raise ConfigError("geminise.json 'members' has an invalid peer name or kind") from None
    own = membership.get(me)
    if own is None or own.kind != "agent":
        raise ConfigError("'from' must be listed in 'members' as an agent")

    secret_hex = str(raw.get("secret") or "")
    try:
        secret = bytes.fromhex(secret_hex)
    except ValueError:
        raise ConfigError("geminise.json 'secret' must be hex") from None
    if len(secret) < MIN_SECRET_BYTES:
        raise ConfigError(f"geminise.json 'secret' must be at least {MIN_SECRET_BYTES} bytes of hex")
    _private(path)

    home = hermes_home()
    honcho = raw.get("honcho") if isinstance(raw.get("honcho"), dict) else None
    if honcho is None:
        honcho = _read_json(home / "honcho.json", "honcho.json")
    base_url = str(honcho.get("baseUrl") or "").rstrip("/")
    workspace = str(honcho.get("workspace") or "")
    url = urllib.parse.urlsplit(base_url)
    loopback = url.scheme == "http" and url.hostname in ("127.0.0.1", "localhost", "::1")
    if not url.hostname or url.username or not (url.scheme == "https" or loopback):
        raise ConfigError("Honcho baseUrl must be https, or http on loopback")
    if not workspace:
        raise ConfigError("Honcho workspace is not set")

    state_dir = Path(raw.get("state_dir") or home / "geminise")
    return Config(
        me=me,
        membership=membership,
        session=str(raw.get("session") or DEFAULT_SESSION),
        base_url=base_url,
        api_key=str(honcho.get("apiKey") or ""),
        workspace=workspace,
        secret=secret,
        state_dir=state_dir,
        hermes_bin=find_hermes_bin(str(raw.get("hermes_bin") or "")),
        poll_seconds=max(2.0, float(raw.get("poll_seconds") or 10)),
        wake=raw.get("wake", True) is not False,
        wake_timeout=int(raw.get("wake_timeout") or 900),
        max_wakes_per_hour=max(0, int(raw.get("max_wakes_per_hour", 6))),
        # 0 = only mail from a turn geminise did not start may start one here (the 0.3.0 rule).
        max_hops=min(HOP_CEILING, max(0, int(raw.get("max_hops", 1)))),
        http_timeout=float(raw.get("http_timeout") or 20),
        push=raw.get("push", True) is not False,
        geminus=_geminus(raw.get("geminus")),
        human_pubkeys=_human_pubkeys(raw.get("human_pubkeys")),
        ota=_ota(raw.get("ota")),
    )


def _ota(value: object) -> object:
    if value is False:
        return False
    if value is None:
        return None
    if not isinstance(value, dict):
        raise ConfigError("geminise.json 'ota' must be an object or false")
    return dict(value)


def _human_pubkeys(value: object) -> dict[str, tuple[bytes, ...]]:
    if value is None:
        return {}
    if not isinstance(value, dict):
        raise ConfigError("geminise.json 'human_pubkeys' must be an object mapping peer names to 64-hex Ed25519 key(s)")
    out: dict[str, tuple[bytes, ...]] = {}
    for peer, raw_val in value.items():
        if not isinstance(peer, str):
            raise ConfigError("geminise.json 'human_pubkeys' peer names must be strings")
        raw_list = [raw_val] if isinstance(raw_val, str) else raw_val
        if not isinstance(raw_list, list) or not raw_list or any(not isinstance(item, str) for item in raw_list):
            raise ConfigError(f"geminise.json 'human_pubkeys[{peer}]' must be a hex string or non-empty list of hex strings")
        keys: list[bytes] = []
        for raw_hex in raw_list:
            try:
                key = bytes.fromhex(raw_hex.strip())
            except ValueError:
                raise ConfigError(f"geminise.json 'human_pubkeys[{peer}]' must be hex") from None
            if len(key) != 32:
                raise ConfigError(f"geminise.json 'human_pubkeys[{peer}]' must be 32 bytes (64 hex chars)")
            keys.append(key)
        out[peer] = tuple(keys)
    return out


def _geminus(value: object) -> dict:
    """``"geminus": false`` turns the bridge off on this host; an object sets its options."""
    if value is False:
        return {"enabled": False}
    if not isinstance(value, dict):
        return {}
    args = value.get("args")
    if args is not None and (not isinstance(args, dict) or any(
            not isinstance(v, list) or any(not isinstance(a, str) for a in v) for v in args.values())):
        raise ConfigError("geminise.json 'geminus.args' must map a harness to a list of strings")
    human_auth = value.get("human_auth")
    if human_auth is not None and str(human_auth).lower() not in ("dual", "ed25519_only"):
        raise ConfigError("geminise.json 'geminus.human_auth' must be 'dual' or 'ed25519_only'")
    return dict(value)


def prepare_state_dir(cfg: Config) -> None:
    cfg.state_dir.mkdir(parents=True, exist_ok=True)
    if os.name == "posix":
        os.chmod(cfg.state_dir, 0o700)


def python_executable() -> str:
    return sys.executable
