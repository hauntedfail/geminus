# geminus protocol

How geminus (the client) and the bridge (a part of geminish that runs on each computer) talk. Everything goes through Honcho. Neither side listens on a port, and geminus keeps no state of its own.

geminus and geminish are separate products that share a Honcho workspace. A record belongs to geminus when its metadata says so (below); geminish's mail (`kind: geminise` sessions, `geminise` message metadata) is never read as a geminus record, and the reverse.

## Where things live

| Thing | Honcho object | Written by |
|---|---|---|
| Personas | Workspace peers that the local `geminise.json` lists as `agent` | (existing) |
| What a computer can run | That peer's metadata, key `geminus_node` | The bridge there |
| A conversation | One session, metadata `kind: geminus` | geminus creates it; both write messages |
| What the person said | A message by the human peer; the content is the prompt, verbatim | geminus |
| What the agent answered | Messages by the persona peer; the content is the output, verbatim | The bridge |
| How far a prompt got | The prompt message's metadata, field `state` | The bridge |
| The person's defaults | The human peer's metadata, key `geminus` | geminus |

geminus reads two existing files and writes neither: the Hermes profile's `honcho.json` (Honcho address, workspace, token, this computer's agent, the human's peer name) and `geminise.json` (members and the signing secret).

## Signatures

Honcho has one workspace token and does not prove who wrote a message, so every record that causes an action or is shown as an agent's words is signed with the secret in `geminise.json`:

```
sig = hex( HMAC-SHA256( secret, SHA256(content) || SHA256(label) || decimal(ts) ) )
```

`ts` is the Unix time of signing. The label names the kind of record and what it is bound to, so a signature made for one purpose never verifies for another. No geminish record uses a label that starts with `geminus-`.

| Record | content | label |
|---|---|---|
| Prompt | the message content | `geminus-prompt:<session>:<persona>:<harness>:<human>:<nonce>` |
| Output | the message content | `geminus-output:<session>:<turn>:<seq>:<final 0 or 1>:<ok 0 or 1>` |
| Node card | harness IDs joined with `,` | `geminus-node:<peer>` |

## Session

```
id        geminus-<persona>-<YYYYMMDD>-<6 hex>
peers     the human and the persona, neither observed (a conversation is not memory)
metadata  {"kind": "geminus",
           "geminus": {"v": 1, "human": "h1karu", "persona": "mirai", "harness": "claude",
                       "cwd": "/home/me/project", "title": "first words of the first prompt",
                       "pending": true, "updated": 1795083503}}
```

`harness` and `cwd` are fixed when the session is created: a conversation belongs to one agent program in one directory. An empty `cwd` means the bridge's default directory.

`updated` is when the last prompt was written; geminus sorts conversations by it, because Honcho records only when a session was created.

`pending` is how the bridge finds work with one request: it lists sessions with `kind: geminus`, its own `persona` and `pending: true`. geminus sets it after writing a prompt. The bridge clears it first and reads the messages afterwards, so a prompt written in between is still seen.

## Prompt

A message with `peer_id` = the human and the prompt as its content, nothing added.

```
metadata  {"geminus": {"v": 1, "role": "prompt", "nonce": "<uuid>", "ts": 1795083503, "sig": "…",
                       "state": "queued"}}
```

The bridge runs a prompt once, when all of these hold: the session is its own; the author is a member listed as `human`; the signature verifies; `ts` is at most 7 days old and not more than 5 minutes ahead; and it has not handled that `nonce` before (it remembers handled nonces locally). It then moves `state` through `running` to `done`, `failed` or `refused`, adding `detail` when there is something to say. Anything else is left alone.

Prompts of one session run one at a time, in the order Honcho stored them.

## Output

Messages with `peer_id` = the persona. The content is what the agent program printed, cut into pieces of at most 6,000 characters.

```
metadata  {"geminus": {"v": 1, "role": "output", "turn": "<id of the prompt message>", "seq": 1,
                       "final": false, "ok": true, "native": "<the agent program's own session id>",
                       "ts": 1795083520, "sig": "…"}}
```

`seq` counts from 1 within a turn. The last piece has `final: true`; `ok: false` on it means the turn failed and the content says why. Pieces are written as output arrives, at most every two seconds. geminus shows a piece only if its signature verifies.

`native` is the session ID inside the agent program. The bridge reads it back from the newest output of the session to continue the same conversation there, so it keeps no table of its own; a person at that computer can open the same conversation with the program's own resume command.

## Node card

```
peer metadata  {"geminus_node": {"v": 1, "harnesses": [{"id": "claude", "name": "Claude Code"}, …],
                                 "ts": 1795083000, "sig": "…"}}
```

The bridge publishes it when it starts and every five minutes, listing the agent programs found on that computer's `PATH` at that moment. geminus offers exactly these, and shows the card's age as "last seen".

## Defaults

```
human peer metadata  {"geminus": {"v": 1, "persona": "mirai",
                                  "personas": {"mirai": {"harness": "claude", "cwd": "/home/me/project"}}}}
```

Not signed: it only preselects what geminus offers, and every prompt is signed on its own.

## Doorbell

After writing a prompt, geminus rings the persona's geminish doorbell: an empty HTTPS POST to the Mozilla push endpoint the persona published and signed in its peer metadata (`geminise_push`). The ring carries nothing. The bridge reads Honcho when it rings, and on its timer otherwise.

## Running a prompt

The bridge hands the prompt to the agent program's own non-interactive, session-continuing mode and passes no option that loosens permissions unless the computer's `geminise.json` says so (`"geminus": {"args": {"claude": ["--permission-mode", "acceptEdits"]}}`). `"geminus": false` there turns the bridge off; `"geminus": {"cwd": …, "timeout": …}` set the default directory and how long one prompt may run (default one hour).

Programs are found and started in the environment of the computer's owner, read from their interactive login shell, so the copy that runs is the one the owner gets by typing its name.

| Harness | First prompt | Later prompts | Output read as |
|---|---|---|---|
| `hermes` | `hermes chat -Q --continue <session> --create-if-missing --source tool --query-file -` | same | text |
| `claude` | `claude -p --session-id <uuid>` (prompt on stdin) | `claude -p --resume <uuid>` | text |
| `codex` | `codex exec --json --skip-git-repo-check -` | `codex exec resume <id> --json --skip-git-repo-check -` | JSON lines: `thread.started`, `agent_message` |
| `grok` | `grok -p <prompt> --output-format json` | `… -r <id>` | one JSON object: `text`, `sessionId` |
| `pi` | `pi -p --session-id <session> <prompt>` | same | text |
| `opencode` | `opencode run -s <session> <prompt>` | same | text |

For `claude` the UUID is derived from the Honcho session ID, so it is the same on every turn without being stored.
