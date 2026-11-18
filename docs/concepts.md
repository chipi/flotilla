# Concepts

flotilla runs business processes on AI agents and keeps a person in control of them. This page
explains its ideas, and how they map onto how the industry talks about agents. Where a part is
designed but not built yet, it says so.

## The words

| Word | What it is |
| --- | --- |
| **flotilla** | the platform: where a business runs its agents and governs them |
| **fleet** | a team of routines for one business area (triage, sales ops, customer success ...) |
| **routine** | one business process, run by code on a schedule or on a case. In the industry's words, an **agent** |
| **version** | one way of running a routine. A routine has a live version, and may have shadow versions being tried beside it |
| **station** | one model step inside a version: its harness, its model, the texts the model reads, its tools and its limits. A routine has as many stations as its process has model steps |
| **harness** | the program that runs a model's loop for a station: sends the prompt, offers tools, runs them, stops it. flotilla's own, or a coding agent such as pi, Claude Code or Codex |
| **case** | one piece of work a routine handles, from detection to close, with its whole timeline |
| **autonomy** | what a routine's plans may do: **record** (write down what it would do), **propose** (a person approves each), **auto** (act alone, within its limits) |
| **shadow** | a version running beside the live one, on the same inputs, with no effect on the system, so the two can be compared |

## An agent is code with a model inside

The industry's working definition of an agent: deterministic software with a non-deterministic part,
the model, inside it. That is a routine. The code decides the process (what runs when, what happens
next, what may leave); the model does the steps where judgement is needed. Each of those steps is a
station.

So flotilla's stance: **code coordinates agents.** Stations hand work to each other through the
routine's code and recorded cases, not through one model directing others. Every handoff can be
read afterwards.

## Agents are hard: the harness

Running a model's loop well is a long list of work: the loop itself, provider abstraction, tool-call
validation, stop conditions, retries, turn caps, timeouts, session state, telemetry, isolation. That
list is a harness's job.

flotilla does not build every harness. It runs any harness through one contract, its **seams**: the
eleven places where flotilla meets a harness (the prompt in, the system text, the model route through
the installation's gateway, the stream read back, the turn cap, tools and read-only, session and
resume, call recording, cost, no host contacted but the gateway, isolation). A harness is adopted when
a test proves every seam with the real program. Then any station can run on it.

- **Built:** flotilla's own loop, and pi as the reference coding harness, its seams proven by a test
  with the real program (not yet released).
- **Planned:** Claude Code, Codex, opencode, DeepSeek Harness and mini-SWE-agent, adopted one at a
  time.

Business processes, not chat: the parts of the list that serve interactive chat agents (streaming
to a user, sockets, image handling) are out of scope.

## Trust, in layers

A common picture of an agent's trust stacks it from the inside: the model, its system prompt, its
tools, hooks around the tools, rules over the whole. flotilla keeps that stack inside each station:
the model, the texts it reads (declared files, hashed and checked before a run), its tools and
read-only switch, hooks, limits.

Then it adds the layers one agent cannot give itself:

1. **Code decides, not the model.** The station proposes; rules you can read decide what leaves.
2. **Autonomy is earned, per action.** Every action starts at record, then propose, then auto, each
   step on its record of plans judged right.
3. **A person approves what cannot be undone.** A sent email, a merge, a launch: always a person.
4. **Everything is recorded.** Every decision, model call and approval, with who and why.
5. **Changes are tried before they ship.** A new version runs in shadow beside the live one, with no
   effect, and is compared before it goes live; it can be rolled back at once.
6. **You can always stop it.** A routine, a fleet, or everything.

## Changing a process: versions and shadows

A business process changes. In flotilla a change is a new **version** of the routine:

1. It arrives as a **shadow**: it sees the same inputs as the live version, at the same moments, and
   writes only to its own record. It files nothing, sends nothing, moves no case, earns no autonomy.
2. Its decisions are **paired** with the live version's, input by input, and compared.
3. The operator **promotes** it when the evidence the routine asks for is in: compared pairs, replays
   of past cases, evaluation results. Its autonomy is chosen at promote, never inherited in silence.
4. If it turns out wrong, the operator **rolls back** to the previous version at once.

**Built, not yet released:** versions, promote, roll back, retire, the version on every record.
**Planned:** shadow runs with no effect and their pairs.

## Measuring: on your own work, not benchmarks

Public benchmarks measure a model on someone else's tasks; their biggest victims are the people too
busy to try the model on their own. A business needs to know how an agent does on its own work.
flotilla measures there:

- **shadow pairs:** a new version against the live one on the same live inputs;
- **replay:** a change run against recorded past cases before it ships;
- **scenario evals:** a version run offline on given inputs, with recorded tool results so the run
  can be repeated, scored by eval-harness, flotilla's sister project;
- **model contracts:** each model the installation uses checked daily for what the routines need.

A routine declares which of these its promote requires.

## Standards

What a shared standard for agents should be, and where flotilla stands:

| A standard should be | flotilla |
| --- | --- |
| domain specific | each fleet is one business area's processes |
| a full harness | the seams: any harness that passes them runs any station |
| portable | fleets are folders in a repository; an installation keeps its own |
| vendor agnostic | any OpenAI-compatible model endpoint, through one gateway |
| composable | stations within a routine; routines through cases |

## Where to read more

- The design: [the v2 design](wip/v2-design.md) and [ADR-0020](adr/ADR-0020-routine-versions-stations-harnesses.md).
- The harness contract: `runtime/HARNESS_SEAMS.md` (today `bugfix/HARNESS_SEAMS.md`).
- Trying it: [Developing flotilla](development.md) and `make demo`.
