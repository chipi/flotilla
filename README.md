# flotilla

A plain, spec-driven platform for small fleets of AI agents. It has a control plane, a ledger of
every decision, and fleets that propose rather than act alone.

flotilla runs the agents of one homelab:
- a **triage** fleet that reads alerts and errors;
- a **bugfix** fleet that drafts fixes;
- an **ops** fleet for daily briefs and operational reviews;
- an **SEO** fleet that watches how a handful of websites do in search.

Each fleet is a group of jobs declared in spec files. A job collects facts with ordinary code, lets
a local language model investigate through a small set of read-only tools, checks the result with
deterministic gates, and records the decision in a ledger. Anything that reaches the outside world
waits for a human.

It is built from scratch on purpose, as a way to learn how agent systems work from the inside.
Projects like Hermes Agent and OpenClaw are inspiration, not dependencies.

## Status

**Design phase.** The design is
[RFC-0001](docs/rfc/RFC-0001-fleet-platform.md), and the decisions taken on it are in
[ADR-0001](docs/adr/ADR-0001-platform-foundations.md). The existing fleets still live in
[agentic-ai-homelab](https://github.com/chipi/agentic-ai-homelab) and move here phase by phase
(RFC-0001 §9).

## Shape

- **flotillad** (Go): the control plane. It holds the spec registry, scheduler, runner, ledger,
  approval queue, budgets and an API. It serves a CLI, an MCP server and a small web UI.
- **The job runtime** (Python): one LLM client, one agent loop, one tool registry, gates and
  outputs. Every job uses it.
- **Specs** (`fleets/<fleet>/*.yaml`) and **playbooks** (Markdown) in git. Config, secrets, the
  ledger and fleet data stay on the host.
- **Local models first,** through a LiteLLM gateway. It reports into an existing observability
  stack (VictoriaMetrics, VictoriaLogs, VictoriaTraces, Langfuse, GlitchTip, Grafana).

## Docs

The docs site is published from `docs/` with MkDocs Material: <https://chipi.github.io/flotilla/>.

```bash
make docs-serve    # http://127.0.0.1:8765
make docs-build    # strict build into ./site
```

## License

[MIT](LICENSE)
