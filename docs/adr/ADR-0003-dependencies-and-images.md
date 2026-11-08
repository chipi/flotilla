# ADR-0003 — Dependencies, spec format and base images for flotillad

**Status:** Accepted (operator, 2026-11-08)
**Date:** 2026-11-08

## Context

Phase 2 builds flotillad (RFC-0001 §9). ADR-0001 chose Go with SQLite, and the docs promise traces in
VictoriaTraces and specs that people write. Go's standard library has no SQLite driver, no YAML and
no OpenTelemetry, and every new dependency needs the operator's yes (global rule 12). ADR-0002 needs
base images for the containers.

## Decision

| Need | Choice | Why |
|---|---|---|
| SQLite | `modernc.org/sqlite` | pure Go: no C toolchain, builds into a static binary on a distroless image |
| Spec and config format | YAML, `gopkg.in/yaml.v3`, strict (unknown fields are errors) | specs are written by people; comments help. The Python side does not read specs in phase 2, so no PyYAML yet |
| Traces | the OpenTelemetry Go SDK and its OTLP/HTTP exporter | the standard; correct propagation and batching. To VictoriaTraces' `/insert/opentelemetry/v1/traces` |
| Images | build: `golang:1.26`; run: `gcr.io/distroless/static-debian12:nonroot` (flotillad, Smokescreen); workers: `python:3.12-slim` | official, small, non-root; glibc for later Python wheels (DuckDB, Parquet). All pinned by digest in the Dockerfiles |
| Egress proxy | Smokescreen, built from a pinned commit (ADR-0002) | |

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
