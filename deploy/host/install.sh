#!/usr/bin/env bash
# Install, upgrade or remove the host agent on one host over SSH (ADR-0006 §9, v1: the operator's
# build; SSH gives the integrity, and the builder is the person with root on the host).
#
#   deploy/host/install.sh install SSH_TARGET --ref REF --os darwin|linux --arch amd64|arm64 \
#       --config FILE --root FILE --addr HOST:PORT --server-name NAME --token-file FILE [--heartbeat 30s]
#   deploy/host/install.sh upgrade SSH_TARGET --ref REF --os ... --arch ... [--config FILE]
#   deploy/host/install.sh remove  SSH_TARGET
#
# --ref: the commit or tag the binary is built from (in a temporary worktree, with the pinned golang
# image on the dev Docker engine). --config: this host's allowlist (internal/hostcfg). --root: the root
# CA's certificate. --token-file: the enrolment token (`flotilla host enrolment NAME`), mode 600; it
# travels in the staging folder, which is deleted on both ends when this ends, and the host deletes the
# token as soon as enrolment has used it. The remote step runs under sudo with a terminal, so sudo can
# ask for a password. Revoking a host needs no access to it: `flotilla host revoke NAME`.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMG="golang@sha256:cfa27959837144bf4649802ba38e59daffd0dd2a7994aa83daaf113285b0f551"  # golang:1.26, as scripts/go.sh
D="docker --context ${DOCKER_CONTEXT_DEV:-dev}"

usage() { sed -n '2,15p' "$0" >&2; exit 2; }
[ $# -ge 2 ] || usage
CMD=$1 TARGET=$2; shift 2
REF='' GOOS='' GOARCH='' CONFIG='' ROOTCRT='' ADDR='' NAME='' TOKEN='' HEARTBEAT=30s
while [ $# -gt 0 ]; do
  case $1 in
    --ref) REF=$2 ;; --os) GOOS=$2 ;; --arch) GOARCH=$2 ;; --config) CONFIG=$2 ;; --root) ROOTCRT=$2 ;;
    --addr) ADDR=$2 ;; --server-name) NAME=$2 ;; --token-file) TOKEN=$2 ;; --heartbeat) HEARTBEAT=$2 ;;
    *) usage ;;
  esac
  shift 2
done

STAGE="$(mktemp -d)"; REMOTE=''
cleanup() {
  rm -rf "$STAGE"
  [ -z "$REMOTE" ] || ssh "$TARGET" "rm -rf '$REMOTE'" || echo "WARN: could not remove $TARGET:$REMOTE"
}
trap cleanup EXIT
chmod 700 "$STAGE"

build() {  # the binary for the host, from REF
  [ -n "$REF" ] && [ -n "$GOOS" ] && [ -n "$GOARCH" ] || usage
  local wt rev
  wt="$(mktemp -d)"
  git -C "$ROOT" worktree add --detach "$wt" "$REF" >/dev/null 2>&1 || { rm -rf "$wt"; echo "INSTALL FAIL: no such ref $REF"; exit 1; }
  rev="$(git -C "$wt" rev-parse --short=7 HEAD)"
  echo "== build flotilla-host sha-$rev for $GOOS/$GOARCH"
  chmod 755 "$wt"
  if ! $D run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod \
      -e GOOS="$GOOS" -e GOARCH="$GOARCH" -e CGO_ENABLED=0 -v flotilla-gocache:/cache -v "$wt":/src -w /src --memory 2g --init "$IMG" \
      go build -trimpath -ldflags "-s -w -X main.version=sha-$rev" -o /src/flotilla-host.out ./cmd/flotilla-host; then
    git -C "$ROOT" worktree remove --force "$wt"; echo "INSTALL FAIL: build"; exit 1
  fi
  mv "$wt/flotilla-host.out" "$STAGE/flotilla-host"
  git -C "$ROOT" worktree remove --force "$wt"
}

stage_common() {
  cp "$ROOT/deploy/host/remote-install.sh" "$STAGE/"
  if [ -n "$CONFIG" ]; then cp "$CONFIG" "$STAGE/config.json"; fi
}

case "$CMD" in
install)
  [ -n "$CONFIG" ] && [ -n "$ROOTCRT" ] && [ -n "$ADDR" ] && [ -n "$NAME" ] && [ -n "$TOKEN" ] || usage
  [ -s "$TOKEN" ] || { echo "INSTALL FAIL: $TOKEN is empty"; exit 1; }
  build; stage_common
  cp "$ROOTCRT" "$STAGE/root.crt"
  printf 'FLOTILLA_ADDR=%s\nFLOTILLA_SERVER_NAME=%s\nFLOTILLA_HEARTBEAT=%s\n' "$ADDR" "$NAME" "$HEARTBEAT" > "$STAGE/agent.env"
  (umask 077 && cp "$TOKEN" "$STAGE/token")
  ;;
upgrade) build; stage_common ;;
remove) stage_common ;;
*) usage ;;
esac

REMOTE="$(ssh "$TARGET" 'mktemp -d /tmp/flotilla-stage.XXXXXX')"
scp -q -p "$STAGE"/* "$TARGET:$REMOTE/"
echo "== $CMD on $TARGET"
ssh -t "$TARGET" "sudo /bin/bash '$REMOTE/remote-install.sh' '$REMOTE' $CMD"
