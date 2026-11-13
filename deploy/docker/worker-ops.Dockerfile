# The ops worker (ADR-0005 decision 5: workers per role): one image, run as two services, ops-read (the
# monitors) and ops-act-host (the acting routines; it holds no host key: flotillad signs executions,
# ADR-0006). Python stdlib only. The registry's declaration is in the image so the acting routine reads
# its preconditions from the same file flotillad does.
FROM python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
RUN groupadd -g 10001 worker && useradd -u 10001 -g 10001 -m -d /home/worker worker
COPY worker /opt/flotilla/worker
COPY ops /opt/flotilla/ops
COPY host/registry.json /opt/flotilla/host/registry.json
ARG VERSION=dev
ENV FLOTILLA_VERSION=${VERSION} \
    FLOTILLA_WORKER=ops \
    FLOTILLA_CODE_ROOT=/opt/flotilla \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HOME=/home/worker
USER 10001:10001
WORKDIR /home/worker
CMD ["python3", "/opt/flotilla/worker/flotilla_worker.py"]
