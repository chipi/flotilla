# flotilla

**A plain, spec-driven platform for small fleets of AI agents that act within limits you set and can
see.** A control plane decides and records, workers diagnose and plan, and host agents run only what
is signed and allowed on their host.

It runs one installation's operations today: a **triage** fleet that files real issues from alerts and
errors, a **jobs** fleet that watches backups, a **review** fleet that reviews closed cases, a **sites**
fleet that checks sites' search reach, and a **bugfix** fleet that drafts fixes as PRs. Every
problem is a case, every
action starts at record and earns its autonomy, and what leaves the platform is decided by code after
the model.

## Where to start

| If you want | Read |
| --- | --- |
| what it is and what runs today | the [README](https://github.com/chipi/flotilla#readme) |
| how it is built | [Architecture](architecture/index.md): context, components, a case end to end, data, deployment |
| the decisions and why | [Decisions (ADR)](adr/README.md), starting with [ADR-0001](adr/ADR-0001-platform-foundations.md); [Proposals (RFC)](rfc/README.md) |
| what keeps it safe | [The threat model](security.md), [security reviews](security-reviews/SR-0001-bugfix-coding-harness.md) |
| how it is tested | [Testing and validation](testing.md) |
| to operate a fleet | [Operations](operations/triage.md): triage, bugfix, seo, host keys, Slack, MCP |
| the example fleets, and running your own | [fleets/README.md](https://github.com/chipi/flotilla/blob/main/fleets/README.md) |
| where it is going | [the plan](wip/vision-plan.md), and [in progress](wip/README.md) |
| how it got here | [History](history/README.md) |

---

Source on GitHub: [chipi/flotilla](https://github.com/chipi/flotilla).
