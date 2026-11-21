# Putting the update server on sibyl

Done once. After it, a release needs nothing on sibyl: `scripts/release.sh` uploads over HTTPS.

The server reads the group secret from the Hermes profile of the user it runs as (`~/.hermes/geminise.json`, with `honcho.json` beside it), keeps releases in `~/.local/share/geminus/updates`, and listens on `127.0.0.1:8651` only.

## 1. The program and its unit

On the build computer:

```sh
bash scripts/crossbuild.sh
scp dist/geminus-latest-channel-linux-amd64 btcvps:.local/bin/geminus-latest-channel
scp deploy/geminus-latest-channel.service btcvps:.config/systemd/user/
```

On sibyl:

```sh
chmod 755 ~/.local/bin/geminus-latest-channel
systemctl --user daemon-reload
systemctl --user enable --now geminus-latest-channel
journalctl --user -u geminus-latest-channel -n 5    # "listening address=127.0.0.1:8651"
```

## 2. The tunnel

In `~/.cloudflared/config.yml`, above the existing `sibyl.haunted.fail` rule (rules are tried in order):

```yaml
  - hostname: sibyl.haunted.fail
    path: ^/geminus/updates(/.*)?$
    service: http://127.0.0.1:8651
```

Check which rule a URL meets, then restart the tunnel (a system unit; Honcho is unreachable for a few seconds):

```sh
cloudflared tunnel --config ~/.cloudflared/config.yml ingress validate
cloudflared tunnel --config ~/.cloudflared/config.yml ingress rule https://sibyl.haunted.fail/geminus/updates/latest
sudo systemctl restart cloudflared-honcho
```

## 3. Check

From any computer of the group, before the first release:

```sh
geminus update     # geminus: update: no release has been published
```

That answer means the request was signed, reached the server, and the server has nothing yet. `401` means the two computers hold different group secrets or their clocks differ by more than five minutes.

## A new version of the server itself

Copy the new program over the old one and `systemctl --user restart geminus-latest-channel`. Releases on disk are kept.
