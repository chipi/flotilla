#!/usr/bin/env bash
# End to end: the real compose file and images on the dev Docker engine, with stand-in services.
#
#   deploy/e2e/run.sh            build the images, bring the stack up, check, tear down
#   KEEP=1 deploy/e2e/run.sh     leave it up to look around (tear down: deploy/e2e/run.sh down)
#   DEMO=1 deploy/e2e/run.sh     `make demo`: the stack with the example fleets on their stand-ins, left up,
#                                the UI on https://localhost:$DEMO_PORT (8443) with a demo admin; it stops after
#                                the examples' checks (tear down: `make demo-down`)
#
# Checks: the heartbeat runs through the worker and the proxy and passes; a host off the allowlist
# is refused by the proxy; metrics and traces reach their stand-ins; an agent token cannot change a routine's
# autonomy; a STOPped fleet's run is recorded as skipped, and a STOP cancels a run in flight; the worker refuses a wrong token; the candidate
# worker (profile candidate) is healthy on its own state volume and its routine cannot leave shadow; the
# heartbeat's model step calls the LiteLLM stand-in through the model client, with the models section
# from flotillad.yaml, the key from the fleet's secrets file and the proxy, and its span is exported. The UI
# in Chromium behind a Caddy TLS front (ui_browser.py): sign-in, the session cookie, a pause and a resume
# from the fleet page, phone width, the fonts, no CSP violations, sign-out. The host agent (ADR-0006): a CA
# made with `flotilla ca`, enrolment, connect and heartbeat, a flotillad restart, revocation. Routine versions
# (docs/wip/v2-design.md): e2e/versioned, a routine folder with versions v1 to v6, promoted, rolled back and
# retired through the CLI (an agent refused, evidence and reasons required), a v2 plan withdrawn by a rollback,
# a promote with a run in flight, a rollback with a run in flight (the run cancelled, the case back with the acting
# routine), a changed live folder refusing flotillad's start and -check (and naming the way out), a changed shadow
# starting fine; a flat routine's playbook edit re-recorded at the start, and its move to the folder layout
# re-recorded at the start when every recorded key and file is equal (-check prints the comparison and the routine fields against the stored snapshot), refused when one byte differs; a schema-25 store migrated and started on by the real image
# (every routine's v1 live, the history version 1); the autonomy rename (a routine at record reads FLEETD_STAGE=shadow);
# the same forms in Chromium. Live reads (RFC-0014 §4.8, section `reads`, deploy/e2e/mcp_stub.py): a routine whose station declares reads from an MCP
# server, on the reads worker behind its own proxy, records them (a result over 8192 characters whole), is refused a tool it did not declare, is replayed
# from the record with the server down, and its shadows are served the record, never the server (a read the record lacks fails the shadow).
# Case 1 (RFC-0014 §4.5, section `ask`): a signed `ask` call opens a question case whose asker is looked up in the people file (a known person, an
# unknown id); knowledge/answer reads documents and tasks on a stub and the library on second-brain's real server (sb serve, from the checkout SB_DIR),
# records the reads, cites only what it read (an answer citing a link no read returned escalates), and plans plumbing.reply at propose; approved it is
# sent once, signed, to the stub plumbing, rejected it sends nothing; its shadow is served the record, and an offline replay with the servers stopped
# makes the same plan. E2E_MUTANT=shadow-reads-live (a shadow reads live, not from the record) must turn it red.
# Never on the production engine (the installation's ADR-0009): it uses the `dev` context only.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
D="docker --context ${DOCKER_CONTEXT_DEV:-dev}"
$D version >/dev/null 2>&1 || { echo "no Docker context '${DOCKER_CONTEXT_DEV:-dev}': name your development engine's context in DOCKER_CONTEXT_DEV (DOCKER_CONTEXT_DEV=default on a machine with one engine)" >&2; exit 2; }
P=flotilla-e2e
TAG=e2e-local
LABEL=flotilla.e2e=1
NETS="e2e-backend e2e-litellm e2e-glitchtip e2e-langfuse e2e-plumbing e2e-mcp"
PLAYWRIGHT_IMAGE=local/flotilla-e2e-browser:$TAG
DEMO_PORT="${DEMO_PORT:-8443}"
# the model contract check measures real models; against the stand-in gateway it can only fail: off in the demo
if [ "${DEMO:-}" = 1 ]; then DEMO_DISABLED=", platform/model-contract"; else DEMO_DISABLED=""; fi
# The mutation checks (E2E_MUTANT=name, run by deploy/e2e/mutant.sh, which passes only if the run goes red on the check the fault must break):
#   shadow-write-cred, shadow-egress-open    the shadow's environment and egress (the hostile shadow's and the stand-ins' checks)
#   station-live-key, station-span-mark      the stations section (the images are built from a patched copy of the tree)
#   matrix-live-write                        a generated shadow writes a file the live ops-read worker shares (the matrix's `unchanged` cell)
#   noeffect-vacuous                         a generated shadow does nothing and writes no record (the matrix's `shadow` cell)
#   required-fallback                        flotillad takes a worker's own token as a caller again under required (the run-token probes)
#   caps-skip                                flotillad takes any worker or agent for run-token capable (the old worker's and the old agent's refusal)
#   shadow-live-handler                      a shadow's report goes through the live path too: e2e/hostile-brief's case, brief line and Slack ask
#   shadow-reads-live                        a shadow's reads go to the live read path instead of the live run's record (section ask: the shadow's plan differs)
# E2E_MUTANT=shadow-write-cred is a mutation check: it puts a write credential into the shadow worker's read environment, and the hostile shadow's
# check must go red. (E2E_ACL_ACTION=open is the proxy's.)
# the shadow check's two routines are enabled in their specs and held off in the config (a shadow of a disabled routine does not run) until
# the shadow section frees them
if [ "${DEMO:-}" = 1 ]; then SHADOW_HELD=""; else SHADOW_HELD=", e2e/shadow-monitor, e2e/cpu, e2e/station-monitor, e2e/reads"; fi
# E2E_MUTANT=station-live-key puts a shadow run's calls (their station and texts with them) under the live routine's key, and station-span-mark drops the
# shadow mark from every span of a shadow run: the stations section's checks (and the older ones the same fault breaks) must go red. Each builds the
# images from a patched copy of the tree (the checks themselves are never patched).
BUILD_CTX="$ROOT"
if [ "${DEMO:-}" = 1 ]; then UI_HOST="localhost:$DEMO_PORT"; else UI_HOST=flotilla.e2e; fi
# The browser check's TLS front, standing in for the installation's caddy-tailscale (Caddy 2.11.9).
CADDY_IMAGE=caddy@sha256:003a97808e781aa9f7e75cc2f43d34d461099031faf5e20faefb9f61126836f0
WORK="${E2E_WORK:-$ROOT/.e2e}"
fails=0
ok() { echo "PASS $*"; }
bad() { echo "FAIL $*"; fails=$((fails + 1)); }
# E2E_SECTIONS=a,b runs only those sections (the build, the config and `up` always run); unset runs all. `noeffect` is versions, hostagent,
# versionscase, shadows, stations and matrix: the no-effect proof (docs/wip/v2-design.md section 4) and the sections whose state it reads (the
# shadow monitor's pairs count the case that versionscase leaves open on the host agent's target, which needs versions' routine). The other sections
# lean on each other as well (ui reads the shadows' and stations' pairs, plumbing hooks' meeting case, remote comes after ui); name those together. The first
# admin is made by ui, hooks and ask alike (they approve as a user, never with the operator token).
# `ask` (RFC-0014 case 1) needs nothing but the first admin (its approvals), which it makes if ui does not; it opens its own cases and approves its own
# plans. Its shadows need flotillad's `shadows:` (reads adds it, ask adds it, and restarts flotillad, when it is not there).
# It reads second-brain's real read server from a checkout: SB_DIR (default /work/second-brain/main; a CI job checks it out).
sections_known="model telemetry cases watching business world triage bugfix controls ledger runtokens inputs candidate versions confighash hostagent versionscase shadows stations reads matrix ui hooks plumbing ask remote hardening"
want() {
  [ -z "${E2E_SECTIONS:-}" ] && return 0
  local s="$1"; case "$s" in versions|hostagent|versionscase|shadows|stations|matrix) s="$s,noeffect" ;; esac
  for x in ${s//,/ }; do case ",$E2E_SECTIONS," in *",$x,"*) return 0 ;; esac; done
  return 1
}
if [ -n "${E2E_SECTIONS:-}" ]; then
  for x in ${E2E_SECTIONS//,/ }; do case " $sections_known noeffect " in *" $x "*) ;; *) echo "E2E_SECTIONS: unknown section '$x' (known: $sections_known noeffect)" >&2; exit 2 ;; esac; done
fi

E2E_ENV=(FLOTILLA_REGISTRY=local FLOTILLA_TAG="$TAG" FLOTILLA_CONFIG="$WORK/config" TRIAGE_ENV_FILE="$WORK/config/fleet.env"
  FLOTILLA_VOLUME_PREFIX=flotilla-e2e FLOTILLA_STATE_VOLUME=flotilla-e2e-state TRIAGE_STATE_VOLUME=flotilla-e2e-triage-state
  COMPOSE_PROFILES=candidate,ops,bugfix,shadow,plumbing,reads TRIAGE_CANDIDATE_STATE_VOLUME=flotilla-e2e-triage-candidate-state TRIAGE_READ_ENV_FILE="$WORK/config/fleet-read.env"
  NET_BACKEND=e2e-backend NET_LITELLM=e2e-litellm NET_GLITCHTIP=e2e-glitchtip NET_LANGFUSE=e2e-langfuse NET_UI=e2e-ui NET_PLUMBING=e2e-plumbing NET_MCP=e2e-mcp
  DOCKER_CONTEXT="${DOCKER_CONTEXT_DEV:-dev}")
# E2E_MUTANT=matrix-live-write gives the ops-read worker and its shadow worker one folder in common, which a generated shadow version then writes to
COMPOSE_MUT=()
[[ ",${E2E_MUTANT:-}," != *,matrix-live-write,* ]] || COMPOSE_MUT=(-f "$ROOT/deploy/e2e/docker-compose.mutant-live-write.yml")
compose() { env "${E2E_ENV[@]}" $D compose -p $P -f "$ROOT/deploy/compose/docker-compose.yml" -f "$ROOT/deploy/e2e/docker-compose.e2e.yml" "${COMPOSE_MUT[@]}" "$@"; }

down() {
  compose down -v --remove-orphans >/dev/null 2>&1
  for c in $($D ps -aq --filter label=$LABEL); do $D rm -f "$c" >/dev/null; done
  for c in flotilla-e2e-remote-worker flotilla-e2e-remote-link flotilla-e2e-remote-link-old e2e-required e2e-accept e2e-oldworker e2e-newworker e2e-upgrade-flotillad e2e-upgrade-start; do $D rm -f "$c" >/dev/null 2>&1; done
  for v in flotilla-e2e-state flotilla-e2e-triage-state flotilla-e2e-triage-candidate-state flotilla-e2e-flotillad-secrets flotilla-e2e-worker-triage-secrets flotilla-e2e-worker-triage-candidate-secrets flotilla-e2e-worker-triage-shadow-secrets flotilla-e2e-triage-snapshots flotilla-e2e-triage-shadow-state flotilla-e2e-required flotilla-e2e-accept flotilla-e2e-worker-ops-read-secrets flotilla-e2e-worker-ops-read-shadow-secrets flotilla-e2e-worker-ops-act-host-secrets flotilla-e2e-worker-bugfix-secrets flotilla-e2e-worker-bugfix-act-secrets flotilla-e2e-worker-plumbing-secrets flotilla-e2e-worker-reads-secrets flotilla-e2e-bugfix-scratch flotilla-e2e-host-state flotilla-e2e-host-audit flotilla-e2e-store25 flotilla-e2e-mut-live; do
    $D volume rm "$v" >/dev/null 2>&1
  done
  for n in $NETS; do $D network rm "$n" >/dev/null 2>&1; done
  rm -rf "$WORK"
}
if [ "${1:-}" = down ]; then down; echo "e2e torn down"; exit 0; fi
down

echo "== build"
case ",${E2E_MUTANT:-}," in *,station-live-key,*|*,station-span-mark,*|*,required-fallback,*|*,caps-skip,*|*,shadow-live-handler,*|*,shadow-reads-live,*)
  BUILD_CTX="$WORK/mutated-src"; mkdir -p "$BUILD_CTX"
  (cd "$ROOT" && tar --exclude=./.git --exclude=./.e2e --exclude=./.venv --exclude=./site --exclude=./node_modules --exclude=./.claude -cf - .) | tar -xf - -C "$BUILD_CTX"
  if [[ ",${E2E_MUTANT}," == *,station-live-key,* ]]; then
    sed -i 's/calls\[i\] = store.ModelCall{RunID: runID, Routine: key,/calls[i] = store.ModelCall{RunID: runID, Routine: t.routineID,/' "$BUILD_CTX/internal/sched/shadow.go"
    cmp -s "$ROOT/internal/sched/shadow.go" "$BUILD_CTX/internal/sched/shadow.go" && { echo "MUTANT station-live-key did not change shadow.go"; exit 1; }
  fi
  if [[ ",${E2E_MUTANT}," == *,station-span-mark,* ]]; then
    sed -i 's/        out\["flotilla.shadow"\] = True/        pass/' "$BUILD_CTX/runtime/telemetry.py"
    cmp -s "$ROOT/runtime/telemetry.py" "$BUILD_CTX/runtime/telemetry.py" && { echo "MUTANT station-span-mark did not change telemetry.py"; exit 1; }
  fi
  if [[ ",${E2E_MUTANT}," == *,required-fallback,* ]]; then   # a worker's own token is a caller again under required
    sed -i 's/^\tif cfg.RunTokens == RunTokensRequired {$/\tif false \&\& cfg.RunTokens == RunTokensRequired {/' "$BUILD_CTX/cmd/flotillad/shadows.go"
    cmp -s "$ROOT/cmd/flotillad/shadows.go" "$BUILD_CTX/cmd/flotillad/shadows.go" && { echo "MUTANT required-fallback did not change shadows.go"; exit 1; }
  fi
  if [[ ",${E2E_MUTANT}," == *,shadow-live-handler,* ]]; then   # a shadow run's report is handled by the live path too: a case, a brief line
    sed -i 's/^\tend := store.ShadowDone$/\tif s.AfterRun != nil \&\& outcome != nil {\n\t\ts.AfterRun(fctx, sj, \&out, outcome)\n\t}\n\tend := store.ShadowDone/' "$BUILD_CTX/internal/sched/shadow.go"
    cmp -s "$ROOT/internal/sched/shadow.go" "$BUILD_CTX/internal/sched/shadow.go" && { echo "MUTANT shadow-live-handler did not change shadow.go"; exit 1; }
  fi
  if [[ ",${E2E_MUTANT}," == *,shadow-reads-live,* ]]; then   # a shadow's reads go to the live read path (reads.py) instead of the live run's record
    sed -i 's/^            tool = lambda name, args: served(name, args) if name in read_names or rep.knows(station, name) else fleet_tool(name, args)$/            pass/' "$BUILD_CTX/runtime/harness.py"
    cmp -s "$ROOT/runtime/harness.py" "$BUILD_CTX/runtime/harness.py" && { echo "MUTANT shadow-reads-live did not change harness.py"; exit 1; }
  fi
  if [[ ",${E2E_MUTANT}," == *,caps-skip,* ]]; then   # flotillad takes any worker and any agent for run-token capable
    sed -i 's/^\tif cfg.RunTokens != RunTokensRequired {$/\tif true || cfg.RunTokens != RunTokensRequired {/; s/link.RequireRunToken = cfg.RunTokens == RunTokensRequired/link.RequireRunToken = false/' "$BUILD_CTX/cmd/flotillad/main.go"
    cmp -s "$ROOT/cmd/flotillad/main.go" "$BUILD_CTX/cmd/flotillad/main.go" && { echo "MUTANT caps-skip did not change main.go"; exit 1; }
  fi ;;
esac
for i in flotillad worker-triage worker-ops worker-bugfix smokescreen; do
  $D build -q -f "$BUILD_CTX/deploy/docker/$i.Dockerfile" --build-arg VERSION=$TAG -t local/flotilla-$i:$TAG "$BUILD_CTX" >/dev/null \
    || { echo "BUILD FAIL $i"; exit 1; }
done
HOST_AGENT_IMAGE=local/flotilla-e2e-host-agent:$TAG
$D build -q -f "$ROOT/deploy/e2e/host-agent.Dockerfile" --build-arg VERSION=$TAG -t "$HOST_AGENT_IMAGE" "$ROOT" >/dev/null \
  || { echo "BUILD FAIL e2e-host-agent"; exit 1; }
if [ "${DEMO:-}" != 1 ] && want remote; then
  OLD_SRC="$WORK/old-agent-src"; mkdir -p "$OLD_SRC"
  (cd "$ROOT" && tar --exclude=./.git --exclude=./.e2e --exclude=./.venv --exclude=./site --exclude=./node_modules --exclude=./.claude -cf - .) | tar -xf - -C "$OLD_SRC"
  sed -i 's/, Caps: \[\]string{hostwire.CapRunToken}//' "$OLD_SRC/internal/hostagent/link.go"
  cmp -s "$ROOT/internal/hostagent/link.go" "$OLD_SRC/internal/hostagent/link.go" && { echo "the old agent's source is not changed"; exit 1; }
  $D build -q -f "$OLD_SRC/deploy/docker/worker-bugfix.Dockerfile" --build-arg VERSION=$TAG -t local/flotilla-worker-bugfix-old:$TAG "$OLD_SRC" >/dev/null \
    || { echo "BUILD FAIL old agent"; exit 1; }
  rm -rf "$OLD_SRC"
fi
$D build -q -f "$ROOT/deploy/e2e/browser.Dockerfile" -t "$PLAYWRIGHT_IMAGE" "$ROOT/deploy/e2e" >/dev/null \
  || { echo "BUILD FAIL e2e-browser"; exit 1; }
ok "images built"

echo "== config and stand-ins"
mkdir -p "$WORK/config"
# the specs: the shipped fleets and the e2e-only fleet (its host.true path, H9c); its registry extension
mkdir -p "$WORK/config/specs" && cp -r "$ROOT/fleets/." "$WORK/config/specs/"
# the e2e-only fleet tests the platform itself (a hang, a remote echo, harmless host actions): not in the demo
[ "${DEMO:-}" = 1 ] || cp -r "$ROOT/deploy/e2e/fleets/." "$WORK/config/specs/"
# one run-token probe per worker class (token-probe on triage is the fleet's own file); the remote worker's is run in its section
[ "${DEMO:-}" = 1 ] || python3 "$ROOT/deploy/e2e/make_probes.py" "$WORK/config/specs" triage-candidate ops-read ops-act-host bugfix remote-e2e:/remote/worker.token >/dev/null
# RFC-0014 case 1: knowledge/answer runs on the reads worker; its v2 (v1 and a marker) is the shadow section ask replays from the live run's record
[ "${DEMO:-}" = 1 ] || python3 "$ROOT/deploy/e2e/make_shadows.py" "$WORK/config/specs" reads | grep >/dev/null "^knowledge/answer$" || { echo "no shadow v2 made for knowledge/answer"; exit 1; }
cp "$ROOT/deploy/e2e/registry-e2e.json" "$WORK/config/registry-e2e.json"
printf '%s\n' "${E2E_ENV[@]}" > "$WORK/.env"
tok() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
OP=$(tok); AGENT=$(tok); WT=$(tok)
hash() { printf %s "$1" | sha256sum | cut -d' ' -f1; }
printf %s "$WT" > "$WORK/config/worker-triage.token"
printf %s "$(tok)" > "$WORK/config/worker-triage-shadow.token"      # the shadow worker's own, and flotillad never takes it for a caller
printf %s "$(tok)" > "$WORK/config/worker-triage-candidate.token"   # its own: flotillad refuses two workers that share one
tok > "$WORK/config/worker-ops-read.token"; tok > "$WORK/config/worker-ops-act-host.token"; tok > "$WORK/config/worker-ops-read-shadow.token"
tok > "$WORK/config/worker-bugfix.token"; tok > "$WORK/config/worker-bugfix-act.token"; tok > "$WORK/config/worker-plumbing.token"
# RFC-0014 §4.8: the reads worker's token, its model key (the endpoint's key_env, as the shadow worker's read environment has it) and the stub MCP
# server's bearer token, which only the reads worker's volume (secrets.sh) and the stub hold
tok > "$WORK/config/worker-reads.token"; printf 'E2E_LLM_KEY=%s\n' "$(tok)" > "$WORK/config/reads-model.env"
mkdir -p "$WORK/config/mcp" && (umask 077; tok > "$WORK/config/mcp/lab.token")
# RFC-0014 case 1 (section ask): the documents and tasks servers (one stub, two names) share a token; second-brain's server has its own
(umask 077; tok > "$WORK/config/mcp/docs.token"; cp "$WORK/config/mcp/docs.token" "$WORK/config/mcp/tasks.token"; tok > "$WORK/config/mcp/library.token")
# who the asker is: the people file (secrets.sh copies it into flotillad's secret volume), the slack ids of the question's askers
cat > "$WORK/config/people.yaml" <<EOF
people:
  - {email: ada@example.org, name: Ada Example, ids: {slack: U0E2EASK01}}
  - {email: slack@example.org, name: E2E Slack, ids: {slack: U0E2E000001}}
EOF
chmod 600 "$WORK/config/people.yaml"
# RFC-0014 §4.6: each plumbing.call callback's signing secret, shared with the stub plumbing (deploy/e2e/plumbing_stub.py)
mkdir -p "$WORK/config/plumbing" && (umask 077; for n in create_tasks post_decisions reply post retract; do tok > "$WORK/config/plumbing/$n.secret"; done)
tok > "$WORK/config/bugfix-model.key"   # a stand-in: the e2e's bugfix routines that would use it are disabled
printf %s "$OP" > "$WORK/config/cli.token"          # the CLI inside flotillad acts as the operator here
printf %s "$AGENT" > "$WORK/agent.token"
DEPLOYTOK=$(tok); printf %s "$DEPLOYTOK" > "$WORK/config/deploy.token"   # the deploy stand-down's caller
cat > "$WORK/config/callers.yaml" <<EOF
callers:
  - {name: operator, role: operator, token_sha256: $(hash "$OP")}
  - {name: e2e-agent, role: agent, token_sha256: $(hash "$AGENT")}
  - {name: deploy, role: agent, token_sha256: $(hash "$DEPLOYTOK")}
EOF
cat > "$WORK/config/flotillad.yaml" <<EOF
listen: 127.0.0.1:8484
worker_listen: 0.0.0.0:8485
ui_listen: 0.0.0.0:8486
ui_host: $UI_HOST
specs_dir: /etc/flotilla/specs          # the image's fleets plus the e2e fleet (deploy/e2e/fleets), staged below
examples_dir: /opt/flotilla/examples    # the catalogue: listed as examples, never run
registry_files: [/opt/flotilla/host/registry.json, /etc/flotilla/registry-e2e.json]   # plus the e2e's host.true
state_dir: /var/lib/flotilla
environment: e2e
timezone: Europe/Amsterdam
brief_at: "00:00"                       # the morning brief is written at the first start of the day: the matrix compares it before and after the shadows
vm_url: http://victoriametrics:8080
otlp_traces_url: http://victoriatraces:8080/insert/opentelemetry/v1/traces
callers_file: /etc/flotilla/callers.yaml
workers:
  triage: {url: "http://worker-triage:8081", token_file: /run/flotilla/worker-triage.token}
  triage-candidate: {url: "http://worker-triage-candidate:8081", token_file: /run/flotilla/worker-triage-candidate.token}
  triage-shadow: {url: "http://worker-triage-shadow:8081", token_file: /run/flotilla/worker-triage-shadow.token, shadow: true}
  ops-read: {url: "http://worker-ops-read:8081", token_file: /run/flotilla/worker-ops-read.token}
  ops-read-shadow: {url: "http://worker-ops-read-shadow:8081", token_file: /run/flotilla/worker-ops-read-shadow.token, shadow: true}
  ops-act-host: {url: "http://worker-ops-act-host:8081", token_file: /run/flotilla/worker-ops-act-host.token}
  bugfix: {url: "http://worker-bugfix:8081", token_file: /run/flotilla/worker-bugfix.token}
  bugfix-act: {url: "http://worker-bugfix-act:8081", token_file: /run/flotilla/worker-bugfix-act.token}
  plumbing: {url: "http://worker-plumbing:8081", token_file: /run/flotilla/worker-plumbing.token}   # RFC-0014 §4.6: plumbing.call's own worker
  reads: {url: "http://worker-reads:8081", token_file: /run/flotilla/worker-reads.token}   # RFC-0014 §4.8: the one worker that holds a read server's address and token
  # the fix routines' own workers on the host are remote (a second machine); the e2e runs neither routine, so they
  # name the local bugfix worker
  bugfix-fix: {url: "http://worker-bugfix:8081", token_file: /run/flotilla/worker-bugfix.token}
  remote-e2e: {remote: w-remote-e2e}   # RFC-0011: a worker reached through its link
# the candidate triage needs a candidate model; bugfix/fix clones the repo, installs its toolchain and runs a
# coding agent on a real model through many tool calls: a stand-in for that would show the stand-in, not
# flotilla. Its unit tests cover it on local repos (bugfix/test_fix.py); here its intake opens the case.
disabled_routines: [bugfix/fix$DEMO_DISABLED$SHADOW_HELD]
models:
  endpoints:
    gateway: {base_url: "http://litellm:8080/v1", kind: litellm, key_env: E2E_LLM_KEY, defaults: {tools: native, json: object, cost: reported}}
    gateway-bugfix: {base_url: "http://litellm:8080/v1", kind: litellm, key_file: /run/flotilla/model.key}
  aliases:
    triage-flash: {endpoint: gateway, model: fleet-triage-flash}
    triage-pro: {endpoint: gateway, model: fleet-triage-pro}
    bugfix-flash: {endpoint: gateway-bugfix, model: fleet-bugfix-flash}
    bugfix-reviewer: {endpoint: gateway-bugfix, model: fleet-bugfix-reviewer}
    # one key per fix routine on the host (2026-11-16); here every alias reads the same stand-in key
    fix-flash: {endpoint: gateway-bugfix, model: fleet-bugfix-flash}
    fix-reviewer: {endpoint: gateway-bugfix, model: fleet-bugfix-reviewer}
    meetings-notes: {endpoint: gateway, model: fleet-meetings-notes}   # RFC-0014 case 3: meetings/notes' station extract
    reads-answer: {endpoint: gateway, model: fleet-reads-answer}   # RFC-0014 §4.8: e2e/reads' station; the key is E2E_LLM_KEY on the reads worker and on the shadow worker
    knowledge-answer: {endpoint: gateway, model: fleet-knowledge-answer}   # RFC-0014 case 1: knowledge/answer's station
directory: {source: file, path: /run/flotilla/people.yaml}
hosts: {listen: "0.0.0.0:8487", ca_dir: /run/flotilla, server_names: [flotillad], heartbeat: 2s, signing_key_file: /run/flotilla/signing.key, signing_key_id: e2e1}
# Slack approvals through the host e2e-host (section hostagent): its agent is no connector, so it logs each plan flotillad asks it to post and posts none.
# That log and the store's slack_asks are the e2e's recording of what Slack would have been sent (the matrix compares them around the shadow runs).
slack: {connector_host: e2e-host, team_id: T0E2E000001, ui_url: "https://$UI_HOST", users: {U0E2E000001: e2e-slack}}
# RFC-0014 §4.2: the stub plumbing on e2e-plumbing calls these (section hooks); meeting's kind is the plumbing section's (meetings/notes, paused until then), smoke closes its own case
hooks_listen: 0.0.0.0:8488
hooks:
  - name: meeting
    secret_file: /run/flotilla/hook-meeting.secret
    kind: meeting
    title: "A meeting's notes arrived"
    project: meetings
    target: file
    target_pattern: '[A-Za-z0-9._-]{1,120}'
    fields: {file: {type: text, max: 120}, date: {type: text, max: 10}, text: {type: text, max: 2000}}
    max_body: 4096
  - name: ask   # RFC-0014 case 1: a question in chat; its sender's slack id is looked up in the directory when the case opens
    secret_file: /run/flotilla/hook-ask.secret
    kind: question
    title: "A question was asked"
    project: knowledge
    target: thread
    target_pattern: '[A-Za-z0-9._-]{1,64}'
    asker: {field: user, tool: slack}
    fields: {thread: {type: text, max: 64}, user: {type: text, max: 40}, text: {type: text, max: 2000}}
    max_body: 4096
  - name: smoke
    secret_file: /run/flotilla/hook-smoke.secret
    kind: smoke
    title: "The plumbing's smoke test"
    project: platform
    target: probe
    target_pattern: '[a-z0-9-]{1,40}'
    fields: {probe: {type: text, max: 40}}
    close_on_arrival: true
EOF
echo "E2E_ONLY=1" > "$WORK/config/fleet.env"
printf 'SHADOW_READ_ONLY=%s\n' "$(tok)" > "$WORK/config/fleet-read.env"   # what a shadow worker may read: no write credential in it
printf 'E2E_LLM_KEY=%s\n' "$(tok)" >> "$WORK/config/fleet-read.env"   # a model key: the shadow's own, never a write credential
[[ ",${E2E_MUTANT:-}," == *,shadow-write-cred,* ]] && printf 'GITHUB_TOKEN=%s\n' "$(tok)" >> "$WORK/config/fleet-read.env"
[ "${DEMO:-}" != 1 ] || sed -i "/^  remote-e2e: /d" "$WORK/config/flotillad.yaml"   # the e2e fleet's remote worker: not in the demo
cat > "$WORK/config/worker-triage.env" <<EOF
ROUTINE_HEARTBEAT_ALLOWED_URL=http://litellm:8080/health/liveliness
ROUTINE_HEARTBEAT_DENIED_URL=http://forbidden:8080/
ROUTINE_HEARTBEAT_MODEL_ALIAS=triage-flash
ROUTINE_SF_SERVICE=triage-fleet
ROUTINE_SF_GLITCHTIP_SYNC=1
ROUTINE_SF_GLITCHTIP_URL=https://errors.example.com
ROUTINE_SF_GRAFANA_URL=https://grafana.example.com
ROUTINE_SF_OPENROUTER_URL=http://litellm:8080/v1/chat/completions
ROUTINE_SF_TRIAGE_MODEL=fleet-triage-flash
ROUTINE_SF_VM_URL=http://victoriametrics:8080
ROUTINE_SF_VT_URL=http://victoriatraces:8080
EOF
# the ops workers' installation facts; the VictoriaMetrics stand-in answers the backup's age (25.5h)
cat > "$WORK/config/worker-ops.env" <<EOF
ROUTINE_VM_URL=http://victoriametrics:8080
ROUTINE_BACKUP_INSTANCE=backup-01
ROUTINE_BACKUP_TARGET=host:backup-01
EOF
# plumbing.call's callbacks: the stub plumbing on e2e-plumbing answers as the installation's n8n (deploy/e2e/plumbing_stub.py)
for n in create_tasks post_decisions reply post retract; do
  N="$(echo "$n" | tr a-z A-Z)"
  printf 'ROUTINE_PLUMBING_%s_URL=http://n8n:8090/webhook/%s\nROUTINE_PLUMBING_%s_SECRET_FILE=/run/flotilla/plumbing-%s.secret\n' "$N" "$n" "$N" "$n"
done > "$WORK/config/worker-plumbing.env"
# The outside world the example fleets read (sites, deps): deploy/e2e/world.py answers as these hosts, with a
# certificate from the stack's own CA (the workers that reach it trust that CA, docker-compose.e2e.yml). The
# certificate is valid for 9 days, below the example sites' 21: the certificate check has something to find.
WORLD_HOSTS="www.example.com docs.example.com shop.example.org errors.example.com grafana.example.com api.github.com pypi.org api.osv.dev"
WORLD_HOSTS_ACL="$(echo $WORLD_HOSTS | sed 's/ /, /g')"
W="$WORK/config/world"; mkdir -p "$W"
openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=flotilla e2e world CA" -keyout "$W/ca.key" -out "$W/ca.pem" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null \
  && openssl req -newkey rsa:2048 -nodes -subj "/CN=www.example.com" -keyout "$W/world.key" -out "$W/world.csr" 2>/dev/null \
  && printf 'subjectAltName=%s\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid\n' \
       "$(echo $WORLD_HOSTS | sed 's/[^ ]*/DNS:&/g; s/ /,/g')" > "$W/ext" \
  && openssl x509 -req -in "$W/world.csr" -CA "$W/ca.pem" -CAkey "$W/ca.key" -CAcreateserial -days 9 -extfile "$W/ext" \
       -out "$W/world.pem" 2>/dev/null \
  || { echo "the world's certificate: openssl failed"; exit 1; }
