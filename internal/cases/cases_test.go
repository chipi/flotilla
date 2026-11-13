package cases

import "testing"

// RFC-0004 §3.5's diagram, written out here on its own: the table must allow exactly these moves, each
// for exactly these roles, and nothing else.
var diagram = map[[2]string][]string{
	{Detected, Diagnosed}:         {ByDiagnose},
	{Detected, HandedOver}:        {ByDetector},
	{Diagnosed, Closed}:           {ByDiagnose},
	{Diagnosed, HandedOver}:       {ByDiagnose},
	{Diagnosed, Escalated}:        {ByDiagnose},
	{HandedOver, Planned}:         {ByAct},
	{HandedOver, Closed}:          {ByAct},
	{HandedOver, Escalated}:       {ByAct, ByPolicy, ByTimer},
	{Planned, Closed}:             {ByPolicy},
	{Planned, AwaitingApproval}:   {ByPolicy},
	{Planned, Executing}:          {ByPolicy},
	{Planned, HandedOver}:         {ByPolicy},
	{Planned, Escalated}:          {ByPolicy},
	{AwaitingApproval, Executing}: {ByOperator},
	{AwaitingApproval, Escalated}: {ByOperator, ByTimer},
	{Executing, Checking}:         {ByAct},
	{Executing, Closed}:           {ByAct},
	{Executing, Escalated}:        {ByAct, ByTimer},
	{Checking, Closed}:            {ByCheck},
	{Checking, Escalated}:         {ByCheck, ByTimer},
	{Escalated, HandedOver}:       {ByOperator},
	{Escalated, Closed}:           {ByOperator, BySync},
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
	if Check(Planned, Closed, ByPolicy, ShadowPlan) != nil {
		t.Fatal("v1's shadow close refused")
	}
	if Check(Planned, Closed, ByPolicy, Verified) == nil {
		t.Fatal("a shadow plan closed as verified")
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
