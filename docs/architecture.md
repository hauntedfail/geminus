# geminus architecture

This document describes how the code works today.

## What geminus is for

geminus lets one person talk directly to an agent that runs on another computer. Launching an agent that is already on this computer is not the point: the agent's own app does that.

It is a proxy screen in front of existing agents. The person chooses a **persona** (who: a label from a Honcho peer) and a **harness** (what: the agent program on that persona's computer), and geminus relays the conversation. Everything behind the screen (model, tools, sessions, history, permissions policy) belongs to the harness and is reached only through the harness's own published interface. geminus never drives a terminal UI, never starts a process per turn, and never touches a harness's files.

An earlier version (commit `d8ec09f`) wrapped the local `claude` and `hermes` command lines in a pseudo-terminal. That design answered the wrong question and was removed. `plans/geminus/plan.md` is its record.

## Shape

```
  terminal ── geminus ── ssh ─────────────── harness's ACP server
                 │        one process per     (hermes acp, grok agent stdio,
                 │        connection          claude-agent-acp, ...) on the
                 │                            persona's computer
                 ├── ssh, once per persona pick: which harnesses are installed there?
                 ├── Honcho: POST /v3/workspaces/<ws>/peers/list   (who exists)
                 └── ~/.geminus/config.json   (routes, defaults, extra harnesses)
```

| Package | Responsibility |
|---|---|
| `cmd/geminus` | `main` only. |
| `internal/chat` | The terminal interface: picking persona and harness, prompt, status line, slash commands, turn display, permission prompts. |
| `internal/persona` | The persona directory (Honcho peers), each persona's node (where its computer is), defaults, and writing the config file. |
| `internal/harness` | The known agent programs and the probe that finds which of them a computer has. |
| `internal/acp` | An Agent Client Protocol client over a child process's stdin and stdout. |
| `internal/acp/acptest` | A scripted ACP agent for tests. |
| `internal/version` | The version string. |

There are no third-party dependencies.

## Decisions

**Talk ACP to the harness's own server.** ACP gives structured turns, streaming, tool calls, permission requests, cancellation and session listing, and many agents serve it: natively (Hermes, OpenCode, Gemini CLI, Grok) or through a small bridge program (Claude Code, Codex, pi). One protocol therefore covers "any agent" without geminus knowing anything about a particular one. One harness process serves the whole connection, across `/new` and `/resume`.

**Reach remote computers with ssh.** ssh already provides authentication, encryption and host verification that the person has set up, so geminus adds no credentials and no listener of its own. geminus runs ssh with `BatchMode=yes` so that a missing key fails with an error instead of a password prompt fighting the conversation for the terminal.

**A persona is a label; it must be chosen.** The workspace's Honcho peers are the personas. Nothing happens before one is selected, by default or by hand. Honcho knows nothing about hosts, and guessing a host from a name would be a way to talk to the wrong machine, so the route is asked for once and stored. This computer's own persona is known from the Hermes profile and needs no route. If Honcho cannot be read, geminus says so and offers only the personas it knows locally.

**Harness choices are observed, not configured.** When a persona is picked, geminus runs one small shell script on that computer that reports which known programs are on its `PATH`. The list offered is exactly what is installed there at that moment: usable harnesses first, then agents that are installed without their ACP bridge, shown with the command that would fix it but not selectable. `/harness` looks again. The registry of known harnesses is built in and can be extended in the config file.

**Choices are made inside geminus and remembered.** The command line carries a persona name at most. Persona, harness and session mode have defaults that are set with `/default` from within the conversation. A persona's own harness beats the general default, and a default the computer does not have is skipped, not forced.

**Slash commands are shared with the harness.** geminus owns a small fixed set. Any other slash command belongs to the harness and is sent as typed, provided the harness announced it or announced no list at all. `//name` reaches a harness command that shares a name with a geminus one. A command unknown to both is a typo and is not sent.

**The remote owns the session.** Sessions, their history and their IDs live with the harness. geminus stores nothing about a conversation. `/sessions`, `--continue` and `/resume` ask the harness (`session/list`, `session/load`), so the list is always the truth, and the same session can be picked up from any computer.

**State is always on screen.** The prompt is `sibyl [REMOTE] · hermes ❯` when connected and `geminus (disconnected) ❯` when not. The status line (shown on every change and by `/status`) adds the harness's reported name and version, the route, the session and the directory. The local persona always carries `[THIS COMPUTER]`.

**Nothing is sent by accident.** Text typed while disconnected is not sent. A message in flight when the connection drops is not sent again; the person is told it may not have arrived. A tool call runs only after an explicit `y` or `a`.

## Finding the harnesses

`harness.Script` builds one line of POSIX shell: print `home=$HOME`, then `have=NAME` for each program that `command -v` finds. `persona.Node.Probe` runs it through `ssh … sh -lc '…'` for a remote node, or uses `exec.LookPath` directly on this computer. The login shell matters: a non-interactive ssh command gets a minimal `PATH` that does not include `~/.local/bin` or a node version manager's directory, which is where agents live.

Program names reach a remote shell, so they are restricted to plain names and paths, both for built-in harnesses and for ones from the config file; a custom `command` may contain only plain arguments. Lines of the probe's output that are not `home=` or `have=` are ignored, because a login shell may print a greeting.

The remote home directory from the probe is the default working directory for sessions when the persona has no `cwd`.

## A connection

1. `persona.Node.Exec` builds the process: the harness's ACP command itself on this computer, or `ssh -T -oBatchMode=yes -oConnectTimeout=10 -- ALIAS sh -lc 'exec hermes acp'` for a remote one.
2. `acp.Start` runs it with pipes on stdin and stdout, keeps the last lines of stderr, and puts the process in its own process group (see below).
3. `initialize` exchanges versions and capabilities. Then `session/new` starts a conversation, or `session/list` plus `session/load` reopens one; the agent replays the history, which geminus prints.
4. Each message is one `session/prompt`. While it is in flight the agent sends `session/update` notifications (reply text, reasoning, tool calls) and may send `session/request_permission` requests, which geminus answers from the keyboard.
5. Closing stdin ends the agent. If it does not exit within three seconds it is killed.

### Why the process group matters

A terminal delivers Ctrl-C to every process in the foreground process group. If ssh shared geminus's group, interrupting a reply would kill the connection. `acp.Start` starts the child in a new group (`Setpgid` on Unix, `CREATE_NEW_PROCESS_GROUP` on Windows), so Ctrl-C reaches only geminus, which turns it into a `session/cancel` message. The agent then ends the turn with the stop reason `cancelled` and the session stays usable.

`acp.Client.Prompt` registers the cancel only after the prompt request has been written, so a cancel can never arrive ahead of the turn it is meant to stop.

### Errors

When the agent's output ends, every pending call fails with `acp.ErrClosed` carrying the notable tail of the process's stderr (at most 40 lines of 1 KiB are kept, however much the process writes): for example `sh: 1: exec: hermes: not found`, or ssh's own `Permission denied (publickey)`. Agents log their startup at INFO level on stderr; those lines are dropped when anything else was written. The interface prints the error, switches to the disconnected prompt, and remembers the session so that `/resume` reconnects to it.

An error reply from the harness (`acp.RPCError`: no credit, an unsupported account, an unknown model) is shown in the harness's own words and does not end the connection.

Setting `GEMINUS_TRACE` to a file path makes the client log every protocol line in both directions, truncated to 4000 bytes each. This is the tool for a harness that connects but behaves unexpectedly.

Hermes answers `session/load` for an unknown ID with a null result, which the protocol also uses for success. geminus therefore resolves the ID against `session/list` first whenever the agent can list sessions.

## What goes on the screen

Agent text is printed as it arrives. Nothing from outside can control the terminal: `chat.Printer` strips control characters and bidirectional overrides from everything it prints (replies, error messages, the remote stderr tail, agent names, session titles and IDs, persona names), keeping only the few color codes geminus writes itself. Values shown inside a line are also flattened to one line so they cannot imitate a status line, and Honcho peers whose IDs are not plain names are not offered as personas. Reasoning is reduced to one `… thinking` line. A tool call is one dim line with its title; a failed one gets a second line. `internal/chat.Printer` tracks whether the cursor is in the middle of a line so that these side notes and the status line always start on a line of their own.

Input is read line by line in the terminal's normal mode, so the terminal's own line editing and input methods work. When stdin is not a terminal, each line is echoed, which keeps piped runs and test output readable. Color is used only on a terminal and is turned off by `NO_COLOR`.

## Tests

| Level | What runs | What it shows |
|---|---|---|
| `internal/acp` | The client against the scripted agent, in process and as a child process | Handshake, streaming, permissions, cancel, load and list, stderr in errors, process group |
| `internal/harness` | The registry and the probe script in a real shell | What is offered for a given set of installed programs, custom harnesses, rejection of shell syntax |
| `internal/persona` | `Load` and the config writers against a stub Honcho server and temporary profiles | Filtering to agents, locality, nodes, defaults, fallback when Honcho is down, invalid config rejected, unknown config keys preserved |
| `internal/chat` | The interface against in-process scripted agents | Picking persona and harness, defaults, every command, harness commands, the prompts and status lines, disconnect and reconnect, interrupt, output sanitizing |
| `test` | The built binary with a fake `ssh`, the scripted agent and a stub Honcho | The pieces together, including the exact ssh command lines of the probe and of the connection, saved defaults across runs, and continuing a session from a second process |

The scripted agent proves the client and the interface. It does not prove that a real agent behaves the same way; that was checked by hand against Hermes and Grok (see the README's harness table) and has not been checked for the bridged harnesses or on Windows.
