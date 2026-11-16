#!/usr/bin/env bash
# Deploy flotilla on the homelab mini: one image tag, checked before and after, rolled back on red.
#
#   deploy.sh sha-<7>          deploy that tag (the images CI pushed for that commit)
#   deploy.sh --rollback       go back to the tag deployed before the current one
#
# Runs as the operator account (the production Docker engine is that account's; homelab ADR-0009).
# Agents run it over SSH, like the homelab's other stacks (ADR-0002).
#
# Layout under FLOTILLA_HOME (default ~/flotilla), none of it in git:
#   repo/            a checkout of github.com/chipi/flotilla (the compose file comes from the commit
#                    of the tag being deployed)
#   .env             the installation: FLOTILLA_CONFIG, TRIAGE_ENV_FILE, TRIAGE_STATE_VOLUME
#   config/          flotillad.yaml, callers.yaml, cli.token, worker-triage.token, worker-triage.env,
#                    smokescreen.yaml, egress-acl.yaml (deploy/compose/README.md)
# State and secrets live in Docker volumes (the compose file); secrets.sh refills the secret volumes
# from the host's files on every deploy.
#   deployed.tsv     every deploy: time, tag, commit, result (the previous tag is read from here)
#
# The triage fleet's hooks (triage_hooks.sh, loaded from this checkout before it moves): a deploy runs the
# filing gate against the commit deployed now, and a deploy or rollback appends the fleet's deploy log.
#
# The store (flotilla.db in the state volume) is snapshotted before every `up`, rollback included, by a
# throwaway container of the worker image with no network (store_snapshot.py): SQLite's online backup,
# into FLOTILLA_HOME/backups/<tag>-<UTC time>/ with the schema number in SCHEMA; the newest
# FLOTILLA_BACKUPS_KEEP (default 10) are kept. An older flotillad refuses a newer store, so a --rollback
# to a tag that does restores the newest snapshot that tag accepts (flotillad stopped first), or stops
# with nothing changed when there is none. Rows written after that snapshot are lost.
#
# Ends with DEPLOY PASS or DEPLOY FAIL; a deploy and a rollback each write an ops_event/v1 line to
# VictoriaLogs (event_type deploy, service flotilla).
set -uo pipefail
export PATH=$PATH:/usr/local/bin:/usr/bin:/bin   # appended: an SSH session's PATH lacks /usr/local/bin (docker)
H="${FLOTILLA_HOME:-$HOME/flotilla}"
REPO="$H/repo"
LOG="$H/deployed.tsv"
VLOGS="${VLOGS:-http://127.0.0.1:9428}"
say() { printf '%s\n' "$*"; }
# store_snapshot.py is read now: a rollback moves the checkout to an older commit that may not have it
STORE_HELPER="$(cat "$REPO/deploy/mini/store_snapshot.py" 2>/dev/null)"
BACKUPS="$H/backups"
KEEP="${FLOTILLA_BACKUPS_KEEP:-10}"
HEALTH_TRIES="${FLOTILLA_HEALTH_TRIES:-30}"
HEALTH_SLEEP="${FLOTILLA_HEALTH_SLEEP:-2}"
die() { say "DEPLOY FAIL: $*"; event failure "${TAG:-?}" "$*"; exit 1; }

