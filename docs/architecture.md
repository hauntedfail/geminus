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
| `internal/chat` | The terminal interface: picking persona and harness, sending, showing answers as they arrive, slash commands. |
| `internal/wire` | The protocol: record shapes, signatures, the node card, the doorbell. |
| `internal/honcho` | The Honcho HTTP calls geminus makes; `honchotest` is an in-memory Honcho for tests. |
| `internal/profile` | Reads the two existing files that say who and where this computer is. |
| `internal/version` | The version string. |

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

What this gives: holding the Honcho token alone, one can read conversations and write records, but cannot make a computer run anything, cannot move a signed prompt to another conversation, harness or directory, cannot point the bridge at a different session of the agent program, cannot make geminus show forged output or forged prompts, cannot change the owner's defaults, and cannot change what a computer appears to offer. Each of these is a field that a signature covers; the first version left the directory, the program's session ID, prompts as displayed and the defaults uncovered, and a review of the commit found it. What it does not give: separation between member computers. Any computer holding the secret can issue prompts as the owner. A key that only the owner's own keyboard holds would need a secret that geminus owns, which the design rules out; it remains the obvious next step if that separation is wanted.

## What goes on the screen

Agent output is printed as it arrives. Nothing from outside can control the terminal: `chat.Printer` strips control characters and bidirectional overrides from everything it prints (output, error text, names, titles, IDs), keeping only the few color codes geminus writes itself, and values shown inside a line are flattened to one line.

Input is read line by line in the terminal's normal mode, so the terminal's own line editing and input methods work. When stdin is not a terminal each line is echoed, which keeps piped runs and test output readable.

## Tests

| Level | What runs | What it shows |
|---|---|---|
| `internal/wire` | Signing and record parsing | Signatures equal the ones the bridge's Python code produces; each is good for one purpose only; node cards and doorbell records must be signed for their own peer |
| `internal/honcho` | The client against the in-memory Honcho | Paging, nested metadata filters, metadata merged rather than replaced, errors without the token |
| `internal/chat` | The interface against the in-memory Honcho and a bridge that follows the protocol | Every flow and command, asynchronous answers, forged output ignored, defaults in Honcho, no files written |
| `test` | The built binary, the real Python bridge, a fake agent program, the in-memory Honcho | That the two products agree end to end, including continuing a conversation, a failing turn, and a forged prompt being refused |
| geminish `tests/test_bridge.py` | The bridge on its own | Runs once, verbatim, in order; refusals; output pieces; each program's command line; the owner's shell environment |

The fake agent program proves the plumbing, not that a real program behaves the same; that was checked by hand for the harnesses marked so in the README.
