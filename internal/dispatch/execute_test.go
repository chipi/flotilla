package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// withExecutor adds an executing routine for action (RFC-0010 §3) to the fixture's scheduler.
func (f *fixture) withExecutor(action string) *spec.Routine {
	j := &spec.Routine{Fleet: "bugfix", Routine: "open-pr", Description: "d", Enabled: true, Worker: "ops", Autonomy: "propose",
		Trigger: spec.Trigger{Execute: &spec.ExecuteTrigger{Action: action}}, Run: spec.Run{Command: "true"}, Limit: time.Minute}
	f.d.API.Sched.Routines = append(f.d.API.Sched.Routines, j)
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
	if err := f.st.MoveCase(ctx, id, cases.Executing, cases.ByOperator, "", "user:marko", "approved"); err != nil {
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
	if e := lastEvent(t, f, id); e.By != cases.ByAct || e.Actor != "bugfix/open-pr" || !strings.Contains(e.Detail, "(chipi/flotilla#7)") {
		t.Fatalf("the move: %+v", e)
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
	h.withExecutor("backup.rerun_observability")
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
