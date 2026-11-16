#!/usr/bin/env bash
# Fill flotilla's knowledge volume from a host checkout (RFC-0002 §7, external knowledge of kind files).
#
#   knowledge.sh <image tag>
#
# Copies the declared paths of a git checkout, as they are at its HEAD commit (never the working tree),
# into the volume's folder for the source, and writes VERSION there with the commit. The volume changes
# only here, at deploy, so a run reads a version that stays fixed and recorded until the next deploy. The
# worker mounts the volume read-only at /knowledge; no host folder is ever mounted into a worker.
#
# Reads FLOTILLA_HOME/.env:
#   KNOWLEDGE_HOMELAB_FROM     the checkout on this host (the homelab repo); unset: nothing is copied
#   KNOWLEDGE_HOMELAB_PATHS    files or folders in it to copy, separated by spaces (default docs/recipes);
#                              the paths flotillad.yaml's knowledge section declares for the source
#   KNOWLEDGE_HOMELAB_SOURCE   the source's name in flotillad.yaml (default homelab-handbook)
#   KNOWLEDGE_VOLUME           the volume (default <FLOTILLA_VOLUME_PREFIX>-knowledge)
#
# The copy is made by a throwaway container of our own worker image, with no network: the bytes go over
# the docker CLI's stdin, as in secrets.sh. The folder is replaced whole, by a rename, so a run never sees
# half a copy. The volume is always created, so the compose stack can mount it before anything is copied.
set -euo pipefail
export PATH=$PATH:/usr/local/bin:/usr/bin:/bin   # appended: a caller's own docker stays first
H="${FLOTILLA_HOME:-$HOME/flotilla}"
TAG="${1:?usage: knowledge.sh sha-<7>}"
set -a; . "$H/.env"; set +a
IMG="${FLOTILLA_REGISTRY:-ghcr.io/chipi}/flotilla-worker-triage:$TAG"
D="docker ${DOCKER_CONTEXT:+--context $DOCKER_CONTEXT}"
VOL="${KNOWLEDGE_VOLUME:-${FLOTILLA_VOLUME_PREFIX:-flotilla}-knowledge}"
NAME="${KNOWLEDGE_HOMELAB_SOURCE:-homelab-handbook}"
FROM="${KNOWLEDGE_HOMELAB_FROM:-}"
PATHS="${KNOWLEDGE_HOMELAB_PATHS:-docs/recipes}"

fail() { echo "KNOWLEDGE FAIL: $*"; exit 1; }

$D volume create "$VOL" >/dev/null
if [ -z "$FROM" ]; then
  echo "KNOWLEDGE SKIP: KNOWLEDGE_HOMELAB_FROM is not set; $VOL keeps what it holds"
  exit 0
fi
[[ "$NAME" =~ ^[a-z][a-z0-9-]{0,62}$ ]] || fail "source name '$NAME': lower-case letters, digits and dashes"
git -C "$FROM" rev-parse --git-dir >/dev/null 2>&1 || fail "$FROM is not a git checkout"
COMMIT="$(git -C "$FROM" rev-parse --verify HEAD)" || fail "$FROM has no commit"

# the same rule as flotillad's -check applies to the paths in the config: relative, clean, no glob
for p in $PATHS; do
  [[ "$p" =~ ^[A-Za-z0-9._/-]+$ ]] && [[ "$p" != /* && "$p" != . && "$p" != .. && "$p" != ../* && "$p" != */.. && "$p" != */../* && "$p" != */ && "$p" != *//* ]] \
    || fail "path '$p': relative and clean, letters, digits and . _ - / only"
  git -C "$FROM" cat-file -e "HEAD:$p" 2>/dev/null || fail "path '$p' is not in $FROM at ${COMMIT:0:12}"
done
# shellcheck disable=SC2086
if [ -n "$(git -C "$FROM" status --porcelain -- $PATHS)" ]; then
  echo "  note: $FROM has uncommitted changes under the paths; the copy is HEAD, not the working tree"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# shellcheck disable=SC2086
git -C "$FROM" archive --format=tar -o "$TMP/k.tar" HEAD -- $PATHS || fail "git archive of $FROM failed"
[ "$(tar -tf "$TMP/k.tar" | wc -l)" -gt 0 ] || fail "the paths hold no file at ${COMMIT:0:12}"

# relative to /k (the volume): the new folder is built beside the old one and renamed over it
$D run --rm -i --network none --user 0 --entrypoint sh -v "$VOL:/k" -w /k -e KN_NAME="$NAME" -e KN_COMMIT="$COMMIT" "$IMG" \
  -c 'set -e
      rm -rf "$KN_NAME.new" "$KN_NAME.old"; mkdir "$KN_NAME.new"
      tar -xf - -C "$KN_NAME.new" --no-same-owner
      printf "%s\n" "$KN_COMMIT" > "$KN_NAME.new/VERSION"
      chmod -R go-w,a+rX "$KN_NAME.new"
      if [ -d "$KN_NAME" ]; then mv "$KN_NAME" "$KN_NAME.old"; fi
      mv "$KN_NAME.new" "$KN_NAME"; rm -rf "$KN_NAME.old"
      echo "  $(find "$KN_NAME" -type f | wc -l) files, $(du -sh "$KN_NAME" | cut -f1)"' < "$TMP/k.tar" \
  || fail "the copy into $VOL failed"
echo "  $VOL/$NAME <- $FROM @ ${COMMIT:0:12} ($PATHS)"
echo "KNOWLEDGE PASS"
