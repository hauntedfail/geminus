#!/usr/bin/env bash
# Run go vet and go test in a Linux container.
# Check Linux PTY behavior (such as termination with EIO) from macOS or Windows.
#
#   bash scripts/test-linux.sh
#   bash scripts/test-linux.sh -run TestRun -v      # Pass arguments straight to go test
#   GO_IMAGE=golang:1.27 bash scripts/test-linux.sh  # Use a different image
#
# Requires Docker. Modules come from the host cache (GOPROXY=off),
# so run go build ./... successfully on the host first.
set -euo pipefail

readonly IMAGE="${GO_IMAGE:-golang:1.27}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
modcache="$(go env GOMODCACHE)"

# Mount the source and module cache read-only so the container can't modify them.
# Run as a non-root user so permission tests (0600 modes and unreadable files) are meaningful.
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