rm -f "$W/ca.key" "$W/world.csr" "$W/ext" "$W/ca.srl"; chmod 755 "$W"; chmod 644 "$W"/*
cat > "$WORK/config/smokescreen.yaml" <<EOF
ip: 0.0.0.0
port: 4750
acl_file: /etc/smokescreen/acl.yaml
allow_missing_role: true
allow_ranges: [172.16.0.0/12]
EOF
cat > "$WORK/config/egress-acl.yaml" <<EOF
version: v1
services: []
default:
  project: flotilla
  action: ${E2E_ACL_ACTION:-enforce}   # E2E_ACL_ACTION=open is the mutation check: the run must FAIL
  allowed_domains: [victoriametrics, victoriatraces, litellm, $WORLD_HOSTS_ACL]
EOF
# a shadow's proxy has no route to the write host (api.github.com: issues, comments, PRs); E2E_MUTANT=shadow-egress-open puts it back
SHADOW_HOSTS_ACL="$(echo "$WORLD_HOSTS_ACL" | sed 's/api\.github\.com, //; s/, api\.github\.com//')"
[[ ",${E2E_MUTANT:-}," != *,shadow-egress-open,* ]] || SHADOW_HOSTS_ACL="$WORLD_HOSTS_ACL"
cat > "$WORK/config/egress-shadow-acl.yaml" <<EOF
version: v1
services: []
default:
  project: flotilla-shadow
  action: enforce
  allowed_domains: [victoriametrics, victoriatraces, litellm, $SHADOW_HOSTS_ACL]
EOF
cat > "$WORK/config/egress-bugfix-acl.yaml" <<EOF
version: v1
services: []
default:
  project: flotilla-bugfix
  action: enforce
  allowed_domains: [litellm, api.github.com, pypi.org, api.osv.dev]
EOF
# plumbing.call's proxy: the stub plumbing (n8n, on e2e-plumbing) and nothing else; its settings, with that network's own range,
# are written once the network exists (below)
cat > "$WORK/config/egress-plumbing-acl.yaml" <<EOF
version: v1
services: []
default:
  project: flotilla-plumbing
  action: enforce
  allowed_domains: [n8n]
EOF
# RFC-0014 §4.8: the reads worker is told the stub MCP server's address (a placeholder host: an installation sets its own) and where its token is;
# its proxy admits that host and the model gateway, nothing else (its settings, with the networks' ranges, are written once the networks exist)
printf 'ROUTINE_MCP_LAB_URL=http://mcp.e2e.example:8091/mcp\nROUTINE_MCP_LAB_TOKEN_FILE=/run/flotilla/mcp-lab.token\n' > "$WORK/config/worker-reads.env"
# case 1's three servers (section ask starts them): documents and tasks on the stub, the library on second-brain's own server (sb serve)
for s in docs:8091 tasks:8091 library:8790; do
  printf 'ROUTINE_MCP_%s_URL=http://%s.e2e.example:%s/mcp\nROUTINE_MCP_%s_TOKEN_FILE=/run/flotilla/mcp-%s.token\n' "$(echo "${s%%:*}" | tr a-z A-Z)" "${s%%:*}" "${s##*:}" "$(echo "${s%%:*}" | tr a-z A-Z)" "${s%%:*}"
done >> "$WORK/config/worker-reads.env"
cat > "$WORK/config/egress-reads-acl.yaml" <<EOF
version: v1
services: []
default:
  project: flotilla-reads
  action: enforce
  allowed_domains: [litellm, mcp.e2e.example, docs.e2e.example, tasks.e2e.example, library.e2e.example]
EOF
find "$WORK/config" -maxdepth 1 -type f -exec chmod 644 {} +   # the files; the specs folder stays traversable
chmod 600 "$WORK/config/people.yaml"   # a people file is the owner's alone (secrets.sh copies it as 600)
chmod -R a+rX "$WORK/config/specs"
for n in $NETS; do $D network create --label $LABEL $([ "$n" = e2e-plumbing ] || [ "$n" = e2e-mcp ] && echo --internal) "$n" >/dev/null; done   # the plumbing tool's network is internal, as the installation's is
# the plumbing proxy allows the plumbing network's range alone, as an installation's does (deploy/compose/README.md): smokescreen
# refuses every private address outside allow_ranges (2026-11-22: 172.16.0.0/12 missed e2e-plumbing's 192.168.16.0/20, a 407)
PLUMBING_RANGE="$($D network inspect e2e-plumbing --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}')"
sed "s|^allow_ranges: .*|allow_ranges: [$PLUMBING_RANGE]|" "$WORK/config/smokescreen.yaml" > "$WORK/config/smokescreen-plumbing.yaml" && chmod 644 "$WORK/config/smokescreen-plumbing.yaml"
grep >/dev/null "^allow_ranges: \[[0-9./]*\]$" "$WORK/config/smokescreen-plumbing.yaml" || { echo "the plumbing proxy's range: '$PLUMBING_RANGE'"; exit 1; }
# the reads proxy reaches the MCP network (its own range) and the model gateway's (inside the default 172.16.0.0/12), and nothing else
MCP_RANGE="$($D network inspect e2e-mcp --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}')"
sed "s|^allow_ranges: .*|allow_ranges: [172.16.0.0/12, $MCP_RANGE]|" "$WORK/config/smokescreen.yaml" > "$WORK/config/smokescreen-reads.yaml" && chmod 644 "$WORK/config/smokescreen-reads.yaml"
grep >/dev/null "^allow_ranges: \[172.16.0.0/12, [0-9./]*\]$" "$WORK/config/smokescreen-reads.yaml" || { echo "the reads proxy's range: '$MCP_RANGE'"; exit 1; }
stub() { $D run -d --label $LABEL --name "e2e-$1" --network "$2" --network-alias "$1" --memory 64m \
  -v "$ROOT/deploy/e2e/stub.py:/stub.py:ro" python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b \
  python3 -u /stub.py "$1" >/dev/null; }
stub victoriametrics e2e-backend; stub victoriatraces e2e-backend; stub litellm e2e-litellm
# off the allowlist but reachable and answering: refusing it is the proxy's doing, nothing else
stub forbidden e2e-backend
# RFC-0014 §4.6: the stub plumbing, as the installation's n8n on its internal network, verifying each callback's signature
$D run -d --label $LABEL --name e2e-n8n --network e2e-plumbing --network-alias n8n --memory 64m \
  -v "$ROOT/deploy/e2e/plumbing_stub.py:/plumbing_stub.py:ro" -v "$WORK/config/plumbing:/secrets:ro" \
  python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b python3 -u /plumbing_stub.py 8090 >/dev/null \
  || { echo "the stub plumbing did not start"; exit 1; }
# RFC-0014 §4.8: the stub MCP server (n8n's MCP Server Trigger's stand-in) on its own internal network, under a placeholder host name
$D run -d --label $LABEL --name e2e-mcp --network e2e-mcp --network-alias mcp.e2e.example --memory 64m \
  -v "$ROOT/deploy/e2e/mcp_stub.py:/mcp_stub.py:ro" -v "$WORK/config/mcp:/secrets:ro" \
  python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b python3 -u /mcp_stub.py 8091 >/dev/null \
  || { echo "the stub MCP server did not start"; exit 1; }
WORLD_ALIASES="$(for h in $WORLD_HOSTS; do printf -- '--alias %s ' "$h"; done)"
$D run -d --label $LABEL --name e2e-world --network e2e-backend $(echo "$WORLD_ALIASES" | sed 's/--alias/--network-alias/g') \
  --memory 64m -v "$ROOT/deploy/e2e/world.py:/world.py:ro" -v "$W:/w:ro" \
  python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b python3 -u /world.py /w/world.pem /w/world.key >/dev/null \
  && $D network connect $WORLD_ALIASES e2e-litellm e2e-world || { echo "the world stand-in did not start"; exit 1; }
ok "stand-ins up"

echo "== the host agents' CA, made as the operator makes it (flotilla ca, offline)"
mkdir -p "$WORK/ca"
caf() { $D run --rm --user "$(id -u):$(id -g)" --network none -v "$WORK:/w" --entrypoint /usr/local/bin/flotilla "local/flotilla-flotillad:$TAG" ca "$@"; }
if caf root /w/ca/root >"$WORK/ca.log" 2>&1 && caf intermediate /w/ca/root /w/config/host-ca >>"$WORK/ca.log" 2>&1; then
  ok "root and intermediate made; the root's key only in the root's folder ($(ls "$WORK/config/host-ca" | tr '\n' ' '))"
else
  cat "$WORK/ca.log"; bad "flotilla ca"
fi
[ ! -e "$WORK/config/host-ca/root.key" ] || bad "root.key beside the intermediate"
SK="$(caf signing-key /w/config/host-ca e2e1 2>&1)"
echo "$SK" | grep >/dev/null '^  "e2e1": "[A-Za-z0-9+/]*=*"$' && [ -s "$WORK/config/host-ca/signing.key" ] \
  && ok "the execution-signing key made; its public line printed for the hosts' configs" || bad "signing-key: $SK"
SIGNPUB="$(echo "$SK" | sed -n 's/^  "e2e1": "\(.*\)"$/\1/p')"

echo "== secrets and seeded state (deploy/server/secrets.sh, as a deploy runs it)"
mkdir -p "$WORK/config/hooks" && (umask 077; tok > "$WORK/config/hooks/meeting.secret"; tok > "$WORK/config/hooks/smoke.secret"; tok > "$WORK/config/hooks/ask.secret")   # RFC-0014: the hooks' signing secrets, shared with the stub plumbing
printf 'SEED_SECRET=must-not-be-copied\nE2E_LLM_KEY=%s\nOPENROUTER_API_KEY=%s\nGLITCHTIP_TOKEN=%s\nGRAFANA_TOKEN=%s\nGITHUB_TOKEN=%s\n' \
  "$(tok)" "$(tok)" "$(tok)" "$(tok)" "$(tok)" > "$WORK/config/fleet.env"   # random stand-in keys, made per run
mkdir -p "$WORK/seed/results"; printf 'ts\tfingerprint\n' > "$WORK/seed/results/dispositions.tsv"
cp "$WORK/config/fleet.env" "$WORK/seed/fleet-gateway.env"; touch "$WORK/seed/STOP"
# the host's folder also holds backups of the secrets file: they must stay out too
cp "$WORK/config/fleet.env" "$WORK/seed/fleet-gateway.env.bak-20261027"
cp "$WORK/config/fleet.env" "$WORK/seed/results/fleet.env.bak-presync"
if FLOTILLA_HOME="$WORK" bash "$ROOT/deploy/server/secrets.sh" $TAG >"$WORK/secrets.log" 2>&1 \
   && FLOTILLA_HOME="$WORK" bash "$ROOT/deploy/server/secrets.sh" $TAG --seed-state flotilla-e2e-triage-state "$WORK/seed" >>"$WORK/secrets.log" 2>&1 \
   && FLOTILLA_HOME="$WORK" bash "$ROOT/deploy/server/secrets.sh" $TAG --seed-state flotilla-e2e-triage-candidate-state "$WORK/seed" >>"$WORK/secrets.log" 2>&1; then
  ok "secrets and state seeded ($(grep -c ' <- ' "$WORK/secrets.log") secret files)"
else
  cat "$WORK/secrets.log"; bad "secrets.sh"
fi
FLOTILLA_HOME="$WORK" bash "$ROOT/deploy/server/secrets.sh" $TAG --seed-state flotilla-e2e-triage-state "$WORK/seed" 2>&1 | grep >/dev/null "SEED SKIP" \
  && ok "a seeded state volume is never overwritten" || bad "seeding twice did not skip"

if [ "${DEMO:-}" != 1 ]; then   # a migration fixture, not something to show
echo "== an old, real-sized store for flotillad (schema step 1, 5000 runs)"
# flotillad migrates it at start, in its read-only container (deploy/e2e/old_store.py says why)
python3 "$ROOT/deploy/e2e/old_store.py" make "$WORK/oldstore/flotilla.db" 5000
$D run --rm --user 0 -v flotilla-e2e-state:/v -v "$WORK/oldstore:/in:ro" --entrypoint sh "local/flotilla-worker-triage:$TAG" \
  -c 'cp /in/flotilla.db /v/flotilla.db && chown -R 65532:65532 /v' && ok "an old store seeded" || bad "seeding the old store"
fi

if [ "${DEMO:-}" != 1 ]; then
echo "== a migrated schema-25 store, started on by the real flotillad (every routine's v1 live, the history version 1)"
# The Go test migrates a store like this (internal/store); here the shipped image does it, in its read-only container, over
# the repo's own fleets and the e2e's: steps 26 to 29, then the start records each routine's first version live.
# old_store.py check28 reads every post-condition off the copy (the migrated rows are all version 1, each routine has one
# live version, integrity_check is ok); the stack's own flotillad comes up after, so nothing else touches it.
python3 "$ROOT/deploy/e2e/old_store.py" make25 "$WORK/store25/flotilla.db"
$D volume create --label $LABEL flotilla-e2e-store25 >/dev/null
$D run --rm --user 0 -v flotilla-e2e-store25:/v -v "$WORK/store25:/in:ro" --entrypoint sh "local/flotilla-worker-triage:$TAG" \
  -c 'cp /in/flotilla.db /v/flotilla.db && chown -R 65532:65532 /v' >/dev/null && ok "a schema-25 store seeded" || bad "seeding the schema-25 store"
$D run -d --label $LABEL --name e2e-store25 --read-only --cap-drop ALL --security-opt no-new-privileges:true --user 65532:65532 --memory 256m \
  -v flotilla-e2e-store25:/var/lib/flotilla -v "$WORK/config/flotillad.yaml:/etc/flotilla/flotillad.yaml:ro" -v "$WORK/config/callers.yaml:/etc/flotilla/callers.yaml:ro" \
  -v flotilla-e2e-flotillad-secrets:/run/flotilla:ro -v "$WORK/config/specs:/etc/flotilla/specs:ro" -v "$WORK/config/registry-e2e.json:/etc/flotilla/registry-e2e.json:ro" \
  "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml >/dev/null || bad "starting flotillad on the schema-25 store"
for _ in $(seq 1 60); do $D logs e2e-store25 2>&1 | grep >/dev/null '"msg":"flotillad listening"' && break; sleep 1; done
$D logs e2e-store25 2>&1 | grep >/dev/null '"msg":"flotillad listening"' && ! $D logs e2e-store25 2>&1 | grep >/dev/null '"msg":"versions"' \
  && ok "flotillad migrated the schema-25 store and started: the versions reconciled with no refusal" || bad "flotillad on the schema-25 store: $($D logs e2e-store25 2>&1 | tail -4)"
$D stop -t 30 e2e-store25 >/dev/null 2>&1
mkdir -p "$WORK/store25out" && $D cp e2e-store25:/var/lib/flotilla/. "$WORK/store25out/" >/dev/null 2>&1 && cp "$WORK/store25/flotilla.db.counts.json" "$WORK/store25out/flotilla.db.counts.json"
S25="$(python3 "$ROOT/deploy/e2e/old_store.py" check28 "$WORK/store25out/flotilla.db" started 2>&1)"
echo "$S25" | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.exit(0 if d["problems"] == [] and d["live_routines"] and d["live_routines"] > 20 else 1)' \
  && ok "the migrated store, after flotillad started on it: $(echo "$S25" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["live_routines"], "routines, each with one live version; the old runs read version 1; integrity ok")')" \
  || bad "the schema-25 store after the start: $S25"
$D rm -f e2e-store25 >/dev/null 2>&1; $D volume rm flotilla-e2e-store25 >/dev/null 2>&1
fi

if [[ ",${E2E_MUTANT:-}," == *,matrix-live-write,* ]]; then
  $D volume create --label $LABEL flotilla-e2e-mut-live >/dev/null && $D run --rm --user 0 -v flotilla-e2e-mut-live:/v --entrypoint sh "local/flotilla-worker-triage:$TAG" -c 'chown -R 10001:10001 /v'
fi
echo "== up"
if ! compose up -d >"$WORK/up.log" 2>&1; then
  cat "$WORK/up.log"; bad "compose up"; [ "${KEEP:-}" = 1 ] || down; echo "E2E FAIL ($fails)"; exit 1
fi
cli() { $D exec flotillad /usr/local/bin/flotilla "$@"; }
for i in $(seq 1 60); do
  cli runs platform/heartbeat 2>/dev/null | grep >/dev/null -E " (ok|error|timeout) " && break; sleep 2
done
if [ "${DEMO:-}" != 1 ]; then   # the demo seeds no old store (above)
mkdir -p "$WORK/storecopy" && $D cp flotillad:/var/lib/flotilla "$WORK/storecopy/" 2>/dev/null
STORE="$(python3 "$ROOT/deploy/e2e/old_store.py" check "$WORK/storecopy/flotilla/flotilla.db")"
echo "$STORE" | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.exit(0 if "error" not in d and d["schema"] == d["latest"] and d["old_runs"] == 5000 and d["old_auto"] == 2500 and d["old_record"] == 2500 and d["shadow_left"] == 0 and d["live_left"] == 0 and d["stage_controls"] == 0 and d["heartbeat_autonomy"] == "auto" else 1)' \
  && ok "flotillad migrated the old store in its read-only container ($STORE)" || bad "the old store's migration: $STORE"
fi
OUT="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"
echo "$OUT" | grep >/dev/null "^action backup.rerun$" && echo "$OUT" | grep >/dev/null "^action host.true$" && echo "$OUT" | grep >/dev/null "^action host.absent$" \
  && echo "$OUT" | grep >/dev/null "^action repo.process_change$" && echo "$OUT" | grep >/dev/null "^action github.open_pr$" && echo "$OUT" | grep >/dev/null "^CHECK PASS.*, 18 actions$" \
  && echo "$OUT" | grep >/dev/null "^action crm.merge$" && echo "$OUT" | grep >/dev/null "^action email.send$" \
  && ok "-check reads the registry the image carries and the e2e's extension (the jobs, review, bugfix and business examples' actions, host.true, host.absent, host.watch)" || bad "-check: $OUT"
RUNS="$(cli runs platform/heartbeat 2>&1)"; echo "$RUNS"
echo "$RUNS" | grep >/dev/null " ok " && ok "heartbeat ran through the worker and passed" || bad "heartbeat did not pass"
# Docker's log driver hands lines to `docker logs` asynchronously, so the line can lag the run's result.
# grep reads all of `docker logs`, never -q: under pipefail an early exit can SIGPIPE docker logs and fail
# the pipe although the line is there (and turn a `!` check into a false pass)
for i in $(seq 1 15); do $D logs flotilla-worker-triage 2>&1 | grep '"line": "HEARTBEAT PASS"' >/dev/null && break; sleep 1; done
$D logs flotilla-worker-triage 2>&1 | grep '"line": "HEARTBEAT PASS"' >/dev/null && ok "the heartbeat saw allowed-in and denied-out" \
  || bad "no HEARTBEAT PASS in the worker's log (waited 15s)"
# the proxy writes its log line just after it refuses: wait for it rather than read once (a race, 2026-11-11)
seen() { for _ in $(seq 1 30); do $D logs "$1" 2>&1 | grep "$2" >/dev/null && return 0; sleep 1; done; return 1; }
# helpers the sections share (so E2E_SECTIONS can leave any section out)
# Each monitor runs at start on the ops-read worker; its detections become cases, the acting routine plans
# each, and policy records the plan in shadow and closes the case. The gates' refusals escalate instead.
want_closed() {  # kind target: a case of that kind on that target closed as a recorded shadow plan
  for _ in $(seq 1 45); do
    cli case list --all 2>&1 | grep >/dev/null "^[^ ]* *$1 .*$2 .*closed .*[^_[:alnum:]]plan_recorded" && return 0; sleep 2
  done
  return 1
}
want_case() {  # kind target status: a case of that kind on that target in that status
  for _ in $(seq 1 45); do
    cli case list --all 2>&1 | grep >/dev/null "^[^ ]* *$1 .*$2 .*$3" && return 0; sleep 2
  done
  return 1
}
# The freeze (ADR-0014 decision 8), through the real scheduler and worker: the heartbeat's own autonomy is auto.
agent() { $D exec -e FLOTILLA_TOKEN="$AGENT" flotillad /usr/local/bin/flotilla "$@"; }
# e2e/versioned is a routine folder with v1 to v6; v1 went live at the first start and the rest arrived as shadows.
vq() {  # vq ROUTINE 'python expression over v, the routine's versions answer': print it
  cli versions "$1" -json | python3 -c "import json,sys; v=json.load(sys.stdin); print($2)"
}
vstates() { vq "$1" "' '.join('%d:%s' % (x['version'], x['state']) for x in v['versions'])"; }
vrow() { cli versions "$1" -json | python3 -c "import json,sys; v=json.load(sys.stdin); x=next(y for y in v['versions'] if y['version'] == $2); print(x$3)"; }
runs_json() { cli runs "$1" -json -n 100; }
started() { $D inspect "$1" --format '{{.State.StartedAt}}' 2>&1; }
hx() { $D exec "$@"; }
hostlog() { $D exec flotilla-e2e-host cat /var/log/flotilla-host/agent.log 2>/dev/null; }
hseen() { for _ in $(seq 1 30); do hostlog | grep >/dev/null "$1" && return 0; sleep 1; done; return 1; }
# H9c, real executions through the whole path: a detection, a case, the acting routine's plan, policy
# (propose: the action promoted, the routine capped at propose), an operator's approval, the envelope
# signed and sent, the host's own checks, the result. The first runs /bin/true and is verified by the
# check routine, which closes it and releases the target; the second is refused by the host's rate
# limit and the third by its allowlist, each escalated and then closed by the operator. Then the
# host's audit log against flotillad's record.
proposed() {  # proposed KIND MONITOR: run the monitor, print the case it opened once it awaits approval
  local id=""
  cli run "e2e/$2" >/dev/null
  for _ in $(seq 1 45); do
    id="$(cli case list --all 2>&1 | awk -v k="$1" '$2 == k && /host:e2e-host/ && /awaiting_approval/ {print $1; exit}')"
    [ -n "$id" ] && break; sleep 2
  done
  echo "$id"
}
status_is() {  # status_is CASE STATUS: wait for it
  for _ in $(seq 1 45); do cli case list --all 2>&1 | grep >/dev/null "^$1 .* $2 " && return 0; sleep 2; done
  return 1
}
# A second, idle flotillad on the stack's network (its one routine runs `true` once a year, and flotillad ends when no timed routine is
# enabled, so it has to be), in the run_tokens mode asked for: e2e-<mode>, its workers' listener on e2e-<mode>:8485.
idle_flotillad() {
  local mode="$1" name="e2e-$1" dir="$WORK/config/specs-$1"
  mkdir -p "$dir/e2e"
  cat > "$dir/e2e/idle.yaml" <<EOF2
fleet: e2e
routine: idle
description: The e2e's second flotillad (run_tokens $mode) has one routine, which only keeps it running (flotillad ends with no timed routine).
enabled: true
worker: triage
trigger: {every: 8760h}
run: {command: "true", workdir: ., timeout: 1m}
autonomy: record
budget: {day_usd: 0}
freshness: 9000h
EOF2
  cat > "$WORK/config/flotillad-$name-live.yaml" <<EOF2
listen: 127.0.0.1:8484
worker_listen: 0.0.0.0:8485
specs_dir: /etc/flotilla/specs
state_dir: /var/lib/flotilla
environment: e2e
callers_file: /etc/flotilla/callers.yaml
run_tokens: $mode
registry_files: [/opt/flotilla/host/registry.json]
workers:
  triage: {url: "http://worker-triage:8081", token_file: /run/flotilla/worker-triage.token}
EOF2
  $D volume create --label $LABEL "flotilla-$name" >/dev/null
  $D run --rm --user 0 -v "flotilla-$name:/v" --entrypoint sh "local/flotilla-worker-triage:$TAG" -c 'chown -R 65532:65532 /v' >/dev/null
  $D run -d --label $LABEL --name "$name" --network "${P}_flotilla" --read-only --cap-drop ALL --security-opt no-new-privileges:true --user 65532:65532 --memory 256m \
    -v "flotilla-$name:/var/lib/flotilla" -v "$WORK/config/flotillad-$name-live.yaml:/etc/flotilla/flotillad.yaml:ro" -v "$WORK/config/callers.yaml:/etc/flotilla/callers.yaml:ro" \
    -v flotilla-e2e-flotillad-secrets:/run/flotilla:ro -v "$dir:/etc/flotilla/specs:ro" \
    "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml >/dev/null || bad "starting flotillad in run_tokens: $mode"
  for _ in $(seq 1 60); do $D logs "$name" 2>&1 | grep >/dev/null '"msg":"flotillad listening for workers' && break; sleep 1; done
}
stop_idle_flotillad() { $D rm -f "e2e-$1" >/dev/null 2>&1; $D volume rm "flotilla-e2e-$1" >/dev/null 2>&1; }
seen flotilla-egress "forbidden" && ok "the proxy logged the refused host" || bad "the proxy has no record of the refused host"
[ -z "$($D logs e2e-forbidden 2>&1)" ] && ok "the refused host never received a request" || bad "the refused host was reached: $($D logs e2e-forbidden 2>&1 | head -2)"
cli status | grep -E "^platform/heartbeat" | grep >/dev/null -E " false " && ok "status: heartbeat not stale" || bad "status: heartbeat stale"

free_routine() {  # take a routine off disabled_routines and restart flotillad: its first run is at start, and starts its shadows
  sed "s|, $1||" "$WORK/config/flotillad.yaml" > "$WORK/config/flotillad-free.yaml" && cat "$WORK/config/flotillad-free.yaml" > "$WORK/config/flotillad.yaml"
  $D restart flotillad >/dev/null
  for i in $(seq 1 30); do cli health >/dev/null 2>&1 && break; sleep 2; done
}
if want model; then
echo "== the model client (runtime/), from the heartbeat's model step"
L="$($D logs flotilla-worker-triage 2>&1)"
echo "$L" | grep >/dev/null 'ok   model   triage-flash: model=fleet-triage-flash deployment=e2e-deployment cost=2.1e-06 (reported:body) usage_reported=True traced=True' \
  && ok "a model call through the client: alias, deployment id, reported cost, traced" \
  || bad "no model step line: $(echo "$L" | grep -o 'model   triage-flash[^"]*' | head -2)"
$D logs e2e-litellm 2>&1 | grep "CHAT auth=bearer model=fleet-triage-flash" >/dev/null && ok "the endpoint got the key from the fleet's secrets file and the alias's model" \
  || bad "the LiteLLM stand-in saw no authorised chat call"
T="$($D exec flotilla-worker-triage python3 /opt/flotilla/runtime/test_telemetry.py 2>&1)"
# with the SDK, the no-SDK test is the one that skips; the SDK test must have run and passed
if echo "$T" | grep >/dev/null "test_model_call_span_under_the_run .* ok$" && echo "$T" | tail -1 | grep >/dev/null "^OK"; then
  ok "runtime/test_telemetry.py in the image: the model call's span under the run's trace"
else
  bad "telemetry test in the image: $(echo "$T" | tail -4 | tr '\n' ' ')"
fi
$D exec flotilla-worker-triage python3 -m pip check >/dev/null 2>&1 && ok "the image's Python packages are consistent (pip check)" || bad "pip check failed in the worker image"

fi
if want telemetry; then
echo "== telemetry"
# pushes and span exports are asynchronous: wait for each line up to 30 s (a fixed 8 s sleep lost the
# model.chat span once on 2026-11-11), the check itself unchanged
seen e2e-victoriametrics 'SAMPLE flotilla_run{autonomy="auto",environment="e2e",routine="platform/heartbeat",service="flotillad",status="ok"' \
  && ok "flotilla_run reached VictoriaMetrics through the proxy" || bad "no flotilla_run sample at the VictoriaMetrics stand-in"
seen e2e-victoriametrics 'SAMPLE flotilla_up' && ok "flotilla_up pushed" || bad "no flotilla_up"
seen e2e-victoriatraces "POST /insert/opentelemetry/v1/traces" && ok "traces reached VictoriaTraces through the proxy" \
  || bad "no traces at the VictoriaTraces stand-in"
seen e2e-victoriatraces "SPAN model.chat" && ok "the routine's model.chat span reached VictoriaTraces through the proxy" \
  || bad "no model.chat span at the VictoriaTraces stand-in"

fi
if want cases; then
echo "== cases: pipeline v1's chain (RFC-0004 §10), the ops workers, in shadow"
# The monitor runs at start; the stand-in says the backup is 25.5h old, above shadow's 25h. The dispatcher
# (every 15s) hands the case to jobs/rerun-backup, whose plan policy records in shadow, closing the case.
for i in $(seq 1 45); do
  CASES="$(cli case list --all 2>&1)"
  echo "$CASES" | grep >/dev/null "failed_backup .*host:backup-01 .*closed .*[^_[:alnum:]]plan_recorded" && break; sleep 2
done
echo "$CASES" | grep >/dev/null "failed_backup .*host:backup-01 .*closed .*[^_[:alnum:]]plan_recorded" \
  && ok "the backup monitor's detection became a case, planned and closed as a shadow plan" || bad "the chain: $CASES"
CID="$(echo "$CASES" | awk '/failed_backup/ {print $1; exit}')"
SHOW="$(cli case show "$CID" 2>&1)"
echo "$SHOW" | grep >/dev/null "case.handed_over .*jobs/backup-monitor .*by detector" && ok "opened by jobs/backup-monitor, handed over as a code detector" || bad "case show (detector): $SHOW"
echo "$SHOW" | grep >/dev/null "case.planned .*jobs/rerun-backup .*by act .*backup_age_above(25h): holds" \
  && ok "planned by jobs/rerun-backup, citing its re-check" || bad "case show (plan): $SHOW"
echo "$SHOW" | grep >/dev/null "case.closed .*by policy .*[^_[:alnum:]]plan_recorded.*would propose" \
  && ok "policy recorded it in shadow, and what it would have done (propose: a new host's ceiling)" || bad "case show (policy): $SHOW"
$D logs e2e-victoriametrics 2>&1 | grep 'QUERY .*backup_last_success_timestamp.*instance%3D%22backup-01%22' >/dev/null \
  && ok "the jobs workers queried the example metric for its instance through the proxy" || bad "no backup query at the VictoriaMetrics stand-in"

fi
if want watching; then
echo "== never silent (ADR-0023): a recorded plan under on_record: watch keeps its case open until the monitor reports it clear"
WFLAG=/tmp/flotilla-example/e2e-watch-clear
$D exec flotilla-worker-ops-read rm -f $WFLAG >/dev/null 2>&1
wmon() {  # run e2e/watch-monitor once and wait until that run has ended
  local n0 n
  n0="$(runs_json e2e/watch-monitor | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["runs"] or []))')"
  cli run e2e/watch-monitor >/dev/null
  for _ in $(seq 1 30); do
    n="$(runs_json e2e/watch-monitor | python3 -c 'import json,sys; r=json.load(sys.stdin)["runs"] or []; print(len(r) if r and r[0]["status"] != "running" else -1)')"
    [ "$n" -gt "$n0" ] && break; sleep 1
  done
  sleep 1
}
wmon
WC=""
for _ in $(seq 1 45); do
  WC="$(cli case list --all 2>&1 | awk '$2 == "e2e_watching" {print $1; exit}')"
  [ -n "$WC" ] && status_is "$WC" watching && break; sleep 2
done
[ -n "$WC" ] && status_is "$WC" watching \
  && ok "the recorded plan kept the case open: $WC is watching, not closed as plan_recorded" || bad "the watching case: $(cli case list --all 2>&1 | grep e2e_watching)"
for _ in 1 2 3; do wmon; done
WSHOW="$(cli case show "$WC" 2>&1)"
echo "$WSHOW" | grep >/dev/null "case.watching .*flotillad .*by policy" && ok "case.watching names its actor (flotillad, by policy)" || bad "case show (watching): $WSHOW"
echo "$WSHOW" | grep >/dev/null "^repeated 3 times unchanged" && [ "$(cli case list --all 2>&1 | grep -c e2e_watching)" = 1 ] \
  && ok "three unchanged detections attached to the open case as repeats: one case" || bad "repeats: $(echo "$WSHOW" | grep -E 'repeated|signal')"
for _ in $(seq 1 40); do cli case show "$WC" 2>&1 | grep >/dev/null "notice.due" && break; sleep 3; done
WSHOW="$(cli case show "$WC" 2>&1)"
[ "$(echo "$WSHOW" | grep -c "notice.due")" = 1 ] && echo "$WSHOW" | grep >/dev/null "notice.due .*flotillad" \
  && ok "the case timer recorded one notice.due (the notice at once), not one per detection" || bad "notice.due: $(echo "$WSHOW" | grep -E 'notice|watching')"
status_is "$WC" watching && ok "the notice moved nothing: the case is still watching" || bad "after the notice: $(cli case list --all 2>&1 | grep e2e_watching)"
$D exec flotilla-worker-ops-read touch $WFLAG
wmon
status_is "$WC" closed && cli case list --all 2>&1 | grep "^$WC " | grep >/dev/null "resolved_itself" \
  && ok "the monitor's clear closed it as resolved_itself" || bad "after the clear: $(cli case list --all 2>&1 | grep e2e_watching)"
cli case show "$WC" 2>&1 | grep >/dev/null "case.closed .*e2e/watch-monitor .*by detector .*resolved_itself" \
  && ok "the close names the monitor and its role (detector)" || bad "case show (clear): $(cli case show "$WC" 2>&1 | tail -3)"
$D exec flotilla-worker-ops-read rm -f $WFLAG >/dev/null 2>&1

fi
if want business; then
echo "== the business examples (crm, campaigns, outbound, renewals), on their stand-in data, in shadow"
want_closed crm_fix "crm:contact/c-101/email" && ok "crm: the email to normalize became a case, planned and recorded in shadow" \
  || bad "crm: $(cli case list --all 2>&1 | grep crm_fix)"
want_closed crm_fix "crm:contact/c-103/merge" && ok "crm: the duplicate became a merge plan, recorded in shadow" \
  || bad "crm merge: $(cli case list --all 2>&1 | grep crm_fix)"
cli case list --all 2>&1 | grep >/dev/null "crm:contact/c-104/country" && bad "crm: a field a person edited became a case" \
  || ok "crm: a field a person edited is never proposed"
want_closed campaign_ready "campaign:k-201" && want_closed campaign_ready "campaign:k-202" \
  && ok "campaigns: both ready campaigns decided (launch and hold), recorded in shadow" || bad "campaigns: $(cli case list --all 2>&1 | grep campaign_ready)"
want_closed prospect_new "prospect:p-301" && ok "outbound: a first touch drafted from cited facts, recorded in shadow" \
  || bad "outbound: $(cli case list --all 2>&1 | grep prospect_new)"
for _ in $(seq 1 45); do cli case list --all 2>&1 | grep >/dev/null "prospect:p-303 .*escalated" && break; sleep 2; done
cli case list --all 2>&1 | grep >/dev/null "prospect:p-303 .*escalated" && ok "outbound: the do-not-contact prospect is refused by the gate, escalated" \
  || bad "outbound gate: $(cli case list --all 2>&1 | grep prospect_new)"
want_closed renewal_risk "account:a-401" && ok "renewals: the account at risk flagged for its owner, recorded in shadow" \
  || bad "renewals: $(cli case list --all 2>&1 | grep renewal_risk)"

fi
if want world; then
echo "== the examples that reach the outside world (sites, deps), on deploy/e2e/world.py, in shadow"
want_case seo_live_regression "site:description/www.example.com/about" "" && ok "sites: the page without a description is a case" \
  || bad "sites description: $(cli case list --all 2>&1 | grep seo_live)"
want_case seo_live_regression "site:link/www.example.com/old-offers" "" && ok "sites: the link that answers 404 is a case" \
  || bad "sites link: $(cli case list --all 2>&1 | grep seo_live)"
want_case seo_live_regression "site:certificate/www.example.com/" "" && ok "sites: the certificate that expires in 9 days (under 21) is a case" \
  || bad "sites certificate: $(cli case list --all 2>&1 | grep seo_live)"
cli case list --all 2>&1 | grep >/dev/null "site:retired" && bad "sites: the retired host answering 404 became a case" \
  || ok "sites: the retired host answers 404, as it should: no case"
want_case dependency_update "repo:example/shop/deps/requests" "closed .*[^_[:alnum:]]plan_recorded" \
  && ok "deps: the vulnerable pin became a case, its upgrade PR planned and recorded in shadow" \
  || bad "deps vulnerable: $(cli case list --all 2>&1 | grep dependency_update)"
want_case dependency_update "repo:example/api/deps/fastapi" "" && ok "deps: the outdated pin of a repo that asks for them is a case" \
  || bad "deps outdated: $(cli case list --all 2>&1 | grep dependency_update)"
cli case list --all 2>&1 | grep >/dev/null "repo:example/shop/deps/flask" && bad "deps: an outdated pin of a repo that does not ask became a case" \
  || ok "deps: an outdated pin of a repo that does not ask for them is no case"
$D logs e2e-world 2>&1 | grep >/dev/null "world GET api.github.com/repos/example/shop/commits/HEAD" \
  && ok "deps reached the GitHub stand-in through the bugfix proxy, over TLS from the stack's CA" || bad "no GitHub request at the world stand-in"

fi
if want triage; then
echo "== the triage example, on the world's GlitchTip and Grafana and the scripted model, in shadow"
TRI=""
for _ in $(seq 1 60); do
  TRI="$(cli runs triage/cycle 2>&1)"; echo "$TRI" | grep >/dev/null -E " (ok|error|timeout) " && break; sleep 3
done
echo "$TRI" | grep >/dev/null " ok " && ok "triage/cycle ran to the end in shadow" || bad "triage/cycle: $TRI"
# the routine's own lines, as the worker logs them (one JSON line each, with its routine and run id)
TRO="$($D logs flotilla-worker-triage 2>&1 | grep -F "\"routine\": \"triage/cycle\"" | sed -n 's/.*"line": "\(.*\)"}$/\1/p')"
echo "$TRO" | grep >/dev/null "cycle done: 2 triaged" && ok "triage: the cycle triaged both signals, the shop's error and the api's alert" \
  || bad "triage cycle: $(echo "$TRO" | tail -5 | tr '\n' ' ')"
echo "$TRO" | grep >/dev/null "=> file" && ok "triage: the shop's TypeError is decided file (repo example/shop); in shadow nothing is filed" \
  || bad "triage file decision: $(echo "$TRO" | grep -E '=>|ESCALATE' | tr '\n' ' ')"
echo "$TRO" | grep >/dev/null "=> escalate" && ok "triage: the api's alert is escalated with a question" \
  || bad "triage escalate decision: $(echo "$TRO" | grep -E '=>|ESCALATE' | tr '\n' ' ')"
$D logs e2e-world 2>&1 | grep >/dev/null "world GET errors.example.com/api/0/organizations/example/issues/" \
  && ok "triage read GlitchTip's issues for the installation's organization (glitchtip_org)" || bad "no GlitchTip issues request at the world stand-in"
$D logs e2e-litellm 2>&1 | grep >/dev/null "CHAT auth=bearer model=fleet-triage-flash" && ok "triage asked the model through the gateway, with its key" \
  || bad "no triage model call at the gateway stand-in"

fi
if want bugfix; then
echo "== the bugfix example's intake, on the world's GitHub"
want_case code_bug "repo:example/shop/issues/7" "handed_over" \
  && ok "bugfix: the open bug issue became a code_bug case (its fix needs a real coding model: off here)" \
  || bad "bugfix intake: $(cli case list --all 2>&1 | grep code_bug)"

fi
if [ "${DEMO:-}" = 1 ]; then
  echo "== the demo"
  echo "cases the example fleets opened (they keep running on their schedules):"
  cli case list --all 2>&1 | awk 'NR == 1 || /crm_fix|campaign_ready|prospect_new|renewal_risk|failed_backup|dependency_update|seo_live_regression|signal/' | sed 's/^/   /'
  UA="$(cli user add demo --role admin 2>&1)"
  DEMOTOK="$(echo "$UA" | sed -n 's/.*"token":"\(flt_[0-9a-f]*\)".*/\1/p')"
  [ ${#DEMOTOK} = 68 ] || { echo "DEMO FAIL: the demo admin: $UA"; exit 1; }
  # one plan waits for a person: the CRM field fix promoted to propose, and a new contact to fix
  dcli() { $D exec -e FLOTILLA_TOKEN="$DEMOTOK" flotillad /usr/local/bin/flotilla "$@"; }
  dcli autonomy crm/fix propose --reason "demo: a plan that waits for you" >/dev/null 2>&1
  dcli action autonomy crm.update_field propose --reason "demo: a plan that waits for you" >/dev/null 2>&1
  $D exec flotilla-worker-ops-read python3 -c 'import json; p="/tmp/flotilla-example/crm/contacts.json"; c=json.load(open(p)); c.append({"id": "c-110", "name": "Eva Novak", "email": "eva.novak@example.org", "country": "france", "created": "2025-09-02"}); json.dump(c, open(p, "w"), indent=1)' \
    && dcli run crm/scan >/dev/null 2>&1
  for _ in $(seq 1 45); do dcli case list --all 2>&1 | grep >/dev/null "crm:contact/c-110/country .*awaiting_approval" && break; sleep 2; done
  dcli case list --all 2>&1 | grep >/dev/null "crm:contact/c-110/country .*awaiting_approval" \
    && ok "demo: a CRM field fix waits for approval" || bad "demo: no plan awaits approval: $(dcli case list --all 2>&1 | grep c-110)"
  printf 'localhost {\n\ttls internal\n\treverse_proxy flotillad:8486\n}\n' > "$WORK/Caddyfile.demo"
  $D run -d --name flotilla-e2e-caddy --label $LABEL -p "127.0.0.1:$DEMO_PORT:443" --memory 256m \
    -v "$WORK/Caddyfile.demo:/etc/caddy/Caddyfile:ro" "$CADDY_IMAGE" >/dev/null && $D network connect e2e-ui flotilla-e2e-caddy \
    || { echo "DEMO FAIL: the TLS front did not start"; exit 1; }
  printf '%s\n' "$DEMOTOK" > "$WORK/demo-admin.token"; chmod 600 "$WORK/demo-admin.token"
  echo
  echo "DEMO UP: https://localhost:$DEMO_PORT  (Caddy's own certificate: accept it once)"
  echo "   sign in with the token in $WORK/demo-admin.token"
  echo "   the fleets: /fleets; their cases: /cases?status=closed; tear down: make demo-down"
  [ "$fails" -eq 0 ] || { echo "DEMO: $fails check(s) failed above"; exit 1; }
  exit 0
fi

if want controls; then
echo "== controls and auth"
OUT="$($D exec -e FLOTILLA_TOKEN="$AGENT" flotillad /usr/local/bin/flotilla autonomy platform/heartbeat record 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 403" && ok "an agent cannot change a routine's autonomy ($OUT)" || bad "agent autonomy change: $OUT"
cli stop platform >/dev/null && cli run platform/heartbeat >/dev/null
sleep 3
cli runs platform/heartbeat -n 1 | grep >/dev/null " skipped " && ok "a STOPped fleet's run is recorded as skipped" || bad "STOP did not skip"
cli start platform --reason "e2e: start after the STOP check" >/dev/null
# STOP also ends a run in flight: e2e/hang blocks for 5 minutes on the triage worker; the STOP must cancel it
cli run e2e/hang >/dev/null
for i in $(seq 1 20); do cli status | grep -E "^e2e/hang " | grep >/dev/null "running " && break; sleep 1; done
cli status | grep -E "^e2e/hang " | grep >/dev/null "running " && ok "e2e/hang is in flight on the worker" || bad "e2e/hang did not start: $(cli runs e2e/hang -n 1 2>&1)"
cli stop e2e >/dev/null
for i in $(seq 1 30); do cli runs e2e/hang -n 1 | grep >/dev/null " running " || break; sleep 1; done
cli runs e2e/hang -n 1 | grep >/dev/null " cancelled .*cancelled: operator, fleet e2e STOPped" \
  && ok "a STOPped fleet's run in flight ends as cancelled, with who and why" || bad "STOP did not cancel the run in flight: $(cli runs e2e/hang -n 1 2>&1)"
cli start e2e --reason "e2e: start after the cancel check" >/dev/null
cli events -n 5 | grep >/dev/null "control.stop" && ok "the event log records the STOP" || bad "no control.stop event"
OUT="$(agent autonomy freeze 2>&1)"
echo "$OUT" | grep >/dev/null '"frozen":"1"' && ok "an agent pulls the freeze" || bad "agent freeze: $OUT"
cli status | grep >/dev/null "^FROZEN since .* by e2e-agent" && ok "status says frozen, and who froze it" || bad "status while frozen: $(cli status 2>&1 | head -3)"
N0=$(cli runs platform/heartbeat -n 500 | wc -l)
cli run platform/heartbeat >/dev/null
for i in $(seq 1 30); do
  L="$(cli runs platform/heartbeat -n 500)"
  [ "$(echo "$L" | wc -l)" -gt "$N0" ] && ! echo "$L" | sed -n 2p | grep >/dev/null " running " && break; sleep 1
done
echo "$L" | sed -n 2p | grep >/dev/null " record " && ok "frozen, a heartbeat run (set auto) ran at record" || bad "frozen run: $(echo "$L" | sed -n 2p)"
OUT="$(agent autonomy unfreeze --reason "e2e" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 403" && ok "an agent cannot lift the freeze" || bad "agent unfreeze: $OUT"
OUT="$(cli autonomy unfreeze 2>&1)"
echo "$OUT" | grep >/dev/null "needs a reason" && ok "lifting the freeze needs a reason" || bad "unfreeze without a reason: $OUT"
cli autonomy unfreeze --reason "e2e: lift the freeze" >/dev/null
OUT="$(cli status 2>&1)"
! echo "$OUT" | grep >/dev/null "^FROZEN" && echo "$OUT" | grep >/dev/null "^platform/heartbeat  *auto  *auto " \
  && ok "lifted, the heartbeat is back at auto (its own control untouched)" || bad "after unfreeze: $OUT"
# Holds (RFC-0004 §5.1 step 4) through the CLI: an agent holds a host, only an operator lifts it, with a reason.
OUT="$(agent hold host:backup-01 2h --reason "e2e: disk work" 2>&1)"
HID="$(echo "$OUT" | sed -n 's/.*"id":"\([0-9]*\)".*/\1/p')"
[ -n "$HID" ] && cli hold list | grep >/dev/null "host:backup-01 .*e2e-agent .*e2e: disk work" && ok "an agent holds a host; hold list shows it" || bad "hold: $OUT / $(cli hold list 2>&1)"
OUT="$(agent hold lift "$HID" --reason "e2e" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 403" && ok "an agent cannot lift a hold" || bad "agent lift: $OUT"
OUT="$(cli hold lift "$HID" 2>&1)"
echo "$OUT" | grep >/dev/null "needs a reason" && ok "lifting a hold needs a reason" || bad "lift without a reason: $OUT"
cli hold lift "$HID" --reason "e2e: disk done" >/dev/null
[ -z "$(cli hold list | sed 1d)" ] && ok "lifted, no hold is active" || bad "after lift: $(cli hold list 2>&1)"
# The deploy stand-down (deploy/server/standdown.sh, as deploy.sh and the installation's pre-pull hook run it):
# a hold by the deploy caller, its token read from flotillad's secrets volume; it never fails its caller.
OUT="$(DOCKER="$D" bash "$ROOT/deploy/server/standdown.sh" backup-01 5m "e2e deploy" 2>&1)"; RC=$?
[ $RC = 0 ] && echo "$OUT" | grep >/dev/null "^STANDDOWN host:backup-01 for 5m" && cli hold list | grep >/dev/null "host:backup-01 .*deploy .*deploy stand-down: e2e deploy" \
  && ok "a deploy stand-down is a hold on the host, taken by the deploy caller" || bad "stand-down: rc=$RC $OUT / $(cli hold list 2>&1)"
OUT="$(DOCKER=false bash "$ROOT/deploy/server/standdown.sh" backup-01 5m "e2e" 2>&1)"; RC=$?
[ $RC = 0 ] && echo "$OUT" | grep >/dev/null "^STANDDOWN SKIPPED" && ok "a stand-down that cannot be taken never fails the deploy" || bad "stand-down failure path: rc=$RC $OUT"
SID="$(cli hold list | awk '/deploy stand-down/ {print $1; exit}')"
cli hold lift "$SID" --reason "e2e: done" >/dev/null
W="$($D exec flotilla-worker-triage python3 -c 'import urllib.request,urllib.error
r=urllib.request.Request("http://127.0.0.1:8081/healthz",headers={"Authorization":"Bearer wrong"})
try: urllib.request.urlopen(r); print(200)
except urllib.error.HTTPError as e: print(e.code)' 2>&1)"
[ "$W" = 401 ] && ok "the worker refuses a wrong token" || bad "worker with a wrong token: $W"
N="$($D exec flotilla-worker-triage sh -c 'env | grep -c "^FLOTILLA_WORKER_TOKEN=" || true' 2>&1)"
[ "$N" = 0 ] && ok "no worker token in the worker's environment (it reads a file)" || bad "worker token in the environment: $N"
M="$($D exec flotilla-worker-triage sh -c 'stat -c "%a %u" /run/flotilla/fleet.env /run/flotilla/worker.token' 2>&1 | tr '\n' ' ')"
[ "$M" = "600 10001 600 10001 " ] && ok "the worker's secrets are mode 600, its own uid ($M)" || bad "worker secret modes: $M"
S="$($D exec flotilla-worker-triage sh -c 'find /home/worker/signal-fleet; cat /home/worker/signal-fleet/results/dispositions.tsv' 2>&1 | tr '\n' ' ')"
case "$S" in *.env*|*STOP*) bad "seeding copied an env file (secrets or a backup of them) or STOP: $S" ;;
  *fingerprint*) ok "the seeded state has the ledgers and no env file (secrets or their backups) or STOP" ;; *) bad "seeded state: $S" ;; esac
