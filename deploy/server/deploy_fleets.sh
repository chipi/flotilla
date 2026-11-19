#!/usr/bin/env bash
# Deploy the installation's own fleets (vision plan step 7, route B): one commit of the private fleets
# repo, checked by the running tag's flotillad, then made live by pointing FLOTILLA_FLEETS_DIR at it and
# recreating flotillad and the workers that read it, which mount it over the image's fleets (compose/fleets.yml).
#
#   deploy_fleets.sh <commit>       deploy that commit of FLOTILLA_FLEETS_REPO (fetched first)
#   deploy_fleets.sh --rollback     the commit deployed before the current one
#   deploy_fleets.sh --diff <commit>  only show what that commit changes against the running fleets
#
# Settings (FLOTILLA_HOME/.env):
#   FLOTILLA_FLEETS_REPO        a checkout of the private fleets repo (fetched, never changed here)
#   FLOTILLA_FLEETS_DIR         FLOTILLA_HOME/fleets: a symlink this script points at fleets-<sha7>/
#   FLOTILLA_ADMIN_TOKEN_FILE   a token with the admin role: the triage routines are paused while their
#                               workers restart, and resumed after (on any exit)
#
# Each commit is exported once to FLOTILLA_HOME/fleets-<sha7>/ (read-only), so the live folder is never
# a working tree. Before the switch it lists every file that differs from what the triage worker runs
# now; after it, every routine whose spec hash changed and every routine version whose state or hash changed
# (a new version of a routine arrives as one new shadow line). A live or previous version whose folder changed
# is frozen (docs/wip/v2-design.md section 1): flotillad -check refuses it before anything is paused or switched,
# and the deploy ends "FLEETS DEPLOY FAIL: versions ..." (ship a changed live version as a new number, or `flotilla versions
# rerecord` if the change is deliberate). A flat routine's v1 is not frozen: its change is re-recorded at the start, and
# the deploy prints each as "== re-recorded". Ends with FLEETS DEPLOY PASS or FLEETS DEPLOY FAIL.
set -uo pipefail
export PATH=$PATH:/usr/local/bin:/usr/bin:/bin
H="${FLOTILLA_HOME:-$HOME/flotilla}"
REPO="$H/repo"
LOG="$H/fleets-deployed.tsv"
say() { printf '%s\n' "$*"; }
die() { say "FLEETS DEPLOY FAIL: $*"; exit 1; }

[ -f "$H/.env" ] || die "$H/.env missing"
envget() { (set -a; . "$H/.env" >/dev/null 2>&1; eval "printf '%s' \"\${$1:-}\""); }
FR="$(envget FLOTILLA_FLEETS_REPO)"; LIVE="$(envget FLOTILLA_FLEETS_DIR)"; ADMIN="$(envget FLOTILLA_ADMIN_TOKEN_FILE)"
[ -n "$FR" ] && [ -d "$FR/.git" ] || die "FLOTILLA_FLEETS_REPO is not a checkout ($FR)"
[ "$LIVE" = "$H/fleets" ] || die "FLOTILLA_FLEETS_DIR must be $H/fleets (it is '$LIVE')"
[ -n "$ADMIN" ] && [ -r "$ADMIN" ] || die "FLOTILLA_ADMIN_TOKEN_FILE unreadable ($ADMIN)"
[ ! -e "$LIVE" ] || [ -L "$LIVE" ] || die "$LIVE exists and is not a symlink"
[ -f "$REPO/deploy/compose/fleets.yml" ] || die "$REPO has no deploy/compose/fleets.yml: deploy a tag that has it first"
TAG="$(awk -F'\t' '$4=="pass"{t=$2} END{print t}' "$H/deployed.tsv" 2>/dev/null)"
[ -n "$TAG" ] || die "no passing flotilla deploy in $H/deployed.tsv"

cli() { docker exec flotillad /usr/local/bin/flotilla "$@"; }
admin() { docker exec -i -e FLOTILLA_TOKEN_FILE=/dev/stdin flotillad /usr/local/bin/flotilla "$@" < "$ADMIN"; }
compose() {  # FLEETS: the folder to mount (the shell's value wins over .env in compose's interpolation)
  (cd "$REPO" && set -a && . "$H/.env" && set +a && export FLOTILLA_TAG="$TAG" FLOTILLA_FLEETS_DIR="$FLEETS" &&
    docker compose --env-file "$H/.env" -f deploy/compose/docker-compose.yml -f deploy/compose/fleets.yml "$@")
}

