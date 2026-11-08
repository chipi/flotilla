# AGENTS.md — flotilla

This file layers on top of the operator's global AGENTS.md (`~/.config/AGENTS.md`). **Never
duplicate, never contradict** the global rules. This file holds only what is specific to *this*
project.

If a global rule conflicts with project reality, either update the global rule (if the new reality
is generally true), or state the deviation here with the *reason*.

---

## Project overview

flotilla is a plain, spec-driven platform for small fleets of AI agents. It is built from scratch
to learn how agent systems work from the inside, while running real work for one homelab. Its
fleets:
- **triage:** alerts and errors;
- **bugfix:** drafts code fixes;
- **ops:** daily brief and operational reviews;
- **seo:** search reach of the operator's sites.

A small control plane (`flotillad`) runs declared jobs, keeps a ledger of every decision, and holds
anything outward for the operator's approval. Design: [RFC-0001](docs/rfc/RFC-0001-fleet-platform.md),
decided as [ADR-0001](docs/adr/ADR-0001-platform-foundations.md).

---

## Stack

| Layer | Choice | Reason |
|---|---|---|
| Control plane | Go, stdlib-first (`flotillad`, CLI, MCP server, server-rendered UI in one binary) | grows from fleetd; single binary (ADR-0001) |
| Job runtime | Python (stdlib-first) | the triage fleet is Python; SEO data work (Parquet, DuckDB) (ADR-0001) |
| State | SQLite on the host (runs, ledger, approvals, budgets); Parquet for analytical fleet data | one file, transactional, easy to back up |
| Models | any OpenAI-compatible endpoint through one model client; LiteLLM is the reference deployment; local first; per-job model policy | ADR-0001 §9, RFC-0002 §3 |
| Test framework | `go test` (fleetd); the triage fleet's own stdlib test scripts (`test_units.py`, `test_capture.py`, `test_scrub.py`) and replays (`make replay`) | as imported from the homelab, phase 1 |
| Linter / formatter | gofmt + go vet (fleetd); Python byte-compile (ruff not yet: it would be a new dependency) | phase 1 |
| CI runner | GitHub Actions | matches `chipi` standard |
| Docs site | MkDocs Material → GitHub Pages, strict build | matches `chipi` standard |
| Secrets / config | mode-600 files on the host, named in `~/.config/flotilla/config.yaml`; never in git | public repo |
| Deploy target | the homelab Mac mini: containers in the production engine, deployed by the operator account; development on the dev engine | ADR-0002 |

---

## Project-specific rules

1. **Public repo, private state.**
   - Never commit secrets, the ledger, findings, fleet data, production payloads, or environment
     specifics (host names, tailnet addresses, user paths, real repo maps). Specs and examples use
     placeholders, and the real values live in host config.
   - Run the secrets scan before every push.
   - Real-data test corpora live in the private `fleet-eval-data` repo.
2. **An agent is never the operator.**
   - Approving an outward action (an issue, a PR, a push, a published change) is operator-only, in
     the UI or the CLI. Never through MCP, never by a job.
   - Every state change in the ledger records its actor.
3. **Behaviour changes are reviewed and replayed.** A change to a playbook, gate, detector or spec
   that changes decisions is replayed against recorded past cases before it ships (the operator's
   `replay-proven-fix` practice). Nothing rewrites its own playbooks or specs at runtime.
4. **Gates are code.** What leaves the platform is decided by deterministic checks after the model.
5. **Every model call goes through the one model client,** to an OpenAI-compatible endpoint, under
   the job's declared model policy (RFC-0002 §3). LiteLLM is the reference deployment, not a
   requirement. No undeclared fallback. An `external` step (a coding harness) points at the same
   endpoint, and the runtime records its inputs and outputs.
6. **Observable like every homelab service.**
   - Metrics, logs, traces, LLM traces, errors, ops events, dashboards and alerts on the homelab's
     conventions (RFC-0001 §6.2).
   - Every job has a freshness metric and a stale alert.
7. **The live triage fleet never stops** during the migration (RFC-0001 §9): shadow-compare, then
   switch.

---

## Code layout (phase 1)

| Path | What | Runs from |
|---|---|---|
| `fleetd/` | the Go scheduler and the bugfix chain; becomes `flotillad` in phase 2 | still agentic-ai-homelab's copy on the mini |
| `signal-fleet/` | the triage fleet (Python, stdlib): `mvp/` the cycle and its tests, `deploy/` the homelab deploy hooks, `replays.sh` the replay gates | still agentic-ai-homelab's copy on the mini |
| `bugfix-fleet/` | the bugfix fleet: bake-off harness scripts (used by `fleetd chain`) and a TypeScript MVP (no CI, RFC-0001 §11) | still agentic-ai-homelab's copy on the mini |

- Imported from agentic-ai-homelab at `f0ed072` with their history (`git filter-repo`), without
  `signal-fleet/reference*`: the replay corpora are real scrubbed production data and live in the
  private `fleet-eval-data` repo, `projects/signal-fleet-replays`. The replays find them through
  `SF_EVAL_DATA`, and the alert rules through `SF_ALERT_RULES` (`signal-fleet/replays.sh`).
- A replay's `BASE_REV` is a commit in **this** repo's history; each carries the homelab commit it
  replaced in a comment.
- **Known deviation from rule 1 until the config pass:** the imported code still holds real
  repo maps and host defaults (`signal-fleet/mvp/filing.py`, `config.py`, `fleetd/deploy/*.json`).
  They were already public in agentic-ai-homelab; moving them into host config is the rest of
  phase 1.
- Until phase 2 the live fleet runs agentic-ai-homelab's copy. A fix the live fleet needs lands
  there first and is ported here.

## Domain knowledge

- The fleets being migrated came from
  [agentic-ai-homelab](https://github.com/chipi/agentic-ai-homelab): `fleetd/`, `signal-fleet/`,
  `bugfix-fleet/` (now here), and `infra/homelab-home/briefing-job.js` (still there). RFC-0001 §2 has
  the inventory: what each does, its state, and its coupling.
- The homelab services flotilla depends on (LiteLLM, the observability stack, GlitchTip, Grafana)
  are operated from agentic-ai-homelab. Its operations handbook (`docs/recipes/agent-access.md`)
  says how to reach each one.
- Models:
  - the DGX's prod Qwen3-30B-A3B in vLLM, which has tool calling not yet enabled;
  - the X3's always-on Qwen3-30B-A3B in Ollama, which does tool calling;

  both reached through LiteLLM aliases.

---

## Named ADR anchors

- [ADR-0001 — Platform foundations](docs/adr/ADR-0001-platform-foundations.md): read first.
- [ADR-0004 — The job runtime](docs/adr/ADR-0004-job-runtime.md): the five core concepts (harness,
  context, memory, external knowledge, evaluation); read before any runtime or fleet work.

---

## Where to look

- **Active session log:** `docs/history/` (latest file = current state).
- **Plan for what's next:** RFC-0001 §9 (migration phases), and `docs/wip/`.
- **Big decisions:** `docs/adr/`.
- **Proposals not yet decided:** `docs/rfc/`.

---

## What overrides this file

- Direct instruction in chat: a one-shot ask supersedes a default.
- The global AGENTS.md for universal rules. This file only narrows or adds context; it never
  overrides the global non-negotiables.
- Per-conversation memory: persistent operator preferences captured across sessions override cold
  defaults here.
