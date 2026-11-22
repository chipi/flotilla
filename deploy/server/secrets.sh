#!/usr/bin/env bash
# Fill flotilla's secret volumes from the host's files, and (once) seed a triage state volume.
#
#   secrets.sh <image tag>                     copy the secrets into their volumes (every deploy)
#   secrets.sh <image tag> --seed-state VOLUME DIR
#                                              copy DIR (the triage fleet's ~/signal-fleet, without any
#                                              env file, its secrets and their backups, and without its
#                                              STOP flag) into VOLUME, if it is empty
#
# Each file is written by a throwaway container of our own worker image, with no network, as root
# inside it only to set the owner: mode 600, owned by the uid of the container that reads it
# (flotillad 65532, the worker 10001). Nothing secret passes through an environment variable, a
# command line or the VM's sshfs mount: the bytes go over the docker CLI's stdin.
#
# Reads FLOTILLA_HOME/.env (FLOTILLA_CONFIG, TRIAGE_ENV_FILE, and for the shadow worker TRIAGE_READ_ENV_FILE). In FLOTILLA_CONFIG: cli.token and
# worker-triage.token.
set -euo pipefail
export PATH=/usr/local/bin:/usr/bin:/bin:$PATH
H="${FLOTILLA_HOME:-$HOME/flotilla}"
TAG="${1:?usage: secrets.sh sha-<7> [--seed-state VOLUME DIR]}"
set -a; . "$H/.env"; set +a
IMG="${FLOTILLA_REGISTRY:-ghcr.io/chipi}/flotilla-worker-triage:$TAG"
D="${FLOTILLA_DOCKER:-docker} ${DOCKER_CONTEXT:+--context $DOCKER_CONTEXT}"   # FLOTILLA_DOCKER: a stub, in the tests
V="${FLOTILLA_VOLUME_PREFIX:-flotilla}"

put() {  # put <volume> <name in volume> <owner uid> <host file>
  [ -s "$4" ] || { echo "SECRETS FAIL: $4 is missing or empty"; exit 1; }
  $D volume create "$1" >/dev/null
  $D run --rm -i --network none --user 0 --entrypoint sh -v "$1:/s" "$IMG" \
    -c "umask 077 && cat > /s/$2.tmp && chown $3:$3 /s/$2.tmp && mv /s/$2.tmp /s/$2" < "$4"
  echo "  $1/$2 <- $(basename "$4") (600, uid $3)"
}

if [ "${2:-}" = --seed-state ]; then
  VOL="${3:?volume}"; DIR="${4:?dir}"
  [ -d "$DIR" ] || { echo "SEED FAIL: $DIR is not a directory"; exit 1; }
  $D volume create "$VOL" >/dev/null
  if [ -n "$($D run --rm --network none --entrypoint sh -v "$VOL:/s" "$IMG" -c 'ls -A /s')" ]; then
    echo "SEED SKIP: $VOL is not empty (it is seeded once; remove it to seed again)"; exit 0
  fi
  # every env file and its backups (fleet-gateway.env.bak-*, fleet.env.bak-*) stays out: they are secrets
  tar -C "$DIR" --exclude '*.env' --exclude '*.env.*' --exclude ./STOP -cf - . \
    | $D run --rm -i --network none --user 0 --entrypoint sh -v "$VOL:/s" "$IMG" \
        -c 'tar -xf - -C /s && chown -R 10001:10001 /s && echo "  $(find /s -type f | wc -l) files, $(du -sh /s | cut -f1)"'
  echo "SEED PASS: $VOL from $DIR"
  exit 0
fi

echo "== secrets"
put $V-flotillad-secrets cli.token 65532 "$FLOTILLA_CONFIG/cli.token"
put $V-flotillad-secrets worker-triage.token 65532 "$FLOTILLA_CONFIG/worker-triage.token"
put $V-worker-triage-secrets worker.token 10001 "$FLOTILLA_CONFIG/worker-triage.token"
put $V-worker-triage-secrets fleet.env 10001 "$TRIAGE_ENV_FILE"
# the triage candidate worker (compose profile candidate), where its token exists: its own worker token (flotillad's workers' listener
# names a caller by its token, and flotillad refuses two workers that share one), and the same fleet env as the live worker
if [ -s "$FLOTILLA_CONFIG/worker-triage-candidate.token" ]; then
  put $V-flotillad-secrets worker-triage-candidate.token 65532 "$FLOTILLA_CONFIG/worker-triage-candidate.token"
  put $V-worker-triage-candidate-secrets worker.token 10001 "$FLOTILLA_CONFIG/worker-triage-candidate.token"
  put $V-worker-triage-candidate-secrets fleet.env 10001 "$TRIAGE_ENV_FILE"
