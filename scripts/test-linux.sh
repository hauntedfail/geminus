#!/usr/bin/env bash
# Linux のコンテナで go vet と go test を走らせる。
# macOS / Windows の開発機から、Linux の PTY 経路（EIO での終端など）を確かめるために使う。
#
#   bash scripts/test-linux.sh
#   bash scripts/test-linux.sh -run TestRun -v      # 引数は go test にそのまま渡す
#   GO_IMAGE=golang:1.27 bash scripts/test-linux.sh  # イメージを変える
#
# docker が要る。モジュールはホストのキャッシュから読む（GOPROXY=off）ので、
# 先にホストで一度 go build ./... を通しておく。
set -euo pipefail

readonly IMAGE="${GO_IMAGE:-golang:1.27}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
modcache="$(go env GOMODCACHE)"

# ソースとモジュールキャッシュは読み取り専用で渡し、コンテナからホストを書き換えさせない。
# root では権限のテスト（0600 / 読めないファイル）の意味が変わるので、一般利用者で実行する。
docker run --rm \
	-u 1000:1000 \
	-e HOME=/tmp/home \
	-e GOCACHE=/tmp/gocache \
	-e GOPROXY=off \
	-e GOFLAGS=-buildvcs=false \
	-e CGO_ENABLED=0 \
	-v "$root":/src:ro \
	-v "$modcache":/go/pkg/mod:ro \
	-w /src \
	"$IMAGE" \
	sh -c 'mkdir -p "$HOME" && uname -srm && go version && go vet ./... && go test ./... -count=1 "$@"' sh "$@"