MODE=deploy
case "${1:-}" in
  --rollback) MODE=rollback
    WANT="$(awk -F'\t' '$3=="pass"{p=c; c=$2} END{print p}' "$LOG" 2>/dev/null)"
    [ -n "$WANT" ] || die "no earlier passing fleets deploy to roll back to" ;;
  --diff) MODE=diff; WANT="${2:-}" ;;
  *) WANT="${1:-}" ;;
esac
[ -n "$WANT" ] || die "usage: deploy_fleets.sh <commit> | --rollback | --diff <commit>"
git -C "$FR" fetch -q origin || die "fetch of $FR failed"
SHA="$(git -C "$FR" rev-parse --verify -q "$WANT^{commit}")" || die "no commit $WANT in $FR"
DIR="$H/fleets-${SHA:0:7}"

# The commit's fleets/, exported once, read-only: a deployed folder never changes under a running worker.
if [ ! -d "$DIR" ]; then
  tmp="$(mktemp -d "$H/.fleets.XXXXXX")" || die "mktemp"
  if ! git -C "$FR" archive "$SHA" fleets | tar -x -C "$tmp" || [ ! -d "$tmp/fleets" ]; then
    rm -rf "$tmp"; die "commit ${SHA:0:7} has no fleets/"
  fi
  # the commit, for a process change's PR on the registry this folder carries (review/process-change)
  echo "$SHA" > "$tmp/fleets/.commit" || die "cannot write .commit"
  chmod -R a+rX,go-w "$tmp/fleets" && mv "$tmp/fleets" "$DIR" && rmdir "$tmp" || die "export to $DIR failed"
  chmod -R a-w "$DIR"
fi
say "== fleets ${SHA:0:7} in $DIR"

# What the triage worker runs now (the image's fleets, or the folder mounted over them) against the commit.
RUN="$(mktemp -d)"; trap 'chmod -R u+w "$RUN"; rm -rf "$RUN"' EXIT   # the image's files are read-only
docker exec flotilla-worker-triage tar -C /opt/flotilla -cf - fleets | tar -x -C "$RUN" || die "cannot read the running fleets"
CHANGED="$(diff -rq "$RUN/fleets" "$DIR" | sed "s#$RUN/fleets#running#; s#$DIR#${SHA:0:7}#")"
if [ -z "$CHANGED" ]; then say "== against the running fleets: identical"; else say "== against the running fleets:"; say "$CHANGED" | sed 's/^/   /'; fi
[ "$MODE" = diff ] && { say "FLEETS DIFF DONE"; exit 0; }

say "== flotillad -check at $TAG with these fleets"
OUT="$(FLEETS="$DIR" compose run --rm --no-deps flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)" \
  || {
    # a frozen version is a refusal of its own: name it, whatever else -check printed
    if printf '%s\n' "$OUT" | grep -q '^versions: '; then
      printf '%s\n' "$OUT" | grep '^versions: ' | sed 's/^/   /'
      die "versions: flotillad -check refused fleets ${SHA:0:7} (a live or previous version changed or does not load, or a new version does not load; nothing was paused or switched): ship a changed live version as a new number, or \`flotilla versions rerecord ROUTINE vN --reason R\` if the change is deliberate"
    fi
    say "$OUT" | tail -15 | sed 's/^/   /'; die "flotillad -check refused fleets ${SHA:0:7}"
  }
# A flat routine's v1 is not frozen: -check says each one it will re-record (the start records it with a version.update event)
RR="$(printf '%s\n' "$OUT" | grep '^versions: .*re-recorded' || true)"
if [ -n "$RR" ]; then say "== re-recorded at the start (a flat routine's v1 is not frozen):"; printf '%s\n' "$RR" | sed 's/^versions: /   /'; fi

hashes() { cli status -json | /usr/bin/python3 -c 'import json,sys; [print(r["id"], r.get("spec_hash","-")) for r in json.load(sys.stdin)["routines"]]'; }
BEFORE="$(hashes)" || die "cannot read the routines' spec hashes"
# Every recorded version of every routine: id, version, state, hash. A flotillad that predates versions has none.
vstates() {
  local ids r
  ids="$(cli status -json | /usr/bin/python3 -c 'import json,sys; [print(r["id"]) for r in json.load(sys.stdin)["routines"]]')" || return 1
  for r in $ids; do
    cli versions "$r" -json | /usr/bin/python3 -c 'import json,sys; v=json.load(sys.stdin); [print(v["routine"], "v%d" % x["version"], x["state"], x["hash"][:12]) for x in v["versions"]]' || return 1
  done
}
NOVERS="(none: the running flotillad has no versions)"
BEFOREV="$(vstates 2>/dev/null)" || BEFOREV="$NOVERS"

