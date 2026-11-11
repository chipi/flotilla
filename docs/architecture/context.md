# Context

flotilla runs small fleets of AI agents for one operator. It owns no models, no observability stack
and no data sources: it uses the ones that exist, and it is careful about what it may change.

## Who uses it

| Actor | Through | May |
| --- | --- | --- |
| **The operator** | the UI, the CLI; Slack for approvals | everything: approve, promote or lower autonomy, change budgets, hold, freeze, stop |
| **Agents** (other assistants and sessions) | MCP, with a per-caller token | read status, cases and findings; trigger or dry-run a routine; pause; propose notes and feedback. **Never approve.** |
| **Routines** (the fleets' own runs) | the API, with their worker's identity | open and update cases, plan writes, report results; act only as policy allows |
| **Host agents** | an outbound connection with a client certificate | receive signed executions their host allows; report results; relay a provider's messages and approval clicks |

## What it talks to

| System | Direction | Why | How |
| --- | --- | --- | --- |
| A model endpoint | out | every model call | any OpenAI-compatible API, through one model client; aliases per routine ([ADR-0004](../adr/ADR-0004-job-runtime.md)) |
| Signal sources (alerting, metrics, logs, traces, error tracking) | in (read) | detection and diagnosis | read capabilities with read-only credentials |
| Code hosting (GitHub) | in and out | issues, comments, PRs; repo docs as knowledge | per-role credentials; repos in config |
| Web analytics, Search Console | in (read) | the digital marketing fleet | read-only scopes |
| Knowledge sources | in (read) | each project's docs and runbooks; other MCP servers | declared per project, trusted or untrusted per path |
| A communication provider (Slack first) | in and out | alerts, approvals, notifications | through a host agent's connector; flotillad holds no provider credentials |
| The hosts flotilla may change | out | host writes (restart, rerun) | only through the host agent there, by signed execution |
| An observability stack | out | flotilla's own metrics, logs, traces, errors, LLM traces | the installation's collectors |

Every endpoint flotilla exposes (API, UI, MCP, the host agents' endpoint) is reachable only on the
network the installation chooses; the host agents' endpoint requires mutual TLS whatever the network.

## What it does not do

- **Act without a bound:** every write is a named capability, decided by policy, executed under the
  host's own allowlist, and recorded on its case. Model-written commands are not part of it (decided
  later, [ADR-0005](../adr/ADR-0005-agent-pipeline.md)).
- **Hold credentials in the control plane:** workers and host agents hold them, each only its own.
- **Store secrets, cases or data in git.**
- **Replace the observability stack.** It reports into it.

## Reference installation

The homelab flotilla was built for:

| What | Where |
| --- | --- |
| Control plane (flotillad, workers, egress proxy) | Docker on the Mac mini |
| Hosts with agents | the Mac mini (first), the DGX, the production VPS (later) |
| Model endpoint | a LiteLLM gateway; local models on the X3 (Ollama) and the DGX (vLLM), cloud models through it |
| Signal sources | VictoriaMetrics, VictoriaLogs, VictoriaTraces, Grafana, GlitchTip |
| Observability | the same stack, plus Langfuse for LLM traces |
| Network | a tailnet; nothing public |
| Provider | Slack |
