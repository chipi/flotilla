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
# starting fine; a flat routine's playbook edit re-recorded at the start, and its move to the folder layout refused
# until the operator's `flotilla versions rerecord`; a schema-25 store migrated and started on by the real image
# (every routine's v1 live, the history version 1); the autonomy rename (a routine at record reads FLEETD_STAGE=shadow);
# the same forms in Chromium.
# Never on the production engine (the installation's ADR-0009): it uses the `dev` context only.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
D="docker --context ${DOCKER_CONTEXT_DEV:-dev}"
$D version >/dev/null 2>&1 || { echo "no Docker context '${DOCKER_CONTEXT_DEV:-dev}': name your development engine's context in DOCKER_CONTEXT_DEV (DOCKER_CONTEXT_DEV=default on a machine with one engine)" >&2; exit 2; }
P=flotilla-e2e
TAG=e2e-local
LABEL=flotilla.e2e=1
NETS="e2e-backend e2e-litellm e2e-glitchtip e2e-langfuse"
PLAYWRIGHT_IMAGE=local/flotilla-e2e-browser:$TAG
DEMO_PORT="${DEMO_PORT:-8443}"
# the model contract check measures real models; against the stand-in gateway it can only fail: off in the demo
if [ "${DEMO:-}" = 1 ]; then DEMO_DISABLED=", platform/model-contract"; else DEMO_DISABLED=""; fi
# E2E_MUTANT=shadow-write-cred is a mutation check: it puts a write credential into the shadow worker's read environment, and the hostile shadow's
# check must go red. (E2E_ACL_ACTION=open is the proxy's.)
# the shadow check's two routines are enabled in their specs and held off in the config (a shadow of a disabled routine does not run) until
# the shadow section frees them
if [ "${DEMO:-}" = 1 ]; then SHADOW_HELD=""; else SHADOW_HELD=", e2e/shadow-monitor, e2e/cpu, e2e/station-monitor"; fi
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

E2E_ENV=(FLOTILLA_REGISTRY=local FLOTILLA_TAG="$TAG" FLOTILLA_CONFIG="$WORK/config" TRIAGE_ENV_FILE="$WORK/config/fleet.env"
  FLOTILLA_VOLUME_PREFIX=flotilla-e2e FLOTILLA_STATE_VOLUME=flotilla-e2e-state TRIAGE_STATE_VOLUME=flotilla-e2e-triage-state
  COMPOSE_PROFILES=candidate,ops,bugfix,shadow TRIAGE_CANDIDATE_STATE_VOLUME=flotilla-e2e-triage-candidate-state TRIAGE_READ_ENV_FILE="$WORK/config/fleet-read.env"
  NET_BACKEND=e2e-backend NET_LITELLM=e2e-litellm NET_GLITCHTIP=e2e-glitchtip NET_LANGFUSE=e2e-langfuse NET_UI=e2e-ui
  DOCKER_CONTEXT="${DOCKER_CONTEXT_DEV:-dev}")
compose() { env "${E2E_ENV[@]}" $D compose -p $P -f "$ROOT/deploy/compose/docker-compose.yml" -f "$ROOT/deploy/e2e/docker-compose.e2e.yml" "$@"; }

down() {
  compose down -v --remove-orphans >/dev/null 2>&1
  for c in $($D ps -aq --filter label=$LABEL); do $D rm -f "$c" >/dev/null; done
  for c in flotilla-e2e-remote-worker flotilla-e2e-remote-link; do $D rm -f "$c" >/dev/null 2>&1; done
  for v in flotilla-e2e-state flotilla-e2e-triage-state flotilla-e2e-triage-candidate-state flotilla-e2e-flotillad-secrets flotilla-e2e-worker-triage-secrets flotilla-e2e-worker-triage-candidate-secrets flotilla-e2e-worker-triage-shadow-secrets flotilla-e2e-triage-snapshots flotilla-e2e-triage-shadow-state flotilla-e2e-required flotilla-e2e-worker-ops-read-secrets flotilla-e2e-worker-ops-act-host-secrets flotilla-e2e-worker-bugfix-secrets flotilla-e2e-worker-bugfix-act-secrets flotilla-e2e-bugfix-scratch flotilla-e2e-host-state flotilla-e2e-host-audit flotilla-e2e-store25; do
    $D volume rm "$v" >/dev/null 2>&1
  done
  for n in $NETS; do $D network rm "$n" >/dev/null 2>&1; done
  rm -rf "$WORK"
}
if [ "${1:-}" = down ]; then down; echo "e2e torn down"; exit 0; fi
down

