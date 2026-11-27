"""Tests for pure-Python RFC 8439 ChaCha20-Poly1305 and GE2E v1 Go<->Python interop."""
from __future__ import annotations

from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
REPO = Path(__file__).resolve().parents[3]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from geminish import config, e2ee  # noqa: E402
from geminish.mailbox import Mailbox, MailboxError  # noqa: E402


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def _find_go() -> str:
    for cand in (shutil.which("go"), str(Path.home() / ".local" / "go" / "bin" / "go")):
        if cand and Path(cand).is_file():
            return cand
    return ""


class TestRFC8439(unittest.TestCase):
    def test_poly1305_section_252(self) -> None:
        key = bytes.fromhex("85d6be7857556d337f4452fe42d506a80103808afb0db2fd4abff6af4149f51b")
        msg = b"Cryptographic Forum Research Group"
        want = bytes.fromhex("a8061dc1305136c6c22b8baf0c0127a9")
        self.assertEqual(e2ee.poly1305_mac(key, msg), want)

    def test_aead_section_282(self) -> None:
        key = bytes.fromhex("808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f")
        nonce = bytes.fromhex("070000004041424344454647")
        aad = bytes.fromhex("50515253c0c1c2c3c4c5c6c7")
        pt = b"Ladies and Gentlemen of the class of '99: If I could offer you only one tip for the future, sunscreen would be it."
        want_ct = bytes.fromhex(
            "d31a8d34648e60db7b86afbc53ef7ec2a4aded51296e08fea9e2b5a736ee62d6"
            "3dbea45e8ca9671282fafb69da92728b1a71de0a9e060b2905d6a5b67ecd3b36"
            "92ddbd7f2d778b8c9803aee328091b58fab324e4fad675945585808b4831d7bc"
            "3ff4def08e4b7a9de576d26586cec64b6116"
        )
        want_tag = bytes.fromhex("1ae10b594f09e26a7e902ecbd0600691")

        sealed = e2ee.seal_aead(key, nonce, pt, aad)
        self.assertEqual(sealed, want_ct + want_tag)
        self.assertEqual(e2ee.open_aead(key, nonce, sealed, aad), pt)

        with self.assertRaises(e2ee.E2EEError):
            e2ee.open_aead(key, nonce, sealed, bytes([aad[0] ^ 1]) + aad[1:])
        with self.assertRaises(e2ee.E2EEError):
            e2ee.open_aead(key, nonce, sealed[:-1] + bytes([sealed[-1] ^ 1]), aad)


class TestEnvelopeAndConfig(unittest.TestCase):
    def test_envelope_roundtrip_and_tamper_detection(self) -> None:
        secret = b"\x42" * 32
        frame, ts, salt = e2ee.seal_request(
            secret,
            "POST",
            "/v3/workspaces/sibyl/sessions/s1/messages",
            "reverse=true",
            {"Authorization": "Bearer secret-tok"},
            b'{"content":"raw-for-honcho"}',
            ts=1795600000,
        )
        self.assertNotIn(b"/v3/workspaces", frame)
        self.assertNotIn(b"secret-tok", frame)
        self.assertNotIn(b"raw-for-honcho", frame)
        self.assertGreaterEqual(len(frame), e2ee.HEADER_SIZE + e2ee.MIN_PAD_BUCKET + e2ee.TAG_SIZE)

        obj, got_ts, got_salt = e2ee.open_request(secret, frame)
        self.assertEqual(got_ts, ts)
        self.assertEqual(got_salt, salt)
        self.assertEqual(obj["p"], "/v3/workspaces/sibyl/sessions/s1/messages")

        # Request frame must not open as a response.
        with self.assertRaises(e2ee.E2EEError):
            e2ee.open_response(secret, frame, ts, salt)

        # Response bound to (ts, salt).
        res_frame = e2ee.seal_response(secret, 200, {"Content-Type": "application/json"}, b'{"ok":true}', ts, salt)
        status, _hdrs, body = e2ee.open_response(secret, res_frame, ts, salt)
        self.assertEqual(status, 200)
        self.assertEqual(body, b'{"ok":true}')
        with self.assertRaises(e2ee.E2EEError):
            e2ee.open_response(secret, res_frame, ts + 1, salt)

    def test_config_e2ee_url_validation(self) -> None:
        old_env = os.environ.pop("GEMINUS_E2EE_URL", None)
        try:
            self.assertEqual(config._e2ee_url("https://sibyl.haunted.fail/geminise/e2ee", None),
                             "https://sibyl.haunted.fail/geminise/e2ee")
            self.assertEqual(config._e2ee_url("https://old.example/a", "http://127.0.0.1:8653/geminise/e2ee"),
                             "http://127.0.0.1:8653/geminise/e2ee")
            with self.assertRaises(config.ConfigError):
                config._e2ee_url("http://insecure.example/e2ee", None)
        finally:
            if old_env is not None:
                os.environ["GEMINUS_E2EE_URL"] = old_env


