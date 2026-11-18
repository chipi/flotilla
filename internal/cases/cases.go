// Package cases holds the case lifecycle (ADR-0005 decision 1, RFC-0004 §3.5): the statuses, who may
// move a case between them, and how a closed case was closed. The store enforces it on every status
// change; it governs status only, so events may still be appended to a closed case.
package cases

import "fmt"

// Statuses.
const (
	Detected         = "detected"
	Diagnosed        = "diagnosed"
	HandedOver       = "handed_over"
	Planned          = "planned"
	AwaitingApproval = "awaiting_approval"
	Executing        = "executing"
	Checking         = "checking"
	Escalated        = "escalated"
	Closed           = "closed"
)

// Who moves a case. Jobs act through flotillad, which records which job's role made the move.
const (
	ByDetector = "detector" // a monitor: opens cases, and hands over a known kind directly
	ByDiagnose = "diagnose" // the diagnosing job
	ByAct      = "act"      // the acting job: re-check, plan, and the actuator's result
	ByCheck    = "check"    // the check run after an execution
	ByPolicy   = "policy"   // flotillad's policy (RFC-0004 §5.1) and its recurrence rule
	ByTimer    = "timer"    // flotillad's stuck-state timers and expiries
	ByOperator = "operator" // a person: approve, reject, reassign, resolve
	BySync     = "sync"     // an outside record resolving it (a GitHub issue closed)
)

// How a case was closed.
const (
	Dismissed      = "dismissed"
	Reported       = "reported"
	ResolvedItself = "resolved_itself"
	// RFC-0013: the problem never existed on main (a dev branch's error); counted apart from resolved_itself,
	// which means it went away
	NotReproducible  = "not_reproducible"
	PlanRecorded     = "plan_recorded"
	Verified         = "verified"
	OperatorResolved = "operator_resolved"
	IssueResolved    = "issue_resolved"
)

type move struct{ from, to string }

// table is RFC-0004 §3.5's diagram: each allowed move, the roles that may make it and, for a move to
// closed, the resolutions it may carry.
var table = map[move]struct {
	by          []string
	resolutions []string
	// only, when set, pins a role to the resolutions it alone may close with
	only map[string][]string
}{
	{Detected, Diagnosed}:   {by: []string{ByDiagnose}},
	{Detected, HandedOver}:  {by: []string{ByDetector}},
	{Diagnosed, Closed}:     {by: []string{ByDiagnose}, resolutions: []string{Dismissed, Reported}},
	{Diagnosed, HandedOver}: {by: []string{ByDiagnose}},
	{Diagnosed, Escalated}:  {by: []string{ByDiagnose}},
	{HandedOver, Planned}:   {by: []string{ByAct}},
	// the monitor that opened it closes a case of a kind no routine acts on, once it stops finding it
	// (operator, 2026-11-15: RFC-0012's findings)
	{HandedOver, Closed}: {by: []string{ByAct, ByDetector}, resolutions: []string{ResolvedItself, NotReproducible},
		only: map[string][]string{ByDetector: {ResolvedItself}}},
	{HandedOver, Escalated}:       {by: []string{ByAct, ByPolicy, ByTimer}},
	{Planned, Closed}:             {by: []string{ByPolicy}, resolutions: []string{PlanRecorded}},
	{Planned, AwaitingApproval}:   {by: []string{ByPolicy}},
	{Planned, Executing}:          {by: []string{ByPolicy}},
	{Planned, HandedOver}:         {by: []string{ByPolicy}},
	{Planned, Escalated}:          {by: []string{ByPolicy}},
	{AwaitingApproval, Executing}: {by: []string{ByOperator}},
	{AwaitingApproval, Escalated}: {by: []string{ByOperator, ByTimer}},
	// a version rolled back (docs/wip/v2-design.md section 1): its plan awaiting approval is withdrawn and the case
	// returns to the acting routine, which plans again with the live version
	{AwaitingApproval, HandedOver}: {by: []string{ByOperator}},
	{Executing, Checking}:          {by: []string{ByAct}},
	// a person closes a repo change they applied by hand as verified (ADR-0016); the acting job still
	// closes only as resolved itself
	{Executing, Closed}: {by: []string{ByAct, ByOperator}, resolutions: []string{ResolvedItself, Verified},
		only: map[string][]string{ByAct: {ResolvedItself}, ByOperator: {Verified}}},
	{Executing, Escalated}:  {by: []string{ByAct, ByTimer}},
	{Checking, Closed}:      {by: []string{ByCheck}, resolutions: []string{Verified}},
	{Checking, Escalated}:   {by: []string{ByCheck, ByTimer}},
	{Escalated, HandedOver}: {by: []string{ByOperator}},
	{Escalated, Closed}:     {by: []string{ByOperator, BySync}, resolutions: []string{OperatorResolved, IssueResolved}},
}

// Statuses lists every status, in lifecycle order.
var Statuses = []string{Detected, Diagnosed, HandedOver, Planned, AwaitingApproval, Executing, Checking, Escalated, Closed}

// Check refuses a status change the table does not allow: an unknown move, a role that may not make
// it, or a close without one of the move's resolutions (and a resolution on any other move).
func Check(from, to, by, resolution string) error {
	m, ok := table[move{from, to}]
	if !ok {
		return fmt.Errorf("a case cannot go from %s to %s", from, to)
	}
	if !contains(m.by, by) {
		return fmt.Errorf("%s may not move a case from %s to %s", by, from, to)
	}
	if to == Closed {
		if !contains(m.resolutions, resolution) {
			return fmt.Errorf("closing from %s needs one of %v, not %q", from, m.resolutions, resolution)
		}
		if only, ok := m.only[by]; ok && !contains(only, resolution) {
			return fmt.Errorf("%s closes a case from %s only as %v, not %q", by, from, only, resolution)
		}
	} else if resolution != "" {
		return fmt.Errorf("only a close carries a resolution")
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
