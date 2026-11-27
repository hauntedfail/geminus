# geminus architecture

This document describes how the code works today. `docs/protocol.md` specifies the records exchanged; this explains the shape and the reasons.

## What geminus is for

The owner works on several computers and wants to stay at one of them: type to the agent on another computer and see what it does, without walking over, without opening ports, and without relaying through a second agent that rephrases things.

geminus is the client for that and nothing else. It is deliberately a thin wrapper: it does not run a model, does not start or drive an agent program, and stores nothing.

## Shape

```
 this computer                         Honcho                      the persona's computer
┌───────────┐   HTTPS (outgoing)   ┌───────────┐   HTTPS (outgoing)   ┌───────────────────────┐
│  geminus  │ ───────────────────▶ │ sessions  │ ◀─────────────────── │ bridge (in geminish's │
│           │ ◀─────────────────── │ messages  │ ───────────────────▶ │ resident poller)      │
└───────────┘                      │ peers     │                      │   └─▶ hermes / claude │
      │                            └───────────┘                      │       / codex / …     │
      └── empty POST to Mozilla push ("doorbell") ───────────────────▶└───────────────────────┘
```

| Package | Responsibility |
|---|---|
| `cmd/geminus` | `main` only. |
| `internal/chat` | The terminal interface: picking persona and harness, sending, showing answers as they arrive, slash commands, and the prompt box with the status around it. |
| `internal/wire` | The protocol: record shapes, signatures, the node card, the doorbell. |
| `internal/honcho` | The Honcho HTTP calls geminus makes (direct or tunneled over `GE2E` v1); `honchotest` is an in-memory Honcho for tests. |
| `internal/e2ee` | Zero-dependency RFC 8439 ChaCha20-Poly1305 + HKDF-SHA256 `GE2E` v1 HTTP envelope and loopback terminating proxy handler. |
| `cmd/geminus-e2ee-proxy` | Loopback terminating proxy that decrypts `GE2E` v1 frames and forwards raw HTTP requests to local Honcho. |
| `internal/profile` | Reads the two existing files that say who and where this computer is. |
| `internal/version` | The version string. |
| `internal/updates` | Updating geminus itself: versions, the signed manifest of a release, the release key, signed requests, the client, and replacing the program on disk. |
| `internal/updates/server`, `cmd/geminus-latest-channel` | The server that builds, signs and hands out releases, its key file, and its `main`. |
| `internal/updates/build` | Building the programs of a release from the tag of its version: git and the Go toolchain, nothing else. |

No third-party dependencies.

## Decisions

**Honcho is the only channel, and the place for everything.** The owner chose to build on Honcho so as not to design or manage persona switching, sessions or storage again. So each concept is a Honcho object: a persona is a peer, a conversation is a session, a turn is messages, what a computer offers and the owner's defaults are peer metadata. geminus has no files. Any computer with the same Hermes profile shows the same personas, conversations and defaults, and a conversation can be continued from any of them.

**No ports.** Both sides only make outgoing HTTPS requests to Honcho, which works from behind NAT. The one other request is the doorbell: an empty POST to the Mozilla push endpoint that geminish already keeps for its own purposes, so the bridge reads Honcho at once instead of at its next timer tick. It carries nothing and decides nothing; without it the prompt is picked up within the bridge's poll interval.

**The bridge is geminish's, the conversation is geminus's.** Something on the other computer has to read Honcho and start the agent program. geminish's poller already runs there, so the bridge is a part of it and geminus stays a client. But the two are different products: geminish is agents writing to agents, where a message is input from a peer and never the owner's word. A geminus prompt is the owner's word. So the bridge shares only the process: geminus records are recognized by metadata (`kind: geminus` on a session, a `geminus` key on a message), never go through geminish's mail rules, and are handed over without any preamble.

**The prompt is passed word for word.** Whether a computer may run something is decided by who signed it, not by wrapping the text in warnings. The prompt message's content is exactly what was typed, and the output messages' content is exactly what the program printed, so the Honcho session reads as the conversation it is.

**The agent program's own non-interactive mode.** The bridge runs one process per prompt in the mode each program provides for exactly this (`claude -p --resume`, `codex exec resume`, `hermes chat --continue`, …). Nothing else is installed, the program's permissions are its own, and the conversation is an ordinary session of that program on that computer.

**Harness choices are observed.** A bridge publishes which agent programs its computer has, found in the owner's interactive shell environment there, and republishes every five minutes. geminus offers exactly that list and shows its age. Looking in the owner's shell environment matters: a service has a short `PATH` and no profile, and would otherwise pick a different copy of a program than the owner gets (this happened: an older Codex with another default model).

**Asynchronous by construction.** A prompt is a record; so is each piece of the answer. Sending never waits for the other side, watching can be abandoned without affecting the work, and a computer that is off simply answers later. The screen states what is known: recorded, doorbell rung, picked up and working, finished or failed, or not picked up since when.

## The bridge

