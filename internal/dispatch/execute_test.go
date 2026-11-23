package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// withExecutor adds an executing routine for action (RFC-0010 §3) to the fixture's scheduler.
func (f *fixture) withExecutor(action string) *spec.Routine {
	j := &spec.Routine{Fleet: "bugfix", Routine: "open-pr", Description: "d", Enabled: true, Worker: "ops", Autonomy: "propose",
		Trigger: spec.Trigger{Execute: &spec.ExecuteTrigger{Action: action}}, Run: spec.Run{Command: "true"}, Limit: time.Minute}
	f.d.API.Sched.SetRoutines(append(f.d.API.Sched.Routines(), j))
	return j
}

// approvedRepo is a process_change case whose repo.process_change plan a person approved: executing.
func (f *fixture) approvedRepo(t *testing.T, key string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "process_change/repo:host/registry.json" + key}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "process_change", Target: "repo:host/registry.json" + key, Keys: []store.Key{k}, Canonical: k}, "ops/case-review", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ to, by string }{{cases.HandedOver, cases.ByDetector}, {cases.Planned, cases.ByAct}} {
		if err := f.st.MoveCase(ctx, id, m.to, m.by, "", "ops/process-change", "run r1"); err != nil {
			t.Fatal(err)
		}
	}
	pid, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r1", Action: "repo.process_change",
		Params:   map[string]string{"file": "host/registry.json", "rule": "below_cycle", "change": "- \"20h\"\n+ \"25h\""},
		PlanHash: "abcdef0123456789" + key, Outcome: "propose", Autonomy: "propose"}, cases.AwaitingApproval, "", "repo:host/registry.json"+key, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.MoveCase(ctx, id, cases.Executing, cases.ByOperator, "", "user:ada", "approved"); err != nil {
		t.Fatal(err)
	}
	return id, pid
}

// An approved repo action goes to its executing routine once, with the approved plan; "executed" moves
// the case to checking with what it made.
func TestExecuteRunsOnceAndMovesToChecking(t *testing.T) {
	f := setup(t)
	f.withExecutor("repo.process_change")
	id, pid := f.approvedRepo(t, "#e")
	f.result(`{"outcome": "executed", "detail": "draft PR opened", "ref": "chipi/flotilla#7"}`)
	f.pass(t)
	c := f.cs(t, id)
	if c.Status != cases.Checking || c.Holder != "" {
		t.Fatalf("after executed: %+v", c)
	}
	if e, ref := executed(t, f, id); e.By != cases.ByAct || e.Actor != "bugfix/open-pr" || ref != "chipi/flotilla#7" || strings.Contains(e.Detail, "#7") {
		t.Fatalf("the move %+v and its ref %q: the ref is its own line, not words in the move", e, ref)
	}
	if f.w.runs() != 1 {
		t.Fatalf("%d runs, want 1", f.w.runs())
	}
	var pj planJSON
	if err := json.Unmarshal([]byte(f.w.got[0].Env["FLOTILLA_PLAN"]), &pj); err != nil || pj.ID != pid || pj.Action != "repo.process_change" ||
		pj.PlanHash != "abcdef0123456789#e" || pj.Params["change"] != "- \"20h\"\n+ \"25h\"" {
		t.Fatalf("FLOTILLA_PLAN %q", f.w.got[0].Env["FLOTILLA_PLAN"])
	}
	if !strings.Contains(f.w.got[0].Env["FLOTILLA_CASE"], id) {
		t.Fatalf("FLOTILLA_CASE %q", f.w.got[0].Env["FLOTILLA_CASE"])
	}
}

