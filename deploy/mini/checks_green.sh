#!/usr/bin/env bash
# Refuse a commit whose required checks are not green.
#
#   checks_green.sh <sha|sha-7> [check-name ...]      names default to: ci-fast
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

rc=0
for name in "${required[@]}"; do
  last="$(printf '%s\n' "$runs" | awk -F'\t' -v n="$name" '$2 == n && $1 > id {id = $1; line = $3 "/" $4} END {print line}')"
  if [ "$last" = completed/success ]; then
    echo "  required $name: ok"
  else
    echo "  required $name: ${last:-missing} (needs completed/success)"; rc=1
  fi
done
if [ "$rc" = 0 ]; then echo "checks_green: GREEN"; else echo "checks_green: RED"; fi
exit "$rc"
