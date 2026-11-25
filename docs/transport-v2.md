# geminise End-to-End (P2P) Transport

Status: Implemented on branch `feat/transport-v2` alongside Central (Honcho) transport. Provides transport abstraction + failover, Obfuscated QUIC with XChaCha20-Poly1305 whitening & silent drop, bridge-side `LocalStore`, `PeerCache`, LAN discovery, blind encrypted HTTPS/WebSocket relay (behind `cloudflared` with zero open firewall ports) and blind UDP relay, and the `cmd/geminised` daemon.

## 1. Positioning: Central (Honcho) vs. End-to-End (P2P)

`geminus` supports two parallel transport modes rather than replacing Honcho:

- **Central transport (Honcho)**: Rides directly on the Honcho ecosystem so conversations automatically feed shared cross-agent memory, representations, and cards across `chrome`, `sibyl`, and `mirai`.
- **End-to-End transport (P2P / direct + blind relay)**: A separate peer-to-peer path that does not depend on Honcho — for direct LAN/WAN sessions, Honcho-outage failover, UDP-blocked networks (via blind HTTPS/WebSocket relay behind `cloudflared`), and external users without a Honcho server.

Today, `geminus` and `geminish` use Honcho over HTTPS as their sole transport channel. That gave fast initial delivery with zero open ports, but creates four structural limits:

1. **Single point of failure**: If Honcho or `sibyl` is unreachable, `mirai` cannot reach `chrome` even when both machines are online.
2. **Adoption barrier**: External users cannot try `geminus` without deploying and operating a Honcho server.
3. **Network visibility**: Standard HTTPS exposes SNI/TLS fingerprints, centralizes connection metadata at one server, and suffers TCP head-of-line blocking under loss.
4. **Fragmented family**: `geminus` (human-to-agent), `geminish` (agent-to-agent mail), and OTA release distribution use separate polling/HTTP paths instead of one unified transport.

Transport v2 defines a dedicated, edge-to-edge P2P transport layer for the entire `geminise` family (`geminus`, `geminish`, `geminised`) optimized across four axes:

- **NAT traversal**: Direct edge-to-edge (`mirai <-> chrome`) across home/office NATs, with automatic blind-relay fallback when punching fails.
- **Surveillance and DPI resistance**: No plaintext SNI, no identifiable QUIC/HTTP headers on the wire, and optional traffic-shape padding.
- **Attack and probing resistance**: Silent drop on unauthenticated packets (invisible to scanners/probers) and stateless DoS cookies before allocating state.
- **Speed and resilience**: 0-RTT/1-RTT setup, multiplexed streams without head-of-line blocking, and seamless connection migration across network changes.

Honcho is re-scoped to what it does best: optional long-term semantic memory and cross-device sync, rather than the required packet bus.

## 2. Threat Model and Non-Goals

### In scope
- **Passive DPI / ISP / corporate middleboxes** fingerprinting TLS/QUIC handshakes, SNI, or known VPN/proxy signatures.
- **Active probing and port scanning** attempting to detect whether a host runs a `geminise` endpoint.
- **Volumetric / state-exhaustion DoS** sending spoofed or garbage UDP packets to exhaust memory or CPU.
- **Compromised relay or Honcho host**: Relays must never read payloads, forge prompts, or modify routing targets.
- **Central infrastructure outage**: `mirai <-> chrome` must continue working when `sibyl` / Honcho is down.

### Out of scope (explicit non-goals)
- **Global passive adversary anonymity**: Protecting IP-level linkability against an adversary observing both ends of the internet requires a mixnet or Tor, which is incompatible with interactive low-latency terminal streaming.
- **Kernel-level VPN (TUN/TAP)**: Everything runs in unprivileged user space so macOS, Linux, and Windows (`mirai`) need no root/admin privileges.

## 3. Architecture Overview

```
  mirai (edge)                                                chrome (edge)
 ┌──────────────────────┐                                    ┌──────────────────────┐
 │ geminus / geminish   │                                    │ bridge / geminish    │
 │  └─ internal/wire    │                                    │  └─ internal/wire    │
 │  └─ transport v2     │                                    │  └─ transport v2     │
 │      (Obfs-QUIC +    │ <====== 1. LAN / WAN Direct =====> │      (Obfs-QUIC +    │
 │       Noise/Ed25519) │                                    │       Noise/Ed25519) │
 └──────────┬───────────┘                                    └──────────┬───────────┘
            │                                                           │
            └──── 2. Rendezvous / Blind Encrypted Relay (fallback) ─────┘
                                         │
                                ┌────────┴────────┐
                                │   geminised     │  (sibyl or any VPS;
                                │  (stateless /   │   cannot read payload)
                                │   blind relay)  │
                                └─────────────────┘

  Optional / Async: Honcho (memory & history sync when reachable)
```

## 4. Layered Protocol Design