echo "== build"
case ",${E2E_MUTANT:-}," in *,station-live-key,*|*,station-span-mark,*)
  BUILD_CTX="$WORK/mutated-src"; mkdir -p "$BUILD_CTX"
  (cd "$ROOT" && tar --exclude=./.git --exclude=./.e2e --exclude=./.venv --exclude=./site --exclude=./node_modules --exclude=./.claude -cf - .) | tar -xf - -C "$BUILD_CTX"
  if [[ ",${E2E_MUTANT}," == *,station-live-key,* ]]; then
    sed -i 's/calls\[i\] = store.ModelCall{RunID: runID, Routine: key,/calls[i] = store.ModelCall{RunID: runID, Routine: t.routineID,/' "$BUILD_CTX/internal/sched/shadow.go"
    cmp -s "$ROOT/internal/sched/shadow.go" "$BUILD_CTX/internal/sched/shadow.go" && { echo "MUTANT station-live-key did not change shadow.go"; exit 1; }
  fi
  if [[ ",${E2E_MUTANT}," == *,station-span-mark,* ]]; then
    sed -i 's/        out\["flotilla.shadow"\] = True/        pass/' "$BUILD_CTX/runtime/telemetry.py"
    cmp -s "$ROOT/runtime/telemetry.py" "$BUILD_CTX/runtime/telemetry.py" && { echo "MUTANT station-span-mark did not change telemetry.py"; exit 1; }
  fi ;;
esac
for i in flotillad worker-triage worker-ops worker-bugfix smokescreen; do
  $D build -q -f "$BUILD_CTX/deploy/docker/$i.Dockerfile" --build-arg VERSION=$TAG -t local/flotilla-$i:$TAG "$BUILD_CTX" >/dev/null \
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
mkdir -p "$WORK/config/specs" && cp -r "$ROOT/fleets/." "$WORK/config/specs/"
# the e2e-only fleet tests the platform itself (a hang, a remote echo, harmless host actions): not in the demo
[ "${DEMO:-}" = 1 ] || cp -r "$ROOT/deploy/e2e/fleets/." "$WORK/config/specs/"
cp "$ROOT/deploy/e2e/registry-e2e.json" "$WORK/config/registry-e2e.json"
printf '%s\n' "${E2E_ENV[@]}" > "$WORK/.env"
tok() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
OP=$(tok); AGENT=$(tok); WT=$(tok)
hash() { printf %s "$1" | sha256sum | cut -d' ' -f1; }
printf %s "$WT" > "$WORK/config/worker-triage.token"
printf %s "$(tok)" > "$WORK/config/worker-triage-shadow.token"      # the shadow worker's own, and flotillad never takes it for a caller
printf %s "$(tok)" > "$WORK/config/worker-triage-candidate.token"   # its own: flotillad refuses two workers that share one
tok > "$WORK/config/worker-ops-read.token"; tok > "$WORK/config/worker-ops-act-host.token"
tok > "$WORK/config/worker-bugfix.token"; tok > "$WORK/config/worker-bugfix-act.token"
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
registry_files: [/opt/flotilla/host/registry.json, /etc/flotilla/registry-e2e.json]   # plus the e2e's host.true
state_dir: /var/lib/flotilla
environment: e2e
timezone: Europe/Amsterdam
vm_url: http://victoriametrics:8080
otlp_traces_url: http://victoriatraces:8080/insert/opentelemetry/v1/traces
callers_file: /etc/flotilla/callers.yaml
workers:
  triage: {url: "http://worker-triage:8081", token_file: /run/flotilla/worker-triage.token}
  triage-candidate: {url: "http://worker-triage-candidate:8081", token_file: /run/flotilla/worker-triage-candidate.token}
  triage-shadow: {url: "http://worker-triage-shadow:8081", token_file: /run/flotilla/worker-triage-shadow.token, shadow: true}
  ops-read: {url: "http://worker-ops-read:8081", token_file: /run/flotilla/worker-ops-read.token}
  ops-act-host: {url: "http://worker-ops-act-host:8081", token_file: /run/flotilla/worker-ops-act-host.token}
  bugfix: {url: "http://worker-bugfix:8081", token_file: /run/flotilla/worker-bugfix.token}
  bugfix-act: {url: "http://worker-bugfix-act:8081", token_file: /run/flotilla/worker-bugfix-act.token}
  # the fix routines' own workers on the host are remote (a second machine); the e2e runs neither routine, so they
  # name the local bugfix worker
  bugfix-fix: {url: "http://worker-bugfix:8081", token_file: /run/flotilla/worker-bugfix.token}
  remote-e2e: {remote: w-remote-e2e}   # RFC-0011: a worker reached through its link
