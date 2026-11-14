#!/usr/bin/env bash
# End to end: the real compose file and images on the dev Docker engine, with stand-in services.
#
#   deploy/e2e/run.sh            build the images, bring the stack up, check, tear down
#   KEEP=1 deploy/e2e/run.sh     leave it up to look around (tear down: deploy/e2e/run.sh down)
#
# Checks: the heartbeat runs through the worker and the proxy and passes; a host off the allowlist
# is refused by the proxy; metrics and traces reach their stand-ins; an agent token cannot change a routine's
# autonomy; a STOPped fleet's run is recorded as skipped; the worker refuses a wrong token; the candidate
# worker (profile candidate) is healthy on its own state volume and its routine cannot leave shadow; the
# heartbeat's model step calls the LiteLLM stand-in through the model client, with the models section
# from flotillad.yaml, the key from the fleet's secrets file and the proxy, and its span is exported. The UI
# in Chromium behind a Caddy TLS front (ui_browser.py): sign-in, the session cookie, a pause and a resume
# from the fleet page, phone width, the fonts, no CSP violations, sign-out. The host agent (ADR-0006): a CA
# made with `flotilla ca`, enrolment, connect and heartbeat, a flotillad restart, revocation.
# Never on the production engine (homelab ADR-0009): it uses the `dev` context only.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
D="docker --context ${DOCKER_CONTEXT_DEV:-dev}"
P=flotilla-e2e
TAG=e2e-local
LABEL=flotilla.e2e=1
NETS="e2e-backend e2e-litellm e2e-glitchtip e2e-langfuse"
PLAYWRIGHT_IMAGE=local/flotilla-e2e-browser:$TAG
# The browser check's TLS front, standing in for the homelab's caddy-tailscale (Caddy 2.11.9).
CADDY_IMAGE=caddy@sha256:003a97808e781aa9f7e75cc2f43d34d461099031faf5e20faefb9f61126836f0
WORK="${E2E_WORK:-$ROOT/.e2e}"
fails=0
ok() { echo "PASS $*"; }
bad() { echo "FAIL $*"; fails=$((fails + 1)); }

E2E_ENV=(FLOTILLA_REGISTRY=local FLOTILLA_TAG="$TAG" FLOTILLA_CONFIG="$WORK/config" TRIAGE_ENV_FILE="$WORK/config/fleet.env"
  FLOTILLA_VOLUME_PREFIX=flotilla-e2e FLOTILLA_STATE_VOLUME=flotilla-e2e-state TRIAGE_STATE_VOLUME=flotilla-e2e-triage-state
  COMPOSE_PROFILES=candidate,ops TRIAGE_CANDIDATE_STATE_VOLUME=flotilla-e2e-triage-candidate-state
  NET_BACKEND=e2e-backend NET_LITELLM=e2e-litellm NET_GLITCHTIP=e2e-glitchtip NET_LANGFUSE=e2e-langfuse NET_UI=e2e-ui
  DOCKER_CONTEXT="${DOCKER_CONTEXT_DEV:-dev}")
compose() { env "${E2E_ENV[@]}" $D compose -p $P -f "$ROOT/deploy/compose/docker-compose.yml" -f "$ROOT/deploy/e2e/docker-compose.e2e.yml" "$@"; }

down() {
  compose down -v --remove-orphans >/dev/null 2>&1
  for c in $($D ps -aq --filter label=$LABEL); do $D rm -f "$c" >/dev/null; done
  for v in flotilla-e2e-state flotilla-e2e-triage-state flotilla-e2e-triage-candidate-state flotilla-e2e-flotillad-secrets flotilla-e2e-worker-triage-secrets flotilla-e2e-worker-ops-read-secrets flotilla-e2e-worker-ops-act-host-secrets flotilla-e2e-host-state flotilla-e2e-host-audit; do
    $D volume rm "$v" >/dev/null 2>&1
  done
  for n in $NETS; do $D network rm "$n" >/dev/null 2>&1; done
  rm -rf "$WORK"
}
if [ "${1:-}" = down ]; then down; echo "e2e torn down"; exit 0; fi
down

echo "== build"
for i in flotillad worker-triage worker-ops smokescreen; do
  $D build -q -f "$ROOT/deploy/docker/$i.Dockerfile" --build-arg VERSION=$TAG -t local/flotilla-$i:$TAG "$ROOT" >/dev/null \
    || { echo "BUILD FAIL $i"; exit 1; }
done
HOST_AGENT_IMAGE=local/flotilla-e2e-host-agent:$TAG
$D build -q -f "$ROOT/deploy/e2e/host-agent.Dockerfile" --build-arg VERSION=$TAG -t "$HOST_AGENT_IMAGE" "$ROOT" >/dev/null \
  || { echo "BUILD FAIL e2e-host-agent"; exit 1; }
$D build -q -f "$ROOT/deploy/e2e/browser.Dockerfile" -t "$PLAYWRIGHT_IMAGE" "$ROOT/deploy/e2e" >/dev/null \
  || { echo "BUILD FAIL e2e-browser"; exit 1; }
ok "images built"

