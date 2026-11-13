# ADR-0003 — Dependencies, spec format and base images for flotillad

**Status:** Accepted (operator, 2026-11-08). *Pending additions from [ADR-0005](ADR-0005-agent-pipeline.md)
(2026-11-09), each approved when it is built: `jsonschema` and what it pulls, pinned by hash; a vector
store for semantic search; a Slack Socket Mode client.*
**Date:** 2026-11-08

## Context

Phase 2 builds flotillad (RFC-0001 §9). ADR-0001 chose Go with SQLite, and the docs promise traces in
VictoriaTraces and specs that people write. Go's standard library has no SQLite driver, no YAML and
no OpenTelemetry, and every new dependency needs the operator's yes (global rule 12). ADR-0002 needs
base images for the containers.

## Decision

| Need | Choice | Why |
| --- | --- | --- |
| SQLite | `modernc.org/sqlite` | pure Go: no C toolchain, builds into a static binary on a distroless image |
| Spec and config format | YAML, `gopkg.in/yaml.v3`, strict (unknown fields are errors) | specs are written by people; comments help. The Python side reads no YAML: flotillad validates the `models` section and hands it to every run as JSON in `FLOTILLA_MODELS` (operator, 2026-11-09), so no PyYAML |
| Errors | `github.com/getsentry/sentry-go` (operator, 2026-11-08): flotillad's own ERROR log records to GlitchTip, one issue per message | the Sentry protocol GlitchTip speaks; no own protocol code |
| Traces | the OpenTelemetry Go SDK and its OTLP/HTTP exporter | the standard; correct propagation and batching. To VictoriaTraces' `/insert/opentelemetry/v1/traces` |
| Traces from jobs | the OpenTelemetry Python SDK and its OTLP/HTTP exporter in the worker image (operator, 2026-11-09): `opentelemetry-sdk` and `opentelemetry-exporter-otlp-proto-http` 1.45.2, 16 packages with their dependencies (`deploy/docker/worker-requirements.txt`), every one pinned with its wheel's sha256 and installed `--require-hashes --no-deps` | the model client's span per call, under the run's trace (TRACEPARENT from flotillad); the same standard as flotillad. A hand-written OTLP encoder was the alternative, rejected: protocol code we would own |
| Images | build: `golang:1.26`; run: `gcr.io/distroless/static-debian12:nonroot` (flotillad, Smokescreen); workers: `python:3.12-slim` | official, small, non-root; glibc for later Python wheels (DuckDB, Parquet). All pinned by digest in the Dockerfiles |
| Egress proxy | Smokescreen, built from a pinned commit (ADR-0002) | |
| UI fonts ([ADR-0014](ADR-0014-operator-ui.md)) | Inter 4.1 (`InterVariable.woff2`) and JetBrains Mono 2.304 (Regular, SemiBold), from the upstream release zips, under the SIL OFL 1.1, embedded in flotillad (`internal/ui/static/fonts/`, whose README has the zips' sha256) | served by flotillad itself: nothing fetched from a third party; about 540 KB |
| The e2e's browser check (operator, 2026-11-13; ADR-0014) | Playwright for Python 1.65.1 on Microsoft's `mcr.microsoft.com/playwright/python` v1.65.1-noble image, pinned by digest (`deploy/e2e/browser.Dockerfile`); Caddy 2.11.9 by digest as its TLS front (`deploy/e2e/Caddyfile`) | e2e only, never in a shipped image. A real Chromium shows what HTTP checks cannot (the Secure cookie kept, forms with their CSRF field, the CSP, the fonts); Caddy stands in for the homelab's caddy-tailscale, so the browser meets the deployment's shape (https, Host without a port) |

What `go mod tidy` brought (2026-11-08): the three direct modules above, plus 27 indirect ones. The
largest are gRPC and protobuf (the OTLP exporter's types) and `modernc.org/libc` (the SQLite
translation). They are pinned in `go.sum`.

## Consequences

- **Positive:** a static flotillad of about 13 MB; one store file; traces on the homelab's
  conventions; strict specs that reject a typo.
- **Negative:** 30 modules to keep current, the OTel ones most of all; `modernc.org/sqlite` is
  slower than the C driver (irrelevant at one row per run).
- **Neutral:** updating any of them is a reviewed change, with the reason in the commit.

## Alternatives considered

- **mattn/go-sqlite3:** needs CGO and a libc in the runtime image.
- **JSONL files instead of SQLite:** no dependency, but the store would be written twice.
- **JSON specs:** no dependency, but no comments for people writing them.
- **A hand-written OTLP/JSON exporter:** no dependency, but our own code for a published protocol.
- **Alpine images:** smaller, but musl differences bite Python wheels later.
