#!/usr/bin/env bash
# Shadow vs live triage decisions on the mini (phase-2-plan 2.7). Read-only.
#   shadow-compare.sh [--since ISO] [--volume NAME] [--json]
# --volume compares another state volume with the live ledger, e.g. the candidate's
# (TRIAGE_CANDIDATE_STATE_VOLUME); --since is then the time it was seeded.
# The live ledger is the fleet's ~/signal-fleet/results/dispositions.tsv; the shadow's is read out of
# its volume by a throwaway container (no network). --since defaults to the shadow's seed: the first
# deploy in FLOTILLA_HOME/deployed.tsv.
set -euo pipefail
export PATH=/usr/local/bin:/usr/bin:/bin:$PATH
H="${FLOTILLA_HOME:-$HOME/flotilla}"
set -a; . "$H/.env"; set +a
TAG="$(awk -F'\t' '$4=="pass"{t=$2} END{print t}' "$H/deployed.tsv")"
SINCE="$(awk -F'\t' 'NR==1{print $1}' "$H/deployed.tsv")"
VOL="${TRIAGE_STATE_VOLUME:-flotilla-triage-state-shadow}"
ARGS=()
while [ $# -gt 0 ]; do case "$1" in --since) SINCE="$2"; shift 2 ;; --volume) VOL="$2"; shift 2 ;; *) ARGS+=("$1"); shift ;; esac; done
T="$(mktemp)"; trap 'rm -f "$T"' EXIT
docker run --rm --network none --entrypoint cat -v "$VOL:/s:ro" \
  "ghcr.io/chipi/flotilla-worker-triage:$TAG" /s/results/dispositions.tsv > "$T"
python3 "$(dirname "$0")/shadow_compare.py" "${LIVE_LEDGER:-$HOME/signal-fleet/results/dispositions.tsv}" "$T" --since "$SINCE" ${ARGS[@]+"${ARGS[@]}"}   # bash 3.2 + set -u: an empty array is unbound
