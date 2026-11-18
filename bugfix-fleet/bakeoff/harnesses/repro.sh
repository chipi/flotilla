#!/usr/bin/env bash
# The reproducer (bugfix/fix.py, Chain.repro): one headless pi session inside $WT that writes the failing test from
# the issue's evidence, or says what is missing. Its prompt is agents/repro.md; $2 is the issue as filed and the
# verdict's task. pi.sh's prompt says "fix the bug", which this step must never do.
set -euo pipefail
WT="$1"; DESC="$2"
HERE="$(cd "$(dirname "$0")/../.." && pwd)"
MODEL="${PI_MODEL:-deepseek/deepseek-v4-flash}"
PROVIDER="${PI_PROVIDER:-openrouter}"
AGENT_DEF=$(awk '/^---$/{c++;next} c>=2' "$HERE/agents/repro.md")
read -r -d '' PROMPT <<EOF || true
$AGENT_DEF

$DESC
EOF
cd "$WT"
export NODE_OPTIONS="--max-old-space-size=4096"
# </dev/null: a headless run must not inherit the caller's stdin (pi.sh, 2026-07-23)
pi -p --mode json --provider "$PROVIDER" --model "$MODEL" ${BUGFIX_PI_RULES:+--no-context-files --append-system-prompt "$BUGFIX_PI_RULES"} "$PROMPT" < /dev/null