The bridge lives in geminish (`bridge.py`, from 0.5.0), next to but apart from the mail client. On every poller cycle it:

1. republishes its node card if it changed or is five minutes old;
2. lists the geminus sessions addressed to its persona with `pending: true` (one request);
3. for each, clears the mark, then reads the messages and runs every prompt that qualifies, in a worker of its own so that one long turn does not block another conversation or the mail.

A prompt qualifies when its signature verifies for this session, persona and harness, its author is a human member, it is at most seven days old, and its one-time value has not been run here before (remembered in the poller's local state, so resetting a message's state or re-posting it does not run it again). Refusals are written back as the prompt's state with the reason.

Output is read as it is printed, written back in signed pieces at most every two seconds, and ends with a final piece; a failed run ends with a final piece that says why, with the program's own error output.

## Trust

The secret is geminish's group secret, already on each member computer. Signatures bind each record to its purpose with a label (see the protocol), so a signature for a prompt cannot serve as output, as a node card, as geminish mail, or as a prompt in another conversation or for another harness.

What this gives: holding the Honcho token alone, one can read conversations and write records, but cannot make a computer run anything, cannot move a signed prompt to another conversation, harness or directory, cannot point the bridge at a different session of the agent program, cannot make geminus show forged output or forged prompts, cannot plant a conversation or change one's harness or directory, cannot write new defaults for the owner, and cannot change what a computer appears to offer. Each of these is a field that a signature covers; the first version left the directory, the program's session ID, prompts as displayed and the defaults uncovered, and a review of the commit found it. One thing the token still allows is putting back an older copy of the owner's signed defaults. Refusing that would take a record on the owner's computer, which geminus does not keep; the age of the defaults and the target in effect are shown instead.

What it also does not give: separation between member computers. Any computer holding the secret can issue prompts as the owner. A key that only the owner's own keyboard holds would need a secret that geminus owns, which the design rules out; it remains the obvious next step if that separation is wanted.

## Updates

A fix used to reach another computer only when someone copied a binary there. Now a release is made once, by a small server on sibyl (`geminus-latest-channel`, behind the existing tunnel at `sibyl.haunted.fail/geminus/updates`), and each geminus fetches it when asked (`geminus update`, `/update`).

This is the one thing geminus does that does not go through Honcho, on purpose: Honcho holds conversations, and programs are files. The server is a directory of releases behind five routes, standard library only, no database; to make a release it starts git and the Go toolchain. The idea of a directory of releases with a "latest" comes from Flipper's update server; none of its code or stack does.

- **Requests are signed with the group secret** under their own label (see the protocol). Reads are signed too, because the address is public and the programs are not.
- **A release is signed by sibyl, with a key of its own.** The first version signed the manifest with the group secret on the computer that built it, so any member computer could have made a release for all the others; a review of that commit said so. Now the manifest (version, and per target the size and SHA-256) carries an Ed25519 signature whose private key is a file on sibyl and nowhere else, and geminus has the public half compiled in. Only the signature made at build time is trusted: geminus verifies it and then the bytes, so neither the tunnel (TLS ends at Cloudflare) nor a member computer can change what a version means.
- **A release is built where it is signed.** Signing whatever a member uploads would have left the same hole, so nothing can be uploaded. A member asks for a version; sibyl fetches the tag `vX.Y.Z` from the repository with a read-only deploy key, builds every target with its own Go toolchain (`GOTOOLCHAIN=local`, `GOPROXY=off`, nothing from the source is run), and signs what it built. Settings of sibyl itself (a git configuration with filters, a go env file) do not reach the build either. What a release contains is therefore decided by who can push a tag to the repository, not by who holds the group secret. A member can only make sibyl build a tag that is there, once, and only forward. A computer that holds both the group secret and the right to push can still release what it likes; that is the computer the source is written on. Having sibyl accept only tags signed with a key kept off the member computers would be the next step if that is to be separated too.
- **Each program trusts its signer.** The server links its public key into the programs it builds and refuses to release a program in which the key is not found, so a release can never ship with a key that does not match the one that signed it. The key in the source is the same one and serves builds made by hand. Losing the private key means one copy by hand per computer again; that is the price of not being able to forge a release, and why `-keygen` never replaces a key.
- **Releases only go forward and never change.** The server refuses a version that is not greater than the latest and never builds a published one again; geminus refuses a version that is not greater than its own. An old signed manifest replayed to a geminus therefore does nothing.
- **Replacing a running program.** The download is written beside the program, flushed to disk and renamed over it, so the file is at every moment the old program or the new one. Windows does not let a running program be overwritten but lets it be renamed, so there it is moved to `geminus.exe.old` first and removed at a later start, once no process runs from it.
- **Carrying on.** geminus keeps no state, so the new program only needs to be told which conversation to reopen. On Unix it replaces the process (`exec`); on Windows it is started on the same console and the old process waits for it. Two programs must never read one keyboard, so the reader stops after an `/update` line and reads on only when geminus says it stays (nothing newer, or an error); the old process on Windows therefore takes no key from the new one. With piped input geminus does not restart, since lines already read would be lost, and it does not restart either when a value it would pass on (a directory named `-x`) would be read as an option.
- **Only when asked.** At the start geminus looks once, beside everything else and for at most two seconds, whether a newer release exists, and says so under the box. A server that does not answer is not mentioned.

