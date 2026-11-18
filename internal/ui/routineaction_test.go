package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// ADR-0017 in the UI: a routine action's plan says which routine carries it out, never that a host
// agent runs it or that a host has not reported its command.
func TestRoutineActionInTheUI(t *testing.T) {
	ex := &spec.Routine{Fleet: "crm", Routine: "apply-update", Description: "d", Enabled: true, Worker: "ops-read", Autonomy: "propose",
		Trigger: spec.Trigger{Execute: &spec.ExecuteTrigger{Action: "crm.update_field"}}, Run: spec.Run{Command: "true"}, Limit: time.Minute}
	f := setupWith(t, []*spec.Routine{ex})
	ctx := context.Background()
	target := "crm:contact/c-110/country"
	k := store.Key{Kind: "target", Value: "crm_fix/" + target}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "crm_fix", Target: target, Keys: []store.Key{k}, Canonical: k}, "crm/scan", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "crm/scan", "")
	_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "crm/fix", "")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, Action: "crm.update_field",
		Params:   map[string]string{"contact": "c-110", "field": "country", "from": "france", "to": "FR"},
		PlanHash: "h-crm-0123456789", Outcome: "propose", Autonomy: "propose"}, cases.AwaitingApproval, "", target, true); err != nil {
		t.Fatal(err)
	}
	ck, _ := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/approvals", ck, nil, nil)
	for _, want := range []string{"once you approve, crm/apply-update carries it out, once",
		"Approving starts crm/apply-update once: it re-checks what the plan assumed"} {
		if !strings.Contains(page, want) {
			t.Errorf("Approvals has no %q", want)
		}
	}
	for _, not := range []string{"the host has not reported its command", "the host agent on"} {
		if strings.Contains(page, not) {
			t.Errorf("a routine action's plan says %q", not)
		}
	}
}