// A routine action (registry executor "routine", here the crm example's crm.update_field) goes to its
// executing routine like a repo change: once, with the approved plan, then to checking. No host agent
// is involved: its target is not a host.
func TestARoutineActionRunsItsExecutor(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	j := f.withExecutor("crm.update_field")
	j.Fleet, j.Routine = "crm", "apply-update"
	k := store.Key{Kind: "target", Value: "crm_fix/crm:contact/c-101/email"}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "crm_fix", Target: "crm:contact/c-101/email", Keys: []store.Key{k}, Canonical: k}, "crm/scan", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ to, by string }{{cases.HandedOver, cases.ByDetector}, {cases.Planned, cases.ByAct}} {
		if err := f.st.MoveCase(ctx, id, m.to, m.by, "", "crm/fix", "run r1"); err != nil {
			t.Fatal(err)
		}
	}
	params := map[string]string{"contact": "c-101", "field": "email", "from": "Ana@Example.org", "to": "ana@example.org"}
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r1", Action: "crm.update_field", Params: params,
		PlanHash: "0123456789abcdef", Outcome: "propose", Autonomy: "propose"}, cases.AwaitingApproval, "", "crm:contact/c-101/email", true); err != nil {
		t.Fatal(err)
	}
	if err := f.st.MoveCase(ctx, id, cases.Executing, cases.ByOperator, "", "user:operator", "approved"); err != nil {
		t.Fatal(err)
	}
	f.result(`{"outcome": "executed", "detail": "c-101 email -> 'ana@example.org'", "ref": "contact c-101"}`)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Checking {
		t.Fatalf("after executed: %+v", c)
	}
	if e, ref := executed(t, f, id); e.Actor != "crm/apply-update" || ref != "contact c-101" {
		t.Fatalf("the move %+v and its ref %q", e, ref)
	}
	f.pass(t)
	if f.w.runs() != 1 {
		t.Fatalf("%d runs, want 1: a plan is carried out once", f.w.runs())
	}
	var pj planJSON
	if err := json.Unmarshal([]byte(f.w.got[0].Env["FLOTILLA_PLAN"]), &pj); err != nil || pj.Action != "crm.update_field" || pj.Params["to"] != "ana@example.org" {
		t.Fatalf("FLOTILLA_PLAN %q", f.w.got[0].Env["FLOTILLA_PLAN"])
	}
}

// escalate escalates; a run with no result is unknown (it may have pushed): escalated, never run again.
func TestExecuteEscalatesAndUnknown(t *testing.T) {
	f := setup(t)
	f.withExecutor("repo.process_change")
	id, _ := f.approvedRepo(t, "#x")
	f.result(`{"outcome": "escalate", "detail": "the base commit is gone"}`)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Escalated || !strings.Contains(lastEvent(t, f, id).Detail, "base commit is gone") {
		t.Fatalf("after escalate: %+v %+v", c, lastEvent(t, f, id))
	}

	for _, plan := range []string{"", `{"outcome": "plan"}`, `not json`} {
		g := setup(t)
		g.withExecutor("repo.process_change")
		b, _ := g.approvedRepo(t, "#u")
		g.result(plan)
		g.pass(t)
		c := g.cs(t, b)
		if c.Status != cases.Escalated || !strings.Contains(lastEvent(t, g, b).Detail, "unknown:") {
			t.Fatalf("result %q: %+v %+v", plan, c, lastEvent(t, g, b))
		}
		if g.w.runs() != 1 {
			t.Fatalf("result %q: %d runs", plan, g.w.runs())
		}
	}
}

// At most once: a plan whose execution started (flotillad stopped mid-run) is not run again; a repo
// action with no executing routine waits for a person; a host action never goes to an executing routine.
func TestExecuteAtMostOnceAndOnlyRepoActions(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	f.withExecutor("repo.process_change")
	id, pid := f.approvedRepo(t, "#s")
	if err := f.st.AddCaseEvent(ctx, id, executeStartedEvent, "bugfix/open-pr", "plan "+itoa(pid)+" abcdef012345"); err != nil {
		t.Fatal(err)
	}
	f.result(`{"outcome": "executed"}`)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Executing || f.w.runs() != 0 {
		t.Fatalf("an execution already started: %+v, %d runs", c, f.w.runs())
	}

	g := setup(t)
	b, _ := g.approvedRepo(t, "#m")
	g.result(`{"outcome": "executed"}`)
	g.pass(t)
	if c := g.cs(t, b); c.Status != cases.Executing || g.w.runs() != 0 {
		t.Fatalf("no executing routine: %+v, %d runs", c, g.w.runs())
	}

	h := setup(t)
	h.withExecutor("backup.rerun")
	x, _ := h.executing(t, "#h")
	h.result(`{"outcome": "executed"}`)
	h.pass(t)
	if c := h.cs(t, x); c.Status != cases.Executing || h.w.runs() != 0 {
		t.Fatalf("a host action: %+v, %d runs", c, h.w.runs())
	}
}