fi
# the triage shadow worker (compose profile shadow), where its token exists: its own worker token (never the live worker's) and the READ
# environment only: TRIAGE_READ_ENV_FILE in .env, a file of the credentials that can read. Without it the shadow worker has no credentials,
# and its runs fail at their first call, which is safe; the live worker's fleet.env (which holds the write credentials) never goes in.
if [ -s "$FLOTILLA_CONFIG/worker-triage-shadow.token" ]; then
  put $V-flotillad-secrets worker-triage-shadow.token 65532 "$FLOTILLA_CONFIG/worker-triage-shadow.token"
  put $V-worker-triage-shadow-secrets worker.token 10001 "$FLOTILLA_CONFIG/worker-triage-shadow.token"
  if [ -n "${TRIAGE_READ_ENV_FILE:-}" ]; then
    put $V-worker-triage-shadow-secrets read.env 10001 "$TRIAGE_READ_ENV_FILE"
  else
    echo "  $V-worker-triage-shadow-secrets: no TRIAGE_READ_ENV_FILE in .env, the shadow worker gets no credentials"
  fi
fi
# the deploy stand-down's caller (deploy/server/standdown.sh), where its token exists
if [ -s "$FLOTILLA_CONFIG/deploy.token" ]; then
  put $V-flotillad-secrets deploy.token 65532 "$FLOTILLA_CONFIG/deploy.token"
fi
# the ops workers (compose profile ops), where their tokens exist: a host without them is unaffected
for w in ops-read ops-read-shadow ops-act-host; do
  if [ -s "$FLOTILLA_CONFIG/worker-$w.token" ]; then
    put $V-flotillad-secrets worker-$w.token 65532 "$FLOTILLA_CONFIG/worker-$w.token"
    put $V-worker-$w-secrets worker.token 10001 "$FLOTILLA_CONFIG/worker-$w.token"
  fi
done
# the bugfix worker (compose profile bugfix, RFC-0010), where its token exists: its token and its model
# key (FLOTILLA_CONFIG/bugfix-model.key, the LiteLLM fleet-bugfix virtual key, alone in its file)
if [ -s "$FLOTILLA_CONFIG/worker-bugfix.token" ]; then
  put $V-flotillad-secrets worker-bugfix.token 65532 "$FLOTILLA_CONFIG/worker-bugfix.token"
  put $V-worker-bugfix-secrets worker.token 10001 "$FLOTILLA_CONFIG/worker-bugfix.token"
  put $V-worker-bugfix-secrets model.key 10001 "$FLOTILLA_CONFIG/bugfix-model.key"
fi
# the bugfix pusher, where its token exists; the GitHub App's key once the operator has made the App
# (without it bugfix/open-pr escalates)
if [ -s "$FLOTILLA_CONFIG/worker-bugfix-act.token" ]; then
  put $V-flotillad-secrets worker-bugfix-act.token 65532 "$FLOTILLA_CONFIG/worker-bugfix-act.token"
  put $V-worker-bugfix-act-secrets worker.token 10001 "$FLOTILLA_CONFIG/worker-bugfix-act.token"
  if [ -s "$FLOTILLA_CONFIG/bugfix-github-app.pem" ]; then
    put $V-worker-bugfix-act-secrets github-app.pem 10001 "$FLOTILLA_CONFIG/bugfix-github-app.pem"
  fi
