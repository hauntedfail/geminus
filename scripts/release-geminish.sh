#!/usr/bin/env bash
# Release geminish plugin: test, tag geminish-vX.Y.Z, push, and ask sibyl's
# geminus-latest-channel to build, sign, and publish the OTA bundle.
# Every host's geminish poller then auto-updates itself on its next OTA check.
#
#   bash scripts/release-geminish.sh 0.9.0
set -euo pipefail

version="${1:-}"
if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
	echo "usage: bash scripts/release-geminish.sh X.Y.Z" >&2
	exit 2
fi
tag="geminish-v${version}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

if [ -n "$(git status --porcelain --untracked-files=no -- plugin/geminish)" ]; then
	echo "release-geminish: there are uncommitted changes in plugin/geminish; commit them first" >&2
	exit 1
fi

if ! grep -q "version: ${version}" plugin/geminish/plugin.yaml; then
	echo "release-geminish: plugin/geminish/plugin.yaml does not declare version: ${version}" >&2
	exit 1
fi
if ! grep -q "__version__ = \"${version}\"" plugin/geminish/__init__.py; then
	echo "release-geminish: plugin/geminish/__init__.py does not declare __version__ = \"${version}\"" >&2
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
		echo "release-geminish: ${tag} is already on another commit (${at}); use the next version" >&2
		exit 1
	fi
done

python3 -m unittest discover -s plugin/geminish/tests -v

if [ -z "$tagged" ]; then
	git tag "$tag"
fi
if [ -z "$remote" ]; then
	git push origin "$tag"
fi

python3 plugin/geminish/cli.py release "$version"
