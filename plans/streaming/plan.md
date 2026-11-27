# Real-Time Ephemeral E2EE Token & Tool Streaming for geminus / geminish

## Goal

Give `geminus` native-agent-grade real-time token-by-token streaming and live tool execution visibility across remote personas (Hermes, Claude Code, Codex) without polluting Honcho with fragmented chunk messages or leaking plaintext over intermediate proxies.

## Architecture

```
Agent harness (hermes --format stream-json / claude --output-format stream-json / codex exec --json)
  |  1. Emits JSONL text deltas + tool_use / tool_result events as they happen
  v
geminish bridge (plugin/geminish/stream_decode.py + stream_pub.py)
  |  2. Signs each stream event with group secret (HMAC-SHA256, domain-separated)
  |  3. Micro-batches delta events (~50ms coalesce) and flushes tool/done immediately
  |  4. Encrypts POST /_stream/publish inside GE2E v1 request frame -> POST <e2ee_url>
  |  5. On turn end, posts authoritative final signed output to Honcho (raw storage preserved)
  v
geminus-e2ee-proxy (BTCVPS loopback, internal/e2ee/stream_hub.go + proxy.go)
  |  6. Validates GE2E v1 frame, timestamp skew, replay cache, and event HMAC signature
  |  7. Stores event in bounded in-memory session ring buffer (ephemeral, TTL 10m, never hits Honcho)
  |  8. Broadcasts to active GET /_stream/subscribe SSE subscribers as encrypted GE2E DirStreamChunk frames
  v
geminus terminal client (internal/e2ee/stream_client.go + internal/chat/stream.go + live_stream.go)
  |  9. Decrypts each SSE chunk verifying (ts, salt, monotonic chunk seq) + bridge HMAC signature
  | 10. Renders completed lines via Markdown and unfinished partial lines immediately in-place via live preview tail
  | 11. Displays live tool activity in normal and Clawd modes
  | 12. Reconciles cleanly with Honcho's final authoritative RoleOutput without duplicating text
```

## Wire Protocol Extensions (`GE2E` v1 Ephemeral Stream)

- Reuses the exact same `POST <e2ee_url>` endpoint and RFC 8439 ChaCha20-Poly1305 + HKDF-SHA256 primitives (zero external dependencies in Go and Python).
- `POST /_stream/publish` (inside GE2E v1 `DirRequest = 1`):
  - JSON body: `{"events": [StreamEvent, ...]}`
  - Each `StreamEvent` carries `session`, `turn`, `seq`, `kind` (`delta` | `tool_use` | `tool_result` | `done`), `text`, `tool`, `detail`, `ok`, `ts`, `sig`.
  - Bridge HMAC signature: `HMAC-SHA256(secret, SHA256(text) || SHA256(label) || decimal(ts))` where `label = geminus-stream:<session>:<turn>:<seq>:<kind>:<H(tool)>:<H(detail)>:<int(ok)>`.
  - Response: normal encrypted GE2E v1 `DirResponse = 2` JSON `{"ok":true,"last_id":<uint64>}`.
- `GET /_stream/subscribe?session=<sid>&after=<uint64>` (inside GE2E v1 `DirRequest = 1`):
  - Response: `Content-Type: text/event-stream`, `Cache-Control: no-store`, `X-Accel-Buffering: no`.
  - Each event is emitted as `data: <base64(chunkFrame)>\n\n` with `DirStreamChunk = 3`:
    - `header[0:4]   = "GE2E"`
    - `header[4]     = 0x01`
    - `header[5]     = 0x03` (`DirStreamChunk`)
    - `header[6:14]  = int64 big-endian ts` (from subscribe request)
    - `header[14:38] = 24-byte salt` (from subscribe request)
    - `header[38:46] = uint64 big-endian chunk_seq` (monotonic starting at 1)
    - `body[46:]     = ChaCha20-Poly1305(stream_key, stream_nonce XOR chunk_seq, json(StreamEvent), aad=header[0:46])`
  - Periodic `: ping\n\n` keepalive every 15s; clean reconnect with `after=lastID`.
- Backward compatibility:
  - When `e2ee_url` is unset, or if an older proxy returns 400/404 for `/_stream/*`, both bridge and client fall back seamlessly to Honcho polling with zero regressions.
  - Older harnesses without `stream-json` (or test fake binaries) auto-fallback via cached `--help` capability probing.
