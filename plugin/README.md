# geminish — Hermes plugin & geminus bridge

`geminish` is the resident Hermes plugin in this monorepo. It provides:

- **Agent-to-agent mail** (`geminise_send`, `geminise_inbox`) over a shared Honcho session (`geminise-main`), with a resident poller and optional Mozilla Web Push doorbell.
- **Owner-delegated cross-device execution** (`geminise_delegate`) gated by Ed25519 human authentication.
- **The `geminus` bridge**, which advertises each host's available agent programs and models in Honcho and executes signed prompts from the `geminus` CLI.
- **Automatic Ed25519-verified OTA updates** from `geminus-latest-channel` (`scripts/release-geminish.sh`).

## Layout

- `plugin/geminish/` — Runtime plugin package (archived verbatim by `geminus-latest-channel` on `geminish-vX.Y.Z` tags; excludes `tests/`, `__pycache__/`, and `geminise.json`).
- `plugin/geminish/tests/` — Standard-library `unittest` suite (run by `scripts/release-geminish.sh`).
- `plugin/tests/` — Full `pytest` suite covering mailbox policy, wake guards, and the push doorbell.
- `plugin/install.py` — Cross-platform installer and in-place upgrader.
- `deploy/ai.geminish.poller.plist` — macOS LaunchAgent template.
- `deploy/geminish-poller.service` — Linux systemd user unit.

## Tools

| Tool | Purpose |
|---|---|
| `geminise_send` | Record a signed message for one or more members or `@all`. Returns once stored; never waits for a reply. |
| `geminise_inbox` | Read mail addressed to this agent. `ref` + `wait_seconds` waits briefly for a reply. |
| `geminise_delegate` | Delegate an owner-authorized task to a peer agent and collect its result. |

Waiting mail is also surfaced at the start of the agent's next turn via the `pre_llm_call` hook.

## Install & upgrade

```sh
# Fresh install
python3 plugin/install.py --from <peer-name> --secret-file secret.txt
# Optional custom member map:
python3 plugin/install.py --from node1 --members '{"owner":"human","node1":"agent"}' --secret-file secret.txt

# Upgrade existing installation (keeps geminise.json)
python3 plugin/install.py --upgrade
```

After a fresh install, register the poller service for your OS and open a new Hermes session:

- **macOS**: edit `HOME_DIR` in `deploy/ai.geminish.poller.plist`, copy to `~/Library/LaunchAgents/`, and run `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/ai.geminish.poller.plist`.
- **Linux**: copy `deploy/geminish-poller.service` to `~/.config/systemd/user/` and run `systemctl --user daemon-reload && systemctl --user enable --now geminish-poller.service`.
- **Windows**:
  ```powershell
  schtasks /Create /TN "geminish-poller" /SC ONLOGON /RL LIMITED /F /TR "\"C:\path\to\pythonw.exe\" \"%LOCALAPPDATA%\hermes\plugins\geminish\cli.py\" poll"
  schtasks /Run /TN "geminish-poller"
  ```

After `--upgrade`, restart the poller service and start a new Hermes session.

## Configuration (`$HERMES_HOME/geminise.json`, mode `0600`)

| Key | Meaning |
|---|---|
| `from` | This host's own peer name |
| `members` | `{peer: "agent" | "human"}` shared across the group |
| `secret` | Group HMAC-SHA256 secret (hex, >= 32 bytes) |
| `human_pubkeys` | Optional `{human_peer: hex_pubkey | [hex_pubkey, ...]}` Ed25519 keys |
| `session` | Shared mailbox session (default `geminise-main`) |
| `poll_seconds` | Poll interval in seconds (default `10`) |
| `wake` | `false` disables waking new turns on this host (default `true`) |
| `max_wakes_per_hour` | Wake rate limit per sender (default `6`) |
| `wake_timeout` | Max seconds per woken turn (default `900`) |
| `push` | `false` disables the Mozilla Web Push doorbell (default `true`) |
| `geminus` | Bridge settings (`enabled`, `cwd`, `args`, `timeout`, `human_auth`) |
| `ota` | Automatic OTA settings (`enabled`, `base_url`, `interval_seconds`, `update_geminus`) |

Honcho connection settings (`baseUrl`, `workspace`, `apiKey`, `aiPeer`, `peerName`) are read from `$HERMES_HOME/honcho.json`.

## Operating & release

```sh
python3 plugin/geminish/cli.py status
python3 plugin/geminish/cli.py poll --once --foreground
python3 plugin/geminish/cli.py update
bash scripts/release-geminish.sh X.Y.Z
```

## Tests

```sh
# Standard-library tests (used by scripts/release-geminish.sh)
python3 -m unittest discover -s plugin/geminish/tests -v

# Full pytest suite (mailbox, policy, push doorbell)
GEMINISE_CONFIG=/nonexistent pytest -p no:cacheprovider plugin/tests -q
```