// A check that reports failed (a PR closed without merging) escalates the case.
func TestCheckFailedEscalates(t *testing.T) {
	f := setup(t)
	id := f.checking(t, "#f")
	f.result(`{"outcome": "failed", "detail": "the PR was closed without merging"}`)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Escalated {
		t.Fatalf("after failed: %+v", c)
	}
	if e := lastEvent(t, f, id); e.By != cases.ByCheck || !strings.Contains(e.Detail, "not verified: run") || !strings.Contains(e.Detail, "without merging") {
		t.Fatalf("event %+v", e)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Two approvals in one pass, or a paused executing routine: the plan that cannot run now waits with nothing
// recorded, and runs later; its one attempt is not spent (the advisor's review, 2026-11-15).
func TestExecuteWaitsWhenBusyOrPaused(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	f.withExecutor("repo.process_change")
	f.w.block, f.w.plan = make(chan struct{}), `{"outcome": "executed", "ref": "o/r#1"}`
	a, _ := f.approvedRepo(t, "#b1")
	b, _ := f.approvedRepo(t, "#b2")
	if err := f.d.Once(ctx); err != nil {
		t.Fatal(err)
	}
	for f.w.runs() == 0 {
		time.Sleep(time.Millisecond)
	}
	_ = f.d.Once(ctx)
	waiting := b
	if f.cs(t, a).Holder == "" {
		waiting = a
	}
	if started, _, _ := f.d.executeStarted(ctx, waiting, mustPlan(t, f, waiting)); started {
		t.Fatal("the second approval spent its attempt while the routine was busy")
	}
	close(f.w.block)
	f.d.Wait()
	f.w.block = nil
	f.pass(t)
	// both ran once and were executed (repo.process_change declares no check, so a case that reached
	// checking is escalated by the next pass: that is the check step, not the execution)
	for _, id := range []string{a, b} {
		if _, ref := executed(t, f, id); ref != "o/r#1" {
			t.Fatalf("approval %s was not executed: %+v", id, lastEvent(t, f, id))
		}
	}
	if f.w.runs() != 2 {
		t.Fatalf("%d runs, want 2", f.w.runs())
	}

	g := setup(t)
	g.withExecutor("repo.process_change")
	g.result(`{"outcome": "executed"}`)
	x, _ := g.approvedRepo(t, "#p")
	if err := g.st.SetControl(ctx, "bugfix/open-pr", sched.KeyPaused, "1", "user:ada", "test"); err != nil {
		t.Fatal(err)
	}
	g.pass(t)
	if c := g.cs(t, x); c.Status != cases.Executing || g.w.runs() != 0 || lastEvent(t, g, x).Kind == executeStartedEvent {
		t.Fatalf("paused: %+v, %d runs, %+v", c, g.w.runs(), lastEvent(t, g, x))
	}
	if err := g.st.SetControl(ctx, "bugfix/open-pr", sched.KeyPaused, "0", "user:ada", "test"); err != nil {
		t.Fatal(err)
	}
	g.pass(t)
	if c := g.cs(t, x); c.Status != cases.Checking {
		t.Fatalf("resumed: %+v", c)
	}
}

// A run the scheduler skips after the guard was written (paused between the check and the run) gives the
// attempt back: execute.not_started, the case still executing, and it runs on a later pass.
func TestExecuteSkippedRunGivesTheAttemptBack(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	j := f.withExecutor("repo.process_change")
	f.result(`{"outcome": "executed"}`)
	id, pid := f.approvedRepo(t, "#s")
	if err := f.st.SetControl(ctx, "bugfix/open-pr", sched.KeyPaused, "1", "user:ada", "test"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AddCaseEvent(ctx, id, executeStartedEvent, j.ID(), "plan "+itoa(pid)+" abcdef012345"); err != nil {
		t.Fatal(err)
	}
	plans, _ := f.st.Plans(ctx, id)
	if err := f.d.execute(ctx, f.cs(t, id), j, plans[len(plans)-1]); err != nil {
		t.Fatal(err)
	}
	if c := f.cs(t, id); c.Status != cases.Executing || lastEvent(t, f, id).Kind != executeNotStartedEvent {
		t.Fatalf("a skipped run: %+v, %+v", c, lastEvent(t, f, id))
	}
	if started, _, _ := f.d.executeStarted(ctx, id, pid); started {
		t.Fatal("a skipped run still counts as started")
	}
	_ = f.st.SetControl(ctx, "bugfix/open-pr", sched.KeyPaused, "0", "user:ada", "test")
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Checking || f.w.runs() != 1 {
		t.Fatalf("after: %+v, %d runs", c, f.w.runs())
	}
}

// An approved repo action whose executing routine has not started (its machine off) has no deadline; once
// started, the deadline counts from the start, not from the approval.
func TestRepoActionDeadlineCountsFromTheStart(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	j := f.withExecutor("repo.process_change")
	id, pid := f.approvedRepo(t, "#d")
	_ = f.st.SetControl(ctx, "bugfix/open-pr", sched.KeyPaused, "1", "user:ada", "test")
	f.now = f.now.Add(400 * time.Hour) // repo.process_change's timeout is 168h; + 15m grace
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Executing {
		t.Fatalf("not started, long after the approval: %+v", c)
	}
	if err := f.st.AddCaseEvent(ctx, id, executeStartedEvent, j.ID(), "plan "+itoa(pid)+" abcdef012345"); err != nil {
		t.Fatal(err)
	}
	ev := lastEvent(t, f, id)
	at, _ := time.Parse(time.RFC3339Nano, ev.At)
	f.now = at.Add(168*time.Hour + 10*time.Minute)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Executing {
		t.Fatalf("inside the deadline from the start: %+v", c)
	}
	f.now = at.Add(168*time.Hour + 16*time.Minute)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Escalated {
		t.Fatalf("past the deadline from the start: %+v", c)
	}
}

func mustPlan(t *testing.T, f *fixture, id string) int64 {
	t.Helper()
	plans, err := f.st.Plans(context.Background(), id)
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans of %s: %v", id, err)
	}
	return plans[len(plans)-1].ID
}

// The scheduler refusing the run because the routine is already running (a race the busy check above
// cannot close) gives the attempt back too.
func TestExecuteRefusedRunGivesTheAttemptBack(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	j := f.withExecutor("repo.process_change")
	f.w.block, f.w.plan = make(chan struct{}), `{"outcome": "executed"}`
	id, pid := f.approvedRepo(t, "#r")
	go func() {
		_, _, _ = f.d.API.Sched.RunWith(ctx, j.ID(), "manual", "test", map[string]string{"FLOTILLA_CASE": "{}"})
	}()
	for f.w.runs() == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := f.st.AddCaseEvent(ctx, id, executeStartedEvent, j.ID(), "plan "+itoa(pid)+" abcdef012345"); err != nil {
		t.Fatal(err)
	}
	plans, _ := f.st.Plans(ctx, id)
	if err := f.d.execute(ctx, f.cs(t, id), j, plans[len(plans)-1]); err != nil {
		t.Fatal(err)
	}
	close(f.w.block)
	if c := f.cs(t, id); c.Status != cases.Executing || lastEvent(t, f, id).Kind != executeNotStartedEvent {
		t.Fatalf("a refused run: %+v, %+v", c, lastEvent(t, f, id))
	}
}

// executed is a case's move to checking and the ref its execution reported (an action.ref event of its own, after the move).
func executed(t *testing.T, f *fixture, id string) (move store.CaseEvent, ref string) {
	t.Helper()
	ev, _ := f.st.CaseEvents(context.Background(), id)
	for i, e := range ev {
		if e.Kind == "case."+cases.Checking {
			move = e
			if i+1 < len(ev) && ev[i+1].Kind == "action.ref" {
				ref = ev[i+1].Detail
			}
		}
	}
	return move, ref
}
