# The bugfix fleet's worker (RFC-0010 §4): the worker service, the bugfix routines (bugfix/), the
# bake-off's leaves they call (bugfix-fleet/bakeoff, with its agent definitions) and the coding harness,
# pi. The base is orrery's own Node image, at the digest orrery's Dockerfile pins, so a fix builds and
# tests on the toolchain orrery ships with. Debian's python3 runs the worker (stdlib only); git and jq
# are what the leaves need, procps their wall-clock cut (pkill -P on their own children).
#   docker build -f deploy/docker/worker-bugfix.Dockerfile --build-arg VERSION=sha-<7> -t flotilla-worker-bugfix:sha-<7> .
FROM node:22-bookworm-slim@sha256:de170b39439eb0e5a3444ad7d60b015614a381e10fecae2e30ec9e735122f38d
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 git jq procps ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 10001 worker && useradd -u 10001 -g 10001 -m -d /home/worker worker \
 && install -d -o 10001 -g 10001 /scratch   # an empty scratch volume takes this owner
# pi, the harness the bake-off measured (BAKEOFF.md §8), at the version installed on the mini; its
# npm-shrinkwrap.json pins what it pulls. No install scripts run.
RUN npm install -g --ignore-scripts --no-fund --no-audit @earendil-works/pi-coding-agent@0.80.10 \
 && npm cache clean --force && pi --version
COPY worker /opt/flotilla/worker
COPY bugfix /opt/flotilla/bugfix
COPY bugfix-fleet/bakeoff/triage_run.sh bugfix-fleet/bakeoff/advisor_run.sh bugfix-fleet/bakeoff/langfuse_push.py /opt/flotilla/bugfix-fleet/bakeoff/
COPY bugfix-fleet/bakeoff/harnesses/pi.sh /opt/flotilla/bugfix-fleet/bakeoff/harnesses/pi.sh
COPY bugfix-fleet/agents /opt/flotilla/bugfix-fleet/agents
COPY host/registry.json /opt/flotilla/host/registry.json
ARG VERSION=dev
ENV FLOTILLA_VERSION=${VERSION} \
    FLOTILLA_WORKER=bugfix \
    FLOTILLA_CODE_ROOT=/opt/flotilla \
    PI_OFFLINE=1 \
    PI_SKIP_VERSION_CHECK=1 \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 \
    NPM_CONFIG_UPDATE_NOTIFIER=false \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HOME=/home/worker
USER 10001:10001
WORKDIR /home/worker
CMD ["python3", "/opt/flotilla/worker/flotilla_worker.py"]
