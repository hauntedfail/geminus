"""JSONL stream decoders for agent harnesses (Hermes, Claude Code, Codex).

Stdlib-only module used by the geminus bridge to extract real-time text deltas
and tool progress events from harness streaming modes without duplicating text
when terminal summary/result events arrive.
"""
from __future__ import annotations

from dataclasses import dataclass
import json
import subprocess
import sys
import threading

STDERR_TAIL = 600
_DETAIL_CAP = 140

_PROBE_LOCK = threading.Lock()
_PROBE_CACHE: dict[tuple[str, str], bool] = {}


@dataclass(frozen=True)
class StreamPiece:
    """One normalized real-time item decoded from a harness JSONL line."""

    kind: str  # "delta" | "tool_use" | "tool_result"
    text: str = ""
    tool: str = ""
    detail: str = ""
    ok: bool = True


def _clip(text: object, limit: int = _DETAIL_CAP) -> str:
    one = " ".join(str(text).split())
    if len(one) <= limit:
        return one
    return one[: max(limit - 1, 1)] + "…"


def _tool_input_preview(inp: object) -> str:
    if not isinstance(inp, dict):
        return _clip(inp) if inp else ""
    for key in ("command", "cmd", "path", "file_path", "pattern", "query", "url", "goal", "toolSummary"):
        val = inp.get(key)
        if isinstance(val, str) and val.strip():
            return _clip(val)
    try:
        return _clip(json.dumps(inp, ensure_ascii=False, separators=(",", ":")))
    except (TypeError, ValueError):
        return ""


def probe_stream_json(harness_id: str, binary: str, env: dict | None = None) -> bool:
    """Cached capability check for whether ``binary`` supports JSONL streaming flags."""
    if harness_id not in ("hermes", "claude") or not binary:
        return False
    key = (harness_id, binary)
    with _PROBE_LOCK:
        cached = _PROBE_CACHE.get(key)
    if cached is not None:
        return cached

    args = [binary, "chat", "--help"] if harness_id == "hermes" else [binary, "--help"]
    kwargs: dict = {}
    if sys.platform == "win32":
        kwargs["creationflags"] = getattr(subprocess, "CREATE_NO_WINDOW", 0)
    supported = False
    try:
        proc = subprocess.run(
            args,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=5.0,
            env=env,
            **kwargs,
        )
        out = (proc.stdout + proc.stderr).decode("utf-8", "replace")
        if harness_id == "hermes":
            supported = proc.returncode == 0 and "stream-json" in out
        elif harness_id == "claude":
            supported = proc.returncode == 0 and "stream-json" in out and "include-partial-messages" in out
    except (OSError, subprocess.SubprocessError):
        supported = False

    with _PROBE_LOCK:
        _PROBE_CACHE[key] = supported
    return supported


def clear_probe_cache() -> None:
    """Clear the cached binary capability probes (for tests)."""
    with _PROBE_LOCK:
        _PROBE_CACHE.clear()


def decode_hermes_stream(line: str, state: dict) -> list[StreamPiece]:
    """Decode one JSONL line from ``hermes chat --format stream-json``."""
    try:
        event = json.loads(line)
    except ValueError:
        return []
    if not isinstance(event, dict):
        return []

    etype = event.get("type")
    if etype == "system":
        sid = event.get("session_id")
        if isinstance(sid, str) and sid:
            state["native"] = sid
        return []

    if etype == "text":
        text = event.get("text")
        if isinstance(text, str) and text:
            state["saw_delta"] = True
            state["streamed"] = state.get("streamed", "") + text
            return [StreamPiece("delta", text=text)]
        return []

    if etype == "tool_use":
        name = str(event.get("name") or "tool")
        detail = _tool_input_preview(event.get("input"))
        return [StreamPiece("tool_use", tool=name, detail=detail)]

    if etype == "tool_result":
        name = str(event.get("name") or "tool")
        dur = event.get("duration_ms")
        detail = f"{int(dur)}ms" if isinstance(dur, (int, float)) and dur >= 0 else ""
        is_err = bool(event.get("is_error", False))
        return [StreamPiece("tool_result", tool=name, detail=detail, ok=not is_err)]

    if etype == "result":
        sid = event.get("session_id")
        if isinstance(sid, str) and sid:
            state["native"] = sid
        if event.get("error"):
            state["error"] = str(event["error"])[:STDERR_TAIL]
        final_text = event.get("text")
        if isinstance(final_text, str) and final_text:
            streamed = state.get("streamed", "")
            if not state.get("saw_delta"):
                state["streamed"] = final_text
                return [StreamPiece("delta", text=final_text)]
            if event.get("replay_gap") and final_text.startswith(streamed) and len(final_text) > len(streamed):
                suffix = final_text[len(streamed):]
                state["streamed"] = final_text
                return [StreamPiece("delta", text=suffix)]
        return []

    return []