echo "== config and stand-ins"
mkdir -p "$WORK/config"
# the specs: the shipped fleets and the e2e-only fleet (its host.true path, H9c); its registry extension
mkdir -p "$WORK/config/specs" && cp -r "$ROOT/fleets/." "$WORK/config/specs/" && cp -r "$ROOT/deploy/e2e/fleets/." "$WORK/config/specs/"
cp "$ROOT/deploy/e2e/registry-e2e.json" "$WORK/config/registry-e2e.json"
printf '%s\n' "${E2E_ENV[@]}" > "$WORK/.env"
tok() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
OP=$(tok); AGENT=$(tok); WT=$(tok)
hash() { printf %s "$1" | sha256sum | cut -d' ' -f1; }
printf %s "$WT" > "$WORK/config/worker-triage.token"
tok > "$WORK/config/worker-ops-read.token"; tok > "$WORK/config/worker-ops-act-host.token"
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
ui_host: flotilla.e2e
specs_dir: /etc/flotilla/specs          # the image's fleets plus the e2e fleet (deploy/e2e/fleets), staged below
registry_files: [/opt/flotilla/host/registry.json, /etc/flotilla/registry-e2e.json]   # plus the e2e's host.true
state_dir: /var/lib/flotilla
environment: e2e
timezone: Europe/Amsterdam
vm_url: http://victoriametrics:8080
otlp_traces_url: http://victoriatraces:8080/insert/opentelemetry/v1/traces
callers_file: /etc/flotilla/callers.yaml
workers:
  triage: {url: "http://worker-triage:8081", token_file: /run/flotilla/worker-triage.token}
  triage-candidate: {url: "http://worker-triage-candidate:8081", token_file: /run/flotilla/worker-triage.token}
  ops-read: {url: "http://worker-ops-read:8081", token_file: /run/flotilla/worker-ops-read.token}
  ops-act-host: {url: "http://worker-ops-act-host:8081", token_file: /run/flotilla/worker-ops-act-host.token}
disabled_routines: [triage/cycle, triage/cycle-shadow]
models:
  endpoints:
    gateway: {base_url: "http://litellm:8080/v1", kind: litellm, key_env: E2E_LLM_KEY, defaults: {tools: native, json: object, cost: reported}}
  aliases:
    e2e-model: {endpoint: gateway, model: fleet-triage-flash}
hosts: {listen: "0.0.0.0:8487", ca_dir: /run/flotilla, server_names: [flotillad], heartbeat: 2s, signing_key_file: /run/flotilla/signing.key, signing_key_id: e2e1}
EOF
echo "E2E_ONLY=1" > "$WORK/config/fleet.env"
cat > "$WORK/config/worker-triage.env" <<EOF
ROUTINE_HEARTBEAT_ALLOWED_URL=http://litellm:8080/health/liveliness
ROUTINE_HEARTBEAT_DENIED_URL=http://forbidden:8080/
ROUTINE_HEARTBEAT_MODEL_ALIAS=e2e-model
ROUTINE_SF_SERVICE=triage-fleet
ROUTINE_SF_GLITCHTIP_SYNC=1
EOF
# the ops workers' installation facts; the VictoriaMetrics stand-in answers the backup's age (20.5h)
cat > "$WORK/config/worker-ops.env" <<EOF
ROUTINE_VM_URL=http://victoriametrics:8080
ROUTINE_BACKUP_INSTANCE=homelab
ROUTINE_BACKUP_TARGET=host:mini
EOF
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
  allowed_domains: [victoriametrics, victoriatraces, litellm]
EOF
find "$WORK/config" -maxdepth 1 -type f -exec chmod 644 {} +   # the files; the specs folder stays traversable
chmod -R a+rX "$WORK/config/specs"
for n in $NETS; do $D network create --label $LABEL "$n" >/dev/null; done
stub() { $D run -d --label $LABEL --name "e2e-$1" --network "$2" --network-alias "$1" --memory 64m \
  -v "$ROOT/deploy/e2e/stub.py:/stub.py:ro" python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b \
  python3 -u /stub.py "$1" >/dev/null; }
stub victoriametrics e2e-backend; stub victoriatraces e2e-backend; stub litellm e2e-litellm
# off the allowlist but reachable and answering: refusing it is the proxy's doing, nothing else
stub forbidden e2e-backend
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
echo "$SK" | grep -q '^  "e2e1": "[A-Za-z0-9+/]*=*"$' && [ -s "$WORK/config/host-ca/signing.key" ] \
  && ok "the execution-signing key made; its public line printed for the hosts' configs" || bad "signing-key: $SK"
SIGNPUB="$(echo "$SK" | sed -n 's/^  "e2e1": "\(.*\)"$/\1/p')"

echo "== secrets and seeded state (deploy/mini/secrets.sh, as a deploy runs it)"
printf 'SEED_SECRET=must-not-be-copied\nE2E_LLM_KEY=%s\n' "$(tok)" > "$WORK/config/fleet.env"   # a random stand-in key, made per run
mkdir -p "$WORK/seed/results"; printf 'ts\tfingerprint\n' > "$WORK/seed/results/dispositions.tsv"
cp "$WORK/config/fleet.env" "$WORK/seed/fleet-gateway.env"; touch "$WORK/seed/STOP"
# the mini's folder also holds backups of the secrets file: they must stay out too
cp "$WORK/config/fleet.env" "$WORK/seed/fleet-gateway.env.bak-20261027"
cp "$WORK/config/fleet.env" "$WORK/seed/results/fleet.env.bak-presync"
if FLOTILLA_HOME="$WORK" bash "$ROOT/deploy/mini/secrets.sh" $TAG >"$WORK/secrets.log" 2>&1 \
   && FLOTILLA_HOME="$WORK" bash "$ROOT/deploy/mini/secrets.sh" $TAG --seed-state flotilla-e2e-triage-state "$WORK/seed" >>"$WORK/secrets.log" 2>&1 \
   && FLOTILLA_HOME="$WORK" bash "$ROOT/deploy/mini/secrets.sh" $TAG --seed-state flotilla-e2e-triage-candidate-state "$WORK/seed" >>"$WORK/secrets.log" 2>&1; then
  ok "secrets and state seeded ($(grep -c ' <- ' "$WORK/secrets.log") secret files)"