$D exec flotilla-worker-triage sh -c 'touch /home/worker/signal-fleet/w && rm /home/worker/signal-fleet/w' 2>/dev/null \
  && ok "the worker can write its state volume" || bad "the worker cannot write its state volume"
SK="$($D exec -w /opt/flotilla/signal-fleet/mvp -e OPENROUTER_API_KEY=x -e SF_OBSERV_DISABLED=1 flotilla-worker-triage \
  python3 -c 'import triage; print(triage.PROMPT_SHA, triage._signal_intro({"alertname": "x", "summary": "No space left on device"}).count("PRE-CLASSIFICATION HINT"))' 2>&1 | tail -1)"
[ "$SK" = "96a4218f 1" ] && ok "the worker image has triage's playbook and hint skills (PROMPT_SHA 96a4218f with the example facts, the hint loads)" \
  || bad "triage in the worker image: '$SK'"


fi
if want ledger; then
echo "== the ledger (RFC-0002 §9 step 5): a worker's routine on flotillad's workers' listener"
# The replay writes rows through the API as a routine's own worker would, which only run_tokens: accept lets a worker's own token do (a run's token
# is confined to the run's own namespace, the operator's is import and read only): it runs against a flotillad in the one-release way back.
if [ "${DEMO:-}" != 1 ]; then
idle_flotillad accept
LR="$($D exec -w /opt/flotilla/signal-fleet/mvp -e FLOTILLA_API_URL=http://e2e-accept:8485 \
  -e FLOTILLA_API_TOKEN_FILE=/run/flotilla/worker.token -e NO_PROXY=e2e-accept -e HTTP_PROXY= -e HTTPS_PROXY= -e SF_OBSERV_DISABLED=1 \
  flotilla-worker-triage python3 gate_ledger_backend_replay.py --synthetic 300 2>&1)"
echo "$LR" | grep >/dev/null "LEDGER BACKEND REPLAY: PASS" \
  && ok "dedup through the ledger equals filed.tsv (on a flotillad in run_tokens: accept): $(echo "$LR" | grep '^lookup:' | cut -c1-90)" \
  || bad "ledger replay: $(echo "$LR" | tail -4 | tr '\n' ' ')"
stop_idle_flotillad accept
fi
WL="$($D exec flotilla-worker-triage python3 -c 'import urllib.request,urllib.error
try:
    urllib.request.urlopen("http://flotillad:8485/v1/status", timeout=5); print("served")
except urllib.error.HTTPError as e: print(e.code)' 2>&1 | tail -1)"
[ "$WL" = 404 ] && ok "the workers' listener does not serve the full API (/v1/status: 404)" || bad "workers' listener /v1/status: $WL"

fi
if want runtokens; then
echo "== run tokens (v2 step 2): every run has its own token for the workers' listener"
# e2e/token-probe (fleets/e2e/token-probe.yaml) reads FLOTILLA_API_TOKEN_FILE, which must be its own file and not the worker's,
# and files a ledger row naming another routine's namespace and another run's id: flotillad files it under the probe's own.
if [ "${DEMO:-}" != 1 ]; then
cli run e2e/token-probe >/dev/null
for i in $(seq 1 30); do cli runs e2e/token-probe -n 1 | grep >/dev/null -E " (ok|error|timeout) " && break; sleep 1; done
[ "$(cli runs e2e/token-probe -json -n 1 | python3 -c 'import json,sys; print(json.load(sys.stdin)["runs"][0]["status"])')" = ok ] \
  && ok "a run was given its own token file (mode 600, not the worker's) and its ledger write named another namespace" \
  || bad "token-probe: $(cli runs e2e/token-probe -n 1) $($D logs flotilla-worker-triage 2>&1 | grep -E 'token_file|own |worker.s token file|namespace the run' | tail -3)"
printf %s "$OP" | $D exec -i flotilla-worker-triage sh -c 'umask 077 && cat > /tmp/op.token'
NSC="$($D exec flotilla-worker-triage python3 -c 'import json,urllib.request
def n(ns):
    r = urllib.request.Request("http://flotillad:8485/v1/ledger/filings?ns=" + ns, headers={"Authorization": "Bearer " + open("/tmp/op.token").read().strip()})
    return len(json.load(urllib.request.urlopen(r, timeout=10))["filings"])
print(n("e2e/token-probe"), n("bugfix/fix"))' 2>&1 | tail -1)"
[ "$NSC" = "1 0" ] && ok "the filing landed in the run's own namespace (e2e/token-probe: 1 row, the namespace it named: 0)" || bad "ledger rows by namespace: '$NSC'"
TL="$($D exec flotilla-worker-triage sh -c 'ls -A /tmp | grep -c "^.flotilla-token-" || true' 2>&1 | tail -1)"
[ "$TL" = 0 ] && ok "no run's token file is left on the worker after the run" || bad "token files left in the worker's /tmp: $TL"
# every worker class: the run's own token file, the worker's own token refused on the listener (401), the run's token accepted, the namespace forced.
# (triage above; the shadow worker by the hostile shadow in its section; the remote worker in its own.)
for W in triage-candidate ops-read ops-act-host bugfix; do
  cli run e2e/token-probe-$W >/dev/null 2>&1
  for i in $(seq 1 30); do cli runs e2e/token-probe-$W -n 1 | grep >/dev/null -E " (ok|error|timeout) " && break; sleep 1; done
  [ "$(cli runs e2e/token-probe-$W -json -n 1 | python3 -c 'import json,sys; print(json.load(sys.stdin)["runs"][0]["status"])')" = ok ] \
    && ok "run tokens on the $W worker: its run's own token file, its own token refused on the listener, the run's accepted, the namespace forced" \
    || bad "token-probe on $W: $(cli runs e2e/token-probe-$W -n 1) $($D logs flotilla-worker-$W 2>&1 | grep -o 'PROBE [^"]*' | tail -2)"
done
# The upgrade path (deploy.sh runs -check while the previous tag's workers still answer, then `up` recreates them, then flotillad starts): a worker
# whose /healthz lacks run_token is the expected state of that -check, so -check reports it and passes; the start refuses it under required (by name),
# and only says so under accept; after the worker is replaced by one that carries the capability, the start runs and required holds. One idle flotillad
# of its own (a spec that only keeps it running) against a worker that answers as e2e-upgrade, first the old stub and then the real worker image.
$D run -d --label $LABEL --name e2e-oldworker --network "${P}_flotilla" --network-alias e2e-upgrade --memory 64m -v "$ROOT/deploy/e2e/old_worker.py:/o.py:ro" \
  python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b python3 -u /o.py >/dev/null || bad "the old worker stand-in did not start"
tok > "$WORK/config/worker-old.token"; chmod 644 "$WORK/config/worker-old.token"
mkdir -p "$WORK/config/specs-upgrade/e2e"
cat > "$WORK/config/specs-upgrade/e2e/idle.yaml" <<EOF2
fleet: e2e
routine: idle
description: The e2e's upgrade flotillad has one routine, which only keeps it running.
enabled: true
worker: old
trigger: {every: 8760h}
run: {command: "true", workdir: ., timeout: 1m}
autonomy: record
budget: {day_usd: 0}
freshness: 9000h
EOF2
upgrade_cfg() {  # upgrade_cfg MODE-LINE
  cat > "$WORK/config/flotillad-upgrade.yaml" <<EOF2
listen: 127.0.0.1:8484
worker_listen: 0.0.0.0:8485
specs_dir: /etc/flotilla/specs
state_dir: /tmp/upgrade-state
environment: e2e
callers_file: /etc/flotilla/callers.yaml
$1
registry_files: [/opt/flotilla/host/registry.json]
workers:
  old: {url: "http://e2e-upgrade:8081", token_file: /run/flotilla-old/old.token}
EOF2
}
UPG=(--network "${P}_flotilla" --read-only --tmpfs /tmp --user 65532:65532 -v "$WORK/config/flotillad-upgrade.yaml:/etc/flotilla/flotillad.yaml:ro"
  -v "$WORK/config/callers.yaml:/etc/flotilla/callers.yaml:ro" -v "$WORK/config/worker-old.token:/run/flotilla-old/old.token:ro"
  -v "$WORK/config/specs-upgrade:/etc/flotilla/specs:ro")
upgrade_check() { $D run --rm "${UPG[@]}" "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml -check 2>&1; echo "rc=$?"; }
# timeout stops the docker client, not the container: a start that does not refuse kept running (a stray container after the run, 2026-11-22)
upgrade_start() {
  $D rm -f e2e-upgrade-start >/dev/null 2>&1
  timeout 25 $D run --rm --name e2e-upgrade-start "${UPG[@]}" "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml 2>&1; echo "rc=$?"
  $D rm -f e2e-upgrade-start >/dev/null 2>&1
}
upgrade_cfg ""
# the stand-in must answer before -check, or -check finds it unreachable (not judged) instead of old (seen on a slower CI runner)
for _ in $(seq 1 30); do $D exec e2e-oldworker python3 -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8081/livez", timeout=3)' >/dev/null 2>&1 && break; sleep 1; done
OLDCHK="$(upgrade_check)"; OLDSTART="$(upgrade_start)"
echo "$OLDCHK" | grep >/dev/null "worker old: NOT run-token capable" && echo "$OLDCHK" | grep >/dev/null "^rc=0" \
  && ok "-check against a worker still on the previous tag (no run_token in /healthz) reports it and passes: a deploy's -check runs before up recreates the workers" \
  || bad "-check against an old worker: $(echo "$OLDCHK" | tail -3)"
echo "$OLDSTART" | grep >/dev/null "worker old does not carry run tokens" && echo "$OLDSTART" | grep >/dev/null "^rc=2" \
  && ok "run_tokens: required: the start refuses a worker whose /healthz lacks run_token, by name (exit 2)" || bad "the start with an old worker: $(echo "$OLDSTART" | tail -3)"
upgrade_cfg "run_tokens: accept"
OLDACC="$(upgrade_start | head -c 600; echo)"; $D rm -f e2e-upgrade-start >/dev/null 2>&1; OLDACCCHK="$(upgrade_check)"
[ -z "$($D ps -aq --filter name='^e2e-upgrade-start$')" ] && ok "the start that ran on to its timeout left no container behind" \
  || bad "the timed-out start's container is still there: $($D ps -a --filter name='^e2e-upgrade-start$' --format '{{.Status}}')"
echo "$OLDACCCHK" | grep >/dev/null "worker old: NOT run-token capable" && echo "$OLDACCCHK" | grep >/dev/null "^rc=0" && ! echo "$OLDACC" | grep >/dev/null "does not carry run tokens" \
  && ok "under accept the same old worker is only reported, and the start does not refuse it" || bad "an old worker under accept: $(echo "$OLDACCCHK" | tail -2) / $(echo "$OLDACC" | tail -2)"
upgrade_cfg ""
# the upgrade: the old worker is replaced by the real worker image (which carries the capability), answering as the same name
$D rm -f e2e-oldworker >/dev/null 2>&1
$D run -d --label $LABEL --name e2e-newworker --network "${P}_flotilla" --network-alias e2e-upgrade --read-only --tmpfs /tmp --tmpfs /home/worker:uid=10001,gid=10001 \
  --user 10001:10001 --memory 128m -e FLOTILLA_WORKER=old -e FLOTILLA_WORKER_TOKEN_FILE=/t/old.token -e FLOTILLA_WORKER_LISTEN=0.0.0.0:8081 \
  -v "$WORK/config/worker-old.token:/t/old.token:ro" "local/flotilla-worker-triage:$TAG" >/dev/null || bad "the upgraded worker did not start"
for _ in $(seq 1 30); do $D exec e2e-newworker python3 -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8081/livez", timeout=3)' >/dev/null 2>&1 && break; sleep 1; done
NEWCHK="$(upgrade_check)"
echo "$NEWCHK" | grep >/dev/null "worker old: run-token capable" && echo "$NEWCHK" | grep >/dev/null "^rc=0" \
  && ok "after the upgrade the same -check says the worker is run-token capable" || bad "-check after the upgrade: $(echo "$NEWCHK" | tail -3)"
$D run -d --label $LABEL --name e2e-upgrade-flotillad "${UPG[@]}" "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml >/dev/null || bad "the upgraded stack's flotillad did not start"
for _ in $(seq 1 30); do $D logs e2e-upgrade-flotillad 2>&1 | grep >/dev/null '"msg":"flotillad listening for workers' && break; sleep 1; done
UPGOUT="$($D exec -e NO_PROXY=e2e-upgrade-flotillad -e HTTP_PROXY= -e HTTPS_PROXY= e2e-newworker python3 -c '
import urllib.error, urllib.request
tok = open("/t/old.token").read().strip()
r = urllib.request.Request("http://e2e-upgrade-flotillad:8485/v1/ledger/counts?ns=e2e/upgrade", headers={"Authorization": "Bearer " + tok})
try:
    urllib.request.urlopen(r, timeout=10); print(200)
except urllib.error.HTTPError as e: print(e.code)' 2>&1 | tail -1)"
$D logs e2e-upgrade-flotillad 2>&1 | grep >/dev/null '"msg":"flotillad listening for workers' && [ "$UPGOUT" = 401 ] \
  && ok "after the upgrade the start runs, and required holds: the worker's own token opens nothing on the listener (401)" \
  || bad "the upgraded stack: listener answered '$UPGOUT'; $($D logs e2e-upgrade-flotillad 2>&1 | tail -2 | cut -c1-200)"
$D rm -f e2e-upgrade-flotillad e2e-newworker >/dev/null 2>&1
# run_tokens: required is the default (the stack's own config says nothing), read by the real binary (-check, read-only): a worker's own
# token is not a caller then; accept is the one-release way back, which -check warns about
sed 's|^state_dir: .*|state_dir: /tmp/accept-check|; /^worker_listen:/a run_tokens: accept' "$WORK/config/flotillad.yaml" > "$WORK/config/flotillad-accept.yaml"
REQ="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"
ACC="$($D run --rm --read-only --tmpfs /tmp --user 65532:65532 -v "$WORK/config/flotillad-accept.yaml:/etc/flotilla/flotillad.yaml:ro" -v "$WORK/config/callers.yaml:/etc/flotilla/callers.yaml:ro" \
  -v flotilla-e2e-flotillad-secrets:/run/flotilla:ro -v "$WORK/config/specs:/etc/flotilla/specs:ro" -v "$WORK/config/registry-e2e.json:/etc/flotilla/registry-e2e.json:ro" \
  "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml -check 2>&1)"
NTOK="$(grep -v "shadow: true" "$WORK/config/flotillad.yaml" | grep -c "token_file:")"   # a shadow worker's token is no caller (S1), so it is not among those required drops
CA="$(echo "$ACC" | grep -o '[0-9]* callers' | cut -d' ' -f1)"; CR="$(echo "$REQ" | grep -o '[0-9]* callers' | cut -d' ' -f1)"
echo "$ACC" | grep >/dev/null "^run tokens: accept " && echo "$ACC" | grep >/dev/null "^WARNING run_tokens: accept" && echo "$REQ" | grep >/dev/null "^run tokens: required .*opens nothing" \
  && ! echo "$REQ" | grep >/dev/null "WARNING run_tokens" && [ -n "$CA" ] && [ "$((CA - CR))" = "$NTOK" ] \
  && ok "-check: run_tokens is required by default; accept warns and keeps the $NTOK workers' own tokens among the callers (required $CR, accept $CA)" || bad "-check run tokens: accept '$(echo "$ACC" | grep -E '^(WARNING )?run tokens|^WARNING run_tokens')' required '$(echo "$REQ" | grep -E '^run tokens|callers' | tr '\n' ' ')'"
# two workers that share a token are refused, by name, by -check (so a deploy's check stops it before anything restarts)
sed 's|worker-triage-candidate.token|worker-triage.token|' "$WORK/config/flotillad.yaml" > "$WORK/config/flotillad-shared.yaml"
SHR="$($D run --rm --read-only --tmpfs /tmp --user 65532:65532 -v "$WORK/config/flotillad-shared.yaml:/etc/flotilla/flotillad.yaml:ro" -v "$WORK/config/callers.yaml:/etc/flotilla/callers.yaml:ro" \
  -v flotilla-e2e-flotillad-secrets:/run/flotilla:ro -v "$WORK/config/specs:/etc/flotilla/specs:ro" -v "$WORK/config/registry-e2e.json:/etc/flotilla/registry-e2e.json:ro" \
  "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml -check 2>&1; echo "rc=$?")"
echo "$SHR" | grep >/dev/null "workers triage and triage-candidate share a token file" && echo "$SHR" | grep >/dev/null "^rc=2" \
  && ok "-check refuses workers that share a token file, naming them (exit 2, before anything restarts)" || bad "-check on shared worker tokens: $(echo "$SHR" | tail -3)"
