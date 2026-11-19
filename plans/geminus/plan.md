# geminus — Implementation plan for a lightweight agent client

- Status: **Implementation complete and verified (2026-11-19)**
- Repository: `/Users/vvx/projekt/go/geminus`
- Language: Go (single static binary, CGO disabled)
- Created: 2026-11-19

---

## 1. Purpose

`geminus` is **a thin CLI client that passes input and output through to and from Claude Code and Hermes Agent unchanged**.

Four properties guide the design:

1. **Transparency**: Forward the bytes as they are, without interpreting or reconstructing agent output.
2. **Continuity**: After closing a session, use the same geminus session ID to resume the conversation and the work.
3. **Lightweight operation**: No daemon, database, HTTP, or resident process. All state lives in local files.
4. **Portability**: Use the same commands on macOS, Linux, and Windows. Handle PTY differences internally.

`geminus` sits on the **client side** of the geminise ecosystem. It is distinct from `geminish`, the Hermes-only client.
`geminus` implements no protocol and does not communicate over the network. It only wraps agent CLIs on a single host.

### Non-goals for v0.1

- A daemon, backend, or relay (these belong to `geminised`)
- Live attach from another terminal (tmux-style multiplexing); **resume** provides continuity
- Inter-agent communication, group fan-out, or push notifications
- Summarizing and reinjecting conversation history (a possible `--replay` feature for v0.2)
- Credential management (use the agent's existing authentication as is)
- A GUI or TUI dashboard

---

## 2. Architecture

```
            ┌──────────────────────────────────────────┐
  terminal  │  geminus (single binary)                 │
  ──stdin──▶│                                          │
            │  internal/cli   Parse commands and config│
            │      │                                   │
            │      ├─▶ internal/agent   Select adapter │
            │      ├─▶ internal/session Persist session│
            │      └─▶ internal/proxy   Relay PTY I/O  │
            │                        │                 │
            │                        ▼                 │
            │            go-pty (unix pty / ConPTY)    │
            └────────────────────────┼─────────────────┘
                                     ▼
                          claude / hermes / any cmd
```

### Layers and responsibilities

| Package | Responsibilities | Dependencies |
|---|---|---|
| `internal/cli` | Subcommands, flags, configuration loading, exit codes | stdlib |
| `internal/agent` | Detect backends, track capabilities, build launch arguments | stdlib |
| `internal/session` | Read and write session metadata, event logs, and raw logs | stdlib |
| `internal/proxy` | Allocate the PTY, manage raw mode and resizing, copy in both directions, propagate exit codes | `go-pty`, `x/term` |
| `cmd/geminus` | Thin entry point (`main` only) | Packages above |

**Only two external dependencies**: `github.com/aymanbagabas/go-pty` (one API for Unix PTYs and Windows ConPTY) and `golang.org/x/term` (raw mode and terminal size).
Everything else uses the standard library. Cross-compilation is supported with `CGO_ENABLED=0`.

### Why a PTY?

`claude` and `hermes chat` are TUIs. Pipes break colors, full-screen updates, and keyboard input.
geminus gives the child process **a real TTY** and relays data between the PTY's master side and the user's terminal.
The agent sees an ordinary terminal, and the user sees the agent behaving as usual.

---

## 3. Session model

### 3.1 Storage layout

```
$GEMINUS_HOME (default: ~/.geminus)
  config.json                      # Optional; sets the default agent and other preferences
  sessions/
    <id>/
      session.json                 # Metadata (updated by atomic rename)
      transcript.jsonl             # Append-only event stream
      raw.log                      # Raw PTY bytes (disabled by --no-record)
```

- `<id>` has the form `YYYYMMDD-HHMMSS-<4hex>`: human-readable and chronologically sortable.
- Directories use `0700`; files use `0600`. Defaults are restrictive because conversations may contain secrets.

### 3.2 session.json

```json
{
  "version": 1,
  "id": "20261119-105700-a1b2",
  "title": "geminus design",
  "agent": "claude",
  "agent_session": { "kind": "uuid", "value": "3f2c...-..." },
  "cwd": "/Users/vvx/projekt/go/geminus",
  "created_at": "2026-11-19T10:57:00+09:00",
  "updated_at": "2026-11-19T11:02:11+09:00",
  "status": "exited",
  "exit_code": 0,
  "turns": 3,
  "raw_bytes": 18422
}
```

### 3.3 transcript.jsonl

Append-only, one event per line. **Byte offsets** into `raw.log` tie events to the exact bytes in the raw log.

```json
{"seq":1,"ts":"...","type":"session.start","agent":"claude","argv":["claude","--session-id","..."]}
{"seq":2,"ts":"...","type":"output","offset":0,"bytes":812}
{"seq":3,"ts":"...","type":"input","text":"..."}          // ask / non-TTY only
{"seq":4,"ts":"...","type":"note","text":"resume unsupported"}
{"seq":5,"ts":"...","type":"session.exit","code":0}
```

In `argv`, **redact only the values** of flags such as `--api-key`, `--token`, and `--password`. Keep the flag names.

### 3.4 Session continuity (highest priority)

geminus does not maintain a conversation history of its own. **It delegates resumption to each agent's native resume mechanism.**

| Agent | New session | Resume | Key |
|---|---|---|---|
| `claude` | `claude --session-id <uuid>` | `claude --resume <uuid>` | UUIDv4 generated by geminus |
| `hermes` | `hermes chat -c <id> --create-if-missing` | Same command (resume by matching name) | geminus ID as the session name |
| Arbitrary command | Launch as given | Unsupported (warn and launch as a new session) | None |

- `claude --session-id <uuid>` requires a UUID. geminus generates it and stores it in `session.json`.
- `hermes -c <name> --create-if-missing` is idempotent: create if missing, otherwise resume. Use the geminus ID itself as the name.
- Resume fails if the target has disappeared (for example, after cleanup by claude). `--fresh` provides a way forward: keep the geminus session but **start a new session on the agent side only**.

### 3.5 Concurrent use

Do not open the same session in two processes at once. Refuse to launch if `status` in `session.json` is `running` and the PID is alive. `--force` overrides the check.
Do not create a lock file: checking the PID is sufficient and avoids stale locks after a crash.

---

## 4. CLI specification

```
geminus run [--agent NAME] [--title T] [--resume ID] [--fresh] [--no-record] [--] [prompt...]
geminus ask [--agent NAME] [--session ID] [--continue] [--json] "prompt"
geminus ls [--json] [--all]
geminus show <id> [--json]
geminus log <id> [--raw] [--json] [--tail N]
geminus rm <id> [--force]
geminus agents            # Detected backends and their capabilities
geminus doctor            # PTY, terminal, and backend diagnostics
geminus version
```

Global options: `--home DIR` (overrides `GEMINUS_HOME`), `--agent NAME` (defaults to the configured agent, otherwise `claude`), `--json`, and `-v`.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | geminus error (such as I/O or PTY allocation failure) |
| 2 | Usage error |
| Other | Return the child's exit code unchanged (`run` / `ask`) |

### `run` behavior

1. Resolve the session (`--resume` / new / most recent).
2. If the terminal is a TTY, enter raw mode (always restore it with `defer`).
3. Allocate a PTY and launch the child with the argv built by the adapter.
4. Copy stdin → PTY and PTY → stdout, teeing output to `raw.log`.
5. Resize on `SIGWINCH`; forward `SIGINT`/`SIGTERM` to the child's process group.
6. Save the child's exit code and exit with the same value.

If stdin is not a TTY (piped input), skip raw mode and send `\x04` once at EOF.

### `ask` behavior

`ask` is the non-interactive counterpart to `run`. Launch the child with pipes rather than a PTY; capture stdout for display and recording.
Use `-p` for claude and `chat -q ... --oneshot` for hermes. Use the same session continuity keys as `run`.

---

## 5. Failure modes and safeguards

| Condition | Behavior |
|---|---|
| Agent binary missing | Detect before launch, provide guidance equivalent to `doctor`, and exit with code 1. |
| PTY allocation fails | Stop with an explicit error. Do not silently fall back to pipes and break the TUI. |
| Resume target missing | Report the error and suggest `--fresh`. |
| Recording write fails | Warn on stderr and keep the session running rather than interrupting the conversation. |
| raw.log exceeds its limit | Stop recording at the default limit of 64 MiB and leave a `note` event. The conversation continues. |
| Terminal might not be restored | Restore via both `defer` and a signal handler. Let `doctor` check the terminal state. |
| Same session launched twice | Refuse unless explicitly overridden with `--force`. |
| Secrets in session data | Use 0600 for files, 0700 for directories, redact argv values, and offer `--no-record`. |

**Out of scope**: reading, storing, or forwarding credentials. geminus simply passes the environment through to the agent without inspecting it.

---

## 6. Verification strategy

| Stage | Method | Pass criteria |
|---|---|---|
| Unit | `go test ./internal/...` | Table-driven tests for session/agent/proxy pass. |
| Relay | Launch a fake agent (`test/fakeagent`) through a PTY | Output matches, exit codes propagate, resizing works, and raw.log offsets match. |
| Non-TTY | `run` with piped stdin | No raw mode; exits normally at EOF. |
| E2E | Execute the built binary as a subprocess | `ls`/`log`/`show`/`rm` behave as expected. A corrupt session.json does not cause a panic. |
| Cross-build | `scripts/crossbuild.sh` | All six darwin/linux/windows × amd64/arm64 targets build (on Windows this guarantees compilation of the ConPTY path). |
| Manual | Complete one exchange with real `claude` and real `hermes`, then continue with `--resume` | Context is retained. |
| Manual (OS) | Windows hardware (mirai), Linux (sibyl) | `run` works as a TUI. |

No CI for v0.1. In keeping with the owner's existing workflow, document the commands in `scripts/` and the README.

---

## 7. Decisions (approved 2026-11-19)

1. **Transparent byte relay**. Do not structure, summarize, or reinject output. Delegate session continuity to each agent's resume mechanism.
2. **No backend**. A single binary and local files only; no daemon or IPC.
3. **Attach is a non-goal**. Resume provides continuity. Reconsider live multiplexing in v0.2 if there is demand.
4. **Two dependencies: go-pty and x/term**. Reject a zero-dependency, in-house PTY implementation because it would substantially increase effort and risk on Windows.
5. **Store state in `~/.geminus`**, overridable with `GEMINUS_HOME`.
6. **Use module path `github.com/hauntedfail/geminus`** to support `go install`.

## 8. Approvals (2026-11-19)

- **A. Meaning of proxy**: Approved transparent byte relay and delegation to each agent's native resume. History reinjection by geminus is deferred until v0.2 or later.
- **B. v0.1 scope**: Approved `run` + `ask` + `ls/show/log/rm/agents/doctor`.
- **C. Dependencies**: Approved `go-pty` and `x/term` only.
- **D. State directory**: Approved `~/.geminus`, overridable with `GEMINUS_HOME`.

## 9. Assumptions and risks

- ~~Go toolchain not installed~~ → Go 1.27.4 installed on 2026-11-19 (`/opt/homebrew/bin/go`).
- `claude --session-id` requires a UUID. Generate and persist it.
- During implementation, check on a real system which characters `hermes -c <name>` accepts in session names. The geminus ID uses only alphanumerics and hyphens as a precaution.
- For Windows ConPTY, **successful compilation is the v0.1 acceptance threshold**. Test manually on Windows hardware (mirai).
- Raw logs contain the conversations themselves. Exclude them from backups and sharing; state this explicitly in the README.

## 10. Task breakdown

| ID | Scope | Dependencies |
|---|---|---|
| gmn-001 | Scaffolding (go.mod, CLI skeleton, version/help) | — |
| gmn-002 | Session store (metadata, events, atomic updates) | 001 |
| gmn-003 | Agent adapters (claude / hermes / generic) | 001 |
| gmn-004 | PTY proxy (raw mode, resizing, exit codes) | 001 |
| gmn-005 | Wire up the CLI (run / ask / ls / show / log / rm / agents / doctor) | 002,003,004 |
| gmn-006 | E2E (fake agent, non-TTY input, corrupt data handling) | 005 |
| gmn-007 | Documentation and cross-builds | 005 |

Implementation starts **only after architecture approval**. Until then, all tasks are `blocked`.

---

## 11. Implementation results (2026-11-19)

All seven units are complete. The single-binary Go implementation is in `/Users/vvx/projekt/go/geminus`.

### Verified by execution

| Check | Result |
|---|---|
| `go build ./...` / `go vet ./...` | exit 0 |
| `go test ./... -count=1` | 579 tests PASS / FAIL 0 / SKIP 0 |
| `-race` / `-count=8` | PASS; no flaky failures observed |
| Coverage | agent 96.5% / cli 93.5% / proxy 90.1% / session 87.0% / version 100% |
| Cross-builds | All six darwin/linux/windows × amd64/arm64 targets succeeded |
| Real claude (ask) | Returned `GEMINUS_OK` and exited with code 0 |
| Real claude (resume) | Correctly recalled the previous token in the same session |
| Real hermes (ask / resume) | Returned `HERMES_GEMINUS_OK` and answered correctly on resume |
| Real PTY input | Sent hello to `sh -c 'read x; echo GOT:$x'`; received `GOT:hello`, exit code 0 |
| Terminal restoration | ECHO on the outer PTY was restored after geminus exited |
| Secret redaction | Values such as `API_KEY="s3cr3t"` did not remain in session.json or the transcript (they did remain in raw.log as child output) |
| Documentation | README: 179 lines; docs/architecture.md: 309 lines; crossbuild.sh: 65 lines |

### Unverified items and subsequent checks

- **Execution on Windows hardware**: Cross-build and vet only. ConPTY and VT mode remain unverified. Test manually on mirai.
- Linux was verified in a container on 2026-11-19 (`bash scripts/test-linux.sh`, `golang:1.27` / linux/arm64, run as an unprivileged user). `go vet` exited with code 0; 579 tests PASS / FAIL 0 / SKIP 0. These included passing tests using a real PTY, including termination with EIO. Real claude / real hermes on Linux (sibyl) have not been tested.
- ~~One exchange in the real interactive claude TUI~~ → Verified on 2026-11-19. Launched `geminus run --agent claude` through a PTY and verified input echoed in the prompt field, a reply containing `GEM_NUS77` (a string not present in the prompt), exit code 0 after `/exit`, and a session entry in `ls`. The earlier failure was in the harness: it sent the prompt before the TUI finished starting. Interactive TUI `--resume` remains unverified (`ask` resume was verified).
- `go install ...@latest` remains unverified because the repository has not been published.
- There is no lock for concurrent launches, by design (`Meta.Active()` + `--force`).

### Decisions still needed

1. Whether to start using Git (`git init` + initial commit, following the signed-commit policy). **Not executed**.
2. Whether to create `hauntedfail/geminus` on GitHub (publication and push require owner approval). **Not executed**.
3. Acceptance testing on Windows hardware (mirai). The Linux test suite passed in a container. Testing with real agents on sibyl would access the VPS, so wait for the owner's instructions.
