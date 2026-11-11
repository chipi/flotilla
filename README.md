# flotilla

A plain, spec-driven platform for small fleets of AI agents that can act, within limits the
operator sets and can see. It has a control plane that decides and records, workers that diagnose
and plan, and host agents that execute only what is signed and allowed on their host.

flotilla is built for one homelab and written for any. It is meant to run (triage runs today, in
shadow):

- a **triage** fleet that reads alerts and errors and files what needs a fix;
- an **Ops** fleet, the SRE team in action: briefs, reviews, and remediation (a failed backup, disk
  pressure, a service in an OOM loop) as cases;
- a **digital marketing** fleet: search performance first, then which posts bring visits;
- a **bugfix** fleet that drafts fixes as PRs.

Every problem is a **case**: detected, diagnosed, handed to the routine that can fix it, planned,
decided by policy (run, ask the operator, or refuse), executed, checked and closed, with its whole
timeline kept. Agents read through named capabilities and only plan writes; how much may happen
without asking is earned per capability and promoted only by the operator. Approvals come from the
UI, the CLI, or Slack within limits.

It is built from scratch on purpose, as a way to learn how agent systems work from the inside.
Projects like Hermes Agent and OpenClaw are inspiration, not dependencies.

## Status

**Phase 3.** The triage fleet runs on flotillad in shadow beside its old implementation (phase 2);
the runtime (model client, recording for exact replay) is being built. The design:

- [RFC-0001](docs/rfc/RFC-0001-fleet-platform.md): the platform, with
  [ADR-0001](docs/adr/ADR-0001-platform-foundations.md) and
  [ADR-0002](docs/adr/ADR-0002-run-in-docker.md);
- [ADR-0004](docs/adr/ADR-0004-job-runtime.md): the runtime;
- [ADR-0005](docs/adr/ADR-0005-agent-pipeline.md): the agent pipeline (cases, capabilities, policy,
  recall, approvals);
- [ADR-0006](docs/adr/ADR-0006-node-agent.md): the host agent.

The existing fleets' code (`fleetd/`, `signal-fleet/`, `bugfix-fleet/`) was imported from
[agentic-ai-homelab](https://github.com/chipi/agentic-ai-homelab) with its history.

```bash
make ci-fast       # lint and unit tests, Go and Python
make replay        # the triage fleet's replays; needs the private corpora (signal-fleet/replays.sh)
```

## Shape

The shape is the design (ADR-0001 to ADR-0006). Built so far: specs, triggers, the runner, the
store (runs, events, controls, spend), the API and CLI, the model client and tracing of runs. Cases,
policy, host agents, MCP and the UI are not built yet.

- **flotillad** (Go): the control plane. Specs and the capability registry, triggers, runner,
  cases, policy and guardrails, approvals, the signer and the host gateway, channels, budgets, and
  one API behind a CLI, an MCP server and a small web UI. It holds no fleet credentials.
- **Workers** (the Python runtime), one per role: the harness, context, recall, capability calls,
  the model client. Each holds only its role's credentials and reaches out through an egress proxy.
- **`flotilla-host`** (Go), on each host flotilla may change: connects out over mutual TLS, runs
  signed executions its host's allowlist permits, and can be the connector to Slack.
- **Specs** (`fleets/<fleet>/*.yaml`) and **playbooks** (Markdown) in git. Config, secrets, cases
  and fleet data stay on the hosts.
- **Any OpenAI-compatible model endpoint** (LiteLLM, vLLM, Ollama, a cloud API). It reports into an
  existing observability stack.

## Docs

The docs site is published from `docs/` with MkDocs Material: <https://chipi.github.io/flotilla/>.

```bash
make docs-serve    # http://127.0.0.1:8765
make docs-build    # strict build into ./site
```

## License

[MIT](LICENSE)
