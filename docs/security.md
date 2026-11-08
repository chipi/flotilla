# Security

Security is designed in from the start, not added later. flotilla runs language models against
real data, with credentials that can change real things (GitHub repos, pushes to a phone), and it
lives in a public repo. This page is the threat model and the controls; each control names where it
is enforced.

## What we protect

| Asset | Why it matters |
|---|---|
| **Credentials:** GitHub App and tokens, LiteLLM keys, Search Console, GlitchTip, Grafana, Home Assistant | they can change repos, read private data, spend money |
| **The operator's repos and sites** | an unwanted issue, PR or change is visible and can do harm |
| **Read access to the homelab** (metrics, logs, errors) | it reveals infrastructure detail |
| **The ledger** | the record of every decision and approval; it must be trustworthy |
| **The approval** | the one thing that lets outward actions happen |
| **The public repo** | anything committed is public forever |

## Who could cause harm

| Actor | How |
|---|---|
| **Untrusted content authors** | text the models read: alert annotations, error messages, issue bodies, PR comments, web pages, competitor pages, search queries. The main threat: **prompt injection**. |
| **A misbehaving model** | wrong, overconfident or manipulated answers; tool calls outside its task |
| **Another agent through MCP** | well-meaning but wrong, or itself injected |
| **A compromised dependency or image** | supply chain |
| **Someone on the tailnet** | the tailnet is trusted, but not every device on it should approve |
| **Ourselves** | a secret committed to the public repo; a spec that grants too much |

## Threats and controls

| # | Threat | Controls | Enforced by |
|---|---|---|---|
| T1 | **Prompt injection leads to a harmful action** (an instruction in an issue body makes the model file, comment, or call a tool it should not) | Tools are read-only and scoped. Outputs are the only write paths and run after deterministic gates, not on the model's word. Outward actions need approval unless the spec allows that exact output at that stage. Untrusted text is passed as data, delimited, never as instructions to the runner. | tool registry, gates, runner, approval queue |
| T2 | **Exfiltration through a tool** (the model sends data out) | No general web or HTTP tool. Tools reach only declared hosts. Container egress is limited to what the fleet needs. | tool registry, container network policy |
| T3 | **An agent approves through MCP** | MCP has no approve tool. Approval is operator-only in the UI or CLI, with the operator's identity. The ledger's events record the actor of every change. | flotillad API authorization |
| T4 | **Silent cloud egress** (data sent to a cloud model or service) | Every call goes through LiteLLM. Each job declares its model policy, and local-only jobs use aliases with no cloud fallback. The LLM client refuses a model outside the policy. | LLM client, LiteLLM config |
| T5 | **A secret leaks** (to the repo, logs, traces or a prompt) | Secrets live in files on the host, mounted per worker, never in git. Secrets scan in pre-commit and CI. Logs and traces redact known secret patterns. Credentials never enter a model's context. | CI, pre-commit, logging, LLM client |
| T6 | **A fleet's credentials used by another fleet** | One worker per fleet, each mounting only its own secrets. flotillad holds none. | deployment (ADR-0002) |
| T7 | **A behaviour change sneaks in** (a playbook, gate or spec edited to do more) | Specs and playbooks change only by a reviewed commit and a deploy, never at runtime. Changes that alter decisions are replayed against recorded cases first. Spec changes that widen permissions are flagged in CI. | registry, CI, replay gate |
| T8 | **Supply chain** | Stdlib-first in both languages. Dependencies pinned and reviewed, each one approved. Images from pinned bases, by digest. Vulnerability scanning in CI (`govulncheck`, `pip-audit`, image scan). | CI |
| T9 | **Container escape or host access** | Non-root, read-only root, all capabilities dropped, `no-new-privileges`, no Docker socket, resource limits. | deployment |
| T10 | **The ledger is tampered with** | Only flotillad writes the database. Workers report through the API. Events are append-only; each event can carry the hash of the previous one, so a rewrite is detectable. Backups go offsite. | flotillad, backups |
| T11 | **Exposure on the network** | API, UI and MCP on the tailnet only, never on a public interface. Being on the tailnet is not authorization: the UI requires the operator's login. MCP callers authenticate with per-caller tokens. | network, flotillad |
| T12 | **Denial of wallet** (runaway spending) | Per-job and per-fleet budgets checked before and during runs. LiteLLM virtual-key budgets as a second limit. `max_steps` per agent loop. | scheduler, runner, LiteLLM |

## Rules for contributors (also in AGENTS.md)

- Never commit secrets, the ledger, findings, fleet data, production payloads, or environment
  specifics.
- A new tool is read-only unless it is an output.
- A new output names its scope check.
- A new dependency needs an explicit yes.
- Any spec field that grants more (a broader repo map, a stage allowance) needs a note in the commit
  saying why.

## Verification

- **Security tests are part of the pyramid** ([Testing](testing.md)):
  - injection fixtures (issue bodies and pages with planted instructions) must never produce an
    output;
  - tools must refuse out-of-scope requests;
  - MCP must refuse an approve call;
  - a local-only job must refuse a cloud model.
- **A threat-model review** with every new fleet and every new kind of output.

The defaults behind T2, T6, T9, T11 and the credential rules (deny-by-default egress through an
allowlisting proxy, identities, image handling, rotation) are in
[Deployment → Defaults](architecture/deployment.md#defaults).

## Not covered yet

- Image signing: planned before any fleet reaches `live`, not built.
- A security review of the coding harness used by the bugfix fleet, which runs code from the repos
  it fixes. Due before phase 6.
- The egress proxy (Smokescreen, approved 2026-11-08) has been checked only for licence (MIT) and
  activity. Reviewing its configuration and testing that a denied host is refused are part of
  phase 2.
