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
# Reads FLOTILLA_HOME/.env (FLOTILLA_CONFIG, TRIAGE_ENV_FILE). In FLOTILLA_CONFIG: cli.token and
# worker-triage.token.
set -euo pipefail
export PATH=/usr/local/bin:/usr/bin:/bin:$PATH
H="${FLOTILLA_HOME:-$HOME/flotilla}"
TAG="${1:?usage: secrets.sh sha-<7> [--seed-state VOLUME DIR]}"
set -a; . "$H/.env"; set +a
IMG="${FLOTILLA_REGISTRY:-ghcr.io/chipi}/flotilla-worker-triage:$TAG"
D="docker ${DOCKER_CONTEXT:+--context $DOCKER_CONTEXT}"
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
# optional: flotillad's GlitchTip DSN (flotillad.yaml errors_dsn_file: /run/flotilla/errors.env)
if [ -n "${FLOTILLA_ERRORS_DSN_FILE:-}" ]; then
  put $V-flotillad-secrets errors.env 65532 "$FLOTILLA_ERRORS_DSN_FILE"
fi
# the triage state volume is external to compose too: make sure it exists (an empty one takes the
# image's directory owner on first mount)
$D volume create "${TRIAGE_STATE_VOLUME:-flotilla-triage-state-shadow}" >/dev/null
echo "SECRETS PASS"
