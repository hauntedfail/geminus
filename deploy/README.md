# Putting the release server on sibyl

Done once. After it, a release needs nothing on sibyl: `scripts/release.sh` pushes a tag and asks the server over HTTPS to build it.

The server reads the group secret from the Hermes profile of the user it runs as (`~/.hermes/geminise.json`, with `honcho.json` beside it), keeps releases in `~/.local/share/geminus/updates`, and listens on `127.0.0.1:8651` only. It builds a release from the tag `vX.Y.Z` of the source repository and signs it with a key that exists on sibyl only.

## 1. Go

No root needed. Use the Go version the source is developed with, and check the archive against the SHA-256 on <https://go.dev/dl/>:

```sh
curl -fsSLO https://go.dev/dl/go1.27.4.linux-amd64.tar.gz
sha256sum go1.27.4.linux-amd64.tar.gz
tar -C ~/.local -xzf go1.27.4.linux-amd64.tar.gz    # ~/.local/go/bin/go
```

The server builds with `GOTOOLCHAIN=local` and `GOPROXY=off`: the source cannot make it download another toolchain or a module. When `go.mod` asks for a newer Go, install that Go here first.

## 2. Read access to the source

A deploy key that can read the geminus repository and nothing else:

```sh
ssh-keygen -t ed25519 -N "" -C "sibyl: geminus-latest-channel (read-only)" -f ~/.ssh/id_ed25519_geminus_deploy
cat >> ~/.ssh/config <<'EOC'
Host github-geminus
  HostName github.com
  User git
  IdentityFile ~/.ssh/id_ed25519_geminus_deploy
  IdentitiesOnly yes
  BatchMode yes
  ConnectTimeout 15
EOC
chmod 600 ~/.ssh/config
```

Register the public half on GitHub without write access (on a computer with `gh`):

```sh
gh repo deploy-key add id_ed25519_geminus_deploy.pub --repo hauntedfail/geminus --title "sibyl: geminus-latest-channel (read-only)"
```

Check on sibyl: `git ls-remote --tags git@github-geminus:hauntedfail/geminus.git` lists the tags without asking anything.

## 3. The program, its key and its unit

On the build computer:

```sh
bash scripts/crossbuild.sh
scp dist/geminus-latest-channel-linux-amd64 btcvps:.local/bin/geminus-latest-channel
scp deploy/geminus-latest-channel.service btcvps:.config/systemd/user/
```

On sibyl:

```sh
chmod 755 ~/.local/bin/geminus-latest-channel
geminus-latest-channel -keygen    # makes ~/.config/geminus/release.key (0600) if there is none; prints the public key
systemctl --user daemon-reload
systemctl --user enable --now geminus-latest-channel
journalctl --user -u geminus-latest-channel -n 5    # "listening address=127.0.0.1:8651 ... key=<public key>"
```

The public key printed there is the value of `releaseKey` in `internal/updates/key.go`. Every geminus trusts that key and no other, so:

- `release.key` never leaves sibyl and is never made again. `-keygen` does not replace a key that exists.
- If the key is lost, no geminus in use accepts another release. Each computer then needs one copy by hand of a geminus built with the new public key.

## 4. The tunnel

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

## 5. Check

From any computer of the group, before the first release:

```sh
geminus update     # geminus: update: no release has been published
```

That answer means the request was signed, reached the server, and the server has nothing yet. `401` means the two computers hold different group secrets or their clocks differ by more than five minutes.

## A new version of the server itself

Copy the new program over the old one and `systemctl --user restart geminus-latest-channel`. Releases on disk and the key are kept. A build that was running is stopped; run `scripts/release.sh` again for it.

## When a release fails to build

`scripts/release.sh` prints the end of what git or the compiler said. The whole of it is in `journalctl --user -u geminus-latest-channel`. Nothing of a failed build is served; fix the cause and run the script again.
