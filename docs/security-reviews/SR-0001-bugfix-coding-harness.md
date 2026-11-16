# SR-0001 — The bugfix fleet's coding harness

**Date:** 2026-11-15. **Scope:** `bugfix/fix` and `bugfix/open-pr` as deployed on the X3 (RFC-0010,
RFC-0011), before `github.open_pr` leaves shadow. **Due by:** [security.md](../security.md), "the
coding harness … its security review is due before that fleet acts". **Reviewed by:** the build
session. A separate model (Fable 5.1) reviewed the code the same day (its six findings fixed in
`02c4803`); it did not review this page.

## What runs, and with what

A fix run reads an **untrusted issue body**, gives it to a model with a **shell tool** (pi), and
runs the **repo's own code** (`npm ci` with install scripts, vitest). All of that is one Linux user
in one container:

| In reach of a fix run | Can it be used? |
| --- | --- |
| The LiteLLM `fleet-bugfix` key (`/run/flotilla/model.key`, read by pi) | yes: it is the run's own key. LiteLLM caps it at $100 |
| The worker's own token | yes, against the worker itself only, which runs what the run already runs |
| GitHub write | **no**: the App key is in `worker-bugfix-act`, another container that runs no repo code |
| flotillad | **no**: the link, with the worker identity's key, is its own container; the fix worker holds no flotillad token (removed 2026-11-15) |
| The network | the X3 proxy's allowlist: GitHub, the npm registry, GitHub's release assets, the mini's LiteLLM |
| The scratch volume | the run's own directory (deleted after), the npm cache, **the shared repo cache** |

## Threats and their controls

| # | Threat | Control | Residual |
| --- | --- | --- | --- |
| H1 | An injected issue makes the model write a harmful fix | The fix is a plan: shadow records it; at propose a person approves its exact diff (hash-bound) and merges the PR. The fleet cannot merge | a person approving a bad diff |
| H2 | Repo code or the model exfiltrates the model key | Egress: no general web; GitHub without a token cannot receive data an attacker can read back; LiteLLM's budget | the key's spend up to its cap |
| H3 | A run plants something in the shared repo cache for the next run (a hook, a URL rewrite, another origin) | Every git in a run has `core.hooksPath=/dev/null` (git's environment); before each fetch the cache's hooks are removed, `origin` is reset, and a URL rewrite, hooks path, fsmonitor, ssh command or protocol setting in its config stops the run | an edit to tracked objects is content-addressed: it cannot pass as another commit |
| H4 | A run changes another case's diff | Runs are one at a time; each run's worktree is new from the fetched commit; open-pr fetches the base from GitHub by its id and applies only the approved diff | — |
| H5 | The npm cache is poisoned | npm checks each package against the lockfile's integrity hash | a package with no integrity entry |
| H6 | A push with more than the approved diff | open-pr recomputes the plan's hash, applies the diff to the index only (no checkout, hooks off), one commit, a new branch only, draft PR; each installation token is narrowed to the plan's one repository and to contents and PR write (2026-11-16: the reused App holds issues write on 18 repos; a narrowed token from the act worker reached 1 repo with only those rights) | the App's own rights are wider than any token flotilla asks for |
| H7 | A timed-out run keeps working | each step's whole process group is killed at its timeout | — |
| H8 | The read-only advisor acts | it is told to read only, but holds the same shell: on orrery #646 it started a dev server (2026-11-15) | it is not read-only by construction: same container, same limits |

## Findings to act on

1. **H8:** the advisor and the verdict step run with the same tools as the fixer. A read-only pi mode,
   or a container without write access to the worktree, would make "read only" true. Not needed for
   safety today (they are inside the same limits), noted for phase B.
2. **Model key spend:** the routine records no spend (pi's cost through a custom provider is now
   known to pi, but the routine does not report it to flotillad). The $100 LiteLLM cap is the only
   bound. A per-routine spend report is phase B's "cost per arm" too.
3. **Denials:** `cdn.jsdelivr.net` (orrery's i18n plugin) is denied and harmless today; any new denial
   is a log line on the X3, not yet an alert there.

## Verdict

The fleet may leave shadow for **propose** once the operator has made the GitHub App (contents and
pull requests write, metadata read, on chipi/orrery and chipi/flotilla only). Nothing found allows a
write without a person's approval of the exact diff. **Auto stays out of reach:** repo actions are
capped at propose by the registry and policy.