else
  cat "$WORK/secrets.log"; bad "secrets.sh"
fi
FLOTILLA_HOME="$WORK" bash "$ROOT/deploy/mini/secrets.sh" $TAG --seed-state flotilla-e2e-triage-state "$WORK/seed" 2>&1 | grep -q "SEED SKIP" \
  && ok "a seeded state volume is never overwritten" || bad "seeding twice did not skip"

echo "== an old, real-sized store for flotillad (schema step 1, 5000 runs)"
# flotillad migrates it at start, in its read-only container (deploy/e2e/old_store.py says why)
python3 "$ROOT/deploy/e2e/old_store.py" make "$WORK/oldstore/flotilla.db" 5000
$D run --rm --user 0 -v flotilla-e2e-state:/v -v "$WORK/oldstore:/in:ro" --entrypoint sh "local/flotilla-worker-triage:$TAG" \
  -c 'cp /in/flotilla.db /v/flotilla.db && chown -R 65532:65532 /v' && ok "an old store seeded" || bad "seeding the old store"

echo "== up"
if ! compose up -d >"$WORK/up.log" 2>&1; then
  cat "$WORK/up.log"; bad "compose up"; [ "${KEEP:-}" = 1 ] || down; echo "E2E FAIL ($fails)"; exit 1
fi
cli() { $D exec flotillad /usr/local/bin/flotilla "$@"; }
for i in $(seq 1 60); do
  cli runs platform/heartbeat 2>/dev/null | grep -q -E " (ok|error|timeout) " && break; sleep 2
done
mkdir -p "$WORK/storecopy" && $D cp flotillad:/var/lib/flotilla "$WORK/storecopy/" 2>/dev/null
STORE="$(python3 "$ROOT/deploy/e2e/old_store.py" check "$WORK/storecopy/flotilla/flotilla.db")"
echo "$STORE" | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.exit(0 if "error" not in d and d["schema"] == d["latest"] and d["old_runs"] == 5000 and d["old_auto"] == 2500 and d["live_left"] == 0 and d["stage_controls"] == 0 and d["heartbeat_autonomy"] == "auto" else 1)' \
  && ok "flotillad migrated the old store in its read-only container ($STORE)" || bad "the old store's migration: $STORE"
OUT="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"
echo "$OUT" | grep -q "^action backup.rerun_observability$" && echo "$OUT" | grep -q "^action host.true$" && echo "$OUT" | grep -q "^action host.absent$" \
  && echo "$OUT" | grep -q "^CHECK PASS.*, 3 actions$" \
  && ok "-check reads the registry the image carries and the e2e's extension (backup.rerun_observability, host.true, host.absent)" || bad "-check: $OUT"
RUNS="$(cli runs platform/heartbeat 2>&1)"; echo "$RUNS"
echo "$RUNS" | grep -q " ok " && ok "heartbeat ran through the worker and passed" || bad "heartbeat did not pass"
# Docker's log driver hands lines to `docker logs` asynchronously, so the line can lag the run's result.
for i in $(seq 1 15); do $D logs flotilla-worker-triage 2>&1 | grep -q '"line": "HEARTBEAT PASS"' && break; sleep 1; done
$D logs flotilla-worker-triage 2>&1 | grep -q '"line": "HEARTBEAT PASS"' && ok "the heartbeat saw allowed-in and denied-out" \
  || bad "no HEARTBEAT PASS in the worker's log (waited 15s)"
# the proxy writes its log line just after it refuses: wait for it rather than read once (a race, 2026-11-11)
seen() { for _ in $(seq 1 30); do $D logs "$1" 2>&1 | grep -q "$2" && return 0; sleep 1; done; return 1; }
seen flotilla-egress "forbidden" && ok "the proxy logged the refused host" || bad "the proxy has no record of the refused host"
[ -z "$($D logs e2e-forbidden 2>&1)" ] && ok "the refused host never received a request" || bad "the refused host was reached: $($D logs e2e-forbidden 2>&1 | head -2)"
cli status | grep -E "^platform/heartbeat" | grep -q -E " false " && ok "status: heartbeat not stale" || bad "status: heartbeat stale"

echo "== the model client (runtime/), from the heartbeat's model step"
L="$($D logs flotilla-worker-triage 2>&1)"
echo "$L" | grep -q 'ok   model   e2e-model: model=fleet-triage-flash deployment=e2e-deployment cost=2.1e-06 (reported:body) usage_reported=True traced=True' \
  && ok "a model call through the client: alias, deployment id, reported cost, traced" \
  || bad "no model step line: $(echo "$L" | grep -o 'model   e2e-model[^"]*' | head -2)"
$D logs e2e-litellm 2>&1 | grep -q "CHAT auth=bearer model=fleet-triage-flash" && ok "the endpoint got the key from the fleet's secrets file and the alias's model" \
  || bad "the LiteLLM stand-in saw no authorised chat call"
T="$($D exec flotilla-worker-triage python3 /opt/flotilla/runtime/test_telemetry.py 2>&1)"
# with the SDK, the no-SDK test is the one that skips; the SDK test must have run and passed
if echo "$T" | grep -q "test_model_call_span_under_the_run .* ok$" && echo "$T" | tail -1 | grep -q "^OK"; then
  ok "runtime/test_telemetry.py in the image: the model call's span under the run's trace"
else
  bad "telemetry test in the image: $(echo "$T" | tail -4 | tr '\n' ' ')"
fi
$D exec flotilla-worker-triage python3 -m pip check >/dev/null 2>&1 && ok "the image's Python packages are consistent (pip check)" || bad "pip check failed in the worker image"

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