fi
# the marketing worker (compose profile seo, RFC-0012), where its token exists
if [ -s "$FLOTILLA_CONFIG/worker-marketing.token" ]; then
  put $V-flotillad-secrets worker-marketing.token 65532 "$FLOTILLA_CONFIG/worker-marketing.token"
  put $V-worker-marketing-secrets worker.token 10001 "$FLOTILLA_CONFIG/worker-marketing.token"
  # its read credentials (docs/operations/seo.md), where they exist: the view-only Umami user and the
  # Search Console service account's key, in FLOTILLA_HOME/marketing
  for f in umami.env gsc.json; do
    if [ -s "$H/marketing/$f" ]; then
      put $V-worker-marketing-secrets "$f" 10001 "$H/marketing/$f"
    fi
  done
fi
# plumbing.call's worker (compose profile plumbing, RFC-0014 §4.6), where its token exists, and each callback's
# signing secret, $FLOTILLA_CONFIG/plumbing/<name>.secret (worker-plumbing.env names it as
# /run/flotilla/plumbing-<name>.secret), shared with the plumbing tool that verifies the callback
if [ -s "$FLOTILLA_CONFIG/worker-plumbing.token" ]; then
  put $V-flotillad-secrets worker-plumbing.token 65532 "$FLOTILLA_CONFIG/worker-plumbing.token"
  put $V-worker-plumbing-secrets worker.token 10001 "$FLOTILLA_CONFIG/worker-plumbing.token"
  for f in "$FLOTILLA_CONFIG"/plumbing/*.secret; do
    [ -e "$f" ] || continue
    put $V-worker-plumbing-secrets "plumbing-$(basename "$f")" 10001 "$f"
  done
fi
# optional: flotillad's GlitchTip DSN (flotillad.yaml errors_dsn_file: /run/flotilla/errors.env)
if [ -n "${FLOTILLA_ERRORS_DSN_FILE:-}" ]; then
  put $V-flotillad-secrets errors.env 65532 "$FLOTILLA_ERRORS_DSN_FILE"
fi
# optional: the DSN of the GlitchTip project that takes the shadow runs' errors, apart from the live ones
# (flotillad.yaml errors_shadow_dsn_file: /run/flotilla/errors-shadow.env)
if [ -n "${FLOTILLA_ERRORS_SHADOW_DSN_FILE:-}" ]; then
  put $V-flotillad-secrets errors-shadow.env 65532 "$FLOTILLA_ERRORS_SHADOW_DSN_FILE"
fi
# optional: each hook's signing secret (RFC-0014 §4.2; flotillad.yaml hooks: secret_file: /run/flotilla/hook-<name>.secret),
# one $FLOTILLA_CONFIG/hooks/<name>.secret per hook, shared with the plumbing tool that signs its calls
if [ -d "$FLOTILLA_CONFIG/hooks" ]; then
  for f in "$FLOTILLA_CONFIG"/hooks/*.secret; do
    [ -e "$f" ] || continue
    put $V-flotillad-secrets "hook-$(basename "$f")" 65532 "$f"
  done
fi
# optional: the host agents' intermediate CA (flotillad.yaml hosts.ca_dir: /run/flotilla), from
# `flotilla ca intermediate ROOTDIR $FLOTILLA_CONFIG/host-ca`; the root's key is never among them
if [ -s "$FLOTILLA_CONFIG/host-ca/intermediate.key" ]; then
  [ ! -e "$FLOTILLA_CONFIG/host-ca/root.key" ] || { echo "SECRETS FAIL: root.key is in $FLOTILLA_CONFIG/host-ca: it stays offline"; exit 1; }
  for f in intermediate.crt intermediate.key root.crt; do
    put $V-flotillad-secrets "$f" 65532 "$FLOTILLA_CONFIG/host-ca/$f"
  done
fi
# optional: the execution-signing key (hosts.signing_key_file: /run/flotilla/signing.key), from
# `flotilla ca signing-key $FLOTILLA_CONFIG/host-ca ID`
if [ -s "$FLOTILLA_CONFIG/host-ca/signing.key" ]; then
  put $V-flotillad-secrets signing.key 65532 "$FLOTILLA_CONFIG/host-ca/signing.key"
fi
# the triage state volume is external to compose too: make sure it exists (an empty one takes the
# image's directory owner on first mount)
$D volume create "${TRIAGE_STATE_VOLUME:-flotilla-triage-state-shadow}" >/dev/null
echo "SECRETS PASS"