# the candidate triage needs a candidate model; bugfix/fix clones the repo, installs its toolchain and runs a
# coding agent on a real model through many tool calls: a stand-in for that would show the stand-in, not
# flotilla. Its unit tests cover it on local repos (bugfix/test_fix.py); here its intake opens the case.
disabled_routines: [triage/cycle-shadow, bugfix/fix$DEMO_DISABLED$SHADOW_HELD]
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
hosts: {listen: "0.0.0.0:8487", ca_dir: /run/flotilla, server_names: [flotillad], heartbeat: 2s, signing_key_file: /run/flotilla/signing.key, signing_key_id: e2e1}
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
find "$WORK/config" -maxdepth 1 -type f -exec chmod 644 {} +   # the files; the specs folder stays traversable
chmod -R a+rX "$WORK/config/specs"
for n in $NETS; do $D network create --label $LABEL "$n" >/dev/null; done
stub() { $D run -d --label $LABEL --name "e2e-$1" --network "$2" --network-alias "$1" --memory 64m \
  -v "$ROOT/deploy/e2e/stub.py:/stub.py:ro" python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b \
  python3 -u /stub.py "$1" >/dev/null; }
stub victoriametrics e2e-backend; stub victoriatraces e2e-backend; stub litellm e2e-litellm
# off the allowlist but reachable and answering: refusing it is the proxy's doing, nothing else
stub forbidden e2e-backend
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
  && echo "$OUT" | grep >/dev/null "^action repo.process_change$" && echo "$OUT" | grep >/dev/null "^action github.open_pr$" && echo "$OUT" | grep >/dev/null "^CHECK PASS.*, 12 actions$" \
  && echo "$OUT" | grep >/dev/null "^action crm.merge$" && echo "$OUT" | grep >/dev/null "^action email.send$" \
  && ok "-check reads the registry the image carries and the e2e's extension (the jobs, review, bugfix and business examples' actions, host.true, host.absent)" || bad "-check: $OUT"
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
seen flotilla-egress "forbidden" && ok "the proxy logged the refused host" || bad "the proxy has no record of the refused host"
[ -z "$($D logs e2e-forbidden 2>&1)" ] && ok "the refused host never received a request" || bad "the refused host was reached: $($D logs e2e-forbidden 2>&1 | head -2)"
cli status | grep -E "^platform/heartbeat" | grep >/dev/null -E " false " && ok "status: heartbeat not stale" || bad "status: heartbeat stale"

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

echo "== the business examples (crm, campaigns, outbound, renewals), on their stand-in data, in shadow"
# Each monitor runs at start on the ops-read worker; its detections become cases, the acting routine plans
# each, and policy records the plan in shadow and closes the case. The gates' refusals escalate instead.
want_closed() {  # kind target: a case of that kind on that target closed as a recorded shadow plan
  for _ in $(seq 1 45); do
    cli case list --all 2>&1 | grep >/dev/null "^[^ ]* *$1 .*$2 .*closed .*[^_[:alnum:]]plan_recorded" && return 0; sleep 2
  done
  return 1
}
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

echo "== the examples that reach the outside world (sites, deps), on deploy/e2e/world.py, in shadow"
want_case() {  # kind target status: a case of that kind on that target in that status
  for _ in $(seq 1 45); do
    cli case list --all 2>&1 | grep >/dev/null "^[^ ]* *$1 .*$2 .*$3" && return 0; sleep 2
  done
  return 1
}
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