echo "$REQ" | grep >/dev/null "share a token file" && bad "the stack's own workers share a token file" || ok "no two of the stack's workers share a token file"
# run_tokens: required on a running flotillad (a second one, idle): a worker's own token opens nothing, the operator's token loads and checks a
# ledger (what ledger_sync does)
idle_flotillad required
REQ_LOG="$($D logs e2e-required 2>&1 | tail -4 | cut -c1-300)"
REQ_OUT="$($D exec -e NO_PROXY=e2e-required -e HTTP_PROXY= -e HTTPS_PROXY= flotilla-worker-triage python3 -c '
import json, sys, urllib.error, urllib.request
base = "http://e2e-required:8485"
worker = open("/run/flotilla/worker.token").read().strip()
op = open("/tmp/op.token").read().strip()
def call(method, path, tok, body=None):
    r = urllib.request.Request(base + path, method=method, data=json.dumps(body).encode() if body is not None else None,
                               headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(r, timeout=10) as x:
            return x.status, json.load(x)
    except urllib.error.HTTPError as e:
        return e.code, {}
rows = {"filings": [{"fingerprint": "a", "repo": "o/r", "issue": "1"}], "entries": [{"kind": "decision", "row": {"fingerprint": "a", "disposition": "file"}}]}
print("worker-count", call("GET", "/v1/ledger/counts?ns=e2e/required", worker)[0])
print("worker-import", call("POST", "/v1/ledger/import?ns=e2e/required", worker, rows)[0])
st, c = call("POST", "/v1/ledger/import?ns=e2e/required", op, rows)
print("op-import", st, c.get("filings"), c.get("decisions"))
st, c = call("GET", "/v1/ledger/counts?ns=e2e/required", op)
print("op-count", st, c.get("filings"), c.get("decisions"))
print("op-filings", call("GET", "/v1/ledger/filings?ns=e2e/required", op)[0], "op-write", call("POST", "/v1/ledger/filings?ns=e2e/required", op, {"row": {"fingerprint": "z"}})[0])
' 2>&1)"
echo "$REQ_OUT" | grep >/dev/null "^worker-count 401" && echo "$REQ_OUT" | grep >/dev/null "^worker-import 401" \
  && ok "run_tokens: required on a running flotillad: the worker's own token opens nothing (401 on the ledger)" || bad "required mode, the worker's token: $(echo "$REQ_OUT" | tail -2) / flotillad: $REQ_LOG"
echo "$REQ_OUT" | grep >/dev/null "^op-import 200 1 1" && echo "$REQ_OUT" | grep >/dev/null "^op-count 200 1 1" && echo "$REQ_OUT" | grep >/dev/null "^op-filings 200 op-write 403" \
  && ok "the operator's token imports a ledger and reads it back in required mode; it writes no row" || bad "required mode, the operator's import: $(echo "$REQ_OUT" | tail -2) / flotillad: $REQ_LOG"
$D exec flotilla-worker-triage rm -f /tmp/op.token
stop_idle_flotillad required
fi

fi
if want inputs; then
echo "== frozen inputs (v2 step 2): the input a live run was handed, exported"
# every live run's input is recorded when it starts (what it was handed, the ledger's marks, the open case keys of what it detects);
# `flotilla input export` writes it to a folder outside any repo (here flotillad's state volume), which is checked by the repo's own
# stdlib schema checker against runtime/input.schema.json
if [ "${DEMO:-}" != 1 ]; then
IKEY="$(cli runs e2e/token-probe -json -n 1 | python3 -c 'import json,sys; print(json.load(sys.stdin)["runs"][0]["id"])')"
IVIEW="$(cli input show "$IKEY" 2>&1)"
echo "$IVIEW" | python3 -c '
import json, sys
v = json.load(sys.stdin); e = v["envelope"]
sys.exit(0 if e["routine"] == "e2e/token-probe" and e["kind"] == "tick" and e["trigger"] == "manual" and e["input_key"] == sys.argv[1]
         and e["ledger"]["mark_filings"] >= 0 and e["ledger"]["mark_entries"] >= 0 and e["live_case_keys"] == [] and v["pinned"] is False
         and v["snapshot_state"] == "none" and v["shadow_runs"] == [] else 1)' "$IKEY" \
  && ok "the run's input is recorded under its id: a manual tick of e2e/token-probe with the ledger's marks, nothing pinned" || bad "input show: $IVIEW"
rm -rf "$WORK/export"; mkdir -p "$WORK/export"
cli input export "$IKEY" --to "/var/lib/flotilla/exports/$IKEY" --with-ledger >/dev/null 2>&1 \
  && $D cp "flotillad:/var/lib/flotilla/exports/$IKEY" "$WORK/export/" >/dev/null 2>&1 \
  && [ "$(python3 "$ROOT/runtime/schemacheck.py" "$ROOT/runtime/input.schema.json" "$WORK/export/$IKEY/input.json" | head -1)" = "PASS $WORK/export/$IKEY/input.json" ] \
  && ok "an exported input validates against runtime/input.schema.json (the repo's own checker)" || bad "input export: $(python3 "$ROOT/runtime/schemacheck.py" "$ROOT/runtime/input.schema.json" "$WORK/export/$IKEY/input.json" 2>&1 | head -3)"
EM="$(stat -c '%a' "$WORK/export/$IKEY" "$WORK/export/$IKEY/input.json" "$WORK/export/$IKEY/README.txt" 2>&1 | tr '\n' ' ')"
[ "$EM" = "700 600 600 " ] && grep >/dev/null "PRODUCTION DATA" "$WORK/export/$IKEY/README.txt" \
  && ok "the export folder is 0700, its files 0600, and its README says it is production data" || bad "export modes: $EM"
cli input export "$IKEY" --to "/var/lib/flotilla/exports/$IKEY" >/dev/null 2>&1 \
  && bad "an export over a folder that already holds one was accepted" || ok "an export never writes into a folder that holds something"
cli input pin "$IKEY" >/dev/null 2>&1 && cli input unpin "$IKEY" >/dev/null 2>&1 \
  && cli events -n 20 | grep >/dev/null "input.pin " && cli events -n 20 | grep >/dev/null "input.unpin " && cli events -n 20 | grep >/dev/null "input.export .*$IKEY" \
  && ok "pin, unpin and the export are in the event log with who made them" || bad "input events: $(cli events -n 8)"
AG="$($D exec -e FLOTILLA_TOKEN="$(cat "$WORK/agent.token")" flotillad /usr/local/bin/flotilla input show "$IKEY" 2>&1)"
echo "$AG" | grep >/dev/null "HTTP 403" && ok "an agent's token cannot read an input (production data)" || bad "an agent reading an input: $AG"
fi

fi
if want candidate; then
echo "== the candidate worker (shadow comparison of a candidate image)"
for i in $(seq 1 30); do
  [ "$($D inspect flotilla-worker-triage-candidate --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ] && break; sleep 2
done
H="$($D inspect flotilla-worker-triage-candidate --format '{{.State.Health.Status}}' 2>&1)"
[ "$H" = healthy ] && ok "the candidate worker is healthy" || bad "candidate worker health: $H"
CS="$($D inspect flotilla-worker-triage-candidate --format '{{range .Mounts}}{{if eq .Destination "/home/worker/signal-fleet"}}{{.Name}}{{end}}{{end}}' 2>&1)"
[ "$CS" = flotilla-e2e-triage-candidate-state ] && ok "the candidate has its own state volume ($CS)" || bad "candidate state volume: '$CS'"
CE="$($D inspect flotilla-worker-triage-candidate --format '{{range .Config.Env}}{{println .}}{{end}}' 2>&1 | grep -E '^ROUTINE_SF_(SERVICE|GLITCHTIP_SYNC)=' | sort | tr '\n' ' ')"
[ "$CE" = "ROUTINE_SF_GLITCHTIP_SYNC=0 ROUTINE_SF_SERVICE=triage-fleet-candidate " ] \
  && ok "the candidate keeps its own name and no GlitchTip sync over the live fleet's env file" || bad "candidate env: '$CE'"
OUT="$(cli autonomy e2e/candidate propose 2>&1)"
echo "$OUT" | grep >/dev/null "capped at record" && ok "e2e/candidate (on the candidate worker) cannot leave record, even for the operator ($OUT)" \
  || bad "autonomy on the capped routine: $OUT"
OUT="$(cli autonomy e2e/candidate shadow 2>&1)"
echo "$OUT" | grep >/dev/null '"autonomy":"record"' && echo "$OUT" | grep >/dev/null "shadow is now called record" \
  && ok "the old level word shadow is stored as record ($OUT)" || bad "autonomy shadow: $OUT"
OUT="$(cli status 2>&1)"
echo "$OUT" | grep >/dev/null 'worker triage-candidate: .*"ok":true' && ok "flotillad reaches the candidate worker" || bad "status: $OUT"
NW="$($D inspect flotilla-worker-triage-candidate --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
[ "$NW" = "${P}_flotilla " ] && ok "the candidate is on the internal network only" || bad "candidate networks: '$NW'"

fi
if want versions; then
echo "== routine versions (docs/wip/v2-design.md section 1): the folder layout, the states, the operator's controls, the freeze, the rename"
[ "$(vstates e2e/versioned)" = "1:live 2:shadow 3:shadow 4:shadow 5:shadow 6:shadow" ] \
  && ok "e2e/versioned (a routine folder): v1 live from the first start, v2 to v6 arrived as shadows" || bad "versions at start: $(vstates e2e/versioned)"
[ "$(runs_json e2e/versioned | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["runs"] or []))')" = 0 ] \
  && ok "no shadow ran: e2e/versioned has no run at all" || bad "e2e/versioned has runs before any case: $(runs_json e2e/versioned | head -c 300)"
cli status -json | python3 -c '
import json, sys
r = {x["id"]: x for x in json.load(sys.stdin)["routines"]}
h, v = r["platform/heartbeat"], r["e2e/versioned"]
sys.exit(0 if h["version"] == 1 and len(h["version_hash"]) == 64 and v["version"] == 1 and v["version_hash"] != h["version_hash"] else 1)' \
  && ok "status carries each routine's bound version and its frozen hash (the heartbeat is a folder routine at v1)" || bad "status version fields"
# every flat spec keeps the hash the unit test pins (internal/spec/testdata/golden_hashes.txt): runs.spec_hash and a deploy's before/after proof are unchanged
if cli status -json | python3 -c '
import json, sys
want = dict(l.split() for l in open(sys.argv[1]).read().split("\n") if l.strip())
got = {x["id"]: x["spec_hash"] for x in json.load(sys.stdin)["routines"]}
miss = sorted(i for i in want if got.get(i) != want[i])
print(len(want) - len(miss), "of", len(want), "flat routines have the pinned hash", miss[:5])
sys.exit(1 if miss else 0)' "$ROOT/internal/spec/testdata/golden_hashes.txt" > "$WORK/golden.out" 2>&1; then
  ok "flat routines load with unchanged hashes in the real image ($(cat "$WORK/golden.out"))"
else
  bad "flat spec hashes moved: $(cat "$WORK/golden.out")"
fi
OUT="$(agent promote e2e/versioned v2 --autonomy propose --reason "e2e: an agent tries" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 403" && ok "an agent cannot promote a version ($OUT)" || bad "agent promote: $OUT"
OUT="$(agent rollback e2e/versioned --autonomy propose --reason "e2e: an agent tries" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 403" && ok "an agent cannot roll back" || bad "agent rollback: $OUT"
OUT="$(agent retire e2e/versioned v4 --reason "e2e: an agent tries" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 403" && ok "an agent cannot retire" || bad "agent retire: $OUT"
OUT="$(cli promote e2e/versioned v2 --reason "e2e: no autonomy chosen" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 400.*needs the routine's autonomy" && ok "a promote without the routine's autonomy is refused (never inherited in silence)" || bad "promote without autonomy: $OUT"
OUT="$(cli promote e2e/versioned v2 --autonomy propose 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 400.*lacks the evidence its acceptance names (pairs)" \
  && ok "a promote without a reason is refused: shadows record no pairs yet, so the default acceptance (pairs) is missing" || bad "promote without a reason: $OUT"
OUT="$(cli promote e2e/versioned v2 --autonomy propose --gate lint=fail 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 400.*failed result (gate lint)" && ok "a failed gate is refused without a reason" || bad "failed gate: $OUT"
OUT="$(cli promote e2e/versioned v2 --autonomy auto --reason "e2e: above the cap" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 400.*capped at propose" && ok "a promote above the routine's max_autonomy is refused" || bad "promote above the cap: $OUT"
OUT="$(cli promote e2e/versioned v9 --autonomy propose --reason "e2e" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 404" && ok "a promote of a version that is not there is refused (404)" || bad "promote v9: $OUT"
OUT="$(cli promote e2e/versioned v1 --autonomy propose --reason "e2e" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 409.*is live, not a shadow" && ok "the live version cannot be promoted again" || bad "promote the live one: $OUT"
[ "$(vstates e2e/versioned)" = "1:live 2:shadow 3:shadow 4:shadow 5:shadow 6:shadow" ] && ok "every refused call left the states alone" || bad "refusals changed the states: $(vstates e2e/versioned)"
OUT="$(cli promote e2e/versioned v2 --autonomy propose --reason "e2e: no pairs yet; the diff was read" --no-pairs --gate replay=pass@e2e/run.sh --eval run-1=pass 2>&1)"
echo "$OUT" | grep >/dev/null '"state":"live"' && [ "$(vstates e2e/versioned)" = "1:previous 2:live 3:shadow 4:shadow 5:shadow 6:shadow" ] \
  && ok "promote with the autonomy, a reason and evidence: v2 live, v1 previous" || bad "promote: $OUT / $(vstates e2e/versioned)"
[ "$(vrow e2e/versioned 2 "['state_by'] + '|' + x['autonomy_at_promote'] + '|' + x['promotion']['gates'][0]['ref'] + '|' + ','.join(x['promotion']['missing']) + '|' + ','.join(x['promotion']['acceptance'])")" = "operator|propose|e2e/run.sh|pairs|pairs" ] \
  && ok "the promote recorded who, the autonomy, the acceptance in force, the evidence given and what it went ahead without" || bad "promote record: $(vrow e2e/versioned 2 '')"
EV="$(cli events -n 40)"
echo "$EV" | grep >/dev/null "operator .*version.promote .*e2e/versioned@v2 .*gate replay=pass.*eval run-1=pass.*no pairs yet" \
  && echo "$EV" | grep >/dev/null "control.autonomy .*e2e/versioned .*propose" && ok "the event log records the promote and the autonomy chosen, who and why" || bad "promote events: $(echo "$EV" | grep -E 'version|e2e/versioned' | head -4)"
OUT="$(cli retire e2e/versioned v2 --reason "e2e" 2>&1)"; OUT2="$(cli retire e2e/versioned v1 --reason "e2e" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 409.*roll back or promote instead" && echo "$OUT2" | grep >/dev/null "HTTP 409.*roll back or promote instead" \
  && ok "the live and the previous version cannot be retired" || bad "retire live/previous: $OUT / $OUT2"
cli retire e2e/versioned v4 --reason "e2e: not needed" >/dev/null 2>&1 && [ "$(vrow e2e/versioned 4 "['state'] + '|' + x['retired_kind']")" = "retired|retired" ] \
  && ok "a shadow is retired with a reason" || bad "retire a shadow: $(vstates e2e/versioned)"
cli status | grep -E "^e2e/versioned " | grep >/dev/null " propose  *propose " && ok "status shows the autonomy the promote set" || bad "status after the promote: $(cli status | grep '^e2e/versioned')"
# The rename (autonomy shadow became record; routines still read FLEETD_STAGE=shadow): e2e/stage-probe, at record, exits 0 only if it does
cli status | grep -E "^e2e/stage-probe " | grep >/dev/null " record  *record " && ok "a routine at the lowest level reads record in status" || bad "stage-probe status: $(cli status | grep '^e2e/stage-probe')"
cli run e2e/stage-probe >/dev/null
for i in $(seq 1 30); do cli runs e2e/stage-probe -n 1 | grep >/dev/null -E " (ok|error|timeout) " && break; sleep 1; done
[ "$(runs_json e2e/stage-probe | python3 -c 'import json,sys; r=json.load(sys.stdin)["runs"][0]; print(r["status"], r["autonomy"])')" = "ok record" ] \
  && ok "at autonomy record a run records autonomy record and reads FLEETD_STAGE=shadow (the probe exits 0 only then)" || bad "stage-probe run: $(cli runs e2e/stage-probe -n 1)"
cli autonomy e2e/stage-probe propose --reason "e2e: up and back down" >/dev/null && cli autonomy e2e/stage-probe record >/dev/null \
  && cli events -n 10 | grep >/dev/null "control.autonomy .*e2e/stage-probe .*record" && ok "the autonomy control lists the new word: set back to record, the event says record" || bad "stage-probe autonomy events: $(cli events -n 6)"
# The freeze: a live version never changes. One byte of v2's folder (live) and flotillad refuses to start; -check refuses it first.
SPEC2="$WORK/config/specs/e2e/versioned/v2/spec.yaml"; SPEC3="$WORK/config/specs/e2e/versioned/v3/spec.yaml"
V3H="$(vrow e2e/versioned 3 "['hash']")"
cp "$SPEC2" "$WORK/v2.spec.orig"; cp "$SPEC3" "$WORK/v3.spec.orig"
echo "# one byte" >> "$SPEC2"
OUT="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"; RC=$?
[ $RC = 2 ] && echo "$OUT" | grep >/dev/null "^versions: e2e/versioned v2 is live and its folder changed (recorded [0-9a-f]*, found [0-9a-f]*): spec.yaml (changed)" \
  && ok "-check refuses a changed live version, naming the file (exit $RC)" || bad "-check on a changed live folder: rc=$RC $(echo "$OUT" | grep -E '^versions' | head -2)"
echo "$OUT" | grep >/dev/null "ship the change as v7 (a shadow or retired folder may change), or .flotilla versions rerecord e2e/versioned v2 --reason R. if this change is deliberate" \
  && ok "the refusal says the way out: the next number, or flotilla versions rerecord" || bad "the freeze refusal's text: $(echo "$OUT" | grep -E '^versions' | head -1)"
$D restart flotillad >/dev/null 2>&1
seen flotillad "versions: e2e/versioned v2 is live and its folder changed" && ok "flotillad refuses to start on it (exit 2), and says why" || bad "flotillad started over a changed live version: $($D logs flotillad 2>&1 | tail -3)"
cli health >/dev/null 2>&1 && bad "flotillad answers while its live version is changed" || ok "flotillad does not serve while a live version is changed"
cp "$WORK/v2.spec.orig" "$SPEC2"
$D start flotillad >/dev/null 2>&1
for i in $(seq 1 45); do cli health >/dev/null 2>&1 && break; sleep 2; done
cli health >/dev/null 2>&1 && [ "$(vstates e2e/versioned | cut -d' ' -f1-2)" = "1:previous 2:live" ] \
  && $D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check >/dev/null 2>&1 \
  && ok "restored, -check passes and flotillad starts: v2 still live, v1 previous (nothing was re-recorded)" || bad "after restoring v2: $(vstates e2e/versioned) $($D logs flotillad 2>&1 | tail -2)"
# a shadow may change: it starts fine and its hash follows its folder
echo "# an edited shadow" >> "$SPEC3"
$D restart flotillad >/dev/null 2>&1
for i in $(seq 1 45); do cli health >/dev/null 2>&1 && break; sleep 2; done
[ "$(vrow e2e/versioned 3 "['state']")" = shadow ] && [ "$(vrow e2e/versioned 3 "['hash']")" != "$V3H" ] && cli events -n 20 | grep >/dev/null "version.update .*e2e/versioned@v3 .*shadow changed" \
  && ok "a changed shadow starts fine and its hash follows (a version.update event says so)" || bad "changed shadow: $(vrow e2e/versioned 3 "['state'] + ' ' + x['hash'][:12]") (was ${V3H:0:12})"

# A flat routine's v1 is not frozen. e2e/flat-playbook is a flat spec with a playbook: an edit of the playbook is an ordinary change,
# -check says it will re-record it (the line deploy_fleets.sh repeats), and the next start records the new hash with a version.update.
FPB="$WORK/config/specs/e2e/playbooks/flat.md"
FH="$(vrow e2e/flat-playbook 1 "['hash']")"
[ "$(vstates e2e/flat-playbook)" = "1:live" ] && ok "e2e/flat-playbook (a flat spec) is its own live v1" || bad "flat-playbook versions: $(vstates e2e/flat-playbook)"
echo "an edit of the playbook" >> "$FPB"
OUT="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"; RC=$?
[ $RC = 0 ] && echo "$OUT" | grep >/dev/null "^versions: e2e/flat-playbook v1 (flat) re-recorded: prompt-file:playbooks/flat.md (changed)$" \
  && ok "-check passes a changed flat routine and says it will re-record it (exit $RC)" || bad "-check on a changed playbook: rc=$RC $(echo "$OUT" | grep -E '^versions' | head -3)"
$D restart flotillad >/dev/null 2>&1
for i in $(seq 1 45); do cli health >/dev/null 2>&1 && break; sleep 2; done
FH2="$(vrow e2e/flat-playbook 1 "['hash']")"
[ "$(vrow e2e/flat-playbook 1 "['state']")" = live ] && [ "$FH2" != "$FH" ] \
  && cli events -n 40 | grep >/dev/null "version.update .*e2e/flat-playbook@v1 .*legacy v1 (flat) re-recorded.*prompt-file:playbooks/flat.md (changed)" \
  && ok "the start re-recorded the flat routine's v1 (still live), with a version.update event naming the playbook" || bad "flat re-record: $(vrow e2e/flat-playbook 1 "['state'] + ' ' + x['hash'][:12]") (was ${FH:0:12}) $(cli events -n 40 | grep flat-playbook | head -2)"
# The routine's own fields are in no version's hash: each start records them (a snapshot) and a field that moved is one routine.update event. A budget
# changed between two starts shows in -check first, then as the event; the start after it, with nothing moved, writes none.
sed -i 's/day_usd: 0/day_usd: 1/' "$WORK/config/specs/e2e/flat-playbook.yaml"
OUT="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"; RC=$?
[ $RC = 0 ] && [ "$(echo "$OUT" | grep -c -F 'routine e2e/flat-playbook: budget {"day_usd":0} -> {"day_usd":1}')" = 1 ] \
  && ok "-check says a routine's changed budget before the deploy: routine e2e/flat-playbook: budget {\"day_usd\":0} -> {\"day_usd\":1} (exit $RC)" || bad "-check on a changed budget: rc=$RC $(echo "$OUT" | grep -E '^routine|^versions' | head -3)"
$D restart flotillad >/dev/null 2>&1
for i in $(seq 1 45); do cli health >/dev/null 2>&1 && break; sleep 2; done
cli events -n 300 | grep >/dev/null 'routine.update .*e2e/flat-playbook .*budget {"day_usd":0} -> {"day_usd":1}' \
  && ok "the start recorded the changed budget as a routine.update event naming the field, old and new" || bad "routine.update: $(cli events -n 300 | grep 'routine.update' | head -3)"
$D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1 | grep >/dev/null '^routine e2e/flat-playbook: ' \
  && bad "-check still reports the budget after the start recorded it" || ok "after the start, -check has no field change to report"
# The forms of a re-record that apply to any version, against the flotillad that runs the flat routine: an agent is refused, a reason is needed,
# the version must exist, a shadow follows its folder.
OUT="$(agent versions rerecord e2e/flat-playbook v1 --reason "e2e: an agent tries" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 403" && ok "an agent cannot re-record a version ($OUT)" || bad "agent rerecord: $OUT"
OUT="$(cli versions rerecord e2e/flat-playbook v1 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 400.*needs a reason" && ok "a re-record without a reason is refused" || bad "rerecord without a reason: $OUT"
OUT="$(cli versions rerecord e2e/flat-playbook v2 --reason "e2e: no such version" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 404" && ok "a re-record of a version that is not there is refused (404)" || bad "rerecord v2: $OUT"
OUT="$(cli versions rerecord e2e/versioned v3 --reason "e2e: a shadow follows its folder" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 409.*is a shadow" && ok "a shadow is not re-recorded (409)" || bad "rerecord a shadow: $OUT"
# Moving it to the folder layout changes its recipe, so its hash. A move that provably changes nothing (every key and file the flat v1 was recorded with is
# equal) is re-recorded at the start, and -check prints what it compared; a move that changes one byte is refused, naming it.
FD="$WORK/config/specs/e2e"
RUNNING="$WORK/running-fleets"   # the flat spec as it ran: the moved routine.yaml is made from its own fields
mkdir -p "$RUNNING/e2e/playbooks" && cp "$FD/flat-playbook.yaml" "$RUNNING/e2e/" && cp "$FPB" "$RUNNING/e2e/playbooks/flat.md" && chmod -R a+rX "$RUNNING"
rm "$FD/flat-playbook.yaml"; mkdir -p "$FD/flat-playbook/v1"
flat_routine() {  # $1: the day_usd the folder's routine.yaml has: the flat spec's own routine fields (its run and prompts left out), with that budget
  awk '/^run:/{skip=1} /^autonomy:/{skip=0} !skip' "$RUNNING/e2e/flat-playbook.yaml" | sed "s/day_usd: [0-9.]*/day_usd: $1/" > "$FD/flat-playbook/routine.yaml"
}
flat_routine 1
cat > "$FD/flat-playbook/v1/spec.yaml" <<EOF
run:
  command: "true"
  workdir: .
  timeout: 1m
prompts:
  system: flat.md
EOF
cp "$FPB" "$FD/flat-playbook/v1/flat.md"; echo "one changed byte" >> "$FD/flat-playbook/v1/flat.md"; chmod -R a+rX "$FD"
OUT="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"; RC=$?
[ $RC = 2 ] && echo "$OUT" | grep >/dev/null "^versions: e2e/flat-playbook v1 is live and its folder changed .*not the same as the recorded flat spec: prompt-file:playbooks/flat.md -> v1/flat.md (changed)" \
  && echo "$OUT" | grep >/dev/null "ship the change as v2 .*flotilla versions rerecord e2e/flat-playbook v1 --reason R" \
  && ok "a move with one changed byte is refused by -check, naming the playbook, the way out named (exit $RC)" || bad "-check on the impure move: rc=$RC $(echo "$OUT" | grep -E '^versions' | head -2)"
# the byte put back, and a changed budget in routine.yaml (not frozen, so not refused)
cp "$FPB" "$FD/flat-playbook/v1/flat.md"; flat_routine 1; chmod -R a+rX "$FD"
OUT="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"; RC=$?
LINE="versions: e2e/flat-playbook v1 moved to the folder layout, unchanged (run, prompt-file:playbooks/flat.md -> v1/flat.md, prompts): re-recorded"
[ $RC = 0 ] && [ "$(echo "$OUT" | grep -c -F "$LINE")" = 1 ] \
  && ok "a pure move passes -check and says what was compared: $LINE (exit $RC)" || bad "-check on the pure move: rc=$RC $(echo "$OUT" | grep -E '^versions' | head -3)"
[ "$(echo "$OUT" | grep -c -F 'versions: e2e/flat-playbook v1 routine fields: identical to the stored snapshot (information only, not a gate')" = 1 ] \
  && ok "a pure move after a snapshot prints routine fields: identical to the stored snapshot" || bad "the pure move's fields line: $(echo "$OUT" | grep -E '^versions|^routine' | head -4)"
# the routine's own fields may change in the same deploy: reported, never refused
flat_routine 3; chmod -R a+rX "$FD"
OUT="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"; RC=$?
[ $RC = 0 ] && echo "$OUT" | grep >/dev/null -F 'routine e2e/flat-playbook: budget {"day_usd":1} -> {"day_usd":3}' \
  && echo "$OUT" | grep >/dev/null -F 'versions: e2e/flat-playbook v1 routine fields: changed since the stored snapshot: budget {"day_usd":1} -> {"day_usd":3} (information only, not a gate' \
  && ok "a moved routine with a changed budget is reported against the snapshot and not refused (exit $RC)" || bad "-check on the move with a changed budget: rc=$RC $(echo "$OUT" | grep -E '^versions|^routine' | head -4)"
flat_routine 1; chmod -R a+rX "$FD"
$D restart flotillad >/dev/null 2>&1
for i in $(seq 1 45); do cli health >/dev/null 2>&1 && break; sleep 2; done
FH4="$(vrow e2e/flat-playbook 1 "['hash']")"
[ "$(vstates e2e/flat-playbook)" = "1:live" ] && [ -n "$FH4" ] && [ "$FH4" != "$FH2" ] \
  && cli events -n 40 | grep >/dev/null "version.update .*e2e/flat-playbook@v1 .*legacy v1 (flat) moved to the folder layout, unchanged (run, prompt-file:playbooks/flat.md -> v1/flat.md, prompts): re-recorded: hash $FH2 -> $FH4" \
  && ok "the start re-recorded the move: v1 still live with the folder's hash, and a version.update event carrying the comparison" || bad "the start on the pure move: $(vstates e2e/flat-playbook) ${FH4:0:12} (was ${FH2:0:12}) $(cli events -n 40 | grep flat-playbook | head -2) $($D logs flotillad 2>&1 | grep versions | tail -2)"
$D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check >/dev/null 2>&1 \
  && [ "$(cli events -n 300 | grep -c 'routine.update .*e2e/flat-playbook ')" = 1 ] \
  && ok "after the start, -check passes with nothing left to re-record, and the starts since the budget change wrote no further routine.update event" || bad "-check after the re-record: $($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1 | grep '^versions' | head -2)"

fi
if want confighash; then
echo "== a changed config recreates its service (deploy.sh's FLOTILLA_CONFIG_HASH_*)"
F0="$(started flotillad)"; E0="$(started flotilla-egress)"; W0="$(started flotilla-worker-triage)"
FLOTILLA_CONFIG_HASH_FLOTILLAD=changed-in-e2e compose up -d >"$WORK/up2.log" 2>&1
for i in $(seq 1 30); do cli health >/dev/null 2>&1 && break; sleep 2; done
[ "$(started flotillad)" != "$F0" ] && ok "a new flotillad config hash recreates flotillad" || bad "flotillad not recreated: $(cat "$WORK/up2.log")"
[ "$(started flotilla-egress)" = "$E0" ] && [ "$(started flotilla-worker-triage)" = "$W0" ] \
  && ok "the proxy and the worker, whose config did not change, keep running" || bad "other services were recreated too"
# The monitor runs again (by hand: since 355bb2f a restarted flotillad runs a routine when it is due, not at
# start); the backup is still old: one case per incident (ADR-0005 decision 1, amended 2026-11-13), the repeat
# absorbed into the closed shadow case: as an event when its data changed, else counted on the case
# ("repeated N times unchanged", 13c46f6).
cli run jobs/backup-monitor >/dev/null
for i in $(seq 1 20); do
  CID="$(agent case list --all 2>&1 | awk '/failed_backup/ {print $1}')"
  agent case show $CID 2>/dev/null | grep >/dev/null -E "signal.absorbed|^repeated [0-9]+ times unchanged" && break; sleep 2
done
N="$(agent case list --all 2>&1 | grep -c failed_backup)"
[ "$N" = 1 ] && agent case show $CID 2>&1 | grep >/dev/null -E "signal.absorbed .*jobs/backup-monitor|^repeated [0-9]+ times unchanged" \
  && ok "after the restart the monitor's repeat went into the incident's case (one case, absorbed or counted)" \
  || bad "one case per incident: $N failed_backup cases; $(agent case show $CID 2>&1 | tail -3)"

fi
if want hostagent; then
echo "== the host agent (ADR-0006): installed by deploy/host/remote-install.sh; enrol, connect, heartbeat, a flotillad restart, revocation, remove"
mkdir -p "$WORK/host"
cp "$WORK/config/host-ca/root.crt" "$WORK/host/root.crt"
# one harmless unprivileged action and one privileged (sudo /bin/true), so the sudoers path runs too;
# host.true once an hour, so H9c's second one is refused by the host's own rate limit
cat > "$WORK/host/config.json" <<EOF
{"host": "e2e-host", "user": "_flotilla", "ceiling": "propose",
 "signing_keys": {"e2e1": "$SIGNPUB"},
 "actions": {
   "host.true": {"argv": ["/bin/true"], "params": {"host": {"kind": "this_host"}}, "per_hour": 1, "timeout": "10s"},
   "host.true_root": {"argv": ["/usr/bin/sudo", "-n", "/bin/true"], "params": {"host": {"kind": "this_host"}}, "per_hour": 5, "timeout": "10s"}}}
EOF
printf 'FLOTILLA_ADDR=flotillad:8487\nFLOTILLA_SERVER_NAME=flotillad\nFLOTILLA_HEARTBEAT=2s\n' > "$WORK/host/agent.env"
chmod 644 "$WORK/host/"*
# the host: a root shell, as the operator's sudo over SSH is; the audit log's directory on a volume, with
# the one capability chattr +a needs (the container's own filesystem does not take the flag)
$D run -d --name flotilla-e2e-host --label $LABEL --network ${P}_flotilla --memory 256m --cap-add LINUX_IMMUTABLE \
  -v flotilla-e2e-host-audit:/var/log/flotilla-host -v "$WORK/host:/stage-src:ro" "$HOST_AGENT_IMAGE" >/dev/null || bad "the stand-in host did not start"
HT="$(cli host enrolment e2e-host 2>&1 | sed -n 's/.*"token":"\(fle_[0-9a-f]*\)".*/\1/p')"
[ ${#HT} = 68 ] && ok "an operator minted an enrolment token" || bad "host enrolment: $(cli host enrolment e2e-host 2>&1)"
# stage as install.sh does: the files, the binary, the token (mode 600)
hx flotilla-e2e-host sh -c 'mkdir -m 700 /stage && cp /stage-src/* /stage/ && cp /opt/flotilla-host /stage/' \
  && cp "$ROOT/deploy/host/remote-install.sh" "$WORK/host-remote-install.sh" && $D cp "$WORK/host-remote-install.sh" flotilla-e2e-host:/stage/remote-install.sh >/dev/null \
  && printf '%s\n' "$HT" | hx -i flotilla-e2e-host sh -c 'umask 077 && cat > /stage/token' || bad "staging"
OUT="$(hx flotilla-e2e-host bash /stage/remote-install.sh /stage install --no-service 2>&1)"; echo "$OUT"
echo "$OUT" | grep >/dev/null "^INSTALL PASS" && echo "$OUT" | grep >/dev/null "sudoers: 1 line(s) in /etc/sudoers.d/flotilla-host, checked with visudo" \
  && ok "remote-install.sh: user, files, sudoers (checked with visudo), enrolled" || bad "install: $OUT"
hx flotilla-e2e-host test ! -e /stage/token && ok "the token is gone from the staging folder" || bad "the token was left on the host"
M="$(hx flotilla-e2e-host sh -c 'stat -c "%n %a %U" /usr/local/libexec/flotilla-host/flotilla-host /etc/flotilla-host/config.json /etc/flotilla-host/root.crt /var/db/flotilla-host /var/db/flotilla-host/key.pem /var/log/flotilla-host /var/log/flotilla-host/audit.jsonl /etc/sudoers.d/flotilla-host' 2>&1 | tr '\n' ';')"
[ "$M" = "/usr/local/libexec/flotilla-host/flotilla-host 755 root;/etc/flotilla-host/config.json 644 root;/etc/flotilla-host/root.crt 644 root;/var/db/flotilla-host 700 _flotilla;/var/db/flotilla-host/key.pem 600 _flotilla;/var/log/flotilla-host 755 root;/var/log/flotilla-host/audit.jsonl 600 _flotilla;/etc/sudoers.d/flotilla-host 440 root;" ] \
  && ok "owners and modes: binary and config root's, state and key the agent's (700/600), audit log the agent's in a root-owned directory, sudoers 440" || bad "owners and modes: $M"
S="$(hx flotilla-e2e-host grep -v '^#' /etc/sudoers.d/flotilla-host 2>&1)"   # the rules, below the generated header
[ "$S" = '_flotilla ALL=(root) NOPASSWD: /bin/true ""  # host.true_root' ] && ok "the sudoers line: exactly the privileged action, no arguments allowed ($S)" || bad "sudoers: $S"
hx flotilla-e2e-host sh -c 'lsattr /var/log/flotilla-host/audit.jsonl' 2>&1 | grep >/dev/null '^-*a' \
  && hx flotilla-e2e-host sudo -u _flotilla sh -c 'echo x >> /var/log/flotilla-host/audit.jsonl' 2>/dev/null \
  && ! hx flotilla-e2e-host sudo -u _flotilla sh -c ': > /var/log/flotilla-host/audit.jsonl' 2>/dev/null \
  && ok "the audit log is append-only (chattr +a): the agent's user appends, cannot truncate" || bad "append-only: $(hx flotilla-e2e-host lsattr /var/log/flotilla-host/audit.jsonl 2>&1)"
# the line appended above is not an audit entry: the agent would refuse that log, as it should; start
# its log over (as root, which clears the flag first) before the agent runs
hx flotilla-e2e-host sh -c 'chattr -a /var/log/flotilla-host/audit.jsonl && : > /var/log/flotilla-host/audit.jsonl && chattr +a /var/log/flotilla-host/audit.jsonl'
OUT="$(printf '%s\n' "$HT" | hx -i flotilla-e2e-host sudo -u _flotilla /usr/local/libexec/flotilla-host/flotilla-host enrol -config /etc/flotilla-host/config.json \
  -state /var/db/flotilla-host -audit /var/log/flotilla-host/audit.jsonl -root /etc/flotilla-host/root.crt -addr flotillad:8487 -server-name flotillad 2>&1)"
echo "$OUT" | grep >/dev/null "enrolment refused" && ok "the same token a second time is refused" || bad "a used token: $OUT"
cli host list | grep >/dev/null "^e2e-host  *no  *- .*[0-9]T" && ok "host list shows it enrolled, not yet connected, with its certificate's expiry" || bad "host list: $(cli host list 2>&1)"
$D logs flotillad 2>&1 | grep '"msg":"host executions are signed".*"key_id":"e2e1"' >/dev/null \
  && ok "flotillad loaded the execution-signing key from its secrets volume (secrets.sh)" || bad "no signing key at start: $($D logs flotillad 2>&1 | grep -i 'sign' | tail -2)"
# the service: what launchd or systemd would run, as the agent's user (the container has neither)
hx -d flotilla-e2e-host sh -c 'sudo -u _flotilla /usr/local/libexec/flotilla-host/flotilla-host run -config /etc/flotilla-host/config.json -state /var/db/flotilla-host -audit /var/log/flotilla-host/audit.jsonl -root /etc/flotilla-host/root.crt -addr flotillad:8487 -server-name flotillad -heartbeat 2s 2>>/var/log/flotilla-host/agent.log'
hseen '"msg":"connected to flotillad"' && seen flotillad '"msg":"host connected".*"host":"e2e-host"' \
  && ok "the installed agent connected as _flotilla; flotillad authenticated it as e2e-host" || bad "connect: $(hostlog | tail -3)"
seen flotillad '"msg":"host log".*"host":"e2e-host","line":"connected to flotillad"' \
  && ok "the agent's own log line reached flotillad's log (G5: host logs through the connection)" || bad "host log: $($D logs flotillad 2>&1 | grep '"host log' | tail -2)"
sleep 8   # four heartbeats at 2s; silent for three, flotillad would drop it
! $D logs flotillad 2>&1 | grep '"msg":"host disconnected"' >/dev/null && ok "heartbeats keep the connection (8s, flotillad drops after 6s of silence)" \
  || bad "the connection dropped: $($D logs flotillad 2>&1 | grep '"host' | tail -3)"
HL="$(cli host list 2>&1)"
# nothing has run on the host, so its audit log is empty and has no chain head yet ("-")
echo "$HL" | grep >/dev/null "^e2e-host  *yes  *$TAG  *[0-9-]*T[0-9:.]*Z  *-\?[0-9.]*[mµn]\?s  *[0-9-]*T[0-9:.]*Z  *-  *-$" \
  && ok "host list: connected, its version, last contact and clock offset, from the store (no chain head: nothing ran)" || bad "host list while connected: $HL"
! $D logs flotillad 2>&1 | grep '"msg":"host alert"' >/dev/null && ok "no host alert while it is connected" || bad "a host alert: $($D logs flotillad 2>&1 | grep '"host alert' | tail -2)"
cli action autonomy host.true propose --reason "e2e: the security e2e's harmless action" >/dev/null 2>&1
cli action list 2>&1 | grep >/dev/null "^host.true  *propose " && ok "host.true promoted to propose (operator, with a reason)" || bad "promote: $(cli action list 2>&1)"
fi
if want versionscase; then
echo "== routine versions on a case: a v2 plan waits for approval, a rollback withdraws it, a promote leaves a run in flight alone"
# v2 is live (promoted above, autonomy propose). The monitor's case goes to v2, whose plan waits at propose.
cli action autonomy host.vtrue propose --reason "e2e: the versions check's harmless action" >/dev/null 2>&1
VC="$(proposed e2e_versioned versioned-monitor)"
[ -n "$VC" ] && ok "the case went to the live version (v2); its plan waits for approval at propose ($VC awaiting approval)" \
  || bad "no e2e_versioned case awaiting approval: $(cli case list --all 2>&1 | grep e2e_versioned)"
[ "$(runs_json e2e/versioned | python3 -c 'import json,sys; r=json.load(sys.stdin)["runs"]; print(len(r), r[0]["version"], r[0]["status"])')" = "1 2 ok" ] \
  && ok "the run after the promote recorded version 2" || bad "the run's version: $(runs_json e2e/versioned | head -c 300)"
OUT="$(cli rollback e2e/versioned --autonomy propose 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 400.*needs a reason" && ok "a rollback without a reason is refused" || bad "rollback without a reason: $OUT"
OUT="$(cli rollback e2e/versioned --reason "e2e: no autonomy" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 400.*needs the routine's autonomy" && ok "a rollback without the autonomy is refused: the operator confirms it" || bad "rollback without autonomy: $OUT"
OUT="$(cli rollback e2e/versioned --autonomy propose --reason "e2e: v2's plan is wrong")"
echo "$OUT" | grep >/dev/null "\"rolled_back\":2" && echo "$OUT" | grep >/dev/null "\"live\":1" && echo "$OUT" | grep >/dev/null "\"cases\":\[\"$VC\"\]" \
  && ok "rolled back: v1 live again; the answer names the case whose plan was withdrawn ($OUT)" || bad "rollback: $OUT"
OUT="$(cli case approve "$VC" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 409" && ok "the withdrawn plan cannot be approved (409: $(echo "$OUT" | sed 's/^HTTP 409: //' | head -c 80))" || bad "approve after the rollback: $OUT"
[ "$(vstates e2e/versioned | cut -d' ' -f1-2)" = "1:live 2:retired" ] \
  && [ "$(vrow e2e/versioned 2 "['retired_kind'] + '|' + x['state_by'] + '|' + x['state_why']")" = "rolled_back|operator|e2e: v2's plan is wrong" ] \
  && ok "v2 is retired as rolled back, who and why recorded; v1 is live and there is no previous" || bad "after the rollback: $(vstates e2e/versioned) $(vrow e2e/versioned 2 '')"
SHOW="$(cli case show "$VC" 2>&1)"
echo "$SHOW" | grep >/dev/null "case.rolled_back .*operator .*by operator .*awaiting_approval->handed_over" \
  && ok "the case went back to the acting routine: case.rolled_back, awaiting_approval to handed_over, by the operator" || bad "case after the rollback: $(echo "$SHOW" | tail -4)"
cli events -n 40 | grep >/dev/null "operator .*version.rollback .*e2e/versioned@v2 .*v1 is live again.*1 plan(s) awaiting approval marked rolled back.*e2e: v2's plan is wrong" \
  && ok "the event log records the rollback, who and why" || bad "rollback event: $(cli events -n 40 | grep version | head -3)"
OUT="$(cli rollback e2e/versioned --autonomy propose --reason "e2e: again" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 409.*no previous" && ok "with no previous version a rollback is refused" || bad "second rollback: $OUT"
# v1 plans again (the previous version, on this image): the same case waits again, a v1 plan
status_is "$VC" awaiting_approval && [ "$(runs_json e2e/versioned | python3 -c 'import json,sys; r=json.load(sys.stdin)["runs"]; print(r[0]["version"], r[0]["status"])')" = "1 ok" ] \
  && ok "the live version (v1, the previous one on this image) planned the case again: its run recorded version 1" || bad "re-plan by v1: $(cli case list --all 2>&1 | grep e2e_versioned) $(runs_json e2e/versioned | head -c 200)"
cli case reject "$VC" --reason "e2e: versions check, plan not needed" >/dev/null 2>&1 && cli case close "$VC" --reason "e2e: versions check done" >/dev/null 2>&1 && status_is "$VC" closed \
  && ok "the case was rejected and closed by the operator (the target is released)" || bad "closing the versions case: $(cli case show "$VC" 2>&1 | tail -2)"
cli action autonomy host.vtrue propose --reason "e2e: back to propose after the rejection's demotion" >/dev/null 2>&1
# a run in flight at promote finishes with the version it started with (e2e/hang's pattern: a hold file in the worker makes the run last)
$D exec flotilla-worker-ops-act-host touch /tmp/e2e-versioned-hold >/dev/null 2>&1 || bad "cannot create the hold file in the ops-act-host worker"
cli run e2e/versioned-monitor >/dev/null
for i in $(seq 1 60); do cli status | grep -E "^e2e/versioned " | grep >/dev/null "running " && break; sleep 1; done
RUNID="$(cli status | awk '/^e2e\/versioned / {for (i = 1; i <= NF; i++) if ($i == "running") print $(i + 1)}')"
[ -n "$RUNID" ] && ok "a run of the live version (v1) is in flight ($RUNID)" || bad "no run in flight: $(cli status | grep '^e2e/versioned')"
OUT="$(cli promote e2e/versioned v3 --autonomy propose --reason "e2e: promoted with a run in flight" --no-pairs 2>&1)"
$D exec flotilla-worker-ops-act-host rm -f /tmp/e2e-versioned-hold >/dev/null 2>&1
echo "$OUT" | grep >/dev/null '"state":"live"' && [ "$(vstates e2e/versioned | cut -d' ' -f1-3)" = "1:previous 2:retired 3:live" ] \
  && ok "v3 promoted while that run was in flight: v3 live, v1 previous, the old previous (none) and v2 stay retired" || bad "promote with a run in flight: $OUT / $(vstates e2e/versioned)"
for i in $(seq 1 90); do cli status | grep -E "^e2e/versioned " | grep >/dev/null "running " || break; sleep 1; done
[ "$(runs_json e2e/versioned | python3 -c 'import json,sys; r=[x for x in json.load(sys.stdin)["runs"] if x["id"] == sys.argv[1]][0]; print(r["version"], r["status"])' "$RUNID")" = "1 ok" ] \
  && ok "the run in flight finished as v1 (the version it started with) and counted" || bad "the run in flight: $(runs_json e2e/versioned | head -c 300)"
VC2="$(cli case list --all 2>&1 | awk '$2 == "e2e_versioned" && /awaiting_approval/ {print $1; exit}')"
for i in $(seq 1 30); do [ -n "$VC2" ] && break; sleep 2; VC2="$(cli case list --all 2>&1 | awk '$2 == "e2e_versioned" && /awaiting_approval/ {print $1; exit}')"; done
[ -n "$VC2" ] && ! cli case show "$VC2" 2>&1 | grep >/dev/null "rolled_back" \
  && ok "the plan v1 made stays approvable (it was live when it made it): $VC2 awaits approval, not marked rolled back" || bad "the in-flight plan: $(cli case list --all 2>&1 | grep e2e_versioned)"
cli case reject "$VC2" --reason "e2e: versions check, plan not needed" >/dev/null 2>&1 && cli case close "$VC2" --reason "e2e: versions check done" >/dev/null 2>&1 && status_is "$VC2" closed \
  && ok "that case was rejected and closed by the operator" || bad "closing the second versions case: $(cli case show "$VC2" 2>&1 | tail -2)"

# a rollback with a run in flight (the hold file again): v3 is live, v1 previous. The run of v3 is cancelled as a STOP cancels
# it and the worker is asked to stop it; the case it was working stays with the acting routine (v1 is live again and plans it
# once the dispatcher's backoff after a run with no decision is over).
cli action autonomy host.vtrue propose --reason "e2e: back to propose after the rejection's demotion" >/dev/null 2>&1
$D exec flotilla-worker-ops-act-host touch /tmp/e2e-versioned-hold >/dev/null 2>&1 || bad "cannot create the hold file in the ops-act-host worker"
cli run e2e/versioned-monitor >/dev/null
for i in $(seq 1 90); do cli status | grep -E "^e2e/versioned " | grep >/dev/null "running " && break; sleep 1; done
RUN3="$(cli status | awk '/^e2e\/versioned / {for (i = 1; i <= NF; i++) if ($i == "running") print $(i + 1)}')"
[ -n "$RUN3" ] && [ "$(vstates e2e/versioned | cut -d' ' -f1-3)" = "1:previous 2:retired 3:live" ] && ok "a run of the live version (v3) is in flight ($RUN3)" || bad "no v3 run in flight: $(cli status | grep '^e2e/versioned') $(vstates e2e/versioned)"
OUT="$(cli rollback e2e/versioned --autonomy propose --reason "e2e: rolled back with a run in flight")"
$D exec flotilla-worker-ops-act-host rm -f /tmp/e2e-versioned-hold >/dev/null 2>&1
echo "$OUT" | grep >/dev/null '"rolled_back":3' && echo "$OUT" | grep >/dev/null '"live":1' && echo "$OUT" | grep >/dev/null "\"cancelled\":\[\"$RUN3\"\]" \
  && ok "rolled back with the run in flight: v1 live again, the answer names the run it cancelled ($RUN3)" || bad "rollback with a run in flight: $OUT"
for i in $(seq 1 30); do  # wait on the run's own status in the JSON, not on a word in the table
  [ "$(runs_json e2e/versioned | python3 -c 'import json,sys; print([x["status"] for x in json.load(sys.stdin)["runs"] if x["id"] == sys.argv[1]][0])' "$RUN3" 2>/dev/null)" != running ] && break; sleep 1
done
CR="$(runs_json e2e/versioned | python3 -c 'import json,sys; r=[x for x in json.load(sys.stdin)["runs"] if x["id"] == sys.argv[1]][0]; print(r.get("version"), r["status"], r.get("detail", ""))' "$RUN3" 2>&1)"
case "$CR" in "3 cancelled "*"v3 rolled back"*) ok "the run ended cancelled as v3, with who and why in its detail ($CR)" ;; *) bad "the cancelled run: $CR" ;; esac
cli events -n 60 | grep "run.cancel .*e2e/versioned .*run $RUN3: v3 rolled back" | grep -v "not delivered" >/dev/null \
  && ok "the worker was asked to stop it (a run.cancel event, delivered)" || bad "run.cancel event: $(cli events -n 60 | grep run.cancel | head -2)"
# The cancelled run made no decision, so the case waits out the dispatcher's backoff (10 minutes) before the live version
# (v1 again) plans it: the e2e shows the case is still with the acting routine, handed over, with no plan from v3.
VC3="$(cli case list --all 2>&1 | awk '$2 == "e2e_versioned" && /handed_over/ {print $1; exit}')"
SHOW="$(cli case show "$VC3" 2>&1)"
[ -n "$VC3" ] && echo "$SHOW" | grep >/dev/null "run.no_decision .*e2e/versioned" && ! echo "$SHOW" | grep >/dev/null "case.planned" \
  && ok "the case stayed with the acting routine: the cancelled run made no decision and filed no plan ($VC3 handed over, waiting its backoff)" || bad "the case after the rollback with a run in flight: $(cli case list --all 2>&1 | grep e2e_versioned) $(echo "$SHOW" | tail -4)"

HC="$(proposed e2e_host_true host-true-monitor)"
[ -n "$HC" ] && ok "the detection became a case; the acting routine planned host.true; policy proposed it ($HC awaiting approval)" \
  || bad "no e2e_host_true case awaiting approval: $(cli case list --all 2>&1 | grep e2e_host_true)"
# Slack approvals (the slack section of flotillad.yaml): the plan that waits is offered to the connector host every 15 seconds. The stand-in host's agent
# is no connector: it logs each plan it is asked to post and posts nothing, which is the e2e's recording of what Slack would have been sent
hseen 'not its Slack connector' && ok "Slack: flotillad asked the connector host to post the plan waiting for approval; the stand-in's agent logged it" \
  || bad "no plan was offered to the Slack connector host: $(hostlog | tail -3) / $($D logs flotillad 2>&1 | grep -i slack | tail -2)"
OUT="$(cli case approve "$HC" 2>&1)"
echo "$OUT" | grep >/dev/null '"approved_by":"operator"' && ok "the operator approved it" || bad "approve: $OUT"
status_is "$HC" closed
SHOW="$(cli case show "$HC" 2>&1)"
echo "$SHOW" | grep >/dev/null "case.executing .*operator .*by operator .*approved host.true on host:e2e-host" \
  && echo "$SHOW" | grep >/dev/null "case.checking .*flotillad .*by act .*on e2e-host: done" \
  && ok "signed, sent, run on the host as _flotilla (/bin/true), done: the case went to checking" || bad "the execution: $SHOW"
echo "$SHOW" | grep >/dev/null "case.closed .*e2e/host-true-verify .*by check .*e2e: nothing to verify" \
  && ok "the check routine verified it: closed verified, the target released" || bad "the check: $SHOW"
# the target is free again: a second host.true reaches the host, which refuses it (per_hour 1)
H2="$(proposed e2e_host_true host-true-monitor)"
[ -n "$H2" ] && cli case approve "$H2" >/dev/null 2>&1 && status_is "$H2" escalated \
  && cli case show "$H2" 2>&1 | grep >/dev/null "case.escalated .*flotillad .*by act .*on e2e-host: refused: host.true ran 1 times in the last hour here, its limit" \
  && ok "a second host.true on the released target: the host refused it, its own rate limit ($H2 escalated)" \
  || bad "the rate limit: $H2 $(cli case show "$H2" 2>&1 | tail -3)"
OUT="$(cli case close "$H2" 2>&1)"
echo "$OUT" | grep >/dev/null "reason" && ok "closing an escalated case needs a reason" || bad "close without a reason: $OUT"
cli case close "$H2" --reason "e2e: refused by the host, as it should" >/dev/null 2>&1 && status_is "$H2" closed \
  && ok "the operator closed it; the target is released" || bad "close: $(cli case show "$H2" 2>&1 | tail -2)"
# an action flotillad's registry declares and the host's allowlist does not
cli action autonomy host.absent propose --reason "e2e: an action the host does not allow" >/dev/null 2>&1
H3="$(proposed e2e_host_absent host-absent-monitor)"
[ -n "$H3" ] && cli case approve "$H3" >/dev/null 2>&1 && status_is "$H3" escalated \
  && cli case show "$H3" 2>&1 | grep >/dev/null "case.escalated .*flotillad .*by act .*on e2e-host: refused: .*host.absent is not allowed on this host" \
  && ok "host.absent, signed and approved: the host refused it, not on its allowlist ($H3 escalated)" \
  || bad "the allowlist: $H3 $(cli case show "$H3" 2>&1 | tail -3)"
# reassigned, it goes back to its acting routine, which plans again; the three executions above are
# the host's hourly limit (policy's per-host cap), so policy defers the new plan
cli case reassign "$H3" --reason "e2e: hand it back" >/dev/null 2>&1
for _ in $(seq 1 45); do cli case show "$H3" 2>&1 | grep >/dev/null "by policy .*planned->handed_over .*defer" && break; sleep 2; done
SHOW="$(cli case show "$H3" 2>&1)"
echo "$SHOW" | grep >/dev/null "case.handed_over .*operator .*by operator .*escalated->handed_over .*reassigned: e2e: hand it back" \
  && echo "$SHOW" | grep >/dev/null "case.planned .*e2e/host-absent-act .*by act .*handed_over->planned" \
  && echo "$SHOW" | grep >/dev/null "case.handed_over .*by policy .*planned->handed_over .*defer at propose.*3 of 3 executions on this host this hour" \
  && ok "the operator reassigned it: its acting routine planned again, and policy deferred it at the host's hourly cap (3 of 3)" \
  || bad "reassign $H3: $(echo "$SHOW" | tail -4) flotillad: $($D logs flotillad 2>&1 | grep "$H3" | tail -4)"
hx flotilla-e2e-host cat /var/log/flotilla-host/audit.jsonl > "$WORK/host-audit.jsonl" 2>/dev/null
AUD="$($D exec -i flotillad /usr/local/bin/flotilla host audit e2e-host --log - < "$WORK/host-audit.jsonl" 2>&1)"; ARC=$?
[ $ARC = 0 ] && echo "$AUD" | grep >/dev/null "^AUDIT PASS: e2e-host, 3 executions on record" && echo "$AUD" | grep >/dev/null "done on both sides" \
  && ok "flotilla host audit: the host's real log matches flotillad's record (AUDIT PASS, 3 executions)" || bad "audit: rc=$ARC $AUD"
sed '0,/"status":"done"/s//"status":"failed"/' "$WORK/host-audit.jsonl" > "$WORK/host-audit-tampered.jsonl"
AUD="$($D exec -i flotillad /usr/local/bin/flotilla host audit e2e-host --log - < "$WORK/host-audit-tampered.jsonl" 2>&1)"; ARC=$?
[ $ARC = 1 ] && echo "$AUD" | grep >/dev/null "^AUDIT FAIL" && ok "a rewritten copy of the host's log fails the audit" || bad "tampered audit: rc=$ARC $AUD"
$D restart flotillad >/dev/null
for i in $(seq 1 30); do cli health >/dev/null 2>&1 && break; sleep 2; done
for i in $(seq 1 30); do [ "$(hostlog | grep -c '"msg":"connected to flotillad"')" -ge 2 ] && break; sleep 1; done
[ "$(hostlog | grep -c '"msg":"connected to flotillad"')" -ge 2 ] && hostlog | grep >/dev/null '"msg":"connection to flotillad lost"' \
  && ok "after a flotillad restart the agent lost the connection and reconnected by itself" || bad "reconnect: $(hostlog | tail -4)"
cli host revoke e2e-host >/dev/null
seen flotillad '"msg":"host cut off".*"host":"e2e-host"' && ok "revoked: flotillad cut the connected host off at its next heartbeat" \
  || bad "revoke: $($D logs flotillad 2>&1 | grep '"host' | tail -3)"
# the agent's next try comes after its backoff (1 s doubling to 60 s, plus up to half again, internal/hostagent Run), which resets
# only after a session of a minute: after this section's restarts it can be 30 to 90 s away (no-effect on 649d659: retry_in 45 s,
# past a 33 s wait), so wait out the longest one
refused=""
for _ in $(seq 1 100); do
  $D logs flotillad 2>&1 | grep >/dev/null '"msg":"host endpoint: refused"' && hostlog | grep >/dev/null 'flotillad refused: refused' && { refused=1; break; }
  sleep 1
done
[ -n "$refused" ] && ok "its reconnects are refused" || bad "after revoke: $(hostlog | tail -3)"
cli host list | grep >/dev/null "^e2e-host  *no .* -  *[0-9-]*T[0-9:.]*Z " && ok "host list shows it revoked, not connected, no certificate" || bad "host list after revoke: $(cli host list 2>&1)"
hx flotilla-e2e-host sh -c 'pkill -u _flotilla flotilla-host; true'
OUT="$(hx flotilla-e2e-host bash /stage/remote-install.sh /stage remove 2>&1)"
R="$(hx flotilla-e2e-host sh -c 'ls -d /usr/local/libexec/flotilla-host /etc/flotilla-host /var/db/flotilla-host /etc/sudoers.d/flotilla-host /var/log/flotilla-host/audit.jsonl 2>/dev/null' | tr '\n' ' ')"
echo "$OUT" | grep >/dev/null "^REMOVE PASS" && [ "$R" = "/var/log/flotilla-host/audit.jsonl " ] \
  && ok "remove: binary, config, state and sudoers gone; the audit log stays" || bad "remove: $OUT / left: $R"
$D rm -f flotilla-e2e-host >/dev/null

fi
if want shadows; then
echo "== shadow versions (v2 step 2): a shadow runs beside the live version on its own worker, its decisions are paired, and it can do no harm"
# shadows are off until now; turn them on in flotillad's config and restart
printf 'shadows: {enabled: true}\n' >> "$WORK/config/flotillad.yaml"   # appended in place: the bind mount keeps its file
SHCHK="$($D run --rm --read-only --tmpfs /tmp --user 65532:65532 -v "$WORK/config/flotillad.yaml:/etc/flotilla/flotillad.yaml:ro" -v "$WORK/config/callers.yaml:/etc/flotilla/callers.yaml:ro" \
  -v flotilla-e2e-flotillad-secrets:/run/flotilla:ro -v "$WORK/config/specs:/etc/flotilla/specs:ro" -v "$WORK/config/registry-e2e.json:/etc/flotilla/registry-e2e.json:ro" \
  "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml -check 2>&1; echo "rc=$?")"
echo "$SHCHK" | grep >/dev/null "^shadows: on " && echo "$SHCHK" | grep >/dev/null "shadows: e2e/versioned v2,v3,v4,v5,v6 on worker triage-shadow" && echo "$SHCHK" | grep >/dev/null "^rc=0" \
  && ok "-check with shadows on names each routine's shadow versions and their worker (rc 0)" || bad "-check with shadows: $(echo "$SHCHK" | grep -E 'shadows|rc=' | head -5)"
$D restart flotillad >/dev/null
for i in $(seq 1 30); do cli health >/dev/null 2>&1 && break; sleep 2; done
for i in $(seq 1 20); do cli status 2>&1 | grep >/dev/null 'worker triage-shadow: .*"ok":true' && break; sleep 2; done
cli status 2>&1 | grep >/dev/null 'worker triage-shadow: .*"ok":true' && ok "flotillad reaches the shadow worker" || bad "status: $(cli status 2>&1 | grep worker)"
for i in $(seq 1 20); do [ "$($D inspect flotilla-worker-triage-shadow --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ] && break; sleep 2; done
[ "$($D inspect flotilla-worker-triage-shadow --format '{{.State.Health.Status}}' 2>&1)" = healthy ] && ok "the shadow worker is healthy" || bad "shadow worker health: $($D logs flotilla-worker-triage-shadow 2>&1 | tail -3)"
NW="$($D inspect flotilla-worker-triage-shadow --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
[ "$NW" = "${P}_shadow " ] && ok "the shadow worker is on its own internal network only, not the live one" || bad "shadow worker networks: '$NW'"
NW="$($D inspect flotilla-egress-shadow --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
echo "$NW" | grep >/dev/null "${P}_shadow " && ! echo "$NW" | grep >/dev/null "${P}_flotilla " && ! echo "$NW" | grep >/dev/null "e2e-langfuse" && ok "the shadow worker has its own egress proxy (flotilla-egress-shadow), on the networks the live proxy reaches except Langfuse" || bad "shadow proxy networks: '$NW'"
MEMS="$($D inspect flotilla-worker-triage-shadow flotilla-worker-triage --format '{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}} {{.HostConfig.CpuShares}}' 2>&1 | tr '\n' ' ')"
echo "$MEMS" | python3 -c '
import sys
s = sys.stdin.read().split()
m1, c1, s1, m2, c2, s2 = map(int, s)
sys.exit(0 if 0 < m1 < m2 and 0 < c1 < c2 and 0 < s1 < 1024 else 1)' \
  && ok "the shadow worker has less memory and CPU than the live worker and a low CPU weight ($MEMS)" || bad "shadow worker limits: $MEMS"
SHM="$($D exec flotilla-worker-triage-shadow sh -c 'ls /run/flotilla | tr "\n" " "; stat -c "%a %u" /run/flotilla/worker.token /run/flotilla/read.env | tr "\n" " "; env | grep -c "^FLOTILLA_ENV_FILES=\|^FLOTILLA_WRITE_ENV_FILES=\|^GITHUB_TOKEN=\|^OPENROUTER_API_KEY=" ; touch /snapshots/x 2>&1 | head -1' 2>&1)"
echo "$SHM" | grep >/dev/null "^read.env worker.token 600 10001 600 10001 0" && echo "$SHM" | grep >/dev/null "Read-only file system" \
  && ok "the shadow worker holds its own token and the read environment only (no fleet.env), names no write file, and cannot write the snapshots" || bad "shadow worker secrets: $SHM"
LIVEM="$($D inspect flotilla-worker-triage-shadow --format '{{range .Mounts}}{{.Name}} {{end}}' 2>&1)"
echo "$LIVEM" | grep >/dev/null "flotilla-e2e-worker-triage-secrets" && bad "the shadow worker mounts the live worker's secrets volume: $LIVEM" || ok "the shadow worker mounts none of the live worker's volumes but the snapshots (read-only): $LIVEM"
# the run: every call counted at the live side first, so a change can be seen
OPT="$(cat "$WORK/config/cli.token")"
live_view() {  # a fingerprint of everything on the live side a shadow could touch: the ledgers' counts, the cases, the live runs, the plans
  { for ns in triage/cycle e2e/hostile e2e/shadow-monitor bugfix/fix; do
      $D exec -i -e NO_PROXY=flotillad -e HTTP_PROXY= -e HTTPS_PROXY= flotilla-worker-triage python3 -c '
import json, sys, urllib.request
ns, tok = sys.argv[1], sys.stdin.read().strip()
r = urllib.request.Request("http://flotillad:8485/v1/ledger/counts?ns=" + ns, headers={"Authorization": "Bearer " + tok})
print(ns, json.load(urllib.request.urlopen(r, timeout=10)))' "$ns" <<<"$OPT" 2>&1
    done
    cli case list --all 2>&1 | awk '{print $1, $2, $3}'
    cli action list 2>&1
  } | sha256sum | cut -c1-16
}
echo "-- the hostile version"
cli versions e2e/hostile -json | python3 -c 'import json,sys; v=json.load(sys.stdin); sys.exit(0 if [(x["version"], x["state"]) for x in v["versions"]] == [(1, "live"), (2, "shadow")] else 1)' \
  && ok "e2e/hostile: v1 live, v2 a shadow that tries everything" || bad "hostile versions: $(cli versions e2e/hostile 2>&1)"
HID="$(cli runs e2e/hostile -json -n 1 | python3 -c 'import json,sys; r=json.load(sys.stdin)["runs"] or []; print(r[0]["id"] if r and r[0]["status"] == "ok" else "")')"
[ -n "$HID" ] && ok "e2e/hostile has the live run made at the start, before shadows were on ($HID): an input and no shadow run" || bad "hostile has no live run: $(cli runs e2e/hostile 2>&1 | head -3)"
store_dump() {  # a consistent copy of flotillad's store (the deploy's own snapshot tool), dumped by deploy/e2e/storediff.py: what a shadow must leave alone
  $D run --rm --network none --user 65532:65532 -e STORE_DIR=/v -v flotilla-e2e-state:/v -v "$ROOT/deploy/server/store_snapshot.py:/s.py:ro" \
    "local/flotilla-worker-triage:$TAG" python3 /s.py backup > "$WORK/store.db" 2> "$WORK/store.err" && python3 "$ROOT/deploy/e2e/storediff.py" "$WORK/store.db" e2e/hostile
}
vol_hash() {  # every file of the live triage worker's state volume
  $D run --rm --network none --user 10001:10001 -v flotilla-e2e-triage-state:/v:ro --entrypoint sh "local/flotilla-worker-triage:$TAG" \
    -c 'cd /v && find . -type f | sort | xargs -r sha256sum' | sha256sum | cut -c1-16
}
LV0="$(live_view)"; SD0="$(store_dump)"; VH0="$(vol_hash)"
echo "$SD0" | grep >/dev/null "^SHADOW 0 0$" && [ "$(echo "$SD0" | sed -n 's/^ROWS //p')" -ge 5 ] \
  && ok "before: the live rows of e2e/hostile are dumped ($(echo "$SD0" | sed -n 's/^ROWS //p') rows over every store table) and no shadow row exists" || bad "store dump before: $(echo "$SD0" | tail -3) $(cat "$WORK/store.err" 2>/dev/null | head -2)"
OUT="$(cli versions shadow-run e2e/hostile v2 --input "$HID" 2>&1)"
echo "$OUT" | grep >/dev/null '"status":"\(queued\|running\|done\)"' && ok "the operator's shadow-run on the live input ($HID) queued v2's run" || bad "shadow-run: $OUT"
OUT2="$(cli versions shadow-run e2e/hostile v2 --input "$HID" 2>&1)"
echo "$OUT2" | grep >/dev/null "HTTP 409.*already has a shadow run" && ok "a second shadow-run of the same version on the input is refused (409)" || bad "second shadow-run: $OUT2"
OUT3="$(agent versions shadow-run e2e/hostile v2 --input "$HID" 2>&1)"
echo "$OUT3" | grep >/dev/null "HTTP 403" && ok "an agent cannot ask for a shadow run (403)" || bad "agent shadow-run: $OUT3"
for i in $(seq 1 60); do $D logs flotilla-worker-triage-shadow 2>&1 | grep -F "HOSTILE END" >/dev/null && break; sleep 2; done
for i in $(seq 1 30); do $D logs flotillad 2>&1 | grep '"msg":"shadow run end"' | grep -q '"routine":"e2e/hostile@v2"' && break; sleep 1; done
HL="$($D logs flotilla-worker-triage-shadow 2>&1 | grep -o 'HOSTILE [^"]*' | sed 's/\\n.*//')"
echo "$HL" | grep >/dev/null "^HOSTILE END PASS" && ! echo "$HL" | grep >/dev/null "BROKEN" \
  && ok "the hostile shadow tried a live secret, git and gh config, the live state, the snapshots, its worker token, its run token on the live namespace, a plan and an off-list host: every one held ($(echo "$HL" | grep -c ' held ') held)" \
  || bad "hostile shadow: $HL"
echo "$HL" | grep >/dev/null "HOSTILE allowlisted-host reached through http://egress-shadow:4750" \
  && ok "its allowed request went out through its own proxy" || bad "the shadow's proxy: $(echo "$HL" | grep allowlisted)"
$D logs flotilla-egress-shadow 2>&1 | grep >/dev/null "litellm" && ok "that request is in flotilla-egress-shadow's log, not mixed into the live proxy's" || bad "the shadow proxy's log: $($D logs flotilla-egress-shadow 2>&1 | tail -3)"
$D logs flotilla-egress-shadow 2>&1 | grep >/dev/null "forbidden" && ok "the host off the allowlist was refused by the shadow's proxy and logged there" || bad "the refusal in the shadow proxy's log"
LV1="$(live_view)"; SD1="$(store_dump)"; VH1="$(vol_hash)"
[ "$LV0" = "$LV1" ] && ok "after the hostile shadow nothing on the live side changed: the ledgers' counts, the cases, the actions' levels ($LV1)" || bad "the live side changed under a hostile shadow: $LV0 -> $LV1"
DIFF="$(diff <(echo "$SD0" | grep -v '^SHADOW') <(echo "$SD1" | grep -v '^SHADOW'))"
[ -z "$DIFF" ] && ok "every live store table is byte-identical before and after the shadow run (rows not keyed @v, hashed per table: $(echo "$SD1" | sed -n 's/^ROWS //p') rows)" || bad "the store changed under a shadow: $DIFF"
echo "$SD1" | grep -E '^SHADOW [1-9][0-9]* [1-9]' >/dev/null && ok "and the shadow did leave its own rows ($(echo "$SD1" | grep '^SHADOW' | sed 's/SHADOW //') shadow-table rows, shadow runs), so the equality is not the shadow doing nothing" || bad "the shadow left no rows of its own: $(echo "$SD1" | grep '^SHADOW')"
[ "$VH0" = "$VH1" ] && ok "the live worker's state volume has the same files and bytes before and after ($VH1)" || bad "the live state volume changed: $VH0 -> $VH1"
LVD="$(cli runs e2e/hostile -json -n 100 | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["runs"] or []))')"
[ "$LVD" = 1 ] && ok "the routine has the one live run (v1's); the shadow's run is not in its runs" || bad "hostile live runs: $LVD"
cli runs e2e/hostile -json -n 100 | python3 -c 'import json,sys; r=json.load(sys.stdin)["runs"]; sys.exit(0 if all(not x.get("shadow") and x["version"] == 1 for x in r) else 1)' \
  && ok "the runs list holds no shadow run" || bad "the runs list shows a shadow: $(cli runs e2e/hostile -json -n 5 | head -c 300)"
cli pairs e2e/hostile v2 --json | python3 -c 'import json,sys; p=json.load(sys.stdin); sys.exit(0 if p["version"] == 2 and p["live_version"] == 1 else 1)' \
  && ok "flotilla pairs answers for the hostile version (no subject: nothing decided to pair)" || bad "pairs: $(cli pairs e2e/hostile v2 2>&1 | head -3)"
echo "-- the model stand-in's record: every shadow call is tagged, no live call is"
CM="$($D logs e2e-litellm 2>&1 | grep 'CHAT_META')"
V2L="$(echo "$CM" | grep "E2E-CALL-V2")"; V1L="$(echo "$CM" | grep "E2E-CALL-V1")"
[ -n "$V2L" ] && [ "$(echo "$V2L" | grep -vc 'tags=shadow,routine_version:2')" = 0 ] \
  && ok "LLM calls: every call the shadow made ($(echo "$V2L" | wc -l)) carries the tags shadow and routine_version:2" || bad "LLM calls: shadow call without its tags: $(echo "$V2L" | head -2)"
[ -n "$V1L" ] && [ "$(echo "$V1L" | grep -vc 'tags=-')" = 0 ] \
  && ok "LLM calls: the live version's call ($(echo "$V1L" | wc -l)) carries no shadow tag" || bad "LLM calls: live call missing or tagged: $(echo "$V1L" | head -2)"
echo "-- the outward stand-ins' record: no write reached them from a shadow"
SHIP="$($D inspect flotilla-egress-shadow --format '{{(index .NetworkSettings.Networks "e2e-backend").IPAddress}}' 2>&1)"
WRL="$($D logs e2e-world 2>&1 | grep 'world WRITE')"
SHW="$(echo "$WRL" | grep "client=$SHIP ")"
[ -n "$SHIP" ] && [ "$(echo "$SHW" | grep -c api.github.com)" = 0 ] && ok "GitHub's stand-in recorded no request from the shadow's proxy ($SHIP): its allowlist has no route to the write host" || bad "a shadow reached GitHub: $(echo "$SHW" | grep api.github.com | head -2)"
$D logs flotilla-egress-shadow 2>&1 | grep >/dev/null "api.github.com" && ok "the shadow's proxy log holds the refused attempt on api.github.com (the hostile version did try)" || bad "no refused GitHub attempt in the shadow proxy's log"
echo "$SHW" | grep >/dev/null "grafana.example.com.*auth=no" && [ "$(echo "$SHW" | grep -c 'auth=yes')" = 0 ] \
  && ok "Grafana's stand-in recorded the shadow's write attempt with no credential, and no write from the shadow carried one ($(echo "$SHW" | wc -l) attempts)" || bad "writes from the shadow: $SHW"
echo "(the live side's own writes at the stand-ins, for the record: $(echo "$WRL" | grep -vc "client=$SHIP ") from the live proxy)"

echo "-- wall time: a live run beside a running shadow, against the same run with none"
# e2e/cpu starts held (a held routine does not run at all): freed, each live run has a shadow doing the same work beside it, on the shadow
# worker (less CPU, low weight); then v2 is retired, so the same live runs have none, and the two sets of three are compared.
cpu_runs() { cli runs e2e/cpu -json -n 20 | python3 -c 'import json,sys; r=json.load(sys.stdin)["runs"] or []; print(len(r), r[0]["status"] if r else "-")'; }
run_cpu() {  # one run, waited for: a second `run` while one is in flight is refused, and two would be measured together
  local n0; n0="$(cpu_runs | cut -d' ' -f1)"
  cli run e2e/cpu >/dev/null 2>&1
  for _ in $(seq 1 60); do set -- $(cpu_runs); [ "$1" -gt "$n0" ] && [ "$2" != running ] && return 0; sleep 1; done
  return 1
}
free_routine e2e/cpu
for i in $(seq 1 30); do cpu_runs | grep >/dev/null " ok$" && break; sleep 2; done   # the run at the restart
for i in 1 2; do run_cpu; done
SHDONE=0
for i in $(seq 1 60); do SHDONE="$($D logs flotillad 2>&1 | grep -c '"msg":"shadow run end".*"routine":"e2e/cpu@v2"')"; [ "$SHDONE" -ge 3 ] && break; sleep 2; done
[ "$SHDONE" -ge 3 ] && ok "the shadow did its work too ($SHDONE shadow runs of e2e/cpu@v2 ended), so the live runs were measured beside a real load" || bad "shadow runs of e2e/cpu: $SHDONE"
cli retire e2e/cpu v2 --reason "e2e: the baseline runs have no shadow" >/dev/null 2>&1
for i in 1 2 3; do run_cpu; done
WT="$(cli runs e2e/cpu -json -n 20 | WALL_RATIO="${WALL_RATIO:-1.5}" WALL_SECS="${WALL_SECS:-0}" python3 -c '
import json, os, statistics, sys
r = [x for x in json.load(sys.stdin)["runs"] if not x.get("shadow")]
ok = [x for x in r if x["status"] == "ok"]   # newest first: the three without a shadow, then the three with one
if len(ok) != 6 or len(r) != 6:
    print("runs", len(r), "ok", len(ok), [x["status"] for x in r]); sys.exit(1)
base, with_ = [x["duration_s"] for x in ok[:3]], [x["duration_s"] for x in ok[3:]]
mb, mw = statistics.median(base), statistics.median(with_)
print("base", " ".join("%.2f" % d for d in base), "with", " ".join("%.2f" % d for d in with_), "medians %.2f %.2f" % (mb, mw))
sys.exit(0 if mw <= max(mb * float(os.environ["WALL_RATIO"]), mb + float(os.environ["WALL_SECS"])) else 1)' 2>&1)"; WTRC=$?
echo "MEASURED wall time: $WT"
[ "$WTRC" = 0 ] && echo "$WT" | grep >/dev/null "medians" && ok "the live run's wall time with a shadow beside it is within the tolerance of the same run without one, and every live run exited ok ($WT)" || bad "wall time: $WT"

echo "-- a monitor's shadow: paired and unpaired detections"
free_routine e2e/shadow-monitor   # its first run is at the restart; the live v1 opens a case, v2 is shadowed on the same input
for i in $(seq 1 30); do cli runs e2e/shadow-monitor 2>&1 | grep >/dev/null " ok " && break; sleep 2; done
PJ=""
for i in $(seq 1 45); do
  PJ="$(cli pairs e2e/shadow-monitor v2 --json 2>/dev/null)"
  echo "$PJ" | python3 -c 'import json,sys; p=json.load(sys.stdin); sys.exit(0 if p["paired"] + p["unpaired"] >= 2 else 1)' 2>/dev/null && break; sleep 2
done
echo "$PJ" | python3 -c '
import json, sys
p = json.load(sys.stdin)
subj = {x["subject"]: x for x in p["pairs"]}
a, b = [v for k, v in subj.items() if "e2e-shadowed" in k], [v for k, v in subj.items() if "e2e-other" in k]
c = [v for k, v in subj.items() if "e2e-host" in k]   # the case left open by the versions section: the live monitor does not report it, and the shadow says it would clear it
sys.exit(0 if p["paired"] == 1 and p["unpaired"] == 2 and a and a[0]["live"] and a[0]["shadow"] and a[0]["same"] and b and not b[0].get("live") and b[0]["shadow"] and c and not c[0].get("live") and c[0]["shadow"] else 1)' \
  && ok "v2 of the monitor: the detection both versions made is a pair and the same; the one only the shadow reported and the old case only the shadow would clear are unpaired, each on its side" || bad "monitor pairs: $(echo "$PJ" | python3 -c '
import json, sys
p = json.load(sys.stdin)
print({k: p[k] for k in ("paired", "differ", "unpaired")})
for x in p["pairs"]:
    print(x["subject"], "| live:", (x.get("live") or {}).get("text"), "| shadow:", (x.get("shadow") or {}).get("text"), "| same", x["same"])' 2>&1)"
cli pairs e2e/shadow-monitor v2 | grep >/dev/null "e2e/shadow-monitor · v2 shadow vs v1 live: 1 pairs" && ok "the CLI prints them, header with the counts" || bad "pairs table: $(cli pairs e2e/shadow-monitor v2 2>&1 | head -3)"
cli versions e2e/shadow-monitor -json | python3 -c 'import json,sys; v=json.load(sys.stdin); x=next(y for y in v["versions"] if y["version"] == 2); sys.exit(0 if x["pairs"] == 1 else 1)' \
  && ok "the versions view counts the pair (what a promote reads)" || bad "pairs count in versions: $(cli versions e2e/shadow-monitor 2>&1 | head -4)"
rm -rf "$WORK/pairs-export"
cli pairs export e2e/shadow-monitor v2 --to /var/lib/flotilla/pairs-export >/dev/null 2>&1 && $D cp flotillad:/var/lib/flotilla/pairs-export "$WORK/pairs-export" >/dev/null 2>&1 \
  && [ "$(stat -c %a "$WORK/pairs-export" "$WORK/pairs-export/pairs.json" "$WORK/pairs-export/README.txt" | tr '\n' ' ')" = "700 600 600 " ] && grep -q PRODUCTION "$WORK/pairs-export/README.txt" \
  && ok "pairs export wrote a folder (0700, files 0600) with a production-data README" || bad "pairs export: $(ls -la "$WORK/pairs-export" 2>&1 | head -4)"
# the monitor's live case (opened by the live v1) belongs to the acting routine: the shadows of e2e/versioned plan on it and send nothing
VCS=""
for i in $(seq 1 45); do VCS="$(cli case list --all 2>&1 | awk '$2 == "e2e_versioned" && $3 == "host:e2e-shadowed" && $4 != "detected" {print $1; exit}')"; [ -n "$VCS" ] && break; sleep 2; done
[ -n "$VCS" ] && ok "the live version took the monitor's case ($VCS)" || bad "the acting routine did not take the case: $(cli case list --all 2>&1 | grep e2e_versioned)"
SHP=""
for i in $(seq 1 45); do
  SHP="$(for v in 3 5 6; do cli pairs e2e/versioned v$v --json 2>/dev/null; echo; done)"
  echo "$SHP" | grep >/dev/null "would run host.vtrue (blocked: shadow)" && break; sleep 2
done
echo "$SHP" | grep >/dev/null "would run host.vtrue (blocked: shadow)" \
  && ok "a shadow of an acting routine recorded \"would run host.vtrue (blocked: shadow)\"" || bad "shadow plan: $(echo "$SHP" | head -c 400)"
SHOW="$(cli case show "$VCS" 2>&1)"
echo "$SHOW" | grep >/dev/null "@v" && bad "a shadow run is in the live case's timeline: $(echo "$SHOW" | grep '@v' | head -2)" \
  || ok "the live case's timeline holds no shadow run: the shadows' plans are not in it"
cli case close "$VCS" --reason "e2e: the shadow check is done" >/dev/null 2>&1
rm -rf "$WORK/pairs-export"

echo "-- every signal a shadow run emits is marked and separable: metrics, logs, traces"
VMLOG="$($D logs e2e-victoriametrics 2>&1)"
SHS="$(echo "$VMLOG" | grep 'SAMPLE' | grep '@v')"
[ -n "$SHS" ] && [ "$(echo "$SHS" | grep -vc 'shadow="1"')" = 0 ] \
  && ok "metrics: every sample with a shadow's routine key carries shadow=\"1\" ($(echo "$SHS" | wc -l) samples, none unmarked)" || bad "metrics: unmarked or no shadow samples: $(echo "$SHS" | grep -v 'shadow="1"' | head -2)"
[ -n "$SHS" ] && [ "$(echo "$SHS" | grep -cE 'last_success|freshness')" = 0 ] \
  && ok "metrics: a shadow emits no freshness or last-success series, so nothing it does can make a stale alert fire or clear" || bad "metrics: a shadow emitted freshness: $(echo "$SHS" | grep -E 'last_success|freshness' | head -2)"
[ "$(echo "$VMLOG" | grep 'SAMPLE' | grep -v '@v' | grep -c 'shadow="1"')" = 0 ] && ok "metrics: no live-keyed sample carries a shadow mark" || bad "metrics: a live sample is marked shadow"
echo "$VMLOG" | grep 'SAMPLE flotilla_run{' | grep 'routine="e2e/hostile"' | grep -c . | grep -qv '^0$' && ok "metrics: the live run of the same routine still pushed its own series under the live key" || bad "metrics: no live-key series for e2e/hostile"
FLOG="$($D logs flotillad 2>&1 | grep '"routine":"[^"]*@v')"
[ -n "$FLOG" ] && [ "$(echo "$FLOG" | grep -vc '"shadow":true')" = 0 ] \
  && ok "logs: every flotillad log line about a shadow's routine key has shadow=true ($(echo "$FLOG" | wc -l) lines, none unmarked)" || bad "logs: unmarked shadow line: $(echo "$FLOG" | grep -v '"shadow":true' | head -2 | cut -c1-300)"
[ "$(echo "$FLOG" | grep -c '"level":"ERROR"')" = 0 ] || echo "note: $(echo "$FLOG" | grep -c '"level":"ERROR"') shadow error lines (level stays truthful)"
TRLOG="$($D logs e2e-victoriatraces 2>&1)"
echo "$TRLOG" | grep >/dev/null "SHADOW_SPAN" && ! echo "$TRLOG" | grep >/dev/null "UNMARKED_SHADOW_SPAN" \
  && ok "traces: a shadow run's span reached VictoriaTraces carrying flotilla.shadow, and none arrived without it" || bad "traces: $(echo "$TRLOG" | grep -c SHADOW_SPAN) marked, $(echo "$TRLOG" | grep -c UNMARKED_SHADOW_SPAN) unmarked"

fi
if want stations; then
echo "== stations (v2 steps 3 and 4): a station version's texts, recorded per call, shadowed, marked, and run offline"
# e2e/station-monitor: v1 live and v2 a shadow, each with one station on harness flotilla that renders a system text and a task text and calls the model
# through the client. v2's system text differs and its task text is the same, so the pair has one text equal and one differing.
free_routine e2e/station-monitor
for i in $(seq 1 30); do cli runs e2e/station-monitor 2>&1 | grep >/dev/null " ok " && break; sleep 2; done
cli runs e2e/station-monitor 2>&1 | grep >/dev/null " ok " && ok "the live run of e2e/station-monitor v1 ended ok (FLOTILLA_STATIONS read, both texts rendered and verified, the model called)" || bad "station-monitor live run: $(cli runs e2e/station-monitor 2>&1 | head -3) $($D logs flotilla-worker-triage 2>&1 | grep -E 'STATION|Traceback|StationsError' | tail -3)"
PJS=""
for i in $(seq 1 45); do
  PJS="$(cli pairs e2e/station-monitor v2 --json 2>/dev/null)"
  echo "$PJS" | python3 -c 'import json,sys; p=json.load(sys.stdin); sys.exit(0 if p["paired"] >= 1 else 1)' 2>/dev/null && break; sleep 2
done
echo "$PJS" | python3 -c '
import json, sys
p = json.load(sys.stdin)
pair = next((x for x in p["pairs"] if "e2e-stations" in x["subject"]), None)
def texts(side):
    return {t["name"]: t for s in (side or {}).get("stations") or [] if s["role"] == "decide" for t in s.get("texts") or []}
if not pair or not pair.get("live") or not pair.get("shadow"):
    sys.exit(1)
lt, sh = texts(pair["live"]), texts(pair["shadow"])
good = (set(lt) == {"system", "task"} == set(sh) and lt["task"]["sha256"] == sh["task"]["sha256"] and lt["system"]["sha256"] != sh["system"]["sha256"]
        and "live version (v1)" in lt["system"]["text"] and "shadow version (v2)" in sh["system"]["text"] and "host:e2e-stations" in lt["task"]["text"]
        and "host:e2e-stations" in sh["task"]["text"] and pair["live"]["stations"][0]["role"] == "decide" == pair["shadow"]["stations"][0]["role"])
sys.exit(0 if good else 1)' \
  && ok "the pair carries each side's station (decide) and its two rendered texts: the task text equal, the system text differing, each body present and the slot filled" \
  || bad "station pair: $(echo "$PJS" | head -c 600)"
# the store: the shadow's station rows are under its own key, none under the live key, and each text is kept once by its hash
store_dump >/dev/null 2>&1
cat > "$WORK/stationq.py" <<'PY'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
shadow = db.execute("SELECT COUNT(*) FROM run_calls WHERE routine = 'e2e/station-monitor@v2' AND station = 'decide' AND texts <> ''").fetchone()[0]
live = db.execute("SELECT COUNT(*) FROM run_calls WHERE routine = 'e2e/station-monitor' AND station = 'decide' AND texts <> ''").fetchone()[0]
leaked = db.execute("SELECT COUNT(*) FROM run_calls c JOIN runs r ON r.id = c.run_id WHERE r.shadow = 1 AND instr(c.routine, '@v') = 0").fetchone()[0]
texts = db.execute("SELECT COUNT(DISTINCT json_extract(j.value, '$.sha256')) FROM run_calls c, json_each(c.texts) j WHERE c.routine LIKE 'e2e/station-monitor%' AND c.texts <> ''").fetchone()[0]
print("shadow", shadow, "live", live, "leaked", leaked, "texts", texts)
sys.exit(0 if shadow >= 1 and live >= 1 and leaked == 0 and texts == 3 else 1)
PY
SQ="$(python3 "$WORK/stationq.py" "$WORK/store.db" 2>&1)"; SQRC=$?
[ "$SQRC" = 0 ] && ok "store: the shadow's station calls are keyed e2e/station-monitor@v2 with their texts' hashes, no call of a shadow run is under a live key, and the three distinct texts are kept once each ($SQ)" || bad "store rows of the station routine: $SQ"
rm -f "$WORK/stationq.py"
# no effect: the live case list holds the one case the live version opened; the shadow's run is in no runs list
[ "$(cli case list --all 2>&1 | awk '$2 == "e2e_stations"' | wc -l)" = 1 ] && ok "the shadow opened no case: one e2e_stations case, the live version's" || bad "e2e_stations cases: $(cli case list --all 2>&1 | grep e2e_stations)"
cli runs e2e/station-monitor -json -n 50 | python3 -c 'import json,sys; r=json.load(sys.stdin)["runs"] or []; sys.exit(0 if r and all(not x.get("shadow") and x["version"] == 1 for x in r) else 1)' \
  && ok "the routine's runs list holds only the live version's run" || bad "station-monitor runs: $(cli runs e2e/station-monitor -json -n 5 | head -c 300)"
# signals: the station's model span carries flotilla.station, and the shadow's carries the shadow mark as well
for i in $(seq 1 15); do $D logs e2e-victoriatraces 2>&1 | grep >/dev/null "STATION_SHADOW_SPAN" && break; sleep 2; done
STR="$($D logs e2e-victoriatraces 2>&1)"
echo "$STR" | grep >/dev/null "STATION_LIVE_SPAN" && echo "$STR" | grep >/dev/null "STATION_SHADOW_SPAN" && ! echo "$STR" | grep >/dev/null "UNMARKED_STATION_SHADOW_SPAN" \
  && ok "traces: the live station's model span and the shadow's reached the trace store with flotilla.station, and none of the shadow's lacked flotilla.shadow" \
  || bad "station spans: $(echo "$STR" | grep -c STATION_LIVE_SPAN) live, $(echo "$STR" | grep -c STATION_SHADOW_SPAN) shadow, $(echo "$STR" | grep -c UNMARKED_STATION_SHADOW_SPAN) unmarked shadow"
cli case close "$(cli case list --all 2>&1 | awk '$2 == "e2e_stations" {print $1; exit}')" --reason "e2e: the stations check is done" >/dev/null 2>&1
echo "-- the offline run of the same version, on an authored input (flotillad -stations, then runtime/offline_run.py in the worker image)"
OFF="$WORK/offline"
off_clean() {  # the offline runs' folders hold files owned by the worker's uid (and 0700 homes): removed by that uid, then the folder by us
  [ -d "$OFF" ] && $D run --rm --user 10001:10001 -v "$OFF:/x" --entrypoint sh "local/flotilla-worker-triage:$TAG" -c 'rm -rf /x/out /x/out-known /x/out-unknown /x/out-hb' >/dev/null 2>&1
  rm -rf "$OFF"
}
off_clean; mkdir -p "$OFF"; chmod 777 "$OFF"
printf 'e2e-eval-key' > "$OFF/eval.key"
cat > "$OFF/models.json" <<EOF
{"endpoints": {"gw": {"base_url": "http://litellm:8080/v1", "kind": "litellm", "key_file": "/work/eval.key", "defaults": {"tools": "native", "json": "object", "cost": "reported"}}},
 "aliases": {"triage-flash": {"endpoint": "gw", "model": "fleet-triage-flash"}}}
EOF
cat > "$OFF/input.json" <<EOF
{"schema_version": 1, "input_key": "e2e-offline-1", "routine": "e2e/station-monitor", "kind": "tick", "version": 1}
EOF
$D run --rm --network none --user 65532:65532 -v "$OFF:/work" -v "$WORK/config/specs:/etc/flotilla/specs:ro" "local/flotilla-flotillad:$TAG" \
  -stations /etc/flotilla/specs/e2e/station-monitor/v1 -models /work/models.json > "$OFF/contract.json" 2> "$OFF/contract.err"
python3 -c '
import json, sys
c = json.load(open(sys.argv[1]))
e = c["env"]
st = json.loads(e["FLOTILLA_STATIONS"])
sys.exit(0 if c["routine"] == "e2e/station-monitor" and "decide" in st["stations"] and e["FLOTILLA_SHADOW"] == "1" and "triage-flash" in e["FLOTILLA_MODELS"] and not any("TOKEN" in k or "API_URL" in k for k in e) else 1)' "$OFF/contract.json" \
  && ok "flotillad -stations (no config, no network) printed the contract: the version's command, FLOTILLA_STATIONS, the models cut to its alias, no address or token" || bad "-stations: $(head -c 300 "$OFF/contract.json") $(cat "$OFF/contract.err")"
$D run --rm --network e2e-litellm --user 10001:10001 -v "$OFF:/work" -v "$WORK/config/specs/e2e:/opt/flotilla/fleets/e2e:ro" "local/flotilla-worker-triage:$TAG" \
  python3 /opt/flotilla/runtime/offline_run.py --contract /work/contract.json --input /work/input.json --models /work/models.json --out /work/out --forbid /opt/flotilla/fleets > "$OFF/offline.out" 2>&1; OFFRC=$?
python3 -c '
import json, sys
r = json.load(open(sys.argv[1] + "/result.json"))
calls = [json.loads(x) for x in open(sys.argv[1] + "/calls.jsonl")]
sys.exit(0 if r["exit_code"] == 0 and any(c.get("station") == "decide" and [t["name"] for t in c.get("texts") or []] == ["system", "task"] for c in calls) else 1)' "$OFF/out" \
  && [ "$OFFRC" = 0 ] && ok "the offline run of v1 on an authored input: its own command ran in the worker image, rendered both texts, called the model on the eval's own key and wrote its calls with the station and texts" || bad "offline run rc=$OFFRC: $(tail -c 400 "$OFF/offline.out") out: $(tail -c 300 "$OFF/out/output.txt" 2>/dev/null) calls: $(head -c 700 "$OFF/out/calls.jsonl" 2>/dev/null)"
$D logs e2e-litellm 2>&1 | grep "CHAT auth=bearer model=fleet-triage-flash" | grep >/dev/null "e2e-eval-key" && ok "the model stand-in saw the eval's own key, not a live or shadow key" || echo "(the stand-in does not print the key it was given; checked by the contract's FLOTILLA_MODELS instead)"
# recorded tool results: a station of the in-process harness answers its probe from the input; a probe with no recorded result fails the run and is never sent live
cat > "$OFF/probe.py" <<'PY'
import os, sys
sys.path.insert(0, "/opt/flotilla/runtime")
import harness
cfg = {"tools": ["probe"], "limits": {"max_tool_calls": 1, "max_steps": 2},
       "quirks": {"bad_json_uses_turn": True, "empty_retry": True, "final_decision_call": True, "reply_echo_chars": 9, "decision_echo_chars": 9, "result_chars": 99}}
fp = sys.argv[1]
replies = iter(['{"action":"probe","probe":"probe","probe_args":{"fp":"%s"}}' % fp, '{"action":"decide","disposition":"file"}'])
live = lambda name, args: sys.exit(77)   # the live tool: reaching it ends the run with 77
r = harness.run_json_step([], lambda m, s: (next(replies), {}), live, cfg, {"fatal": (OSError,), "defer": RuntimeError, "defer_prefix": ""}, station="decide", log=lambda *a: None)
print("RESULT", r.trace[0]["result"])
PY
cat > "$OFF/rec-input.json" <<EOF
{"schema_version": 1, "input_key": "e2e-offline-2", "routine": "e2e/station-monitor", "kind": "tick", "recorded_tool_results": {"decide": [{"tool": "probe", "args": {"fp": "known"}, "result": "the recorded answer"}]}}
EOF
for fp in known unknown; do
  printf '{"routine": "e2e/station-monitor", "version": 1, "workdir": ".", "timeout": "1m", "command": "python3 /work/probe.py %s", "env": {}}' "$fp" > "$OFF/contract-$fp.json"
  $D run --rm --network none --user 10001:10001 -v "$OFF:/work" "local/flotilla-worker-triage:$TAG" python3 /opt/flotilla/runtime/offline_run.py \
    --contract /work/contract-$fp.json --input /work/rec-input.json --models /work/models.json --out /work/out-$fp --forbid /opt/flotilla/fleets > "$OFF/rec-$fp.out" 2>&1
done
grep >/dev/null "RESULT the recorded answer" "$OFF/out-known/output.txt" && ok "a probe with a recorded result is answered from the input, the live tool not reached" || bad "recorded probe: $(cat "$OFF/out-known/output.txt" 2>&1 | tail -3)"
python3 -c '
import json, sys
r = json.load(open(sys.argv[1] + "/result.json"))
out = open(sys.argv[1] + "/output.txt").read()
sys.exit(0 if r["exit_code"] not in (0, 77) and "RecordedMiss" in out else 1)' "$OFF/out-unknown" \
  && ok "a probe with no recorded result fails the run (RecordedMiss) and the live tool is never reached" || bad "unrecorded probe: $(cat "$OFF/out-unknown/result.json" 2>&1) $(tail -3 "$OFF/out-unknown/output.txt" 2>&1)"
$D run --rm --network none --user 10001:10001 -v "$OFF:/work" "local/flotilla-worker-triage:$TAG" python3 /opt/flotilla/runtime/offline_run.py \
  --contract /work/contract-known.json --input /work/rec-input.json --models /work/models.json --out /opt/flotilla/fleets/e2e/out --forbid /opt/flotilla/fleets > "$OFF/forbid.out" 2>&1; FRC=$?
[ "$FRC" = 2 ] && grep >/dev/null "offline_run: --out" "$OFF/forbid.out" && ok "--out inside the fleets tree is refused" || bad "--out in the fleets tree: rc=$FRC $(cat "$OFF/forbid.out" | tail -2)"
# A shipped example's version, offline, on an authored input (design section 7, step 5): platform/heartbeat v2, whose one model call is the probe
# station (the example fleets' own folder, as the image carries it). triage/cycle v1 cannot be run here: offline_run hands a run the contract's
# FLOTILLA_* variables only, and triage reads its sources from SF_* settings a worker supplies. The heartbeat's two URLs are such settings; they
# are put in front of its own command, as a scenario's author would, and name the model host (allowed) and the stack's refused host (denied).
cat > "$OFF/input-hb.json" <<EOF2
{"schema_version": 1, "input_key": "e2e-offline-hb", "routine": "platform/heartbeat", "kind": "tick", "version": 2}
EOF2
$D run --rm --network none --user 65532:65532 -v "$OFF:/work" -v "$WORK/config/specs:/etc/flotilla/specs:ro" "local/flotilla-flotillad:$TAG" \
  -stations /etc/flotilla/specs/platform/heartbeat/v2 -models /work/models.json > "$OFF/contract-hb.json" 2> "$OFF/contract-hb.err"
cat > "$OFF/hb_contract.py" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["command"] = "env HEARTBEAT_ALLOWED_URL=http://litellm:8080/health/liveliness HEARTBEAT_DENIED_URL=http://forbidden:8080/ " + c["command"]
json.dump(c, open(sys.argv[1], "w"))
PY
python3 "$OFF/hb_contract.py" "$OFF/contract-hb.json"
$D run --rm --network e2e-litellm --user 10001:10001 -v "$OFF:/work" "local/flotilla-worker-triage:$TAG" \
  python3 /opt/flotilla/runtime/offline_run.py --contract /work/contract-hb.json --input /work/input-hb.json --models /work/models.json --out /work/out-hb --forbid /opt/flotilla/fleets > "$OFF/offline-hb.out" 2>&1; HBRC=$?
python3 -c '
import json, sys
r = json.load(open(sys.argv[1] + "/result.json"))
c = json.load(open(sys.argv[2]))
calls = [json.loads(x) for x in open(sys.argv[1] + "/calls.jsonl")]
refused = [json.loads(x) for x in open(sys.argv[1] + "/refused.jsonl")]
out = open(sys.argv[1] + "/output.txt").read()
sys.exit(0 if r["exit_code"] == 0 and "HEARTBEAT PASS" in out and c["routine"] == "platform/heartbeat" and c["version"] == 2
         and any(x.get("station") == "probe" and [t["name"] for t in x.get("texts") or []] == ["task"] for x in calls)
         and any("forbidden" in x["target"] for x in refused) and not any("TOKEN" in k or "API_URL" in k for k in c["env"]) else 1)' "$OFF/out-hb" "$OFF/contract-hb.json" \
  && [ "$HBRC" = 0 ] && ok "the example platform/heartbeat v2, offline on an authored input: its own command ran in the worker image, the probe station's call was made on the eval's own key and recorded with its text, the host off the list was refused, and the contract carries no flotillad address or token" \
  || bad "offline run of heartbeat v2 rc=$HBRC: $(tail -c 300 "$OFF/offline-hb.out") $(tail -c 300 "$OFF/out-hb/output.txt" 2>/dev/null)"
off_clean

fi
if want reads && [ "${DEMO:-}" != 1 ]; then
echo "== live reads (RFC-0014 section 4.8): the reads worker reads through its own proxy, the reads are recorded, a replay is served the record, a shadow too"
# e2e/reads (deploy/e2e/fleets/e2e/reads): v1 is live on the reads worker, whose station declares reads from the stub MCP server (deploy/e2e/mcp_stub.py:
# n8n's stand-in, on its own internal network); v2 and v3 are shadows on the triage shadow worker, which has no route to that server and no address
# or token for it. A shadow waits for its live run and is served its record: v2 asks for the same reads, v3 for one the live run never made.
grep >/dev/null '^shadows:' "$WORK/config/flotillad.yaml" || printf 'shadows: {enabled: true}\n' >> "$WORK/config/flotillad.yaml"   # appended in place: the bind mount keeps its file
mcplog() { $D logs e2e-mcp 2>&1; }
RC="python3 $ROOT/deploy/e2e/reads_check.py"
# the reads worker alone holds the server's address and token; the shadow worker and the live triage worker hold none (names only: no value is read)
RW_ENV="$($D exec flotilla-worker-reads env 2>&1 | grep -c '^ROUTINE_MCP_LAB_\(URL\|TOKEN_FILE\)=')"
OTHER_ENV="$(for w in flotilla-worker-triage-shadow flotilla-worker-triage flotilla-worker-ops-read; do $D exec $w env 2>&1; done | grep -c 'MCP_')"
[ "$RW_ENV" = 2 ] && [ "$OTHER_ENV" = 0 ] && ok "the reads worker holds the stub server's address and token file; the shadow worker, the live triage worker and ops-read hold no MCP_* setting" \
  || bad "MCP settings: reads worker $RW_ENV of 2, the others $OTHER_ENV"
NW="$($D inspect flotilla-worker-reads --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"; NP="$($D inspect flotilla-egress-reads --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
NM="$($D network inspect e2e-mcp --format '{{range $k, $v := .Containers}}{{$v.Name}} {{end}}' 2>&1 | tr ' ' '\n' | grep . | sort | tr '\n' ' ')"
[ "$NW" = "${P}_flotilla " ] && echo "$NP" | grep >/dev/null "e2e-mcp" && [ "$NM" = "e2e-mcp flotilla-egress-reads " ] \
  && ok "the reads worker is on the internal flotilla network alone; the MCP network holds the stub server and the reads proxy, nothing else" || bad "reads networks: worker '$NW' proxy '$NP' mcp network '$NM'"
# the proxy's allowlist is the stub server and the model gateway: a host it can reach on the gateway's network but that is not listed is refused
PXA="$($D exec flotilla-worker-reads python3 -c 'import urllib.request as u
p = u.ProxyHandler({"http": "http://egress-reads:4750"})
try:
    print(u.build_opener(p).open("http://www.example.com/", timeout=10).status)
except Exception as e:
    print(getattr(e, "code", type(e).__name__))' 2>&1)"
[ "$PXA" = 407 ] || [ "$PXA" = 403 ] && ok "the reads proxy refuses a host off its list (www.example.com: $PXA)" || bad "the reads proxy let www.example.com through: $PXA"
free_routine e2e/reads
for i in $(seq 1 45); do cli runs e2e/reads 2>&1 | grep >/dev/null -E " (ok|error|timeout) " && break; sleep 2; done
cli runs e2e/reads 2>&1 | grep >/dev/null " ok " && ok "the live run of e2e/reads v1 ended ok on the reads worker" || bad "e2e/reads live run: $(cli runs e2e/reads 2>&1 | head -3) $($D logs flotilla-worker-reads 2>&1 | tail -5)"
RID="$(cli runs e2e/reads -json -n 5 | python3 -c 'import json,sys; r=[x for x in json.load(sys.stdin)["runs"] or [] if x["status"] == "ok"]; print(r[0]["id"] if r else "")')"
reads_snap() { $D run --rm --network none --user 65532:65532 -e STORE_DIR=/v -v flotilla-e2e-state:/v -v "$ROOT/deploy/server/store_snapshot.py:/s.py:ro" \
  "local/flotilla-worker-triage:$TAG" python3 /s.py backup > "$WORK/reads.db" 2> "$WORK/reads.err"; }
# the record is written after the run ends (the input is frozen at dispatch, before the reads happen): wait for it
RCHK=""
for _ in $(seq 1 15); do reads_snap; RCHK="$($RC record "$WORK/reads.db" "$RID" 2>&1)"; [ "${RCHK%% *}" = PASS ] && break; sleep 2; done
[ "${RCHK%% *}" = PASS ] && ok "a live run records its reads in input_reads, keyed by its id: ${RCHK#PASS }" || bad "the live run's record: $RCHK"
cli input show "$RID" 2>&1 | python3 -c '
import json, sys
e = json.load(sys.stdin)["envelope"]
a = (e.get("recorded_tool_results") or {}).get("answer") or []
sys.exit(0 if [x["tool"] for x in a] == ["lab.get_page", "lab.get_log"] and len(a[1]["result"]) == 12000 and all(x.get("ok") for x in a) else 1)' \
  && ok "the run's input carries the reads as recorded_tool_results of its station (answer), the long result whole: what a replay is served" || bad "input show: $(cli input show "$RID" 2>&1 | head -c 300)"
MC1="$(mcplog | $RC mcpcalls)"
[ "$MC1" = "PASS CALLS get_page,get_log UNAUTH 0 NOSESSION initialize" ] \
  && ok "the stub server was asked for the page and the log, each with the bearer token from the token file and, after initialize, its session id ($MC1)" || bad "the stub server's requests: $MC1"
RWL="$($D logs flotilla-worker-reads 2>&1 | grep "\"run_id\": \"$RID\"")"
echo "$RWL" | grep >/dev/null 'probe\[3\] lab.delete_page -> <unknown probe: lab.delete_page>' && ! echo "$MC1" | grep >/dev/null delete_page \
  && ok "lab.delete_page, a tool the station does not declare, was refused before any call: the stub server never saw it, and it is not in the record" || bad "the undeclared tool: $(echo "$RWL" | grep -o 'probe\[3\][^"]*') / $MC1"
