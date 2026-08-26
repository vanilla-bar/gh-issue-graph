#!/usr/bin/env bash
# Cross-compiles the release binaries for gh-extension-precompile.
set -euo pipefail

version="${1:?usage: build-release.sh <version>}"
platforms=(
  darwin-amd64 darwin-arm64
  freebsd-386 freebsd-amd64 freebsd-arm64
  linux-386 linux-amd64 linux-arm linux-arm64
  windows-386 windows-amd64 windows-arm64
)

mkdir -p dist
for platform in "${platforms[@]}"; do
  goos="${platform%%-*}"
  goarch="${platform##*-}"
  output="dist/${platform}"
  if [ "$goos" = "windows" ]; then
    output="${output}.exe"
  fi
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w -X main.version=${version}" -o "$output" ./cmd/gh-issue-graph
done