class TestGoProxyInterop(unittest.TestCase):
    """Cross-language E2E test: Python Mailbox -> Go geminus-e2ee-proxy -> raw mock Honcho."""

    def test_python_mailbox_talks_to_go_e2ee_proxy(self) -> None:
        go_bin = _find_go()
        if not go_bin:
            self.skipTest("go toolchain not available")

        secret = b"\x5a" * 32
        received: list[dict] = []

        class MockHoncho(BaseHTTPRequestHandler):
            def log_message(self, format: str, *args) -> None:
                del format, args
                return

            def do_POST(self) -> None:
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length)
                received.append({
                    "method": "POST",
                    "path": self.path,
                    "auth": self.headers.get("Authorization"),
                    "body": body.decode("utf-8"),
                })
                data = json.loads(body.decode("utf-8"))
                msg = data["messages"][0]
                reply = json.dumps([{"id": "m-1", "peer_id": msg["peer_id"], "content": msg["content"]}]).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(reply)))
                self.end_headers()
                self.wfile.write(reply)

        honcho_port = _free_port()
        honcho_srv = HTTPServer(("127.0.0.1", honcho_port), MockHoncho)
        t = threading.Thread(target=honcho_srv.serve_forever, daemon=True)
        t.start()

        with tempfile.TemporaryDirectory() as tmp:
            proxy_bin = Path(tmp) / "geminus-e2ee-proxy"
            subprocess.run(
                [go_bin, "build", "-o", str(proxy_bin), "./cmd/geminus-e2ee-proxy"],
                cwd=str(REPO), check=True, capture_output=True, text=True,
            )
            secret_file = Path(tmp) / "secret.hex"
            secret_file.write_text(secret.hex() + "\n", encoding="utf-8")
            os.chmod(secret_file, 0o600)

            proxy_port = _free_port()
            proc = subprocess.Popen(
                [
                    str(proxy_bin),
                    "-listen", f"127.0.0.1:{proxy_port}",
                    "-path", "/geminise/e2ee",
                    "-upstream", f"http://127.0.0.1:{honcho_port}",
                    "-secret-file", str(secret_file),
                ],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            )
            try:
                deadline = time.monotonic() + 10.0
                while time.monotonic() < deadline:
                    try:
                        with urllib.request.urlopen(f"http://127.0.0.1:{proxy_port}/healthz", timeout=1.0) as r:
                            if r.status == 200:
                                break
                    except Exception:
                        time.sleep(0.05)

                mb = Mailbox(
                    f"http://127.0.0.1:{honcho_port}",
                    "honcho-bearer-xyz",
                    "sibyl",
                    "geminise-main",
                    e2ee_url=f"http://127.0.0.1:{proxy_port}/geminise/e2ee",
                    e2ee_secret=secret,
                )
                stored = mb.post("sibyl", "hello across go-python e2ee 日本語", {"k": "v"})
                self.assertEqual(stored["id"], "m-1")
                self.assertEqual(stored["content"], "hello across go-python e2ee 日本語")
                self.assertEqual(len(received), 1)
                self.assertEqual(received[0]["path"], "/v3/workspaces/sibyl/sessions/geminise-main/messages")
                self.assertEqual(received[0]["auth"], "Bearer honcho-bearer-xyz")
                self.assertIn("hello across go-python e2ee", received[0]["body"])

                # Wrong secret must be refused by the Go proxy with 401.
                bad_mb = Mailbox(
                    f"http://127.0.0.1:{honcho_port}",
                    "honcho-bearer-xyz",
                    "sibyl",
                    "geminise-main",
                    e2ee_url=f"http://127.0.0.1:{proxy_port}/geminise/e2ee",
                    e2ee_secret=b"\x99" * 32,
                )
                with self.assertRaises(MailboxError) as ctx:
                    bad_mb.post("sibyl", "should fail", {})
                self.assertIn("401", str(ctx.exception))
            finally:
                proc.terminate()
                proc.wait(timeout=5)
                if proc.stdout:
                    proc.stdout.close()
                if proc.stderr:
                    proc.stderr.close()
                honcho_srv.shutdown()
                honcho_srv.server_close()


if __name__ == "__main__":
    unittest.main()