seen flotilla-egress-reads "mcp.e2e.example" && ok "the reads proxy logged the stub server's host" || bad "the reads proxy has no record of the stub server's host"
# the shadows (v2: the same reads, v3: one the live run never made) wait for the live run and are served its record
S2=""; S3=""
for _ in $(seq 1 45); do
  reads_snap; S2="$($RC shadow "$WORK/reads.db" e2e/reads 2 2>&1)"; S3="$($RC shadow "$WORK/reads.db" e2e/reads 3 2>&1)"
  echo "$S2" | grep >/dev/null -E "status=(done|not_run)" && echo "$S3" | grep >/dev/null -E "status=(done|not_run)" && break; sleep 2
done
SR2="$(echo "$S2" | sed -n 's/.* run=\([^ ]*\) .*/\1/p')"
probes() { grep -o '"line": "PROBE [^"]*"' | sed 's/^"line": "//; s/"$//'; }
LIVEP="$(echo "$RWL" | probes)"; SHP2="$($D logs flotilla-worker-triage-shadow 2>&1 | grep "\"run_id\": \"$SR2\"" | probes)"
echo "$S2" | grep >/dev/null -E "^PASS status=done run=[^ ]+ run_status=ok exit=0 reason_chars=0 input_reads_rows=0 " && [ "$(echo "$LIVEP" | grep -c .)" = 3 ] && [ "$SHP2" = "$LIVEP" ] \
  && ok "the shadow v2 ran on the shadow worker and ended ok on the live run's record: its 3 PROBE lines (lengths, hashes, heads) are the live run's, the long log included, and it wrote no input_reads" \
  || bad "shadow v2: $S2 / live: $LIVEP / shadow: $SHP2"
