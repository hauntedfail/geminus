# App-Layer E2EE Transport for geminus / geminish

## Goal

Conceal the communication channel between remote geminus/geminish clients and the BTCVPS Honcho instance across intermediate networks (including Cloudflare Edge TLS termination), while keeping Honcho storage completely raw and preserving existing Honcho API semantics, signing, and multi-device session continuity.

## Architecture

```
geminus (Go) / geminish (Python)
  |  1. Serialize inner HTTP request (method, path, query, headers incl. Authorization, body) + bucketed padding
  |  2. Encrypt with RFC 8439 ChaCha20-Poly1305 using HKDF-SHA256 one-time key derived from group secret + 24B random salt
  |  3. POST fixed endpoint (e.g. https://sibyl.haunted.fail/geminise/e2ee) as opaque binary GE2E v1 frame
  v
Cloudflare Edge / Tunnel (sees only fixed POST endpoint and padded opaque ciphertext)
  v
geminus-e2ee-proxy (BTCVPS loopback, e.g. 127.0.0.1:8653)
  |  4. Validate GE2E header, timestamp window (+/-300s), replay nonce cache, and AEAD tag
  |  5. Validate inner HTTP method & path allowlist (/v3/... or /health; no path traversal / SSRF)
  |  6. Forward raw HTTP request to loopback Honcho (http://127.0.0.1:8000) -> stored raw in Honcho
  |  7. Encrypt raw HTTP response with response-direction HKDF key & AAD + bucketed padding
  v
geminus / geminish decrypts response and processes it identically to direct Honcho HTTP.
```

## Cryptographic Specification (`GE2E` v1)

- Zero external dependencies in both Go (`go.mod` unchanged) and Python (stdlib only).
- AEAD: RFC 8439 ChaCha20-Poly1305 (256-bit key, 96-bit nonce, 128-bit Poly1305 tag).
- Key derivation: HKDF-SHA256 (RFC 5869) with `ikm = group secret` (>=32B from `geminise.json`) and 24-byte CSPRNG `salt` per request:
  - `prk = HKDF-Extract(salt, secret)`
  - Request key: `HKDF-Expand(prk, "geminus-e2ee-v1:req", 32)`
  - Request nonce: `HKDF-Expand(prk, "geminus-e2ee-v1:req-nonce", 12)`
  - Response key: `HKDF-Expand(prk, "geminus-e2ee-v1:res", 32)`
  - Response nonce: `HKDF-Expand(prk, "geminus-e2ee-v1:res-nonce", 12)`
- Frame header (38 bytes, authenticated as AAD together with domain tag):
  - `magic[0:4] = "GE2E"`
  - `version[4] = 0x01`
  - `direction[5] = 0x01` (request) or `0x02` (response)
  - `ts[6:14] = int64 big-endian Unix seconds`
  - `salt[14:38] = 24-byte random salt`
  - `body[38:] = ciphertext || 16-byte Poly1305 tag`
- AAD: `header[0:38]` (binds magic, version, direction, timestamp, and salt; prevents reflection, direction confusion, timestamp tampering, and cross-request response swapping).
- Replay protection (proxy): rejects `|now - ts| > 300s` and caches seen `salt` values for 600s with bounded memory.
- Traffic-size padding: inner JSON carries `"pad"` padded to power-of-two buckets (min 512B, 4KB steps above 32KB) to hide small payload size distinctions.

## Compatibility & Configuration

- Optional `e2ee_url` in `geminise.json` (top-level or under `geminus.e2ee_url`) or `GEMINUS_E2EE_URL` env var.
- When unset (or when Honcho `baseUrl` is loopback and `e2ee_url` is unset), clients talk directly to Honcho with zero behavior change.
- Production service deployment on BTCVPS (systemd unit enablement and `cloudflared` ingress reload) is A3 and requires explicit owner approval after verification.
