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

// withClarify adds a second-stage routine for failed_backup cases (RFC-0013 §2), on the same fake worker.
func withClarify(f *fixture) *spec.Routine {
	c := &spec.Routine{Fleet: "ops", Routine: "clarify", Description: "d", Enabled: true, Worker: "ops", Autonomy: "record",
		Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "failed_backup", Stage: "clarify"}}, Run: spec.Run{Command: "true"}, Limit: time.Minute}
	f.d.API.Sched.SetRoutines(append(f.d.API.Sched.Routines(), c))
	return c
}

func (f *fixture) decide(plan string) {
	f.w.mu.Lock()
	f.w.plan = plan
	f.w.mu.Unlock()
}

// Fix -> clarify -> fix: the case stays handed over, each hand-over is a case.staged event with the data,
// the next routine is the stage's and reads the data; a third hand-over escalates (no loops).
func TestHandOverThroughStages(t *testing.T) {
	f := setup(t)
	withClarify(f)
	id := f.handOver(t, "")

	f.decide(`{"outcome": "hand_over", "stage": "clarify", "detail": "needs info", "data": {"questions": ["which host?"]}}`)
	f.pass(t)
	c := f.cs(t, id)
	if c.Status != cases.HandedOver || c.Stage != "clarify" || c.StageChanges != 1 {
		t.Fatalf("after the first hand-over: %s stage %q changes %d", c.Status, c.Stage, c.StageChanges)
	}
	if ev := lastEvent(t, f, id); ev.Kind != "case.staged" || ev.To != "clarify" || !strings.Contains(ev.Data, "which host?") {
		t.Fatalf("event %+v", ev)
	}

	f.now = f.now.Add(time.Hour) // past the backoff
	f.decide(`{"outcome": "hand_over", "stage": "", "detail": "answered: host-01"}`)
	f.pass(t)
	got := f.w.got[len(f.w.got)-1]
	var cj caseJSON
	if err := json.Unmarshal([]byte(got.Env["FLOTILLA_CASE"]), &cj); err != nil {
		t.Fatal(err)
	}
	if got.Routine != "ops/clarify" || cj.Stage != "clarify" || !strings.Contains(string(cj.StageData), "which host?") {
		t.Fatalf("the clarify run got routine %s, case %+v", got.Routine, cj)
	}
	if c := f.cs(t, id); c.Stage != "" || c.StageChanges != 2 {
		t.Fatalf("back at the first stage: stage %q changes %d", c.Stage, c.StageChanges)
	}

	f.now = f.now.Add(time.Hour)
	f.decide(`{"outcome": "hand_over", "stage": "clarify", "detail": "still unclear"}`)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Escalated || c.Stage != "" {
		t.Fatalf("a third hand-over escalates: %s stage %q", c.Status, c.Stage)
	}
}

