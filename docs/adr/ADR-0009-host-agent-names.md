# ADR-0009 — Host agent names: workers think, host agents act

**Status:** Accepted (operator, 2026-11-11)
**Date:** 2026-11-11

## Context

[ADR-0006](ADR-0006-node-agent.md) named the program flotilla installs on each host in several ways:
"host agent" in prose, `flotilla-node` as the binary, `flotilla node …` in the CLI, `nodes` in the
store, "node id" and "node gateway" in the protocol. Its role that runs writes was the "actuator"
(ADR-0005's word), which reads like a separate component. The UI page for them is called Hosts.
Nothing of it was built, so renaming cost only documents.

## Decision

| Before | After |
| --- | --- |
| worker | **worker** (unchanged): a container that runs routines; the thinking happens there |
| host agent, node, `flotilla-node` | **host agent** in prose; the binary **`flotilla-host`**; the CLI **`flotilla host list\|install\|upgrade\|revoke\|remove`**; the store `hosts`; "host id", "host gateway" |
| actuator | not a name of its own: the host agent's role **`actions`** (it runs host actions) |
| connector | **connector** (unchanged): the host agent's role that talks to a provider |

In one line: workers think, host agents act. The operator manages hosts; a host agent has the roles
`actions`, `connector`, or both.

"Agent" also names the AI agents, but "host agent" is the operator's word and the usual one
(Datadog Agent, Grafana Agent); the binary, the CLI and the files never say "agent".

Accepted RFCs and ADRs keep their wording as records; this ADR is the mapping. ADR-0006's file
name stays.

## Consequences

**Positive:** one name per thing; the UI, the CLI and the store all say "host".

**Neutral:** no code or deployment changes; the names apply when the host agent is built.

## Alternatives considered

- **"node" everywhere:** already the CLI word, but a "node" is usually the machine itself, and the
  UI says Hosts.
- **`flotilla-agent` as the binary:** puts "agent" in the CLI and the files, next to the AI agents.
- **Keep "actuator":** a role is not a component; `actions` says what it does.
