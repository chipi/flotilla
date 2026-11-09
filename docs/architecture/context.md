# Context

flotilla is one service in a homelab. It owns no models, no observability stack and no data
sources. It uses the ones that exist and is careful about what it may change.

## Who uses it

| Actor | Through | May |
|---|---|---|
| **The operator** | the UI, the CLI | everything, including approving outward actions, changing stages and budgets, and pausing or stopping |
| **Agents** (Claude sessions, the X3 assistant, later others) | MCP | read status, runs, the ledger and findings; dry-run or trigger a job; pause; add notes or proposals. **Never approve.** |
| **Jobs** (the fleets' own runs) | the API, with a fleet identity | report runs and decisions; act within what their spec and stage allow |

## What it talks to

| System | Direction | Why | Notes |
|---|---|---|---|
| LiteLLM gateway | out | every model call | the reference OpenAI-compatible endpoint ([RFC-0002](../rfc/RFC-0002-job-runtime.md) §3); per-fleet virtual key and budget; local models first |
| Local models (X3 Qwen3-30B in Ollama, DGX Qwen3-30B in vLLM) | via LiteLLM | reasoning in agent steps | the DGX endpoint has no tool calling yet |
| VictoriaMetrics, VictoriaLogs, VictoriaTraces | in (read) | evidence for triage and ops jobs | read-only tools |
| VictoriaMetrics, VictoriaLogs, VictoriaTraces | out (write) | flotilla's own metrics, logs, traces | through the host's Alloy collector |
| Grafana | in (read) | alerts (triage) | read-only API token |
| Grafana | out | dashboards and alert rules for flotilla | provisioned from this repo |
| GlitchTip | in (read) | errors to triage | read token |
| GlitchTip | out | flotilla's own errors; triage comments | separate write token |
| Langfuse | out | LLM traces | through LiteLLM's callback, plus the run's trace id |
| GitHub | out | issues, comments, labels, branches, PRs | a GitHub App or tokens per fleet, scoped to the repos in config |
| Google Search Console, Bing Webmaster | in (read) | the SEO fleet | read-only scopes |
| Umami | in (read) | usage for SEO and ops | read-only |

Everything is reached over the tailnet or on the host. flotilla's own API, UI and MCP listen on the
tailnet only.

## What it does not do

- **Host models.** It never calls a cloud model unless a job's model policy allows it, and then only
  through LiteLLM with a budget.
- **Publish to the outside world without a human,** except what a job's spec explicitly allows at
  its stage. The triage fleet filing an issue in `propose` is an example, as it does today.
- **Store secrets, findings or data in git.**
- **Replace the observability stack or the Hub.** It reports into them.
