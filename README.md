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
| geminus-latest-channel | Also this repository (`cmd/geminus-latest-channel`): the small server on sibyl that builds, signs and hands out geminus releases. See [Updating](#updating). |

geminus and geminish are separate products that share a Honcho workspace. geminish's agent-to-agent mail and geminus's conversations are told apart by their metadata and never mix. `docs/protocol.md` is the contract between the two.

## Install

Go 1.25 or later.

```sh
go build -o geminus ./cmd/geminus
```

`bash scripts/crossbuild.sh` builds darwin, linux and windows binaries for amd64 and arm64 into `dist/`.

geminus needs nothing set up. It reads two files that a computer with Hermes and geminish already has, and writes neither: `~/.hermes/honcho.json` (the Honcho connection and who you are) and `~/.hermes/geminise.json` (the group's members and signing secret). `HERMES_HOME` moves `~/.hermes`. On Windows, without `HERMES_HOME`, the two files are read from `%LOCALAPPDATA%\hermes`, where Hermes keeps its profile on Windows, and from `%USERPROFILE%\.hermes` when that directory has no `honcho.json`.

A computer can be talked to once geminish 0.5 or later runs there. Setting a model or an effort for it needs geminish 0.6 or later there.

## Updating

A computer gets geminus by hand once. After that it updates itself:

```sh
geminus update        # geminus 0.4.0 → 0.5.0, or: already the latest (0.5.0)
```

Inside geminus, `/update` does the same and then the new program takes over the open conversation, so nothing is lost and nothing has to be reopened. When a newer release exists, the status under the box says `update 0.5.0 · /update`; geminus never installs one unasked.

Releases come from `https://sibyl.haunted.fail/geminus/updates` (`GEMINUS_UPDATES_URL` names another address). Every request is signed with the group secret, so only the group's computers can read them. A release is built on sibyl from the tag of its version and signed there with a key that exists on sibyl only; the group secret cannot sign one. geminus replaces itself only with a file whose size and SHA-256 that signature covers, and only with a version greater than its own. The program is swapped by a rename beside it, so its directory must be writable; a failed download leaves the old program as it was.

Releasing one, on a computer of the group with the repository:

```sh
bash scripts/release.sh 0.5.0     # tests, tags v0.5.0, pushes the tag, has sibyl build and sign it
```

Nothing is built or uploaded from that computer. A release is whatever the tag holds, so the right to push a tag to the repository is the right to release.

`deploy/README.md` says how the server was put on sibyl.

## Personas and harnesses

A **persona** is a Honcho peer that the group lists as an agent. One must be selected before anything else happens. The persona of the computer you are sitting at is tagged `THIS COMPUTER`, every other one `REMOTE`; the tag stays on screen so a local conversation is never mistaken for a remote one.

A **harness** is an agent program on the persona's computer. The list is not fixed anywhere: each bridge looks at its own computer and publishes what it finds, and geminus shows exactly that, with how long ago the bridge reported. If the computer has one harness, or a default applies, geminus does not ask.

The bridge finds and starts programs in the environment of that computer's owner (their interactive login shell), so the copy that runs is the one they would get by typing the name there.

| Harness | Run by the bridge as | Checked | `/model`, `/effort` checked |
|---|---|---|---|
| `hermes` | `hermes chat -Q --continue <conversation>` | Two-turn conversations through Honcho, also to a second computer | Effort set and cleared; Hermes's own configuration unchanged afterwards. Model: only that Hermes accepts the line |
| `claude` | `claude -p --session-id` / `--resume` | Two-turn conversations through Honcho; its own `/context` sent through geminus | Model and effort |
| `codex` | `codex exec --json` / `codex exec resume` | Two-turn conversations through Honcho | Effort |
| `grok` | `grok -p … --output-format json` / `-r` | Two-turn conversations through Honcho | Effort |
| `pi` | `pi -p --session-id` | Not checked: no API key was configured where it was tried | Not checked |
| `opencode` | `opencode run -s` | Not checked: the account had no credit where it was tried | Not checked |

Every harness is driven through its own non-interactive, session-continuing mode. Nothing has to be installed beside it, and the bridge passes no option that loosens the program's permissions unless that computer's `geminise.json` adds one.

## Conversations

A conversation is one Honcho session between you and the persona, bound to one harness and one directory on that computer. It is created by your first prompt.

**Answers are asynchronous.** Sending returns as soon as Honcho has the prompt, and the box takes the next prompt at once; answers scroll above it as their pieces arrive. Leaving geminus does not stop an agent: the answer is there when you reopen the conversation, from this computer or another. If the persona's computer is off, the prompt waits in Honcho and runs when its bridge is back (within seven days).

geminus says what actually happened: that the prompt is in Honcho, whether the doorbell rang, when the agent started working, and, if nothing picks the prompt up, that it has not been picked up and when the bridge was last seen.

## The screen

```
◆ geminus 0.4.0-dev · you · Honcho workspace home

❯ fix the failing test in the parser

 ⠹ Claude Code on mirai is working… 12s
╭─ mirai · Claude Code ──────────────────────────────────────── REMOTE ─╮
│ ❯ and then run the whole suite                                        │
╰───────────────────────────────────────────────────────────────────────╯
  ● bridge seen 15s ago · in /home/me/project · model opus · effort high
  geminus-mirai-20261119-4f2a9c
  / commands · Enter send · Alt+Enter new line · ↑ earlier prompts · Ctrl-C twi…
```

The prompt box stays at the bottom. Its top edge names the persona and the harness, with `REMOTE` or `THIS COMPUTER`; the edge is yellow for this computer and red when nothing can be sent. Under it is the connection (when the bridge last reported, or that Honcho cannot be reached), the directory on that computer the conversation runs in, its model and effort, and which conversation it is; what does not fit on one line goes to a second. The directory is always there: the one you chose, or the bridge's own marked `(default)` (a bridge older than geminish 0.7 does not say which that is, and the line then reads `default directory`). The line above it follows the prompt that is waiting: in Honcho, picked up, working, with the time so far.

| Key | |
|---|---|
| Enter | Send. |
| Alt+Enter, Shift+Enter, Ctrl-J, or `\` before Enter | New line. Pasted text keeps its line breaks. |
| Tab | Complete a geminus command; typing `/` lists them. |
| ↑ ↓ | Move between lines; from the first or last line, earlier and later prompts of this run. |
| Ctrl-A Ctrl-E Ctrl-U Ctrl-K Ctrl-W, Alt+← → | Line editing. |
| Ctrl-C | Clear the box; cancel a question; on an empty box, twice to quit. Ctrl-D on an empty box quits. |

When geminus is not on a terminal (piped input, `TERM=dumb`) it works line by line instead: one prompt per line, and it waits for each answer.

## Commands

Inside geminus, a line starting with `/` is a command.

| Command | Effect |
|---|---|
| `/persona [NAME]` | Pick who to talk to. |
| `/harness [ID]` | Pick the agent program there. Starts a new conversation. |
| `/cwd [DIR]` | Show or choose the directory on that computer for the conversation the next prompt starts. `~` and relative paths are read there, relative to the bridge's default directory (geminish 0.7 or later; an older bridge takes a full path only). `/cwd default` goes back to the bridge's own. Once a conversation has started it stays in its directory. |
| `!cd [DIR]`, `!pwd` | Move from the directory chosen so far, the way a shell on that computer would: `!cd projekt`, `!cd ..`, `!cd ~/src`, `!cd -` for the one before, `!cd` alone for the bridge's own. `!pwd` shows it, and so does the line under the prompt box. Works until the conversation starts, like `/cwd`. geminus works the path out from its text (`/` and `..` anywhere, `\` and drive letters on a Windows computer); the bridge says whether it exists when the first prompt arrives. Any other line starting with `!` goes to the agent as typed. |
| `/new [DIR]` | Start a new conversation, optionally in directory DIR on that computer. |
| `/sessions` | List your conversations with this persona, most recently used first. |
| `/resume [ID\|N]` | Reopen a conversation and show it so far: the latest, a number from `/sessions`, an ID, or the unique end of one. |
| `/model [NAME]` | Model for this conversation. Running `/model` alone opens an interactive searchable model selector on a terminal; `/model default` goes back to the agent program's own. |
| `/effort [LEVEL]` | Effort for this conversation. `/effort default` goes back. |
| `/status` | Show persona, harness, bridge and conversation. |
| `/update` | Install the latest release of geminus and carry on in this conversation with it. |
| `/default …` | Show or change your defaults (below). |
| `/help`, `/quit` | |

**Every other slash line goes to the agent as typed**, so the agent program's own slash commands work through geminus (`/compact`, `/context`, a skill). To send one that geminus also has, double the slash: `//help` sends `/help`. While a slash line is being typed, the line under the box says which of the two it is.

### Model and effort

`/model` and `/effort` belong to one conversation. The value travels with each prompt, inside what you sign, and the bridge gives it to the agent program for that conversation only: as an option of the run (`claude --model … --effort …`, `codex exec -m … -c model_reasoning_effort="…"`, `grok -m … --reasoning-effort …`), or, for Hermes, with Hermes's own `/model …` and `/reasoning …` inside the session. Nothing is written to the program's configuration: its default, and every other session of it, stay as they are. A new conversation starts with the program's own settings again, and a reopened one gets back what was set for it.

geminus does not know which models or levels a program has. It passes the name through, and the program's own error comes back if it does not accept it. A program that has no such option (the list comes from the bridge) is refused before anything is sent.

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
geminus NAME [HARNESS]   talk to NAME (optionally with HARNESS)
geminus persona          list personas and exit
geminus persona NAME     talk to NAME
  --harness ID           use this harness
  --cwd DIR              directory there for the new conversation
  --continue, -c         reopen the latest conversation
  --resume ID            reopen a conversation by ID
geminus update           install the latest release of geminus
geminus version | help
```

`geminus NAME [HARNESS]` (and `geminus persona NAME`) either opens the conversation or exits with status 1 and the reason. Bad usage exits with 2.

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
- **Windows is compile-checked only**, for geminus and for the bridge. That includes `/update` there: moving the running program aside and handing the console to the new one have not been run on Windows.
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

`bash scripts/release.sh X.Y.Z` makes a release (see [Updating](#updating)).

Unit tests use an in-memory Honcho (`internal/honcho/honchotest`) and never call a model. `test/` runs the built binary against the real bridge (the geminish installed on this computer, or `GEMINISH_SRC`) with that in-memory Honcho between them; it is skipped where no geminish with the bridge is found.
