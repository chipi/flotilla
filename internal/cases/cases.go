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
	Dismissed        = "dismissed"
	Reported         = "reported"
	ResolvedItself   = "resolved_itself"
	ShadowPlan       = "shadow_plan_recorded"
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
}{
	{Detected, Diagnosed}:         {by: []string{ByDiagnose}},
	{Detected, HandedOver}:        {by: []string{ByDetector}},
	{Diagnosed, Closed}:           {by: []string{ByDiagnose}, resolutions: []string{Dismissed, Reported}},
	{Diagnosed, HandedOver}:       {by: []string{ByDiagnose}},
	{Diagnosed, Escalated}:        {by: []string{ByDiagnose}},
	{HandedOver, Planned}:         {by: []string{ByAct}},
	{HandedOver, Closed}:          {by: []string{ByAct}, resolutions: []string{ResolvedItself}},
	{HandedOver, Escalated}:       {by: []string{ByAct, ByPolicy, ByTimer}},
	{Planned, Closed}:             {by: []string{ByPolicy}, resolutions: []string{ShadowPlan}},
	{Planned, AwaitingApproval}:   {by: []string{ByPolicy}},
	{Planned, Executing}:          {by: []string{ByPolicy}},
	{Planned, HandedOver}:         {by: []string{ByPolicy}},
	{Planned, Escalated}:          {by: []string{ByPolicy}},
	{AwaitingApproval, Executing}: {by: []string{ByOperator}},
	{AwaitingApproval, Escalated}: {by: []string{ByOperator, ByTimer}},
	{Executing, Checking}:         {by: []string{ByAct}},
	{Executing, Closed}:           {by: []string{ByAct}, resolutions: []string{ResolvedItself}},
	{Executing, Escalated}:        {by: []string{ByAct, ByTimer}},
	{Checking, Closed}:            {by: []string{ByCheck}, resolutions: []string{Verified}},
	{Checking, Escalated}:         {by: []string{ByCheck, ByTimer}},
	{Escalated, HandedOver}:       {by: []string{ByOperator}},
	{Escalated, Closed}:           {by: []string{ByOperator, BySync}, resolutions: []string{OperatorResolved, IssueResolved}},
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
