package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/policy"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

const rerun = "backup.rerun_observability"

// plannedCase opens a failed_backup case on host:mini (key suffix keeps two cases on one target apart),
// hands it over, and has triage/cycle plan it in run runID, as an acting job would.
func plannedCase(t *testing.T, s *Server, key, runID string) string {
	t.Helper()
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:mini" + key}
	id, _, err := s.Store.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:mini", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "triage/cycle", "run "+runID); err != nil {
		t.Fatal(err)
	}
	if runID != "" {
		_ = s.Store.StartRun(ctx, store.Run{ID: runID, Routine: "triage/cycle", SpecHash: "x", Trigger: "event", Actor: "flotillad",
			Autonomy: "shadow", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: store.StatusRunning})
	}
	return id
}

func planFor(caseID, runID, host string) policy.Plan {
	return policy.Plan{CaseID: caseID, RunID: runID, Action: rerun, Params: map[string]string{"host": host}}
}

func caseStatus(t *testing.T, s *Server, id string) store.Case {
	t.Helper()
	c, err := s.Store.Case(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// v1: a new action is shadow; the plan is recorded with what it would have done, the case closes, and
// nothing holds the target.
func TestShadowPlanIsRecordedAndClosed(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := plannedCase(t, s, "", "run-1")
	d, pid, err := s.DecidePlan(ctx, planFor(id, "run-1", "mini"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Outcome != policy.Shadow || d.Would != policy.Propose {
		t.Fatalf("decision %+v", d)
	}
	c := caseStatus(t, s, id)
	if c.Status != cases.Closed || c.Resolution != cases.ShadowPlan {
		t.Fatalf("case %+v", c)
	}
	plans, _ := s.Store.Plans(ctx, id)
	if len(plans) != 1 || plans[0].ID != pid || plans[0].PlanHash != d.PlanHash || plans[0].Would != policy.Propose || len(plans[0].Reasons) == 0 {
		t.Fatalf("plan record %+v", plans)
	}
	if h, _ := s.Store.LockHolder(ctx, "host:mini"); h != "" {
		t.Fatalf("a shadow plan holds the target: %s", h)
	}
}

// At propose the case awaits approval holding the lock; a second case on the target is deferred; the
// lock goes when the first is rejected.
func TestProposeLocksAndASecondCaseDefers(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	_ = s.Store.SetControl(ctx, "triage/cycle", sched.KeyAutonomy, "propose", "user:ada", "api")
	_ = s.Store.SetControl(ctx, actionSubject(rerun), "autonomy", "propose", "user:ada", "api")
	a := plannedCase(t, s, "#a", "run-a")
	if d, _, err := s.DecidePlan(ctx, planFor(a, "run-a", "mini")); err != nil || d.Outcome != policy.Propose {
		t.Fatalf("first: %+v %v", d, err)
	}
	if c := caseStatus(t, s, a); c.Status != cases.AwaitingApproval {
		t.Fatalf("first case %s", c.Status)
	}
	if h, _ := s.Store.LockHolder(ctx, "host:mini"); h != a {
		t.Fatalf("lock held by %q, want %s", h, a)
	}
	b := plannedCase(t, s, "#b", "run-b")
	d, _, err := s.DecidePlan(ctx, planFor(b, "run-b", "mini"))
	if err != nil || d.Outcome != policy.Defer || !strings.Contains(strings.Join(d.Reasons, ";"), "locked by "+a) {
		t.Fatalf("second: %+v %v", d, err)
	}
	if c := caseStatus(t, s, b); c.Status != cases.HandedOver {
		t.Fatalf("a deferred case is %s", c.Status)
	}
	_ = s.Store.MoveCase(ctx, a, cases.Escalated, cases.ByOperator, "", "user:ada", "rejected")
	if h, _ := s.Store.LockHolder(ctx, "host:mini"); h != "" {
		t.Fatalf("a rejected proposal still holds the target: %s", h)
	}
}

func TestRefusedPlansEscalate(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := plannedCase(t, s, "", "run-1")
	d, _, err := s.DecidePlan(ctx, planFor(id, "run-1", "dgx"))
	if err != nil || d.Outcome != policy.Refuse || caseStatus(t, s, id).Status != cases.Escalated {
		t.Fatalf("retargeted: %+v %v", d, err)
	}
	// A run that is not the one that planned the case (or no run at all) is not on its record.
	id2 := plannedCase(t, s, "#2", "run-2")
	d, _, _ = s.DecidePlan(ctx, planFor(id2, "run-nope", "mini"))
	if d.Outcome != policy.Refuse || !strings.Contains(strings.Join(d.Reasons, ";"), "not on the case's record") {
		t.Fatalf("an unknown run: %+v", d)
	}
	// A real run, but of a routine other than the one that planned the case.
	k := store.Key{Kind: "target", Value: "failed_backup/host:mini#3"}
	id3, _, _ := s.Store.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:mini", Keys: []store.Key{k}, Canonical: k}, "m", "")
	_ = s.Store.MoveCase(ctx, id3, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = s.Store.MoveCase(ctx, id3, cases.Planned, cases.ByAct, "", "ops/act-host", "")
	_ = s.Store.StartRun(ctx, store.Run{ID: "run-3", Routine: "triage/cycle", SpecHash: "x", Trigger: "event", Actor: "flotillad",
		Autonomy: "shadow", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: store.StatusRunning})
	d, _, _ = s.DecidePlan(ctx, planFor(id3, "run-3", "mini"))
	if d.Outcome != policy.Refuse || !strings.Contains(strings.Join(d.Reasons, ";"), "not on the case's record") {
		t.Fatalf("another routine's run: %+v", d)
	}
}

func TestHoldsDefer(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	_ = s.Store.SetControl(ctx, "triage/cycle", sched.KeyAutonomy, "propose", "user:ada", "api")
	_ = s.Store.SetControl(ctx, actionSubject(rerun), "autonomy", "propose", "user:ada", "api")
	agent := Caller{Name: "claude-homelab", Role: RoleAgent}
	hid, err := s.AddHold(ctx, agent, "api", "host:mini", time.Hour, "migrating the backup disk")
	if err != nil {
		t.Fatal(err)
	}
	id := plannedCase(t, s, "", "run-1")
	d, _, _ := s.DecidePlan(ctx, planFor(id, "run-1", "mini"))
	if d.Outcome != policy.Defer || !strings.Contains(strings.Join(d.Reasons, ";"), "migrating the backup disk") {
		t.Fatalf("held: %+v", d)
	}
	if err := s.LiftHold(ctx, agent, "api", hid, "done"); AsProblem(err) == nil || AsProblem(err).Code != 403 {
		t.Fatalf("an agent lifting a hold: %v", err)
	}
	op := Caller{Name: "operator", Role: RoleOperator}
	if err := s.LiftHold(ctx, op, "api", hid, " "); AsProblem(err) == nil || AsProblem(err).Code != 400 {
		t.Fatalf("lifting without a reason: %v", err)
	}
	if err := s.LiftHold(ctx, op, "api", hid, "disk migrated"); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "triage/cycle", "")
	if d, _, _ := s.DecidePlan(ctx, planFor(id, "run-1", "mini")); d.Outcome != policy.Propose {
		t.Fatalf("after the hold: %+v", d)
	}
}

// The freeze reaches policy: whatever is set, a plan is shadow.
func TestFrozenPlansAreShadow(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	_ = s.Store.SetControl(ctx, "triage/cycle", sched.KeyAutonomy, "auto", "user:ada", "api")
	_ = s.Store.SetControl(ctx, actionSubject(rerun), "autonomy", "auto", "user:ada", "api")
	_ = s.Store.SetControl(ctx, sched.FreezeSubject, sched.KeyFrozen, "1", "user:ada", "api")
	id := plannedCase(t, s, "", "run-1")
	d, _, _ := s.DecidePlan(ctx, planFor(id, "run-1", "mini"))
	if d.Outcome != policy.Shadow || !strings.Contains(strings.Join(d.Reasons, ";"), "frozen") {
		t.Fatalf("frozen, and the record says so: %+v", d)
	}
}

func TestOnlyAPlannedCaseIsDecided(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := plannedCase(t, s, "", "run-1")
	_, _, _ = s.DecidePlan(ctx, planFor(id, "run-1", "mini")) // closes it (shadow)
	if _, _, err := s.DecidePlan(ctx, planFor(id, "run-1", "mini")); !errors.Is(err, store.ErrNotPlanned) {
		t.Fatalf("a closed case decided again: %v", err)
	}
}