echo "== the bugfix example's intake, on the world's GitHub"
want_case code_bug "repo:example/shop/issues/7" "handed_over" \
  && ok "bugfix: the open bug issue became a code_bug case (its fix needs a real coding model: off here)" \
  || bad "bugfix intake: $(cli case list --all 2>&1 | grep code_bug)"

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
# The freeze (ADR-0014 decision 8), through the real scheduler and worker: the heartbeat's own autonomy is auto.
agent() { $D exec -e FLOTILLA_TOKEN="$AGENT" flotillad /usr/local/bin/flotilla "$@"; }
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

echo "== the ledger (RFC-0002 §9 step 5): a worker's routine on flotillad's workers' listener"
LR="$($D exec -w /opt/flotilla/signal-fleet/mvp -e FLOTILLA_API_URL=http://flotillad:8485 \
  -e FLOTILLA_API_TOKEN_FILE=/run/flotilla/worker.token -e NO_PROXY=flotillad -e SF_OBSERV_DISABLED=1 \
  flotilla-worker-triage python3 gate_ledger_backend_replay.py --synthetic 300 2>&1)"
echo "$LR" | grep >/dev/null "LEDGER BACKEND REPLAY: PASS" \
  && ok "dedup through the ledger equals filed.tsv: $(echo "$LR" | grep '^lookup:' | cut -c1-90)" \
  || bad "ledger replay: $(echo "$LR" | tail -4 | tr '\n' ' ')"
WL="$($D exec flotilla-worker-triage python3 -c 'import urllib.request,urllib.error
try:
    urllib.request.urlopen("http://flotillad:8485/v1/status", timeout=5); print("served")
except urllib.error.HTTPError as e: print(e.code)' 2>&1 | tail -1)"
[ "$WL" = 404 ] && ok "the workers' listener does not serve the full API (/v1/status: 404)" || bad "workers' listener /v1/status: $WL"

echo "== run tokens (v2 step 2): every run has its own token for the workers' listener"
# e2e/token-probe (fleets/e2e/token-probe.yaml) reads FLOTILLA_API_TOKEN_FILE, which must be its own file and not the worker's,
# and files a ledger row naming another routine's namespace and another run's id: flotillad files it under the probe's own.
if [ "${DEMO:-}" != 1 ]; then
cli run e2e/token-probe >/dev/null
for i in $(seq 1 30); do cli runs e2e/token-probe -n 1 | grep >/dev/null -E " (ok|error|timeout) " && break; sleep 1; done
[ "$(cli runs e2e/token-probe -json -n 1 | python3 -c 'import json,sys; print(json.load(sys.stdin)["runs"][0]["status"])')" = ok ] \
  && ok "a run was given its own token file (mode 600, not the worker's) and its ledger write named another namespace" \
  || bad "token-probe: $(cli runs e2e/token-probe -n 1) $($D logs flotilla-worker-triage 2>&1 | grep -E 'token_file|own |worker.s token file|namespace the run' | tail -3)"
NSC="$($D exec flotilla-worker-triage python3 -c 'import json,urllib.request
def n(ns):
    r = urllib.request.Request("http://flotillad:8485/v1/ledger/filings?ns=" + ns, headers={"Authorization": "Bearer " + open("/run/flotilla/worker.token").read().strip()})
    return len(json.load(urllib.request.urlopen(r, timeout=10))["filings"])
print(n("e2e/token-probe"), n("bugfix/fix"))' 2>&1 | tail -1)"
[ "$NSC" = "1 0" ] && ok "the filing landed in the run's own namespace (e2e/token-probe: 1 row, the namespace it named: 0)" || bad "ledger rows by namespace: '$NSC'"
TL="$($D exec flotilla-worker-triage sh -c 'ls -A /tmp | grep -c "^.flotilla-token-" || true' 2>&1 | tail -1)"
[ "$TL" = 0 ] && ok "no run's token file is left on the worker after the run" || bad "token files left in the worker's /tmp: $TL"
# run_tokens: required, read by the real binary (-check, read-only): a worker's own token is not a caller then
sed 's|^state_dir: .*|state_dir: /tmp/required-check|; /^worker_listen:/a run_tokens: required' "$WORK/config/flotillad.yaml" > "$WORK/config/flotillad-required.yaml"
ACC="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"
REQ="$($D run --rm --read-only --tmpfs /tmp --user 65532:65532 -v "$WORK/config/flotillad-required.yaml:/etc/flotilla/flotillad.yaml:ro" -v "$WORK/config/callers.yaml:/etc/flotilla/callers.yaml:ro" \
  -v flotilla-e2e-flotillad-secrets:/run/flotilla:ro -v "$WORK/config/specs:/etc/flotilla/specs:ro" -v "$WORK/config/registry-e2e.json:/etc/flotilla/registry-e2e.json:ro" \
  "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml -check 2>&1)"
