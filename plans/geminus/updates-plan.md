# geminus updates: implementation plan

**Goal:** `geminus update` / `/update` fetch the latest release from `https://sibyl.haunted.fail/geminus/updates` and switch to it in place; `scripts/release.sh X.Y.Z` publishes one.

**Architecture:** `internal/updates` holds what both sides share (versions, the signed manifest, signed requests) plus the HTTP client and the code that replaces the program. `internal/updates/server` is the file service as an `http.Handler`; `cmd/geminus-latest-channel` wraps it. `internal/chat` gains the commands and the notice.

**Tech stack:** Go 1.25 standard library only (`net/http`, `os.Root`, `log/slog`).

**Spec:** `plans/geminus/updates-design.md`

## Global constraints

- No module outside the standard library. Text in the repository is English.
- Base URL `https://sibyl.haunted.fail/geminus/updates`; `GEMINUS_UPDATES_URL` replaces it. Nothing derived from `honcho.haunted.fail`.
- Versions on the server are `X.Y.Z`, digits only. Targets are `<os>-<arch>`.
- Labels: `geminus-updates-request:<METHOD>:<path>:<sha256 hex of body>` and `geminus-release:<version>`, signed with `wire.Sign`.
- Request headers `Geminus-Time`, `Geminus-Body-SHA256` and `Geminus-Signature`; accepted within 300 s.
- Limits: 32 MiB per program, 64 KiB per manifest.
- Server program and unit: `geminus-latest-channel`. Default listen address `127.0.0.1:8651`.
- geminus writes no file other than its own program (and `<program>.old` on Windows).

## Review focus

1. The download is cut short or the server answers with an error page: the program on disk must stay the old one, and no temporary file is left.
2. The program's directory is not writable: the error names the path; nothing is half replaced.
3. A request path with `..`, an encoded slash, or a version such as `latest`: refused, nothing read or written outside the directory.
4. The clock of a computer is off by more than five minutes: the message says so instead of "forbidden".
5. `/update` while an answer is still arriving: the conversation reopens and the answer is shown.

## Tasks

### 1. Shared records (`internal/updates`)

Files: `internal/updates/version.go`, `manifest.go`, `request.go` and tests; `go.mod` to `go 1.25`.

- `ParseVersion(s string) (Version, bool)`: accepts `X.Y.Z` with any suffix after `-` or `+`; `Version{Major, Minor, Patch int; Exact bool}`. `(v Version) Less(o Version) bool`: numeric; with equal numbers a version that is not exact is less than an exact one. `IsRelease(s string) bool`: digits only.
- `Target(goos, goarch string) string`; `ValidTarget(s string) bool` (`^[a-z0-9]+-[a-z0-9]+$`).
- `type File struct{SHA256 string; Size int64}`; `type Manifest struct{Version string; TS int64; Files map[string]File; Sig string}` with JSON names `version`, `ts`, `files`, `sha256`, `size`, `sig`.
- `Seal(secret []byte, version string, files map[string]File, ts int64) Manifest`; `(m Manifest) Verify(secret []byte) bool` (also false for a version that is not a release, a bad target, a hash that is not 64 hex digits, a negative size).
- `SignRequest(req *http.Request, secret []byte, bodySHA string, now time.Time)`; `VerifyRequest(req *http.Request, secret []byte, bodySHA string, now time.Time) error` with distinct errors `ErrUnsigned`, `ErrClock`, `ErrSignature`. The signed path is `req.URL.EscapedPath()`.

Tests: order of versions (`0.4.0-dev < 0.4.0 < 0.4.1 < 0.10.0`), a manifest changed in any field fails, a signature for one path or method fails for another, a `ts` 301 s off is `ErrClock`.

### 2. Server (`internal/updates/server`, `cmd/geminus-latest-channel`)

