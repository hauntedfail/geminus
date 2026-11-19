# geminus architecture

This document describes how the code works today. The original plan is in `plans/geminus/plan.md`.
See [Differences from the plan](#differences-from-the-plan) for discrepancies. Where the two differ, the implementation is authoritative.

## Goals

geminus is a CLI that relays input and output unchanged for Claude Code and Hermes Agent. It has four design goals:

1. **Transparency:** Pass the agent's output through byte for byte without interpreting it.
2. **Continuity:** Close a session and resume the conversation later with the same geminus session ID.
3. **Low overhead:** No daemon, database, HTTP layer, or resident process. All state lives in local files.
4. **Portability:** Offer the same commands on macOS, Linux, and Windows. Handle PTY differences internally.

geminus implements no protocol and does not communicate over the network. It wraps agent CLIs on the same host.

## Layers

```
            ┌───────────────────────────────────────────┐
  terminal  │  geminus (single binary)                  │
  ──stdin──▶│                                           │
            │  internal/cli      commands and exit codes│
            │      │                                    │
            │      ├─▶ internal/agent    build argv     │
            │      ├─▶ internal/session  persist files  │
            │      └─▶ internal/proxy    relay PTY I/O  │
            │                        │                  │
            │                        ▼                  │
            │            go-pty (Unix PTY / ConPTY)     │
            └────────────────────────┼──────────────────┘
                                     ▼
                          claude / hermes / any command
```

| Package | Responsibility | Dependencies |
|---|---|---|
| `cmd/geminus` | Contains only `main`; exits with the return value of `cli.Run` | `internal/cli` |
| `internal/cli` | Handles subcommands, flags, `config.json`, exit codes, and session startup and shutdown | `agent`, `session`, `proxy`, `x/term` |
| `internal/agent` | Builds backend-specific argument lists, detects agents, and redacts secrets in launch arguments | Standard library |
| `internal/session` | Reads and writes `session.json`, `transcript.jsonl`, and `raw.log` | Standard library |
| `internal/proxy` | Allocates the PTY, manages raw mode and resizing, forwards signals, copies data in both directions, and reports exit codes | `go-pty`, `x/term`, `x/sys` |
| `internal/version` | Provides the version string (overridden at build time with `-ldflags -X`) | Standard library |

`agent`, `session`, and `proxy` do not depend on one another. Only `cli` ties them together.

There are three external modules: `github.com/aymanbagabas/go-pty`, `golang.org/x/term`, and `golang.org/x/sys`.
The binary can be cross-compiled with `CGO_ENABLED=0`.

### Why use a PTY?

`claude` and `hermes chat` are terminal UIs. Connecting them to pipes breaks colors, full-screen updates, and keyboard input.
geminus gives the child a real TTY and relays data between the master side of the PTY and the user's terminal.
The agent sees an ordinary terminal, and the user sees the agent as usual.

Only `ask` skips the PTY. It runs a single noninteractive turn, launching the agent with pipes and capturing stdout.

## Session model

### Storage layout

```
$GEMINUS_HOME (defaults to ~/.geminus)
  config.json                      Optional. Set {"agent": "hermes"} to change the default backend.
  sessions/
    <id>/
      session.json                 Session metadata
      transcript.jsonl             Append-only event stream
      raw.log                      Raw PTY output (not created with --no-record)
```

- The state directory is chosen in this order: `--home`, the `GEMINUS_HOME` environment variable, then `~/.geminus`.
- `<id>` has the form `YYYYMMDD-HHMMSS-<4 hex digits>`. It uses local time, so sorting by name gives chronological order.
  Because the ID forms part of a path, geminus checks its format with a regular expression before accessing any files.
- Directories use mode `0700` and files use `0600`. geminus reapplies these permissions on creation and when opening for append, even if the umask removed permissions or an existing file's permissions were too broad.
- Directories are not created until the first `Create`. Neither `ls` nor `doctor` writes anything.

### session.json

```json
{
  "version": 1,
  "id": "20261119-122625-df5d",
  "title": "first prompt",
  "agent": "claude",
  "agent_session": { "kind": "uuid", "value": "65f7d3f5-8641-41bf-a82d-3abb247e72bd" },
  "cwd": "/Users/vvx/projekt/go/geminus",
  "created_at": "2026-11-19T12:26:25.883775+09:00",
  "updated_at": "2026-11-19T12:26:26.134521+09:00",
  "status": "exited",
  "exit_code": 0,
  "turns": 0,
  "raw_bytes": 398
}
```

- To update metadata, geminus writes a temporary file in the same directory, calls `fsync`, then replaces the original with `rename`. Readers see either the old contents or the new ones.
- `status` goes from `created` to `running` to `exited`. While it is `running`, `pid` holds the PID of the geminus relay process.
- `agent_session` identifies the conversation on the agent side. claude uses `kind: "uuid"`, hermes uses `kind: "name"`, and cmd has no key.
- `exit_code` is absent until the child exits. Zero is stored as a value, too.
- `turns` counts `ask` exchanges. `run` does not increment it because geminus does not count turns in an interactive conversation.
- geminus rejects files whose `version` exceeds the current format version (1).

### transcript.jsonl events

The transcript is append-only, with one event per line. `seq` numbers events within a session and continues where it left off when the session resumes.

| `type` | Meaning | Main fields |
|---|---|---|
| `session.start` | Agent started | `agent`, `argv` (secrets redacted) |
| `input` | User input: only `ask` prompts and piped input to `run` | `text` |
| `output` | Byte range in `raw.log` | `offset`, `bytes` |
| `note` | Note from geminus (for example, `--fresh`, unsupported resume, or the recording limit) | `text` |
| `session.exit` | Child exited | `code` |

```json
{"seq":1,"ts":"…","type":"session.start","agent":"claude","argv":["claude","--session-id","65f7…","first prompt"]}
{"seq":2,"ts":"…","type":"input","text":"hello\nquit\n"}
{"seq":3,"ts":"…","type":"output","offset":0,"bytes":149}
{"seq":4,"ts":"…","type":"session.exit","code":0}
```

### Events and offsets

An `output` event does not contain the output itself. It points to the byte range `[offset, offset+bytes)` in `raw.log`.

- A TUI can write dozens of times per second. Rather than creating an event for every read, geminus groups consecutive output into one `output` event per one-second window. If another event occurs during the window, geminus flushes pending output first to preserve event order.
- On resume, the initial offset is the existing length of `raw.log`. A session's `raw.log` contains the concatenated output from all its runs.
- Interactive keystrokes are not recorded as input events. In raw mode, input consists of fragments of escape sequences; echoed input appears in `raw.log` as PTY output.
- Each line is appended with a single `write` (`O_APPEND`). If a crash leaves an incomplete line without a trailing newline, the next append inserts a newline first. Readers skip malformed lines and return the rest.
- When `raw.log` reaches 64 MiB, geminus stops recording and writes a single `note`. The relay continues.

### Resuming a conversation

geminus does not maintain conversation history. It relies on each agent's resume mechanism.

| Scenario | Behavior |
|---|---|
| New session | The adapter creates a key, and geminus saves it in `session.json` before launching the agent. |
| `run --resume ID` | Resume with the saved key. |
| `run --resume ID --fresh` | Generate a new key and start a new conversation. The geminus session, including its recording and title, stays the same. |
| Adapter without resume support (cmd) | Record a `note` and start a new process. |
| `ask --session ID` / `ask --continue` | Continue with a one-shot request using the saved key. |

When resuming, geminus runs the child in the `cwd` where the session was created because claude stores conversations by directory.
If that differs from the current directory, geminus reports it on stderr. A relative `cwd` or one that no longer exists is an error.

### Detecting duplicate starts

If `status` in `session.json` is `running` and the process identified by `pid` is alive, geminus treats the session as open and refuses to start it unless you pass `--force`. There is no lock file.

- Two processes can both start if their read and write operations overlap.
- If another process reuses the PID, the session can appear open when it is not. `ls` reports a session marked `running` as `stale` if its PID is no longer alive.

### Safeguards when reading session data

Other programs can write to session files, so geminus does not trust their contents.

- If `session.json`, `transcript.jsonl`, or `raw.log` is not a regular file (for example, a symbolic link, named pipe, device, or directory), geminus returns `ErrNotRegular` without reading it. Following a symbolic link could make `geminus log` display a file outside the session. Opening a named pipe could block while waiting for a writer.
- geminus checks with `Lstat`. On Unix it opens the file with `O_NOFOLLOW` and `O_NONBLOCK` and checks the open file with `fstat` in case it was replaced between `Lstat` and `open`. Windows uses only `Lstat` and the post-open check.
- The same checks apply when opening for append. On Unix, geminus uses `O_NOFOLLOW` and `O_NONBLOCK` and verifies that the open file is regular before setting permissions. It does not distinguish hard links: creating one requires write access to the session directory, which already allows its contents to be replaced directly.
- If the session directory `sessions/<id>` is itself a symbolic link, geminus neither reads nor writes its target. `rm` removes only the link. `GEMINUS_HOME` and its parent directories may be symbolic links.
- `ls` and `show` replace control characters in fields such as `title` and `cwd` with `?` before displaying them. `log` strips control sequences by default.
- On resume, `cwd` must be an absolute path to an existing directory.

## Adapter contract

Each backend implements `agent.Adapter`. An adapter builds argument lists; it does not launch processes.

```go
type Adapter interface {
    Name() string                 // Name passed to --agent
    Binary() string               // Executable to look up on PATH; empty for cmd
    Capabilities() Capabilities   // Resume / Oneshot / SessionKey
    NewKey(geminusID string, fresh bool) (SessionKey, error)
    Interactive(req Request) ([]string, error)            // argv for run
    Oneshot(req Request, prompt string) ([]string, error) // argv for ask
}
```

| | claude | hermes | cmd |
|---|---|---|---|
| Key | UUIDv4 generated by geminus | Session name (the geminus ID) | None |
| New | `claude --session-id <uuid> [prompt]` | `hermes chat -c <name> --create-if-missing [-q prompt]` | argv after `--`, unchanged |
| Resume | `claude --resume <uuid> [prompt]` | Same as new | `ErrNoResume` (the CLI starts a new process) |
| One-shot | `claude -p --session-id\|--resume <uuid> <prompt>` | Append `-q <prompt> --oneshot` to the new-session argv | `ErrNoOneshot` |
| Key with `--fresh` | New UUID | `<ID>-<4 hex digits>` | Not applicable (error) |

Rules:

- Positional arguments are joined with spaces to form a single prompt. A prompt starting with `-` is a usage error because the agent would interpret it as a flag.
- Each adapter validates its key format (the shape of a UUID or the characters allowed in a hermes name). Even if someone edits `session.json`, an invalid key never reaches argv.
- geminus looks up the executable on `PATH` before launch. If it is missing, geminus stops before creating a session and suggests running `doctor`.
- If launch fails, geminus deletes a newly created session or restores an existing session's metadata to its pre-launch state.

### Redacting secrets in launch arguments

Both `argv` in `session.start` and the cmd backend's default title pass through `agent.RedactArgv`. It replaces values with `[REDACTED]` but keeps the key names.

- Flags and environment-style assignments: `--api-key VALUE`, `--token=VALUE`, `OPENAI_API_KEY=VALUE`. Matching uses the end of the key name, so `--max-tokens` and `--tokenizer` do not match.
- Assignments embedded in a single argument: `NAME_TOKEN=value`, `NAME_TOKEN="value"`, `NAME_TOKEN='value'`. Quotation marks are preserved.
- Headers: `X-Api-Key: value`, `client_secret: value`. Only compound names containing a separator match. Standalone `token:` and `password:` do not match, so text such as `Unexpected token: }` is left alone.
- JSON keys: `"token": "value"`, `'password': 'value'`. If both key and value are quoted, even a single-word key matches.
- `Authorization: Bearer value` (the scheme name is preserved), `Authorization: value`, and `scheme://user:pass@host`. An unquoted `Authorization:` value is redacted only if it consists of one or two words, so prose such as `authorization: handled by the middleware` is left alone.

Redaction cannot identify every secret in arbitrary text. It can miss `--token value` inside a single argument or `password: value` in prose, for example. With `--no-record`, all arguments except the command name are hidden.
Redaction applies only to argv; it does not modify `raw.log`.

## PTY relay and raw mode

`proxy.Run` handles one relay session. The terminal (`Terminal`) and child launcher (`Spawner`) are interfaces; tests can substitute in-memory implementations.

Sequence of operations:

1. Start handling signals (SIGINT, SIGTERM, SIGHUP, SIGQUIT, and SIGPIPE on Unix).
2. If the input is a TTY, enter raw mode. Use `defer` to restore the terminal.
3. Read the terminal size, allocate a PTY of that size, and launch the child. If the size is unavailable, use 80x24.
4. Use separate goroutines to copy output (PTY to terminal and `raw.log`) and input (terminal to PTY).
5. Forward resize events and signals to the child.
6. Wait for the child to exit, drain the remaining output, then close the PTY.
7. Restore the terminal and stop handling signals.

Key details:

- **Signal handling begins before raw mode and ends after restoration.** Otherwise, the default action for SIGTERM could leave the terminal in raw mode. geminus forwards signals to the child's process group and waits for the child before exiting normally. It handles SIGPIPE without forwarding it: writing to a closed stdout returns EPIPE, allowing geminus to restore the terminal before exiting.
- **In raw mode, Ctrl-C is not a signal at the user's terminal.** The byte `0x03` passes through the PTY, and the terminal driver on the child's side sends SIGINT to the child.
- **Non-TTY input does not trigger raw mode.** At EOF, geminus sends EOT (`0x04`) to the child once. EOT in the middle of a line does not signal EOF, so geminus first adds a newline if the input did not end with one. The child still gets a PTY, which echoes the input.
- **Failure to allocate a PTY is an error.** geminus does not silently fall back to pipes because that would break the TUI.
- **Recording failures do not stop the relay.** If a write to `raw.log` fails, geminus stops recording but continues relaying and issues a warning after the session ends.
- **Exit codes** come directly from the child. On Unix, if a signal terminates the child, the code is 128 + the signal number.
- **After the child exits,** geminus reads until PTY EOF. EOF may never arrive if a grandchild holds the PTY open or when using ConPTY, so draining stops after 100 ms without output or two seconds overall.

Platform differences:

| | Unix | Windows |
|---|---|---|
| PTY | `openpty` via go-pty | ConPTY via go-pty |
| Resize detection | SIGWINCH | Compare sizes every 250 ms |
| Signal forwarding | `kill` the child's process group with the signal | Send Ctrl-C as the input byte `0x03`; force termination for other signals |
| Terminal setup | Raw mode | Raw mode with VT input and output enabled |
| Echo check (`doctor`) | Read the termios `ECHO` flag | Not supported |

### Terminal checks in doctor

`geminus doctor` reports whether stdin is a TTY, the terminal size, and whether echo is disabled.

Raw mode disables echo. If echo is off while geminus is not relaying, a process may have exited without restoring the terminal. This can happen to geminus itself because it cannot catch `SIGKILL`.
If echo is off, `doctor` reports `warn` and suggests `stty sane` in a one-line message. It does not change the terminal settings.

With `--json`, the output includes a `terminal` object:

```json
"terminal": { "tty": true, "cols": 120, "rows": 40, "echo": true, "echo_supported": true }
```

- `tty` indicates whether stdin is a terminal.
- `cols` and `rows` are 0 if the size is unavailable.
- `echo_supported` indicates whether the echo setting could be read. It is `false` on Windows or when none of stdin, stdout, and stderr is a terminal. In that case `echo` has no meaning.

## Failure modes

| Condition | Behavior |
|---|---|
| Agent executable missing | Stop before creating a session, suggest `doctor`, and exit with code 1. |
| Unknown `--agent` | Usage error (exit code 2). |
| Cannot allocate a PTY | Stop with an error; do not fall back to pipes. Delete any session created for this run. |
| Cannot launch the child | Delete a newly created session. For an existing session, restore the metadata and record a `note`. |
| Agent no longer has the conversation to resume | geminus does not detect this. The user sees the agent's error, and geminus returns its exit code. Use `--fresh` to start over. |
| Cannot write to `raw.log` or `transcript.jsonl` | Continue relaying and warn on stderr after exit. |
| `raw.log` reaches 64 MiB | Stop recording, but keep relaying. Record a `note`. |
| Session already open | Refuse to start unless `--force` is given. |
| Corrupt `session.json` | Return `ErrCorrupt`. `ls` lists the remaining sessions and warns on stderr; `rm` can still delete the corrupt session. |
| Malformed line in `transcript.jsonl` | Skip that line. `log --json` prints the readable events and exits with code 1. |
| Session file is not a regular file | Return `ErrNotRegular` without reading it. |
| geminus receives SIGTERM or SIGHUP | Forward the signal to the child, wait for it to exit, and restore the terminal. |
| geminus receives SIGKILL | The terminal stays in raw mode. `doctor` detects it; `stty sane` restores it. |
| Secrets in recordings | Protections include `0600` files, `0700` directories, redaction of launch argument values, and `--no-record`. |

geminus does not read, store, or forward credentials. The agent simply inherits its environment unchanged.

## Non-goals

The following are out of scope for v0.1:

- **Attach:** Connecting to a running session from another terminal (tmux-style multiplexing). Reopen with `--resume` to continue instead.
- **Daemon, backend service, or relay service:** There is no resident process or IPC.
- **History reinjection:** Summarizing a conversation and feeding it back to the agent. geminus does not maintain history; it relies on the agent to resume conversations.
- Agent-to-agent communication, sending to multiple agents at once, and push notifications.
- Credential management.
- A GUI or TUI dashboard.

## Differences from the plan

Where `plans/geminus/plan.md` differs from the implementation, the implementation is authoritative.

| Plan | Implementation |
|---|---|
| Two external dependencies: `go-pty` and `x/term` | `golang.org/x/sys` is also a direct dependency so `doctor` can read the termios `ECHO` flag. `x/term` already uses that module, so `go.sum` did not change. |
| `run` selects a session by `--resume`, new session, or most recent session | `run` only resumes an explicit session or starts a new one. `ask --continue` selects the most recent session. |
| Missing conversation on resume: error and a hint to use `--fresh` | geminus does not detect this; the agent reports its own error. The README explains `--fresh`. |
| On non-TTY EOF, send `\x04` once | If the input does not end in a newline, send `\n\x04` (EOT in the middle of a line does not signal EOF). |
| One `output` event per read | Group output into one event per one-second window. |
| No `pid` in the `session.json` example | While `running`, the session has a `pid` for detecting duplicate starts. |
| No `run --force` in the CLI listing | Both `run --force` and `rm --force` are available. |
| No description of `ls --all` | `ls` lists the 20 newest sessions first by default; `--all` lists them all. |
| Redact values for flags like `--api-key`, `--token`, and `--password` | Also redact environment-style values, embedded assignments (including quoted ones), headers, values of JSON keys, and URL userinfo. |
| No mention of symbolic links | Session files and directories are read or written only if they are regular files and real directories, respectively. |
| Use `doctor` to check whether the terminal was restored | Read the echo setting and warn if it is off. Not supported on Windows. |
| Windows: guarantee only that ConPTY compiles | As planned. Runtime behavior on Windows has not been verified. |
