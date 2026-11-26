# Go permission and directory contract

## Permission questions

The console verifies the existing bridge ask signature, including its offered
choices, before showing an actionable question. Legacy asks without a choice
list retain the protocol's allow/deny list. The console never manufactures a
session/persistent option. Only an exact offered value is sent in the signed
answer; aliases and numbers select those values, not new permission rules.

The terminal picker is asynchronous with respect to the main loop. Presence
heartbeats and bridge polling continue while it is open. PageUp/PageDown and
the wheel page the full question and detail; arrows or j/k navigate choices,
numbers and offered hotkeys select immediately, and Enter confirms. Escape
only dismisses. `/ask` reopens the same unanswered question. Line mode shows
the full context and accepts an offered value, its number, or an applicable
alias (y/s/a/n). Expired questions cannot send an answer.

Sending an answer is not proof that the host applied it. In particular, an
always-allow answer reports that host acknowledgment is still awaited. The
console displays authenticated bridge state messages, but does not write,
widen, or emulate native permission rules. The bridge remains responsible for
its native persistence semantics and for reporting a successful save.

The legacy ask signature binds question text and choices, not the separate
Tool, Detail, or Deadline metadata. Safety-relevant tool inputs should therefore
be included in the signed question text by the bridge. This Go change preserves
that wire contract; it does not claim the extra metadata is authenticated.

## Scoped mode and compatibility

`/mode` controls the selected conversation. Its nonempty value is only sent
when the verified host options advertise `mode`. Values are `auto`, `read-only`,
`accept-edits`, `bypass`, and `manual`; interpretation and enforcement belong to
the host's native adapter. `/mode default` clears the request. No native
configuration is changed. Explicit `/default mode VALUE` or the settings picker
can separately save a per-persona preselection in the owner's signed defaults.

Session metadata and each prompt carry `mode` when set. For prompt and session
signature labels, a nonempty mode adds `:<sha256(mode)>` after the model and
effort digests. A mode-only request still includes both empty model/effort
digests. An absent mode leaves the old label byte-for-byte unchanged. HMAC and
Ed25519 use the same labels. Resume restores the signed mode; sending to a
host that no longer advertises it fails closed rather than silently dropping it.
Legacy mode-free conversations remain compatible with the bundled bridge.

## Host-reported defaults

Unknown host model/effort values remain unknown, not inferred from this
computer or from a model catalog. Explicit conversation values are requests;
a bridge default is not a guarantee of a provider's eventual resolved model.

An optional per-harness `defaults` object reports model, effort, and mode.
Because the old options signature does not cover that object, a node card must
also provide `defaults_sig`. Its signing label is `NodeLabel(peer) + ":defaults"`.
Its content is compact JSON of an array in harness order; each entry has `id`
followed by `defaults` (null if absent). Default map keys are sorted, as in Go's
encoding/json. Example content:

    [{"id":"claude","defaults":{"effort":"high","mode":"manual","model":"host-model"}}]

The existing timestamp/signing function is used. Unsigned or altered defaults
are discarded without invalidating otherwise valid legacy options. This is an
optional Go-side extension requiring the host publisher to implement the same
signature contract. No host publisher or deployed plugin was changed here.
Refresh replaces the active harness advertisement, so stale options and default
values do not survive a refreshed card.

## Directory recall

Precedence for a new conversation is explicit directory, signed per-persona
pinned directory, most recently used verified successful directory for that
persona, then host default. Explicit/pinned selection does not depend on a
history lookup. `/cwd save` pins the current selection; defaults writes are
read back and verified before reporting that they were saved.

Recall requires an owner-signed session description, a matching owner-signed
prompt, and a successful final bridge-signed output tied to that prompt. A
queued prompt, a failed/refused run, a forged output, or another persona is
not evidence. Recall does not overwrite pins and does not assume a previously
valid remote directory still exists. The bridge validates it on use. `/cwd`
on a terminal offers prior successful directories plus explicit/pinned choices;
line mode retains `/cwd DIR`, `/cwd default`, `/cwd save`, and `!cd`.

## Verification

The in-memory permission tests use only loopback Honcho, virtual keyboard input,
and an in-memory terminal. They exercise single-key answers, reopen, full-detail
paging, expiry, heartbeat continuity, offered-choice enforcement, scoped mode,
signed resume, and verified recall. The integration tests use the bundled
Python bridge, temporary profiles, a fake local agent, and a loopback update URL,
not the owner's installed profile or a live fleet node.
