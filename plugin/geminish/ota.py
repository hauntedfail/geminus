"""Automatic OTA updates for geminish (Hermes plugin) and geminus (CLI).

Every release is built and signed on sibyl (geminus-latest-channel) using the
Ed25519 release key that never leaves sibyl. Every HTTP request is signed with
the group secret using the same Geminus-Time / Geminus-Body-SHA256 /
Geminus-Signature headers as internal/updates/request.go.

Standard library only (Python 3.9+), works on macOS, Linux, and Windows.
"""
from __future__ import annotations

import hashlib
import io
import json
import logging
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

from .ed25519_verify import verify_ed25519
from .mailbox import _NoRedirect, sign

log = logging.getLogger("geminish.ota")

DEFAULT_BASE = "https://sibyl.haunted.fail/geminus/updates"
PLUGIN_TARGET = "plugin-noarch"
RELEASE_HEADER = "geminus-release"
# Public half of sibyl's Ed25519 release signing key (matches internal/updates/key.go).
RELEASE_PUBKEY_HEX = "1780a76724aaa94c9ca5295c56e6f6f879c867f37fd88497dc3997b95dcc10f8"
EMPTY_SHA256 = hashlib.sha256(b"").hexdigest()
MAX_BUNDLE_BYTES = 32 << 20
DEFAULT_INTERVAL_SECONDS = 300.0
HTTP_TIMEOUT = 30.0
USER_AGENT = "geminish-ota/0.9"
_OPENER = urllib.request.build_opener(_NoRedirect)
_SEMVER_RE = re.compile(r"^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(?:[-+].*)?$")
_EXACT_RE = re.compile(r"^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$")
_HASH_RE = re.compile(r"^[0-9a-f]{64}$")
_TARGET_RE = re.compile(r"^[a-z0-9]+-[a-z0-9]+$")


class OTAError(RuntimeError):
    """An update check, download, verification, or install step failed."""


def parse_version(text: str) -> tuple[tuple[int, int, int], bool] | None:
    """Return ((major, minor, patch), is_exact) or None."""
    if not isinstance(text, str):
        return None
    s = text.strip()
    m = _SEMVER_RE.match(s)
    if not m:
        return None
    return (int(m.group(1)), int(m.group(2)), int(m.group(3))), bool(_EXACT_RE.match(s))


def is_newer(current: str, release: str) -> bool:
    """True iff release is strictly newer than current (matching internal/updates/version.go)."""
    rel = parse_version(release)
    if rel is None or not rel[1]:
        return False
    cur = parse_version(current)
    if cur is None:
        return True
    if cur[0] != rel[0]:
        return cur[0] < rel[0]
    return not cur[1] and rel[1]


def manifest_text(version: str, ts: int, files: dict[str, dict]) -> bytes:
    """Canonical text covered by the Ed25519 release signature (matches manifest.go)."""
    lines = [RELEASE_HEADER, version, str(int(ts))]
    for target in sorted(files):
        entry = files[target]
        lines.append(f"{target} {entry['sha256']} {int(entry['size'])}")
    return "\n".join(lines).encode("utf-8")


def verify_manifest(manifest: object, pubkey_hex: str = RELEASE_PUBKEY_HEX) -> dict:
    """Validate structure and Ed25519 signature of a release manifest."""
    if not isinstance(manifest, dict):
        raise OTAError("manifest is not a JSON object")
    version = manifest.get("version")
    ts = manifest.get("ts")
    files = manifest.get("files")
    sig_hex = manifest.get("sig")
    if not isinstance(version, str) or not _EXACT_RE.match(version):
        raise OTAError("manifest version is not X.Y.Z")
    if type(ts) is not int:
        raise OTAError("manifest ts is not an integer")
    if not isinstance(files, dict) or not files:
        raise OTAError("manifest has no files")
    for target, entry in files.items():
        if not isinstance(target, str) or not _TARGET_RE.match(target):
            raise OTAError(f"invalid target {target!r}")
        if not isinstance(entry, dict):
            raise OTAError(f"invalid file entry for {target}")
        digest = entry.get("sha256")
        size = entry.get("size")
        if not isinstance(digest, str) or not _HASH_RE.match(digest):
            raise OTAError(f"invalid sha256 for {target}")
        if type(size) is not int or size <= 0 or size > MAX_BUNDLE_BYTES:
            raise OTAError(f"invalid size for {target}")
    if not isinstance(sig_hex, str) or len(sig_hex) != 128:
        raise OTAError("manifest signature missing or wrong length")
    try:
        sig = bytes.fromhex(sig_hex)
        pubkey = bytes.fromhex(pubkey_hex)
    except ValueError:
        raise OTAError("manifest signature or pubkey is not hex") from None
    if not verify_ed25519(pubkey, manifest_text(version, ts, files), sig):
        raise OTAError("manifest Ed25519 signature failed verification")
    return manifest


