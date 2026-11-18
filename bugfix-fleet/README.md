# bugfix-fleet — the July 2026 prototype and the bake-off

**This is not what runs.** The bugfix fleet flotilla runs is the routines in
[`bugfix/`](../bugfix/) with their specs in [`fleets/bugfix/`](../fleets/bugfix/): intake, fix,
open-pr and pr-check, on remote workers, operated as [docs/operations/bugfix.md](../docs/operations/bugfix.md)
and designed in [RFC-0010](../docs/rfc/RFC-0010-bugfix-on-the-platform.md). It replaced this directory's
code on 2026-11-15.

This directory keeps two earlier things, as a record (no CI, RFC-0001 §11):

1. **The TypeScript prototype** (`src/main.ts`, July 2026): the GitHub issue → triage → fix → PR loop
   of the installation's first bugfix fleet. It ran one batch, was paused, and was retired with fleetd on
   2026-11-16. Only its `direct` worker adapter was ever wired; `piAdapter.ts` and
   `opencodeAdapter.ts` are stubs.
2. **The bake-off rig** (`bakeoff/`): the pi-vs-opencode measurement study that shelled the real
   coding CLIs. It found that the harness barely moves the fix rate and ticket quality is the lever
   ([BAKEOFF.md](BAKEOFF.md) §8; the arc:
   [history/origin/0005](../docs/history/origin/0005-bakeoff-matrix-arc.md)). Its real bug data
   has left this repo (research lives in the evaluation project).

"RFC-0002" below and in `BAKEOFF.md` is the installation's ops repo's RFC-0002 (the bugfix fleet), not
flotilla's RFC-0002 (the job runtime). The prototype's model calls went through a LiteLLM gateway
with the installation's keys; its settings are in `.env.example`.

## What's here (MVP scope)

- **Flow A — Triage** (`src/flows/triage.ts`): `bug` → structured verdict → labels + recommendation.
- **Flow B — Fix** (`src/flows/fix.ts`): `flow:approved` → worktree → fix → local tests → land on `fixes`.
- **Worker seam** (`src/worker/types.ts`): one interface, three adapters —
  `directAdapter.ts` (**the only one wired**), plus `piAdapter.ts` / `opencodeAdapter.ts`
  which are **Phase-0 stubs that throw**. `direct` is both
  the working path and the bake-off's control baseline.
- **Deterministic orchestrator** (`src/orchestrator.ts`): advances issues by
  swapping `flow:` labels (the state machine); LLMs only at the worker leaves.
- **GitHub App auth** (`src/github/appAuth.ts`): installation-token dance hidden.
- **Langfuse trace** (`src/observability/langfuse.ts`): every leaf, for the model/cost comparison.

## Status

Skeleton wired end-to-end; the two adapter `TODO(pi)`/`TODO(opencode)` blocks are
the actual harness integration — filling them in **is** the Phase-0 spike. Not yet
included (Phase 1): the whole-PR review + feedback loop, batch-PR cut, deploy.

## Run (once the App + sandbox + creds exist)

```sh
npm install
cp .env.example .env   # HARNESS=direct; the gateway's URL and key
npm run typecheck
npm run dev -- dispatch     # safe no-mutation smoke test (real LLM via the gateway)
# npm run dev -- poll       # the real triage→fix loop (mutates the target repo)
```

## Bake-off protocol

The **north star + full evaluation methodology** lives in **[BAKEOFF.md](BAKEOFF.md)**
— how to think about tool calls, the benchmark-difficulty trap, the eval setup,
scoring dimensions, and how to assess "which harness is better at what." Read
that before wiring the opencode/pi adapters.