## What goes on the screen

Agent output is printed as it arrives. Nothing from outside can control the terminal: `chat.Printer` strips control characters and bidirectional overrides from everything it prints (output, error text, names, titles, IDs), keeping only the few color codes geminus writes itself, and values shown inside a line are flattened to one line.

On a terminal the bottom rows are a live region: the prompt box, the line above it that follows the waiting prompt, and the status and hints under it. Everything else scrolls above. The pieces:

| File | Part |
|---|---|
| `frame.go` | `render`: a pure function from what is to be shown (`view`, the typed text, the terminal size, the time) to rows and a cursor position. The whole look is tested as text. |
| `live.go` | The live region: prints above itself, redraws in place by moving up from the cursor and erasing downward, and applies keys to the editor. |
| `editor.go`, `keys.go`, `width.go` | The typed text and its history; raw terminal input turned into keys (UTF-8, escape sequences, bracketed paste); column widths of wide and combining characters. |
| `tty_*.go` | Raw mode and terminal size for macOS, Linux and Windows, with the standard library only. |
| `rich.go` | The main loop on a terminal, and the copy of App state (`view`) the box shows. |

Three goroutines touch the live region, under one lock: the keyboard reader, which also does the editing so that typing never waits for Honcho; a 100 ms timer for the spinner and the terminal size; and the main loop, which prints and updates the status. Entered lines reach the main loop through a channel, exactly as lines read from a pipe do, so commands and conversations are the same code in both modes. The one difference is that on a terminal sending does not wait for the answer.

The live region writes its own cursor movement, so it cannot go through `Printer`. What it draws is safe for the same reason as the rest: every value from outside is flattened and stripped before it enters `view`, typed and pasted text loses control characters when it enters the editor, and unfinished output reaches it only through `Printer`.

When stdin or stdout is not a terminal, or raw mode is not available, geminus reads line by line in the terminal's normal mode and echoes piped lines, which keeps piped runs and test output readable.

Model and effort are not geminus state either. They sit in the conversation's signed description in Honcho and travel inside each signed prompt; the bridge turns them into options of that one run of the agent program and writes nothing to the program's configuration.

## Tests

| Level | What runs | What it shows |
|---|---|---|
| `internal/wire` | Signing and record parsing | Signatures equal the ones the bridge's Python code produces; each is good for one purpose only; node cards and doorbell records must be signed for their own peer |
| `internal/honcho` | The client against the in-memory Honcho | Paging, nested metadata filters, metadata merged rather than replaced, errors without the token |
| `internal/chat` | The interface against the in-memory Honcho and a bridge that follows the protocol | Every flow and command, asynchronous answers, forged output ignored, defaults in Honcho, no files written |
| `internal/chat` (`live_test.go`) | The prompt box against a small terminal emulator (`vt_test.go`) | What is left on the glass: output above the box in order, wrapping of wide text, the cursor, keys, paste, questions, resize, and the whole app with the box |
| `test` | The built binary, the real Python bridge, a fake agent program, the in-memory Honcho | That the two products agree end to end, including continuing a conversation, a failing turn, and a forged prompt being refused |
| `internal/updates`, `internal/updates/server` | Versions, manifests, the release key, signed requests; the server over a temporary directory with a stand-in for the build; the client against that server; replacing a stand-in program | A release is built once, signed with the server's key and never changed; the group secret cannot sign one; unsigned, stale and forged requests, uploads and names outside the directory are refused; a failed build serves nothing; a cut or altered download leaves the program as it was |
| `internal/updates/build` | A small repository shaped like this one, real git and the real Go toolchain | Only a tag is built, with its version and the signer's key linked in; a branch of that name, a missing tag and a version that is not `X.Y.Z` are refused; a compile error is reported with what the compiler said |
| `test` (`updates_test.go`) | The built server and the built geminus, and this source as a tagged repository | `geminus release` has the server build and sign the tag, `geminus update` installs it, and the installed program then reports the new version; a geminus that trusts another key refuses the release |
| geminish `tests/test_bridge.py` | The bridge on its own | Runs once, verbatim, in order; refusals; output pieces; each program's command line; the owner's shell environment |

The fake agent program proves the plumbing, not that a real program behaves the same; that was checked by hand for the harnesses marked so in the README. The terminal emulator in the tests proves the drawing logic, not every terminal: the box was also run in a real pseudo-terminal on macOS. The Windows console code is compiled and vetted, and has not been run; neither has replacing the running program there. The restart after `/update` is tested up to the arguments the new program is started with: replacing a test process with another program is not something a test can survive.
