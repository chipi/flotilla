#!/usr/bin/env bash
# pi harness adapter — headless single fix attempt inside $WT.
set -euo pipefail
WT="$1"; DESC="$2"
MODEL="${PI_MODEL:-deepseek/deepseek-v4-pro}"
PROVIDER="${PI_PROVIDER:-openrouter}"
# PI_RESUME_SESSION (bugfix/fix.py): continue the previous round's session with DESC as the next message (the
# gates' or the review's verdict on the fix), instead of a new session that explores the repo from nothing
if [ -n "${PI_RESUME_SESSION:-}" ]; then
  cd "$WT"
  export NODE_OPTIONS="--max-old-space-size=4096"
  exec pi -p --mode json --provider "$PROVIDER" --model "$MODEL" --session "$PI_RESUME_SESSION" ${BUGFIX_PI_RULES:+--no-context-files --append-system-prompt "$BUGFIX_PI_RULES"} "$DESC" < /dev/null
fi
read -r -d '' PROMPT <<EOF || true
You are fixing a bug in the repository at the current working directory.

Bug report:
$DESC

Diagnose the cause in the source and fix it. Add or update a regression test if
appropriate. Keep the change tight and correct; do not touch unrelated code.
When done, stop — do not commit, push, or open a PR.
EOF
cd "$WT"
export NODE_OPTIONS="--max-old-space-size=4096"
# </dev/null: headless run must not inherit caller stdin — pi -p blocks reading
# a held-open stdin socket (bit us 2026-07-23: 11-min 0-CPU hang, zero output)
# BUGFIX_PI_RULES (bugfix/fix.py): the fleet's rule file instead of the repo's AGENTS.md (bugfix/fleet_rules.md)
pi -p --mode json --provider "$PROVIDER" --model "$MODEL" ${BUGFIX_PI_RULES:+--no-context-files --append-system-prompt "$BUGFIX_PI_RULES"} "$PROMPT" < /dev/null
