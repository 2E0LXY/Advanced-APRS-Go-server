#!/usr/bin/env sh
set -eu

VERSION="${VERSION:-dev}"
OUT_DIR="${OUT_DIR:-dist}"
mkdir -p "$OUT_DIR"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
  -o "$OUT_DIR/aprsnet-client-linux-amd64" ./cmd/aprsnet-client

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
  -o "$OUT_DIR/aprsnet-client-windows-amd64.exe" ./cmd/aprsnet-client

echo "Built APRSNET client binaries in $OUT_DIR"
