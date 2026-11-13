#!/usr/bin/env bash
# standdown.sh HOST DURATION WHY: a deploy stand-down on a host (RFC-0004 §5.1 step 4). It is a hold
# (flotilla hold host:HOST), so while it lasts policy defers every plan on that host; it ends on its own.
# Taken by flotilla's own deploy.sh and by the homelab's mini-deploy.sh (a pre-pull hook), before they
# change anything on the host.
#
# It never fails its caller: a deploy must not wait for flotilla (flotillad may be down, or be what is
# being deployed). It prints STANDDOWN or STANDDOWN SKIPPED with why.
#
# The token: the installation's `deploy` caller (role agent: it may hold, never lift), its file in
# flotillad's secrets volume as deploy.token (secrets.sh puts it there from config/deploy.token), so it
# passes through no environment variable or command line, only its path does.
#
# DOCKER overrides the docker command (the e2e: docker --context dev).
set -u
export PATH=$PATH:/usr/local/bin:/usr/bin:/bin
HOST="${1:?usage: standdown.sh HOST DURATION WHY}"
FOR="${2:-30m}"
WHY="${3:-deploy}"
D="${DOCKER:-docker}"
if out=$($D exec -e FLOTILLA_TOKEN_FILE=/run/flotilla/deploy.token flotillad /usr/local/bin/flotilla \
    hold "host:$HOST" "$FOR" --reason "deploy stand-down: $WHY" 2>&1) && grep -q '"id"' <<<"$out"; then
  echo "STANDDOWN host:$HOST for $FOR ($WHY): $out"
else
  echo "STANDDOWN SKIPPED (the deploy continues): $(tr '\n' ' ' <<<"$out" | cut -c1-300)"
fi
exit 0
