# The bugfix fleet

**Why this exists:** the bugfix fleet ([RFC-0010](../rfc/RFC-0010-bugfix-on-the-platform.md)) needs
things on the host that no other fleet has: two workers, a model key of its own, its own egress
proxy and, before a PR can be opened, a GitHub App. This page says what to put where, and how to see
what it does. It is in beta evaluation: whether its fixes are good is RFC-0010 §8, not this page.

## What runs

| Routine | Worker | When | What it does |
| --- | --- | --- | --- |
| `bugfix/intake` | `bugfix` | every hour | a `code_bug` case per open orrery issue labelled `bug` or `triage-fleet/routed:bugfix` |
| `bugfix/fix` | `bugfix` | a `code_bug` case handed over | the chain: verdict, fixes with the gates, review; a `github.open_pr` plan or an escalation |
| `bugfix/open-pr` | `bugfix-act` | a person approved a `github.open_pr` plan | applies the diff to its base and opens a draft PR from `flotilla/<case>` |
| `bugfix/pr-check` | `bugfix` | the case is in checking | merged: verified; closed without merging: escalated; open: looks again |

`github.open_pr` starts at shadow: plans are recorded on the case and closed (`shadow_plan_recorded`),
nothing reaches GitHub. Promote it to propose on Autonomy, after the coding harness's security review
(RFC-0010 §5), for plans to wait in Approvals. A repo action never goes above propose.

## Quick reference

```bash
# on the mini: the fleet's containers
docker ps --filter name=flotilla-worker-bugfix --filter name=flotilla-egress-bugfix
# what it did: the fleet's page in the UI, or
docker exec flotillad /usr/local/bin/flotilla runs bugfix/fix
docker exec flotillad /usr/local/bin/flotilla case list | grep code_bug
```

## Install (once, on the host)

All in `FLOTILLA_CONFIG` (`~/flotilla/config` on the mini), each secret file created mode 600 and never
printed.

1. **Worker tokens:** `worker-bugfix.token` and `worker-bugfix-act.token`, 32 random bytes as hex each,
   made the way the ops workers' were.
2. **The model key:** `bugfix-model.key`, the LiteLLM `fleet-bugfix` virtual key alone in the file (on
   the mini it is `LITELLM_FLEET_BUGFIX_KEY` in `~/signal-fleet/fleet-gateway.env`). `secrets.sh` puts
   it in the bugfix worker's volume as `/run/flotilla/model.key`.
3. **`flotillad.yaml`:**

   ```yaml
   workers:
     bugfix: {url: "http://worker-bugfix:8081", token_file: /run/flotilla/worker-bugfix.token}
     bugfix-act: {url: "http://worker-bugfix-act:8081", token_file: /run/flotilla/worker-bugfix-act.token}
   models:
     endpoints:
       gateway-bugfix: {base_url: "http://litellm:4000/v1", kind: litellm, key_file: /run/flotilla/model.key}
     aliases:
       bugfix-flash: {endpoint: gateway-bugfix, model: fleet-bugfix-flash}
       bugfix-reviewer: {endpoint: gateway-bugfix, model: fleet-bugfix-reviewer}
   ```

   flotillad's `-check` refuses the bugfix specs without these aliases (security T4).
4. **`egress-bugfix-acl.yaml`** (the bugfix proxy's allowlist; the other workers' `egress-acl.yaml`
   is unchanged):

   ```yaml
   version: v1
   services: []
   default:
     project: flotilla-bugfix
     action: enforce
     allowed_domains: [litellm, github.com, codeload.github.com, api.github.com, registry.npmjs.org]
   ```

   A package whose install fetches a binary from elsewhere is denied, and the denial is logged: add
   the domain only after reading what asked for it.
5. **`FLOTILLA_HOME/.env`:** add `bugfix` to `COMPOSE_PROFILES`.
6. Deploy as usual (`deploy/mini/deploy.sh`): `secrets.sh` fills the two workers' volumes.

## The GitHub App (before `github.open_pr` leaves shadow)

The operator makes it (GitHub, Settings, Developer settings, GitHub Apps):

- **Permissions:** Contents read and write, Pull requests read and write, Metadata read. Nothing else:
  no Issues write, no Actions, no Administration.
- **Installed on:** `chipi/orrery` and `chipi/flotilla` only.
- **Its private key:** downloaded straight into `FLOTILLA_CONFIG/bugfix-github-app.pem` (mode 600).
  `secrets.sh` puts it in the pusher's volume only.
- **Its ids** (not secrets): `BUGFIX_GITHUB_APP_ID` and `BUGFIX_GITHUB_APP_INSTALLATION_ID` in
  `FLOTILLA_HOME/.env`.

Without the App, `bugfix/open-pr` escalates the case ("no GitHub App configured"), and the diff on the
case page can be applied by hand.

## Verification

- `docker exec flotillad /usr/local/bin/flotillad -config /etc/flotilla/flotillad.yaml -check` lists
  `github.open_pr` and passes.
- After the first hour, `bugfix/intake` has a run with `BUGFIX INTAKE chipi/orrery: N open issues` in
  its log, and the fleet page shows N `code_bug` cases.
- A `bugfix/fix` run ends with `BUGFIX FIX case=… outcome=plan` or `outcome=escalate` and a reason.

## Troubleshooting

- **`the run did not start` or a fix run that never starts:** one fix runs at a time; the other cases
  wait without events. A case handed over for 24 hours is escalated by the stuck-state timer.
- **`the repo's setup failed on the base`:** `npm ci` failed in the worker; read the run's log. A denied
  download is in the bugfix proxy's log (`docker logs flotilla-egress-bugfix`).
- **`the verdict step gave no verdict`:** the triage leaf wrote nothing; its stderr tail is in the
  escalation. A model error (an empty answer) is the usual cause: check LiteLLM's key budget.
- **`unknown: …`** on an approved PR case: the pusher's run ended without a result after it may have
  pushed. It is not run again: look at `flotilla/<case>` on the repo before anything else.
