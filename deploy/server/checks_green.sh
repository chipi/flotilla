#!/usr/bin/env bash
# Refuse a commit whose required checks are not green.
#
#   checks_green.sh <sha|sha-7> [check-name ...]      names default to: ci-fast
#
# A name ending in ? (e2e?) is required when the commit has a run of it, and may be absent: e2e.yml skips
# docs-only commits (paths-ignore), so such a commit has no e2e run at all. A run that exists must be green.
#
# Reads the commit's check runs from GitHub (gh api repos/$FLOTILLA_REPO/commits/<sha>/check-runs;
# default chipi/flotilla), prints one line per check run it saw, then GREEN or RED. Exit 0 only when
# every named check's latest run (a re-run replaces the run before it) is completed and successful.
# A check that did not run, is still running, failed, or was cancelled is RED, never "not applicable".
# deploy.sh calls it before it pulls an image, so a tag whose commit failed its gates is not deployed.
set -uo pipefail
REPO="${FLOTILLA_REPO:-chipi/flotilla}"
ref="${1:-}"; shift || true
ref="${ref#sha-}"
[[ "$ref" =~ ^[0-9a-f]{7,40}$ ]] || { echo "usage: checks_green.sh <sha|sha-7> [check-name ...]" >&2; exit 2; }
required=("$@"); [ "${#required[@]}" -gt 0 ] || required=(ci-fast)

runs="$(gh api --paginate "repos/$REPO/commits/$ref/check-runs?per_page=100" \
  --jq '.check_runs[] | [.id, .name, .status, (.conclusion // "-")] | @tsv' 2>&1)" \
  || { echo "checks_green: RED: could not read the check runs of $ref: $runs"; exit 1; }

echo "check runs of $REPO@$ref:"
[ -n "$runs" ] && printf '%s\n' "$runs" | awk -F'\t' '{printf "  %-28s %s/%s (run %s)\n", $2, $3, $4, $1}'
[ -n "$runs" ] || echo "  (none)"

latest() { awk -F'\t' -v n="$1" '$2 == n && $1 > id {id = $1; line = $3 "/" $4} END {print line}'; }
DEPTH=30

rc=0
for name in "${required[@]}"; do
  optional=; [ "${name%\?}" != "$name" ] && { optional=1; name="${name%\?}"; }
  last="$(printf '%s\n' "$runs" | latest "$name")"
  if [ -z "$last" ] && [ -n "$optional" ]; then
    # a path-filtered workflow skips most commits: a later commit it skipped must not hide an earlier one it failed
    if ! shas="$(gh api "repos/$REPO/commits?sha=$ref&per_page=$DEPTH" --jq '.[].sha' 2>&1)"; then
      echo "  required $name: no run for this commit, and could not read the commits before it: $shas"; rc=1; continue
    fi
    anc=; alast=
    for s in $(printf '%s\n' "$shas" | tail -n +2); do
      ar="$(gh api --paginate "repos/$REPO/commits/$s/check-runs?per_page=100" \
        --jq '.check_runs[] | [.id, .name, .status, (.conclusion // "-")] | @tsv' 2>&1)" \
        || { echo "  required $name: could not read the check runs of $s: $ar"; rc=1; continue 2; }
      alast="$(printf '%s\n' "$ar" | latest "$name")"
      [ -n "$alast" ] && { anc="$s"; break; }
    done
    if [ -z "$anc" ]; then
      echo "  required $name: no run for this commit or its last $((DEPTH - 1)) ancestors (its workflow skips them)"
    elif [ "$alast" = completed/success ]; then
      echo "  required $name: no run for this commit; ${anc:0:7} (the nearest ancestor with one): ok"
    else
      echo "  required $name: no run for this commit; ${anc:0:7} (the nearest ancestor with one): $alast (needs completed/success)"; rc=1
    fi
  elif [ "$last" = completed/success ]; then
    echo "  required $name: ok"
  else
    echo "  required $name: ${last:-missing} (needs completed/success)"; rc=1
  fi
done
if [ "$rc" = 0 ]; then echo "checks_green: GREEN"; else echo "checks_green: RED"; fi
exit "$rc"
