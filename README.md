# geminus

A lightweight CLI that relays input and output unchanged between your terminal and Claude Code or Hermes Agent through a pseudoterminal (PTY).

- geminus passes the agent's output bytes straight to the terminal without interpreting or reconstructing them.
- After closing a session, you can resume the conversation with the same geminus session ID. Each agent handles resumption itself.
- There is no daemon, database, HTTP layer, or resident process. All state lives in local files.
- A single binary (CGO disabled) provides the same commands on macOS, Linux, and Windows.

See [the architecture guide](docs/architecture.md) for the design.

## Installation

Requires Go 1.23 or later.

```sh
go install github.com/hauntedfail/geminus/cmd/geminus@latest
```

To build from source, run these commands from the repository root:

```sh
go build -o geminus ./cmd/geminus
./geminus version
```

To build release binaries, run `bash scripts/crossbuild.sh`. It produces six binaries in `dist/`: amd64 and arm64 builds for darwin, linux, and windows.

Install the agents (`claude`, `hermes`) separately and put them on your `PATH`. Each agent handles its own authentication; geminus neither reads nor stores credentials.

## Quick start

```sh
# Check the PTY, terminal, and available agents
geminus doctor
geminus agents

# Start an interactive session (defaults to claude)
geminus run
geminus run "Explain how this repository is organized"
geminus run --agent hermes --title "Design discussion"

# When the session ends, geminus prints the resume command to stderr
#   geminus: session 20261119-122625-df5d ended (exit 0). Resume with: geminus run --resume 20261119-122625-df5d
geminus run --resume 20261119-122625-df5d

# Ask a single question (no PTY)
geminus ask "List the dependencies in go.mod"
geminus ask --continue "Which of those are direct dependencies?"  # Continue the most recently updated session in this directory
geminus ask --session 20261119-122625-df5d "Go on"               # Continue a specific session
cat notes.txt | geminus ask "Summarize these notes"               # stdin goes straight to the agent
geminus ask --json "hello"                             # {"session","agent","exit_code","output"}

# Inspect sessions
geminus ls                    # The 20 newest first; --all lists every session, --json outputs JSON
geminus show 20261119-122625-df5d
geminus log 20261119-122625-df5d              # Recorded output without control sequences
geminus log 20261119-122625-df5d --tail 20    # Last 20 lines
geminus log 20261119-122625-df5d --raw        # Keep control sequences
geminus log 20261119-122625-df5d --json       # Events from transcript.jsonl

# Delete a session
geminus rm 20261119-122625-df5d
```

You can also run an arbitrary command through a PTY. Everything after `--` is passed through as the command line.

```sh
geminus run --agent cmd -- sh -c 'echo hello'
```

The common flags are `--home DIR`, `--agent NAME`, `--json`, and `-v`. They can go before or after the command name.
Run `geminus help <command>` for details about a command.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | geminus error (for example, an I/O error or failure to allocate a PTY) |
| 2 | Usage error |
| Other | `run` and `ask` return the child's exit code unchanged. On Unix, if a signal terminates the child, the code is 128 + the signal number. |

## Backends

| `--agent` | New session | Resume (`run --resume`) | One-shot (`ask`) |
|---|---|---|---|
| `claude` | `claude --session-id <uuid>` | `claude --resume <uuid>` | `claude -p --session-id <uuid> <prompt>` (use `--resume <uuid>` to continue) |
| `hermes` | `hermes chat -c <name> --create-if-missing` | Same command; resumes if the name matches | Append `-q <prompt> --oneshot` to the new-session command |
| `cmd` | Run the command line after `--` unchanged | Not supported; geminus records a note and starts a new process | Not supported |

- geminus generates `<uuid>` and saves it in `session.json`.
- `<name>` is the geminus session ID.
- `--resume ID --fresh` starts a new conversation on the agent side but keeps the geminus session.
- The default backend is `claude`. To change it, set `{"agent": "hermes"}` in `$GEMINUS_HOME/config.json`.
- A resumed session runs in the directory where it was created because claude stores conversations by directory. If that differs from your current directory, geminus tells you on stderr.

## Session storage

```
~/.geminus/                         # Override with GEMINUS_HOME or --home
  config.json                       # Optional
  sessions/
    20261119-122625-df5d/           # ID format: YYYYMMDD-HHMMSS-<4 hex digits>
      session.json                  # Session metadata
      transcript.jsonl              # Events: start, input, output ranges, notes, exit
      raw.log                       # Raw PTY output
```