event() {  # status tag message
  local line
  line=$(printf '{"_time":"%s","schema":"ops_event/v1","event_type":"deploy","service":"flotilla","environment":"operations","app":"flotilla","env":"operations","status":"%s","release":"%s","sha":"%s","host":"%s","_msg":"deploy %s flotilla %s: %s"}' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" "${2#sha-}" "$(hostname -s)" "$1" "$2" "${3//\"/\'}")
  curl -s -m 10 -o /dev/null -X POST -H 'Content-Type: application/stream+json' \
    "$VLOGS/insert/jsonline?_stream_fields=app,env,service,environment,event_type&_time_field=_time&_msg_field=_msg" \
    --data-binary "$line" || say "(deploy event not written: VictoriaLogs unreachable)"
}

[ -f "$H/.env" ] || die "$H/.env missing (deploy/compose/README.md)"
[ -d "$REPO/.git" ] || die "$REPO is not a checkout of chipi/flotilla"
[ -n "$STORE_HELPER" ] || die "$REPO/deploy/mini/store_snapshot.py missing: check out a commit that has it, then run this again"

previous() { awk -F'\t' '$4=="pass"{t=$2} END{print t}' "$LOG" 2>/dev/null; }
CURRENT="$(previous)"
CURRENT_COMMIT="$(awk -F'\t' '$4=="pass"{c=$3} END{print c}' "$LOG" 2>/dev/null)"
HOOKS="$REPO/deploy/mini/triage_hooks.sh"
[ -f "$HOOKS" ] || die "$HOOKS missing"
# shellcheck source=triage_hooks.sh
. "$HOOKS"
envget() { (set -a; . "$H/.env" >/dev/null 2>&1; eval "printf '%s' \"\${$1:-}\""); }
STATE_VOL="$(envget FLOTILLA_STATE_VOLUME)"; STATE_VOL="${STATE_VOL:-flotilla-state}"   # as the compose file names it
REGISTRY="$(envget FLOTILLA_REGISTRY)"; REGISTRY="${REGISTRY:-ghcr.io/chipi}"
TRIAGE_DEPLOY_LOG="$(envget TRIAGE_DEPLOY_LOG)"
TRIAGE_FILING_GATE="$(envget TRIAGE_FILING_GATE)"
EVENT=deploy
if [ "${1:-}" = --rollback ]; then
  TAG="$(awk -F'\t' '$4=="pass"{p=c; c=$2} END{print p}' "$LOG" 2>/dev/null)"
  [ -n "$TAG" ] || die "no earlier passing deploy to roll back to"
  say "rollback: $CURRENT -> $TAG"
  EVENT=rollback
else
  TAG="${1:-}"
fi
[[ "$TAG" =~ ^sha-[0-9a-f]{7}$ ]] || die "usage: deploy.sh sha-<7> | --rollback"

compose() {
  (cd "$REPO" && set -a && . "$H/.env" && set +a && FLOTILLA_TAG="$1" \
    docker compose --env-file "$H/.env" -f deploy/compose/docker-compose.yml "${@:2}")
}

# store_run TAG MODE: store_snapshot.py in a throwaway container of that tag's worker image (python3 and
# sqlite3 are in it; flotillad's is distroless). No network, read-only root, uid 65532 = flotillad's, so the
# files stay the ones it opens. Data goes over stdin and stdout, never through a host mount.
store_run() {
  docker run --rm -i --network none --read-only --cap-drop ALL --security-opt no-new-privileges:true \
    --user 65532:65532 --tmpfs /tmp:size=512m --memory 1g -v "$STATE_VOL:/v" \
    --entrypoint python3 "$REGISTRY/flotilla-worker-triage:$1" -c "$STORE_HELPER" "${@:2}"
}

# snapshot TAG: the store, consistent, into $BACKUPS/TAG-<UTC>/. Sets SNAP (empty when there is
# no store yet). Fails, with nothing written under its final name, when the copy is not a sound database.
snapshot() {
  SNAP=""
  local dir n tmp rc
  dir="$BACKUPS/$1-$(date -u +%Y%m%dT%H%M%SZ)"
  n=1; while [ -e "$dir" ]; do n=$((n+1)); dir="${dir%-[0-9]}-$n"; done   # two in one second
  tmp="$dir.partial"
  mkdir -p -m 700 "$BACKUPS" "$tmp" || return 1   # the store holds sessions and approvals: owner only
  (umask 077; store_run "$1" backup </dev/null >"$tmp/flotilla.db"); rc=$?
  if [ "$rc" = 3 ]; then rm -rf "$tmp"; say "no store in $STATE_VOL yet (a first install): nothing to snapshot"; return 0; fi
  [ "$rc" = 0 ] || { rm -rf "$tmp"; return 1; }
  n="$(store_run "$1" inspect <"$tmp/flotilla.db")" && [[ "$n" =~ ^SCHEMA\ ([0-9]+)$ ]] || { rm -rf "$tmp"; return 1; }
  printf '%s\n' "${BASH_REMATCH[1]}" >"$tmp/SCHEMA"
  printf 'deploy of %s from %s\n' "$1" "${CURRENT:-none}" >"$tmp/WHY"
  mv "$tmp" "$dir" || return 1
  SNAP="$dir"
  say "snapshot: $dir (schema ${BASH_REMATCH[1]})"
}

# the snapshots of this script, newest first (the time in the name; -N for a second one in a second)
snapshots() {
  find "$BACKUPS" -maxdepth 1 -type d -name 'sha-???????-????????T??????Z*' ! -name '*.partial' 2>/dev/null \
    | awk -F/ '{n=split($NF,a,"-"); print a[3] "\t" (a[4]==""?0:a[4]) "\t" $0}' | sort -k1,1r -k2,2nr | cut -f3
}
prune_snapshots() {
  local d
  snapshots | tail -n +$((KEEP+1)) | while IFS= read -r d; do rm -rf "$d"; done
}
trap prune_snapshots EXIT

# fitting_snapshot LIMIT: the newest snapshot of a store this flotillad can run (schema <= LIMIT)
fitting_snapshot() {
  local d
  while IFS= read -r d; do
    [ -f "$d/SCHEMA" ] && [ -f "$d/flotilla.db" ] && [ "$(cat "$d/SCHEMA")" -le "$1" ] && { printf '%s\n' "$d"; return 0; }
  done < <(snapshots)
  return 1
}

# tag_schema COMMIT: how many schema steps that commit's flotillad knows (its store.go); empty when unknown
tag_schema() {
  git -C "$REPO" show "$1:internal/store/store.go" 2>/dev/null \
    | awk '/^var schema = \[\]string\{/{b=1;next} b&&/^\}/{exit} b{n+=gsub(/`/,"`")} END{if (n>0) print n/2}'
}

restore_checkout() {  # after a refusal that changed nothing
  [ -z "$CURRENT_COMMIT" ] || git -C "$REPO" -c advice.detachedHead=false checkout -q --detach "$CURRENT_COMMIT"
}

wait_healthy() {
  local _
  for _ in $(seq 1 "$HEALTH_TRIES"); do
    if docker exec flotillad /usr/local/bin/flotilla health >/dev/null 2>&1 \
       && docker exec flotillad /usr/local/bin/flotilla status 2>/dev/null | grep -q '"ok":true'; then return 0; fi
    sleep "$HEALTH_SLEEP"
  done
  return 1
}

say "== checkout ${TAG#sha-}"
git -C "$REPO" fetch -q origin || die "git fetch failed"
COMMIT="$(git -C "$REPO" rev-parse --verify -q "${TAG#sha-}^{commit}")" || die "no commit ${TAG#sha-} in chipi/flotilla"
git -C "$REPO" -c advice.detachedHead=false checkout -q --detach "$COMMIT" || die "checkout failed"

if [ "$EVENT" = deploy ]; then
  say "== the triage fleet's filing gate (${CURRENT_COMMIT:0:7} -> ${COMMIT:0:7})"
  if ! triage_filing_gate "$CURRENT_COMMIT" "$COMMIT" | sed 's/^/   /'; then   # pipefail: the gate's status
    [ -n "$CURRENT_COMMIT" ] && git -C "$REPO" -c advice.detachedHead=false checkout -q --detach "$CURRENT_COMMIT"
    die "filing gate failed or did not run for $TAG (set SF_FILING_GATE_ACCEPT=\"<why>\" to accept)"
  fi
fi

say "== pull $TAG"
compose "$TAG" pull -q || die "images for $TAG not pullable (did the images workflow run for ${TAG#sha-}?)"

say "== secrets into their volumes (a rotated secret is picked up here)"
FLOTILLA_HOME="$H" bash "$REPO/deploy/mini/secrets.sh" "$TAG" | sed 's/^/   /'
[ "${PIPESTATUS[0]}" = 0 ] || die "secrets.sh failed"

say "== check config, callers, specs and the store's schema with the new flotillad"
OUT="$(compose "$TAG" run --rm --no-deps flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"
say "$OUT" | sed 's/^/   /'
REFUSED=0
if ! grep -q "^CHECK PASS" <<<"$OUT"; then
  if [ "$EVENT" = rollback ] && grep -q "newer than this flotillad" <<<"$OUT"; then
    REFUSED=1
  else
    restore_checkout
    die "flotillad -check failed for $TAG (nothing was stopped)$(grep -q 'newer than this flotillad' <<<"$OUT" && echo ": the store is newer than $TAG runs; deploy.sh --rollback restores a snapshot")"
  fi
fi
# The newest schema $TAG accepts: from its -check's refusal, or, for a tag whose -check does not look at the
# store (older than this check), from the schema steps in its source.
LIMIT="$(sed -n 's/.*newer than this flotillad (\([0-9][0-9]*\)).*/\1/p' <<<"$OUT" | head -1)"
[ -n "$LIMIT" ] || LIMIT="$(tag_schema "$COMMIT")"
LIVE="$(store_run "$TAG" schema </dev/null)" || { restore_checkout; die "could not read the store's schema (nothing was stopped)"; }
if [ -n "$LIMIT" ] && [ "$LIVE" -gt "$LIMIT" ]; then REFUSED=1; fi
if [ "$REFUSED" = 1 ] && [ "$EVENT" != rollback ]; then
  restore_checkout
  die "the store is at schema $LIVE and $TAG runs up to ${LIMIT:-?}: deploy.sh --rollback restores a snapshot (nothing was stopped)"
fi
RESTORE=""
if [ "$REFUSED" = 1 ]; then
  say "== $TAG accepts store schema up to $LIMIT and the store is at $LIVE: a snapshot is needed"
  RESTORE="$(fitting_snapshot "$LIMIT")" || {
    restore_checkout
    die "no snapshot with schema <= $LIMIT in $BACKUPS (have: $(for d in $(snapshots); do printf '%s=%s ' "$(basename "$d")" "$(cat "$d/SCHEMA" 2>/dev/null)"; done)): nothing was stopped, restored or changed"
  }
  say "   will restore $RESTORE (schema $(cat "$RESTORE/SCHEMA"), $(cat "$RESTORE/WHY" 2>/dev/null))"
fi

say "== snapshot the store"
snapshot "$TAG" || { restore_checkout; die "snapshot of the store failed (nothing was stopped)"; }

if [ -n "$RESTORE" ]; then
  say "== restore the store from $RESTORE"
  compose "$TAG" stop flotillad 2>&1 | sed 's/^/   /'
  store_run "$TAG" restore <"$RESTORE/flotilla.db" | sed 's/^/   /'
  [ "${PIPESTATUS[0]}" = 0 ] || die "restoring $RESTORE failed: flotillad is stopped, the store may be half restored; the snapshot of what was there is ${SNAP:-none}"
  RESTORED="restored the store from $RESTORE (schema $(cat "$RESTORE/SCHEMA")); rows written after it are lost, what was there is kept in ${SNAP:-no snapshot}"
  say "   $RESTORED"
  OUT="$(compose "$TAG" run --rm --no-deps flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"
  say "$OUT" | sed 's/^/   /'
  grep -q "^CHECK PASS" <<<"$OUT" || die "flotillad -check still fails for $TAG after the restore: flotillad is stopped"
fi

# fall_back TAG COMMIT: bring TAG back after an unhealthy up; 0 only when it is running and healthy. The
# unhealthy flotillad may have moved the store past what TAG accepts: the snapshot taken for this run (or,
# failing that, the newest that fits) is restored first, as a rollback does. Sets FALL_BACK_WHY on failure.
fall_back() {
  local t="$1" c="$2" out limit live snap
  FALL_BACK_WHY=""
  git -C "$REPO" -c advice.detachedHead=false checkout -q --detach "${t#sha-}" || { FALL_BACK_WHY="checkout of $t failed"; return 1; }
  compose "$t" stop flotillad >/dev/null 2>&1
  out="$(compose "$t" run --rm --no-deps flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"
  limit="$(sed -n 's/.*newer than this flotillad (\([0-9][0-9]*\)).*/\1/p' <<<"$out" | head -1)"
  [ -n "$limit" ] || limit="$(tag_schema "$c")"
  live="$(store_run "$t" schema </dev/null)" || { FALL_BACK_WHY="could not read the store's schema"; return 1; }
  if [ -n "$limit" ] && [ "$live" -gt "$limit" ]; then
    if [ -n "${SNAP:-}" ] && [ -f "$SNAP/SCHEMA" ] && [ "$(cat "$SNAP/SCHEMA")" -le "$limit" ]; then snap="$SNAP"; else snap="$(fitting_snapshot "$limit")"; fi
    [ -n "$snap" ] || { FALL_BACK_WHY="the store is at schema $live, $t runs up to $limit, and no snapshot fits ($BACKUPS)"; return 1; }
    store_run "$t" restore <"$snap/flotilla.db" | sed 's/^/   /'
    [ "${PIPESTATUS[0]}" = 0 ] || { FALL_BACK_WHY="restoring $snap failed"; return 1; }
    say "   restored the store from $snap (schema $(cat "$snap/SCHEMA")); rows written after it are lost"
  fi
  compose "$t" up -d --remove-orphans >/dev/null 2>&1
  wait_healthy || { FALL_BACK_WHY="it did not come up healthy either"; return 1; }
}

say "== up"
# A deploy stand-down on this host (RFC-0004 §5.1): plans on it are deferred while the stack changes.
# FLOTILLA_STANDDOWN_HOST in .env names it; unset, none is taken. Never blocks the deploy.
if [ -n "$(envget FLOTILLA_STANDDOWN_HOST)" ] && [ -f "$REPO/deploy/mini/standdown.sh" ]; then   # an older tag has none
  bash "$REPO/deploy/mini/standdown.sh" "$(envget FLOTILLA_STANDDOWN_HOST)" 30m "flotilla $TAG" | sed 's/^/   /'
fi
# A container whose mounted config changed must restart to read it: compose recreates a container
# only when its own definition changes, never for a changed bind-mounted file. Each service's config
# files are hashed into its environment, so a changed file is a changed definition (found 2026-11-09:
# a models section added to flotillad.yaml reached -check, but not the running flotillad).
cfghash() { for f in "$@"; do printf '%s\n' "$f"; cat "$f" 2>/dev/null; done | { shasum -a 256 2>/dev/null || sha256sum; } | cut -c1-16; }
CFG="$(envget FLOTILLA_CONFIG)"
# host-ca/: flotillad reads the intermediate and the signing key only at start, so a rotated one
# (docs/operations/host-keys.md) must recreate it too
FLOTILLA_CONFIG_HASH_FLOTILLAD="$(cfghash "$CFG/flotillad.yaml" "$CFG/callers.yaml" "$CFG/cli.token" "$CFG/worker-triage.token" "$CFG/worker-ops-read.token" "$CFG/worker-ops-act-host.token" "$CFG/worker-bugfix.token" "$CFG/worker-bugfix-act.token" "$CFG/worker-marketing.token" "$(envget FLOTILLA_ERRORS_DSN_FILE)" \
  "$CFG/host-ca/intermediate.crt" "$CFG/host-ca/root.crt" "$CFG/host-ca/signing.key")"
FLOTILLA_CONFIG_HASH_EGRESS="$(cfghash "$CFG/smokescreen.yaml" "$CFG/egress-acl.yaml")"
FLOTILLA_CONFIG_HASH_EGRESS_BUGFIX="$(cfghash "$CFG/smokescreen.yaml" "$CFG/egress-bugfix-acl.yaml")"
FLOTILLA_CONFIG_HASH_EGRESS_SEO="$(cfghash "$CFG/smokescreen-seo.yaml" "$CFG/egress-seo-acl.yaml")"
export FLOTILLA_CONFIG_HASH_FLOTILLAD FLOTILLA_CONFIG_HASH_EGRESS FLOTILLA_CONFIG_HASH_EGRESS_BUGFIX FLOTILLA_CONFIG_HASH_EGRESS_SEO
compose "$TAG" up -d --remove-orphans 2>&1 | sed 's/^/   /'
triage_deploy_log "$EVENT" "$CURRENT_COMMIT" "$COMMIT" | sed 's/^/   /'
if ! wait_healthy; then
  printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$TAG" "$COMMIT" fail >> "$LOG"
  if [ -n "$CURRENT" ] && [ "$CURRENT" != "$TAG" ]; then
    say "flotillad or the worker did not come up healthy: back to $CURRENT"
    if fall_back "$CURRENT" "$CURRENT_COMMIT"; then
      triage_deploy_log rollback "$COMMIT" "$CURRENT_COMMIT" | sed 's/^/   /'
      if [ "$EVENT" = rollback ]; then
        event failure "$TAG" "rollback target unhealthy; $CURRENT is running again"
        say "DEPLOY FAIL (rollback to $TAG): not healthy; $CURRENT is running and healthy again, $TAG is not"
      else
        event failure "$TAG" "unhealthy after up; rolled back to $CURRENT"
        say "rolled back to $CURRENT (running and healthy)"
        say "DEPLOY FAIL ($TAG): rolled back to $CURRENT"
      fi
    else
      event failure "$TAG" "unhealthy after up; $CURRENT could not be brought back: $FALL_BACK_WHY"
      say "DEPLOY FAIL ($TAG): $CURRENT is NOT running and healthy: $FALL_BACK_WHY"
    fi
    exit 1
  fi
  die "flotillad or the worker did not come up healthy (no earlier tag to roll back to)"
fi
docker exec flotillad /usr/local/bin/flotilla status | sed 's/^/   /'
printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$TAG" "$COMMIT" pass >> "$LOG"
event success "$TAG" "up and healthy (previous ${CURRENT:-none})${RESTORED:+; $RESTORED}"
[ "$EVENT" != rollback ] || say "rolled back to $TAG (running and healthy)${RESTORED:+; $RESTORED}"
say "DEPLOY PASS ($TAG)"