// A hand-over to a stage no enabled routine acts on, or with data that is not an object, moves nothing.
func TestHandOverRefused(t *testing.T) {
	for name, plan := range map[string]string{
		"no routine for the stage": `{"outcome": "hand_over", "stage": "nobody"}`,
		"a malformed stage":        `{"outcome": "hand_over", "stage": "Clarify; rm"}`,
		"data not an object":       `{"outcome": "hand_over", "stage": "clarify", "data": [1, 2]}`,
		"the same stage":           `{"outcome": "hand_over", "stage": ""}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			withClarify(f)
			id := f.handOver(t, "")
			f.decide(plan)
			f.pass(t)
			if c := f.cs(t, id); c.Status != cases.HandedOver || c.Stage != "" || c.StageChanges != 0 {
				t.Fatalf("%s stage %q changes %d", c.Status, c.Stage, c.StageChanges)
			}
		})
	}
}

// A case's runs share one limit: their spend and time add up on the case, FLOTILLA_CASE says what is left,
// and a case past either is escalated instead of run again (RFC-0013 §2).
// A case's run gets no more time than its case has left (the Fable advisor's review, 2026-11-16: a 3 h run
// timeout let one run spend twice the case's 90 minutes); with no limit, the spec's timeout as before.
func TestRunTimeoutIsTheCasesTimeLeft(t *testing.T) {
	f := setup(t)
	f.d.API.Sched.Routines()[0].Limit = 3 * time.Hour
	f.d.API.Sched.Routines()[0].Trigger.Case.Limit = &spec.CaseLimit{USD: 1, Minutes: 90}
	id := f.handOver(t, "")
	if err := f.st.AddCaseSpend(context.Background(), id, 0.1, 60*60); err != nil {
		t.Fatal(err)
	}
	f.decide(`{"outcome": "nothing"}`)
	f.pass(t)
	if got := f.w.got[len(f.w.got)-1].TimeoutS; got != 30*60 {
		t.Fatalf("the run's timeout is %v s, want the case's 1800 s left", got)
	}
	g := setup(t)
	g.d.API.Sched.Routines()[0].Limit = 3 * time.Hour
	g.handOver(t, "")
	g.decide(`{"outcome": "nothing"}`)
	g.pass(t)
	if got := g.w.got[len(g.w.got)-1].TimeoutS; got != 3*60*60 {
		t.Fatalf("without a limit the run's timeout is %v s, want the spec's 10800 s", got)
	}
}

func TestCaseLimitIsShared(t *testing.T) {
	for name, c := range map[string]struct {
		spend, secs float64
		why         string
	}{
		"money": {0.6, 60, "spent $1.20 of its $1.00"},
		"time":  {0.01, 50 * 60, "taken 100 of its 90 minutes"},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.d.API.Sched.Routines()[0].Trigger.Case.Limit = &spec.CaseLimit{USD: 1, Minutes: 90}
			id := f.handOver(t, "")
			f.w.mu.Lock()
			f.w.spend, f.w.secs = c.spend, c.secs
			f.w.mu.Unlock()
			f.decide(`{"outcome": "nothing"}`) // no decision: the case is tried again after the backoff
			f.pass(t)
			f.now = f.now.Add(time.Hour)
			f.pass(t)
			runs := len(f.w.got)
			var cj caseJSON
			_ = json.Unmarshal([]byte(f.w.got[runs-1].Env["FLOTILLA_CASE"]), &cj)
			if cj.Left == nil || cj.Left.USD != max(1-c.spend, 0) || cj.Left.Seconds != max(90*60-c.secs, 0) {
				t.Fatalf("the second run was told %+v", cj.Left)
			}
			f.now = f.now.Add(time.Hour)
			f.pass(t)
			cs := f.cs(t, id)
			if cs.Status != cases.Escalated || len(f.w.got) != runs || !strings.Contains(lastEvent(t, f, id).Detail, c.why) {
				t.Fatalf("%s after %d runs (%d before), spent $%.2f %.0fs: %q", cs.Status, len(f.w.got), runs, cs.SpentUSD, cs.SpentS, lastEvent(t, f, id).Detail)
			}
		})
	}
}

// The limit holds for the bug, not the case: after a person closes an escalated case, the next detection's
// new case (the same canonical key) starts with what the earlier one spent, and is escalated, not run.
func TestLimitIsPerBugAcrossCases(t *testing.T) {
	f := setup(t)
	f.d.API.Sched.Routines()[0].Trigger.Case.Limit = &spec.CaseLimit{USD: 1, Minutes: 90}
	ctx := context.Background()
	id := f.handOver(t, "")
	if err := f.st.AddCaseSpend(ctx, id, 1.2, 60); err != nil {
		t.Fatal(err)
	}
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Escalated {
		t.Fatalf("first case %s", c.Status)
	}
	if err := f.st.MoveCase(ctx, id, cases.Closed, cases.ByOperator, cases.OperatorResolved, "user:op", "done here"); err != nil {
		t.Fatal(err)
	}
	id2 := f.handOver(t, "") // the same canonical key: the issue is still open
	if id2 == id {
		t.Fatal("the closed case was reused")
	}
	runs := f.w.runs()
	f.pass(t)
	if c := f.cs(t, id2); c.Status != cases.Escalated || f.w.runs() != runs {
		t.Fatalf("the reopened bug ran again with a fresh limit: %s, %d runs (was %d)", c.Status, f.w.runs(), runs)
	}
}

// A plan may name what follows it (RFC-0013 §2, amended 2026-11-22): its `then` is kept with the run that
// planned it, and policy decides the plan as any other.
func TestPlanThenIsRecorded(t *testing.T) {
	f := setup(t)
	withClarify(f)
	id := f.handOver(t, "")
	f.decide(`{"outcome": "plan", "action": "backup.rerun", "params": {"host": "host-01"},
		"then": {"stage": "clarify", "data": {"decisions": ["rerun nightly"]}}}`)
	f.pass(t)
	plans, err := f.st.Plans(context.Background(), id)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans %v %v", plans, err)
	}
	stage, data, ok, err := f.st.PlanThen(context.Background(), id, plans[0].RunID)
	if err != nil || !ok || stage != "clarify" || !strings.Contains(data, "rerun nightly") {
		t.Fatalf("then: stage %q data %q ok %v err %v", stage, data, ok, err)
	}
	if _, _, ok, _ := f.st.PlanThen(context.Background(), id, "another-run"); ok {
		t.Fatal("a then is another run's")
	}
}

// A then to a stage no enabled routine acts on, or with data that is not an object, is no decision: the
// plan is not made.
func TestPlanThenRefused(t *testing.T) {
	for name, then := range map[string]string{
		"no routine for the stage": `{"stage": "nobody"}`,
		"a malformed stage":        `{"stage": "Clarify; rm"}`,
		"the first stage":          `{"stage": ""}`,
		"data not an object":       `{"stage": "clarify", "data": [1, 2]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			withClarify(f)
			id := f.handOver(t, "")
			f.decide(`{"outcome": "plan", "action": "backup.rerun", "params": {"host": "host-01"}, "then": ` + then + `}`)
			f.pass(t)
			if c := f.cs(t, id); c.Status != cases.HandedOver {
				t.Fatalf("%s", c.Status)
			}
			if e := lastEvent(t, f, id); e.Kind != "run.no_decision" || !strings.Contains(e.Detail, "then") {
				t.Fatalf("event %+v", e)
			}
		})
	}
}