MC2="$(mcplog | $RC mcpcalls)"
[ "$MC2" = "$MC1" ] && ok "the stub server saw no request from either shadow (its log is as the live run left it)" || bad "the stub server's requests after the shadows: $MC2 (before: $MC1)"
SW="$($D exec flotilla-worker-triage-shadow python3 -c 'import socket
try:
    socket.create_connection(("mcp.e2e.example", 8091), timeout=3)
    print("connected")
except OSError as e:
    print("unreachable", type(e).__name__)' 2>&1)"
[ "${SW%% *}" = unreachable ] && ok "the shadow worker has no route to the stub server at all ($SW)" || bad "the shadow worker reached the stub server: $SW"
echo "$S3" | grep >/dev/null -E "^PASS status=done run=[^ ]+ run_status=error exit=1 " && echo "$S3" | grep >/dev/null 'detail_last=recorded.RecordedMiss: no recorded result for station .answer. tool .lab.get_page. with arguments {"id":2}$' \
  && ok "the shadow v3 asked for a page the live run never read: its run failed as a miss (RecordedMiss), and nothing was sent to the server" || bad "shadow v3: $S3"
# a replay of the same input with the stub server down: flotillad -stations prints the contract, runtime/offline_run.py serves the record
echo "-- a replay of that input with the stub server down (flotillad -stations, then runtime/offline_run.py in the worker image)"
OFR="$WORK/offline-reads"
reads_off_clean() {  # the replay's folders hold files owned by the worker's uid: removed by that uid, then the folder by us
  [ -d "$OFR" ] && $D run --rm --user 10001:10001 -v "$OFR:/x" --entrypoint sh "local/flotilla-worker-triage:$TAG" -c 'rm -rf /x/out /x/out-miss' >/dev/null 2>&1
  rm -rf "$OFR"
}
reads_off_clean; mkdir -p "$OFR"; chmod 777 "$OFR"
printf 'e2e-eval-key' > "$OFR/eval.key"
cat > "$OFR/models.json" <<EOF
{"endpoints": {"gw": {"base_url": "http://litellm:8080/v1", "kind": "litellm", "key_file": "/work/eval.key", "defaults": {"tools": "native", "json": "object", "cost": "reported"}}},
 "aliases": {"reads-answer": {"endpoint": "gw", "model": "fleet-reads-answer"}}}
