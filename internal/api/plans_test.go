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

const rerun = "backup.rerun"

// plannedCase opens a failed_backup case on host:backup-01 (key suffix keeps two cases on one target apart),
// hands it over, and has triage/cycle plan it in run runID, as an acting job would.
func plannedCase(t *testing.T, s *Server, key, runID string) string {
	t.Helper()
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01" + key}
	id, _, err := s.Store.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k}, "ops/backup-monitor", "")
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
			Autonomy: "record", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: store.StatusRunning})
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

// v1: a new action is record; the plan is recorded with what it would have done, the case closes, and
// nothing holds the target.
func TestRecordedPlanIsRecordedAndClosed(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := plannedCase(t, s, "", "run-1")
	d, pid, err := s.DecidePlan(ctx, planFor(id, "run-1", "backup-01"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Outcome != policy.Record || d.Would != policy.Propose {
		t.Fatalf("decision %+v", d)
	}
	c := caseStatus(t, s, id)
	if c.Status != cases.Closed || c.Resolution != cases.PlanRecorded {
		t.Fatalf("case %+v", c)
	}
	plans, _ := s.Store.Plans(ctx, id)
	if len(plans) != 1 || plans[0].ID != pid || plans[0].PlanHash != d.PlanHash || plans[0].Would != policy.Propose || len(plans[0].Reasons) == 0 {
		t.Fatalf("plan record %+v", plans)
	}
	if h, _ := s.Store.LockHolder(ctx, "host:backup-01"); h != "" {
		t.Fatalf("a recorded plan holds the target: %s", h)
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
	if d, _, err := s.DecidePlan(ctx, planFor(a, "run-a", "backup-01")); err != nil || d.Outcome != policy.Propose {
		t.Fatalf("first: %+v %v", d, err)
	}
	if c := caseStatus(t, s, a); c.Status != cases.AwaitingApproval {
		t.Fatalf("first case %s", c.Status)
	}
	if h, _ := s.Store.LockHolder(ctx, "host:backup-01"); h != a {
		t.Fatalf("lock held by %q, want %s", h, a)
	}
	b := plannedCase(t, s, "#b", "run-b")
	d, _, err := s.DecidePlan(ctx, planFor(b, "run-b", "backup-01"))
	if err != nil || d.Outcome != policy.Defer || !strings.Contains(strings.Join(d.Reasons, ";"), "locked by "+a) {
		t.Fatalf("second: %+v %v", d, err)
	}
	if c := caseStatus(t, s, b); c.Status != cases.HandedOver {
		t.Fatalf("a deferred case is %s", c.Status)
	}
	_ = s.Store.MoveCase(ctx, a, cases.Escalated, cases.ByOperator, "", "user:ada", "rejected")
	if h, _ := s.Store.LockHolder(ctx, "host:backup-01"); h != "" {
		t.Fatalf("a rejected proposal still holds the target: %s", h)
	}
}

func TestRefusedPlansEscalate(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := plannedCase(t, s, "", "run-1")
	d, _, err := s.DecidePlan(ctx, planFor(id, "run-1", "model-host"))
	if err != nil || d.Outcome != policy.Refuse || caseStatus(t, s, id).Status != cases.Escalated {
		t.Fatalf("retargeted: %+v %v", d, err)
	}
	// A run that is not the one that planned the case (or no run at all) is not on its record.
	id2 := plannedCase(t, s, "#2", "run-2")
	d, _, _ = s.DecidePlan(ctx, planFor(id2, "run-nope", "backup-01"))
	if d.Outcome != policy.Refuse || !strings.Contains(strings.Join(d.Reasons, ";"), "not on the case's record") {
		t.Fatalf("an unknown run: %+v", d)
	}
	// A real run, but of a routine other than the one that planned the case.
	k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01#3"}
	id3, _, _ := s.Store.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k}, "m", "")
	_ = s.Store.MoveCase(ctx, id3, cases.HandedOver, cases.ByDetector, "", "m", "")
	_ = s.Store.MoveCase(ctx, id3, cases.Planned, cases.ByAct, "", "ops/act-host", "")
	_ = s.Store.StartRun(ctx, store.Run{ID: "run-3", Routine: "triage/cycle", SpecHash: "x", Trigger: "event", Actor: "flotillad",
		Autonomy: "record", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: store.StatusRunning})
	d, _, _ = s.DecidePlan(ctx, planFor(id3, "run-3", "backup-01"))
	if d.Outcome != policy.Refuse || !strings.Contains(strings.Join(d.Reasons, ";"), "not on the case's record") {
		t.Fatalf("another routine's run: %+v", d)
	}
}

func TestHoldsDefer(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	_ = s.Store.SetControl(ctx, "triage/cycle", sched.KeyAutonomy, "propose", "user:ada", "api")
	_ = s.Store.SetControl(ctx, actionSubject(rerun), "autonomy", "propose", "user:ada", "api")
	agent := Caller{Name: "claude-example", Role: RoleAgent}
	hid, err := s.AddHold(ctx, agent, "api", "host:backup-01", time.Hour, "migrating the backup disk")
	if err != nil {
		t.Fatal(err)
	}
	id := plannedCase(t, s, "", "run-1")
	d, _, _ := s.DecidePlan(ctx, planFor(id, "run-1", "backup-01"))
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
	if d, _, _ := s.DecidePlan(ctx, planFor(id, "run-1", "backup-01")); d.Outcome != policy.Propose {
		t.Fatalf("after the hold: %+v", d)
	}
}

// The freeze reaches policy: whatever is set, a plan is record.
func TestFrozenPlansAreRecorded(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	_ = s.Store.SetControl(ctx, "triage/cycle", sched.KeyAutonomy, "auto", "user:ada", "api")
	_ = s.Store.SetControl(ctx, actionSubject(rerun), "autonomy", "auto", "user:ada", "api")
	_ = s.Store.SetControl(ctx, sched.FreezeSubject, sched.KeyFrozen, "1", "user:ada", "api")
	id := plannedCase(t, s, "", "run-1")
	d, _, _ := s.DecidePlan(ctx, planFor(id, "run-1", "backup-01"))
	if d.Outcome != policy.Record || !strings.Contains(strings.Join(d.Reasons, ";"), "frozen") {
		t.Fatalf("frozen, and the record says so: %+v", d)
	}
}

const crmUpdate = "crm.update_field"

// plannedOther is plannedCase for another kind and target, planned by triage/cycle in run runID.
func plannedOther(t *testing.T, s *Server, kind, target, runID string) string {
	t.Helper()
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: kind + "/" + target}
	id, _, err := s.Store.OpenCase(ctx, store.NewCase{Kind: kind, Target: target, Keys: []store.Key{k}, Canonical: k}, "ops/monitor", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/monitor", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "triage/cycle", "run "+runID); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.StartRun(ctx, store.Run{ID: runID, Routine: "triage/cycle", SpecHash: "x", Trigger: "event", Actor: "flotillad",
		Autonomy: "record", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: store.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	return id
}

func crmPlan(id, runID string) policy.Plan {
	return policy.Plan{CaseID: id, RunID: runID, Action: crmUpdate,
		Params: map[string]string{"contact": "c1", "field": "email", "from": "a@example.org", "to": "b@example.org"}}
}

// earned sets the routine's autonomy and an action's earned autonomy, as the operator would.
func earned(t *testing.T, s *Server, routine, action, name string) {
	t.Helper()
	ctx := context.Background()
	if err := s.Store.SetControl(ctx, "triage/cycle", sched.KeyAutonomy, routine, "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetControl(ctx, actionSubject(name), "autonomy", action, "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
}

// ADR-0017 and the operator's decision of 2026-11-19: a routine action reaches auto only when the routine's autonomy, the action's
// earned autonomy and flotillad.yaml's routine_actions.max_autonomy all allow it. Unset it is propose, so no decision changes
// until an installation opts in.
func TestRoutineActionAutoNeedsEveryCap(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		option, routine, action string
		outcome, status         string
	}{
		{"", "auto", "auto", policy.Propose, cases.AwaitingApproval},
		{"propose", "auto", "auto", policy.Propose, cases.AwaitingApproval},
		{"auto", "auto", "auto", policy.Auto, cases.Executing},
		{"auto", "propose", "auto", policy.Propose, cases.AwaitingApproval},
		{"auto", "auto", "propose", policy.Propose, cases.AwaitingApproval},
		{"auto", "auto", "record", policy.Record, cases.Closed},
	} {
		s := newServer(t, func(*spec.Routine) {})
		s.RoutineActionMax = c.option
		earned(t, s, c.routine, c.action, crmUpdate)
		id := plannedOther(t, s, "crm_fix", "crm:contact/c-1/email", "run-1")
		d, _, err := s.DecidePlan(ctx, crmPlan(id, "run-1"))
		name := "option " + c.option + ", routine " + c.routine + ", action " + c.action
		if err != nil || d.Outcome != c.outcome {
			t.Errorf("%s: %+v %v, want %s", name, d, err, c.outcome)
			continue
		}
		if got := caseStatus(t, s, id).Status; got != c.status {
			t.Errorf("%s: the case is %s, want %s", name, got, c.status)
		}
		if h, _ := s.Store.LockHolder(ctx, "crm:contact/c-1/email"); (h == id) != (c.status != cases.Closed) {
			t.Errorf("%s: the lock is held by %q", name, h)
		}
	}
}

// The option is for routine actions only: a host action at auto everywhere still proposes (its executor's ceiling is propose), and a
// repo change never goes above propose.
func TestRoutineActionOptionDoesNotLiftHostOrRepoActions(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	s.RoutineActionMax = spec.AutonomyAuto
	earned(t, s, "auto", "auto", rerun)
	id := plannedCase(t, s, "", "run-1")
	d, _, err := s.DecidePlan(ctx, planFor(id, "run-1", "backup-01"))
	if err != nil || d.Outcome != policy.Propose || !strings.Contains(strings.Join(d.Reasons, ";"), "host ceiling propose") {
		t.Fatalf("a host action with the option at auto: %+v %v", d, err)
	}
	if err := s.Store.SetControl(ctx, actionSubject("repo.process_change"), "autonomy", "auto", "user:ada", "api"); err != nil {
		t.Fatal(err)
	}
	rid := plannedOther(t, s, "process_change", "repo:example/fleets", "run-2")
	d, _, err = s.DecidePlan(ctx, policy.Plan{CaseID: rid, RunID: "run-2", Action: "repo.process_change",
		Params: map[string]string{"file": "none", "rule": "below_cycle", "change": "x"}})
	if err != nil || d.Outcome != policy.Propose {
		t.Fatalf("a repo change with the option at auto: %+v %v", d, err)
	}
}

// RoutineActionCeiling: only auto lifts it; anything else is propose.
func TestRoutineActionCeilingDefaultsToPropose(t *testing.T) {
	for in, want := range map[string]string{"": "propose", "propose": "propose", "auto": "auto", "record": "propose", "yolo": "propose"} {
		if got := (&Server{RoutineActionMax: in}).RoutineActionCeiling(); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

func TestOnlyAPlannedCaseIsDecided(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := plannedCase(t, s, "", "run-1")
	_, _, _ = s.DecidePlan(ctx, planFor(id, "run-1", "backup-01")) // closes it (record)
	if _, _, err := s.DecidePlan(ctx, planFor(id, "run-1", "backup-01")); !errors.Is(err, store.ErrNotPlanned) {
		t.Fatalf("a closed case decided again: %v", err)
	}
}

// A shadow run's plan is never decided: what it would run is a shadow record. The shadow path does not call DecidePlan, and this is
// the refusal under it (Q7: the action is blocked by construction).
func TestAShadowRunsPlanIsNeverDecided(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, func(*spec.Routine) {})
	id := plannedCase(t, s, "", "")
	if err := s.Store.StartRun(ctx, store.Run{ID: "shadow-run", Routine: "triage/cycle@v2", SpecHash: "x", Trigger: "shadow", Actor: "flotillad", Autonomy: "record",
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: store.StatusRunning, Version: 2, Shadow: true, InputKey: "live-1"}); err != nil {
		t.Fatal(err)
	}
	_, pid, err := s.DecidePlan(ctx, planFor(id, "shadow-run", "backup-01"))
	if !errors.Is(err, store.ErrShadowRun) || pid != 0 {
		t.Fatalf("a shadow's plan: %v %d", err, pid)
	}
	if c := caseStatus(t, s, id); c.Status != cases.Planned {
		t.Fatalf("the case moved: %+v", c)
	}
	if ps, _ := s.Store.Plans(ctx, id); len(ps) != 0 {
		t.Fatalf("a plan was recorded: %+v", ps)
	}
}
