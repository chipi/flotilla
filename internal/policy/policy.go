// Package policy is flotillad's decision on a planned action (ADR-0005 decision 4, RFC-0004 §5.1):
// agents plan, policy decides, the host agent runs. Decide is a pure function of the plan and the state
// the caller gathered, so every rule is tested on its own; the store applies the decision.
//
// The order is RFC-0004 §5.1's, and any "no" decides:
//  1. the registry: the action exists, is for the case's kind, its parameters are in scope and target
//     the case's own target, and the plan cites the run that re-checked it;
//  2. autonomy, lowest wins: the routine's, the action's own (earned, ADR-0008), the host's ceiling, a
//     tainted run (at most propose), an action that cannot be undone (at most propose), the freeze (at
//     most shadow: ADR-0014 changed RFC-0004's propose);
//  3. the per-target lock and the limits (per target per day, per host per hour, platform-wide per
//     hour, the circuit breaker, the host incident rule);
//  4. holds: the fleet's STOP, an operator's hold on the host or the target.
//
// Shadow records what policy would have done, then closes the case: v1's gate compares those records
// with what the operator would have done (RFC-0004 §10).
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/spec"
)

// Outcomes.
const (
	Refuse  = "refuse"  // the registry or autonomy says no: the case escalates
	Defer   = "defer"   // a lock, a limit or a hold: the case returns to handed_over
	Shadow  = "shadow"  // recorded only: the case closes as shadow_plan_recorded
	Propose = "propose" // awaits the operator's approval of this exact plan
	Auto    = "auto"    // runs
)

// Plan is what an acting job returns: one action on the case's target.
type Plan struct {
	CaseID string            `json:"case_id"`
	RunID  string            `json:"run_id"` // the run that re-checked the case: its numbers are on its record
	Action string            `json:"action"`
	Params map[string]string `json:"params"`
}