### Layer 0: Identity and Addressing
- Every node has a static **Ed25519 keypair**; its 32-byte public key is its canonical `NodeID`.
- Human prompt authority remains separated via the owner's Ed25519 key (`geminus_human.key`) already defined in `docs/protocol.md`.
- Peers are authorized strictly by an explicit allowlist of known `NodeID`s (distributed via `geminise.json` or signed group roster). There is no X.509 CA chain and no DNS/SNI trust anchor.

### Layer 1: Anti-Probing Outer Envelope (`PacketConn` Whitening)
Standard QUIC Initial packets expose fixed header bits, QUIC version, and a DCID-derived obfuscation key that DPI trivially decodes. To eliminate this fingerprint:

- Wrap the UDP socket in a custom `net.PacketConn` obfuscation layer (conceptually similar to Hysteria2's Salamander + WireGuard's Silent Drop).
- Every UDP datagram is wrapped with a per-packet random salt/nonce and authenticated/whitened using a group transport key (or pairwise key derived from static keys):
  - Header and payload appear as uniform high-entropy bytes with no fixed magic bytes or version fields.
  - Variable padding is appended to break fixed-size handshake signatures (such as QUIC's 1200-byte Initial floor or WireGuard's 148-byte handshake).
- **Silent Drop**: Any datagram whose outer authentication tag fails is discarded immediately with zero response. Active scanners and probes observe a dead UDP port.
- **Stateless DoS Cookie**: Under elevated unauthenticated or half-open load, the responder issues a time-bound stateless cookie (modeled on WireGuard Cookie Reply / QUIC Retry) before allocating QUIC connection state.

### Layer 2: Transport and Stream Multiplexing (Obfuscated QUIC)
Inside the Layer 1 envelope runs QUIC (via `quic-go` in Go, producing a single static binary for macOS, Linux, and Windows):

- **Mutual authentication**: Raw Ed25519 peer-key verification inside the handshake (or Noise_IK handshake binding the QUIC session keys). No cleartext SNI is sent.
- **Zero head-of-line blocking**: One persistent encrypted connection between two peers carries independent multiplexed streams for the entire `geminise` family:

| Stream class | Purpose | Replaces |
|---|---|---|
| Control (`0`) | Node cards, harness list, keepalive, path-probing (`disco`) | Honcho peer metadata polling |
| Interactive (`1`) | `geminus` sessions: signed prompts, live output streaming, `/sessions`, `/resume`, approvals | Honcho `kind: geminus` polling + Mozilla Push doorbell |
| Mail / A2A (`2`) | `geminish` agent-to-agent messages, delegation, heartbeats | Honcho `geminise-main` mailbox |
| Bulk / OTA (`3`) | Signed binary updates, file/artifact transfer | Separate HTTP release server / ad-hoc transfers |

- **Connection migration**: QUIC connection IDs allow an edge laptop (`mirai` or `chrome`) to roam between Wi-Fi, tethering, and VPN without dropping active turns.
- **Congestion control**: Standard CUBIC/BBR by default, with an option for loss-tolerant pacing (Hysteria-style) on hostile or high-loss links.

### Layer 3: NAT Traversal and Fallback Ladder
Connection establishment follows a strict direct-first ladder:

1. **LAN Direct (Zero-Config, IP Messenger style)**:
   - Encrypted UDP multicast/broadcast beacons on the local subnet allow peers on the same LAN to discover each other and connect immediately with zero external servers.
2. **WAN Direct (UDP Hole-Punching)**:
   - Peers discover their reflexive external `IP:port` via STUN/QAD probes (from `geminised` or public/self-hosted STUN).
   - A lightweight rendezvous exchange (via `geminised`, or cached last-known endpoints) coordinates simultaneous UDP hole-punching (modeled on Tailscale `disco` and `iroh` MagicSock).
   - Once direct packets succeed, all streams migrate directly `mirai <-> chrome` with no central server in the data path.
3. **Blind Encrypted Relay (`geminised`)**:
   - When both peers sit behind symmetric NAT or CGNAT and hole-punching fails, traffic routes through `geminised` (on `sibyl` or any user-supplied VPS).
   - Because end-to-end encryption and Ed25519 signing terminate strictly at the edge peers, `geminised` acts as a blind byte forwarder: it cannot read prompts, outputs, or even forge control frames.
   - Background direct-path probes continue periodically to upgrade back to direct whenever the network permits.
4. **Restrictive-Network Fallback (TCP/443 Masquerade)**:
   - For enterprise networks that drop all unknown UDP, an optional TCP/443 fallback using TLS 1.3 masquerading (uTLS / REALITY-style or HTTP/3 MASQUE) connects to the blind relay while looking like ordinary HTTPS traffic.
5. **Honcho Backend (Legacy / Coexistence)**:
   - The existing Honcho HTTP transport remains implemented behind the same Go interface during migration and for environments where only outbound HTTPS to Honcho is allowed.

### Layer 4: Agent-Specific Traffic Analysis Mitigations
Interactive LLM streaming produces a highly recognizable traffic pattern: a prompt burst, a silent thinking pause, then a cadence of small token chunks.