- geminus creates directories with mode `0700` and files with mode `0600`. `geminus doctor` warns if other users can read them.
- geminus uses `--home DIR` first, then the `GEMINUS_HOME` environment variable, then `~/.geminus`.
- Recording to `raw.log` stops at 64 MiB. The conversation continues.

### What gets recorded

**`raw.log` contains the conversation itself.** It captures everything the agent displays, including pasted code, the contents of files it reads, and any tokens shown on screen.

- Before including `~/.geminus` in backups or cloud sync, make sure the destination is suitable for storing your conversations.
- Review session directories and `geminus log` output before sharing them.
- To avoid recording output, use `geminus run --no-record`. It skips `raw.log` and output events, keeps only the command name from the launch arguments, and does not use the prompt as the title.

In the launch arguments saved to `transcript.jsonl`, geminus replaces values that look like secrets with `[REDACTED]`. Examples include:

- `--api-key VALUE`, `--token=VALUE`, `OPENAI_API_KEY=VALUE`
- Assignments inside a single argument: `NAME_TOKEN=value`, `NAME_TOKEN="value"`, or `NAME_TOKEN='value'`
- `X-Api-Key: value`, `"token": "value"`, `Authorization: Bearer value`, `https://user:pass@host`

Redaction applies only to launch arguments; it does not change `raw.log`. It cannot identify every secret in arbitrary text (for example, `abc` in `sh -c 'curl --token abc'`). Pass sensitive values through environment variables or use `--no-record`.

If `session.json`, `transcript.jsonl`, or `raw.log` has been replaced with a symbolic link, named pipe, or other non-regular file, geminus returns an error rather than opening it.

## Known limitations

- **Piped input is echoed on screen.** Even when stdin is not a terminal, as in `printf 'hello\n' | geminus run`, the child gets a PTY. The PTY echoes its input, so a copy of the input (and `^D`) appears in the output and in `raw.log`. geminus signals the end of input by sending EOT (Ctrl-D) to the child once.
- **Windows support is compile-tested only.** The ConPTY code builds, but it has not been tested on a Windows machine. The full test suite passes on macOS and Linux.
- **There is no lock against concurrent starts.** geminus checks the `status` and PID in `session.json` and refuses to reopen a running session unless you pass `--force`. Without a lock file, two processes starting close enough together can both get through. A reused PID can also make a session appear to be running.
- **You cannot attach from another terminal.** Reopen the session with `--resume` to continue.
- **geminus does not maintain conversation history.** Resuming depends on the agent's own history. If the agent deletes it, the conversation cannot be resumed.
- `ask` records only stdout. It sends stderr to the terminal without recording it.
- With the `cmd` backend, `--resume` appends to the same geminus session log but does not continue the conversation.

## Troubleshooting

**The agent says the session does not exist and exits when I try to resume.**
The agent's history is gone. Use `--fresh` to start a new agent conversation while keeping the geminus session's recording and title.

```sh
geminus run --resume 20261119-122625-df5d --fresh
```

**I can't see what I type, or line breaks are misaligned.**
The terminal is still in raw mode. This can happen if geminus is killed (for example, with `kill -9`). `geminus doctor` reports `warn` for `terminal echo`. To restore the terminal, run:

```sh
stty sane
```

You won't see `stty sane` as you type it. If Enter doesn't submit the command, press Ctrl-J. On Windows, reopen the terminal instead.

**I can't open a session: `session ... is already open in process N`.**
Another geminus process has the session open. This can also happen if the previous process crashed and another process reused its PID. Check `geminus ls` for `running` or `stale`. Use `--force` only if you know the session is not running.

**I get `agent binary not found`.**
Run `geminus doctor` to see which agents are on your `PATH`.

**`ls` warns about a corrupt session.**
`geminus rm <id>` can delete the session even if `session.json` is unreadable.

## Development

```sh
go build ./...
go vet ./...
go test ./... -count=1
go test ./... -count=1 -race
bash scripts/crossbuild.sh
bash scripts/test-linux.sh        # Run vet and tests in a Linux container using Docker
```

The tests use a real PTY and a fake agent (`test/fakeagent`). There is no CI setup.
Use `scripts/test-linux.sh` to check Linux PTY behavior from a macOS or Windows development machine. It passes its arguments unchanged to `go test`.
