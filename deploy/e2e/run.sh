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
# from flotillad.yaml, the key from the fleet's secrets file and the proxy, and its span is exported.
# Never on the production engine (homelab ADR-0009): it uses the `dev` context only.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
D="docker --context ${DOCKER_CONTEXT_DEV:-dev}"
P=flotilla-e2e
TAG=e2e-local
LABEL=flotilla.e2e=1
NETS="e2e-backend e2e-litellm e2e-glitchtip e2e-langfuse"
WORK="${E2E_WORK:-$ROOT/.e2e}"
fails=0
ok() { echo "PASS $*"; }
bad() { echo "FAIL $*"; fails=$((fails + 1)); }

E2E_ENV=(FLOTILLA_REGISTRY=local FLOTILLA_TAG="$TAG" FLOTILLA_CONFIG="$WORK/config" TRIAGE_ENV_FILE="$WORK/config/fleet.env"
  FLOTILLA_VOLUME_PREFIX=flotilla-e2e FLOTILLA_STATE_VOLUME=flotilla-e2e-state TRIAGE_STATE_VOLUME=flotilla-e2e-triage-state
  COMPOSE_PROFILES=candidate TRIAGE_CANDIDATE_STATE_VOLUME=flotilla-e2e-triage-candidate-state
  NET_BACKEND=e2e-backend NET_LITELLM=e2e-litellm NET_GLITCHTIP=e2e-glitchtip NET_LANGFUSE=e2e-langfuse
  DOCKER_CONTEXT="${DOCKER_CONTEXT_DEV:-dev}")
compose() { env "${E2E_ENV[@]}" $D compose -p $P -f "$ROOT/deploy/compose/docker-compose.yml" "$@"; }

down() {
  compose down -v --remove-orphans >/dev/null 2>&1
  for c in $($D ps -aq --filter label=$LABEL); do $D rm -f "$c" >/dev/null; done
  for v in flotilla-e2e-state flotilla-e2e-triage-state flotilla-e2e-triage-candidate-state flotilla-e2e-flotillad-secrets flotilla-e2e-worker-triage-secrets; do
    $D volume rm "$v" >/dev/null 2>&1
  done
  for n in $NETS; do $D network rm "$n" >/dev/null 2>&1; done
  rm -rf "$WORK"
}
if [ "${1:-}" = down ]; then down; echo "e2e torn down"; exit 0; fi
down

echo "== build"
for i in flotillad worker-triage smokescreen; do
  $D build -q -f "$ROOT/deploy/docker/$i.Dockerfile" --build-arg VERSION=$TAG -t local/flotilla-$i:$TAG "$ROOT" >/dev/null \
    || { echo "BUILD FAIL $i"; exit 1; }
done
ok "images built"

echo "== config and stand-ins"
mkdir -p "$WORK/config"
printf '%s\n' "${E2E_ENV[@]}" > "$WORK/.env"
tok() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
OP=$(tok); AGENT=$(tok); WT=$(tok)
hash() { printf %s "$1" | sha256sum | cut -d' ' -f1; }
printf %s "$WT" > "$WORK/config/worker-triage.token"
printf %s "$OP" > "$WORK/config/cli.token"          # the CLI inside flotillad acts as the operator here
printf %s "$AGENT" > "$WORK/agent.token"
cat > "$WORK/config/callers.yaml" <<EOF
callers:
  - {name: operator, role: operator, token_sha256: $(hash "$OP")}
  - {name: e2e-agent, role: agent, token_sha256: $(hash "$AGENT")}
EOF
cat > "$WORK/config/flotillad.yaml" <<EOF
listen: 127.0.0.1:8484
specs_dir: /opt/flotilla/fleets
state_dir: /var/lib/flotilla
environment: e2e
timezone: Europe/Amsterdam
vm_url: http://victoriametrics:8080
otlp_traces_url: http://victoriatraces:8080/insert/opentelemetry/v1/traces
callers_file: /etc/flotilla/callers.yaml
workers:
  triage: {url: "http://worker-triage:8081", token_file: /run/flotilla/worker-triage.token}
  triage-candidate: {url: "http://worker-triage-candidate:8081", token_file: /run/flotilla/worker-triage.token}
disabled_routines: [triage/cycle, triage/cycle-shadow]
models:
  endpoints:
    gateway: {base_url: "http://litellm:8080/v1", kind: litellm, key_env: E2E_LLM_KEY, defaults: {tools: native, json: object, cost: reported}}
  aliases:
    e2e-model: {endpoint: gateway, model: fleet-triage-flash}
