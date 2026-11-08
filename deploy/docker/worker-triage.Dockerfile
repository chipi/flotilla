# The triage fleet's worker: the worker service, the heartbeat, and the triage fleet's code (stdlib
# Python, no packages installed). Build context: the repo root.
#   docker build -f deploy/docker/worker-triage.Dockerfile --build-arg VERSION=sha-<7> -t flotilla-worker-triage:sha-<7> .
FROM python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
RUN groupadd -g 10001 worker && useradd -u 10001 -g 10001 -m -d /home/worker worker
COPY worker /opt/flotilla/worker
COPY signal-fleet/mvp /opt/flotilla/signal-fleet/mvp
ARG VERSION=dev
ENV FLOTILLA_VERSION=${VERSION} \
    FLOTILLA_WORKER=triage \
    FLOTILLA_CODE_ROOT=/opt/flotilla \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HOME=/home/worker
USER 10001:10001
WORKDIR /home/worker
CMD ["python3", "/opt/flotilla/worker/flotilla_worker.py"]
