# The marketing worker (RFC-0012): the seo fleet's routines, on the host. Python stdlib only; it holds its
# worker token and, from the Search Console routine on, the fleet's read credentials, nothing else.
FROM python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
RUN groupadd -g 10001 worker && useradd -u 10001 -g 10001 -m -d /home/worker worker
COPY worker /opt/flotilla/worker
COPY seo /opt/flotilla/seo
COPY fleets /opt/flotilla/fleets
ARG VERSION=dev
ENV FLOTILLA_VERSION=${VERSION} \
    FLOTILLA_WORKER=marketing \
    FLOTILLA_CODE_ROOT=/opt/flotilla \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HOME=/home/worker
USER 10001:10001
WORKDIR /home/worker
CMD ["python3", "/opt/flotilla/worker/flotilla_worker.py"]
