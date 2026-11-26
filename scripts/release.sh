#!/usr/bin/env bash
# Release geminus: test, tag, have the release server build the tag, and
# publish the signed binaries to GitHub Releases.
# Every geminus then gets it with `geminus update` or /update, and GitHub
# Releases stays in sync with the exact binaries built by sibyl.
#
#   bash scripts/release.sh 0.5.0
#
# The version is X.Y.Z and must be greater than the latest release. Nothing
# is built or signed here: the server (sibyl) fetches the tag vX.Y.Z from the
# repository, builds every target and signs the release with a key only it
# holds. Run this on a computer of the group; the request to the server is
# signed with the group secret of this computer's Hermes profile.
#
# Optional environment variables:
#   GEMINUS_UPDATES_URL   update server URL (default: https://sibyl.haunted.fail/geminus/updates)
#   SKIP_GITHUB_RELEASE   set to 1 to skip publishing to GitHub Releases
#   RELEASE_NOTES         markdown body for the GitHub release
#   RELEASE_NOTES_FILE    path to a markdown file for the GitHub release body
#
# Run again after a failure that was not in the source (the server was
# restarted, it could not reach the repository, or GitHub release upload
# failed): a tag that is already on this commit is used as it is. If the
# source at the tag does not build, the release was never published, so
# either release the fix as the next version or move the tag: the script
# says how.
set -euo pipefail

version="${1:-}"
if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
	echo "usage: bash scripts/release.sh X.Y.Z" >&2
	exit 2
fi
tag="v${version}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

gh_publish=0
if [ "${SKIP_GITHUB_RELEASE:-0}" != "1" ]; then
	if ! command -v gh >/dev/null 2>&1; then
		echo "release: gh CLI is required for GitHub Releases (or set SKIP_GITHUB_RELEASE=1)" >&2
		exit 1
	fi
	if ! gh auth status >/dev/null 2>&1; then
		echo "release: gh is not authenticated; run 'gh auth login' (or set SKIP_GITHUB_RELEASE=1)" >&2
		exit 1
	fi
	if [ -n "${RELEASE_NOTES_FILE:-}" ] && [ ! -r "$RELEASE_NOTES_FILE" ]; then
		echo "release: RELEASE_NOTES_FILE is not readable: ${RELEASE_NOTES_FILE}" >&2
		exit 1
	fi
	gh_publish=1
fi

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

if [ "$gh_publish" != "1" ]; then
	go run ./cmd/geminus release "$version"
	exit 0
fi

if gh release view "$tag" >/dev/null 2>&1; then
	go run ./cmd/geminus release "$version"
	echo "release: GitHub release ${tag} already exists; skipping"
	exit 0
fi

tmp_root="${TMPDIR:-/tmp}"
assets_dir="$(mktemp -d "${tmp_root%/}/geminus-release.XXXXXX")"
trap 'rm -rf "$assets_dir"' EXIT

# Select the tag-push run, even when several tags point at the same commit.
run_id=""
for ((attempt=0; attempt<30; attempt++)); do
	run_id=$(gh run list --workflow release.yml --event push --commit "$head" \
		--json databaseId,displayTitle --jq ".[] | select(.displayTitle == \"Build $tag\") | .databaseId" | sed -n '1p')
	[ -n "$run_id" ] && break
	sleep 2
done
if [ -z "$run_id" ]; then
	echo "release: could not find the Release build run for $tag ($head)" >&2
	exit 1
fi
gh run watch "$run_id" --exit-status
stage="$assets_dir"
assets_dir="$stage/actions"
trap 'rm -rf "$stage"' EXIT
gh run download "$run_id" --name "geminus-$tag-$head" --dir "$assets_dir"
go run ./cmd/geminus release "$version" --download "$stage/ota"
# Compare the complete directory trees, including the checksum file.
if ! diff -qr "$stage/ota" "$assets_dir"; then
	echo "release: GitHub Actions and signed OTA artifacts differ for $tag" >&2
	exit 1
fi

notes_args=()
if [ -n "${RELEASE_NOTES_FILE:-}" ]; then
	notes_args=(--notes-file "$RELEASE_NOTES_FILE")
elif [ -n "${RELEASE_NOTES:-}" ]; then
	notes_args=(--notes "$RELEASE_NOTES")
else
	prev_tag="$(git tag --list 'v*' --merged "$tag" --sort=-v:refname | awk -v t="$tag" '$0 != t && !found { print; found=1 }')"
	notes_body=""
	if [ -n "$prev_tag" ]; then
		commits="$(git log --no-merges --format='- %s (%h)' "${prev_tag}..${tag}")"
		if [ -n "$commits" ]; then
			notes_body="$(printf 'Changes since %s:\n\n%s\n' "$prev_tag" "$commits")"
		fi
	fi
	if [ -n "$notes_body" ]; then
		notes_args=(--notes "$notes_body")
	else
		notes_args=(--generate-notes)
	fi
fi

shopt -s nullglob
bins=("$assets_dir"/geminus-*)
shopt -u nullglob
if [ "${#bins[@]}" -eq 0 ] || [ ! -f "$assets_dir/SHA256SUMS" ]; then
	echo "release: expected downloaded binaries and SHA256SUMS in ${assets_dir}" >&2
	exit 1
fi

gh release create "$tag" \
	--verify-tag \
	--title "geminus ${tag}" \
	"${notes_args[@]}" \
	"${bins[@]}" \
	"$assets_dir/SHA256SUMS"
