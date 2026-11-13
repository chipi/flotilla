package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

var (
	opCaller    = Caller{Name: "operator", Role: RoleOperator}
	agentCaller = Caller{Name: "claude-homelab", Role: RoleAgent}
)

// proposedCase is a case on host:mini whose plan policy proposed: awaiting approval, the target locked.
func proposedCase(t *testing.T, s *Server, key string) string {
	t.Helper()
	id := plannedCase(t, s, key, "")
	if _, err := s.Store.ApplyPlan(context.Background(), store.PlanRecord{CaseID: id, Action: rerun,
		Params: map[string]string{"host": "mini"}, PlanHash: "hash-" + key + "-0123456789", Outcome: policy.Propose,
		Autonomy: policy.Propose}, cases.AwaitingApproval, "", "host:mini", true); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestApprove(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := proposedCase(t, s, "a")
	if _, err := s.ApproveCase(ctx, agentCaller, "test", id); err == nil {
		t.Fatal("an agent approved")
	}
	hash, err := s.ApproveCase(ctx, opCaller, "test", id)
	if err != nil || hash != "hash-a-0123456789" {
		t.Fatalf("approve: %q %v", hash, err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.Executing {
		t.Fatalf("after approve: %s", c.Status)
	}
	// the approver is who the envelope names (api.approvedBy reads it from the case)
	plans, _ := s.Store.Plans(ctx, id)
	c := caseStatus(t, s, id)
	if who, err := s.approvedBy(ctx, c, plans[len(plans)-1]); err != nil || who != "operator" {
		t.Fatalf("approved by: %q %v", who, err)
	}
	if h, _ := s.Store.LockHolder(ctx, "host:mini"); h != id {
		t.Fatalf("approved, the target is not held by the case: %q", h)
	}
	if _, err := s.ApproveCase(ctx, opCaller, "test", id); err == nil || AsProblem(err).Code != 409 {
		t.Fatalf("approved twice: %v", err)
	}
	if _, err := s.ApproveCase(ctx, opCaller, "test", "case-none"); err == nil || AsProblem(err).Code != 404 {
		t.Fatalf("an unknown case: %v", err)
	}
}

func TestApproveRefusedWhenFrozenOrHeld(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := proposedCase(t, s, "b")
	if err := s.SetFrozen(ctx, opCaller, "test", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveCase(ctx, opCaller, "test", id); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("approved while frozen: %v", err)
	}
	if err := s.SetFrozen(ctx, opCaller, "test", false, "test over"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.AddHold(ctx, "host:mini", time.Now().Add(time.Hour), "operator", "test", "disk work"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveCase(ctx, opCaller, "test", id); err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("approved while the host is held: %v", err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.AwaitingApproval {
		t.Fatalf("a refused approval moved the case: %s", c.Status)
	}
}

func TestReject(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	if err := s.SetActionAutonomy(ctx, opCaller, "test", rerun, "propose", "e2e"); err != nil {
		t.Fatal(err)
	}
	id := proposedCase(t, s, "c")
	if err := s.RejectCase(ctx, opCaller, "test", id, " "); err == nil || AsProblem(err).Code != 400 {
		t.Fatalf("rejected without a reason: %v", err)
	}
	if err := s.RejectCase(ctx, agentCaller, "test", id, "no"); err == nil {
		t.Fatal("an agent rejected")
	}
	if err := s.RejectCase(ctx, opCaller, "test", id, "the backup ran by hand"); err != nil {
		t.Fatal(err)
	}
	if c := caseStatus(t, s, id); c.Status != cases.Escalated {
		t.Fatalf("after reject: %s", c.Status)
	}
	if h, _ := s.Store.LockHolder(ctx, "host:mini"); h != "" {
		t.Fatalf("a rejected case still holds the target: %q", h)
	}
	if v, _ := s.Store.Control(ctx, actionSubject(rerun), "autonomy"); v != "shadow" {
		t.Fatalf("the action after a rejection: %q, want shadow", v)
	}
}