# The triage routines are paused while their workers restart; they are resumed on any exit from here on.
paused=()
resume() {
  for i in $(seq 1 30); do cli status >/dev/null 2>&1 && break; sleep 2; done
  # ${a[@]+...}: an empty array under set -u fails in macOS's bash 3.2
  for r in ${paused[@]+"${paused[@]}"}; do admin resume "$r" --reason "fleets ${SHA:0:7} deployed" >/dev/null || say "RESUME FAIL: resume $r by hand"; done
  for r in ${paused[@]+"${paused[@]}"}; do   # a cycle due during the restart was recorded as skipped: run it now
    cli runs "$r" -n 1 2>/dev/null | grep -q "routine is paused" && { admin run "$r" >/dev/null || say "RUN FAIL: $r runs at its next interval"; }
  done
}
for r in triage/cycle triage/cycle-shadow; do
  st="$(cli status | awk -v r="$r" '$1==r')"
  [ -n "$st" ] || continue
  case "$st" in *paused*) continue ;; esac
  admin pause "$r" --reason "fleets ${SHA:0:7} deploying" >/dev/null || die "cannot pause $r"
  paused+=("$r")
done
trap 'chmod -R u+w "$RUN"; rm -rf "$RUN"; resume' EXIT
for i in $(seq 1 60); do cli status | grep -E '^triage/' | grep -q running || break; sleep 5; done

PREV="$(readlink "$LIVE" 2>/dev/null || true)"
ln -sfn "$DIR" "$LIVE" && [ "$(readlink "$LIVE")" = "$DIR" ] || die "cannot point $LIVE at $DIR"
say "== $LIVE -> $DIR (was ${PREV:-the image fleets})"
svcs=(flotillad worker-triage worker-marketing)
docker ps -a --format '{{.Names}}' | grep -qx flotilla-worker-triage-candidate && svcs+=(worker-triage-candidate)
# the bugfix and ops workers read the folder too (a repos file, a registry of the installation's own) on a tag whose
# fleets.yml mounts it into them
for w in worker-bugfix worker-ops-read worker-ops-act-host; do
  grep -q "^  $w:" "$REPO/deploy/compose/fleets.yml" && docker ps -a --format '{{.Names}}' | grep -qx "flotilla-$w" && svcs+=("$w")
done
FLEETS="$DIR" compose up -d --no-deps --force-recreate "${svcs[@]}" 2>&1 | sed 's/^/   /'

# As deploy.sh waits: flotillad's own health command and the triage worker answering it, not Docker's
# health status, which flotillad's 60 s interval leaves at "starting" for a minute (2026-11-17, a false FAIL).
ok=0
for i in $(seq 1 45); do
  cli health >/dev/null 2>&1 && cli status 2>/dev/null | grep -q '^worker triage: .*"ok":true' && { ok=1; break; }
  sleep 2
done
mounted="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/opt/flotilla/fleets"}}{{.Source}}{{end}}{{end}}' flotillad)"
AFTER="$(hashes)" || ok=0
say "== spec hashes:"
CH="$(diff <(printf '%s\n' "$BEFORE") <(printf '%s\n' "$AFTER") | grep '^[<>]' || true)"
if [ -z "$CH" ]; then say "   unchanged ($(printf '%s\n' "$AFTER" | wc -l | tr -d ' ') routines)"; else say "$CH" | sed 's/^/   /'; fi
say "== versions:"
AFTERV="$(vstates 2>/dev/null)" || { AFTERV="$NOVERS"; [ "$BEFOREV" = "$NOVERS" ] || ok=0; }
CHV="$(diff <(printf '%s\n' "$BEFOREV") <(printf '%s\n' "$AFTERV") | grep '^[<>]' || true)"
if [ -z "$CHV" ]; then say "   unchanged ($(printf '%s\n' "$AFTERV" | grep -c . | tr -d ' ') versions)"; else say "$CHV" | sed 's/^/   /'; fi
printf '%s\t%s\t%s\t%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$SHA" "$([ $ok = 1 ] && echo pass || echo fail)" "$TAG" >> "$LOG"
[ "$ok" = 1 ] || die "flotillad or the triage worker is not healthy after the restart (mounted: ${mounted:-none})"
say "FLEETS DEPLOY PASS: ${SHA:0:7} at $TAG ($MODE; mounted $mounted)"