NTOK="$(grep -v "shadow: true" "$WORK/config/flotillad.yaml" | grep -c "token_file:")"   # a shadow worker's token is no caller (S1), so it is not among those required drops
CA="$(echo "$ACC" | grep -o '[0-9]* callers' | cut -d' ' -f1)"; CR="$(echo "$REQ" | grep -o '[0-9]* callers' | cut -d' ' -f1)"
echo "$ACC" | grep >/dev/null "^run tokens: accept " && echo "$REQ" | grep >/dev/null "^run tokens: required .*opens nothing" && [ -n "$CA" ] && [ "$((CA - CR))" = "$NTOK" ] \
  && ok "-check: run_tokens accept by default; required drops the $NTOK workers' own tokens from the callers ($CA to $CR)" || bad "-check run tokens: accept '$(echo "$ACC" | grep '^run tokens')' required '$(echo "$REQ" | grep -E '^run tokens|callers' | tr '\n' ' ')'"
# two workers that share a token are refused, by name, by -check (so a deploy's check stops it before anything restarts)
sed 's|worker-triage-candidate.token|worker-triage.token|' "$WORK/config/flotillad.yaml" > "$WORK/config/flotillad-shared.yaml"
SHR="$($D run --rm --read-only --tmpfs /tmp --user 65532:65532 -v "$WORK/config/flotillad-shared.yaml:/etc/flotilla/flotillad.yaml:ro" -v "$WORK/config/callers.yaml:/etc/flotilla/callers.yaml:ro" \
  -v flotilla-e2e-flotillad-secrets:/run/flotilla:ro -v "$WORK/config/specs:/etc/flotilla/specs:ro" -v "$WORK/config/registry-e2e.json:/etc/flotilla/registry-e2e.json:ro" \
  "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml -check 2>&1; echo "rc=$?")"
echo "$SHR" | grep >/dev/null "workers triage and triage-candidate share a token file" && echo "$SHR" | grep >/dev/null "^rc=2" \
  && ok "-check refuses workers that share a token file, naming them (exit 2, before anything restarts)" || bad "-check on shared worker tokens: $(echo "$SHR" | tail -3)"
echo "$ACC" | grep >/dev/null "share a token file" && bad "the stack's own workers share a token file" || ok "no two of the stack's workers share a token file"
# run_tokens: required on a running flotillad (a second one, idle: its one routine runs `true` once a year, and flotillad ends when no
# timed routine is enabled, so it has to be): a worker's own token opens nothing, the operator's token loads and checks a ledger (what ledger_sync does)
mkdir -p "$WORK/config/specs-required/e2e"
cat > "$WORK/config/specs-required/e2e/idle.yaml" <<EOF2
fleet: e2e
routine: idle
description: The e2e's required-mode flotillad has one routine, which only keeps it running (flotillad ends with no timed routine).
enabled: true
worker: triage
trigger: {every: 8760h}
run: {command: "true", workdir: ., timeout: 1m}
autonomy: record
budget: {day_usd: 0}
freshness: 9000h
EOF2
cat > "$WORK/config/flotillad-required-live.yaml" <<EOF2
listen: 127.0.0.1:8484
worker_listen: 0.0.0.0:8485
specs_dir: /etc/flotilla/specs
state_dir: /var/lib/flotilla
environment: e2e
callers_file: /etc/flotilla/callers.yaml
run_tokens: required
registry_files: [/opt/flotilla/host/registry.json]
workers:
  triage: {url: "http://worker-triage:8081", token_file: /run/flotilla/worker-triage.token}
