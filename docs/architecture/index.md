# Architecture

How flotilla is built, at four levels of detail. The decisions behind it are in
[ADR-0001](../adr/ADR-0001-platform-foundations.md), and the reasoning is in
[RFC-0001](../rfc/RFC-0001-fleet-platform.md).

| Page | What it answers |
|---|---|
| [Context](context.md) | What flotilla talks to, and who uses it |
| [Components](components.md) | What the control plane and the job runtime are made of |
| [A run, end to end](run-lifecycle.md) | What happens from a trigger to a decision, an approval and an outcome |
| [Data](data.md) | What is stored where: specs, config, state, the ledger |
| [Deployment](deployment.md) | How it runs on the homelab mini (Docker, proposed in [ADR-0002](../adr/ADR-0002-run-in-docker.md)) |
| [Security](../security.md) | The threat model and the controls; designed in from the start |
| [Testing and validation](../testing.md) | The test pyramid, validating the platform on its own, validating each fleet as it runs |

## In one picture

```
 operator ──► UI / CLI ─┐                             ┌─► LiteLLM ─► local models (X3, DGX)
 agents ────► MCP ──────┤                             │
                        ▼                             │
                 ┌─────────────┐   starts runs   ┌────┴─────────┐   reads    ┌────────────────────┐
                 │  flotillad  │ ──────────────► │ fleet worker │ ─────────► │ sources: Grafana,  │
                 │ (Go)        │ ◄────────────── │ (Python job  │            │ VictoriaMetrics,   │
                 │ specs, runs │  decisions,     │  runtime)    │            │ GlitchTip, GitHub, │
                 │ ledger,     │  events         └────┬─────────┘            │ Search Console,    │
                 │ approvals   │                      │ writes (only        │ Umami …            │
                 └─────┬───────┘                      │ via outputs)        └────────────────────┘
                       │                              ▼
                       │                       GitHub issues/PRs, pages, pushes
                       ▼                       (when the stage allows, or approved)
     metrics, logs, traces, errors ─► the homelab observability stack
```

## Vocabulary

| Term | Meaning |
|---|---|
| fleet | a group of jobs with one purpose: `triage`, `bugfix`, `seo`, `ops` |
| job | a declared unit of work: trigger, steps, output, stage, budget |
| run | one execution of a job |
| step | `collect`, `detect`, `agent`, `gate`, `output` |
| building block | named code a step uses: a source, detector, tool, gate, output |
| playbook | the agent step's instructions, a Markdown file in git |
| decision | what a run concluded about one subject, with evidence: one ledger row |
| proposal | a decision that wants an outward action |
| stage | `shadow`, `propose`, `live`: how much a job may do without approval |
