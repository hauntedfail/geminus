# geminus

geminus is a terminal client for talking to your agents on other computers.

You pick a persona (an agent in your Honcho workspace), geminus connects to that agent where it runs, and you talk to it directly. The persona and the connection state stay on screen the whole time.

```
$ geminus
Personas (Honcho workspace sibyl)
  1  chrome  THIS COMPUTER  hermes (local)
  2  mirai   REMOTE         no route: add "mirai" to /Users/you/.geminus/config.json
  3  sibyl   REMOTE         hermes via ssh btcvps
Type a persona's name or number to connect. /help lists commands.
geminus (disconnected) ❯ sibyl
connecting to sibyl [REMOTE] (hermes via ssh btcvps)…
● connected  sibyl [REMOTE]  hermes-agent 0.23.0 via ssh btcvps  session local-b32e1240…  cwd /home/hermes
sibyl [REMOTE] ❯ what are you working on?
  … thinking
  ⚙ terminal: git status
...
```

geminus does not run a model and does not wrap an agent's command-line interface. It speaks the [Agent Client Protocol](https://agentclientprotocol.com) (ACP) to the agent's own ACP server, over one ssh connection that stays open for the whole conversation. There is no daemon and no server of its own.

## Install

Go 1.23 or later.

```sh
go build -o geminus ./cmd/geminus
```

`bash scripts/crossbuild.sh` builds darwin, linux and windows binaries for amd64 and arm64 into `dist/`. geminus has no dependencies outside the Go standard library.

## Personas and routes

Two things decide what geminus can connect to.

**Who exists** comes from Honcho. geminus reads the connection from the Hermes profile on this computer (`~/.hermes/honcho.json`) and lists the workspace's peers. The human peer is left out. If `~/.hermes/geminise.json` has a `members` map, only peers it lists as `agent` are shown. The profile's own agent (`aiPeer`) is tagged `THIS COMPUTER`; every other persona is `REMOTE`.

**How to reach each one** comes from `~/.geminus/config.json`:

```json
{
  "personas": {
    "sibyl": {"ssh": "btcvps", "runtime": "hermes", "cwd": "/home/hermes"}
  }
}
```

| Field | Meaning |
|---|---|
| `ssh` | An ssh host alias from your ssh config. Leave it out for an agent on this computer. |
| `runtime` | `hermes` or `claude`. |
| `cwd` | The directory sessions run in on the agent's machine. Required with `ssh`; defaults to the current directory otherwise. |
| `command` | Replaces the command that starts the agent's ACP server. |

This computer's own persona needs no entry: geminus runs `hermes acp` locally. A persona without a route is listed, and connecting to it fails with the entry to add.

The ssh alias must work without prompts (`ssh -oBatchMode=yes ALIAS true`): key authentication and a known host key. geminus runs `ssh -T ALIAS sh -lc 'exec hermes acp'`, through a login shell so the agent is found on the remote `PATH`. Set `command` when the remote has no POSIX shell or the agent lives elsewhere.

`GEMINUS_HOME` moves `~/.geminus`; `HERMES_HOME` moves `~/.hermes`. To read Honcho from somewhere other than the Hermes profile:

```json
{"honcho": {"baseUrl": "https://honcho.example", "workspace": "main", "apiKeyEnv": "HONCHO_API_KEY"}}
```

### Runtimes

| Runtime | Agent command | State |
|---|---|---|
| `hermes` | `hermes acp` | Verified from macOS against Hermes 0.23.0 on Linux over ssh, and against Hermes 0.22.5 locally. |
| `claude` | `claude-agent-acp` ([Zed's ACP adapter for Claude Code](https://github.com/zed-industries/claude-agent-acp)) | Not verified. The adapter must be installed on the agent's machine; geminus never installs anything remotely. |

## Use

```
geminus                          list personas, pick one, and talk
geminus persona                  list personas and exit
geminus persona NAME             connect to NAME and start a new session
geminus persona NAME --continue  connect and reopen NAME's latest session
geminus persona NAME --resume ID connect and reopen a session by ID
geminus version | help
```

`geminus persona NAME` either opens the conversation or exits with status 1 and the reason. Bad usage exits with 2.

While running:

| Command | Effect |
|---|---|
| `/persona [NAME]` | List personas, or switch to NAME (the current connection is closed first). |
| `/new` | Start a new session with the current persona. |
| `/sessions` | List the persona's sessions on its machine. |
| `/resume [ID\|N]` | Reopen a session: the latest, a number from `/sessions`, or any unique start of an ID. After a lost connection, `/resume` reconnects to the session that was open. |
| `/status` | Show the persona, agent, route, session and directory. |
| `/disconnect` | Close the connection. |
| `/quit` | Leave. |

**Ctrl-C** during a reply stops that turn and keeps the connection. At the prompt it quits.

**Tool permissions.** When the agent asks to run a tool, geminus shows the request and waits:

```
  ? sibyl asks to run: terminal: rm -rf build
    [y] allow once  [a] always allow  [n] reject (default n) ❯
```

Only `y` or `a` allows. Anything else, including an empty line, rejects.

**Sessions live on the agent's machine.** geminus keeps no transcript and no session list of its own; `/sessions` and `--continue` ask the agent. Reopening a session prints the conversation so far.

**When the connection drops**, geminus says so, shows the agent's or ssh's last error output, and goes back to `geminus (disconnected) ❯`. A message that was in flight is never sent again. Text typed while disconnected is not sent anywhere.

## Limits

- **mirai-style hosts need a route.** A machine behind NAT that this computer cannot ssh into cannot be reached. geminus does not relay through Honcho or any other server.
- **Claude Code is unverified**, as above.
- **Windows is compile-checked only.** It has not been run on a real machine.
- **Input is line-based.** There is no multi-line editor; paste multi-line text as one line or send it in parts.
- **Agent replies are shown as plain text.** Control characters are removed, so a reply cannot restyle or clear the terminal. Markdown is not rendered.
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
