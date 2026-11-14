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
# Ends with DEPLOY PASS or DEPLOY FAIL; a deploy and a rollback each write an ops_event/v1 line to
# VictoriaLogs (event_type deploy, service flotilla).
set -uo pipefail
export PATH=$PATH:/usr/local/bin:/usr/bin:/bin   # appended: an SSH session's PATH lacks /usr/local/bin (docker)
H="${FLOTILLA_HOME:-$HOME/flotilla}"
REPO="$H/repo"
LOG="$H/deployed.tsv"
VLOGS="${VLOGS:-http://127.0.0.1:9428}"
say() { printf '%s\n' "$*"; }
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

previous() { awk -F'\t' '$4=="pass"{t=$2} END{print t}' "$LOG" 2>/dev/null; }
CURRENT="$(previous)"
CURRENT_COMMIT="$(awk -F'\t' '$4=="pass"{c=$3} END{print c}' "$LOG" 2>/dev/null)"
HOOKS="$REPO/deploy/mini/triage_hooks.sh"
[ -f "$HOOKS" ] || die "$HOOKS missing"
# shellcheck source=triage_hooks.sh
. "$HOOKS"
envget() { (set -a; . "$H/.env" >/dev/null 2>&1; eval "printf '%s' \"\${$1:-}\""); }
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

say "== checkout ${TAG#sha-}"
git -C "$REPO" fetch -q origin || die "git fetch failed"
COMMIT="$(git -C "$REPO" rev-parse --verify -q "${TAG#sha-}^{commit}")" || die "no commit ${TAG#sha-} in chipi/flotilla"
git -C "$REPO" -c advice.detachedHead=false checkout -q --detach "$COMMIT" || die "checkout failed"

if [ "$EVENT" = deploy ]; then
  say "== the triage fleet's filing gate (${CURRENT_COMMIT:0:7} -> ${COMMIT:0:7})"
  if ! triage_filing_gate "$CURRENT_COMMIT" "$COMMIT" | sed 's/^/   /'; then   # pipefail: the gate's status
    [ -n "$CURRENT_COMMIT" ] && git -C "$REPO" -c advice.detachedHead=false checkout -q --detach "$CURRENT_COMMIT"
    die "filing gate failed for $TAG (set SF_FILING_GATE_ACCEPT=\"<why>\" to accept the change)"
  fi
fi

say "== pull $TAG"
compose "$TAG" pull -q || die "images for $TAG not pullable (did the images workflow run for ${TAG#sha-}?)"

say "== secrets into their volumes (a rotated secret is picked up here)"
FLOTILLA_HOME="$H" bash "$REPO/deploy/mini/secrets.sh" "$TAG" | sed 's/^/   /'
[ "${PIPESTATUS[0]}" = 0 ] || die "secrets.sh failed"

say "== check config, callers and specs with the new flotillad"
OUT="$(compose "$TAG" run --rm --no-deps flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"
say "$OUT" | sed 's/^/   /'
grep -q "^CHECK PASS" <<<"$OUT" || die "flotillad -check failed for $TAG"

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
FLOTILLA_CONFIG_HASH_FLOTILLAD="$(cfghash "$CFG/flotillad.yaml" "$CFG/callers.yaml" "$CFG/cli.token" "$CFG/worker-triage.token" "$CFG/worker-ops-read.token" "$CFG/worker-ops-act-host.token" "$(envget FLOTILLA_ERRORS_DSN_FILE)" \
  "$CFG/host-ca/intermediate.crt" "$CFG/host-ca/root.crt" "$CFG/host-ca/signing.key")"
FLOTILLA_CONFIG_HASH_EGRESS="$(cfghash "$CFG/smokescreen.yaml" "$CFG/egress-acl.yaml")"
export FLOTILLA_CONFIG_HASH_FLOTILLAD FLOTILLA_CONFIG_HASH_EGRESS
compose "$TAG" up -d --remove-orphans 2>&1 | sed 's/^/   /'
triage_deploy_log "$EVENT" "$CURRENT_COMMIT" "$COMMIT" | sed 's/^/   /'
ok=0
for _ in $(seq 1 30); do
  if docker exec flotillad /usr/local/bin/flotilla health >/dev/null 2>&1 \
     && docker exec flotillad /usr/local/bin/flotilla status 2>/dev/null | grep -q '"ok":true'; then ok=1; break; fi
  sleep 2
done
if [ "$ok" -ne 1 ]; then
  printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$TAG" "$COMMIT" fail >> "$LOG"
  if [ -n "$CURRENT" ] && [ "$CURRENT" != "$TAG" ]; then
    say "flotillad or the worker did not come up healthy: back to $CURRENT"
    git -C "$REPO" -c advice.detachedHead=false checkout -q --detach "${CURRENT#sha-}"
    compose "$CURRENT" up -d --remove-orphans >/dev/null 2>&1
    triage_deploy_log rollback "$COMMIT" "$CURRENT_COMMIT" | sed 's/^/   /'
    event failure "$TAG" "unhealthy after up; rolled back to $CURRENT"
    say "DEPLOY FAIL ($TAG): rolled back to $CURRENT"; exit 1
  fi
  die "flotillad or the worker did not come up healthy (no earlier tag to roll back to)"
fi
docker exec flotillad /usr/local/bin/flotilla status | sed 's/^/   /'
printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$TAG" "$COMMIT" pass >> "$LOG"
event success "$TAG" "up and healthy (previous ${CURRENT:-none})"
say "DEPLOY PASS ($TAG)"
