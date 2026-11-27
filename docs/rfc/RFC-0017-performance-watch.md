# RFC-0017 — Performance watch: where the time goes, what changed, what to fix next

**Status:** Draft, for the operator's review (2026-11-27).

**Builds on:**

- [RFC-0016](RFC-0016-blocks-and-flows.md): blocks and flows; this is a library block and a flow;
- [ADR-0023](../adr/ADR-0023-never-silent-and-answers-in-the-core.md): a case is never silent;
- the installation ops repo's RFC-0011 (its CI runners, items 16 to 24).

## 1. Why

The operator fleet watches whether things work: a backup ran, a disk has room. The next question
is how well they work. The operator (2026-11-27): "the next set of routines should be looking into
the performance of some services and processes."

The first case is the X3's CI runners. The Hub's CI tab ranks the steps where the X3 loses the
most time against GitHub-hosted runners, per period. In the 7 days to 2026-11-27, Player's top
four steps lost 160 minutes between them; the largest is a HuggingFace cache restore at 418 s
against 20 s. Fixes are under way for most of them. The operator wants it watched all the time:
where the time goes, what changed, and what to fix next.

## 2. The shape

A **performance watch** watches one process. It names:

- **a ranked cost list:** items with a cost and the numbers behind it (here: the slow steps, the
  cost being the time lost);
- **health signals with their service levels** (here: runners online, queue wait, CPU headroom);
- **a comparison:** this run against the previous run of the same watch.

Each run:

1. ranks the top N by cost;
2. compares them with the previous run's snapshot: new entries, large rises, and entries that
   dropped out (often after a fix);
3. reads each health signal against its service level;
4. names, for each of the top 3, the known fix and its owner, or flags "no owner".

It writes nothing outside flotilla. It reports in the usual way:

- **the brief:** a section per watch, every run;
- **a case** per change worth a person's time: a new entry in the top N, a rise past a threshold,
  or a service level breached. Kind `perf_regression`, at record, never silent (ADR-0023).

## 3. The block: `ranked-watch@1`

One library block, role `monitor`, code only, no model. Its parameters:

| Param | Kind | CI runners |
| --- | --- | --- |
| `source` | a URL the worker's egress allows | the Hub's `ci.json` |
| `list` | a JSON path to the ranked list | `steps.7d` |
| `key` | the fields that name an item | `repo`, `job`, `step` |
| `cost` | the field to rank by | `lost` (seconds) |
| `top` | how many | 15 |
| `rise` | the rise that counts as a change | 50 % and 10 min |
| `signals` | the service-level source and the rows to read | the Hub's `slo.json`, group Platform, service "X3 CI runners" |
| `fixes` | the flow's knowledge file of known fixes | `knowledge/fixes.yaml` |

**Its own snapshots.** The block compares against the previous run's snapshot, which it keeps in the
routine's memory (ADR-0004), keyed by the item's key. It does not depend on the source holding a
previous period: on 2026-11-27, every 7-day row of `ci.json` had no previous period (`x3Prev:
null`), and the service levels held one day of history. The first run records a baseline and
reports no changes.

**Known fixes are knowledge.** `fixes.yaml` lists each fix under way: the item it targets, a link
(a PR or an issue) and its owner (a repo or a session). It is shown on the Knowledge tab and
changed by a person, like an SOP. An entry that drops out of the top N while its fix is listed is
reported as "fixed?", for a person to confirm and remove the fix.

## 4. The first flow: `ci-runners`

```yaml
flow: ci-runners
case_kind: perf_regression
steps:
  - name: ci-perf-watch
    use: ranked-watch@1
    every: 168h
    with: {source: hub_ci, list: steps.7d, key: [repo, job, step], cost: lost, top: 15,
           rise: {ratio: 0.5, at_least: 600}, signals: {source: hub_slo, service: X3 CI runners},
           fixes: knowledge/fixes.yaml}
  - name: ci-perf-daily
    use: ranked-watch@1
    every: 24h
    with: {source: hub_ci, list: steps.yesterday, key: [repo, job, step], cost: lost, top: 5,
           rise: {ratio: 1.0, at_least: 300}, fixes: knowledge/fixes.yaml}
knowledge:
  - {name: fixes, path: knowledge/fixes.yaml, format: yaml}
```

- **Weekly:** the whole picture, for the brief.
- **Daily:** only yesterday's regressions, so a slow step is found the day after it lands, not
  a week later.
- **The source URLs are host config,** not the spec: `hub_ci` and `hub_slo` name the installation's
  addresses, and the worker's egress allowlist gains the Hub's host.

## 5. What comes after

The same block, with other sources:

- **service latency:** the probe and latency service levels;
- **pipeline stage durations;**
- **model latency** through the gateway.

A case of kind `perf_regression` is recorded and shown in v1, and nothing acts on it. A later
step could hand a case with no known fix to an investigation, once a fix can be expressed. Most CI
fixes are code changes in another repo, not a registry action, so that investigation would need its
own design.

## 6. Not decided here

- Whether the brief section or the cases come first, if the brief is not read weekly.
- Whether `fixes.yaml` lives in flotilla's knowledge (proposed here), or whether the source
  publishes its known fixes itself.
- How many weeks of snapshots to keep (proposed: 12).

## 7. Order of work

1. RFC-0016 step 1 (blocks) lands first: this RFC uses its library.
2. `ranked-watch@1`, with tests against a recorded `ci.json` and `slo.json`.
3. The `ci-runners` flow in the installation's fleets repo, at record. Its first run sets the
   baseline.
4. After two weekly runs: read the reports with the operator, and decide whether the rise
   thresholds and the top N are right.