def decode_claude_stream(line: str, state: dict) -> list[StreamPiece]:
    """Decode one JSONL line from ``claude -p --output-format stream-json --verbose --include-partial-messages``."""
    try:
        event = json.loads(line)
    except ValueError:
        return []
    if not isinstance(event, dict):
        return []

    sid = event.get("session_id")
    if isinstance(sid, str) and sid:
        state["native"] = sid

    etype = event.get("type")
    if etype == "stream_event":
        inner = event.get("event")
        if not isinstance(inner, dict):
            return []
        itype = inner.get("type")
        if itype == "content_block_delta":
            delta = inner.get("delta")
            if isinstance(delta, dict) and delta.get("type") == "text_delta":
                text = delta.get("text")
                if isinstance(text, str) and text:
                    state["turn_streamed"] = True
                    state["emitted_any"] = True
                    return [StreamPiece("delta", text=text)]
        elif itype == "content_block_start":
            cb = inner.get("content_block")
            if isinstance(cb, dict) and cb.get("type") == "tool_use":
                tid = str(cb.get("id") or "")
                name = str(cb.get("name") or "tool")
                if tid:
                    state.setdefault("announced_tools", set()).add(tid)
                return [StreamPiece("tool_use", tool=name, detail=_tool_input_preview(cb.get("input")))]
        return []

    if etype == "assistant":
        msg = event.get("message")
        blocks = msg.get("content") if isinstance(msg, dict) else None
        out: list[StreamPiece] = []
        if isinstance(blocks, list):
            saw_stream = bool(state.get("turn_streamed"))
            announced = state.setdefault("announced_tools", set())
            for block in blocks:
                if not isinstance(block, dict):
                    continue
                btype = block.get("type")
                if btype == "text" and not saw_stream:
                    text = block.get("text")
                    if isinstance(text, str) and text:
                        state["emitted_any"] = True
                        out.append(StreamPiece("delta", text=text))
                elif btype == "tool_use":
                    tid = str(block.get("id") or "")
                    if tid and tid in announced:
                        continue
                    if tid:
                        announced.add(tid)
                    out.append(StreamPiece(
                        "tool_use",
                        tool=str(block.get("name") or "tool"),
                        detail=_tool_input_preview(block.get("input")),
                    ))
        state["turn_streamed"] = False
        return out

    if etype == "user":
        msg = event.get("message")
        blocks = msg.get("content") if isinstance(msg, dict) else None
        out = []
        if isinstance(blocks, list):
            for block in blocks:
                if isinstance(block, dict) and block.get("type") == "tool_result":
                    out.append(StreamPiece(
                        "tool_result",
                        tool=str(block.get("tool_name") or "tool"),
                        ok=not bool(block.get("is_error", False)),
                    ))
        return out

    if etype == "result":
        if event.get("is_error"):
            err = event.get("result") or event.get("error") or "claude failed"
            state["error"] = str(err)[:STDERR_TAIL]
            return []
        res_text = event.get("result")
        if isinstance(res_text, str) and res_text and not state.get("emitted_any"):
            state["emitted_any"] = True
            return [StreamPiece("delta", text=res_text)]
        return []

    return []


def decode_codex_stream(line: str, state: dict) -> list[StreamPiece]:
    """Decode one JSONL line from ``codex exec --json`` into deltas and tool events."""
    try:
        event = json.loads(line)
    except ValueError:
        return []
    if not isinstance(event, dict):
        return []

    etype = event.get("type")
    if etype == "thread.started" and isinstance(event.get("thread_id"), str):
        state["native"] = event["thread_id"]
        return []

    if etype in ("item.agentMessage.delta", "response.output_text.delta"):
        delta = event.get("delta") or event.get("text")
        if isinstance(delta, str) and delta:
            iid = str(event.get("item_id") or "_default")
            by_id = state.setdefault("codex_streamed", {})
            by_id[iid] = by_id.get(iid, "") + delta
            return [StreamPiece("delta", text=delta)]
        return []

    item = event.get("item")
    if not isinstance(item, dict):
        if etype in ("turn.failed", "error"):
            state["error"] = json.dumps(event.get("error") or event.get("message") or event, ensure_ascii=False)[:STDERR_TAIL]
        return []

    itype = item.get("type")
    iid = str(item.get("id") or "_default")
    if etype == "item.started" and itype in ("command_execution", "mcp_tool_call", "file_change"):
        tool = str(item.get("tool") or itype)
        detail = _clip(item.get("command") or item.get("path") or "")
        return [StreamPiece("tool_use", tool=tool, detail=detail)]

    if etype == "item.updated" and itype == "agent_message":
        text = str(item.get("text") or "")
        by_id = state.setdefault("codex_streamed", {})
        prev = by_id.get(iid, "")
        if text.startswith(prev) and len(text) > len(prev):
            by_id[iid] = text
            return [StreamPiece("delta", text=text[len(prev):])]
        return []

    if etype == "item.completed":
        if itype == "agent_message":
            text = str(item.get("text") or "")
            by_id = state.setdefault("codex_streamed", {})
            prev = by_id.get(iid, "")
            if prev and text.startswith(prev):
                suffix = text[len(prev):]
                by_id[iid] = text
                return [StreamPiece("delta", text=suffix + "\n")]
            by_id[iid] = text
            return [StreamPiece("delta", text=text + "\n")]
        if itype in ("command_execution", "mcp_tool_call", "file_change"):
            tool = str(item.get("tool") or itype)
            ok = item.get("exit_code", 0) == 0 and item.get("status") != "failed"
            return [StreamPiece("tool_result", tool=tool, ok=ok)]

    if etype in ("turn.failed", "error"):
        state["error"] = json.dumps(event.get("error") or event.get("message") or event, ensure_ascii=False)[:STDERR_TAIL]
    return []
