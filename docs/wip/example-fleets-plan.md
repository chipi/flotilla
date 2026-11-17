# Six generic example fleets

The operator, 2026-11-17: flotilla's `fleets/` must be generic examples, not the operator's fleets with
placeholders. Then every trace of the operator's installation leaves the docs and the site, which are
redone on these examples, or on ideas and concepts only. The site's message is trust: how you build
it, how you roll out fleets, how you control and govern them. Its angle (the operator): agents as
operationalized business processes, away from the tech hype; how you gain trust in them, stay
confident, and stay in control. Later, a link to eval-harness and how to do evals.

Today `fleets/` is the operator's fleets with the repos swapped: its specs still name the operator's
repos, hosts, models and schedules, and some shared code names the operator's backup chain. Building
the six replaces `fleets/` and generalizes that code. The operator's live fleets, in the private fleets
repo, keep running unchanged; where a shared name changes, the private repo changes with it in one
deploy (`deploy_fleets.sh`), and the deploy's spec-hash proof shows nothing else moved.

## The six

Chosen by the operator from research on what teams run agents on (incident triage, background coding
agents on narrow verifiable jobs, pipeline babysitting, approval gates on writes).

| Fleet | What it does | Trust it shows | Built from |
| --- | --- | --- | --- |
| `triage` | alerts and errors become deduplicated cases; what needs a fix is filed as an issue | a model proposes; gates in code decide what leaves | `signal-fleet` (as is), example facts |
| `bugfix` | a labelled issue becomes a draft PR whose test fails before the fix and passes after | outward code change; a person always merges | `bugfix/` (as is), one example repo |
| `deps` | an outdated or vulnerable package becomes an upgrade PR, the repo's tests as the gate | autonomy earned on a narrow, verifiable job | new routine code; `github.open_pr` |
| `jobs` | a stale backup or a failed nightly job is rerun on its host, then checked | a signed host action plus a check, shadow to propose to auto | `ops/` backup chain, generalized |
| `sites` | robots, canonicals, sitemaps, broken links and certificate expiry, read only | a read-only fleet; findings are cases | `seo/site_check.py`, plus links and certificates |
| `review` | closed cases become a proposed lesson or a process change | the system proposes changes to itself; a person approves | `ops/case_review.py`, `process_change` |

`platform` (heartbeat, model contract) stays: it is flotilla checking itself, already generic.

**Business processes too** (the operator, 2026-11-17): more examples beyond engineering, focused on
marketing, sales and sales ops, outbound, and customer success. Researched and designed like the six
(what teams run there, what each example shows about trust), then proposed to the operator before
building. They carry the site's angle: agents as operationalized business processes.

## Rules for every example

- Placeholders only: `example/app`, `example.com`, `backup-01`, documentation addresses; model
  aliases named by role (`fleet-triage`, `fleet-coder`), never a provider's model.
- No schedule, freshness or limit that comes from the operator's estate (a nightly window, a 20 h
  backup cadence); each is the example's own, stated in its spec.
- Each runs in the e2e stack against stand-in data and is covered there.
- `scripts/installation_facts.py` gets the private names it does not know yet through the
  denylist; the spec descriptions are checked by the same lint (they are code, not docs).

## Shared names that are the operator's

Found by `git grep` on 2026-11-17; each is generalized, with the private repo and the ledger's existing
rows carried over:

- the host action `backup.rerun_observability` (Go: `dispatch`, `registry`, the host registry, tests);
- the routine names `backup-monitor`, `rerun-backup`, `verify-backup` in Go (`ui/views.go`,
  `mcpserver.go`) and the case kind `failed_backup` (`spec.go`, `store/briefs.go`);
- the per-repo bugfix profile `intake-podcast` / `fix-podcast` (`bugfix/intake.py`, the remote
  compose, the e2e run);
- the triage candidate `cycle-shadow` (compose, e2e, `deploy_fleets.sh`), whose spec describes the
  operator's model host and nightly.

Open, decided in the `jobs` step after reading the registry's design (ADR-0006, ADR-0009): whether an
installation can add its own host actions, so the example registry holds only generic ones.

## Order

1. `sites`: read only, smallest; proves the example layout and the e2e stand-in.
2. `triage`: example facts; the candidate routine without the operator's model host and nightly.
3. `bugfix`: one example repo; drop the operator's per-repo profiles.
4. `jobs`: the shared backup names, with the private repo in the same deploy.
5. `review`: on the generalized `jobs` cases.
6. `deps`: new code.
7. The business examples (marketing, sales, outbound, customer success): research, then propose.
8. `fleets/README.md`, then the docs and site scrub (a separate plan).

Step 1 left: `sites/site-check` is not in the e2e yet (it reads the live web); a stand-in site in the
e2e stack runs it there.

Done when `fleets/` holds the six and `platform`, none names the operator's installation (lint and a
grep for the private names), e2e covers each, and the mini runs unchanged from the private repo.