// A verified check of a plan with a then hands the case on to that stage instead of closing it: the
// stage's routine acts next and reads the then's data; the target stays the case's. The verification is on
// the timeline.
func TestVerifiedThenHandsOn(t *testing.T) {
	f := setup(t)
	withClarify(f)
	ctx := context.Background()
	id := f.checking(t, "#then")
	if err := f.st.SetPlanThen(ctx, id, "r1", "ops/rerun", "clarify", `{"decisions":["rerun nightly"]}`); err != nil {
		t.Fatal(err)
	}
	f.result(`{"outcome": "verified", "detail": "backup at 10:42"}`)
	f.pass(t)
	c := f.cs(t, id)
	if c.Status != cases.HandedOver || c.Stage != "clarify" || c.StageChanges != 1 || c.Holder != "" {
		t.Fatalf("case %s stage %q changes %d holder %q", c.Status, c.Stage, c.StageChanges, c.Holder)
	}
	evs, _ := f.st.CaseEvents(ctx, id)
	var moved, staged bool
	for _, e := range evs {
		if e.Kind == "case.handed_over" && e.From == cases.Checking && e.By == cases.ByCheck && strings.Contains(e.Detail, "verified") && strings.Contains(e.Detail, "backup at 10:42") {
			moved = true
		}
		if e.Kind == "case.staged" && e.To == "clarify" && strings.Contains(e.Data, "rerun nightly") {
			staged = true
		}
	}
	if !moved || !staged {
		t.Fatalf("handed on %v, staged %v: %+v", moved, staged, evs)
	}
	if h := f.holder(t); h != id {
		t.Fatalf("the target's lock is %q, want the case's", h)
	}

	f.now = f.now.Add(time.Hour)
	f.decide(`{"outcome": "resolved_itself", "detail": "nothing to add"}`)
	f.pass(t)
	got := f.w.got[len(f.w.got)-1]
	var cj caseJSON
	if err := json.Unmarshal([]byte(got.Env["FLOTILLA_CASE"]), &cj); err != nil {
		t.Fatal(err)
	}
	if got.Routine != "ops/clarify" || cj.Stage != "clarify" || !strings.Contains(string(cj.StageData), "rerun nightly") {
		t.Fatalf("the next stage's run: routine %s case %+v", got.Routine, cj)
	}
}

// A then to a stage whose routine is gone by the time the check verifies escalates the verified case: what
// was to follow did not happen, and a person decides (the stage limit: the store's TestHandOnVerified).
func TestVerifiedThenCannotHandOn(t *testing.T) {
	f := setup(t)
	withClarify(f)
	id := f.checking(t, "#gone")
	if err := f.st.SetPlanThen(context.Background(), id, "r1", "ops/rerun", "clarify", `{}`); err != nil {
		t.Fatal(err)
	}
	f.d.API.Sched.SetRoutines(f.d.API.Sched.Routines()[:3])
	f.result(`{"outcome": "verified", "detail": "done"}`)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Escalated {
		t.Fatalf("%s stage %q", c.Status, c.Stage)
	}
	if e := lastEvent(t, f, id); !strings.Contains(e.Detail, "verified") || !strings.Contains(e.Detail, "clarify") {
		t.Fatalf("the escalation says %q", e.Detail)
	}
}

// not_reproducible is its own close (RFC-0013, decision 2): never existed on main, not "went away".
func TestNotReproducibleCloses(t *testing.T) {
	f := setup(t)
	id := f.handOver(t, "")
	f.decide(`{"outcome": "not_reproducible", "detail": "the call is not on main"}`)
	f.pass(t)
	if c := f.cs(t, id); c.Status != cases.Closed || c.Resolution != cases.NotReproducible {
		t.Fatalf("%s %s", c.Status, c.Resolution)
	}
	// a monitor that stops finding its case closes it only as resolved itself
	id2 := f.handOver(t, "b")
	if err := f.st.MoveCase(context.Background(), id2, cases.Closed, cases.ByDetector, cases.NotReproducible, "ops/backup-monitor", ""); err == nil {
		t.Fatal("a detector closed a case as not_reproducible")
	}
	_ = store.ErrBadTransition
}
