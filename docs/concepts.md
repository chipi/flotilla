# Concepts

flotilla runs business processes on AI agents and keeps a person in control of them. This page
explains its ideas. Where a part is designed but not built yet, it says so.

## Agents, in flotilla's words

An **agent** is software that does a piece of work with a model at its core: it reads, decides,
uses tools and acts. That is the word the industry uses, and the one this page starts from.

In flotilla an agent is a **routine**: a business process, run by code, with a model wherever the
work needs judgement. The rest of flotilla's words hang off it:

- agents work in a **fleet**: the agents of one business area (triage, sales ops, customer success),
  specialized in it, with that domain's expertise (its playbooks, skills and facts);
- an agent does its model work at **stations**, one per step that needs judgement, each a model with
  its instructions, tools and limits;
- each station's model runs in a **harness**, the loop that drives the model and its tools;
- an agent changes by **versions**: a new version is tried in **shadow** beside the live one before
  it takes over;
- and **flotilla** is where a business runs its agents and governs them.

So when this page says routine, read agent; a fleet is a team of agents specialized in one domain.

## The words

| Word | What it is |
| --- | --- |
| **flotilla** | the platform: where a business runs its agents and governs them |
| **fleet** | a team of agents (routines) specialized in one business area (triage, sales ops, customer success ...), with that domain's playbooks, skills and facts |
| **routine** | an agent: one business process, run by code on a schedule or on a case, with a model at each step that needs judgement |
| **version** | one way of running a routine. A routine has a live version, and may have shadow versions being tried beside it |
| **station** | one model step inside a version: its harness, its model, the texts the model reads, its tools and its limits |
| **harness** | the program that runs a model's loop for a station: sends the texts, offers the tools, runs them, stops it. flotilla's own loop, or a coding agent |
| **case** | one piece of work a routine handles, from detection to close, with its whole timeline |
| **autonomy** | what a routine's plans may do: **record** (write down what it would do), **propose** (a person approves each), **auto** (act alone, within its limits) |
| **shadow** | a version running beside the live one, on the same inputs, with no effect on the system |

## A routine: code, with stations where judgement is needed

A routine's code is the process: what runs when, in what order, what may leave the platform. Where a
step needs judgement (read an alert and decide, reproduce a bug, write a fix, review it), the code
hands that step to a **station**: a model, with the texts it reads, the tools it may use and its
limits. A routine has as many stations as its process has such steps, each on the model and harness
that suit it.

The code coordinates the stations. Work moves between them through the routine's code and the case's
recorded timeline, so every handoff can be read afterwards.

## Harnesses, and one contract for all of them

A station runs on a harness: flotilla's own loop, or a coding agent that edits a repository. flotilla
meets every harness through one contract, its **seams**: how the texts go in, how the model is reached
(always through the installation's own gateway), how the run is read back, how it is capped and
stopped, what it may touch, how a killed run resumes, what is recorded, what it costs, and that it
reaches nothing else. A harness is adopted when a test proves every seam with the real program; from
then on any station can run on it.

- **Built:** flotilla's own loop, and pi as the reference coding harness, its seams proven by a test
  with the real program (not yet released).
- **Planned:** more coding harnesses, adopted one at a time, each through the same test.

## Earning trust

Inside each station: the model, the texts it reads (declared files, hashed and checked before a run),
its tools and a read-only switch, and its limits. Around every routine, flotilla adds what a model
cannot give itself:

1. **Code decides what leaves.** A station proposes; rules you can read decide.
2. **Autonomy is earned, one action at a time.** Every action starts at record, moves to propose, and
   only then to auto, each step on its record of plans judged right.
3. **A person approves what cannot be undone.** A sent email, a merge, a launch.
4. **Everything is recorded.** Every decision, model call and approval, with who and why.
5. **Changes are tried before they ship,** as shadow versions (below).
6. **You can always stop it.** A routine, a fleet, or everything.

## Changing a process: versions and shadows

A business process changes. In flotilla a change is a new **version** of the routine:

1. It arrives as a **shadow**: it sees the same inputs as the live version, at the same moments, and
   writes only to its own record. It files nothing, sends nothing, moves no case, earns no autonomy.
2. Its decisions are **paired** with the live version's, input by input, and compared.
3. The operator **promotes** it when the evidence the routine asks for is in. Its autonomy is chosen
   at promote, never inherited in silence.
4. If it turns out wrong, the operator **rolls back** to the previous version at once.

**Built, not yet released:** versions, promote, roll back, retire, the version on every record.
**Planned:** shadow runs with no effect and their pairs.

## Measuring on your own work

What matters is how a routine does on the business's own work. flotilla measures there:

- **shadow pairs:** a new version against the live one, on the same live inputs;
- **replay:** a change run against recorded past cases before it ships;
- **scenario evaluations:** a version run on given inputs, with recorded tool results so the run can
  be repeated, scored by eval-harness, flotilla's sister project;
- **model checks:** each model the installation uses checked daily for what its routines need.

A routine declares which of these its promote requires.

## Where to read more

- The design: [the v2 design](wip/v2-design.md) and
  [ADR-0020](adr/ADR-0020-routine-versions-stations-harnesses.md).
- The harness contract: `bugfix/HARNESS_SEAMS.md` (moving to `runtime/`).
- Trying it: [Developing flotilla](development.md) and `make demo`.
