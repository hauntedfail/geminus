#!/usr/bin/env bash
# Build geminus for six targets as dist/geminus-<os>-<arch>[.exe], and the
# release server as dist/geminus-latest-channel-linux-amd64.
#
# These builds are for copying by hand and for trying things out. A release
# is not made here: the release server builds it from the tag
# (scripts/release.sh).
#
#   bash scripts/crossbuild.sh
#   VERSION=0.1.0 bash scripts/crossbuild.sh   # Set the version string
#
# Exit with a nonzero status on the first failure.
set -euo pipefail

readonly MODULE="github.com/hauntedfail/geminus"
readonly TARGETS="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# Use VERSION if set, otherwise the Git tag, falling back to dev.
# Without Git, a repository, or a tag, the fallback is dev.
# Check the repository root so we don't pick up a tag from a parent repository.
detect_version() {
	if [ -n "${VERSION:-}" ]; then
		printf '%s\n' "$VERSION"
		return
	fi
	local top described
	if command -v git >/dev/null 2>&1 &&
		top="$(git -C "$root" rev-parse --show-toplevel 2>/dev/null)" &&
		[ "$(cd "$top" && pwd -P)" = "$(pwd -P)" ] &&
		described="$(git -C "$root" describe --tags --dirty 2>/dev/null)" &&
		[ -n "$described" ]; then
		printf '%s\n' "${described#v}"
		return
	fi
	printf 'dev\n'
}

version="$(detect_version)"
# -ldflags splits on whitespace. Restrict version characters to keep other flags out.
case "$version" in
'' | *[!A-Za-z0-9._+-]*)
	echo "crossbuild: invalid version '${version}' (allowed: A-Z a-z 0-9 . _ + -)" >&2
	exit 2
	;;
esac
ldflags="-s -w -X ${MODULE}/internal/version.Version=${version}"

mkdir -p dist
# Remove previous builds so a failed build can't leave old binaries looking like new ones.
rm -f dist/geminus-*

echo "version: ${version}"
for target in $TARGETS; do
	goos="${target%/*}"
	goarch="${target#*/}"
	out="dist/geminus-${goos}-${goarch}"
	if [ "$goos" = "windows" ]; then
		out="${out}.exe"
	fi
	echo "build ${out}"
	# Build only ./cmd/geminus, not test/fakeagent.
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
		go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/geminus
done

# The release server runs on one computer (sibyl, Linux on amd64).
echo "build dist/geminus-latest-channel-linux-amd64"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -trimpath -ldflags "$ldflags" -o dist/geminus-latest-channel-linux-amd64 ./cmd/geminus-latest-channel

echo
ls -l dist/