def sign_request_headers(secret: bytes, method: str, path: str, *,
                         body_sha: str = EMPTY_SHA256, now: float | None = None) -> dict[str, str]:
    """Headers matching internal/updates/request.go SignRequest."""
    ts = int(time.time() if now is None else now)
    label = f"geminus-updates-request:{method}:{path}:{body_sha}"
    sig = sign(secret, "", label, ts)
    return {
        "User-Agent": USER_AGENT,
        "Geminus-Time": str(ts),
        "Geminus-Body-SHA256": body_sha,
        "Geminus-Signature": sig,
    }


def _request(secret: bytes, url: str, *, method: str = "GET", timeout: float = HTTP_TIMEOUT,
             opener=None) -> tuple[int, bytes]:
    parts = urllib.parse.urlsplit(url)
    path = parts.path or "/"
    headers = sign_request_headers(secret, method, path)
    req = urllib.request.Request(url, data=None, method=method, headers=headers)
    op = opener or _OPENER
    try:
        with op.open(req, timeout=timeout) as resp:
            return int(resp.status), resp.read(MAX_BUNDLE_BYTES + 1)
    except urllib.error.HTTPError as exc:
        body = exc.read(4096) if hasattr(exc, "read") else b""
        return int(exc.code), body
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        raise OTAError(f"HTTP {method} {url} failed: {exc.__class__.__name__}") from None


def ota_settings(cfg) -> dict:
    raw = getattr(cfg, "ota", None)
    if raw is False:
        return {"enabled": False}
    settings = dict(raw) if isinstance(raw, dict) else {}
    return {
        "enabled": settings.get("enabled", True) is not False,
        "base_url": str(settings.get("base_url") or os.environ.get("GEMINUS_UPDATES_URL") or DEFAULT_BASE).rstrip("/"),
        "interval_seconds": max(30.0, float(settings.get("interval_seconds") or DEFAULT_INTERVAL_SECONDS)),
        "pubkey_hex": str(settings.get("pubkey_hex") or RELEASE_PUBKEY_HEX),
        "update_geminus": settings.get("update_geminus", True) is not False,
    }


def fetch_latest_plugin_manifest(cfg, *, opener=None) -> dict | None:
    """Return verified latest geminish manifest, or None if no release is published yet."""
    s = ota_settings(cfg)
    url = f"{s['base_url']}/geminish/latest"
    status, body = _request(cfg.secret, url, method="GET", opener=opener)
    if status == 404:
        return None
    if status != 200:
        raise OTAError(f"plugin channel answered HTTP {status}: {body.decode('utf-8', 'replace').strip()[:200]}")
    try:
        data = json.loads(body.decode("utf-8"))
    except ValueError:
        raise OTAError("plugin channel returned non-JSON manifest") from None
    return verify_manifest(data, pubkey_hex=s["pubkey_hex"])


def _safe_extract_bundle(archive_bytes: bytes, dest_dir: Path, expected_version: str) -> Path:
    """Extract a geminish tar.gz bundle safely into dest_dir/geminish and verify its version."""
    out_root = dest_dir / "geminish"
    out_root.mkdir(parents=True, exist_ok=True)
    with tarfile.open(fileobj=io.BytesIO(archive_bytes), mode="r:gz") as tf:
        members = tf.getmembers()
        if not members:
            raise OTAError("plugin bundle is empty")
        extracted = 0
        for member in members:
            if member.isdir():
                continue
            if not member.isfile():
                raise OTAError(f"plugin bundle contains non-regular entry: {member.name!r}")
            name = member.name.lstrip("./")
            if name.startswith("geminish/"):
                rel = name[len("geminish/"):]
            else:
                rel = name
            parts = Path(rel).parts
            if not parts or any(p in ("", ".", "..") for p in parts) or Path(rel).is_absolute():
                raise OTAError(f"unsafe path in plugin bundle: {member.name!r}")
            if parts[0] == "geminise.json":
                raise OTAError("plugin bundle must not contain geminise.json")
            target = (out_root / rel).resolve()
            if out_root.resolve() not in target.parents:
                raise OTAError(f"path escapes destination: {member.name!r}")
            target.parent.mkdir(parents=True, exist_ok=True)
            src = tf.extractfile(member)
            if src is None:
                raise OTAError(f"could not read {member.name!r} from bundle")
            target.write_bytes(src.read())
            if target.name == "cli.py" and os.name == "posix":
                os.chmod(target, 0o755)
            extracted += 1
        if extracted == 0:
            raise OTAError("plugin bundle contained no files")

    plugin_yaml = out_root / "plugin.yaml"
    init_py = out_root / "__init__.py"
    if not plugin_yaml.is_file() or not init_py.is_file():
        raise OTAError("plugin bundle missing plugin.yaml or __init__.py")
    if f"version: {expected_version}" not in plugin_yaml.read_text(encoding="utf-8"):
        raise OTAError(f"plugin.yaml version mismatch (expected {expected_version})")
    if f'__version__ = "{expected_version}"' not in init_py.read_text(encoding="utf-8"):
        raise OTAError(f"__init__.py version mismatch (expected {expected_version})")
    return out_root


