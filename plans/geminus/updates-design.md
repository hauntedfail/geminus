# geminus updates: design

Status: approved by the owner on 2026-11-21 (address, tech stack, and the name `geminus-latest-channel` are theirs).

## Why

A fix to geminus reaches another computer only when someone copies a binary there by hand, and from the agent's side each such step waits for an approval. The owner already runs a central, private computer (sibyl). Releases are published to sibyl once, and every geminus fetches its own update from there with one command, without leaving the conversation it is in.

## Goals

- `geminus update` and `/update` replace the running program with the latest release and carry on in the same conversation.
- Publishing a release is one command on the build computer (chrome). No ssh, no step on sibyl.
- No new open port, no new key or token, no configuration file, no state kept by geminus.
- The server is a small file service: easy to read in one sitting, standard library only.

## Not in this work

- Updating the bridge (geminish). The address leaves room for it (`/geminish/updates/` beside `/geminus/updates/`).
- Automatic updates. geminus says a release exists; the person decides.
- Channels (beta, nightly), release notes, deltas, rollback to an older release. Going back is done by publishing a new version.

## Not acceptable, even if it works

- Anything stored in Honcho for this. Honcho carries conversations, not files.
- Anything new on `honcho.haunted.fail`, or a URL derived from it.
- A program that replaces itself with bytes it has not verified.

## Approaches

| | Approach | Trade-off | |
|---|---|---|---|
| A | Releases as chunked messages in a Honcho session | Nothing to run on sibyl; but files in a conversation store, about 170 messages per binary | Rejected by the owner: too much for Honcho |
| B | A small Go file service on sibyl behind the existing tunnel, modelled on the idea of Flipper's update server (a directory of releases, an index, a "latest") | One more small process on sibyl, one tunnel rule | **Chosen** |
| C | GitHub releases of the private repository | A GitHub token on every computer; not central on sibyl | Not considered further |

From Flipper's server only the concept is taken. Its stack (gin, a GitHub client) is not.

## Address

Base: `https://sibyl.haunted.fail/geminus/updates`

| Method and path | What it is |
|---|---|
| `GET /geminus/updates/latest` | The manifest of the newest release |
| `GET /geminus/updates/{version}` | The manifest of that release |
| `GET /geminus/updates/{version}/{target}` | The program for one target, e.g. `0.5.0/windows-amd64` |
| `PUT /geminus/updates/{version}/{target}` | Upload one program |
| `PUT /geminus/updates/{version}` | Publish: upload the manifest, which makes the release visible and the latest |

`{version}` is `X.Y.Z`, digits only. `{target}` is `<os>-<arch>` as Go names them. No file extensions and no `files/` level: the path says product, purpose, release, target and nothing else.

geminus has the base built in and `GEMINUS_UPDATES_URL` replaces it (tests, a move of the server).

## Tech stack (settled with the owner)

| Matter | Choice |
|---|---|
| HTTP | `net/http` of the standard library; method and wildcard patterns of `ServeMux`. No framework |
| Dependencies | None outside the standard library, as in geminus |
| Where the code lives | This repository, `cmd/geminus-latest-channel` (server) and `internal/updates` (manifest, request signing, shared by server and client) |
| Storage | A directory: `<dir>/<version>/<target>`, `<dir>/<version>/manifest.json`, `<dir>/latest`. No database |
| File access | `os.Root`, so no path leaves the directory |
| Logs | `log/slog` to stderr, collected by journald |
| Settings | Two flags, `-listen` (default `127.0.0.1:8651`) and `-dir`. The signing secret comes from the Hermes profile already on sibyl |
| Running it | systemd user unit, `Restart=always` (no sudo; same as the geminish poller) |
| Replacing the program | Own code, rename in place. No self-update library |
| Go version | `go.mod` goes from 1.23 to 1.25: `os.Root` came with 1.24, its `Rename` and `ReadFile` with 1.25 |

## Trust

Everything is signed with the group secret that geminus and the bridge already use (`internal/wire.Sign`), under new labels:

| Record | content | label |
|---|---|---|
| Request | empty | `geminus-updates-request:<METHOD>:<path>:<sha256 of the body>`, `ts` in header `Geminus-Time`, the body digest in `Geminus-Body-SHA256`, signature in `Geminus-Signature` |
| Manifest | one line per file, `<target> <sha256> <size>`, sorted, joined with newlines | `geminus-release:<version>`, `ts` = time of release |

