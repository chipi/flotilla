# Host agent plan: `flotilla-host`, v1 (ADR-0006 §12)

→ becomes a `docs/history/` entry when its security e2e passes and the agent runs on the mini.

**Status:** planned 2026-11-13 (operator: build it after pipeline v1's shadow chain, while v1 runs live
in shadow). The design is [ADR-0006](../adr/ADR-0006-node-agent.md), with
[ADR-0009](../adr/ADR-0009-host-agent-names.md)'s names (the binary `flotilla-host`, the CLI
`flotilla host …`, the store's `hosts`, the role `actions`). The threats it answers are
[security](../security.md) T13 to T15. This plan orders the work and says how each step is proven.

## v1's scope (ADR-0006 §12)

One host (the mini), one action (`backup.rerun_observability`). v1 builds §1 to §7 (the binary, the
outbound connection, identity, signed executions, the host's allowlist, least privilege, execution),
§8's log and chain head, §9's operator build, and §10's `host list` and disconnected alert. The
connector is outbound only (notifications). While the action is in shadow, flotillad sends nothing to
the host; the path is built and proven in the e2e, and used from the first promotion to propose.

## Steps

| # | Step | Proven by |
| --- | --- | --- |
| H1 | **The signed envelope** (`internal/envelope`, shared by flotillad and the agent): an execution (id, case, host, action, parameters, plan hash, how approved, signed and expiry times) signed with Ed25519 over the exact bytes sent; verify then decode, no canonicalization (ADR-0006 decided 3) | forged, tampered, wrong key, expired, signed in the future, another host: each refused; fuzzed decoding |
| H2 | **The host's config**: the allowlist (each action an exact argv template, typed parameters, a privileged one an enum), rate limits, the autonomy ceiling, the accepted signing keys; strict, validated; the **sudoers generator** (one line per enum value, absolute paths, no wildcards) | a bad config refused; the generated sudoers checked with `visudo -c` in a container; no wildcard can be generated |
| H3 | **Execution on the host**: SQLite (the store flotillad uses) with `started` committed before the command and the result after; a `started` with no result is `unknown` after a restart and never runs again; argv without a shell; a timeout per action (`unknown`); output capped and hashed; local preconditions (`precondition_false`); a seen id with the same bytes returns the recorded result, different bytes are refused as a forgery | each path, with a fake command; a crash between `started` and the result |
| H4 | **The audit log**: append-only JSONL, each entry hash-chained, in a root-owned directory; the chain head sent with every result | a rewritten or removed entry breaks the chain; mutants |
| H5 | **Identity**: an intermediate CA in flotillad's secrets volume under an offline root; enrolment tokens (single use, 15 minutes, bound to the host name); the agent's key generated on the host, a CSR, a 30-day certificate renewed over the connection; the `hosts` table; revocation in flotillad alone | a token used twice, late or for another name refused; a revoked or expired certificate refused at connect |
| H6 | **The connection**: flotillad's host endpoint (its own listener, mutual TLS terminated by flotillad); the agent dials out; TLS with length-prefixed JSON frames (no new dependency: ADR-0006 named a websocket library only if one is used); heartbeat with the agent's clock; reconnect with backoff; a lost connection marks the host unreachable and an execution in flight `unknown` | an e2e with an agent container: connect, heartbeat, a dropped connection, a revoked host |
| H7 | **flotillad's side**: schema step 9 (`hosts`, execution records, chain heads); `flotilla host list` (version, last contact, clock offset, allowlist hash, ceiling, certificate expiry); alerts (not connected for N minutes, clock skew over 30s, a certificate close to expiry) as ERRORs; sending a decided execution (auto, or approved) to its host and taking its result into the case | unit tests; the e2e |
| H8 | **Install over SSH** (§9, the operator's build): `deploy/host/install.sh HOST` builds `flotilla-host` from a tagged commit, copies it, creates the `_flotilla` user (no login, no shell), writes the root-owned config, binary and sudoers (`visudo -c` first), installs the launchd service, enrols; `upgrade`, `revoke`, `remove` | run on a Linux container in the e2e; on the mini only on the operator's go |
| H9 | **The security e2e** (ADR-0006: before the first write): an agent container with a harmless test action; a signed execution runs once; a forged one, an expired one, another host's, one not in the allowlist, one over the rate limit, one above the ceiling are refused and logged; `flotilla host audit` compares the host's log with flotillad's record | the e2e, with mutants on the agent's checks |
| H10 | **Docs before the first write at propose** (ADR-0006 §11): the procedures for the execution key's rotation, the intermediate CA and a host's certificate; security.md's T13 to T15 marked built | docs build |

## Decided (operator, 2026-11-13)

- **The mini's agent reaches flotillad's host endpoint through Tailscale** (ADR-0006 decided 1: this
  homelab publishes it on its tailnet only): a TCP passthrough to flotillad's port; mutual TLS stays
  flotillad's own. No port is published on the production engine.
- **The root CA is made on the operator's laptop and kept offline, as the backup decryption key is**
  (ADR-0006 decided 2). A script the operator runs makes the root and the intermediate; the
  intermediate goes to flotillad's secrets volume, the root stays with the operator.

## Not in v1

Releases built and signed by CI and self-update (before the second host); drift alerts (before the
second host); the connector relaying approvals (with the first promotion to propose); the DGX and the
VPS.

## Progress

- **H1, the signed envelope:** built 2026-11-13, `internal/envelope` (stdlib only). Verified over the
  exact payload bytes, then decoded strictly; refuses a bad signature, an unknown key id, another host,
  an expired envelope, one signed more than 10 minutes in the future, and one valid for longer than
  10 minutes (even with a good signature); returns the signed bytes for the agent's resend-or-forgery
  check. 8 mutants killed; `FuzzVerify` 30s (2.5M executions), no failure.
- **H2, the host's config and its sudoers:** built 2026-11-13, `internal/hostcfg` (stdlib only).
  Strict JSON: the host, the agent's user (never root), the ceiling, one or two signing keys, each
  action as an exact argv. A parameter fills one whole argv element; a privileged command
  (`/usr/bin/sudo -n [-u user] /absolute/command ...`) takes only enum parameters, and no element or
  value it gets may hold a sudoers wildcard, separator, space or quote; a pattern parameter only for
  an unprivileged command, matched whole. `Argv` is the only way a command is built (this host, declared
  parameters only, values checked). `Sudoers` writes one line per action and enum value, with `""` for
  a command with no arguments (alone it would allow any). `visudo -c` (Debian's sudo, in a container)
  parses it; note that it parses a wildcard line too, so the refusal of wildcards is this package's.
  10 mutants: 9 killed, 1 a redundant duplicate check, removed.
- **H3, execution on the host:** built 2026-11-13, `internal/hostexec`. `Handle` takes the raw bytes
  and, in order: verifies the envelope; a seen id with identical bytes returns the recorded result
  (`resent`, nothing runs), with other bytes is refused as a forgery; builds the argv (`hostcfg.Argv`);
  refuses everything at ceiling shadow and an `auto` approval above the ceiling; refuses past the
  action's hourly limit here (a `precondition_false` ran nothing and does not count); commits `started`;
  checks the local preconditions (`precondition_false`; one that cannot be checked is `unknown`); runs
  the argv without a shell, in its own process group, with a built environment (`PATH`, `LANG=C`),
  SIGTERM then SIGKILL to the group past the action's timeout (`unknown`, the target stays held);
  keeps the last 16 KiB of output and the sha256 of all of it. `Open` marks a `started` left by a
  previous run `unknown`: it never runs again. One precondition kind, `launchd_not_running`:
  `/bin/launchctl print <label>` (no sudo), holds unless the `state =` line says `running`; a failed
  exit or no state line is `unknown`. `hostcfg` now refuses, at load, a precondition kind the agent
  cannot check, or one with the wrong arguments. 18 mutants killed (10 executor, 5 checker, 3 config).
  Not verified: that `_flotilla` (not the operator's account) can run `launchctl print` on the mini;
  checked when H8 installs it.
- **H4, the audit log:** built 2026-11-13, `internal/hostaudit` (stdlib only). JSONL, each line naming
  the sha256 of the exact bytes of the line before it; the head is the last line's hash. Opening
  verifies the whole log and refuses a broken or torn one (the agent then runs nothing). The executor
  logs `started` (with the resolved argv and who approved) before the store records it, so nothing runs
  unlogged, then one line per outcome (`result`, `refused`, `resent`); every result carries the head
  after its own line. A removed, moved, repeated or edited line breaks the chain; the last line
  edited or removed shows only against the head flotillad stored (`Leads`, used by `host audit` in
  H9).
  12 mutants: 11 killed, 1 (the sequence-number check) redundant with the chain, removed. Each line
  is fsynced; no test can tell a missing fsync. For H8: the agent's user owns the file, so it could
  rewrite the whole chain; what catches that is flotillad's stored head and the copy Alloy already
  shipped. On macOS, install can set `sappnd` on the file (append-only, cleared only in single-user
  mode); to decide at H8.
- **H5a, the certificates:** built 2026-11-13, `internal/hostca` (stdlib only, Ed25519 throughout).
  The root (10 years) and the intermediate (3 years, path length 0, never outliving the root); a
  host's client certificate (30 days) issued only for a request signed by its own Ed25519 key and
  naming exactly that host; flotillad's server certificate (90 days); `VerifyHost` checks a chain up
  to the root for client use and returns the host and serial (the hosts table decides revocation,
  H5b). A real TLS 1.3 handshake in the test: an agent connects, another CA's agent is refused, and
  the agent refuses a server under another name. `flotilla ca root DIR` and `flotilla ca intermediate
  ROOTDIR OUTDIR` make them on the operator's machine, offline: keys mode 600, nothing overwritten,
  the root's key never written beside the intermediate. The root key is a plain PEM file (the
  standard library has no encrypted PKCS #8); it is kept offline as the backup key is. 15 mutants
  killed.
- **H5b, flotillad's side of identity:** built 2026-11-13. Schema step 9: `hosts` (the current
  certificate's serial and expiry, enrolled, revoked) and `host_enrolments` (a token's hash, its host,
  15 minutes, used once). `flotilla host enrolment NAME` (operator or admin; agents may not) prints
  a token once; `EnrolHost` spends it and issues the certificate in one transaction (a bad request does
  not spend it; a token for another host, used, late or unknown is refused, and nothing is issued);
  the new certificate is the host's only accepted one. `AuthenticateHost`, flotillad's check at
  connect: the chain up to the pinned root, then the host's current serial. `flotilla host revoke NAME`
  clears the serial and spends the host's unused tokens (no access to the host needed); a new token
  brings a revoked host back. `flotilla host list`. Rotating the intermediate leaves issued
  certificates working (the root is what is pinned). The enrolment itself goes over the host
  endpoint's connection (H6); flotillad's config for the CA files also comes with H6. 14 mutants: 13
  killed; one (`revoked_at` checked at connect as well as the serial cleared) was a second mechanism
  for the same refusal, removed: revoking clears the serial, and that is what refuses it.
- **H6a, the connection:** built 2026-11-13. `internal/hostwire`: frames of a 4-byte length and JSON
  (1 MiB at most, refused from the header), strict bodies. `internal/hostlink`, flotillad's endpoint:
  TLS 1.3; a connection without a client certificate may only send `enrol`; one with a certificate
  must be the host's current one (`AuthenticateHost`), checked again at every heartbeat, so a revoked
  host is cut off within one; `hello` then `welcome`; one connection per host (the newer wins);
  silent for three heartbeats, dropped; `Execute` sends a signed execution and waits for its result:
  `ErrNotConnected` (nothing sent) or `ErrUnknown` (sent, then the connection was lost or the time
  ran out); each host's version, connection time, last contact, clock offset and chain head, in
  memory (stored in H7). Renewal over the connection: a request for the same key, issued in place of
  the certificate the connection used (`store.RenewHost` refuses any other), and then flotillad
  closes the connection. `internal/hostagent`, the agent's side: `Enrol`; a session pinned to the root
  and flotillad's name, its certificate checked by the agent too (this host, this key, the root);
  heartbeats with its clock and chain head; executions run through `hostexec` and their results
  sent back; renewal when under 10 days are left, then an immediate reconnect; otherwise backoff from
  1 s to 60 s with jitter. Tests over loopback TLS, with `-race`, three runs: connect, execute, clock
  offset, heartbeats; a revoke cuts off a connected host, an execution in flight comes back unknown,
  it cannot reconnect; reconnects after flotillad drops it; renewal; enrolment refusals (another
  host's token, used twice, another server name or root); another CA's agent; a silent agent
  dropped. 22 mutants killed; 5 of them survived the first run, each showing a weak test (another
  CA's agent was refused by its own check before reaching flotillad; nothing sent a good token under
  another frame type, kept a connection open after a renewal, timed the reconnect after one, or
  checked why an oversize frame was refused), fixed. Not tested: a host's older connection closed
  when a newer one arrives.
- **H6b, flotillad's config and the agent's binary:** built 2026-11-13. flotillad's optional
  `hosts:` (`listen`, `ca_dir`, `server_names`): it loads the intermediate and the root, refuses an
  intermediate the root did not sign, and starts the endpoint; its server certificate is made in
  memory from the intermediate and issued again when under 30 days are left. `cmd/flotilla-host`:
  `enrol` (the token on stdin only; the key made on the host, mode 600, kept on a second enrolment),
  `run` (the executor, the audit log, the connection; a `started` left by the last run is logged as
  unknown), `sudoers`; connection settings are flags, the host's name is its config's. It builds for
  the mini (darwin/amd64, macOS 15.7.7, checked with `uname -m`). 3 mutants killed. Not tested:
  that a crash mid-write leaves the old certificate file (the write is a rename). Next, H6c: the
  container e2e.
