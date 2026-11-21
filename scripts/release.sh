#!/usr/bin/env bash
# Publish a geminus release: test, build every target, upload to the update
# server, tag. Every geminus then gets it with `geminus update` or /update.
#
#   bash scripts/release.sh 0.5.0
#
# The version is X.Y.Z and must be greater than the latest release. Run it on
# a computer of the group: the release is signed with the group secret of
# this computer's Hermes profile. GEMINUS_UPDATES_URL names another server
# than https://sibyl.haunted.fail/geminus/updates.
set -euo pipefail

version="${1:-}"
if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
	echo "usage: bash scripts/release.sh X.Y.Z" >&2
	exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# What is released is what is committed. Other untracked files are left
# alone, but an untracked Go file would be built into the release.
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
	echo "release: there are uncommitted changes; commit them first" >&2
	exit 1
fi
if [ -n "$(git ls-files --others --exclude-standard -- '*.go')" ]; then
	echo "release: there are Go files that are not committed; commit or remove them first" >&2
	exit 1
fi
if git rev-parse -q --verify "refs/tags/v${version}" >/dev/null ||
	[ -n "$(git ls-remote --tags origin "refs/tags/v${version}")" ]; then
	echo "release: v${version} is already tagged" >&2
	exit 1
fi

go vet ./...
go test ./... -count=1
VERSION="$version" bash scripts/crossbuild.sh

host="dist/geminus-$(go env GOOS)-$(go env GOARCH)"
if [ "$(go env GOOS)" = "windows" ]; then
	host="${host}.exe"
fi
"$host" release "$version" dist

# From here on the release is out, whatever happens to the tag.
git tag "v${version}"
if ! git push origin "v${version}"; then
	echo "release: ${version} is published, but the tag was not pushed; run: git push origin v${version}" >&2
	exit 1
fi
echo "released ${version}"
