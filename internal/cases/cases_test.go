package cases

import "testing"

// RFC-0004 §3.5's diagram, written out here on its own: the table must allow exactly these moves, each
// for exactly these roles, and nothing else.
var diagram = map[[2]string][]string{
	{Detected, Diagnosed}:          {ByDiagnose},
	{Detected, HandedOver}:         {ByDetector},
	{Diagnosed, Closed}:            {ByDiagnose},
	{Diagnosed, HandedOver}:        {ByDiagnose},
	{Diagnosed, Escalated}:         {ByDiagnose},
	{HandedOver, Planned}:          {ByAct},
	{HandedOver, Closed}:           {ByAct, ByDetector, ByOperator},
	{HandedOver, Escalated}:        {ByAct, ByPolicy, ByTimer},
	{Planned, Closed}:              {ByPolicy},
	{Planned, Watching}:            {ByPolicy},
	{Planned, AwaitingApproval}:    {ByPolicy},
	{Planned, Executing}:           {ByPolicy},
	{Planned, HandedOver}:          {ByPolicy},
	{Planned, Escalated}:           {ByPolicy},
	{AwaitingApproval, Executing}:  {ByOperator},
	{AwaitingApproval, Escalated}:  {ByOperator, ByTimer},
	{AwaitingApproval, HandedOver}: {ByOperator},
	{Executing, Checking}:          {ByAct},
	{Executing, Closed}:            {ByAct, ByOperator},
	{Executing, Escalated}:         {ByAct, ByTimer},
	{Checking, Closed}:             {ByCheck},
	{Checking, Escalated}:          {ByCheck, ByTimer},
	{Checking, HandedOver}:         {ByCheck},
	{Watching, Closed}:             {ByDetector},
	{Watching, HandedOver}:         {ByOperator},
	{Escalated, HandedOver}:        {ByOperator},
	{Escalated, Closed}:            {ByOperator, BySync},
}

var roles = []string{ByDetector, ByDiagnose, ByAct, ByCheck, ByPolicy, ByTimer, ByOperator, BySync}

func TestTableIsTheDiagram(t *testing.T) {
	for _, from := range Statuses {
		for _, to := range Statuses {
			allowed := diagram[[2]string{from, to}]
			for _, by := range roles {
				res := ""
				if to == Closed {
					if m, ok := table[move{from, to}]; ok && len(m.resolutions) > 0 {
						res = m.resolutions[0]
						if only := m.only[by]; len(only) > 0 {
							res = only[0] // a role pinned to some resolutions closes with one of those
						}
					}
				}
				got := Check(from, to, by, res) == nil
				want := contains(allowed, by)
				if got != want {
					t.Errorf("%s -> %s by %s: allowed=%v, the diagram says %v", from, to, by, got, want)
				}
			}
		}
	}
	if len(table) != len(diagram) {
		t.Errorf("the table has %d moves, the diagram %d", len(table), len(diagram))
	}
}

func TestResolutions(t *testing.T) {
	if Check(Planned, Closed, ByPolicy, PlanRecorded) != nil {
		t.Fatal("v1's record close refused")
	}
	if Check(Planned, Closed, ByPolicy, Verified) == nil {
		t.Fatal("a recorded plan closed as verified")
	}
	if Check(Planned, Closed, ByPolicy, "") == nil {
		t.Fatal("a close without a resolution")
	}
	if Check(Detected, HandedOver, ByDetector, Dismissed) == nil {
		t.Fatal("a resolution on a move that is not a close")
	}
	if Check(Closed, HandedOver, ByOperator, "") == nil {
		t.Fatal("a closed case reopened")
	}
}

// An escalated case is dismissed by a person only: no detector, timer or policy rules it away (ADR-0023).
func TestDismissedMove(t *testing.T) {
	if err := Check(Escalated, Closed, ByOperator, Dismissed); err != nil {
		t.Fatal(err)
	}
	for _, by := range []string{ByDetector, ByDiagnose, ByAct, ByCheck, ByPolicy, ByTimer} {
		if Check(Escalated, Closed, by, Dismissed) == nil {
			t.Errorf("%s dismissed an escalated case", by)
		}
	}
	if Check(AwaitingApproval, Closed, ByOperator, Dismissed) == nil || Check(Watching, Closed, ByOperator, Dismissed) == nil {
		t.Error("dismissed outside escalated")
	}
}

// A person answers a proposal that waits in handed_over: accepted (operator resolved) or rejected (dismissed), and only
// by the operator; the acting job and the detector keep their own resolutions.
func TestHandedOverAnswerMoves(t *testing.T) {
	for _, res := range []string{OperatorResolved, Dismissed} {
		if err := Check(HandedOver, Closed, ByOperator, res); err != nil {
			t.Errorf("the operator closing a handed-over case as %s: %v", res, err)
		}
	}
	for _, c := range []struct{ by, res string }{{ByOperator, ResolvedItself}, {ByOperator, Verified}, {ByAct, OperatorResolved},
		{ByAct, Dismissed}, {ByDetector, OperatorResolved}, {ByDetector, Dismissed}, {ByTimer, OperatorResolved}, {ByPolicy, Dismissed}} {
		if Check(HandedOver, Closed, c.by, c.res) == nil {
			t.Errorf("%s closed a handed-over case as %s", c.by, c.res)
		}
	}
}

// A watched case closes only as resolved itself, and only by the detector: the policy that put it
// there and the operator who answers it never close it (ADR-0023).
func TestWatchingMoves(t *testing.T) {
	if err := Check(Watching, Closed, ByDetector, ResolvedItself); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ by, res string }{{ByDetector, PlanRecorded}, {ByDetector, OperatorResolved}, {ByOperator, ResolvedItself}, {ByTimer, ResolvedItself}, {ByPolicy, ResolvedItself}} {
		if Check(Watching, Closed, c.by, c.res) == nil {
			t.Errorf("%s closed a watching case as %s", c.by, c.res)
		}
	}
	if Check(Planned, Watching, ByPolicy, "") != nil || Check(Planned, Watching, ByPolicy, PlanRecorded) == nil {
		t.Error("planned -> watching carries no resolution")
	}
	if Check(Watching, HandedOver, ByOperator, "") != nil || Check(Watching, HandedOver, ByTimer, "") == nil {
		t.Error("watching -> handed_over is the operator's alone")
	}
}
