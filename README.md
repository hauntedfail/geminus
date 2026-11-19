# geminus

geminus is a terminal client for talking to your agents on other computers.

You pick a **persona** (an agent of your Honcho workspace) and a **harness** (which agent program to use on that persona's computer: Hermes, Claude Code, Codex, pi, …), and you talk. geminus is a proxy screen. The model, the tools, the sessions and their history all stay with the harness on the persona's computer; geminus relays and never reaches below the harness's own interface.

```
$ geminus
Personas (Honcho workspace sibyl)
  1  chrome  THIS COMPUTER  this computer
  2  mirai   REMOTE         no route yet
  3  sibyl   REMOTE         ssh btcvps
persona ❯ sibyl
looking at sibyl [REMOTE] (ssh btcvps)…
connecting to sibyl [REMOTE] · Hermes Agent (ssh btcvps)…
● connected  sibyl [REMOTE]  Hermes Agent (hermes-agent 0.23.0)  via ssh btcvps  session local-8b1845fc…  cwd /home/hermes
sibyl [REMOTE] · hermes ❯ what are you working on?
```

Typing `geminus` is enough. Who and what to connect to is chosen inside geminus and remembered with `/default`; the command line carries almost nothing.

## Install

Go 1.23 or later. No dependencies outside the standard library.

```sh
go build -o geminus ./cmd/geminus
```

`bash scripts/crossbuild.sh` builds darwin, linux and windows binaries for amd64 and arm64 into `dist/`.

## Personas

A persona is a label taken from a Honcho peer. One must be selected before anything else happens: by default, by name, or from the list.

geminus reads the Honcho connection from the Hermes profile on this computer (`~/.hermes/honcho.json`) and lists the workspace's peers. The human peer is left out; if `~/.hermes/geminise.json` has a `members` map, only peers it lists as `agent` are shown. The profile's own agent is tagged `THIS COMPUTER`, every other persona `REMOTE`. Talking to this computer's own persona works, and the tag stays on screen so it is never mistaken for a remote one.

geminus reaches a persona's computer over ssh. The first time you pick a persona it has no route for, it asks for the ssh host alias once and remembers it. The alias must work without prompts (`ssh -oBatchMode=yes ALIAS true`). This computer's own persona needs no route.

## Harnesses

After the persona, geminus looks at that persona's computer and offers the agent programs that are **actually installed there right now**. Nothing in the list is fixed.

```
Harnesses on chrome [THIS COMPUTER]
  1  hermes    Hermes Agent
  2  opencode  OpenCode
  3  gemini    Gemini CLI
  4  grok      Grok
  -  claude    Claude Code: installed, but its ACP bridge is missing (npm install -g @zed-industries/claude-agent-acp)
  -  codex     Codex: installed, but its ACP bridge is missing (npm install -g @zed-industries/codex-acp)
  -  pi        pi: installed, but its ACP bridge is missing (npm install -g pi-acp)
harness ❯
```

If the computer has exactly one usable harness, or a default applies, geminus does not ask.

geminus talks to a harness through the [Agent Client Protocol](https://agentclientprotocol.com) (ACP), which the harness serves on its own. Any agent that speaks ACP can be a harness:

| ID | Harness | ACP server | Checked |
|---|---|---|---|
| `hermes` | Hermes Agent | `hermes acp` | Conversations over ssh (0.23.0 on Linux) and locally (0.22.5) |
| `grok` | Grok | `grok agent stdio` | Conversation locally |
| `opencode` | OpenCode | `opencode acp` | Connects and opens a session locally; no conversation was held |
| `gemini` | Gemini CLI | `gemini --acp` | Handshake only; the account used could not open a session |
| `claude` | Claude Code | `claude-agent-acp` (bridge) | Not checked; the bridge was not installed |
| `codex` | Codex | `codex-acp` (bridge) | Not checked; the bridge was not installed |
| `pi` | pi | `pi-acp` (bridge) | Not checked; the bridge was not installed |

Claude Code, Codex and pi do not serve ACP themselves; each needs its bridge program installed next to it. geminus never installs anything. When the agent is there but the bridge is not, the list says so and shows the command.

Add any other ACP agent in the config file:

```json
{"harnesses": {"myagent": {"name": "My Agent", "command": "myagent acp"}}}
```

`command` starts the ACP server; `cli` optionally names the agent's own program when a separate bridge is needed, and `install` is the hint shown when the bridge is missing. A custom harness with a built-in ID replaces it.

## Commands

Inside geminus, lines starting with `/` are commands.

| Command | Effect |
|---|---|
| `/persona [NAME]` | Pick who to talk to (from the list, or by name). |
| `/harness [ID]` | Pick the agent program on that persona's computer. geminus looks at the computer again each time. |
| `/new` | Start a new session. |
| `/sessions` | List the sessions the harness has there. |
| `/resume [ID\|N]` | Reopen a session: the latest, a number from `/sessions`, or any unique start of an ID. After a lost connection it reconnects to the session that was open. |
| `/status` | Show persona, harness, connection, session and directory. |
| `/default …` | Show or change the defaults (below). |
| `/disconnect` | Close the connection. |
| `/help` | List these commands and the harness's own. |
| `/quit` | Leave. |

**The harness's own slash commands work too.** Hermes's `/model`, for example, is not a geminus command, so it is sent to the agent as typed. `/help` lists what the connected harness announced. For a harness command that shares a name with a geminus one, type two slashes: `//help`. A slash command that neither geminus nor the harness knows is reported and not sent.

**Ctrl-C** during a reply stops that turn and keeps the connection. At the prompt it quits.

**Tool permissions.** When the agent asks to run a tool, geminus shows the request and waits. Only `y` (once) or `a` (always) allows; anything else, including an empty line, rejects.

## Defaults

```
/default                       show the defaults
/default save                  start with the current persona and harness from now on
/default persona NAME|none     the persona geminus connects to when started bare
/default harness ID|none       the harness to prefer on any persona
/default session new|continue  start a new session, or reopen the latest
```

`/default save` stores the harness with the persona, because another persona's computer may not have it. A persona's own harness wins over the general one, and a default that the computer does not have is ignored rather than forced.

With a default persona set, `geminus` connects straight away. Without one, it asks.

## Command line

```
geminus                  connect to the default persona, or pick one
geminus persona          list personas and exit
geminus persona NAME     connect to NAME
  --harness ID           use this harness instead of the default
  --continue, -c         reopen the latest session
  --resume ID            reopen a session by ID
geminus version | help
```

`geminus persona NAME` either opens the conversation or exits with status 1 and the reason. Bad usage exits with 2.

## Configuration file

Everything above is stored in `~/.geminus/config.json`. You never have to edit it, but you can:

```json
{
  "defaults": {"persona": "sibyl", "harness": "hermes", "session": "continue"},
  "personas": {
    "sibyl": {"ssh": "btcvps", "cwd": "/home/hermes", "harness": "hermes"}
  },
  "harnesses": {}
}
```

| Persona field | Meaning |
|---|---|
| `ssh` | ssh host alias. Leave out for this computer. |
| `cwd` | Directory sessions run in. Default: the home directory there, or the current directory on this computer. |
| `harness` | The harness to use on this persona. |

`GEMINUS_HOME` moves `~/.geminus`; `HERMES_HOME` moves `~/.hermes`. To read Honcho from somewhere other than the Hermes profile, add `{"honcho": {"baseUrl": "…", "workspace": "…", "apiKeyEnv": "HONCHO_API_KEY"}}`.

## When something goes wrong

- **The connection drops.** geminus says so, shows the last error output of the harness or of ssh, and goes back to `geminus (disconnected) ❯`. A message that was in flight is never sent again. Text typed while disconnected is not sent anywhere.
- **The harness refuses a message** (no credit, unsupported account, unknown model). geminus shows the harness's own words and stays connected.
- **A harness misbehaves.** `GEMINUS_TRACE=/path/to/file geminus` records every protocol line in both directions. The file contains the conversation.

## Limits

- **A computer this one cannot ssh into cannot be reached.** geminus does not relay through Honcho or any other server.
- **Remote computers need a POSIX shell**: geminus runs `sh -lc` there to find and start harnesses. Windows is compile-checked only, both as the local and as the remote side.
- **Input is line-based.** There is no multi-line editor.
- **Replies are shown as plain text.** Control characters are removed, so a reply cannot restyle or clear the terminal. Markdown is not rendered.
- **The agent's reasoning is not shown**, only that it is thinking.

## Develop

```sh
go build ./...
go vet ./...
go test ./... -count=1 -race
bash scripts/test-linux.sh        # the same tests in a Linux container
bash scripts/crossbuild.sh
```

Tests use a scripted ACP agent (`internal/acp/acptest`) and never call a model. The end-to-end tests in `test/` run the built binary against that agent, a fake `ssh`, and a stub Honcho server. `docs/architecture.md` describes the design.
