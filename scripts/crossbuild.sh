#!/usr/bin/env bash
# geminus を6ターゲット向けにビルドし、dist/geminus-<os>-<arch>[.exe] を作る。
#
#   bash scripts/crossbuild.sh
#   VERSION=0.1.0 bash scripts/crossbuild.sh   # バージョン文字列を指定する
#
# 1つでも失敗したら、その時点で非0で終わる。
set -euo pipefail

readonly MODULE="github.com/hauntedfail/geminus"
readonly TARGETS="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# バージョンは VERSION、無ければ git のタグ、それも無ければ dev。
# git が無い、リポジトリでない、タグが無い、のどれでも dev になる。
# 親ディレクトリが別のリポジトリだった場合に、そのタグを拾わないよう最上位を確かめる。
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
# -ldflags は空白で分割される。バージョンに使える文字を限り、他のフラグが紛れ込まないようにする。
case "$version" in
'' | *[!A-Za-z0-9._+-]*)
	echo "crossbuild: invalid version '${version}' (allowed: A-Z a-z 0-9 . _ + -)" >&2
	exit 2
	;;
esac
ldflags="-s -w -X ${MODULE}/internal/version.Version=${version}"

mkdir -p dist
# 前回の成果物を残さない。途中で失敗したときに古いバイナリを新しいものと取り違えないため。
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
	# ビルドするのは ./cmd/geminus だけ（test/fakeagent は含めない）。
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
		go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/geminus
done

echo
ls -l dist/