// Hash binds an approval to this exact plan (RFC-0004 §3.8): the case, the action and its parameters.
func (p Plan) Hash() string {
	keys := make([]string, 0, len(p.Params))
	for k := range p.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	params := make([][2]string, 0, len(keys))
	for _, k := range keys {
		params = append(params, [2]string{k, p.Params[k]})
	}
	// JSON of the parts: every value is quoted and escaped, so none can imitate a boundary.
	b, _ := json.Marshal([]any{p.CaseID, p.Action, params})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Limits are the guardrails' numbers (RFC-0004 §5.3). Zero means the default.
type Limits struct {
	HostPerHour       int `yaml:"host_per_hour"`
	PlatformPerHour   int `yaml:"platform_per_hour"`
	BreakerFailures   int `yaml:"breaker_failures"`    // failed or unknown executions in an hour: auto drops to propose
	HostIncidentCases int `yaml:"host_incident_cases"` // new cases on one host in HostIncidentWindow: auto paused there
}

func (l Limits) withDefaults() Limits {
	if l.HostPerHour <= 0 {
		l.HostPerHour = 3
	}
	if l.PlatformPerHour <= 0 {
		l.PlatformPerHour = 10
	}
	if l.BreakerFailures <= 0 {
		l.BreakerFailures = 3
	}
	if l.HostIncidentCases <= 0 {
		l.HostIncidentCases = 5
	}
	return l
}

// State is what the caller gathered for one plan.
type State struct {
	CaseKind, CaseTarget string
	Action               registry.Action
	ActionFound          bool
	RunOnRecord          bool   // the cited run exists and belongs to the case's acting routine
	RoutineAutonomy      string // the planning routine's autonomy (the freeze already applied by sched)
	ActionAutonomy       string // earned; a new action is shadow
	HostCeiling          string
	Tainted              bool
	Frozen               bool
	LockHeldBy           string // the case holding the target's lock, "" when free
	TargetToday          int    // executions of this action on this target today
	HostHour             int    // executions on this host in the last hour
	PlatformHour         int    // executions anywhere in the last hour
	FailedHour           int    // failed or unknown executions in the last hour
	HostNewCases         int    // cases opened on this host in the incident window
	FleetStopped         bool
	Holds                []string // active holds on the host or the target, as their descriptions
}

// Decision is policy's answer, with every check's result in order.
type Decision struct {
	Outcome  string   `json:"outcome"`
	Autonomy string   `json:"autonomy"`        // the lowest of the autonomy checks
	Would    string   `json:"would,omitempty"` // in shadow: what it would have decided
	Reasons  []string `json:"reasons"`
	PlanHash string   `json:"plan_hash"`
}

func (d Decision) String() string {
	b, _ := json.Marshal(d)
	return string(b)
}

// HostOf is the host a target lives on: "host:mini" is on mini. Only host targets exist in v1.
func HostOf(target string) string {
	if h, ok := strings.CutPrefix(target, "host:"); ok {
		return h
	}
	return ""
}

// lowest is the lowest level; a word that is not a level counts as shadow, never as itself.
func lowest(levels ...string) string {
	low := spec.AutonomyAuto
	for _, l := range levels {
		switch l {
		case spec.AutonomyShadow, spec.AutonomyPropose, spec.AutonomyAuto:
		default:
			l = spec.AutonomyShadow
		}
		if spec.Rank(l) < spec.Rank(low) {
			low = l
		}
	}
	return low
}

// Decide applies RFC-0004 §5.1 to one plan.
func Decide(p Plan, s State, lim Limits) Decision {
	lim = lim.withDefaults()
	d := Decision{PlanHash: p.Hash()}
	say := func(f string, a ...any) { d.Reasons = append(d.Reasons, fmt.Sprintf(f, a...)) }

	// 1. The registry.
	var refusals []string
	if !s.ActionFound {
		refusals = append(refusals, fmt.Sprintf("action %q is not in the registry", p.Action))
	} else {
		if !s.Action.For(s.CaseKind) {
			refusals = append(refusals, fmt.Sprintf("%s is not for %s cases", p.Action, s.CaseKind))
		}
		for name, prm := range s.Action.Params {
			v, ok := p.Params[name]
			switch {
			case !ok:
				refusals = append(refusals, fmt.Sprintf("parameter %s is missing", name))
			case prm.Kind == "target_host" && v != HostOf(s.CaseTarget):
				refusals = append(refusals, fmt.Sprintf("parameter %s=%q is not the case's host %q: no retargeting", name, v, HostOf(s.CaseTarget)))
			case prm.Kind == "enum" && !in(prm.Enum, v):
				refusals = append(refusals, fmt.Sprintf("parameter %s=%q is not one of %v", name, v, prm.Enum))
			}
		}
		for name := range p.Params {
			if _, ok := s.Action.Params[name]; !ok {
				refusals = append(refusals, fmt.Sprintf("parameter %s is not declared", name))
			}
		}
	}
	if !s.RunOnRecord {
		refusals = append(refusals, fmt.Sprintf("the plan cites run %q, which is not on the case's record", p.RunID))
	}
	if len(refusals) > 0 {
		d.Outcome = Refuse
		d.Reasons = append(d.Reasons, refusals...)
		return d
	}
	say("registry: %s for %s on %s", p.Action, s.CaseKind, s.CaseTarget)

	// 2. Autonomy, lowest wins.
	caps := []string{s.RoutineAutonomy, s.ActionAutonomy, s.HostCeiling}
	say("autonomy: routine %s, action %s, host ceiling %s", s.RoutineAutonomy, s.ActionAutonomy, s.HostCeiling)
	if s.Tainted {
		caps = append(caps, spec.AutonomyPropose)
		say("tainted run: at most propose")
	}
	if s.Action.Reversible == "no" {
		caps = append(caps, spec.AutonomyPropose)
		say("cannot be undone: at most propose")
	}
	if s.FailedHour >= lim.BreakerFailures {
		caps = append(caps, spec.AutonomyPropose)
		say("circuit breaker: %d failed or unknown executions in the last hour: at most propose", s.FailedHour)
	}
	if s.HostNewCases > lim.HostIncidentCases {
		caps = append(caps, spec.AutonomyPropose)
		say("host incident: %d new cases on %s: auto paused there", s.HostNewCases, HostOf(s.CaseTarget))
	}
	if s.Frozen {
		caps = append(caps, spec.AutonomyShadow)
		say("frozen: at most shadow")
	}
	d.Autonomy = lowest(caps...)
	var above []string // what the caps say once the shadow ones are set aside: what shadow would have done
	for _, c := range caps {
		if lowest(c) != spec.AutonomyShadow {
			above = append(above, c)
		}
	}
	unshadowed := lowest(above...)

	// 3 and 4. Locks, limits and holds: what would defer it.
	var blocks []string
	if s.LockHeldBy != "" {
		blocks = append(blocks, fmt.Sprintf("the target is locked by %s", s.LockHeldBy))
	}
	if s.TargetToday >= s.Action.Limits.PerTargetPerDay {
		blocks = append(blocks, fmt.Sprintf("%d of %d executions on this target today", s.TargetToday, s.Action.Limits.PerTargetPerDay))
	}
	if s.HostHour >= lim.HostPerHour {
		blocks = append(blocks, fmt.Sprintf("%d of %d executions on this host this hour", s.HostHour, lim.HostPerHour))
	}
	if s.PlatformHour >= lim.PlatformPerHour {
		blocks = append(blocks, fmt.Sprintf("%d of %d executions platform-wide this hour", s.PlatformHour, lim.PlatformPerHour))
	}
	if s.FleetStopped {
		blocks = append(blocks, "the fleet is STOPped")
	}
	for _, h := range s.Holds {
		blocks = append(blocks, "hold: "+h)
	}

	level := d.Autonomy
	if level == spec.AutonomyShadow {
		level = unshadowed
	}
	switch {
	case len(blocks) > 0:
		d.Outcome = Defer
		d.Reasons = append(d.Reasons, blocks...)
	case level == spec.AutonomyAuto:
		d.Outcome = Auto
	default:
		d.Outcome = Propose
	}
	if d.Autonomy == spec.AutonomyShadow {
		d.Would, d.Outcome = d.Outcome, Shadow
	}
	return d
}

func in(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