def _install_extracted(new_dir: Path, target_dir: Path, old_version: str) -> Path:
    """Back up target_dir and replace it with new_dir cleanly on POSIX and Windows."""
    backup_root = target_dir.parent.parent / "plugins-backup"
    backup_root.mkdir(parents=True, exist_ok=True)
    ts = time.strftime("%Y%m%dT%H%M%S", time.gmtime())
    backup_dir = backup_root / f"geminish-{old_version}-{ts}"
    if target_dir.exists():
        shutil.copytree(target_dir, backup_dir, dirs_exist_ok=True,
                        ignore=shutil.ignore_patterns("__pycache__", "*.pyc"))

    # Preserve geminise.json if the user stored it inside the plugin directory.
    local_cfg = target_dir / "geminise.json"
    if local_cfg.is_file() and not (new_dir / "geminise.json").exists():
        shutil.copy2(local_cfg, new_dir / "geminise.json")

    if os.name == "posix":
        staged = target_dir.parent / f".geminish-new-{os.getpid()}"
        if staged.exists():
            shutil.rmtree(staged)
        shutil.copytree(new_dir, staged)
        old_tmp = target_dir.parent / f".geminish-old-{os.getpid()}"
        if old_tmp.exists():
            shutil.rmtree(old_tmp)
        try:
            if target_dir.exists():
                os.replace(target_dir, old_tmp)
            os.replace(staged, target_dir)
        except OSError:
            if old_tmp.exists() and not target_dir.exists():
                os.replace(old_tmp, target_dir)
            raise
        finally:
            if old_tmp.exists():
                shutil.rmtree(old_tmp, ignore_errors=True)
    else:
        # Windows: open .pyc or directory handles can block directory rename, so overwrite files in place.
        target_dir.mkdir(parents=True, exist_ok=True)
        new_files = {p.relative_to(new_dir) for p in new_dir.rglob("*") if p.is_file()}
        for rel in new_files:
            dst = target_dir / rel
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(new_dir / rel, dst)
        for existing in list(target_dir.rglob("*.py")):
            rel = existing.relative_to(target_dir)
            if rel not in new_files:
                try:
                    existing.unlink()
                except OSError:
                    pass
    return backup_dir


def update_geminish(cfg, current_version: str, *, plugin_dir: Path | None = None, opener=None) -> dict:
    """Check for a newer geminish release and apply it atomically if available."""
    s = ota_settings(cfg)
    if not s["enabled"]:
        return {"ok": True, "updated": False, "reason": "disabled"}
    manifest = fetch_latest_plugin_manifest(cfg, opener=opener)
    if manifest is None:
        return {"ok": True, "updated": False, "reason": "no_release"}
    latest = manifest["version"]
    if not is_newer(current_version, latest):
        return {"ok": True, "updated": False, "current": current_version, "latest": latest}
    entry = manifest["files"].get(PLUGIN_TARGET)
    if not entry:
        raise OTAError(f"release {latest} has no {PLUGIN_TARGET} bundle")
    url = f"{s['base_url']}/geminish/{latest}/{PLUGIN_TARGET}"
    status, payload = _request(cfg.secret, url, method="GET", opener=opener)
    if status != 200:
        raise OTAError(f"download {latest} answered HTTP {status}")
    if len(payload) != entry["size"] or hashlib.sha256(payload).hexdigest() != entry["sha256"]:
        raise OTAError(f"bundle {latest} size or sha256 mismatch")

    target_dir = plugin_dir or Path(__file__).resolve().parent
    with tempfile.TemporaryDirectory(prefix="geminish-ota-") as tmp:
        extracted = _safe_extract_bundle(payload, Path(tmp), latest)
        backup = _install_extracted(extracted, target_dir, current_version)
    log.info("ota: updated geminish %s -> %s (backup=%s)", current_version, latest, backup)
    return {"ok": True, "updated": True, "from": current_version, "to": latest, "backup": str(backup)}