- Every request is signed, reads too: the programs are private and the address is public. The server accepts a `ts` within 5 minutes of its clock.
- The manifest is signed on the build computer. geminus verifies that signature, then the size and SHA-256 of what it downloaded, before anything is replaced. TLS ends at Cloudflare, so the transport alone is not trusted.
- geminus only moves to a version greater than its own, so an old signed manifest cannot be replayed to push a release back.
- A release cannot be changed once published: a second `PUT` of a version or file with different bytes is refused.
- Who can publish: whoever holds the group secret. That holder can already send prompts to the agents on every computer, so this adds no new power. A stolen secret is as bad as before, not worse.
- The server refuses a manifest unless its signature verifies, its version is greater than the latest, and every file it lists is present with the stated size and hash.

## Server

- Listens on `127.0.0.1` only. The existing cloudflared tunnel gets one rule above the current `sibyl.haunted.fail` rule: `hostname: sibyl.haunted.fail`, `path: ^/geminus/updates(/.*)?$`, `service: http://127.0.0.1:8651`.
- The signature is checked before a body is read, so nobody outside the group can make the server store anything. Uploads are then streamed to a temporary file while hashed, checked against the signed body hash, and renamed into place. Limits: 32 MiB per program, 64 KiB per manifest. Read, write and header timeouts set.
- Downloads go through `http.ServeContent`.
- `latest` is a one-line file replaced by rename.

## Publishing

`scripts/release.sh X.Y.Z`, on chrome: refuses a tree with uncommitted changes, runs the tests, cross-builds with that version, uploads the six programs, signs and uploads the manifest, tags `vX.Y.Z` and pushes the tag. The upload is done by a hidden `geminus release` subcommand so signing lives in one place.

## geminus

- `geminus update`: fetch `latest`, verify, compare; print `already the latest (0.5.0)` or download, verify, replace, print `geminus 0.4.0 → 0.5.0`.
- `/update` in a conversation: the same, then geminus starts the new program in the same conversation (`persona <name> --resume <id>`; before a conversation exists, the persona, harness and directory chosen). With piped input it does not restart: lines already read would be lost. The conversation lives in Honcho, so the screen comes back with what was said and any answer still on its way.
- When it starts, geminus asks for `latest` once in the background (2 s limit, silent on failure). A newer release is shown in the status under the box: `update 0.5.0 · /update`.
- Replacing: the download is written beside the program and renamed over it. On Windows a running program cannot be overwritten but can be renamed, so the old one becomes `geminus.exe.old` and is removed at the next start.
- Restarting: on Unix the process image is replaced (`exec`). On Windows the new program is started on the same console and the old one waits for it and passes on its exit code.
- A program whose own version is not a plain `X.Y.Z` (`0.4.0-dev`, a build from the repository) counts as older than the release `X.Y.Z`; one without any version (`dev`) takes any release.
- If the program's directory is not writable, the error names the path. geminus does not ask for more rights.

## One-time steps that need the owner

1. sibyl: place `geminus-latest-channel` and its unit `geminus-latest-channel.service`, add the tunnel rule, restart `cloudflared-honcho.service` (a system unit; Honcho is unreachable for a few seconds).
2. mirai: copy one geminus that has `update`. Every later version arrives through it.

After these, no step of an update needs an approval.

## Tests

- `internal/updates`: manifest signing and verification, version order, request signatures, refusals (bad signature, old `ts`, wrong hash, wrong size, older version).
- `cmd/geminus-latest-channel` with `httptest`: every route, paths that try to leave the directory, a release that cannot be overwritten, size limits.
- Replacing a program: on a temporary directory, for the Unix and the Windows rule.
- `test/`: the built server and the built geminus, a release published and fetched, the program on disk replaced and reporting the new version.
- Not testable here: the Windows console hand-over and the rename of a running `.exe`. They are checked on mirai.

## Left to the builder

File and function names, the port if 8651 is taken, the wording of messages, how `/update` is drawn while it downloads.

## Names

The server's program and its systemd unit are `geminus-latest-channel`, named by the owner. The path stays `/geminus/updates/`.
