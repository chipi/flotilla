# The triage fleet's worker: the worker service, the heartbeat, the job runtime (runtime/) and the triage
# fleet's code. Python packages: only worker-requirements.txt (the OpenTelemetry SDK, ADR-0003), pinned by
# hash. Build context: the repo root.
#   docker build -f deploy/docker/worker-triage.Dockerfile --build-arg VERSION=sha-<7> -t flotilla-worker-triage:sha-<7> .
FROM python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
RUN groupadd -g 10001 worker && useradd -u 10001 -g 10001 -m -d /home/worker worker \
 && install -d -o 10001 -g 10001 /home/worker/signal-fleet /snapshots   # the state and snapshots volumes take this owner
COPY deploy/docker/worker-requirements.txt /tmp/worker-requirements.txt
RUN pip install --no-cache-dir --require-hashes --no-deps -r /tmp/worker-requirements.txt \
 && python3 -m pip check && rm /tmp/worker-requirements.txt
COPY worker /opt/flotilla/worker
COPY runtime /opt/flotilla/runtime
COPY signal-fleet/mvp /opt/flotilla/signal-fleet/mvp
# meetings/notes (RFC-0014 case 3): its model call, on this worker's model key
COPY meetings /opt/flotilla/meetings
# knowledge/answer (RFC-0014 case 1): runs on the reads worker, which is this image
COPY knowledge /opt/flotilla/knowledge
# ops/sop_investigate.py (operator step 1, C6): the SOP loop's investigation, on this worker's model key; it shares sop_act with the ops image
COPY ops /opt/flotilla/ops
# the specs, playbooks and fleet skills: the same bytes flotillad's -check saw (ADR-0012)
COPY fleets /opt/flotilla/fleets
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