EOF
echo "E2E_ONLY=1" > "$WORK/config/fleet.env"
cat > "$WORK/config/worker-triage.env" <<EOF
ROUTINE_HEARTBEAT_ALLOWED_URL=http://litellm:8080/health/liveliness
ROUTINE_HEARTBEAT_DENIED_URL=http://forbidden:8080/
ROUTINE_HEARTBEAT_MODEL_ALIAS=e2e-model
ROUTINE_SF_SERVICE=triage-fleet
ROUTINE_SF_GLITCHTIP_SYNC=1
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
chmod 644 "$WORK"/config/*
for n in $NETS; do $D network create --label $LABEL "$n" >/dev/null; done
stub() { $D run -d --label $LABEL --name "e2e-$1" --network "$2" --network-alias "$1" --memory 64m \
  -v "$ROOT/deploy/e2e/stub.py:/stub.py:ro" python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b \
  python3 -u /stub.py "$1" >/dev/null; }
stub victoriametrics e2e-backend; stub victoriatraces e2e-backend; stub litellm e2e-litellm
# off the allowlist but reachable and answering: refusing it is the proxy's doing, nothing else
stub forbidden e2e-backend
ok "stand-ins up"

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
RUNS="$(cli runs platform/heartbeat 2>&1)"; echo "$RUNS"
echo "$RUNS" | grep -q " ok " && ok "heartbeat ran through the worker and passed" || bad "heartbeat did not pass"
$D logs flotilla-worker-triage 2>&1 | grep -q '"line": "HEARTBEAT PASS"' && ok "the heartbeat saw allowed-in and denied-out" \
  || bad "no HEARTBEAT PASS in the worker's log"
$D logs flotilla-egress 2>&1 | grep -q "forbidden" && ok "the proxy logged the refused host" || bad "the proxy has no record of the refused host"
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
seen() { for _ in $(seq 1 30); do $D logs "$1" 2>&1 | grep -q "$2" && return 0; sleep 1; done; return 1; }
seen e2e-victoriametrics 'SAMPLE flotilla_run{autonomy="auto",environment="e2e",routine="platform/heartbeat",service="flotillad",status="ok"' \
  && ok "flotilla_run reached VictoriaMetrics through the proxy" || bad "no flotilla_run sample at the VictoriaMetrics stand-in"
seen e2e-victoriametrics 'SAMPLE flotilla_up' && ok "flotilla_up pushed" || bad "no flotilla_up"
seen e2e-victoriatraces "POST /insert/opentelemetry/v1/traces" && ok "traces reached VictoriaTraces through the proxy" \
  || bad "no traces at the VictoriaTraces stand-in"
seen e2e-victoriatraces "SPAN model.chat" && ok "the routine's model.chat span reached VictoriaTraces through the proxy" \
  || bad "no model.chat span at the VictoriaTraces stand-in"

echo "== controls and auth"
OUT="$($D exec -e FLOTILLA_TOKEN="$AGENT" flotillad /usr/local/bin/flotilla autonomy platform/heartbeat shadow 2>&1)"
echo "$OUT" | grep -q "HTTP 403" && ok "an agent cannot change a routine's autonomy ($OUT)" || bad "agent autonomy change: $OUT"
cli stop platform >/dev/null && cli run platform/heartbeat >/dev/null
sleep 3
cli runs platform/heartbeat -n 1 | grep -q " skipped " && ok "a STOPped fleet's run is recorded as skipped" || bad "STOP did not skip"
cli start platform >/dev/null
cli events -n 5 | grep -q "control.stop" && ok "the event log records the STOP" || bad "no control.stop event"
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

echo "== hardening"
for c in flotillad flotilla-worker-triage flotilla-worker-triage-candidate flotilla-egress; do
  RO=$($D inspect "$c" --format '{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.Config.User}}')
  case "$RO" in "true [ALL] "*) ok "$c: read-only root, all capabilities dropped, user ${RO##* }" ;; *) bad "$c hardening: $RO" ;; esac
done
for c in flotillad flotilla-worker-triage; do
  NW="$($D inspect "$c" --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
  [ "$NW" = "${P}_flotilla " ] && ok "$c is on the internal network only" || bad "$c networks: '$NW'"
done
NW="$($D network inspect ${P}_flotilla --format '{{.Internal}}' 2>&1)"
[ "$NW" = true ] && ok "the flotilla network is internal (no route out)" || bad "flotilla network internal=$NW"

if [ "${KEEP:-}" != 1 ]; then down; fi
if [ "$fails" -eq 0 ]; then echo "E2E PASS"; else echo "E2E FAIL ($fails)"; exit 1; fi
