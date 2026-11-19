# geminus architecture

This document describes how the code works today.

## What geminus is for

geminus lets one person talk directly to an agent that runs on another computer. Launching an agent that is already on this computer is not the point: the agent's own app does that. So geminus starts disconnected, shows who can be reached, and makes it visible at all times which persona it is connected to and whether that persona is local or remote.

An earlier version (commit `d8ec09f`) wrapped the local `claude` and `hermes` command lines in a pseudo-terminal. That design answered the wrong question and was removed. `plans/geminus/plan.md` is its record.

## Shape

```
  terminal ── geminus ── ssh ─────────────── agent's ACP server (hermes acp)
                 │        one process per     on the persona's machine;
                 │        connection          owns sessions, tools, history
                 │
                 ├── Honcho: POST /v3/workspaces/<ws>/peers/list   (who exists)
                 └── ~/.geminus/config.json                        (how to reach them)
```

| Package | Responsibility |
|---|---|
| `cmd/geminus` | `main` only. |
| `internal/chat` | The terminal interface: command line, prompt, status line, slash commands, turn display, permission prompts. |
| `internal/persona` | The persona directory (Honcho peers plus configured routes) and the command that reaches a route. |
| `internal/acp` | An Agent Client Protocol client over a child process's stdin and stdout. |
| `internal/acp/acptest` | A scripted ACP agent for tests. |
| `internal/version` | The version string. |

There are no third-party dependencies.

## Decisions

**Talk ACP to the agent's own server.** Hermes ships `hermes acp`; Claude Code has an ACP adapter. ACP gives structured turns, streaming, tool calls, permission requests, cancellation and session listing. geminus therefore never scrapes a terminal UI and never starts a process per turn. One agent process serves the whole connection, across `/new` and `/resume`.

**Reach remote machines with ssh.** ssh already provides authentication, encryption and host verification that the person has set up, so geminus adds no credentials and no listener of its own. The route names an ssh host alias; everything else about the connection is ssh configuration. geminus runs ssh with `BatchMode=yes` so that a missing key fails with an error instead of a password prompt fighting the conversation for the terminal.

**Honcho says who, the config file says where.** The workspace's peers are the personas. Honcho knows nothing about hosts, and guessing a host from a persona's name would be a way to talk to the wrong machine, so routes are written down explicitly in `~/.geminus/config.json`. The one exception is this computer's own persona, which is known from the Hermes profile and needs no route. If Honcho cannot be read, geminus says so and offers only the personas it knows locally.

**The remote owns the session.** Sessions, their history and their IDs live with the agent. geminus stores nothing about a conversation. `/sessions`, `--continue` and `/resume` ask the agent (`session/list`, `session/load`), so the list is always the truth, and the same session can be picked up from any computer.

**State is always on screen.** The prompt is `sibyl [REMOTE] ❯` when connected and `geminus (disconnected) ❯` when not. The status line (shown on every change and by `/status`) adds the agent, the route, the session and the directory. The local persona always carries `[THIS COMPUTER]`.

**Nothing is sent by accident.** Text typed while disconnected is not sent. A message in flight when the connection drops is not sent again; the person is told it may not have arrived. A tool call runs only after an explicit `y` or `a`.

## A connection

1. `persona.Route.Exec` builds the process: the agent command itself for a local route, or `ssh -T -oBatchMode=yes -oConnectTimeout=10 -- ALIAS sh -lc 'exec hermes acp'` for a remote one. The login shell is there because a non-interactive ssh command gets a minimal `PATH`, which does not include `~/.local/bin` where agents are usually installed.
2. `acp.Start` runs it with pipes on stdin and stdout, keeps the last lines of stderr, and puts the process in its own process group (see below).
3. `initialize` exchanges versions and capabilities. Then `session/new` starts a conversation, or `session/list` plus `session/load` reopens one; the agent replays the history, which geminus prints.
4. Each message is one `session/prompt`. While it is in flight the agent sends `session/update` notifications (reply text, reasoning, tool calls) and may send `session/request_permission` requests, which geminus answers from the keyboard.
5. Closing stdin ends the agent. If it does not exit within three seconds it is killed.

### Why the process group matters

A terminal delivers Ctrl-C to every process in the foreground process group. If ssh shared geminus's group, interrupting a reply would kill the connection. `acp.Start` starts the child in a new group (`Setpgid` on Unix, `CREATE_NEW_PROCESS_GROUP` on Windows), so Ctrl-C reaches only geminus, which turns it into a `session/cancel` message. The agent then ends the turn with the stop reason `cancelled` and the session stays usable.

`acp.Client.Prompt` registers the cancel only after the prompt request has been written, so a cancel can never arrive ahead of the turn it is meant to stop.

### Errors

When the agent's output ends, every pending call fails with `acp.ErrClosed` carrying the notable tail of the process's stderr (at most 40 lines of 1 KiB are kept, however much the process writes): for example `sh: 1: exec: hermes: not found`, or ssh's own `Permission denied (publickey)`. Agents log their startup at INFO level on stderr; those lines are dropped when anything else was written. The interface prints the error, switches to the disconnected prompt, and remembers the session so that `/resume` reconnects to it.

An error reply from the agent (`acp.RPCError`) is shown with the agent's message and does not end the connection.

Hermes answers `session/load` for an unknown ID with a null result, which the protocol also uses for success. geminus therefore resolves the ID against `session/list` first whenever the agent can list sessions.

## What goes on the screen

Agent text is printed as it arrives. Nothing from outside can control the terminal: `chat.Printer` strips control characters and bidirectional overrides from everything it prints (replies, error messages, the remote stderr tail, agent names, session titles and IDs, persona names), keeping only the few color codes geminus writes itself. Values shown inside a line are also flattened to one line so they cannot imitate a status line, and Honcho peers whose IDs are not plain names are not offered as personas. Reasoning is reduced to one `… thinking` line. A tool call is one dim line with its title; a failed one gets a second line. `internal/chat.Printer` tracks whether the cursor is in the middle of a line so that these side notes and the status line always start on a line of their own.

Input is read line by line in the terminal's normal mode, so the terminal's own line editing and input methods work. When stdin is not a terminal, each line is echoed, which keeps piped runs and test output readable. Color is used only on a terminal and is turned off by `NO_COLOR`.

## Tests

| Level | What runs | What it shows |
|---|---|---|
| `internal/acp` | The client against the scripted agent, in process and as a child process | Handshake, streaming, permissions, cancel, load and list, stderr in errors, process group |
| `internal/persona` | `Load` against a stub Honcho server and temporary profiles | Filtering to agents, locality, routes, fallback when Honcho is down, invalid routes rejected |
| `internal/chat` | The interface against in-process scripted agents | Every command, the prompts and status lines, disconnect and reconnect, interrupt, output sanitizing |
| `test` | The built binary with a fake `ssh`, the scripted agent and a stub Honcho | The pieces together, including the exact ssh command line and continuing a session from a second process |

The scripted agent proves the client and the interface. It does not prove that a real agent behaves the same way; that was checked by hand against Hermes (see the README's runtime table) and has not been checked for Claude Code or on Windows.