- `server.New(root *os.Root, secret []byte, now func() time.Time, log *slog.Logger) http.Handler` with the five routes of the spec under `/geminus/updates/`.
- Upload: body through `http.MaxBytesReader`, streamed into `.tmp-*` in the root while hashed; signature checked against the hash after reading; renamed into `<version>/<target>`. Same bytes again: 200. Different bytes, or a version already published: 409.
- Manifest `PUT`: verified, version greater than `latest`, every file present with the stated size and hash; then `manifest.json` and `latest` are written by rename.
- Reads: signature first (401 with a one-line reason; `ErrClock` says the clocks differ), then `http.ServeContent`.
- `cmd/geminus-latest-channel`: flags `-listen`, `-dir`; secret from `profile.Load()`; `http.Server` with timeouts; stops on SIGINT/SIGTERM.

Tests (`httptest`): each route; unsigned, wrong secret, stale time; `..`, `%2f`, `latest` as a version, an unknown target; oversize; overwrite refused; manifest listing a missing or different file refused; older version refused.

### 3. Client and replacing the program (`internal/updates`)

- `type Client struct{Base string; Secret []byte; HTTP *http.Client; Now func() time.Time}`.
- `(c *Client) Latest(ctx) (Manifest, error)`: verified manifest; `ErrNoRelease` on 404.
- `(c *Client) Fetch(ctx, m Manifest, target string, w io.Writer) error`: checks size and hash while copying; any mismatch is an error.
- `(c *Client) Publish(ctx, version string, files map[string]string) (Manifest, error)`: target → local path; uploads each, then the sealed manifest.
- `Replace(exe string, goos string, write func(io.Writer) error) error`: writes `<exe>.new` beside the program (mode 0755), then renames; on `windows` first renames the program to `<exe>.old`. On any failure the program is as before and `.new` is gone.
- `Sweep(exe string)`: removes `<exe>.old`, silently.
- `DefaultBase` and `Base(getenv func(string) string) string`.

Tests: against the server of task 2; a body cut short and a wrong hash leave the program untouched and no `.new`; the Windows rule on a temporary directory; a directory without write permission (skipped as root and on Windows).

### 4. geminus (`internal/chat`)

- `geminus update`: `request.update`; prints `already the latest (X)` or `geminus A → B`; exit 1 with the reason otherwise. No Honcho call.
- `geminus release VERSION DIR` (not in the usage text): publishes `DIR/geminus-<os>-<arch>[.exe]`.
- `/update`: same as `update`, then restart. `App.Restart func(exe string, args []string) error` is replaceable for tests; the default is `restart` in `restart_unix.go` (`syscall.Exec`) and `restart_windows.go` (start on the same console, wait, exit with its code). The terminal is put back first. Arguments: `persona <name> --resume <id>`, or `persona <name> --harness <h>` when no conversation is open.
- Notice: one `Latest` in the background at start (2 s), result kept in `App.newer`; shown in the status under the box and in `/status` as `update X · /update`. Silent on any error.
- `Sweep` at start.

Tests: the commands against the server of task 2 with a stand-in program file; the restart arguments; the notice on the terminal emulator; nothing shown when the server is away.

### 5. Release script, unit, end-to-end test, documents

- `scripts/release.sh X.Y.Z`: refuses a dirty tree or an existing tag, `go vet`, `go test`, `VERSION=X.Y.Z scripts/crossbuild.sh`, `dist/geminus-<host> release X.Y.Z dist`, `git tag vX.Y.Z`. `scripts/crossbuild.sh` also builds `dist/geminus-latest-channel-linux-amd64`, and `geminus release` ignores it.
- `deploy/geminus-latest-channel.service` (user unit) and `deploy/README.md` with the tunnel rule.
- `test/updates_test.go`: built server and built geminus at version 0.0.1; publish 0.0.2 built from the same source; `geminus update` on a copy; the copy prints 0.0.2.
- `README.md`, `docs/architecture.md`, `docs/protocol.md`.

### 6. Putting it on sibyl (needs the owner)

Copy the server and unit, start it, add the tunnel rule, restart cloudflared, publish the first release, check `geminus update` on chrome.