EOF2
$D volume create --label $LABEL flotilla-e2e-required >/dev/null
$D run --rm --user 0 -v flotilla-e2e-required:/v --entrypoint sh "local/flotilla-worker-triage:$TAG" -c 'chown -R 65532:65532 /v' >/dev/null
$D run -d --label $LABEL --name e2e-required --network "${P}_flotilla" --read-only --cap-drop ALL --security-opt no-new-privileges:true --user 65532:65532 --memory 256m \
  -v flotilla-e2e-required:/var/lib/flotilla -v "$WORK/config/flotillad-required-live.yaml:/etc/flotilla/flotillad.yaml:ro" -v "$WORK/config/callers.yaml:/etc/flotilla/callers.yaml:ro" \
  -v flotilla-e2e-flotillad-secrets:/run/flotilla:ro -v "$WORK/config/specs-required:/etc/flotilla/specs:ro" \
  "local/flotilla-flotillad:$TAG" -config /etc/flotilla/flotillad.yaml >/dev/null || bad "starting flotillad in run_tokens: required"
for _ in $(seq 1 60); do $D logs e2e-required 2>&1 | grep >/dev/null '"msg":"flotillad listening for workers' && break; sleep 1; done
REQ_LOG="$($D logs e2e-required 2>&1 | tail -4 | cut -c1-300)"
printf %s "$OP" | $D exec -i flotilla-worker-triage sh -c 'umask 077 && cat > /tmp/op.token'
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
$D rm -f e2e-required >/dev/null 2>&1; $D volume rm flotilla-e2e-required >/dev/null 2>&1
fi

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
echo "$OUT" | grep >/dev/null "capped at record" && ok "triage/cycle-shadow cannot leave record, even for the operator ($OUT)" \
  || bad "autonomy on the capped routine: $OUT"
OUT="$(cli autonomy triage/cycle-shadow shadow 2>&1)"
echo "$OUT" | grep >/dev/null '"autonomy":"record"' && echo "$OUT" | grep >/dev/null "shadow is now called record" \
  && ok "the old level word shadow is stored as record ($OUT)" || bad "autonomy shadow: $OUT"
OUT="$(cli status 2>&1)"
echo "$OUT" | grep >/dev/null 'worker triage-candidate: .*"ok":true' && ok "flotillad reaches the candidate worker" || bad "status: $OUT"
NW="$($D inspect flotilla-worker-triage-candidate --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
[ "$NW" = "${P}_flotilla " ] && ok "the candidate is on the internal network only" || bad "candidate networks: '$NW'"

echo "== routine versions (docs/wip/v2-design.md section 1): the folder layout, the states, the operator's controls, the freeze, the rename"
# e2e/versioned is a routine folder with v1 to v6; v1 went live at the first start and the rest arrived as shadows.
vq() {  # vq ROUTINE 'python expression over v, the routine's versions answer': print it
  cli versions "$1" -json | python3 -c "import json,sys; v=json.load(sys.stdin); print($2)"
}
vstates() { vq "$1" "' '.join('%d:%s' % (x['version'], x['state']) for x in v['versions'])"; }
vrow() { cli versions "$1" -json | python3 -c "import json,sys; v=json.load(sys.stdin); x=next(y for y in v['versions'] if y['version'] == $2); print(x$3)"; }
runs_json() { cli runs "$1" -json -n 100; }
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
# Moving it to the folder layout changes its recipe: refused, the way out named, until the operator re-records it (operators only, with a reason).
FD="$WORK/config/specs/e2e"
rm "$FD/flat-playbook.yaml"; mkdir -p "$FD/flat-playbook/v1"
cat > "$FD/flat-playbook/routine.yaml" <<EOF
fleet: e2e
routine: flat-playbook
description: The e2e's flat-routine check, moved to the folder layout.
enabled: false
worker: triage
trigger:
  every: 60m
autonomy: record
budget:
  day_usd: 0
freshness: 120m
EOF
cat > "$FD/flat-playbook/v1/spec.yaml" <<EOF
run:
  command: "true"
  workdir: .
  timeout: 1m
prompts:
  system: system.md
EOF
cp "$FPB" "$FD/flat-playbook/v1/system.md"; chmod -R a+rX "$FD"
OUT="$($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1)"; RC=$?
[ $RC = 2 ] && echo "$OUT" | grep >/dev/null "^versions: e2e/flat-playbook v1 is live and its folder changed" \
  && echo "$OUT" | grep >/dev/null "ship the change as v2 .*flotilla versions rerecord e2e/flat-playbook v1 --reason R" \
  && ok "moved to the folder layout, the routine is refused by -check, the way out named (exit $RC)" || bad "-check on the folder move: rc=$RC $(echo "$OUT" | grep -E '^versions' | head -2)"
