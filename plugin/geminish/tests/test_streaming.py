"""Tests for stream-json decoders, HMAC event signing, and live Go GE2E proxy interop."""
from __future__ import annotations

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

ROOT = Path(__file__).resolve().parents[1]
REPO = Path(__file__).resolve().parents[3]
if str(ROOT.parent) not in sys.path:
    sys.path.insert(0, str(ROOT.parent))

from geminish import e2ee
from geminish.bridge import HARNESSES, run_prompt
from geminish.stream_decode import (
    clear_probe_cache,
    decode_claude_stream,
    decode_codex_stream,
    decode_hermes_stream,
    probe_stream_json,
)
from geminish.stream_pub import StreamPublisher, sign_stream_event


class StreamDecodeTests(unittest.TestCase):
    def test_hermes_stream_deltas_tools_and_no_duplicate_result(self) -> None:
        state: dict = {}
        lines = [
            json.dumps({"type": "system", "subtype": "init", "session_id": "sess-h1"}),
            json.dumps({"type": "tool_use", "name": "terminal", "input": {"command": "go test ./..."}}),
            json.dumps({"type": "tool_result", "name": "terminal", "duration_ms": 42, "is_error": False}),
            json.dumps({"type": "text", "text": "Hello "}),
            json.dumps({"type": "text", "text": "world!"}),
            json.dumps({"type": "result", "session_id": "sess-h1", "text": "Hello world!", "exit_code": 0}),
        ]
        pieces = []
        for ln in lines:
            pieces.extend(decode_hermes_stream(ln, state))
        self.assertEqual(state.get("native"), "sess-h1")
        self.assertEqual([p.kind for p in pieces], ["tool_use", "tool_result", "delta", "delta"])
        self.assertEqual(pieces[0].tool, "terminal")
        self.assertEqual(pieces[0].detail, "go test ./...")
        self.assertEqual("".join(p.text for p in pieces if p.kind == "delta"), "Hello world!")

    def test_hermes_replay_gap_emits_only_missing_suffix(self) -> None:
        state: dict = {}
        p1 = decode_hermes_stream(json.dumps({"type": "text", "text": "part1 "}), state)
        p2 = decode_hermes_stream(
            json.dumps({"type": "result", "text": "part1 part2", "replay_gap": True}),
            state,
        )
        self.assertEqual([p.text for p in p1 + p2], ["part1 ", "part2"])

    def test_claude_stream_partial_deltas_do_not_duplicate_assistant_block(self) -> None:
        state: dict = {}
        lines = [
            json.dumps({"type": "system", "session_id": "claude-s1"}),
            json.dumps({
                "type": "stream_event",
                "event": {"type": "content_block_delta", "delta": {"type": "text_delta", "text": "Hi "}},
            }),
            json.dumps({
                "type": "stream_event",
                "event": {"type": "content_block_delta", "delta": {"type": "text_delta", "text": "there"}},
            }),
            json.dumps({
                "type": "assistant",
                "message": {"content": [{"type": "text", "text": "Hi there"}]},
            }),
            json.dumps({"type": "result", "result": "Hi there", "is_error": False, "session_id": "claude-s1"}),
        ]
        pieces = []
        for ln in lines:
            pieces.extend(decode_claude_stream(ln, state))
        self.assertEqual(state.get("native"), "claude-s1")
        self.assertEqual("".join(p.text for p in pieces if p.kind == "delta"), "Hi there")

    def test_codex_stream_deltas_and_tools(self) -> None:
        state: dict = {}
        lines = [
            json.dumps({"type": "thread.started", "thread_id": "thr-1"}),
            json.dumps({"type": "item.started", "item": {"id": "c1", "type": "command_execution", "command": "ls -la"}}),
            json.dumps({"type": "item.completed", "item": {"id": "c1", "type": "command_execution", "exit_code": 0}}),
            json.dumps({"type": "item.agentMessage.delta", "item_id": "m1", "delta": "Done "}),
            json.dumps({"type": "item.agentMessage.delta", "item_id": "m1", "delta": "now"}),
            json.dumps({"type": "item.completed", "item": {"id": "m1", "type": "agent_message", "text": "Done now"}}),
        ]
        pieces = []
        for ln in lines:
            pieces.extend(decode_codex_stream(ln, state))
        self.assertEqual(state.get("native"), "thr-1")
        self.assertEqual([p.kind for p in pieces], ["tool_use", "tool_result", "delta", "delta", "delta"])
        self.assertEqual("".join(p.text for p in pieces if p.kind == "delta"), "Done now\n")

    def test_probe_and_run_prompt_hermes_stream_json(self) -> None:
        if sys.platform == "win32":
            self.skipTest("shell script fake harness")
        clear_probe_cache()
        with tempfile.TemporaryDirectory() as tmp:
            fake = Path(tmp) / "fake-hermes"
            fake.write_text(
                "#!/bin/sh\n"
                "if [ \"$2\" = \"--help\" ]; then\n"
                "  echo 'usage: hermes chat --format {text,stream-json}'\n"
                "  exit 0\n"
                "fi\n"
                "cat >/dev/null\n"
                "echo '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s-live\"}'\n"
                "echo '{\"type\":\"tool_use\",\"name\":\"read_file\",\"input\":{\"path\":\"a.go\"}}'\n"
                "echo '{\"type\":\"tool_result\",\"name\":\"read_file\",\"duration_ms\":5,\"is_error\":false}'\n"
                "echo '{\"type\":\"text\",\"text\":\"alpha \"}'\n"
                "echo '{\"type\":\"text\",\"text\":\"beta\"}'\n"
                "echo '{\"type\":\"result\",\"session_id\":\"s-live\",\"text\":\"alpha beta\",\"exit_code\":0}'\n",
                encoding="utf-8",
            )
            fake.chmod(0o755)
            self.assertTrue(probe_stream_json("hermes", str(fake)))
            hermes = next(h for h in HARNESSES if h.id == "hermes")
            emitted: list[str] = []
            events = []
            res = run_prompt(
                hermes,
                str(fake),
                "geminus-sibyl-test",
                None,
                "hello",
                cwd=tmp,
                emit=emitted.append,
                on_event=events.append,
                stream_json=True,
                environ=os.environ.copy(),
            )
            self.assertTrue(res.ok, res.detail)
            self.assertEqual("".join(emitted), "alpha beta")
            self.assertEqual([(e.kind, e.tool) for e in events], [("tool_use", "read_file"), ("tool_result", "read_file")])


