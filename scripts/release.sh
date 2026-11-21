#!/usr/bin/env bash
# Release geminus: test, tag, and have the release server build the tag.
# Every geminus then gets it with `geminus update` or /update.
#
#   bash scripts/release.sh 0.5.0
#
# The version is X.Y.Z and must be greater than the latest release. Nothing
# is built or signed here: the server (sibyl) fetches the tag vX.Y.Z from the
# repository, builds every target and signs the release with a key only it
# holds. Run this on a computer of the group; the request to the server is
# signed with the group secret of this computer's Hermes profile.
# GEMINUS_UPDATES_URL names another server than
# https://sibyl.haunted.fail/geminus/updates.
#
# Run again after a failure that was not in the source (the server was
# restarted, it could not reach the repository): a tag that is already on
# this commit is used as it is. If the source at the tag does not build, the
# release was never published, so either release the fix as the next version
# or move the tag: the script says how.
set -euo pipefail

version="${1:-}"
if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
	echo "usage: bash scripts/release.sh X.Y.Z" >&2
	exit 2
fi
tag="v${version}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# What is released is what is committed and tagged. Other untracked files are
# left alone, but an untracked Go file would make the tests here differ from
# the build on the server.
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
	echo "release: there are uncommitted changes; commit them first" >&2
	exit 1
fi
if [ -n "$(git ls-files --others --exclude-standard -- '*.go')" ]; then
	echo "release: there are Go files that are not committed; commit or remove them first" >&2
	exit 1
fi

head="$(git rev-parse HEAD)"
remote="$(git ls-remote --tags origin "refs/tags/${tag}" "refs/tags/${tag}^{}" | tail -n 1 | cut -f 1)"
tagged=""
if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
	tagged="$(git rev-parse "refs/tags/${tag}^{commit}")"
fi
for at in "$tagged" "$remote"; do
	if [ -n "$at" ] && [ "$at" != "$head" ]; then
		cat >&2 <<-EOM
		release: ${tag} is already on another commit (${at})
		release: if ${version} was never published (its build failed), remove the tag and run this again:
		release:   git tag -d ${tag}; git push origin :refs/tags/${tag}
		release: a published release cannot be changed; use the next version instead
		EOM
		exit 1
	fi
done

go vet ./...
go test ./... -count=1

if [ -z "$tagged" ]; then
	git tag "$tag"
fi
if [ -z "$remote" ]; then
	git push origin "$tag"
fi

go run ./cmd/geminus release "$version"
