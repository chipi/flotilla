#!/usr/bin/env bash
# The triage fleet's replay gates: every decision change replayed against recorded real cases.
#
# The corpora are real (scrubbed) production data and live in the private fleet-eval-data repo, so
# this runs on a host that has a checkout of it, not in public CI:
#   SF_EVAL_DATA    fleet-eval-data's projects/signal-fleet-replays
#   SF_ALERT_RULES  the homelab checkout's .../grafana/provisioning/alerting/rules.yaml
# HOME is an empty directory for each run: every live state path is under ~/signal-fleet, so no
# replay can read a host's ledgers (2026-10-17: a replay passed on a laptop, failed on the mini).
set -uo pipefail
cd "$(dirname "$0")/mvp" || exit 1
for v in SF_EVAL_DATA SF_ALERT_RULES; do
  [ -n "${!v:-}" ] || { echo "REPLAYS FAIL: $v is not set (see the header of $0)"; exit 1; }
done
LOGS="$(mktemp -d)"; GATE_HOME="$(mktemp -d)"; ok=0; bad=0
for g in gate_hardening.py gate_dedup.py gate_filing_replay.py gate_stale_replay.py gate_routing_replay.py \
         gate_triager_failure_replay.py gate_glitchtip_litellm_replay.py gate_recurrence_replay.py \
         gate_open_preference_replay.py gate_duplicate_follow_replay.py gate_identity_replay.py \
         gate_promotion_replay.py gate_retriage_replay.py gate_oom_edge_replay.py gate_assistant_failover_replay.py gate_inbox_open_replay.py \
         gate_filing_link_replay.py; do
  args=""; [ "$g" = gate_filing_replay.py ] && args="--cases-only"
  if HOME="$GATE_HOME" SF_OBSERV_DISABLED=1 python3 "$g" $args >"$LOGS/$g.log" 2>&1; then
    echo "PASS $g"; ok=$((ok + 1))
  else
    echo "FAIL $g:"; tail -3 "$LOGS/$g.log" | sed 's/^/    /'; bad=$((bad + 1))
  fi
done
rm -rf "$GATE_HOME"
echo "logs: $LOGS"
if [ "$bad" -eq 0 ]; then echo "REPLAYS PASS ($ok)"; else echo "REPLAYS FAIL ($bad of $((ok + bad)))"; exit 1; fi
