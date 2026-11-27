# Operator fleet, step 1: where it stands (2026-11-27 morning)

Step 1 of [the operator fleet plan](operator-fleet-plan.md), built overnight to
[the build plan](operator-step1-build.md). All of it is on flotilla's main; none of it is deployed.

## What is on main

| Commit | What | Review |
| --- | --- | --- |
| `4495313` | ADR-0023 (never silent; answers in the core, chat apps as connectors), the build plan | — |
| `a3bd3c9` C2 | the `watching` status, `on_record: watch`, notices due on a bounded schedule | read by me |
| `68408d5` and `d4b9e35` C3 | one answers API (`/v1/cases/{id}/answers`) behind the UI, the CLI and Slack | independent review: no blocker; the Slack cap race and the ack scope fixed |
| `c0284b7` C4 | Slack shows any answer a case offers, and sends the notices | independent review: no blocker; a stale-button ack and a silent drop on a mixed deploy fixed |
| `54fcbe8` C5 | the `triage-fleet/routed:operator` label on escalations, an example operator intake | triage's replay `REPLAYS PASS (18)` before and after |
| `0a5b312` C6 | the SOP loop: match, follow, or investigate (read-only, metrics), and a drafted SOP as a proposal | independent review: no blocker; model text kept out of proposals |
| `d848c93` C6b | a fleet brings its own knowledge (`knowledge:` in `fleet.yaml`), a Knowledge tab per fleet, Accept or Reject of a proposal | read by me |
| `ed82491`, `acb6742` | e2e fixes (below) | — |

## What went wrong overnight

- **Docs and lint failures pushed:** twice, a gate's failure did not stop my push (C2's docs-lint,
  C5's stale `llms.txt`). Both were fixed within minutes. Pushes since check every gate's exit code.
- **A flake surfaced by C2:** the e2e suite spent exactly 10 executions of the platform's 10 an
  hour before the question section. Fixed in `ed82491`: one fewer execution, and a check that fails
  loudly when a section spends past the budget.
- **A date-bound test:** a fixture with a fixed date failed after midnight. Fixed in `79dd9c5`.
- **C6 broke the full e2e on main** (0a5b312 to d848c93). Its local runs covered its own sections
  only. Two causes:
  - The e2e read `case list`'s default 20 rows, and C6's cases pushed older ones off. It now reads
    500 (`fbff9a0`).
  - The suite needs 11 executions in its hour, and the cap was 10. `limits:` in `flotillad.yaml`
    was declared in `policy.Limits` but never wired. `acb6742` wires it, and the e2e sets
    `platform_per_hour: 12`. An installation without the key stays at 10: the mini's config does
    not set it. I took this decision overnight. Say if you want it otherwise.
  - A full local e2e passed on it: `E2E PASS`, `EXIT=0`, with "11 of 12 executions spent before the
    questions".

## C7: ready, waiting for your go

The rename `ops` to `operator`, wired to all of the above, is `87acbe8` in the private fleets repo.

- **On the fleets repo's main since 05:31, by mistake.** Another session pushed it there with its
  own commit on top (`6d35b36`, one data file outside `fleets/`, no routine).
- **Part of the cause is mine:** my C7 agent left the shared checkout on `operator/rename`.
- **The operator chose to keep it** (2026-11-27). Nothing deploys from main by itself: the fleets
  deploy takes an explicit commit.

- **Checked:**
  - A local `-check` of the fleets against a d848c93-era image: `CHECK PASS: 27 routines`, every
    `operator/*` routine new at v1, no `ops/*` left.
  - A drain check on the mini, read-only: nothing to drain. Both old backup cases are cleared, and
    no `ops/*` case is open. Every routine's autonomy equals its spec, so nothing needs re-setting.
- **In it:**
  - `rerun-backup` is SOP-aware (`ops/sop_act.py`), with `on_record: watch`.
  - The first SOP, `backup-stale` (rung 2, the two real cases as evidence).
  - `knowledge:` declared.
  - `sop-investigate` (`triage-flash`, at most `propose`) and `sop-draft`.
- **Not in it:** the operator intake. No worker on the mini carries `bugfix/intake.py`. It is a
  small image change, after the deploy.

**The deploy, in order:**

1. Deploy flotilla at `acb6742` or later (`deploy.sh`). flotillad and the mini's host agent
   upgrade together.
2. Run `deploy_fleets.sh --check 6d35b36` on the mini: the real check against the live store.
3. Run `deploy_fleets.sh 6d35b36`.
4. Verify:
   - `flotilla status` shows `operator/*` at v1;
   - `operator/backup-monitor` ran;
   - the Knowledge tab lists `backup-stale`.

**Rollback:** `deploy_fleets.sh --rollback` (back to the fleets deploy before it, `cea5ee6`),
then the previous flotilla tag's `deploy.sh`.

## Not covered

- **Nothing has run on the mini.** The model call of `sop-investigate`, the triage worker's egress
  to VictoriaMetrics, and the registry env are untested there.
- **Never-silent notices show in the UI and CLI only** until a Slack connector is configured on the
  mini. That is separate from the second-brain bot.
- **Not tested against real Slack:** the Slack connector's messages are tested against a fake.
- **Labels to create:** `triage-fleet/routed:operator` must exist in each repo before you can apply
  it, and creating labels on your repos needs your go.
- **Local e2e runs share one project name,** so two agents' runs can collide (they did twice).
  This is a follow-up.
