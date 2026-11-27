# geminus protocol

How geminus (the client) and the bridge (a part of geminish that runs on each computer) talk. Everything goes through Honcho. Neither side listens on a port, and geminus keeps no state of its own.

One exchange is not with the bridge and not through Honcho: geminus fetching a newer release of itself. It is at the end, under [Releases](#releases).

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

### Asymmetric human signing (Ed25519)

To separate human prompt authority from the shared group secret (so a compromised peer computer cannot forge human prompts), the owner may generate an Ed25519 key on their own computer (`geminus keygen` writes `<HERMES_HOME>/geminus_human.key` mode `0600`) and distribute only the 64-hex public key in every bridge's `geminise.json`:

```json
{
  "human_pubkeys": {"h1karu": "<64-hex-ed25519-pubkey>"},
  "geminus": {"human_auth": "ed25519_only"}
}
```

When `<HERMES_HOME>/geminus_human.key` is present, geminus signs human-authored records (prompts, session descriptions, and defaults) with Ed25519 and sets `"sig_alg": "ed25519"`:

```
sig = hex( Ed25519.Sign( human_priv, "geminus-ed25519:" || SHA256(content) || SHA256(label) || decimal(ts) ) )
```

Bridges support `"human_auth": "dual"` (default during migration: verifies Ed25519 when `sig_alg == "ed25519"` and still accepts legacy HMAC) and `"ed25519_only"` (refuses all HMAC-signed prompts).

| Record | content | label |
|---|---|---|
| Prompt | the message content | `geminus-prompt:<session>:<persona>:<harness>:<human>:<nonce>:<H(cwd)>[:<H(model)>:<H(effort)>]` |
| Output | the message content | `geminus-output:<session>:<turn>:<seq>:<final 0 or 1>:<ok 0 or 1>:<H(native)>` |
| Node card | harness IDs joined with `,` | `geminus-node:<peer>` |
| Node card options | one entry per harness, joined with `,`; an entry is the ID and its options joined with `:` (`claude:model:effort,opencode:model`) | `geminus-node-options:<peer>` |
| Node card directory | the directory | `geminus-node-cwd:<peer>` |
| Session | the title | `geminus-session:<session>:<persona>:<harness>:<human>:<H(cwd)>[:<H(model)>:<H(effort)>]`, with `ts` = `updated` |
| Defaults | the defaults as JSON text | `geminus-defaults:<human>` |

`H(x)` is the hex SHA-256 of `x`, used for free-form values so that they cannot run into the fields beside them.

The part in brackets is present when a model or an effort is set, with both digests, and absent when neither is. A record without settings is therefore signed exactly as it was before settings existed (geminish 0.5), and geminus and bridges of different ages keep working together for such records. The two forms cannot be confused: adding a setting to a record signed without one, or removing the settings from one signed with them, changes the label, so the signature no longer verifies.

What a signature covers decides what cannot be changed behind the signer's back. A prompt is bound to the directory it runs in, because session metadata, where the directory is written, can be rewritten by anyone holding the Honcho token. It is bound to the model and effort it carries for the same reason. An output piece is bound to the agent program's session ID it reports, because the bridge continues that session on the next prompt.

## Session

```
id        geminus-<persona>-<YYYYMMDD>-<6 hex>
peers     the human and the persona, neither observed (a conversation is not memory)
metadata  {"kind": "geminus",
           "geminus": {"v": 1, "human": "h1karu", "persona": "mirai", "harness": "claude",
                       "cwd": "/home/me/project", "model": "opus", "effort": "high",
                       "title": "first words of the first prompt",
                       "pending": true, "updated": 1795083503, "sig": "…"}}
```

`harness` and `cwd` are fixed when the session is created: a conversation belongs to one agent program in one directory. Until then the owner can name any directory (`/cwd`, `/new DIR`, `--cwd` in geminus); geminus does not check it, because only that computer knows what is there. `!cd DIR` in geminus names one relative to the directory chosen so far: geminus joins the two as text and, when the node card says which directory the bridge starts in, sends the full path, since the bridge reads a relative `cwd` from its own default and not from an earlier choice.

The bridge reads `cwd` the way its owner would type it on that computer: `~` is their home directory, a relative path starts at the bridge's default directory, and an empty `cwd` is the default directory itself. A directory that does not exist fails the prompt with a message naming it and the prompt's `detail` set to `the working directory does not exist`; geminus reads that detail to let go of a conversation in which no agent has answered yet. The directory becomes the working directory of the agent program's process and is never put on a command line. What is signed is the text as typed, not what it resolves to.

`model` and `effort` are the owner's settings for this conversation (`/model`, `/effort` in geminus); either is left out when not set. They are kept here only so that geminus gets them back when the conversation is reopened, from any computer. The bridge does not read them from the session: each prompt carries its own.

`sig` signs the description: persona, harness, human, `cwd`, `model`, `effort`, title and `updated`, bound to the session ID. geminus treats a session as one of the owner's conversations only if it verifies; a session anyone else created, or whose description was changed, is counted and ignored, never listed or reopened. `pending` is outside the signature, because the bridge flips it and it only causes a read.

`updated` is when the last prompt was written; geminus sorts conversations by it, because Honcho records only when a session was created.

`pending` is how the bridge finds work with one request: it lists sessions with `kind: geminus`, its own `persona` and `pending: true`. geminus sets it after writing a prompt. The bridge clears it first and reads the messages afterwards, so a prompt written in between is still seen.

## Prompt

A message with `peer_id` = the human and the prompt as its content, nothing added.

```
metadata  {"geminus": {"v": 1, "role": "prompt", "nonce": "<uuid>", "ts": 1795083503, "sig": "…",
                       "state": "queued", "model": "opus", "effort": "high"}}
```

`model` and `effort` are optional and apply to this run only. A value is a bare name: a model matches `[A-Za-z0-9][A-Za-z0-9._:/@#\[\]+-]{0,127}`, an effort `[A-Za-z0-9][A-Za-z0-9_-]{0,31}`. geminus does not send anything else and the bridge refuses anything else, because the value becomes an argument of the agent program.

The bridge runs a prompt once, when all of these hold: the session is its own; the author is a member listed as `human`; the signature verifies for the session's persona, harness and `cwd` as its metadata states them now and for the `model` and `effort` in the prompt's own metadata; `ts` is at most 7 days old and not more than 5 minutes ahead; and it has not handled that `nonce` before (it remembers handled nonces locally). It then moves `state` through `running` to `done`, `failed` or `refused`, adding `detail` when there is something to say. Anything else is left alone.

Prompts of one session run one at a time, in the order Honcho stored them.

## Output

Messages with `peer_id` = the persona. The content is what the agent program printed, cut into pieces of at most 6,000 characters.

```
metadata  {"geminus": {"v": 1, "role": "output", "turn": "<id of the prompt message>", "seq": 1,
                       "final": false, "ok": true, "native": "<the agent program's own session id>",
                       "ts": 1795083520, "sig": "…"}}
```

`seq` counts from 1 within a turn. The last piece has `final: true`; `ok: false` on it means the turn failed and the content says why. Pieces are written as output arrives, at most every two seconds. geminus shows a piece only if its signature verifies.

`native` is the session ID inside the agent program. The bridge reads it back from the newest output of the session that carries its own valid signature, to continue the same conversation there, so it keeps no table of its own; a person at that computer can open the same conversation with the program's own resume command.

## Node card

```
peer metadata  {"geminus_node": {"v": 1, "harnesses": [{"id": "claude", "name": "Claude Code",
                                                         "options": ["model", "effort"]}, …],
                                 "cwd": "/home/me", "ts": 1795083000,
                                 "sig": "…", "options_sig": "…", "cwd_sig": "…"}}
```

The bridge publishes it when it starts and every five minutes, listing the agent programs found on that computer's `PATH` at that moment. geminus offers exactly these, and shows the card's age as "last seen".

`options` names the settings the bridge can pass to that program for one run. geminus accepts `/model` or `/effort` only for a program that lists it. IDs and options contain neither `:` nor `,`.

`sig` covers the harness IDs, as it has from the start, and `options_sig` the IDs with their options. A geminus older than options verifies `sig` and ignores the rest; a card from a bridge older than options has no `options_sig`, and geminus then offers its programs without settings. Options whose signature does not verify are treated the same way: not there.

`cwd` is the bridge's default directory: where a prompt runs when its conversation names none. geminus only shows it, under the prompt box, and `cwd_sig` signs it so that what is shown is what that computer said. A card without it, or with one that does not verify, is read as before and geminus then says "default directory" without a path.

## Defaults

```
human peer metadata  {"geminus": {"v": 1, "data": "<the defaults as JSON text>", "ts": 1795083000, "sig": "…"}}

the defaults         {"v": 1, "persona": "mirai",
                      "personas": {"mirai": {"harness": "claude", "cwd": "/home/me/project"}}}
```

Signed, because they choose which computer, program and directory the owner's next prompt goes to. geminus ignores defaults that do not verify and asks instead.

The signature does not prove the defaults are the newest. Someone holding the Honcho token can put back an older copy the owner once signed. Detecting that would need a record on the owner's computer, and geminus keeps none; instead `/default` shows when the defaults were saved, and the persona, harness and directory in effect are on screen before anything is sent.

## What geminus shows

geminus shows a prompt as the owner's, and an output as the agent's, only if its signature verifies; anything else in the conversation is counted and named as unsigned, never displayed. It does not reopen a conversation in which no prompt verifies against the harness and directory in the session's metadata: that means the metadata was changed after the prompts were signed.

## Doorbell

After writing a prompt, geminus rings the persona's geminish doorbell: an empty HTTPS POST to the Mozilla push endpoint the persona published and signed in its peer metadata (`geminise_push`). The ring carries nothing. The bridge reads Honcho when it rings, and on its timer otherwise.

## Running a prompt

The bridge hands the prompt to the agent program's own non-interactive, session-continuing mode. A model or effort the prompt carries is given to the program for that conversation only, in the way the program provides (table below): as an option of the run, or, where the program refuses such options on a session that exists, as a line sent in the session before the prompt. With none set, nothing is passed and the program's own default applies. The bridge never writes a program's configuration, so nothing set in geminus reaches the program's default or its other sessions. A prompt that sets something the program has no way to take fails without running.

It passes no option that loosens permissions unless the computer's `geminise.json` says so (`"geminus": {"args": {"claude": ["--permission-mode", "acceptEdits"]}}`). `"geminus": false` there turns the bridge off; `"geminus": {"cwd": …, "timeout": …}` set the default directory and how long one prompt may run (default one hour).

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

| Harness | Model | Effort |
|---|---|---|
| `hermes` | the line `/model <model>` in the session | the line `/reasoning <level>`; `/reasoning reset` to go back |
| `claude` | `--model <model>` | `--effort <level>` |
| `codex` | `-m <model>` | `-c model_reasoning_effort="<level>"` |
| `grok` | `-m <model>` | `--reasoning-effort <level>` |
| `pi` | `--model <model>` | `--thinking <level>` |
| `opencode` | `-m <provider/model#variant>` | none; the variant is part of the model name |

Options go before the positional arguments. Which names and levels exist is the program's business: geminus passes the value through and the program's own error comes back.

Hermes fixes model and reasoning when a session is created and rejects them as options on every later run, but its `/model` and `/reasoning` commands change them for that session only. The bridge sends the line as a turn of its own before every prompt that carries the setting (Hermes keeps it in the memory of its gateway process, which may have restarted), and `/reasoning reset` before a prompt without an effort once the conversation has had one. What Hermes answers to the line is written to the conversation when the setting differs from the previous prompt's, so a model name Hermes does not know is seen at once; Hermes reports that in words with a zero exit status, so the bridge cannot tell and runs the prompt. Hermes has no line that returns a session to its default model: after a model was set, a prompt without one fails and says so.

## Releases

geminus updates itself from one server (`cmd/geminus-latest-channel`), which builds each release from the source, signs it and hands it out. Base address: `https://sibyl.haunted.fail/geminus/updates`, or `GEMINUS_UPDATES_URL`.

| Method and path | What it is |
|---|---|
| `GET <base>/latest` | The manifest of the newest release |
| `GET <base>/<version>` | The manifest of that release |
| `GET <base>/<version>/<target>` | The program for one target |
| `POST <base>/<version>` | Build the tag `v<version>` of the source, sign it and publish it |
| `GET <base>/<version>/build` | How that build is going |

`<version>` is `X.Y.Z`, digits only, without leading zeros. `<target>` is `<os>-<arch>` in Go's names, lower case (`windows-amd64`). No request has a body, and nothing can be uploaded.

A manifest:

```json
{"version": "0.5.0", "ts": 1795300000,
 "files": {"windows-amd64": {"sha256": "<64 hex digits>", "size": 7478784}},
 "sig": "<128 hex digits>"}
```

`sig` is an Ed25519 signature, in hex, over these lines joined with a newline (no newline at the end):

```
geminus-release
<version>
<ts>
<target> <sha256> <size>        one line per file, sorted by target
```

The private key is a file on the server's computer and exists nowhere else. Its public half is compiled into geminus (`internal/updates/key.go`), and the server links its own public key into every program it builds, so a released program trusts the key that signed it. This is the one signature in the protocol that the group secret cannot make.

A request is signed with the group secret as every other record is (see [Signatures](#signatures)): content empty, label `geminus-updates-request:<METHOD>:<path>:<sha256 of the body>`. It carries three headers: `Geminus-Time` (the `ts` it was signed with), `Geminus-Body-SHA256` (the hex SHA-256 of its body, that is, of nothing) and `Geminus-Signature`. `<path>` is the escaped path as sent, from the first slash. The server refuses a request without them (401), with a signature that does not verify (401), or signed more than 300 seconds from its own clock (401, and the text says the clocks differ).

The answer to `POST <base>/<version>` and to `GET <base>/<version>/build`:

```json
{"version": "0.5.0", "state": "building"}
```

`state` is `building`, `published`, or `failed` with `error` holding the end of what git or the compiler said. `POST` answers 202 when it started the build or the build is running, 200 when the release exists already, 409 when another release is being built or the version is not greater than the latest. A build that failed can be asked for again.

Rules the server keeps:

- A release is built here, from the tag `v<version>` (a tag, not a branch) of the one repository the server is set up with, with the Go toolchain installed here. The source cannot choose another toolchain or fetch a module, and nothing from it is run. A program that does not hold the server's public key is not released.
- One release is built at a time. A release is visible, and its programs can be fetched, once its manifest is written; nothing of a failed build is.
- A version must be greater than the latest. A published release never changes and is never built again.

Rules geminus keeps:

- It believes a manifest only if the signature verifies with the release key it was built with, and installs a file only if its size and SHA-256 are the ones the manifest names.
- It moves only to a version greater than its own. A program whose version is not a plain `X.Y.Z` counts as older than the release `X.Y.Z`; one that has no version at all takes any release.

## App-layer E2EE transport (`GE2E` v1)

When clients reach Honcho across an HTTPS reverse proxy or tunnel (such as Cloudflare Tunnel), TLS terminates at the edge. To conceal Honcho API paths, workspace/session/peer identifiers, Bearer tokens, prompts, and outputs from intermediate hops while keeping Honcho storage raw, clients may tunnel all Honcho HTTP calls through the loopback terminating proxy (`cmd/geminus-e2ee-proxy`).

Configuration (optional; when unset, clients speak directly to Honcho):

- `e2ee_url` at the top level of `geminise.json`, or `geminus.e2ee_url` (overrides top level), or `GEMINUS_E2EE_URL` (overrides both). Must be `https://` or loopback `http://`.

Wire format (`POST <e2ee_url>`, `Content-Type: application/octet-stream`, no outer `Authorization` header):

```
header[0:4]   = "GE2E"
header[4]     = 0x01                       (version)
header[5]     = 0x01 (request) | 0x02 (response)
header[6:14]  = int64 big-endian Unix seconds (ts)
header[14:38] = 24-byte random salt
body[38:]     = ChaCha20-Poly1305(key, nonce, plaintext_json, aad=header[0:38])
```

- Key derivation (HKDF-SHA256, RFC 5869) from the `geminise.json` group secret and per-request 24-byte `salt`:
  - `prk = HKDF-Extract(salt, secret)`
  - Request key/nonce: `HKDF-Expand(prk, "geminus-e2ee-v1:req", 32)` and `"geminus-e2ee-v1:req-nonce"` (12 bytes)
  - Response key/nonce: `HKDF-Expand(prk, "geminus-e2ee-v1:res", 32)` and `"geminus-e2ee-v1:res-nonce"` (12 bytes)
- AEAD: RFC 8439 ChaCha20-Poly1305, implemented with zero external dependencies in both Go (`internal/e2ee`) and Python (`plugin/geminish/e2ee.py`).
- Inner JSON: requests carry `m` (method), `p` (path), optional `q` (query), `h` (headers), `b` (base64 body), and `pad`; responses carry `s` (status), `h`, `b`, and `pad`. `pad` rounds inner JSON length up to coarse buckets (powers of two 512B..32KiB, 4KiB steps above 32KiB).
- Proxy defenses: rejects `|now - ts| > 300s`, rejects replayed `salt` values within a 600s window, allows only `/v3/...` and `/health` without traversal, strips hop-by-hop headers, and forwards the decrypted raw HTTP request to loopback Honcho. Clients verify that the decrypted response's `(ts, salt)` match the originating request.