OUT="$(agent versions rerecord e2e/flat-playbook v1 --reason "e2e: an agent tries" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 403" && ok "an agent cannot re-record a version ($OUT)" || bad "agent rerecord: $OUT"
OUT="$(cli versions rerecord e2e/flat-playbook v1 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 400.*needs a reason" && ok "a re-record without a reason is refused" || bad "rerecord without a reason: $OUT"
OUT="$(cli versions rerecord e2e/flat-playbook v2 --reason "e2e: no such version" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 404" && ok "a re-record of a version that is not there is refused (404)" || bad "rerecord v2: $OUT"
OUT="$(cli versions rerecord e2e/versioned v3 --reason "e2e: a shadow follows its folder" 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 409.*is a shadow" && ok "a shadow is not re-recorded (409)" || bad "rerecord a shadow: $OUT"
OUT="$(cli versions rerecord e2e/flat-playbook v1 --reason "e2e: moved to the folder layout" 2>&1)"
NEWH="$(echo "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin)["new_hash"])' 2>/dev/null)"
[ -n "$NEWH" ] && [ "$NEWH" != "$FH2" ] && echo "$OUT" | grep >/dev/null "\"old_hash\":\"$FH2\"" && echo "$OUT" | grep >/dev/null '"by":"operator"' \
  && ok "the operator re-recorded v1 from the folder as it is now: old and new hash in the answer" || bad "rerecord: $OUT"
cli events -n 40 | grep >/dev/null "operator .*version.rerecord .*e2e/flat-playbook@v1 .*live v1 re-recorded (loaded as folder).*$FH2 -> $NEWH.*e2e: moved to the folder layout" \
  && ok "the event log records the re-record: who, the old and new hash, what changed and why" || bad "rerecord event: $(cli events -n 40 | grep rerecord | head -2)"
$D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check >/dev/null 2>&1 \
  && ok "after the re-record, -check passes over the folder layout" || bad "-check after the re-record: $($D exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check 2>&1 | grep '^versions' | head -2)"
$D restart flotillad >/dev/null 2>&1
for i in $(seq 1 45); do cli health >/dev/null 2>&1 && break; sleep 2; done
[ "$(vstates e2e/flat-playbook)" = "1:live" ] && [ "$(vrow e2e/flat-playbook 1 "['hash']")" = "$NEWH" ] \
  && ok "flotillad starts on the folder layout: v1 still live, with the re-recorded hash" || bad "after the folder move: $(vstates e2e/flat-playbook) $($D logs flotillad 2>&1 | grep versions | tail -2)"

echo "== a changed config recreates its service (deploy.sh's FLOTILLA_CONFIG_HASH_*)"
started() { $D inspect "$1" --format '{{.State.StartedAt}}' 2>&1; }
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
hseen() { for _ in $(seq 1 30); do hostlog | grep >/dev/null "$1" && return 0; sleep 1; done; return 1; }
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
cli action autonomy host.true propose --reason "e2e: the security e2e's harmless action" >/dev/null 2>&1
cli action list 2>&1 | grep >/dev/null "^host.true  *propose " && ok "host.true promoted to propose (operator, with a reason)" || bad "promote: $(cli action list 2>&1)"
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
sleep 3
seen flotillad '"msg":"host endpoint: refused"' && hostlog | grep >/dev/null 'flotillad refused: refused' \
  && ok "its reconnects are refused" || bad "after revoke: $(hostlog | tail -3)"
cli host list | grep >/dev/null "^e2e-host  *no .* -  *[0-9-]*T[0-9:.]*Z " && ok "host list shows it revoked, not connected, no certificate" || bad "host list after revoke: $(cli host list 2>&1)"
hx flotilla-e2e-host sh -c 'pkill -u _flotilla flotilla-host; true'
OUT="$(hx flotilla-e2e-host bash /stage/remote-install.sh /stage remove 2>&1)"
R="$(hx flotilla-e2e-host sh -c 'ls -d /usr/local/libexec/flotilla-host /etc/flotilla-host /var/db/flotilla-host /etc/sudoers.d/flotilla-host /var/log/flotilla-host/audit.jsonl 2>/dev/null' | tr '\n' ' ')"
echo "$OUT" | grep >/dev/null "^REMOVE PASS" && [ "$R" = "/var/log/flotilla-host/audit.jsonl " ] \
  && ok "remove: binary, config, state and sudoers gone; the audit log stays" || bad "remove: $OUT / left: $R"