echo "== cases: pipeline v1's chain (RFC-0004 §10), the ops workers, in shadow"
# The monitor runs at start; the stand-in says the backup is 20.5h old, above shadow's 20h. The dispatcher
# (every 15s) hands the case to ops/rerun-backup, whose plan policy records in shadow, closing the case.
for i in $(seq 1 45); do
  CASES="$(cli case list --all 2>&1)"
  echo "$CASES" | grep -q "failed_backup .*host:mini .*closed .*shadow_plan_recorded" && break; sleep 2
done
echo "$CASES" | grep -q "failed_backup .*host:mini .*closed .*shadow_plan_recorded" \
  && ok "the backup monitor's detection became a case, planned and closed as a shadow plan" || bad "the chain: $CASES"
CID="$(echo "$CASES" | awk '/failed_backup/ {print $1; exit}')"
SHOW="$(cli case show "$CID" 2>&1)"
echo "$SHOW" | grep -q "case.handed_over .*ops/backup-monitor .*by detector" && ok "opened by ops/backup-monitor, handed over as a code detector" || bad "case show (detector): $SHOW"
echo "$SHOW" | grep -q "case.planned .*ops/rerun-backup .*by act .*backup_age_above(20h): holds" \
  && ok "planned by ops/rerun-backup, citing its re-check" || bad "case show (plan): $SHOW"
echo "$SHOW" | grep -q "case.closed .*by policy .*shadow_plan_recorded.*would propose" \
  && ok "policy recorded it in shadow, and what it would have done (propose: a new host's ceiling)" || bad "case show (policy): $SHOW"
$D logs e2e-victoriametrics 2>&1 | grep -q 'QUERY .*instance%3D%22homelab%22' \
  && ok "the ops workers queried VictoriaMetrics through the proxy" || bad "no backup query at the VictoriaMetrics stand-in"

echo "== controls and auth"
OUT="$($D exec -e FLOTILLA_TOKEN="$AGENT" flotillad /usr/local/bin/flotilla autonomy platform/heartbeat shadow 2>&1)"
echo "$OUT" | grep -q "HTTP 403" && ok "an agent cannot change a routine's autonomy ($OUT)" || bad "agent autonomy change: $OUT"
cli stop platform >/dev/null && cli run platform/heartbeat >/dev/null
sleep 3
cli runs platform/heartbeat -n 1 | grep -q " skipped " && ok "a STOPped fleet's run is recorded as skipped" || bad "STOP did not skip"
cli start platform --reason "e2e: start after the STOP check" >/dev/null
cli events -n 5 | grep -q "control.stop" && ok "the event log records the STOP" || bad "no control.stop event"
# The freeze (ADR-0014 decision 8), through the real scheduler and worker: the heartbeat's own autonomy is auto.
agent() { $D exec -e FLOTILLA_TOKEN="$AGENT" flotillad /usr/local/bin/flotilla "$@"; }
OUT="$(agent autonomy freeze 2>&1)"
echo "$OUT" | grep -q '"frozen":"1"' && ok "an agent pulls the freeze" || bad "agent freeze: $OUT"
cli status | grep -q "^FROZEN since .* by e2e-agent" && ok "status says frozen, and who froze it" || bad "status while frozen: $(cli status 2>&1 | head -3)"
N0=$(cli runs platform/heartbeat -n 500 | wc -l)
cli run platform/heartbeat >/dev/null
for i in $(seq 1 30); do
  L="$(cli runs platform/heartbeat -n 500)"
  [ "$(echo "$L" | wc -l)" -gt "$N0" ] && ! echo "$L" | sed -n 2p | grep -q " running " && break; sleep 1
done
echo "$L" | sed -n 2p | grep -q " shadow " && ok "frozen, a heartbeat run (set auto) ran at shadow" || bad "frozen run: $(echo "$L" | sed -n 2p)"
OUT="$(agent autonomy unfreeze --reason "e2e" 2>&1)"
echo "$OUT" | grep -q "HTTP 403" && ok "an agent cannot lift the freeze" || bad "agent unfreeze: $OUT"
OUT="$(cli autonomy unfreeze 2>&1)"
echo "$OUT" | grep -q "needs a reason" && ok "lifting the freeze needs a reason" || bad "unfreeze without a reason: $OUT"
cli autonomy unfreeze --reason "e2e: lift the freeze" >/dev/null
OUT="$(cli status 2>&1)"
! echo "$OUT" | grep -q "^FROZEN" && echo "$OUT" | grep -q "^platform/heartbeat  *auto  *auto " \
  && ok "lifted, the heartbeat is back at auto (its own control untouched)" || bad "after unfreeze: $OUT"
# Holds (RFC-0004 §5.1 step 4) through the CLI: an agent holds a host, only an operator lifts it, with a reason.
OUT="$(agent hold host:mini 2h --reason "e2e: disk work" 2>&1)"
HID="$(echo "$OUT" | sed -n 's/.*"id":"\([0-9]*\)".*/\1/p')"
[ -n "$HID" ] && cli hold list | grep -q "host:mini .*e2e-agent .*e2e: disk work" && ok "an agent holds a host; hold list shows it" || bad "hold: $OUT / $(cli hold list 2>&1)"
OUT="$(agent hold lift "$HID" --reason "e2e" 2>&1)"
echo "$OUT" | grep -q "HTTP 403" && ok "an agent cannot lift a hold" || bad "agent lift: $OUT"
OUT="$(cli hold lift "$HID" 2>&1)"
echo "$OUT" | grep -q "needs a reason" && ok "lifting a hold needs a reason" || bad "lift without a reason: $OUT"
cli hold lift "$HID" --reason "e2e: disk done" >/dev/null
[ -z "$(cli hold list | sed 1d)" ] && ok "lifted, no hold is active" || bad "after lift: $(cli hold list 2>&1)"
# The deploy stand-down (deploy/mini/standdown.sh, as deploy.sh and the homelab's pre-pull hook run it):
# a hold by the deploy caller, its token read from flotillad's secrets volume; it never fails its caller.
OUT="$(DOCKER="$D" bash "$ROOT/deploy/mini/standdown.sh" mini 5m "e2e deploy" 2>&1)"; RC=$?
[ $RC = 0 ] && echo "$OUT" | grep -q "^STANDDOWN host:mini for 5m" && cli hold list | grep -q "host:mini .*deploy .*deploy stand-down: e2e deploy" \
  && ok "a deploy stand-down is a hold on the host, taken by the deploy caller" || bad "stand-down: rc=$RC $OUT / $(cli hold list 2>&1)"
