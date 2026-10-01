#!/usr/bin/env bash
# The Go version lives in two places: the Nix flake (local + CI) and the
# Dockerfile's builder image (what ships). Fail if they drift (ADR-0001).
set -euo pipefail

shell_go=$(go env GOVERSION)                                   # e.g. go1.27.1
image_go=$(grep -oE '^FROM golang:[0-9]+\.[0-9]+\.[0-9]+' Dockerfile | head -1 | cut -d: -f2)

if [[ "go$image_go" != "$shell_go" ]]; then
  echo "Go version drift: devShell has $shell_go, Dockerfile builds with go$image_go" >&2
  exit 1
fi
echo "Go version OK: $shell_go in devShell and Dockerfile"
