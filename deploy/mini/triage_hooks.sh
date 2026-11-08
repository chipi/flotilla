#!/usr/bin/env bash
# The triage fleet's deploy hooks, sourced by deploy.sh. They keep the readers in fleet-eval-data working
# (phase-2-plan, "Before the switch", items 4 and 4a):
#
#   triage_filing_gate PREV NOW   the filing gate, as homelab's signal-fleet/deploy/gates.sh calls it: it replays
#                                 the newest frozen week with the two flotilla commits; any change in what filing
#                                 does fails, unless SF_FILING_GATE_ACCEPT names the reason (read by the gate).
#                                 Returns 0 on PASS or when there is nothing to compare, 1 on FAIL.
#   triage_deploy_log EVENT OLD NEW
#                                 one row in the fleet's deploy log, with `flotilla` as the 7th field (the header
#                                 keeps its six columns; the readers take the 7th by position). Never fails.
#
# Settings (FLOTILLA_HOME/.env):
#   TRIAGE_DEPLOY_LOG   the deploy log to append to. Unset: no row. Until the switch it is mirror-test's
#                       results/deploys.tsv, since the live fleet still runs homelab code; at the switch it
#                       becomes ~/signal-fleet/results/deploys.tsv.
#   TRIAGE_FILING_GATE  the gate script (default ~/fleet-eval-data/projects/signal-fleet-filing/scripts/filing_gate.py)
# Both need REPO (the flotilla checkout) and H (FLOTILLA_HOME) from deploy.sh.

triage_filing_gate() {
  local prev="$1" now="$2" gate="${TRIAGE_FILING_GATE:-$HOME/fleet-eval-data/projects/signal-fleet-filing/scripts/filing_gate.py}"
  local log="$H/filing_gate.log"
  if [ -z "$prev" ]; then echo "filing gate: NOT RUN (no earlier deploy to compare with)"; return 0; fi
  if [ ! -f "$gate" ]; then echo "filing gate: NOT RUN (no $gate)"; return 0; fi
  if SF_CHECKOUT="$REPO" python3 "$gate" "$prev" "$now" >"$log" 2>&1; then
    echo "filing gate: $(tail -1 "$log")"
    return 0
  fi
  echo "filing gate: FAIL (full output: $log)"
  tail -8 "$log" | sed 's/^/    /'
  return 1
}

triage_deploy_log() {
  local event="$1" old="$2" new="$3" log="${TRIAGE_DEPLOY_LOG:-}"
  [ -n "$log" ] || { echo "deploy log: no row (TRIAGE_DEPLOY_LOG unset)"; return 0; }
  local tree o n
  tree="$(git -C "$REPO" rev-parse --verify -q "${new}:signal-fleet/mvp" 2>/dev/null)" || tree=-
  o="$(git -C "$REPO" rev-parse --verify -q --short "${old}^{commit}" 2>/dev/null)" || o="${old:--}"
  n="$(git -C "$REPO" rev-parse --verify -q --short "${new}^{commit}" 2>/dev/null)" || n="$new"
  if mkdir -p "$(dirname "$log")" 2>/dev/null \
     && { [ -s "$log" ] || printf 'utc\tevent\told\tnew\tmvp_tree\tby\n' > "$log"; } 2>/dev/null \
     && printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$event" "$o" "$n" "$tree" \
          "${USER:-?}" flotilla 2>/dev/null >> "$log"; then
    echo "deploy log: $event $o -> $n in $log"
  else
    echo "deploy log: row not written ($log)"
  fi
  return 0
}