EOF
cli input export "$RID" --to "/var/lib/flotilla/exports/reads-$RID" >/dev/null 2>&1 && $D cp "flotillad:/var/lib/flotilla/exports/reads-$RID" "$OFR/in" >/dev/null 2>&1 && chmod -R a+rX "$OFR/in"
for v in 1 3; do
  $D run --rm --network none --user 65532:65532 -v "$OFR:/work" -v "$WORK/config/specs:/etc/flotilla/specs:ro" "local/flotilla-flotillad:$TAG" \
    -stations /etc/flotilla/specs/e2e/reads/v$v -models /work/models.json > "$OFR/contract-v$v.json" 2> "$OFR/contract-v$v.err"
done
python3 -c '
import json, sys
c = json.load(open(sys.argv[1]))
e = c["env"]
st = json.loads(e["FLOTILLA_STATIONS"])["stations"]["answer"]
sys.exit(0 if st["reads"][0]["server"] == "lab" and st["reads"][0]["tools"] == ["get_page", "get_log"] and not any("MCP_" in k for k in e) else 1)' "$OFR/contract-v1.json" \
  && ok "flotillad -stations printed v1's contract: the station's reads, and no MCP_* setting in its environment (the server's address and token are the reads worker's)" || bad "-stations: $(head -c 300 "$OFR/contract-v1.json") $(cat "$OFR/contract-v1.err")"
$D stop e2e-mcp >/dev/null
$D run --rm --network e2e-litellm --user 10001:10001 -v "$OFR:/work" -v "$WORK/config/specs/e2e:/opt/flotilla/fleets/e2e:ro" "local/flotilla-worker-triage:$TAG" \
  python3 /opt/flotilla/runtime/offline_run.py --contract /work/contract-v1.json --input /work/in/input.json --models /work/models.json --out /work/out --forbid /opt/flotilla/fleets > "$OFR/offline.out" 2>&1; ORC=$?
OFFP="$(grep '^PROBE ' "$OFR/out/output.txt" 2>/dev/null)"
python3 -c '
import json, sys
sys.exit(0 if json.load(open(sys.argv[1] + "/result.json"))["exit_code"] == 0 else 1)' "$OFR/out" && [ "$ORC" = 0 ] && [ "$OFFP" = "$LIVEP" ] \
  && ok "with the stub server stopped, the offline run of the live run's input succeeded from the record: the same 3 PROBE lines as the live run, the long log whole" \
  || bad "offline run rc=$ORC: $(tail -c 400 "$OFR/offline.out") out: $(tail -c 400 "$OFR/out/output.txt" 2>/dev/null)"
$D run --rm --network e2e-litellm --user 10001:10001 -v "$OFR:/work" -v "$WORK/config/specs/e2e:/opt/flotilla/fleets/e2e:ro" "local/flotilla-worker-triage:$TAG" \
  python3 /opt/flotilla/runtime/offline_run.py --contract /work/contract-v3.json --input /work/in/input.json --models /work/models.json --out /work/out-miss --forbid /opt/flotilla/fleets > "$OFR/offline-miss.out" 2>&1
python3 -c '
import json, sys
r = json.load(open(sys.argv[1] + "/result.json"))
out = open(sys.argv[1] + "/output.txt").read()
sys.exit(0 if r["exit_code"] != 0 and "RecordedMiss" in out and "is not set" not in out and "read failed" not in out else 1)' "$OFR/out-miss" \
  && ok "the offline run of v3 (a read the record lacks) failed as a miss, and no read was attempted: nothing was sent live" || bad "offline miss: $(tail -c 300 "$OFR/out-miss/output.txt" 2>/dev/null) $(tail -c 200 "$OFR/offline-miss.out")"
$D start e2e-mcp >/dev/null
MC3="$(mcplog | $RC mcpcalls)"
[ "$MC3" = "$MC1" ] && ok "the stub server's log is still the live run's two calls after the replays" || bad "the stub server's requests after the replays: $MC3"
reads_off_clean
fi
if want matrix; then
echo "== the no-effect matrix (docs/wip/v2-design.md section 4): every example routine on the ops-read worker, shadowed, promoted, rolled back, on extended state"
if [ "${DEMO:-}" != 1 ]; then
# The example fleets ship v1 only; the generated v2 (deploy/e2e/make_shadows.py) is v1's files plus a marker, so the real example code runs as a shadow.
# The folder gains them while flotillad runs: they are listed as shadows at its next start (shadows are on since the shadow section).
# First every routine of the matrix gets work and a live run (new records, plans approved, acting routines and checks run), while it has no shadow to
# start beside each live run.
MXENV=(WORK="$WORK" ROOT="$ROOT" TAG="$TAG" DOCKER_CONTEXT_DEV="${DOCKER_CONTEXT_DEV:-dev}" E2E_MUTANT="${E2E_MUTANT:-}")
env "${MXENV[@]}" python3 "$ROOT/deploy/e2e/matrix.py" drive || bad "the matrix's live runs"
python3 "$ROOT/deploy/e2e/make_shadows.py" "$WORK/config/specs" ops-read > "$WORK/matrix.list" || bad "make_shadows.py"
NGEN="$(sed -n 's/^GENERATED //p' "$WORK/matrix.list")"; grep -v '^GENERATED' "$WORK/matrix.list" > "$WORK/matrix-routines.txt"
# E2E_MUTANT=noeffect-vacuous: crm/scan's shadow does nothing, and writes no record; matrix-live-write: it writes to a file the live worker shares (the
# worker state hash must change)
if [[ ",${E2E_MUTANT:-}," == *,noeffect-vacuous,* ]]; then sed -i 's|^  command: .*|  command: "true"|' "$WORK/config/specs/crm/scan/v2/spec.yaml"; fi
if [[ ",${E2E_MUTANT:-}," == *,matrix-live-write,* ]]; then sed -i 's|^  command: \(.*\)$|  command: echo shadow-wrote >> /mut/live-state.txt \&\& \1|' "$WORK/config/specs/crm/scan/v2/spec.yaml"; fi
$D restart flotillad >/dev/null
for i in $(seq 1 30); do cli health >/dev/null 2>&1 && break; sleep 2; done
[ "${NGEN:-0}" -ge 20 ] && ok "$NGEN example routines have a generated shadow v2 (the ops-read worker's)" || bad "generated shadows: '$NGEN'"
for i in $(seq 1 30); do [ "$($D inspect flotilla-worker-ops-read-shadow --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ] && break; sleep 2; done
[ "$($D inspect flotilla-worker-ops-read-shadow --format '{{.State.Health.Status}}' 2>&1)" = healthy ] && ok "the ops-read shadow worker is healthy" || bad "ops-read shadow worker: $($D logs flotilla-worker-ops-read-shadow 2>&1 | tail -3)"
MX="$(WORK="$WORK" ROOT="$ROOT" TAG="$TAG" DOCKER_CONTEXT_DEV="${DOCKER_CONTEXT_DEV:-dev}" E2E_MUTANT="${E2E_MUTANT:-}" python3 "$ROOT/deploy/e2e/matrix.py" "$WORK/matrix-routines.txt" 2>&1)"; MXRC=$?
echo "$MX"
MXS="$(echo "$MX" | grep '^NOEFFECT PASS')"
[ "$MXRC" = 0 ] && echo "$MXS" | grep >/dev/null -E "FAIL 0$" && [ "$(echo "$MX" | grep -c 'shadow=PASS')" -ge 5 ] \
  && ok "the no-effect matrix: $(echo "$MXS" | cut -c1-200)" || bad "the no-effect matrix: $(echo "$MX" | grep -E '^   |^NOEFFECT PASS' | head -12 | tr '\n' '|' | cut -c1-900)"
fi
fi
if want ui || want hooks || want ask; then   # the first admin is the one hooks and ask approve with: they need no UI checks
echo "== users and the UI listener (ADR-0014); last, since the first admin retires the operator token"
UA="$(cli user add ada --role admin 2>&1)"
ADMIN="$(echo "$UA" | sed -n 's/.*"token":"\(flt_[0-9a-f]*\)".*/\1/p')"
[ ${#ADMIN} = 68 ] && ok "the first admin, with the operator token (a 256-bit token, shown once)" || bad "first admin: $UA"
OUT="$(cli status 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 403" && ok "after it, the operator token is retired" || bad "operator token after the first admin: $OUT"
fi
if want ui; then   # the UI's own checks lean on the shadows' and stations' pairs
OUT="$($D exec -e FLOTILLA_TOKEN="$ADMIN" flotillad /usr/local/bin/flotilla resume platform/heartbeat 2>&1)"
echo "$OUT" | grep >/dev/null "needs a reason" && ok "loosening without a reason is refused" || bad "resume without a reason: $OUT"
OUT="$($D exec -e FLOTILLA_TOKEN="$ADMIN" flotillad /usr/local/bin/flotilla resume platform/heartbeat --reason "e2e" 2>&1)"
echo "$OUT" | grep >/dev/null '"by":"user:ada"' && ok "the admin resumes with a reason, as user:ada" || bad "admin resume: $OUT"
UIC="$($D exec -i flotilla-worker-triage python3 - "$ADMIN" "$OP" "$AGENT" < "$ROOT/deploy/e2e/ui_check.py" 2>&1)"; UIRC=$?
while IFS= read -r line; do
  case "$line" in PASS*) ok "UI: ${line#PASS }" ;; FAIL*) bad "UI: ${line#FAIL }" ;; esac
done <<< "$UIC"
[ "$UIRC" = 0 ] || bad "ui_check.py exit $UIRC: $(echo "$UIC" | tail -3 | tr '\n' ' ')"
$D run -d --name flotilla-e2e-caddy --label $LABEL --network e2e-ui --memory 256m \
  -v "$ROOT/deploy/e2e/Caddyfile:/etc/caddy/Caddyfile:ro" "$CADDY_IMAGE" >/dev/null || bad "the TLS front did not start"
UIB="$($D run --rm --label $LABEL --network e2e-ui --memory 2g --init -v "$ROOT/deploy/e2e:/e2e:ro" "$PLAYWRIGHT_IMAGE" \
  python3 /e2e/ui_browser.py "$ADMIN" 2>&1)"; UIBRC=$?
while IFS= read -r line; do
  case "$line" in PASS*) ok "browser: ${line#PASS }" ;; FAIL*) bad "browser: ${line#FAIL }" ;; esac
done <<< "$UIB"
[ "$UIBRC" = 0 ] || bad "ui_browser.py exit $UIBRC: $(echo "$UIB" | tail -3 | tr '\n' ' ')"

fi
if want hooks; then   # needs the first admin ($ADMIN), which it makes if ui does not
echo "== hooks (RFC-0014 §4.2): the stub plumbing on the plumbing network calls them, signed"
hcli() { $D exec -e FLOTILLA_TOKEN="$ADMIN" flotillad /usr/local/bin/flotilla "$@"; }   # an admin exists by now: the callers-file operator token is refused
# the meeting case waits, handed over, until the plumbing section resumes its acting routine (a paused routine's case waits)
hcli pause meetings/notes >/dev/null 2>&1 || bad "pause meetings/notes: $(hcli pause meetings/notes 2>&1)"
HK="$($D run --rm --network e2e-plumbing --read-only --user 10001:10001 -v "$ROOT/deploy/e2e/hookcall.py:/hookcall.py:ro" \
  -e HOOK_SECRET_MEETING="$(cat "$WORK/config/hooks/meeting.secret")" -e HOOK_SECRET_SMOKE="$(cat "$WORK/config/hooks/smoke.secret")" \
  --entrypoint python3 "local/flotilla-worker-triage:$TAG" /hookcall.py http://flotillad:8488 2>&1)"
echo "$HK"
hk() { echo "$HK" | awk -v l="$1" '$1 == l {print $2; exit}'; }
HCASE="$(echo "$HK" | awk '$1 == "opened" {print $3}' | python3 -c 'import json,sys; print(json.load(sys.stdin).get("case", ""))' 2>/dev/null)"
[ "$(hk opened)" = 202 ] && [ -n "$HCASE" ] && hcli case list --all 2>&1 | grep "^$HCASE " | grep >/dev/null "handed_over" \
  && ok "a signed call opened one meeting case, handed over ($HCASE)" || bad "hook opened: $(hk opened) case '$HCASE': $(hcli case list --all 2>&1 | grep "^$HCASE " )"
[ "$(hk replay)" = 200 ] && echo "$HK" | grep '^replay ' | grep >/dev/null "\"case\":\"$HCASE\"" \
  && ok "the same delivery again opened nothing (200, the same case)" || bad "hook replay: $(echo "$HK" | grep '^replay ')"
[ "$(hk unsigned)" = 401 ] && [ "$(hk wrong-key)" = 401 ] && [ "$(hk stale)" = 401 ] \
  && ok "an unsigned call, one signed with another key and a stale one are refused (401)" || bad "hook signatures: $(echo "$HK" | grep -E '^(unsigned|wrong-key|stale) ')"
[ "$(hk oversized)" = 413 ] && [ "$(hk bad-target)" = 400 ] && [ "$(hk undeclared)" = 404 ] \
  && ok "an oversized body (413), a target outside its pattern (400) and an undeclared hook (404) are refused" || bad "hook refusals: $(echo "$HK" | grep -E '^(oversized|bad-target|undeclared) ')"
[ "$(hcli case list --all 2>&1 | awk '$2 == "meeting"' | wc -l)" = 1 ] \
  && ok "the refused calls opened no case: one meeting case in all" || bad "meeting cases: $(hcli case list --all 2>&1 | awk '$2 == "meeting"')"
HSHOW="$(hcli case show "$HCASE" 2>&1)"
echo "$HSHOW" | grep >/dev/null " hook:meeting=standup-2026-11-22.md$" && echo "$HSHOW" | grep >/dev/null "fields not declared, ignored: kind" && echo "$HSHOW" | grep >/dev/null "by detector  detected->handed_over" \
  && ok "the case is in the hook's own key space, a payload field the hook does not declare (kind) was ignored and recorded, the hook handed it over" || bad "hook case: $HSHOW"
SCASE="$(echo "$HK" | awk '$1 == "smoke" {print $3}' | python3 -c 'import json,sys; print(json.load(sys.stdin).get("case", ""))' 2>/dev/null)"
[ "$(hk smoke)" = 202 ] && hcli case list --all 2>&1 | grep "^$SCASE " | grep >/dev/null "closed" \
  && ok "the smoke hook's case closed on arrival ($SCASE)" || bad "smoke: $(hk smoke) $(hcli case list --all 2>&1 | grep "^$SCASE ")"
[ "$(hk api-route)" = 404 ] && ok "the hooks' listener serves no API route (/v1/status: 404)" || bad "hooks listener /v1/status: $(hk api-route)"
HEV="$(hcli events -n 200 2>&1)"
[ "$(echo "$HEV" | grep -c 'hook.refused')" -ge 5 ] && [ "$(echo "$HEV" | grep -c 'hook.accepted')" -ge 3 ] \
  && ok "every call to a declared hook is in the events, accepted or refused" || bad "hook events: $(echo "$HEV" | grep 'hook\.')"
OUT="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"
# the warning for a kind no routine acts on is cmd/flotillad's unit test (hookLines); here meetings/notes acts on meeting
echo "$OUT" | grep >/dev/null "^hook meeting: " && ! echo "$OUT" | grep >/dev/null "^WARNING hook meeting" && echo "$OUT" | grep >/dev/null "^hook smoke: .*closes it on arrival" \
  && ok "-check lists the hooks, and gives no warning for a kind a routine acts on" || bad "-check hooks: $(echo "$OUT" | grep -i hook)"
fi

if want plumbing; then   # needs hooks' meeting case ($HCASE) and the first admin: name hooks with it
echo "== case 3 and plumbing.call (RFC-0014 §4.5, §4.6): a meeting's follow-ups, then its decisions, each approved once, posted signed to the stub plumbing, each checked back"
n8nlog() { $D logs e2e-n8n 2>&1; }
sends() { n8nlog | grep '"event": "send"' | grep "\"name\": \"$1\""; }
waitcase() { for _ in $(seq 1 60); do hcli case list --all 2>&1 | grep "^$1 " | grep >/dev/null "$2" && return 0; sleep 2; done; return 1; }
hcli action autonomy plumbing.create_tasks propose --reason "e2e: the meeting's follow-ups wait for an approval" >/dev/null 2>&1
hcli action autonomy plumbing.post_decisions propose --reason "e2e: the meeting's decisions wait for an approval" >/dev/null 2>&1
# the no-effect matrix's rollback left meetings/decisions (an ops-read routine) at record; its spec says propose
hcli autonomy meetings/decisions propose --reason "e2e: back to its spec's autonomy after the matrix" >/dev/null 2>&1 || bad "autonomy meetings/decisions: $(hcli autonomy meetings/decisions propose --reason e2e 2>&1)"
hcli resume meetings/notes --reason "e2e: the plumbing section" >/dev/null 2>&1 || bad "resume meetings/notes: $(hcli resume meetings/notes --reason e2e 2>&1)"
waitcase "$HCASE" awaiting_approval; PSHOW="$(hcli case show "$HCASE" 2>&1)"
echo "$PSHOW" | grep >/dev/null "plumbing.create_tasks" && echo "$PSHOW" | grep >/dev/null "then decisions" && hcli case list --all 2>&1 | grep "^$HCASE " | grep >/dev/null "awaiting_approval" \
  && ok "meetings/notes read the notes with the scripted model and planned one plumbing.create_tasks, then decisions; it waits for an approval" || bad "the first plan: $(echo "$PSHOW" | tail -8)"
[ -z "$(n8nlog | grep '"event": "send"')" ] && ok "nothing reached the plumbing before the approval" || bad "a send before the approval: $(n8nlog)"
# a second meeting whose list a person rejects, before any send (a rejection executes nothing, so the platform's hourly cap
# on executions, which the sections above fill, never defers it): nothing is sent for it, and it never reaches its decisions
H2="$($D run --rm --network e2e-plumbing --read-only --user 10001:10001 -v "$ROOT/deploy/e2e/hookcall.py:/hookcall.py:ro" \
  -e HOOK_SECRET_MEETING="$(cat "$WORK/config/hooks/meeting.secret")" -e HOOK_SECRET_SMOKE="$(cat "$WORK/config/hooks/smoke.secret")" \
  --entrypoint python3 "local/flotilla-worker-triage:$TAG" /hookcall.py http://flotillad:8488 second 2>&1)"
RCASE="$(echo "$H2" | awk '$1 == "second" {print $3}' | python3 -c 'import json,sys; print(json.load(sys.stdin).get("case", ""))' 2>/dev/null)"
[ -n "$RCASE" ] && waitcase "$RCASE" awaiting_approval && hcli case reject "$RCASE" --reason "e2e: the list is wrong" >/dev/null 2>&1 && waitcase "$RCASE" escalated \
  && ok "the second meeting's list was proposed and rejected ($RCASE escalated)" || bad "the rejection: $H2 $(hcli case show "$RCASE" 2>&1 | tail -6)"
hcli case approve "$HCASE" >/dev/null 2>&1 || bad "approve: $(hcli case approve "$HCASE" 2>&1)"
for _ in $(seq 1 60); do n8nlog | grep '"event": "status"' | grep '"name": "create_tasks"' | grep >/dev/null '"done": true' && break; sleep 2; done
SEND="$(sends create_tasks)"
[ "$(echo "$SEND" | grep -c .)" = 1 ] && echo "$SEND" | grep >/dev/null "\"case\": \"$HCASE\"" && echo "$SEND" | grep >/dev/null '"action": "plumbing.create_tasks"' \
  && echo "$SEND" | grep >/dev/null '"who": "ana@example.com"' && echo "$SEND" | grep >/dev/null '"by": "2026-11-30"' && echo "$SEND" | grep >/dev/null '"delivery": "plan-[0-9a-f]\{16\}"' \
  && ok "the approved list reached the plumbing once, signed with its callback's secret, its follow-ups in their fixed shape, delivery plan-<hash16>" \
  || bad "the plumbing's create_tasks sends: $(n8nlog)"
DL="$(echo "$SEND" | sed -n 's/.*"delivery": "\(plan-[0-9a-f]*\)".*/\1/p')"
n8nlog | grep '"event": "status"' | grep >/dev/null "\"delivery\": \"$DL\"" \
  && ok "the check asked the plumbing for the same delivery ($DL)" || bad "the check's status call: $(n8nlog | grep status)"
# verified, the case is not closed: its plan's then hands it to the stage decisions (RFC-0013 §2, amended 2026-11-22)
for _ in $(seq 1 60); do hcli case show "$HCASE" 2>&1 | grep >/dev/null "plumbing.post_decisions" && hcli case list --all 2>&1 | grep "^$HCASE " | grep >/dev/null "awaiting_approval" && break; sleep 2; done
PSHOW="$(hcli case show "$HCASE" 2>&1)"
echo "$PSHOW" | grep >/dev/null "checking->handed_over" && echo "$PSHOW" | grep >/dev/null "plumbing.post_decisions" && hcli case list --all 2>&1 | grep "^$HCASE " | grep >/dev/null "awaiting_approval" \
  && ok "verified, the same case went on to its stage decisions, whose plumbing.post_decisions waits for its own approval" || bad "after the follow-ups: $(echo "$PSHOW" | tail -10)"
[ -z "$(sends post_decisions)" ] && ok "the decisions were not sent before their own approval" || bad "post_decisions before its approval: $(sends post_decisions)"
hcli case approve "$HCASE" >/dev/null 2>&1 || bad "approve the decisions: $(hcli case approve "$HCASE" 2>&1)"
for _ in $(seq 1 60); do n8nlog | grep '"event": "status"' | grep '"name": "post_decisions"' | grep >/dev/null '"done": true' && break; sleep 2; done
SEND2="$(sends post_decisions)"
[ "$(echo "$SEND2" | grep -c .)" = 1 ] && echo "$SEND2" | grep >/dev/null "\"case\": \"$HCASE\"" && echo "$SEND2" | grep >/dev/null '"page": "decisions"' \
  && echo "$SEND2" | grep >/dev/null 'Decisions from standup-2026-11-22.md' && echo "$SEND2" | grep >/dev/null -- '- Ship on Friday.' \
  && ok "the approved decisions reached the plumbing once, for the decisions page, labelled" || bad "the plumbing's post_decisions sends: $(n8nlog)"
DL2="$(echo "$SEND2" | sed -n 's/.*"delivery": "\(plan-[0-9a-f]*\)".*/\1/p')"
waitcase "$HCASE" closed; PSHOW="$(hcli case show "$HCASE" 2>&1)"
hcli case list --all 2>&1 | grep "^$HCASE " | grep >/dev/null "closed" && echo "$PSHOW" | grep >/dev/null "create_tasks-$DL" && echo "$PSHOW" | grep >/dev/null "post_decisions-$DL2" \
  && ok "the case closed on the second check, both refs on its record (create_tasks-$DL, post_decisions-$DL2)" || bad "the case after the decisions: $PSHOW"
