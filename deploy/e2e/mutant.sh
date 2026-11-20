#!/usr/bin/env bash
# A mutation check of the e2e: run it with one fault put in (E2E_MUTANT) over some sections, and pass only if it goes red, on a check that fault
# must break (a FAIL line matching the pattern), not on something else.
#
#   deploy/e2e/mutant.sh MUTANT SECTIONS 'regex'     e.g. mutant.sh shadow-egress-open noeffect 'a shadow reached GitHub'
#
# Exit 0: the run failed and a FAIL line matches. Exit 1: it passed (the check cannot fail) or failed elsewhere. The run's output goes to stdout.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
MUT="${1:?mutant}"; SECTIONS="${2:?sections}"; PATTERN="${3:?pattern}"
OUT="$(mktemp)"; trap 'rm -f "$OUT"' EXIT
E2E_MUTANT="$MUT" E2E_SECTIONS="$SECTIONS" bash "$ROOT/deploy/e2e/run.sh" > "$OUT" 2>&1; RC=$?
[ -z "${MUTANT_LOG:-}" ] || cp "$OUT" "$MUTANT_LOG"   # the whole run's output, for whoever reads why
grep -E '^(FAIL|E2E|MUTANT)' "$OUT" | cut -c1-300
if [ "$RC" != 0 ] && grep -E "^FAIL .*($PATTERN)" "$OUT" >/dev/null; then
  echo "MUTANT $MUT RED: the run failed on '$PATTERN', as it must"; exit 0
fi
if [ "$RC" = 0 ]; then echo "MUTANT $MUT NOT RED: the run passed with the fault put in, so the check cannot catch it"
else echo "MUTANT $MUT FAILED, BUT NOT ON '$PATTERN': the check it was meant to break did not fail"; fi
exit 1
