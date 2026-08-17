#!/usr/bin/env sh
set -eux

ROOT=$(cd "$(dirname "$0")" && pwd)
DIST=$ROOT/dist
TARGET=${TARGET_ARCH:-$(uname -m)}

case "$TARGET" in
  x86_64|amd64)
    GOARCH=amd64
    ARCH_SUFFIX=amd64
    ;;
  arm64|aarch64)
    GOARCH=arm64
    ARCH_SUFFIX=arm64
    ;;
  *)
    printf 'unsupported target arch %s\n' "$TARGET" >&2
    exit 1
    ;;
esac

RELEASE=${DIST}/mocker-linux-${ARCH_SUFFIX}-musl

mkdir -p "$DIST"

CGO_ENABLED=0 \
GOOS=linux \
GOARCH=$GOARCH \
GOMEMLIMIT=64MiB \
GODEBUG="madvdontneed=1" \
go build -trimpath -ldflags='-s -w' -o "$RELEASE" ./cmd/mocker

echo "Built musl binary at $RELEASE"