def find_geminus_bin() -> str:
    candidates = [
        shutil.which("geminus") or "",
        str(Path.home() / ".local" / "bin" / "geminus"),
        str(Path.home() / ".local" / "bin" / "geminus.exe"),
    ]
    local_app = os.environ.get("LOCALAPPDATA")
    if local_app:
        candidates += [
            str(Path(local_app) / "geminus" / "geminus.exe"),
            str(Path(local_app) / "Programs" / "geminus" / "geminus.exe"),
        ]
    for c in candidates:
        if c and Path(c).is_file():
            return c
    return ""


def _geminus_current_version(binary: str) -> str:
    try:
        done = subprocess.run([binary, "--version"], capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        return ""
    text = (done.stdout or done.stderr or "").strip()
    m = re.search(r"geminus\s+([0-9]+\.[0-9]+\.[0-9]+(?:[-+][^\s)]+)?)", text)
    return m.group(1) if m else ""


def update_geminus_cli(cfg, *, opener=None) -> dict:
    """Check if a newer geminus CLI release exists and run `geminus update` if so."""
    s = ota_settings(cfg)
    if not s["enabled"] or not s["update_geminus"]:
        return {"ok": True, "updated": False, "reason": "disabled"}
    binary = find_geminus_bin()
    if not binary:
        return {"ok": True, "updated": False, "reason": "not_installed"}
    status, body = _request(cfg.secret, f"{s['base_url']}/latest", method="GET", opener=opener)
    if status == 404:
        return {"ok": True, "updated": False, "reason": "no_release"}
    if status != 200:
        raise OTAError(f"geminus channel answered HTTP {status}")
    manifest = verify_manifest(json.loads(body.decode("utf-8")), pubkey_hex=s["pubkey_hex"])
    current = _geminus_current_version(binary)
    if not is_newer(current, manifest["version"]):
        return {"ok": True, "updated": False, "current": current, "latest": manifest["version"]}
    env = dict(os.environ, GEMINUS_UPDATES_URL=s["base_url"])
    kwargs: dict = {}
    if sys.platform == "win32":
        kwargs["creationflags"] = getattr(subprocess, "CREATE_NO_WINDOW", 0)
    done = subprocess.run([binary, "update"], capture_output=True, text=True, timeout=120, env=env, **kwargs)
    if done.returncode != 0:
        raise OTAError(f"geminus update failed ({done.returncode}): {(done.stderr or done.stdout).strip()[:200]}")
    log.info("ota: updated geminus %s -> %s", current or "unknown", manifest["version"])
    return {"ok": True, "updated": True, "from": current, "to": manifest["version"]}


def trigger_plugin_release(cfg, version: str, *, poll_seconds: float = 2.0, timeout: float = 300.0,
                           opener=None) -> dict:
    """Ask sibyl's geminus-latest-channel to build, sign, and publish geminish <version>."""
    if not _EXACT_RE.match(version):
        raise OTAError(f"{version!r} is not X.Y.Z")
    s = ota_settings(cfg)
    base = s["base_url"]
    status, body = _request(cfg.secret, f"{base}/geminish/{version}", method="POST", opener=opener)
    if status not in (200, 202):
        raise OTAError(f"POST /geminish/{version} answered HTTP {status}: {body.decode('utf-8', 'replace').strip()[:300]}")
    state = json.loads(body.decode("utf-8"))
    deadline = time.monotonic() + timeout
    while state.get("state") == "building":
        if time.monotonic() >= deadline:
            raise OTAError(f"plugin release {version} build timed out")
        time.sleep(poll_seconds)
        st_code, st_body = _request(cfg.secret, f"{base}/geminish/{version}/build", method="GET", opener=opener)
        if st_code != 200:
            raise OTAError(f"build poll answered HTTP {st_code}: {st_body.decode('utf-8', 'replace').strip()[:200]}")
        state = json.loads(st_body.decode("utf-8"))
    if state.get("state") != "published":
        raise OTAError(f"plugin release {version} {state.get('state')}: {state.get('error', '')}")
    m_status, m_body = _request(cfg.secret, f"{base}/geminish/{version}", method="GET", opener=opener)
    if m_status != 200:
        raise OTAError(f"manifest fetch answered HTTP {m_status}")
    return verify_manifest(json.loads(m_body.decode("utf-8")), pubkey_hex=s["pubkey_hex"])


def restart_poller() -> None:
    """Replace the running poller process so the newly installed geminish code takes effect."""
    log.info("ota: restarting poller to load updated geminish")
    if os.name == "posix":
        os.execv(sys.executable, [sys.executable, *sys.argv])
    else:
        creationflags = getattr(subprocess, "DETACHED_PROCESS", 0) | getattr(subprocess, "CREATE_NO_WINDOW", 0)
        subprocess.Popen([sys.executable, *sys.argv], close_fds=True, creationflags=creationflags)
        os._exit(0)
