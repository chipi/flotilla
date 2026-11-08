#!/usr/bin/env bash
# Run go (or gofmt) in the pinned golang image on the dev Docker engine, for machines without Go.
#   scripts/go.sh test ./...      scripts/go.sh gofmt -l .      cd fleetd && ../scripts/go.sh vet ./...
#   make GO=scripts/go.sh GOFMT="scripts/go.sh gofmt" ci-fast
# The image is pinned by digest (ADR-0003); the module and build caches live in a named volume.
# It runs in the directory it was called from (which must be inside the repo). --init: without an
# init process nothing reaps orphans, and fleetd's process-group tests see zombies as survivors.
set -euo pipefail
IMG="golang@sha256:cfa27959837144bf4649802ba38e59daffd0dd2a7994aa83daaf113285b0f551"  # golang:1.26
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REL="${PWD#"$ROOT"}"
[ "$REL" != "$PWD" ] || { echo "go.sh: run it from inside $ROOT" >&2; exit 2; }
BIN=go
if [ "${1:-}" = gofmt ]; then BIN=gofmt; shift; fi
exec docker --context "${DOCKER_CONTEXT_DEV:-dev}" run --rm --user "$(id -u):$(id -g)" \
  -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e GOFLAGS="${GOFLAGS:-}" \
  -v flotilla-gocache:/cache -v "$ROOT":/src -w "/src$REL" --memory 2g --init "$IMG" "$BIN" "$@"