$D rm -f flotilla-e2e-host >/dev/null

echo "== shadow versions (v2 step 2): a shadow runs beside the live version on its own worker, its decisions are paired, and it can do no harm"
# shadows are off until now; turn them on in flotillad's config and restart
printf 'shadows: {enabled: true}\n' >> "$WORK/config/flotillad.yaml"   # appended in place: the bind mount keeps its file
free_routine() {  # take a routine off disabled_routines and restart flotillad: its first run is at start, and starts its shadows
  sed "s|, $1||" "$WORK/config/flotillad.yaml" > "$WORK/config/flotillad-free.yaml" && cat "$WORK/config/flotillad-free.yaml" > "$WORK/config/flotillad.yaml"
  $D restart flotillad >/dev/null
  for i in $(seq 1 30); do cli health >/dev/null 2>&1 && break; sleep 2; done
}
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
texts = db.execute("SELECT COUNT(*) FROM call_texts").fetchone()[0]
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
  [ -d "$OFF" ] && $D run --rm --user 10001:10001 -v "$OFF:/x" --entrypoint sh "local/flotilla-worker-triage:$TAG" -c 'rm -rf /x/out /x/out-known /x/out-unknown' >/dev/null 2>&1
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
off_clean

echo "== users and the UI listener (ADR-0014); last, since the first admin retires the operator token"
UA="$(cli user add ada --role admin 2>&1)"
ADMIN="$(echo "$UA" | sed -n 's/.*"token":"\(flt_[0-9a-f]*\)".*/\1/p')"
[ ${#ADMIN} = 68 ] && ok "the first admin, with the operator token (a 256-bit token, shown once)" || bad "first admin: $UA"
OUT="$(cli status 2>&1)"
echo "$OUT" | grep >/dev/null "HTTP 403" && ok "after it, the operator token is retired" || bad "operator token after the first admin: $OUT"
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

echo "== hardening"
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
$D rm -f flotilla-e2e-remote-worker >/dev/null

for w in flotilla-worker-ops-read flotilla-worker-ops-act-host flotilla-worker-bugfix flotilla-worker-bugfix-act; do
  NW="$($D inspect $w --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
  [ "$NW" = "${P}_flotilla " ] && ok "$w is on the internal network only" || bad "$w networks: '$NW'"
done
for c in flotillad flotilla-worker-triage flotilla-worker-triage-candidate flotilla-worker-triage-shadow flotilla-egress-shadow flotilla-worker-ops-read flotilla-worker-ops-act-host flotilla-worker-bugfix flotilla-worker-bugfix-act flotilla-egress flotilla-egress-bugfix; do
  RO=$($D inspect "$c" --format '{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.Config.User}}')
  case "$RO" in "true [ALL] "*) ok "$c: read-only root, all capabilities dropped, user ${RO##* }" ;; *) bad "$c hardening: $RO" ;; esac
done
NW="$($D inspect flotilla-worker-triage --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
[ "$NW" = "${P}_flotilla " ] && ok "flotilla-worker-triage is on the internal network only" || bad "flotilla-worker-triage networks: '$NW'"
NW="$($D inspect flotillad --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>&1)"
[ "$NW" = "e2e-ui ${P}_flotilla ${P}_shadow " ] && ok "flotillad is on the internal flotilla and shadow networks and the UI network only" || bad "flotillad networks: '$NW'"
NW="$($D network inspect e2e-ui --format '{{.Internal}}' 2>&1)"
[ "$NW" = true ] && ok "the UI network is internal (no route out)" || bad "ui network internal=$NW"
NW="$($D network inspect ${P}_flotilla --format '{{.Internal}}' 2>&1)"
[ "$NW" = true ] && ok "the flotilla network is internal (no route out)" || bad "flotilla network internal=$NW"
NW="$($D network inspect ${P}_shadow --format '{{.Internal}}' 2>&1)"
[ "$NW" = true ] && ok "the shadow network is internal (no route out)" || bad "shadow network internal=$NW"

if [ "${KEEP:-}" != 1 ]; then down; fi
if [ "$fails" -eq 0 ]; then echo "E2E PASS"; else echo "E2E FAIL ($fails)"; exit 1; fi