- **Length bucketing**: Pad interactive frames to fixed size buckets (e.g., 256 / 512 / 1024 / 2048 bytes) so prompt length and token sizes are blurred.
- **Optional pacing / cover mode**: For high-sensitivity networks, allow a configurable stealth profile that adds small randomized jitter and periodic dummy frames on idle streams (off by default to preserve lowest latency).

## 5. State Ownership Without Honcho

To work without Honcho (and survive Honcho outages) while keeping `geminus` thin and stateless:

- **Bridge-owned local log**: Each bridge host persists its own conversations in a lightweight local append-only store (SQLite or JSONL under `<HERMES_HOME>/geminus/sessions/`). Every prompt and output record retains its original `internal/wire` signature.
- **Client remains thin**: `geminus` stores no conversation transcripts. It keeps only a small local peer cache (`<HERMES_HOME>/geminus_peers.json`) containing known `NodeID`s, Ed25519 public keys, last-known direct endpoints, and relay addresses so failover works even if Honcho is down at startup.
- **Optional Honcho sync**: When Honcho is configured and healthy, the bridge can asynchronously mirror finalized turns or summaries to Honcho for long-term memory without ever blocking live interaction.

## 6. Codebase Integration (`geminus` Go Architecture)

Today, `internal/chat` calls `internal/honcho` directly, while `internal/wire` already handles record formatting and Ed25519 signing independently of Honcho.

### Proposed package layout
- `internal/wire`: Unchanged responsibility — canonical signed records (`Prompt`, `Output`, `NodeCard`, `Session`, `Defaults`). Transport-agnostic.
- `internal/transport`: New transport interface:
  - `Discover(ctx) ([]PeerInfo, error)`
  - `OpenSession(ctx, peer, spec) (SessionHandle, error)`
  - `SendPrompt(ctx, session, signedPrompt) error`
  - `Subscribe(ctx, session) (<-chan Event, error)`
  - `ListSessions(ctx, peer) ([]SessionSummary, error)`
- `internal/transport/honcho`: Wraps existing `internal/honcho` behind `internal/transport`.
- `internal/transport/p2p`: New Obfuscated QUIC + `PacketConn` whitener + LAN discovery + UDP hole-punching + blind relay client.
- `cmd/geminised`: Standalone static Go binary acting as the lightweight rendezvous + blind relay (and optionally embedded as a local node daemon so Python `geminish` talks to it over loopback).

### Dependency policy change
`geminus` v0.x used the Go standard library only. Transport v2 intentionally relaxes this for `internal/transport/p2p` to adopt audited cryptographic and QUIC primitives (`quic-go` and `golang.org/x/crypto`), while keeping zero CGo and single-binary cross-compilation for darwin/linux/windows (amd64/arm64).

## 7. Phased Rollout Plan

### Phase 0: Transport Abstraction (Zero Behavior Change)
- Extract `internal/transport` interface from `internal/chat`.
- Move current Honcho calls behind `internal/transport/honcho`.
- Acceptance: All existing unit and E2E tests pass unchanged.

### Phase 1: Edge-to-Edge Direct PoC (`mirai <-> chrome`, No Honcho)
- Implement Layer 1 `PacketConn` whitener (Silent Drop + group-keyed whitening) and Layer 2 `quic-go` multiplexed transport.
- Implement LAN UDP discovery and explicit `--direct <addr>` connection.
- Add bridge-side local session persistence so `/sessions` and `/resume` work offline from Honcho.
- Acceptance: `mirai -> chrome` interactive session runs end-to-end with Honcho completely stopped; unauthenticated UDP probes receive zero bytes.

### Phase 2: NAT Hole-Punching, Blind Relay, and Automatic Failover
- Build `cmd/geminised` stateless rendezvous + blind encrypted relay.
- Implement WAN UDP hole-punching with automatic fallback to `geminised` and background direct upgrade.
- Add automatic Honcho-down failover in `geminus` using cached peer endpoints.
- Acceptance: `mirai` and `chrome` behind separate NATs punch directly when possible, fall back transparently when blocked, and survive `sibyl`/Honcho outage.

### Phase 3: Full `geminise` Family Unification and Stealth Hardening
- Migrate `geminish` A2A mail and signed OTA updates onto streams 2 and 3.
- Add optional TCP/443 TLS-masquerade fallback for UDP-blocked enterprise networks.
- Add optional traffic-shape padding/jitter profile and formal protocol verification specs.

## 8. Open Decisions

1. **Primary threat profile**: Prioritize ISP/corporate DPI + active-probing resistance first (Phase 1–2), or include TCP/443 REALITY-style masquerade in Phase 1?
2. **Implementation stack**: Pure Go (`quic-go` + `x/crypto`, single binary, native integration with `geminus`) vs. Rust sidecar (`iroh` fork). This draft recommends pure Go.
3. **Bridge runtime**: Embed the v2 listener directly in a Go node daemon (`geminised` in edge mode) that invokes local harnesses or proxies to Python `geminish` over loopback, rather than re-implementing QUIC in Python.
4. **Honcho coexistence**: Keep Honcho as optional async memory sync, or allow fully Honcho-free deployments by default for external OSS users.
