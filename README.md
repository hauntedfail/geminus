# geminus

geminus is a terminal client for talking to the agents on your other computers, through Honcho.

You pick a **persona** (an agent of your Honcho workspace) and a **harness** (which agent program to use on that persona's computer: Hermes, Claude Code, Codex, …) and type. geminus writes your prompt into Honcho exactly as typed. A bridge on the persona's computer hands it to the agent program there, and what the program prints shows up on your screen when it arrives.

```
$ geminus
Personas (Honcho workspace sibyl)
  1  chrome  THIS COMPUTER  hermes claude codex grok pi opencode  (bridge seen 8s ago)
  2  mirai   REMOTE         claude codex  (bridge seen 21s ago)
  3  sibyl   REMOTE         hermes  (bridge seen 40s ago)
persona ❯ mirai
Harnesses on mirai [REMOTE] right now
  1  claude  Claude Code
  2  codex   Codex
harness ❯ claude
● mirai [REMOTE]  Claude Code  bridge seen 21s ago  new conversation
mirai [REMOTE] · claude ❯ run the tests and fix what fails
recorded in Honcho for mirai · doorbell rung
mirai · Claude Code is working…
```

(The listing is an illustration; yours shows what your bridges report.)

Three properties define it:

- **Honcho is the only channel.** geminus and the bridge each make outgoing HTTPS requests to Honcho. Nothing listens on any computer: no ssh, no new port, no server of geminus's own.
- **geminus keeps no state.** No config file, no session store. Personas, what each computer offers, conversations, and your defaults all live in Honcho, so every computer you sit at shows the same thing.
- **It is a proxy screen.** The prompt reaches the agent program word for word, and the answer comes back word for word. The model, tools, sessions and history stay with the agent program on its computer; you can sit down there later and open the same conversation with the program's own resume command.

## Parts

| Part | What it is |
|---|---|
| geminus | This repository: the client. One static binary, no dependencies outside the Go standard library. |
| The bridge | A part of [geminish](docs/architecture.md#the-bridge) 0.5 or later, the resident poller that already runs on each computer. It picks prompts up from Honcho and runs them. |

geminus and geminish are separate products that share a Honcho workspace. geminish's agent-to-agent mail and geminus's conversations are told apart by their metadata and never mix. `docs/protocol.md` is the contract between the two.

## Install

Go 1.23 or later.

```sh
go build -o geminus ./cmd/geminus
```

`bash scripts/crossbuild.sh` builds darwin, linux and windows binaries for amd64 and arm64 into `dist/`.

geminus needs nothing set up. It reads two files that a computer with Hermes and geminish already has, and writes neither: `~/.hermes/honcho.json` (the Honcho connection and who you are) and `~/.hermes/geminise.json` (the group's members and signing secret). `HERMES_HOME` moves `~/.hermes`.

A computer can be talked to once geminish 0.5 or later runs there.

## Personas and harnesses

A **persona** is a Honcho peer that the group lists as an agent. One must be selected before anything else happens. The persona of the computer you are sitting at is tagged `THIS COMPUTER`, every other one `REMOTE`; the tag stays on screen so a local conversation is never mistaken for a remote one.

A **harness** is an agent program on the persona's computer. The list is not fixed anywhere: each bridge looks at its own computer and publishes what it finds, and geminus shows exactly that, with how long ago the bridge reported. If the computer has one harness, or a default applies, geminus does not ask.

The bridge finds and starts programs in the environment of that computer's owner (their interactive login shell), so the copy that runs is the one they would get by typing the name there.

| Harness | Run by the bridge as | Checked |
|---|---|---|
| `hermes` | `hermes chat -Q --continue <conversation>` | Two-turn conversations through Honcho |
| `claude` | `claude -p --session-id` / `--resume` | Two-turn conversations through Honcho |
| `codex` | `codex exec --json` / `codex exec resume` | Two-turn conversations through Honcho |
| `grok` | `grok -p … --output-format json` / `-r` | Two-turn conversations through Honcho |
| `pi` | `pi -p --session-id` | Not checked: no API key was configured where it was tried |
| `opencode` | `opencode run -s` | Not checked: the account had no credit where it was tried |

Every harness is driven through its own non-interactive, session-continuing mode. Nothing has to be installed beside it, and the bridge passes no option that loosens the program's permissions unless that computer's `geminise.json` adds one.

## Conversations

A conversation is one Honcho session between you and the persona, bound to one harness and one directory on that computer. It is created by your first prompt.

**Answers are asynchronous.** Sending returns as soon as Honcho has the prompt. geminus then shows the answer as pieces arrive. Ctrl-C stops watching, not the agent: the rest appears at the prompt when it arrives, and it is there when you reopen the conversation, from this computer or another. If the persona's computer is off, the prompt waits in Honcho and runs when its bridge is back (within seven days).

geminus says what actually happened: `recorded in Honcho`, whether the doorbell rang, when the agent started working, and, if nothing picks the prompt up, that it has not been picked up and when the bridge was last seen.

## Commands

Inside geminus, a line starting with `/` is a command.

| Command | Effect |
|---|---|
| `/persona [NAME]` | Pick who to talk to. |
| `/harness [ID]` | Pick the agent program there. Starts a new conversation. |
| `/new [DIR]` | Start a new conversation, optionally in directory DIR on that computer. |
| `/sessions` | List your conversations with this persona, most recently used first. |
| `/resume [ID\|N]` | Reopen a conversation and show it so far: the latest, a number from `/sessions`, an ID, or the unique end of one. |
| `/status` | Show persona, harness, bridge and conversation. |
| `/default …` | Show or change your defaults (below). |
| `/help`, `/quit` | |
| `//TEXT` | Send a line that starts with a slash to the agent (for the agent program's own slash commands). |

A single-slash line geminus does not know is reported and not sent, so a typo never reaches an agent.

At the prompt, Ctrl-C quits.

## Defaults

```
/default                       show your defaults
/default save                  start with the current persona, harness and directory from now on
/default persona NAME|none     who geminus talks to when started bare
/default harness ID|none       the harness on the current persona
/default cwd DIR|none          the directory on the current persona
```

Defaults are stored in your own peer in Honcho, not on disk. With a default persona, `geminus` starts talking at once; without one, it asks.

## Command line

```
geminus                  talk to the default persona, or pick one
geminus persona          list personas and exit
geminus persona NAME     talk to NAME
  --harness ID           use this harness
  --cwd DIR              directory there for the new conversation
  --continue, -c         reopen the latest conversation
  --resume ID            reopen a conversation by ID
geminus version | help
```

`geminus persona NAME` either opens the conversation or exits with status 1 and the reason. Bad usage exits with 2.

## Trust

Honcho has one workspace token and does not prove who wrote a message. So:

- Every prompt is signed with the group secret, bound to its conversation, persona, harness, directory and a one-time value. A bridge runs a prompt once, only if the signature verifies and the author is a human member. Someone holding only the Honcho token cannot make any computer run anything, and cannot replay or move a signed prompt.
- Every piece of output is signed by the bridge. geminus does not show a message that claims to be an agent's words, or yours, without the matching signature.
- Your defaults are signed too, and a conversation whose harness or directory was changed in Honcho after you signed its prompts is not reopened. Otherwise the token would be enough to redirect your next prompt.
- The group secret is the one already on your computers for geminish. Any computer that holds it can issue prompts as you; keeping the secret off a computer you do not trust that far is the control.

The wording of a prompt is never part of this. Nothing is added to what you type.

## Limits

- **Output arrives in pieces every couple of seconds**, not character by character, and a harness that prints only at the end (Grok, Hermes) shows its answer at once when it finishes.
- **A harness's own permission questions cannot be answered from geminus.** A program that would stop to ask in its interactive mode follows its own non-interactive behaviour instead.
- **Windows is compile-checked only**, for geminus and for the bridge.
- **Input is line-based.** There is no multi-line editor.
- **Replies are shown as plain text.** Control characters are removed, so a reply cannot restyle or clear the terminal. Markdown is not rendered.

## Develop

```sh
go build ./...
go vet ./...
go test ./... -count=1 -race
bash scripts/test-linux.sh        # the same tests in a Linux container
bash scripts/crossbuild.sh
```

Unit tests use an in-memory Honcho (`internal/honcho/honchotest`) and never call a model. `test/` runs the built binary against the real bridge (the geminish installed on this computer, or `GEMINISH_SRC`) with that in-memory Honcho between them; it is skipped where no geminish with the bridge is found.