OUT="$(DOCKER=false bash "$ROOT/deploy/mini/standdown.sh" mini 5m "e2e" 2>&1)"; RC=$?
[ $RC = 0 ] && echo "$OUT" | grep -q "^STANDDOWN SKIPPED" && ok "a stand-down that cannot be taken never fails the deploy" || bad "stand-down failure path: rc=$RC $OUT"
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
[ "$SK" = "219b34bf 1" ] && ok "the worker image has triage's playbook and hint skills (PROMPT_SHA 219b34bf, the hint loads)" \
  || bad "triage in the worker image: '$SK'"

echo "== the ledger (RFC-0002 §9 step 5): a worker's routine on flotillad's workers' listener"
LR="$($D exec -w /opt/flotilla/signal-fleet/mvp -e FLOTILLA_API_URL=http://flotillad:8485 \
  -e FLOTILLA_API_TOKEN_FILE=/run/flotilla/worker.token -e NO_PROXY=flotillad -e SF_OBSERV_DISABLED=1 \
  flotilla-worker-triage python3 gate_ledger_backend_replay.py --synthetic 300 2>&1)"
echo "$LR" | grep -q "LEDGER BACKEND REPLAY: PASS" \
  && ok "dedup through the ledger equals filed.tsv: $(echo "$LR" | grep '^lookup:' | cut -c1-90)" \
  || bad "ledger replay: $(echo "$LR" | tail -4 | tr '\n' ' ')"
WL="$($D exec flotilla-worker-triage python3 -c 'import urllib.request,urllib.error
try:
    urllib.request.urlopen("http://flotillad:8485/v1/status", timeout=5); print("served")
except urllib.error.HTTPError as e: print(e.code)' 2>&1 | tail -1)"
[ "$WL" = 404 ] && ok "the workers' listener does not serve the full API (/v1/status: 404)" || bad "workers' listener /v1/status: $WL"

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
OUT="$(cli autonomy triage/cycle-shadow propose 2>&1)"
echo "$OUT" | grep -q "capped at shadow" && ok "triage/cycle-shadow cannot leave shadow, even for the operator ($OUT)" \
  || bad "autonomy on the capped routine: $OUT"
OUT="$(cli status 2>&1)"
echo "$OUT" | grep -q 'worker triage-candidate: {"ok":true' && ok "flotillad reaches the candidate worker" || bad "status: $OUT"
NW="$($D inspect flotilla-worker-triage-candidate --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
[ "$NW" = "${P}_flotilla " ] && ok "the candidate is on the internal network only" || bad "candidate networks: '$NW'"

echo "== a changed config recreates its service (deploy.sh's FLOTILLA_CONFIG_HASH_*)"
started() { $D inspect "$1" --format '{{.State.StartedAt}}' 2>&1; }
F0="$(started flotillad)"; E0="$(started flotilla-egress)"; W0="$(started flotilla-worker-triage)"
FLOTILLA_CONFIG_HASH_FLOTILLAD=changed-in-e2e compose up -d >"$WORK/up2.log" 2>&1
for i in $(seq 1 30); do cli health >/dev/null 2>&1 && break; sleep 2; done
[ "$(started flotillad)" != "$F0" ] && ok "a new flotillad config hash recreates flotillad" || bad "flotillad not recreated: $(cat "$WORK/up2.log")"
[ "$(started flotilla-egress)" = "$E0" ] && [ "$(started flotilla-worker-triage)" = "$W0" ] \
  && ok "the proxy and the worker, whose config did not change, keep running" || bad "other services were recreated too"
# The recreated flotillad runs its monitor again at start; the backup is still old: one case per incident
# (ADR-0005 decision 1, amended 2026-11-13), the repeat absorbed into the closed shadow case.
for i in $(seq 1 20); do
  CID="$(agent case list --all 2>&1 | awk '/failed_backup/ {print $1}')"
  agent case show $CID 2>/dev/null | grep -q "signal.absorbed" && break; sleep 2