[ -z "$(n8nlog | grep '"event": "refused"')" ] && ok "the plumbing refused nothing: every call carried a valid signature" || bad "refused at the plumbing: $(n8nlog | grep refused)"
[ -n "$RCASE" ] && [ -z "$(n8nlog | grep '"event": "send"' | grep "\"case\": \"$RCASE\"")" ] && ! hcli case show "$RCASE" 2>&1 | grep >/dev/null "plumbing.post_decisions" \
  && hcli case list --all 2>&1 | grep "^$RCASE " | grep >/dev/null "escalated" \
  && ok "the rejected list sent nothing, and its case never went on to its decisions" || bad "after the rejection: $(n8nlog | grep "$RCASE") $(hcli case show "$RCASE" 2>&1 | tail -6)"
[ "$(hcli case list --all 2>&1 | awk '$2 == "meeting"' | wc -l)" = 2 ] \
  && ok "two meetings, two cases: no case was opened for a follow-up" || bad "meeting cases: $(hcli case list --all 2>&1 | awk '$2 == "meeting"')"
# the worker reaches the plumbing only through its proxy's allowlist: flotillad's hooks listener is on the same network, and refused
PX="$($D exec flotilla-worker-plumbing python3 -c 'import urllib.request as u
p = u.ProxyHandler({"http": "http://egress-plumbing:4750"})
try:
    print(u.build_opener(p).open("http://flotillad:8488/healthz", timeout=10).status)
except Exception as e:
    print(getattr(e, "code", e))' 2>&1)"
[ "$PX" = 407 ] || [ "$PX" = 403 ] && ok "the plumbing proxy refuses any host but the plumbing's (flotillad:8488: $PX)" || bad "the plumbing proxy let flotillad:8488 through: $PX"
fi

if want ask && [ "${DEMO:-}" != 1 ]; then   # needs the first admin ($ADMIN), which it makes if ui does not; nothing else (not hooks, plumbing or reads)
echo "== case 1 (RFC-0014 §4.2, §4.5, §4.8): a question opens a case, knowledge/answer reads three servers and cites what it read, a person approves the reply"
hcli() { $D exec -e FLOTILLA_TOKEN="$ADMIN" flotillad /usr/local/bin/flotilla "$@"; }
n8nlog() { $D logs e2e-n8n 2>&1; }
sends() { n8nlog | grep '"event": "send"' | grep "\"name\": \"$1\""; }
waitcase() { for _ in $(seq 1 60); do hcli case list --all 2>&1 | grep "^$1 " | grep >/dev/null "$2" && return 0; sleep 2; done; return 1; }
PYIMG=python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
SB_DIR="${SB_DIR:-/work/second-brain/main}"
# the library is second-brain's own read server over an index built from its own fixtures (sb seed, sb build), never a stand-in
if [ ! -f "$SB_DIR/sb/server.py" ]; then
  bad "second-brain's checkout is not at SB_DIR=$SB_DIR (the ask section reads its real server)"
else
  SBW="$WORK/sb"; rm -rf "$SBW"; mkdir -p "$SBW" && cp -r "$SB_DIR/tests/fixtures/content" "$SBW/content" && cp -r "$SB_DIR/tests/fixtures/data" "$SBW/data" && chmod -R a+rwX "$SBW"
  sbrun() { $D run --rm --network none --user "$(id -u):$(id -g)" -e PYTHONDONTWRITEBYTECODE=1 -e PYTHONPATH=/sb -v "$SB_DIR:/sb:ro" -v "$SBW:/w" $PYIMG python3 -m sb "$@"; }
  SBB="$(sbrun seed --content /w/content 2>&1 && sbrun build --content /w/content --data /w/data 2>&1)"
  chmod -R a+rX "$SBW/data"
  echo "$SBB" | grep >/dev/null '"papers": 4, "with_text": 3' && ok "second-brain's index built from its fixtures: $(echo "$SBB" | tail -1)" || bad "sb build: $SBB"
  $D run -d --label $LABEL --name e2e-library --network e2e-mcp --network-alias library.e2e.example --memory 128m --read-only --cap-drop ALL --user "$(id -u):$(id -g)" \
    -e PYTHONDONTWRITEBYTECODE=1 -e PYTHONPATH=/sb -v "$SB_DIR:/sb:ro" -v "$SBW/data:/d:ro" -v "$WORK/config/mcp/library.token:/secrets/library.token:ro" \
    $PYIMG python3 -u -m sb serve --data /d --host 0.0.0.0 --port 8790 --token-file /secrets/library.token >/dev/null || bad "sb serve did not start"
fi
# the documents and tasks servers: the stub MCP server under two names, with a token of its own
$D run -d --label $LABEL --name e2e-mcp-docs --network e2e-mcp --network-alias docs.e2e.example --network-alias tasks.e2e.example --memory 64m \
  -e MCP_STUB_TOKEN_FILE=/secrets/docs.token -v "$ROOT/deploy/e2e/mcp_stub.py:/mcp_stub.py:ro" -v "$WORK/config/mcp:/secrets:ro" \
  $PYIMG python3 -u /mcp_stub.py 8091 >/dev/null || bad "the documents and tasks stub did not start"
LU=""
for _ in $(seq 1 20); do
  LU="$($D run --rm --network e2e-mcp $PYIMG python3 -c 'import urllib.request as u, urllib.error as e
try:
    u.urlopen(u.Request("http://library.e2e.example:8790/mcp", data=b"{}", headers={"Content-Type": "application/json"}), timeout=5); print(200)
except e.HTTPError as x:
    print(x.code)
except Exception as x:
    print(type(x).__name__)' 2>&1)"
  [ "$LU" = 401 ] && break; sleep 1
done
LA="$($D run --rm --network e2e-mcp --user "$(id -u):$(id -g)" -v "$WORK/config/mcp/library.token:/t:ro" $PYIMG python3 -c 'import urllib.request as u
t = open("/t").read().strip()
r = u.urlopen(u.Request("http://library.e2e.example:8790/mcp", data=b"{\"jsonrpc\": \"2.0\", \"id\": 1, \"method\": \"initialize\", \"params\": {\"protocolVersion\": \"2025-06-18\", \"capabilities\": {}, \"clientInfo\": {\"name\": \"e2e\", \"version\": \"1\"}}}", headers={"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Authorization": "Bearer " + t}), timeout=5)
print(r.status)' 2>&1 | tail -1)"
[ "$LU" = 401 ] && [ "$LA" = 200 ] && ok "second-brain's server is up on the MCP network: 401 without its bearer token, 200 with it" || bad "sb serve: without a token $LU, with it $LA: $($D logs e2e-library 2>&1 | tail -3)"
# shadows: a v2 of knowledge/answer waits for the live run (reads added the setting; a run of this section alone adds it, and flotillad reads it at start)
if ! grep >/dev/null '^shadows:' "$WORK/config/flotillad.yaml"; then
  printf 'shadows: {enabled: true}\n' >> "$WORK/config/flotillad.yaml"
  $D restart flotillad >/dev/null
  for _ in $(seq 1 30); do hcli health >/dev/null 2>&1 && break; sleep 2; done
fi
hcli action autonomy plumbing.reply propose --reason "e2e: the answers wait for an approval" >/dev/null 2>&1 || bad "plumbing.reply autonomy: $(hcli action autonomy plumbing.reply propose --reason e2e 2>&1)"
# The platform's cap is 10 executions an hour and every section shares the hour: the questions' plans are proposed only below it, and the first
# answer's approval is the last execution to fit. A section that spends more makes these plans defer (policy: "10 of 10 executions platform-wide").
$D run --rm --network none --user 65532:65532 -e STORE_DIR=/v -v flotilla-e2e-state:/v -v "$ROOT/deploy/server/store_snapshot.py:/s.py:ro" \
  "local/flotilla-worker-triage:$TAG" python3 /s.py backup > "$WORK/store-budget.db" 2> "$WORK/store-budget.err"
EXN="$(python3 "$ROOT/deploy/e2e/exec_budget.py" "$WORK/store-budget.db" 2>&1)"
[ "$EXN" -le 9 ] 2>/dev/null && ok "the platform's hourly budget: $EXN of 10 executions spent before the questions, room for the first answer's" \
  || bad "the platform's hourly budget: '$EXN' executions in the last hour before the questions (at most 9 leave the plans proposable and room to approve one): $(cat "$WORK/store-budget.err" | tail -2)"
hcase() { echo "$HA" | awk -v l="$1" '$1 == l {print $3}' | python3 -c 'import json,sys; print(json.load(sys.stdin).get("case", ""))' 2>/dev/null; }
hak() { echo "$HA" | awk -v l="$1" '$1 == l {print $2; exit}'; }
HA="$($D run --rm --network e2e-plumbing --read-only --user 10001:10001 -v "$ROOT/deploy/e2e/hookcall.py:/hookcall.py:ro" \
  -e HOOK_SECRET_ASK="$(cat "$WORK/config/hooks/ask.secret")" --entrypoint python3 "local/flotilla-worker-triage:$TAG" /hookcall.py http://flotillad:8488 ask 2>&1)"
echo "$HA"
Q1="$(hcase q1)"; Q2="$(hcase q2)"; Q3="$(hcase q3)"
# the asker: the directory's answer is written into the case's data when it opens, a payload cannot say it, and every call is an event
ASNAP() { $D run --rm --network none --user 65532:65532 -e STORE_DIR=/v -v flotilla-e2e-state:/v -v "$ROOT/deploy/server/store_snapshot.py:/s.py:ro" \
  "local/flotilla-worker-triage:$TAG" python3 /s.py backup > "$WORK/ask.db" 2> "$WORK/ask.err"; }
RC="python3 $ROOT/deploy/e2e/reads_check.py"
[ "$(hak q1)" = 202 ] && [ "$(hak q1-replay)" = 200 ] && [ "$(hcase q1-replay)" = "$Q1" ] && [ "$(hak q-unsigned)" = 401 ] && [ -n "$Q2" ] && [ -n "$Q3" ] \
  && ok "a signed ask call opened a question case ($Q1); the same delivery again opened nothing (200); an unsigned one was refused (401)" || bad "ask calls: $HA"
ASNAP
CD1="$($RC casedata "$WORK/ask.db" "$Q1")"; CD2="$($RC casedata "$WORK/ask.db" "$Q2")"
[ "$CD1" = "PASS asker=ada@example.org asker_name=Ada_Example thread=t-e2e-1 user=U0E2EASK01" ] \
  && ok "the asker resolved from the directory: slack id U0E2EASK01 is Ada Example (ada@example.org), written into the case's data" || bad "the known asker: $CD1"
[ "$CD2" = "PASS asker=unknown asker_name=- thread=t-e2e-2 user=U0NOBODY00" ] \
  && ok "an id the directory does not hold resolved to unknown, though its payload named ada@example.org as asker (the payload cannot say who asked)" || bad "the unknown asker: $CD2"
AEV="$(hcli events -n 300 2>&1 | grep 'hook.asker')"
echo "$AEV" | grep >/dev/null "$Q1 .*known: ada@example.org (slack id U0E2EASK01)" && echo "$AEV" | grep >/dev/null "$Q2 .*unknown: slack id U0NOBODY00 is not in the directory" \
  && [ "$(echo "$AEV" | grep -c "$Q1")" = 1 ] && ok "each asker is a hook.asker event of its case, once (the replayed delivery wrote none)" || bad "hook.asker events: $AEV"
hcli case show "$Q2" 2>&1 | grep >/dev/null "fields not declared, ignored: asker, asker_name" && ok "the forged asker and asker_name fields were ignored and recorded" || bad "the forged fields: $(hcli case show "$Q2" 2>&1 | tail -3)"
# knowledge/answer: each case is handed to it; the model is scripted (deploy/e2e/stub.py), its reads are real
waitcase "$Q1" awaiting_approval && waitcase "$Q2" awaiting_approval && waitcase "$Q3" escalated \
  && ok "knowledge/answer planned an answer for each of the two questions it could source (awaiting approval) and escalated the one about the ghost" \
  || bad "the question cases: $(hcli case list --all 2>&1 | grep question)"
ASNAP
AR1="$($RC askrun "$WORK/ask.db" "$Q1")"; AR2="$($RC askrun "$WORK/ask.db" "$Q2")"; AR3="$($RC askrun "$WORK/ask.db" "$Q3")"
RID1="$(echo "$AR1" | sed -n 's/.* run=\([^ ]*\) .*/\1/p')"; RID2="$(echo "$AR2" | sed -n 's/.* run=\([^ ]*\) .*/\1/p')"
echo "$AR1" | grep >/dev/null "^PASS run=[^ ]* action=plumbing.reply outcome=propose thread=t-e2e-1 channel=general$" && echo "$AR2" | grep >/dev/null "^PASS run=[^ ]* action=plumbing.reply outcome=propose thread=t-e2e-2 channel=general$" \
  && ok "each plan is plumbing.reply at propose, the thread from its case (t-e2e-1, t-e2e-2) and the channel the action's one (general)" || bad "the plans: $AR1 / $AR2"
echo "$AR3" | grep >/dev/null "^PASS run=[^ ]* plans=0 escalated=refused_by_the_gates:_no_sourced_answer:_citation_1_(docs:_https://docs.e2e.example/d/ghost-project)_is_not_in_a_read_this_run_made" \
  && ok "the answer that cited a link no read returned (the ghost project) escalated as \"no sourced answer\" and planned nothing" || bad "the ghost question: $AR3"
# the reads, recorded with the run: docs and tasks on the stub, library on second-brain's real server
RK="$($RC askreads "$WORK/ask.db" "$RID1")"
[ "${RK%% *}" = PASS ] && ok "a live run records its reads in input_reads: ${RK#PASS }" || bad "the answer run's record: $RK"
LIBLOG="$($D logs e2e-library 2>&1)"
[ "$(echo "$LIBLOG" | grep -c '"status": "unauthorized"')" = 1 ] && echo "$LIBLOG" | grep >/dev/null '"tool": "search"' && echo "$LIBLOG" | grep >/dev/null '"tool": "read"' \
  && ok "second-brain's server served the search and the read (sb serve's own log), and refused only the unauthenticated probe" || bad "sb serve's log: $(echo "$LIBLOG" | tail -5)"
DOCLOG="$($D logs e2e-mcp-docs 2>&1 | $RC mcpcalls)"
echo "$DOCLOG" | grep >/dev/null "^PASS CALLS .* UNAUTH 0 NOSESSION initialize$" && ! echo "$DOCLOG" | grep >/dev/null "delete\|write" \
  && ok "the documents and tasks server was asked only to search, read and list, each with its bearer token and session ($DOCLOG)" || bad "the docs server's requests: $DOCLOG"
cli_input() { hcli input show "$1" 2>&1; }
cli_input "$RID1" | python3 -c '
import json, sys
a = (json.load(sys.stdin)["envelope"].get("recorded_tool_results") or {}).get("answer") or []
sys.exit(0 if [x["tool"] for x in a] == ["docs.search", "docs.read", "tasks.list", "library.search", "library.read"] and all(x.get("ok") for x in a) else 1)' \
  && ok "the run's input carries the five reads as recorded_tool_results of its station: what a shadow is served" || bad "input show: $(cli_input "$RID1" | head -c 300)"
# the plan waits: its detail names what each claim rests on, the text is labelled, nothing was sent
PSHOW="$(hcli case show "$Q1" 2>&1)"
echo "$PSHOW" | grep >/dev/null "plumbing.reply" && echo "$PSHOW" | grep >/dev/null "https://docs.e2e.example/d/repair-agents" && echo "$PSHOW" | grep >/dev/null "library: arxiv:2002.00002" \
  && ok "the plan's detail names its sources: the documents link and the paper (a read this run made each)" || bad "the plan's detail: $(echo "$PSHOW" | tail -6)"
[ -z "$(sends reply)" ] && ok "nothing reached the plumbing before the approval" || bad "a reply sent before the approval: $(sends reply)"
# the shadow v2 (v1 and a marker) of knowledge/answer, on the shadow worker, which holds no read server's address, token or route: it is served the live run's record
OTHER_ENV="$(for w in flotilla-worker-triage-shadow flotilla-worker-triage flotilla-worker-ops-read; do $D exec $w env 2>&1; done | grep -c 'MCP_')"
[ "$OTHER_ENV" = 0 ] && ok "no shadow worker, nor the live triage or ops-read worker, holds an MCP_* setting (only the reads worker holds the three servers')" || bad "MCP settings elsewhere: $OTHER_ENV"
pairs_same() {  # the shadow's plan against the live plan, for every plumbing.reply pair: $1 is how many pairs are expected
  hcli pairs knowledge/answer 2 --json 2>&1 | python3 -c '
import json, sys
pl = [x for x in json.load(sys.stdin)["pairs"] if x.get("live") and x.get("shadow") and (x["live"].get("plan") or {}).get("action") == "plumbing.reply"]
sys.exit(0 if len(pl) == int(sys.argv[1]) and all(x["same"] and x["shadow"]["plan"].get("params") == x["live"]["plan"].get("params") for x in pl) else 1)' "$1"
}
for _ in $(seq 1 45); do pairs_same 2 && break; sleep 2; done
pairs_same 2 && ok "the shadow of knowledge/answer (v2) was served each live run's record and made the same plan: both plumbing.reply pairs are the same" \
  || bad "the shadow of knowledge/answer made no matching plans: $(hcli pairs knowledge/answer 2 --json 2>&1 | head -c 1200)"
# now with the servers stopped: a replay of the first question's input on v2's contract (flotillad -stations, then runtime/offline_run.py in the shadow
# worker's image), served the record alone, makes the live run's plan
$D stop e2e-mcp-docs e2e-library >/dev/null 2>&1
[ -z "$($D ps -q --filter name=e2e-mcp-docs --filter name=e2e-library)" ] && ok "the documents, tasks and library servers are stopped" || bad "the read servers are still running"
OFA="$WORK/offline-ask"
ask_off_clean() {  # the replay's folders hold files owned by the worker's uid: removed by that uid, then the folder by us
  [ -d "$OFA" ] && $D run --rm --user 10001:10001 -v "$OFA:/x" --entrypoint sh "local/flotilla-worker-triage:$TAG" -c 'rm -rf /x/out' >/dev/null 2>&1
  rm -rf "$OFA"
}
ask_off_clean; mkdir -p "$OFA"; chmod 777 "$OFA"
printf 'e2e-eval-key' > "$OFA/eval.key"
cat > "$OFA/models.json" <<EOM
{"endpoints": {"gw": {"base_url": "http://litellm:8080/v1", "kind": "litellm", "key_file": "/work/eval.key", "defaults": {"tools": "native", "json": "object", "cost": "reported"}}},
 "aliases": {"knowledge-answer": {"endpoint": "gw", "model": "fleet-knowledge-answer"}}}
EOM
hcli input export "$RID1" --to "/var/lib/flotilla/exports/ask-$RID1" >/dev/null 2>&1 && $D cp "flotillad:/var/lib/flotilla/exports/ask-$RID1" "$OFA/in" >/dev/null 2>&1 && chmod -R a+rX "$OFA/in"
$D run --rm --network none --user 65532:65532 -v "$OFA:/work" -v "$WORK/config/specs:/etc/flotilla/specs:ro" "local/flotilla-flotillad:$TAG" \
  -stations /etc/flotilla/specs/knowledge/answer/v2 -models /work/models.json > "$OFA/contract.json" 2> "$OFA/contract.err"
ASNAP; CJ="$($RC contract "$WORK/ask.db" "$Q1" "$OFA/contract.json")"
$D run --rm --network e2e-litellm --user 10001:10001 -v "$OFA:/work" -v "$WORK/config/specs/knowledge:/opt/flotilla/fleets/knowledge:ro" "local/flotilla-worker-triage:$TAG" \
  python3 /opt/flotilla/runtime/offline_run.py --contract /work/contract.json --input /work/in/input.json --models /work/models.json --out /work/out --forbid /opt/flotilla/fleets > "$OFA/offline.out" 2>&1; ORC=$?
LIVEK="$($D logs flotilla-worker-reads 2>&1 | grep "\"run_id\": \"$RID1\"" | grep -o '"line": "KNOWLEDGE [^"]*"' | sed 's/^"line": "//; s/"$//')"
OFFK="$(grep '^KNOWLEDGE ' "$OFA/out/output.txt" 2>/dev/null)"
[ "${CJ%% *}" = PASS ] && [ "$ORC" = 0 ] && echo "$LIVEK" | grep >/dev/null "^KNOWLEDGE outcome=plan plumbing.reply " && [ "$OFFK" = "$LIVEK" ] \
  && ok "with the three servers stopped, the offline run of v2 on the first question's input was served the record alone and made the live run's plan (the same action and sources)" \
  || bad "offline replay rc=$ORC contract: $CJ live: '$LIVEK' offline: '$OFFK' $(tail -c 400 "$OFA/offline.out") $(tail -c 300 "$OFA/out/output.txt" 2>/dev/null)"
ask_off_clean
# the approvals: the first answer is approved and sent once, signed; the second is rejected and sends nothing
hcli case approve "$Q1" >/dev/null 2>&1 || bad "approve: $(hcli case approve "$Q1" 2>&1)"
for _ in $(seq 1 60); do n8nlog | grep '"event": "status"' | grep '"name": "reply"' | grep >/dev/null '"done": true' && break; sleep 2; done
SEND="$(sends reply)"
[ "$(echo "$SEND" | grep -c .)" = 1 ] && echo "$SEND" | grep >/dev/null "\"case\": \"$Q1\"" && echo "$SEND" | grep >/dev/null '"action": "plumbing.reply"' \
  && echo "$SEND" | grep >/dev/null '"thread": "t-e2e-1"' && echo "$SEND" | grep >/dev/null '"channel": "general"' && echo "$SEND" | grep >/dev/null '"text": "\[flotilla\] ' \
  && echo "$SEND" | grep >/dev/null 'Sources:\\n\[1\] docs: https://docs.e2e.example/d/repair-agents\\n\[2\] library: arxiv:2002.00002' && echo "$SEND" | grep >/dev/null '"delivery": "plan-[0-9a-f]\{16\}"' \
  && ok "the approved answer reached the plumbing once, signed, labelled, in thread t-e2e-1, its sources listed (a docs link and the paper)" || bad "the plumbing's reply sends: $(n8nlog | tail -5)"
waitcase "$Q1" closed && hcli case show "$Q1" 2>&1 | grep >/dev/null "reply-plan-" && ok "the check asked the plumbing for the same delivery, and the case closed with the plumbing's ref on its record" || bad "the case after the send: $(hcli case show "$Q1" 2>&1 | tail -5)"
hcli case reject "$Q2" --reason "e2e: the answer is not wanted" >/dev/null 2>&1 && waitcase "$Q2" escalated \
  && ok "the second question's plan was rejected ($Q2 escalated)" || bad "the rejection: $(hcli case show "$Q2" 2>&1 | tail -4)"
[ "$(sends reply | grep -c .)" = 1 ] && [ -z "$(n8nlog | grep '"event": "send"' | grep -e "$Q2" -e "$Q3")" ] \
  && ok "the rejected answer and the escalated question sent nothing: one reply in all" || bad "sends: $(n8nlog | grep '"event": "send"')"
[ -z "$(n8nlog | grep '"event": "refused"')" ] && ok "the plumbing refused nothing: the reply carried a valid signature" || bad "refused at the plumbing: $(n8nlog | grep refused)"
$D start e2e-mcp-docs e2e-library >/dev/null 2>&1
fi

echo "== hardening"
if want remote; then
echo "== a remote worker (RFC-0011): its link dials the host endpoint, flotillad's network never reaches it"
acli() { $D exec -e FLOTILLA_TOKEN="$ADMIN" flotillad /usr/local/bin/flotilla "$@"; }   # the first admin has retired the operator token
R="$(acli run e2e/remote-echo 2>&1)"
acli runs e2e/remote-echo 2>&1 | grep >/dev/null "skipped.*worker remote-e2e is not connected" \
  && ok "before its link connects, the remote worker's routine is skipped: not connected" || bad "not connected: $R $(acli runs e2e/remote-echo 2>&1 | head -3)"
mkdir -p "$WORK/remote" && tok > "$WORK/remote/worker.token" && chmod 600 "$WORK/remote/worker.token" \
  && cp "$WORK/config/host-ca/root.crt" "$WORK/remote/root.crt"
RT="$(acli host enrolment w-remote-e2e 2>&1 | sed -n 's/.*"token":"\(fle_[0-9a-f]*\)".*/\1/p')"
RW=(--label $LABEL --network ${P}_flotilla --memory 256m --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$WORK/remote:/remote")
$D run -d --name flotilla-e2e-remote-worker "${RW[@]}" -e FLOTILLA_WORKER=remote-e2e -e FLOTILLA_WORKER_TOKEN_FILE=/remote/worker.token \
  -e FLOTILLA_WORKER_LISTEN=0.0.0.0:8081 local/flotilla-worker-bugfix:$TAG >/dev/null || bad "the remote worker did not start"
mkdir -p "$WORK/remote/state" && chmod 700 "$WORK/remote/state"
OUT="$(printf '%s\n' "$RT" | $D run --rm -i "${RW[@]}" --entrypoint /usr/local/bin/flotilla-host local/flotilla-worker-bugfix:$TAG \
  enrol -identity w-remote-e2e -state /remote/state -root /remote/root.crt -addr flotillad:8487 -server-name flotillad 2>&1)"
echo "$OUT" | grep >/dev/null "^enrolled w-remote-e2e" && ok "the link enrolled under the worker's identity ($OUT)" || bad "link enrol: $OUT"
$D run -d --name flotilla-e2e-remote-link "${RW[@]}" --entrypoint /usr/local/bin/flotilla-host local/flotilla-worker-bugfix:$TAG \
  link -identity w-remote-e2e -worker remote-e2e -worker-url http://flotilla-e2e-remote-worker:8081 -worker-token-file /remote/worker.token \
  -state /remote/state -root /remote/root.crt -addr flotillad:8487 -server-name flotillad -heartbeat 2s >/dev/null || bad "the link did not start"
seen flotillad '"msg":"remote worker connected".*"worker":"remote-e2e"' \
  && ok "flotillad authenticated the link as remote-e2e's (identity w-remote-e2e, version $TAG)" || bad "link connect: $($D logs flotilla-e2e-remote-link 2>&1 | tail -3)"
for _ in $(seq 1 10); do acli host list 2>&1 | grep >/dev/null "^w-remote-e2e  *yes  *$TAG " && break; sleep 1; done
# run_tokens: required, on the remote worker: the run's token travels in the request through the link, the worker writes it to its own file, the
# worker's own token is refused by the listener (the stack's remote worker can reach it; a real one is on another host and does not)
acli run e2e/token-probe-remote-e2e >/dev/null 2>&1
for _ in $(seq 1 30); do acli runs e2e/token-probe-remote-e2e 2>&1 | sed -n 2p | grep >/dev/null -E " (ok|error|timeout) " && break; sleep 1; done
acli runs e2e/token-probe-remote-e2e 2>&1 | sed -n 2p | grep >/dev/null " ok " \
  && ok "run tokens on the remote worker: the run's token came through the link into its own file, the worker's own token is refused, the run's accepted" \
  || bad "token-probe on the remote worker: $(acli runs e2e/token-probe-remote-e2e 2>&1 | head -2) $($D logs flotilla-e2e-remote-worker 2>&1 | grep -o 'PROBE [^"]*' | tail -2)"
acli host list 2>&1 | grep >/dev/null "^w-remote-e2e  *yes  *$TAG  *[0-9-]*T[0-9:.]*Z" \
  && ok "host list shows the link connected, its version and last contact" || bad "host list for the link: $(acli host list 2>&1)"
! $D logs flotillad 2>&1 | grep '"msg":"host alert".*w-remote-e2e' >/dev/null && ok "no not_connected alert for the connected link" \
  || bad "a host alert for the link: $($D logs flotillad 2>&1 | grep '"host alert' | tail -2)"
acli run e2e/remote-echo >/dev/null
for _ in $(seq 1 20); do acli runs e2e/remote-echo 2>&1 | grep >/dev/null " ok " && break; sleep 1; done
acli runs e2e/remote-echo 2>&1 | grep >/dev/null " ok " && $D logs flotilla-e2e-remote-link 2>&1 | grep '"msg":"run ended"' >/dev/null \
  && ok "a run went to the remote worker through its link and came back ok" || bad "remote run: $(acli runs e2e/remote-echo 2>&1 | head -3) $($D logs flotilla-e2e-remote-link 2>&1 | tail -3)"
$D rm -f flotilla-e2e-remote-link >/dev/null
for _ in $(seq 1 15); do $D logs flotillad 2>&1 | grep '"msg":"remote worker disconnected".*"worker":"remote-e2e"' >/dev/null && break; sleep 1; done
acli run e2e/remote-echo >/dev/null 2>&1
# The newest row, read with sed so the whole listing is consumed: `| head -2` under pipefail can fail
# the pipeline by SIGPIPE although the row matched (c93ddc7 and 613903a, the row there 0.1 s before the
# check failed). The loop also waits for the run to be recorded after `run` returns.
newest_remote_run() { acli runs e2e/remote-echo 2>&1 | sed -n 2p; }
for _ in $(seq 1 10); do newest_remote_run | grep >/dev/null "skipped.*not connected" && break; sleep 1; done
newest_remote_run | grep >/dev/null "skipped.*not connected" \
  && ok "with its link gone, the remote worker's routine is skipped again" || bad "after the link: $(acli runs e2e/remote-echo 2>&1 | head -3)"
# an old agent (this tree's, before it listed run_token in its hello): under required its link is refused, with the way out, and the stack runs on
$D run -d --name flotilla-e2e-remote-link-old "${RW[@]}" --entrypoint /usr/local/bin/flotilla-host local/flotilla-worker-bugfix-old:$TAG \
  link -identity w-remote-e2e -worker remote-e2e -worker-url http://flotilla-e2e-remote-worker:8081 -worker-token-file /remote/worker.token \
  -state /remote/state -root /remote/root.crt -addr flotillad:8487 -server-name flotillad -heartbeat 2s >/dev/null || bad "the old agent's link did not start"
seen flotillad '"msg":"remote worker refused: its agent does not carry run tokens"' \
  && ok "run_tokens: required refused the link of an agent whose hello lists no run_token capability, logged by flotillad" || bad "old agent: $($D logs flotillad 2>&1 | grep -i 'remote worker' | tail -2)"
seen flotilla-e2e-remote-link-old 'the agent does not carry run tokens' \
  && ok "the old agent was told why and what to do: update the agent to this build" || bad "old agent's message: $($D logs flotilla-e2e-remote-link-old 2>&1 | tail -3)"
acli runs e2e/remote-echo 2>&1 >/dev/null; acli run e2e/remote-echo >/dev/null 2>&1
for _ in $(seq 1 10); do newest_remote_run | grep >/dev/null "skipped.*not connected" && break; sleep 1; done
newest_remote_run | grep >/dev/null "skipped.*not connected" && [ "$(acli health 2>&1 | head -c 2)" != "" ] && acli status 2>&1 | grep >/dev/null "^platform/heartbeat" \
  && ok "the refused link is not connected, its routine is skipped, and the stack still answers" || bad "after the old agent: $(acli runs e2e/remote-echo 2>&1 | head -2)"
$D rm -f flotilla-e2e-remote-link-old >/dev/null
$D rm -f flotilla-e2e-remote-worker >/dev/null

fi
if want hardening; then
for w in flotilla-worker-ops-read flotilla-worker-ops-act-host flotilla-worker-bugfix flotilla-worker-bugfix-act flotilla-worker-reads; do
  NW="$($D inspect $w --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
  [ "$NW" = "${P}_flotilla " ] && ok "$w is on the internal network only" || bad "$w networks: '$NW'"
done
for c in flotillad flotilla-worker-triage flotilla-worker-triage-candidate flotilla-worker-triage-shadow flotilla-egress-shadow flotilla-worker-ops-read flotilla-worker-ops-act-host flotilla-worker-bugfix flotilla-worker-bugfix-act flotilla-worker-reads flotilla-egress flotilla-egress-bugfix flotilla-egress-reads; do
  RO=$($D inspect "$c" --format '{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.Config.User}}')
  case "$RO" in "true [ALL] "*) ok "$c: read-only root, all capabilities dropped, user ${RO##* }" ;; *) bad "$c hardening: $RO" ;; esac
done
NW="$($D inspect flotilla-worker-triage --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
[ "$NW" = "${P}_flotilla " ] && ok "flotilla-worker-triage is on the internal network only" || bad "flotilla-worker-triage networks: '$NW'"
NW="$($D inspect flotillad --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
[ "$NW" = "e2e-plumbing e2e-ui ${P}_flotilla ${P}_shadow " ] && ok "flotillad is on the internal flotilla, shadow and plumbing networks and the UI network only" || bad "flotillad networks: '$NW'"
NW="$($D network inspect e2e-ui --format '{{.Internal}}' 2>&1)"
[ "$NW" = true ] && ok "the UI network is internal (no route out)" || bad "ui network internal=$NW"
NW="$($D network inspect ${P}_flotilla --format '{{.Internal}}' 2>&1)"
[ "$NW" = true ] && ok "the flotilla network is internal (no route out)" || bad "flotilla network internal=$NW"
NW="$($D network inspect ${P}_shadow --format '{{.Internal}}' 2>&1)"
[ "$NW" = true ] && ok "the shadow network is internal (no route out)" || bad "shadow network internal=$NW"
NW="$($D network inspect e2e-plumbing --format '{{.Internal}}' 2>&1)"
[ "$NW" = true ] && ok "the plumbing network is internal (no route out)" || bad "plumbing network internal=$NW"

fi
if [ "${KEEP:-}" != 1 ]; then down; fi
if [ "$fails" -eq 0 ]; then echo "E2E PASS"; else echo "E2E FAIL ($fails)"; exit 1; fi
