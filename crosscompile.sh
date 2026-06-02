#!/usr/bin/env bash

set -eux

VARIANT="${1:-}"
if [ -z "$VARIANT" ]; then
    echo "Usage: $0 (x86_64|armv6|arm64)"
    exit 1
fi

VERSION="$(git tag --points-at HEAD 2>/dev/null || true)"
VERSION="${VERSION#v}"

case "$VARIANT" in
  x86_64) GOARCH="amd64"; GOARM="" ;;
  armv6)  GOARCH="arm";   GOARM="6" ;;
  arm64)  GOARCH="arm64"; GOARM="" ;;
  *) echo "Unsupported variant: $VARIANT"; exit 1 ;;
esac

CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" GOARM="$GOARM" \
  go build \
    -trimpath \
    -ldflags "-s -w -X github.com/devgianlu/go-librespot.version=$VERSION" \
    -o "./go-librespot-$VARIANT" \
    ./cmd/thing-daemon

file "./go-librespot-$VARIANT"