done
N="$(agent case list --all 2>&1 | grep -c failed_backup)"
[ "$N" = 1 ] && agent case show $CID 2>&1 | grep -q "signal.absorbed .*ops/backup-monitor" \
  && ok "after the restart the monitor's repeat went into the incident's case (one case, signal.absorbed)" \
  || bad "one case per incident: $N failed_backup cases; $(agent case show $CID 2>&1 | tail -3)"

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
hx() { $D exec "$@"; }
hostlog() { $D exec flotilla-e2e-host cat /var/log/flotilla-host/agent.log 2>/dev/null; }
hseen() { for _ in $(seq 1 30); do hostlog | grep -q "$1" && return 0; sleep 1; done; return 1; }
HT="$(cli host enrolment e2e-host 2>&1 | sed -n 's/.*"token":"\(fle_[0-9a-f]*\)".*/\1/p')"
[ ${#HT} = 68 ] && ok "an operator minted an enrolment token" || bad "host enrolment: $(cli host enrolment e2e-host 2>&1)"
# stage as install.sh does: the files, the binary, the token (mode 600)
hx flotilla-e2e-host sh -c 'mkdir -m 700 /stage && cp /stage-src/* /stage/ && cp /opt/flotilla-host /stage/' \
  && cp "$ROOT/deploy/host/remote-install.sh" "$WORK/host-remote-install.sh" && $D cp "$WORK/host-remote-install.sh" flotilla-e2e-host:/stage/remote-install.sh >/dev/null \
  && printf '%s\n' "$HT" | hx -i flotilla-e2e-host sh -c 'umask 077 && cat > /stage/token' || bad "staging"
OUT="$(hx flotilla-e2e-host bash /stage/remote-install.sh /stage install --no-service 2>&1)"; echo "$OUT"
echo "$OUT" | grep -q "^INSTALL PASS" && echo "$OUT" | grep -q "sudoers: 1 line(s) in /etc/sudoers.d/flotilla-host, checked with visudo" \
  && ok "remote-install.sh: user, files, sudoers (checked with visudo), enrolled" || bad "install: $OUT"
hx flotilla-e2e-host test ! -e /stage/token && ok "the token is gone from the staging folder" || bad "the token was left on the host"
M="$(hx flotilla-e2e-host sh -c 'stat -c "%n %a %U" /usr/local/libexec/flotilla-host/flotilla-host /etc/flotilla-host/config.json /etc/flotilla-host/root.crt /var/db/flotilla-host /var/db/flotilla-host/key.pem /var/log/flotilla-host /var/log/flotilla-host/audit.jsonl /etc/sudoers.d/flotilla-host' 2>&1 | tr '\n' ';')"
[ "$M" = "/usr/local/libexec/flotilla-host/flotilla-host 755 root;/etc/flotilla-host/config.json 644 root;/etc/flotilla-host/root.crt 644 root;/var/db/flotilla-host 700 _flotilla;/var/db/flotilla-host/key.pem 600 _flotilla;/var/log/flotilla-host 755 root;/var/log/flotilla-host/audit.jsonl 600 _flotilla;/etc/sudoers.d/flotilla-host 440 root;" ] \
  && ok "owners and modes: binary and config root's, state and key the agent's (700/600), audit log the agent's in a root-owned directory, sudoers 440" || bad "owners and modes: $M"
S="$(hx flotilla-e2e-host grep -v '^#' /etc/sudoers.d/flotilla-host 2>&1)"   # the rules, below the generated header
[ "$S" = '_flotilla ALL=(root) NOPASSWD: /bin/true ""  # host.true_root' ] && ok "the sudoers line: exactly the privileged action, no arguments allowed ($S)" || bad "sudoers: $S"
hx flotilla-e2e-host sh -c 'lsattr /var/log/flotilla-host/audit.jsonl' 2>&1 | grep -q '^-*a' \
  && hx flotilla-e2e-host sudo -u _flotilla sh -c 'echo x >> /var/log/flotilla-host/audit.jsonl' 2>/dev/null \
  && ! hx flotilla-e2e-host sudo -u _flotilla sh -c ': > /var/log/flotilla-host/audit.jsonl' 2>/dev/null \
  && ok "the audit log is append-only (chattr +a): the agent's user appends, cannot truncate" || bad "append-only: $(hx flotilla-e2e-host lsattr /var/log/flotilla-host/audit.jsonl 2>&1)"
# the line appended above is not an audit entry: the agent would refuse that log, as it should; start
# its log over (as root, which clears the flag first) before the agent runs
hx flotilla-e2e-host sh -c 'chattr -a /var/log/flotilla-host/audit.jsonl && : > /var/log/flotilla-host/audit.jsonl && chattr +a /var/log/flotilla-host/audit.jsonl'
OUT="$(printf '%s\n' "$HT" | hx -i flotilla-e2e-host sudo -u _flotilla /usr/local/libexec/flotilla-host/flotilla-host enrol -config /etc/flotilla-host/config.json \
  -state /var/db/flotilla-host -audit /var/log/flotilla-host/audit.jsonl -root /etc/flotilla-host/root.crt -addr flotillad:8487 -server-name flotillad 2>&1)"
echo "$OUT" | grep -q "enrolment refused" && ok "the same token a second time is refused" || bad "a used token: $OUT"
cli host list | grep -q "^e2e-host  *no  *- .*[0-9]T" && ok "host list shows it enrolled, not yet connected, with its certificate's expiry" || bad "host list: $(cli host list 2>&1)"
$D logs flotillad 2>&1 | grep -q '"msg":"host executions are signed".*"key_id":"e2e1"' \
  && ok "flotillad loaded the execution-signing key from its secrets volume (secrets.sh)" || bad "no signing key at start: $($D logs flotillad 2>&1 | grep -i 'sign' | tail -2)"
# the service: what launchd or systemd would run, as the agent's user (the container has neither)
hx -d flotilla-e2e-host sh -c 'sudo -u _flotilla /usr/local/libexec/flotilla-host/flotilla-host run -config /etc/flotilla-host/config.json -state /var/db/flotilla-host -audit /var/log/flotilla-host/audit.jsonl -root /etc/flotilla-host/root.crt -addr flotillad:8487 -server-name flotillad -heartbeat 2s 2>>/var/log/flotilla-host/agent.log'
hseen '"msg":"connected to flotillad"' && seen flotillad '"msg":"host connected".*"host":"e2e-host"' \
  && ok "the installed agent connected as _flotilla; flotillad authenticated it as e2e-host" || bad "connect: $(hostlog | tail -3)"
sleep 8   # four heartbeats at 2s; silent for three, flotillad would drop it
! $D logs flotillad 2>&1 | grep -q '"msg":"host disconnected"' && ok "heartbeats keep the connection (8s, flotillad drops after 6s of silence)" \
  || bad "the connection dropped: $($D logs flotillad 2>&1 | grep '"host' | tail -3)"
HL="$(cli host list 2>&1)"
# nothing has run on the host, so its audit log is empty and has no chain head yet ("-")
echo "$HL" | grep -q "^e2e-host  *yes  *$TAG  *[0-9-]*T[0-9:.]*Z  *-\?[0-9.]*[mµn]\?s  *[0-9-]*T[0-9:.]*Z  *-  *-$" \
  && ok "host list: connected, its version, last contact and clock offset, from the store (no chain head: nothing ran)" || bad "host list while connected: $HL"
! $D logs flotillad 2>&1 | grep -q '"msg":"host alert"' && ok "no host alert while it is connected" || bad "a host alert: $($D logs flotillad 2>&1 | grep '"host alert' | tail -2)"
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
  for _ in $(seq 1 45); do cli case list --all 2>&1 | grep -q "^$1 .* $2 " && return 0; sleep 2; done
  return 1
}
cli action autonomy host.true propose --reason "e2e: the security e2e's harmless action" >/dev/null 2>&1
cli action list 2>&1 | grep -q "^host.true  *propose " && ok "host.true promoted to propose (operator, with a reason)" || bad "promote: $(cli action list 2>&1)"
HC="$(proposed e2e_host_true host-true-monitor)"
[ -n "$HC" ] && ok "the detection became a case; the acting routine planned host.true; policy proposed it ($HC awaiting approval)" \
  || bad "no e2e_host_true case awaiting approval: $(cli case list --all 2>&1 | grep e2e_host_true)"
OUT="$(cli case approve "$HC" 2>&1)"
echo "$OUT" | grep -q '"approved_by":"operator"' && ok "the operator approved it" || bad "approve: $OUT"
status_is "$HC" closed
SHOW="$(cli case show "$HC" 2>&1)"
echo "$SHOW" | grep -q "case.executing .*operator .*by operator .*approved host.true on host:e2e-host" \
  && echo "$SHOW" | grep -q "case.checking .*flotillad .*by act .*on e2e-host: done" \
  && ok "signed, sent, run on the host as _flotilla (/bin/true), done: the case went to checking" || bad "the execution: $SHOW"
echo "$SHOW" | grep -q "case.closed .*e2e/host-true-verify .*by check .*e2e: nothing to verify" \
  && ok "the check routine verified it: closed verified, the target released" || bad "the check: $SHOW"
# the target is free again: a second host.true reaches the host, which refuses it (per_hour 1)
H2="$(proposed e2e_host_true host-true-monitor)"
[ -n "$H2" ] && cli case approve "$H2" >/dev/null 2>&1 && status_is "$H2" escalated \
  && cli case show "$H2" 2>&1 | grep -q "case.escalated .*flotillad .*by act .*on e2e-host: refused: host.true ran 1 times in the last hour here, its limit" \
  && ok "a second host.true on the released target: the host refused it, its own rate limit ($H2 escalated)" \
  || bad "the rate limit: $H2 $(cli case show "$H2" 2>&1 | tail -3)"
OUT="$(cli case close "$H2" 2>&1)"
echo "$OUT" | grep -q "reason" && ok "closing an escalated case needs a reason" || bad "close without a reason: $OUT"
cli case close "$H2" --reason "e2e: refused by the host, as it should" >/dev/null 2>&1 && status_is "$H2" closed \
  && ok "the operator closed it; the target is released" || bad "close: $(cli case show "$H2" 2>&1 | tail -2)"
# an action flotillad's registry declares and the host's allowlist does not
cli action autonomy host.absent propose --reason "e2e: an action the host does not allow" >/dev/null 2>&1
H3="$(proposed e2e_host_absent host-absent-monitor)"
[ -n "$H3" ] && cli case approve "$H3" >/dev/null 2>&1 && status_is "$H3" escalated \
  && cli case show "$H3" 2>&1 | grep -q "case.escalated .*flotillad .*by act .*on e2e-host: refused: .*host.absent is not allowed on this host" \
  && ok "host.absent, signed and approved: the host refused it, not on its allowlist ($H3 escalated)" \
  || bad "the allowlist: $H3 $(cli case show "$H3" 2>&1 | tail -3)"
cli case close "$H3" --reason "e2e: refused by the host, as it should" >/dev/null 2>&1 && status_is "$H3" closed \
  || bad "close $H3: $(cli case show "$H3" 2>&1 | tail -2)"
hx flotilla-e2e-host cat /var/log/flotilla-host/audit.jsonl > "$WORK/host-audit.jsonl" 2>/dev/null
AUD="$($D exec -i flotillad /usr/local/bin/flotilla host audit e2e-host --log - < "$WORK/host-audit.jsonl" 2>&1)"; ARC=$?
[ $ARC = 0 ] && echo "$AUD" | grep -q "^AUDIT PASS: e2e-host, 3 executions on record" && echo "$AUD" | grep -q "done on both sides" \
  && ok "flotilla host audit: the host's real log matches flotillad's record (AUDIT PASS, 3 executions)" || bad "audit: rc=$ARC $AUD"
sed '0,/"status":"done"/s//"status":"failed"/' "$WORK/host-audit.jsonl" > "$WORK/host-audit-tampered.jsonl"
AUD="$($D exec -i flotillad /usr/local/bin/flotilla host audit e2e-host --log - < "$WORK/host-audit-tampered.jsonl" 2>&1)"; ARC=$?
[ $ARC = 1 ] && echo "$AUD" | grep -q "^AUDIT FAIL" && ok "a rewritten copy of the host's log fails the audit" || bad "tampered audit: rc=$ARC $AUD"
$D restart flotillad >/dev/null
for i in $(seq 1 30); do cli health >/dev/null 2>&1 && break; sleep 2; done
for i in $(seq 1 30); do [ "$(hostlog | grep -c '"msg":"connected to flotillad"')" -ge 2 ] && break; sleep 1; done
[ "$(hostlog | grep -c '"msg":"connected to flotillad"')" -ge 2 ] && hostlog | grep -q '"msg":"connection to flotillad lost"' \
  && ok "after a flotillad restart the agent lost the connection and reconnected by itself" || bad "reconnect: $(hostlog | tail -4)"
cli host revoke e2e-host >/dev/null
seen flotillad '"msg":"host cut off".*"host":"e2e-host"' && ok "revoked: flotillad cut the connected host off at its next heartbeat" \
  || bad "revoke: $($D logs flotillad 2>&1 | grep '"host' | tail -3)"
sleep 3
seen flotillad '"msg":"host endpoint: refused"' && hostlog | grep -q 'flotillad refused: refused' \
  && ok "its reconnects are refused" || bad "after revoke: $(hostlog | tail -3)"
cli host list | grep -q "^e2e-host  *no .* -  *[0-9-]*T[0-9:.]*Z " && ok "host list shows it revoked, not connected, no certificate" || bad "host list after revoke: $(cli host list 2>&1)"
hx flotilla-e2e-host sh -c 'pkill -u _flotilla flotilla-host; true'
OUT="$(hx flotilla-e2e-host bash /stage/remote-install.sh /stage remove 2>&1)"
R="$(hx flotilla-e2e-host sh -c 'ls -d /usr/local/libexec/flotilla-host /etc/flotilla-host /var/db/flotilla-host /etc/sudoers.d/flotilla-host /var/log/flotilla-host/audit.jsonl 2>/dev/null' | tr '\n' ' ')"
echo "$OUT" | grep -q "^REMOVE PASS" && [ "$R" = "/var/log/flotilla-host/audit.jsonl " ] \
  && ok "remove: binary, config, state and sudoers gone; the audit log stays" || bad "remove: $OUT / left: $R"
$D rm -f flotilla-e2e-host >/dev/null

echo "== users and the UI listener (ADR-0014); last, since the first admin retires the operator token"
UA="$(cli user add ada --role admin 2>&1)"
ADMIN="$(echo "$UA" | sed -n 's/.*"token":"\(flt_[0-9a-f]*\)".*/\1/p')"
[ ${#ADMIN} = 68 ] && ok "the first admin, with the operator token (a 256-bit token, shown once)" || bad "first admin: $UA"
OUT="$(cli status 2>&1)"
echo "$OUT" | grep -q "HTTP 403" && ok "after it, the operator token is retired" || bad "operator token after the first admin: $OUT"
OUT="$($D exec -e FLOTILLA_TOKEN="$ADMIN" flotillad /usr/local/bin/flotilla resume platform/heartbeat 2>&1)"
echo "$OUT" | grep -q "needs a reason" && ok "loosening without a reason is refused" || bad "resume without a reason: $OUT"
OUT="$($D exec -e FLOTILLA_TOKEN="$ADMIN" flotillad /usr/local/bin/flotilla resume platform/heartbeat --reason "e2e" 2>&1)"
echo "$OUT" | grep -q '"by":"user:ada"' && ok "the admin resumes with a reason, as user:ada" || bad "admin resume: $OUT"
UIC="$($D exec -i flotilla-worker-triage python3 - "$ADMIN" "$OP" < "$ROOT/deploy/e2e/ui_check.py" 2>&1)"; UIRC=$?
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

echo "== hardening"
for w in flotilla-worker-ops-read flotilla-worker-ops-act-host; do
  NW="$($D inspect $w --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
  [ "$NW" = "${P}_flotilla " ] && ok "$w is on the internal network only" || bad "$w networks: '$NW'"
done
for c in flotillad flotilla-worker-triage flotilla-worker-triage-candidate flotilla-worker-ops-read flotilla-worker-ops-act-host flotilla-egress; do
  RO=$($D inspect "$c" --format '{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.Config.User}}')
  case "$RO" in "true [ALL] "*) ok "$c: read-only root, all capabilities dropped, user ${RO##* }" ;; *) bad "$c hardening: $RO" ;; esac
done
NW="$($D inspect flotilla-worker-triage --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
[ "$NW" = "${P}_flotilla " ] && ok "flotilla-worker-triage is on the internal network only" || bad "flotilla-worker-triage networks: '$NW'"
NW="$($D inspect flotillad --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
[ "$NW" = "e2e-ui ${P}_flotilla " ] && ok "flotillad is on the internal flotilla network and the UI network only" || bad "flotillad networks: '$NW'"
NW="$($D network inspect e2e-ui --format '{{.Internal}}' 2>&1)"
[ "$NW" = true ] && ok "the UI network is internal (no route out)" || bad "ui network internal=$NW"
NW="$($D network inspect ${P}_flotilla --format '{{.Internal}}' 2>&1)"
[ "$NW" = true ] && ok "the flotilla network is internal (no route out)" || bad "flotilla network internal=$NW"

if [ "${KEEP:-}" != 1 ]; then down; fi
if [ "$fails" -eq 0 ]; then echo "E2E PASS"; else echo "E2E FAIL ($fails)"; exit 1; fi
