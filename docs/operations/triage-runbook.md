# Triage fleet runbook — run, deploy, review, improve

This page was brought up to date on 2026-11-17. Its fleetd-era text, from the installation's ops
repo, is in git history. The example fleets:
[fleets/README.md](https://github.com/chipi/flotilla/blob/main/fleets/README.md).

How to operate the triage fleet (`signal-fleet`, Fleet 2) on flotilla, written so a different agent
can pick it up each week. For what the parts are and why they work the way they do, read the
[Triage fleet guide](triage.md) first. For the operator's own rituals (inbox, routing, labels,
judging a fleet PR), see [section 7](#7-operator-rituals).

The deploy mechanics are not repeated here. They are in
[Deployment](../architecture/deployment.md) and in the header comments of
[`deploy/server/deploy.sh`](https://github.com/chipi/flotilla/blob/main/deploy/server/deploy.sh) and
[`deploy/server/deploy_fleets.sh`](https://github.com/chipi/flotilla/blob/main/deploy/server/deploy_fleets.sh).
Host names, repo names and credentials of the real installation are not written in this repo; the
examples use placeholders (`example/shop`, `<owner>/<repo>`, `<host>`). Commands marked **[host]**
run on the machine that hosts the Docker engine.

## 1. What has to exist

| Component | Where | Notes |
| --- | --- | --- |
| flotillad | container `flotillad` | schedules `triage/cycle` every 10 minutes; owns autonomy, budget, pause, STOP and the ledger store |
| the triage worker | container `flotilla-worker-triage`, image `flotilla-worker-triage:sha-<7>` | built from `deploy/docker/worker-triage.Dockerfile`; Python standard library plus the OpenTelemetry SDK; **no pip installs on the host** |
| the candidate worker | container `flotilla-worker-triage-candidate` (compose profile `candidate`) | runs `triage/cycle-shadow` at record, with its own state volume |
| fleet code | in the image: `/opt/flotilla/signal-fleet/mvp`, `/opt/flotilla/runtime`, `/opt/flotilla/fleets` | **not a git checkout**: a deploy is a new image tag |
| the installation's fleets | `FLOTILLA_FLEETS_DIR` on the host, mounted over `/opt/flotilla/fleets` | holds the real `triage/installation.json`; see section 3 |
| fleet state | Docker volume `TRIAGE_STATE_VOLUME`, at `/home/worker/signal-fleet` in the worker | `results/` (the TSV ledgers), `queue/`, `export/`; never in git |
| secrets | Docker volume `<prefix>-worker-triage-secrets`, at `/run/flotilla` in the worker | `worker.token`, `fleet.env`; filled by `deploy/server/secrets.sh` on every deploy from the host's files (`TRIAGE_ENV_FILE`) |
| the ledger | flotillad's store (`SF_LEDGER_BACKEND=api`), in the `flotillad` state volume | the TSVs are still written beside it |
| egress | the `egress` proxy (Smokescreen) | the worker has no route out except through it; its allowlist is `egress-acl.yaml` in the host's config folder |
| Grafana + VictoriaMetrics/Logs/Traces | the observability stack | alerts in, probes and metrics out |
| GlitchTip | the error source | |
| LiteLLM gateway | the model endpoint (the reference deployment) | the triager's LLM |
| Langfuse (optional) | project "agents" | traces of triage calls |
| GitHub | a token that can read and write issues and labels on the repos in the installation's `repo_map` and `ops_repo` | filing |

The compose file and the host's file layout are in
[`deploy/compose/docker-compose.yml`](https://github.com/chipi/flotilla/blob/main/deploy/compose/docker-compose.yml)
and the header of `deploy.sh`. The names above are the compose file's today.

### The worker's environment

The worker builds each run's environment (`worker/flotilla_worker.py`, `build_env`) from four
layers: `PATH`, `HOME` and `LANG`; every `KEY=VALUE` of the secrets file `/run/flotilla/fleet.env`;
every `ROUTINE_<KEY>` variable of the worker container, as `<KEY>`; and the run's own variables
(`FLEETD_STAGE`, the autonomy; `FLEETD_CYCLE_ID`, the run id; `TRACEPARENT`). Values are secrets:
never print them or commit them. To change one, edit the host's `TRIAGE_ENV_FILE` and deploy (or run
`deploy/server/secrets.sh sha-<7>`); a rotated secret is read at the next run, with no restart.

| Variable | Read by | What it is |
| --- | --- | --- |
| `OPENROUTER_API_KEY` | `triage.py` | the key for the model endpoint (the LiteLLM virtual key; the name is historical) |
| `SF_OPENROUTER_URL` | `config.py` | the endpoint's `.../chat/completions` URL; it must end in that path |
| `SF_TRIAGE_MODEL` | `config.py` | the model alias (unset, the code falls back to `deepseek/deepseek-v4-flash`) |
| `SF_HOST` | `config.py` | host of Grafana/VM/VL/VT/GlitchTip (the code's default is `localhost`) |
| `GITHUB_TOKEN` | `filing.py`, `stale.py` | GitHub token (issues and labels, read/write) |
| `GLITCHTIP_TOKEN` | `sources.py` | GlitchTip API token, read access |
| `GLITCHTIP_WRITE_TOKEN` | `glitchtip_sync.py`, `filing.py` | GlitchTip token with `event:write`: the sync's resolves, merges and "Tracked in GitHub" comments |
| `SF_GLITCHTIP_SYNC` | `config.py` | `1` = run the GitHub → GlitchTip sync at the end of each cycle; unset = off |
| `GRAFANA_TOKEN`, or `GRAFANA_ADMIN_USER` + `GRAFANA_ADMIN_PASSWORD` | `config.py`, `sources.py` | read firing alerts (a Viewer token is preferred) |
| `LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY`, `LANGFUSE_HOST` | `observ.py` | optional tracing |
| `SF_STALE_NUDGE` | `stale.py` | `1` = the daily "looks resolved?" pass comments and labels; unset = dry run |
| `SF_CAPTURE_PROBES` | `config.py` → `capture.py` | `1` = keep one JSON per decision under `results/probes/`; unset = off |
| `SF_CAPTURE_EAGER`, `SF_CAPTURE_DIR` | `config.py` → `capture.py` | with capture on: also record the rest of the probe menu (default `1`); where to write (default `results/probes/`) |
| `SF_ASSISTANT_BACKEND` | `config.py` | overrides the installation's `assistant_backend`, for the `assistant-failover` class |
| `SF_LEDGER_BACKEND` | `actions.py`, `filing.py` | `api` (flotillad's store, with the TSVs still written) or `tsv`. The compose file sets it from `TRIAGE_LEDGER_BACKEND` (`ROUTINE_SF_LEDGER_BACKEND`), default `tsv`; the live worker runs with `api` |

Optional knobs, with their defaults:

| Knob | Default |
| --- | --- |
| `SF_RETRIAGE_HOURS` | 24 |
| `SF_MAX_PROBES` | 3 |
| `SF_TRIAGE_MAX_TOKENS` | 8192 |
| `SF_DEFER_LIMIT` | 6 |
| `SF_ALERT_KEY_DEDUP` | on |
| `SF_FILING_LINK` | on (`0` turns off the linked rows) |
| `SF_OPS_REPO` | unset; the fallback is the installation's `ops_repo` |
| `SF_TRIAGE_MILESTONE` | `triage` |

The state paths can be overridden with `SF_LEDGER`, `SF_FILED_LEDGER`, `SF_QUEUE`, `SF_SPEND_FILE`,
`SF_DEFER_STATE`, `SF_STALE_LAST_RUN` and `SF_INBOX_EXPORT`; the defaults are under
`~/signal-fleet/`, which is the state volume in the worker.

### The routine's spec

The schedule and limits are not a host config file any more. They are the routine's spec,
[`fleets/triage/cycle/`](https://github.com/chipi/flotilla/tree/main/fleets/triage/cycle)
(`routine.yaml` and `v1/spec.yaml`), read by flotillad (`flotillad -check` validates it):

- **`trigger.every: 10m`**, `worker: triage`, `run.timeout: 20m`; the command is
  `python3 orchestrator.py --cycle --limit 10 --no-dry-run` in `signal-fleet/mvp`.
- **`autonomy`:** `record` in the spec (the example spec still spells it `shadow`, the old name, which
  loads as record), the safe default. The installation raises it with
  `flotilla autonomy triage/cycle propose --reason "..."`. `record` takes no outward action;
  `propose` files issues, comments and reopens; `auto` also resolves cleaned-up errors in
  GlitchTip (ADR-0008).
- **`--no-dry-run` alone does not make the cycle write.** `orchestrator.run_poll` forces a dry run
  whenever `FLEETD_STAGE=shadow`, and flotillad sets that from the autonomy.
- **`budget.day_usd`** (1.0 in the example spec): the routine's daily model spend, enforced by
  flotillad from the cycle's spend file. Past it, the routine's runs are skipped for the rest of
  the day.
- **`freshness: 35m`:** with no run ending ok for that long the routine is stale.
- **`skills` and `harness`:** the class hints and the probe loop's settings, the same as
  `fleets/triage/skills/default-rules.json` and `harness-default.json` (a Go test keeps them equal).

The routine's page in the UI draws the same spec with a text per block.

## 2. Install, or move to a new host

The ledgers are the part you can't recreate: without them the fleet re-files every old problem as
new. The full sequence for a new host is the deploy's job (`deploy.sh`, `secrets.sh`, the compose
file) and is in [Deployment](../architecture/deployment.md); the triage-specific parts are these:

1. **Freeze the old host** **[old host]**: `docker exec flotillad flotilla stop triage`, and wait
   for the run in flight to end (`docker exec flotillad flotilla status`). Never run two fleets
   that file at once: they would race on GitHub, each with its own ledger.
2. **Carry the state.** The fleet's state is two things: the triage state volume (the TSVs,
   `queue/`) and flotillad's store (the ledger the fleet reads). Take the store from a snapshot
   (`FLOTILLA_HOME/backups/`, written by every deploy) and the state volume with
   `deploy/server/secrets.sh <tag> --seed-state <volume> <dir>`, which seeds an empty volume once and
   leaves out env files and the STOP flag. Check that row counts match on both hosts.
3. **Write the host's files**: `.env`, `config/` with the worker's token, `TRIAGE_ENV_FILE` (the
   variables above), and the egress allowlist. The credentials must work before anything runs:
    - `curl -s -H "Authorization: Bearer $GLITCHTIP_TOKEN" http://$SF_HOST:8090/api/0/projects/`
      returns a JSON list;
    - `gh api user` with `GITHUB_TOKEN` returns the account;
    - a `chat/completions` call to the gateway with the key returns 200;
    - Grafana `GET /api/alertmanager/grafana/api/v2/alerts` returns 200.

4. **Deploy** `deploy/server/deploy.sh sha-<7>` (section 3). The routine comes up at `record`:
   check that new ledger rows say `stage` = `shadow`, and that the shadow day looks right (the
   section 4 checks).
5. **Promote to `propose`:** `docker exec flotillad flotilla autonomy triage/cycle propose --reason
   "..."`. The change is recorded with its actor.
6. **Surfaces:** Grafana alert rules and dashboards are provisioned from the installation's ops
   repo, not
   from this one. After any rule change, its drift check must report no drift.
7. **Retire the old host's daemon for good,** so two fleets never file at once.

## 3. Day-to-day operation

Run the CLI inside the control plane: `docker exec flotillad flotilla <command>` (it reads its
token from the container).

| Task | How |
| --- | --- |
| **Is it alive** | `docker exec flotillad flotilla status`: per routine, its autonomy, last ok, stale, today's spend against `budget.day_usd`, and `paused` / `STOPPED` / `running` |
| **Recent runs** | `docker exec flotillad flotilla runs triage/cycle -n 20`; the run's page in the UI shows its model calls and spans. A failed run keeps the last 800 characters of its output in its detail |
| **Run a cycle now** | `docker exec flotillad flotilla run triage/cycle` |
| **Pause the routine** | `flotilla pause triage/cycle`; `flotilla resume triage/cycle --reason "..."`. The UI's routine page has the same controls |
| **STOP the fleet** | `flotilla stop triage` cancels the fleet's runs in flight and skips its later starts; `flotilla start triage --reason "..."` ends it. `flotilla autonomy freeze` holds every routine at record |
| **Change autonomy** | `flotilla autonomy triage/cycle record\|propose\|auto --reason "..."` (the routine's own cap is in its spec; `shadow` still works as the old name of `record`) |
| **Let routine actions earn auto** | `routine_actions: {max_autonomy: auto}` in `flotillad.yaml` (unset is `propose`: every routine action, such as a CRM field change, waits for approval); `flotillad -check` prints the value. It only lifts the ceiling: the routine and the action must still be at auto, earned as above. Host actions stay at propose; the Autonomy page shows both caps |
| **Deploy a code change** | merge to `main`, wait for CI, then **[host]** `deploy/server/deploy.sh sha-<7>`: see "Deploy and roll back" below. A new image restarts the worker; the next cycle runs the new code |
| **Deploy the installation's own fleets** | **[host]** `deploy/server/deploy_fleets.sh <commit>`; first `--diff <commit>` to see what changes and `--check <commit>` to see what the deploy's `-check` says (versions, re-recorded moves, routine fields) without pausing or switching anything; `--rollback` goes back one commit |
| **Roll back code** | **[host]** `deploy/server/deploy.sh --rollback` |
| **Logs** | `docker logs flotilla-worker-triage` (the worker's own); the Logs tab of the fleet page; `docker logs flotillad` for flotillad. Kept elsewhere: the sync's writes (VictoriaLogs `{fleet="triage", event="glitchtip_sync"}`), metrics (`signal_fleet_*`), the ledgers |
| **Check the GitHub → GlitchTip sync** | metric `signal_fleet_glitchtip_sync{kind=...}` (one sample per run; read it with `count_over_time` or the export API, an instant query can miss it), alert `glitchtip-sync-failing`, the audit lines above. Dry run by hand: see "A command in the worker" |
| **Turn the sync off** | remove `SF_GLITCHTIP_SYNC=1` from `TRIAGE_ENV_FILE`, then run `deploy/server/secrets.sh sha-<7>`; the next cycle skips it |
| **Triage the parked proposals** (`queue/*.json`) | the files are in the state volume; `signal_fleet_queue_oldest_age_seconds` says when the queue is ageing. The `triage-fleet-review` skill's `proposals.py` predates the move to flotilla and has not been checked against it |

Never edit a ledger while a cycle runs. Pause the routine first, and back up before an edit: the
store through a deploy snapshot, a TSV with `cp filed.tsv filed.tsv.bak-<date>-<reason>` in the
volume.

### Deploy and roll back

`deploy/server/deploy.sh sha-<7>` deploys the image tag CI built for a commit. It:

1. checks out the commit and requires its `ci-fast` and `e2e` checks green
   (`deploy/server/checks_green.sh`; a docs-only commit has no e2e run);
2. runs the **triage filing gate** (`deploy/server/triage_hooks.sh`): the newest frozen week of
   filing decisions, replayed with the previous and the new commit and the installation's own
   facts. Any change in what filing does fails the deploy. The gate script is the installation's
   (`TRIAGE_FILING_GATE`); if it is unset the deploy fails, unless `SF_FILING_GATE_ACCEPT="<why>"`
   accepts it;
3. pulls the images, refills the secret volumes (`secrets.sh`), and checks config, specs and the
   store's schema with the new flotillad (`flotillad -check`);
4. snapshots the store (SQLite online backup, the newest 10 kept in `FLOTILLA_HOME/backups/`);
5. runs `docker compose up -d` and waits for flotillad and the worker to be healthy; if they are
   not, it **rolls back by itself** to the previous tag;
6. ends with `DEPLOY PASS` or `DEPLOY FAIL`, and writes a deploy event to VictoriaLogs.

`deploy.sh --rollback` goes back to the tag deployed before the current one. An older flotillad
refuses a newer store, so when the schema moved, the rollback restores the newest snapshot that tag
accepts (rows written after it are lost) or stops with nothing changed. Every deploy appends a row
to `FLOTILLA_HOME/deployed.tsv`. Agents run the script over SSH as the operator account, and need
the operator's go first.

The installation's own fleets (its `installation.json`, specs, playbooks) deploy apart with
`deploy_fleets.sh`: it exports one commit read-only, checks it with the running tag's flotillad,
pauses the triage routines, switches `FLOTILLA_FLEETS_DIR`, recreates flotillad and the workers,
resumes the routines and prints every routine whose spec hash changed. It ends with
`FLEETS DEPLOY PASS` or `FLEETS DEPLOY FAIL`.

A flat routine moved to the folder layout needs no extra step. When every key and file its live v1
was recorded with is equal in the folder, `-check` says `versions: R v1 moved to the folder layout,
unchanged (...): re-recorded`, the deploy prints it under "re-recorded at the start", and flotillad
records it at the start. The deploy also prints, as information, each routine's own fields that
differ from the snapshot the last start recorded (budget, trigger, worker, autonomy: `routine R:
budget ... -> ...`), and for a moved routine `routine fields: identical to the stored snapshot` or
what changed. That is for the operator's go, not a gate; the start records each change as a
`routine.update` event. A move with any difference in the version's keys or files is refused before
anything is paused, naming it.

### A command in the worker

A one-off command (the sync's dry run, a replay, a ledger check) needs the environment a run has.
This sources the secrets file and maps the container's `ROUTINE_<KEY>` variables, as `build_env`
does:

```sh
docker exec flotilla-worker-triage sh -c '
  set -a; . /run/flotilla/fleet.env
  for v in $(env | sed -n "s/^ROUTINE_\([A-Za-z0-9_]*\)=.*/\1/p"); do eval "$v=\$ROUTINE_$v"; done
  set +a
  cd /opt/flotilla/signal-fleet/mvp && python3 glitchtip_sync.py'
```

`glitchtip_sync.py` without `--apply` is a dry run. A command that **writes** a ledger must also
run with `FLOTILLA_ROUTINE=triage/cycle` (the ledger's namespace), `FLOTILLA_API_URL` and
`FLOTILLA_API_TOKEN_FILE=/run/flotilla/worker.token` set, or it writes the TSV and not the store.
Do that only between cycles, with the routine paused, and with the operator's go.

### GlitchTip project recreated (same slug)

Recreating a project is the installation's change. The new project's short ids restart at 1. Since
2026-10-17 the fleet ignores a ledger row filed before the issue now holding its short id was
first seen, so a new error no longer reopens or resolves an unrelated old issue. Retiring the dead
project's rows is optional tidying, and the tool edits `filed.tsv` only. With the `api` backend,
filing reads the store, so a retire needs the store's namespace re-imported
(`ledger_sync.py`, see its docstring). Unless a collision still happens, leave the rows alone. If one
does:

1. Pause the routine and take a snapshot.
2. Dry run:
   `python3 filing.py --retire-glitchtip-project <SLUG> <ISO time the new project was created> --dry-run`
   in the worker (see "A command in the worker"). Read the list.
3. Get the operator's go, run it without `--dry-run`, and re-import the namespace.
4. Re-close any issue the collision reopened, with a comment saying why.

## Shadow versions of a triage routine

A new version of `triage/cycle` (a folder `v2/` beside `v1/`) runs in shadow once `shadows:
{enabled: true}` is set and the shadow worker is up (`deploy/compose/README.md`, "The shadow
worker"). Read its pairs with `flotilla pairs triage/cycle v2` or on the routine page; nothing it
does is filed, mirrored or posted. Every dashboard and alert that reads the fleet's metrics, logs
or errors leaves shadow rows out (`shadow="1"`, `shadow=true`); the shadow's own are separable by
those marks. `flotillad -check` names each shadow version and the worker it runs on.

## 4. The weekly review — "how is the fleet doing?"

The `triage-fleet-review` skill
(`workstation/claude/skills/triage-fleet-review/` in the installation's ops repo) automates much of
this. It
was written for the fleetd era and has not been checked against flotilla, so use the checks below.
For what never reaches the fleet (warnings, logs), use the `o11y-prod` skill for the same window.

Run these read-only checks, then report in four parts: what it did, what went wrong, what it missed,
what to improve. Each check states what "healthy" looks like.

1. **Is it alive?**
    - **Check:** `docker exec flotillad flotilla status` and `flotilla runs triage/cycle -n 20`;
      the fleet page's Workflow tab; the routine's stale alert (`flotilla-routine-stale`, from
      the freshness metric below).
    - **Healthy:** the routine is not `STALE` or `paused`; runs end `ok` every 10 min; today's
      spend is well under `budget.day_usd`.

2. **What did it decide?**
    - **Check:** the fleet page's Decisions tab, or the disposition counts for the week:

        ```sh
        docker exec flotilla-worker-triage sh -c 'awk -F"\t" -v s="$(date -u -d "7 days ago" +%F)" \
          "\$1>=s {print \$4, \$6}" /home/worker/signal-fleet/results/dispositions.tsv' \
          | sort | uniq -c
        ```

    - **Healthy:** escalations rare. Many `recurrence` rows for one fingerprint is a bug: it writes
      one per count change, not one per cycle.

3. **What did it do on GitHub?**
    - **Check:** issues touched and fleet comments:

        ```sh
        gh api "repos/<owner>/<repo>/issues?since=<7 days ago>&state=all"
        gh api "repos/<owner>/<repo>/issues/comments?since=<7 days ago>"
        ```

        Filter comments for `Recurred` / `signal-fleet`.
    - **Look for:**
        - **fan-out:** several issues reopened at once for one condition (2026-10-01: six disk
          issues);
        - **wrong target:** a recurrence landing on a closed duplicate or on a different bug;
        - **burying:** a real bug as one line in a `[low-signal] aggregate`.

4. **False dismissals** (the one thing no dashboard checks): sample 3–5 recent `dismiss` rows
   (the same `awk` with `$6=="dismiss"`, then `tail`). Check each one's cited evidence against
   GlitchTip or Grafana.
5. **What it cannot see:**
    - recurring WARNING patterns in VictoriaLogs (`_time:7d level:warning`) — warnings never reach
      the fleet;
    - new GlitchTip projects missing from the installation's `repo_map`.

6. **The parked proposals and the sync:** look at `queue/` (section 3) and check the sync's errors
   metric is 0.
7. **Ledger health:**
    - row counts and growth (`wc -l results/*.tsv` in the volume);
    - `.deferred.json` present = something is being deferred;
    - the open `triage-fleet/stale-candidate` issues waiting for the operator.

Turn each finding into one of three things:

- an issue action for the operator (close, mute, route);
- a fleet fix through section 5;
- a known gap in the guide.

### Metrics and alerts

| What | Name |
| --- | --- |
| a run, per routine | `flotilla_run{routine, status, autonomy}` (1 per run; count with `count_over_time`), `flotilla_run_seconds` |
| the routine's spend today | `flotilla_spend_day_usd{routine}` |
| freshness | `flotilla_routine_last_success_timestamp_seconds{routine}` and `flotilla_routine_freshness_seconds{routine}` (the declared freshness, so the stale alert needs no per-routine threshold) |
| flotillad alive | `flotilla_up` |
| a store write that failed | `flotilla_store_error{op}` |
| the fleet's own | `signal_fleet_disposition{disposition}`, `signal_fleet_cost_usd`, `signal_fleet_tokens`, `signal_fleet_probes`, `signal_fleet_queue_depth{kind}`, `signal_fleet_queue_oldest_age_seconds`, `signal_fleet_escalations_7d`, `signal_fleet_glitchtip_sync{kind}` |
| alerts | `flotilla-routine-stale` (ADR-0007) and `glitchtip-sync-failing`; the rules are provisioned in the installation's Grafana, not in this repo |

The fleetd-era alerts `fleetd-cycle-failing` and `fleetd-silent`, and the metric
`fleetd_spend_day`, no longer exist.

## 5. Improving the fleet — the loop every change follows

The generic version of this loop, with a replay scaffold, a mutation runner (`mutate.py`) and a
baseline git worktree for non-Python projects, is the **`replay-proven-fix`** skill.

Since 2026-09-30 every behaviour change has gone through the same loop, and the next one should too.
The operator's requirement is: *for each fix, a replayed test case with the input, today's output,
the problem, and the output after the fix.* AGENTS.md rule 3 says the same for flotilla.

1. **Find it in real data**, with counts: "10,764 of 11,700 recurrence rows repeat", not "lots of
   noise".
2. **Freeze the evidence** as a corpus: ledger snapshots, GitHub issue states, GlitchTip payloads.
   Scrub secrets and user ids (`scrub.py`, with the installation's `scrub` rules). The corpus is
   real production data, so it lives in the installation's private repos, never here (AGENTS.md
   rule 1). History moves on; a frozen corpus keeps the replay reproducible.
3. **Write the fix.** Prefer a deterministic rule over a prompt change.
4. **Write the replay**, in the installation's private fleets repo, run by its
   `triage/gates/run.sh`:
    - the **baseline** is the code at the commit before the fix, loaded from git
      (`git show <BASE_REV>:signal-fleet/mvp/<file>.py`), so the "before" is real, not
      re-implemented; a replay's `BASE_REV` is a commit in **this** repo's history;
    - first assert that **the baseline reproduces history** (the replay is faithful);
    - then compare against the fix, with explicit pass criteria and exit 1 on failure;
    - to run a decision over a recorded world with nothing sent, use
      `signal-fleet/mvp/replay_api.py`, the supported entry point.

5. **Mutation-check the replay:** break the fix in plausible ways and confirm each broken version
   fails the replay. A replay that passes broken code proves nothing. Two lessons:
    - 2026-10-02: a first disk replay passed a version that still fanned out, until a fan-out check
      was added;
    - a mutant can "fail" for the wrong reason (a syntax error from a bad `sed`), so read why it
      failed.

6. **Look at the changed cases themselves**, not only the totals, and judge them against **explicit
   links**, not titles. On 2026-10-02:
    - **a real catch:** a passing replay sent 13 recurrences into a low-signal bucket, so the rule
      was narrowed;
    - **a false alarm:** judging by titles, a later change looked like it routed closed bugs to
      *different* open bugs. GitHub's `duplicateOf` links showed most of those moves were correct,
      and one apparent mismatch was a GlitchTip short id reused by a different error. Check GitHub's
      duplicate links and the ledger row's own signal before calling a move wrong.

7. **Gate it — hermetically.** A gate reads frozen corpora only: set every state path it touches
   (`SF_LEDGER`, `SF_FILED_LEDGER`, ...) to a temp file, and run it with an empty `HOME`
   (`HOME=$(mktemp -d) python3 gate_x.py`), because the defaults are the live ledgers.
   (2026-10-17: a replay passed on a laptop and failed on the host, because new code read the live
   filed ledger.) Run the unit tests with `make test-unit` (the triage fleet's `test_units.py`,
   `test_capture.py`, `test_scrub.py`, `test_replay_api.py` and `test_dev_filter.py` run in an empty
   `HOME`), and the installation's replays with
   `SF_REPLAYS=<private fleets repo>/triage/gates/run.sh SF_ALERT_RULES=<rules file> make replay`.
8. **Commit with the numbers** in the message (input, before, after, mutation result). Push and
   deploy only with the operator's go.
9. **Verify live:** after deploying, prove the new behaviour on the real system. Either run a cycle
   (`flotilla run triage/cycle`) and read what it did, or run the decision read-only against the
   live ledger and GitHub. "The replay passed" is not "it works in production".
10. **Update the guide:** a new rule goes under "Why it is built this way", a new limitation under
    "Known gaps".

### The gates today

| Gate | Where it runs | Guards |
| --- | --- | --- |
| `make ci-fast` (lint, `make test-unit`) | CI, on every push | the triage unit tests, the worker, the runtime, the Go code; no installation facts in the code |
| `deploy/e2e/run.sh` (`make test-e2e`) | CI, the dev Docker engine | the compose stack with stand-in services |
| `gate_harness_replay.py` | `signal-fleet/mvp/`, run on a host that has captures | the probe loop on the shared harness: the same requests, decisions and gates as the old loop |
| `gate_ledger_backend_replay.py` | in a worker | dedup through flotillad's ledger equals dedup through the TSVs, lookups and writes |
| `gate_model_client_replay.py`, `gate_skills_replay.py` | `signal-fleet/mvp/` | the model client's requests; the class hints from `default-rules.json` and from the spec |
| the filing gate | `deploy.sh`, before every deploy | what filing does on the newest frozen week, old code against new, with the installation's facts |
| the installation's replays (18 until 2026-11-16) | `make replay` with `SF_REPLAYS` | the fleet's rules against real history: the August flood's operational classes, normalized dedup, duplicate following, stale nudges, routing, triager failures, recurrence rows, open-issue preference, identity (reused short ids, fatal events), promotion, the OOM and edge cases, assistant failover, inbox counts |

Until 2026-11-16 the eighteen replays ran in `signal-fleet/deploy/gates.sh` from the installation's
ops repo.
They moved to the installation's private fleets repo with their corpora, so `ci-fast` does not run
them: run `make replay` before shipping a change to a rule.

### Measuring a change to the brief, the gates or the model

A change to what the LLM sees or what the gates do is measured on real decisions in eval-harness
(`example/eval-harness`, pin a release),
`examples/triage-fleet`, before it ships. The worked example is round r4 (2026-11-04):
[`EVAL-LESSONS-0.6.0.md`](https://github.com/chipi/flotilla/blob/main/signal-fleet/EVAL-LESSONS-0.6.0.md)
section 7.

1. Commit the change on a branch here; record what it should change, item by item, before the run (a
   `hypothesis:` block in the arm's config).
2. In an eval-harness checkout: `python examples/triage-fleet/fetch.py` (both dataset revisions),
   then an arm config naming the round, the dataset and `fleet_commit`; run it with
   `TRIAGE_FLEET_SRC=<this repo's signal-fleet at that commit>` and the evaluation's gateway key,
   capped with `EVAL_MAX_COST_USD`.
3. `evh arm-smoke --config <arm>` first, then `evh run --config <arm> --repeat 3` (about $1 for 65
   decisions).
4. Compare by item: `evh pair-test --dataset-id <dataset> --round <rN> --b <baseline arm>`; read
   the items that moved (`evh inspect --runs A,B --disagree`) before trusting the number.
5. Ship only through the gates above and the operator's go; then watch it live.

To compare a model or an image against the live fleet on live signals, run it as the candidate
routine `triage/cycle-shadow` (record, never files; `FLOTILLA_CANDIDATE_TAG` picks its image).

## 6. Troubleshooting

| Symptom | Likely cause | Check / fix |
| --- | --- | --- |
| every cycle fails, or one "triager unavailable" issue appears | the model key wiped or its budget exhausted (a gateway database recreate does this) | a gateway call with the key; recreate the key, update `OPENROUTER_API_KEY` in `TRIAGE_ENV_FILE`, run `secrets.sh` |
| GlitchTip or Grafana pass errors in the run's output | source token wiped by a recreate | re-issue `GLITCHTIP_TOKEN` / `GRAFANA_TOKEN`; test with curl (section 2 step 3); refill with `secrets.sh` |
| a new error reopens an unrelated old issue | GlitchTip project recreated under the same slug | handled automatically since 2026-10-17 (`reused_short_id`); if it still happens, check the row's `filed_at` against the issue's `firstSeen`, then "GlitchTip project recreated" |
| `glitchtip-sync-failing` fires | `GLITCHTIP_WRITE_TOKEN` revoked or wiped by a recreate, or a GlitchTip/GitHub API change | run the sync's dry run by hand (section 3); re-mint the token with the `mint-credential` skill |
| one condition opens or reopens many issues | the alert rule fans out (labels too fine) | fix the **rule**, not the issues; replay the rule against VictoriaMetrics history |
| issues land in the wrong repo | routing: `repo_map` (GlitchTip project prefix) or `instance_repo_map` (the Grafana rule's `instance` label) in the installation's `installation.json` | compare the file the worker mounts with the private fleets repo (`deploy_fleets.sh --diff <commit>`) |
| the worker stops at start with an installation error | `installation.json` missing or unreadable: the fleet refuses to route on empty maps | check the mounted fleets folder (`FLOTILLA_FLEETS_DIR`), then `docker logs flotilla-worker-triage` |
| the routine is `STALE` or its runs are `skipped` | `paused`, `STOPPED`, the day's budget reached, a deploy in progress, or the worker down | `flotilla status`; `docker ps` for `flotilla-worker-triage`; `flotilla start triage --reason "..."` after a STOP |
| a deploy refuses | checks not green for the commit, the filing gate failed or did not run, or the store is newer than the tag | read the `DEPLOY FAIL` line: it names which; never skip a check without the operator's reason (`FLOTILLA_SKIP_CHECKS`, `SF_FILING_GATE_ACCEPT`) |
| a deploy ended `rolled back` | the new flotillad or worker was not healthy | `docker logs flotillad` and `docker logs flotilla-worker-triage`; the store was restored if its schema had moved |

## 7. Operator rituals

The operator's side, folded in from the 2026-07-29 operator playbook. The fleets do the volume;
the operator glances, routes, answers and merges.

**Glance.** The UI's brief and the fleet page for `triage` (Decisions tab); in Grafana, the
installation's triage dashboards ("Fleet Workforce — Triage", "Triage — Operator Inbox", provisioned
in
the ops repo): top row needs attention, escalations 7d (open ones only), oldest queued item,
queue by kind; the inbox dashboard shows the items themselves. Its bottom panel shows `meta=true`
substrate alerts, which only the operator answers: a broken fleet can't triage its own substrate.

**Route.** The inboxes, as GitHub searches over the repos the installation maps (replace `<owner>`):

- machine-fixable: `owner:<owner> is:issue is:open label:"triage-fleet/actionable"`
- everything the fleet filed: `owner:<owner> is:issue is:open label:"triage-fleet/filed"`
- escalations awaiting the operator: `owner:<owner> is:issue is:open label:"triage-fleet/escalated"`

On a fleet-filed issue, four native GitHub actions that the fleet reads back:

| The operator does | Meaning | What happens |
| --- | --- | --- |
| label `triage-fleet/routed:bugfix` | dispatch Fleet 1 | the bugfix fleet picks it up (below) |
| close the issue | not worth it | the filing ledger respects it; a recurrence within 7 days reopens it, a later one files fresh with a back-link |
| comment | answer or add context | the fleet re-triages with the comment as reporter input |
| label `triage-fleet/muted` | never again for this fingerprint | permanent silence for that signal |

Don't route `config-enhancement` issues: Fleet 1 can't test config. They wait for Fleet 3.

**Dispatch Fleet 1.** The bugfix fleet is a set of routines on flotilla ([The bugfix
fleet](bugfix.md)). Its `bugfix/intake` routine opens a case for every open issue of a chain repo
(the installation's `chain_repos`) labelled `bug` or `triage-fleet/routed:bugfix`, so labelling the
issue is the dispatch; there is no manual `make chain` any more. The fix, the draft PR and the PR
check follow as that page says, and a PR waits for the operator's approval in the UI. A bug ticket
written for the fleet should read like a busy maintainer's: symptom, where, what correct looks
like. Don't add file paths or acceptance criteria; the loop has to earn those, and adding them
blinds the measurement.

**Judge a draft PR** like a maintainer: does the regression test encode *this* bug, is the fix
at the cause, what does the review comment say. Merge = shipped. Close with a one-line reason =
rejected; every rejection becomes a fixture for the next round. Never merge a fleet PR you
wouldn't merge from a human.

**Levers** beyond section 3: autonomy back to `record` (`flotilla autonomy triage/cycle record
--reason "..."`); `flotilla stop triage`; `flotilla autonomy freeze`; deploy rollback
(`deploy.sh --rollback`); GitHub token rotation (a new fine-grained token into the host's
`TRIAGE_ENV_FILE`, then `secrets.sh`; the next run reads it).