class LiveGoProxyStreamInteropTests(unittest.TestCase):
    def test_python_stream_publisher_interops_with_go_e2ee_proxy(self) -> None:
        go_bin = shutil.which("go")
        if not go_bin:
            self.skipTest("go toolchain not installed")

        secret_hex = "a5" * 32
        secret = bytes.fromhex(secret_hex)

        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        sock.close()

        with tempfile.TemporaryDirectory() as tmp:
            proxy_bin = Path(tmp) / "geminus-e2ee-proxy"
            secret_file = Path(tmp) / "secret.hex"
            secret_file.write_text(secret_hex, encoding="utf-8")
            subprocess.run(
                [go_bin, "build", "-o", str(proxy_bin), "./cmd/geminus-e2ee-proxy"],
                cwd=str(REPO),
                check=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            proc = subprocess.Popen(
                [
                    str(proxy_bin),
                    "-listen", f"127.0.0.1:{port}",
                    "-path", "/geminise/e2ee",
                    "-upstream", "http://127.0.0.1:1",
                    "-secret-file", str(secret_file),
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            try:
                # Wait for healthz
                deadline = time.monotonic() + 5.0
                while time.monotonic() < deadline:
                    try:
                        with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=0.5) as resp:
                            if resp.status == 200:
                                break
                    except OSError:
                        time.sleep(0.05)

                e2ee_url = f"http://127.0.0.1:{port}/geminise/e2ee"
                pub = StreamPublisher(
                    e2ee_url,
                    secret,
                    "sess-py-interop",
                    "turn-1",
                    coalesce_s=0.02,
                )
                pub.emit_tool_use("terminal", "echo hi")
                pub.emit_tool_result("terminal", "8ms", ok=True)
                pub.emit_delta("Hello ")
                pub.emit_delta("from Python!")
                pub.close(ok=True)
                self.assertTrue(pub.published_any)
                self.assertFalse(pub.disabled)

                # Verify signature helper matches valid event shape accepted by Go proxy.
                sig = sign_stream_event(
                    secret,
                    session="sess-py-interop",
                    turn="turn-1",
                    seq=1,
                    kind="delta",
                    text="ping",
                    ts=int(time.time()),
                )
                self.assertEqual(len(sig), 64)
            finally:
                proc.terminate()
                try:
                    proc.wait(timeout=3.0)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()


if __name__ == "__main__":
    unittest.main()
