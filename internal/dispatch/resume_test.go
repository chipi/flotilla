package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

func withResume(f *fixture, on ...string) {
	f.d.API.Sched.Routines[0].Resume = &spec.Resume{On: on, Max: 1}
}

func (f *fixture) resumeOf(t *testing.T, i int) *sched.Resumption {
	t.Helper()
	raw := f.w.got[i].Env[sched.EnvResume]
	if raw == "" {
		return nil
	}
	var r sched.Resumption
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func (f *fixture) events(t *testing.T, id, kind string) []store.CaseEvent {
	t.Helper()
	ev, err := f.st.CaseEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.CaseEvent
	for _, e := range ev {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// A run that ended in error is continued once: the next run is told which run and its status, the case's
// timeline says so, and a second failure starts fresh (max 1).
func TestResumeAfterErrorOnce(t *testing.T) {
	f := setup(t)
	withResume(f, "interrupted", "error")
	f.w.status = "error"
	id := f.handOver(t, "")
	f.pass(t)
	if f.resumeOf(t, 0) != nil {
		t.Fatal("the first run is fresh")
	}
	first := f.events(t, id, sched.EventRunStarted)
	if len(first) != 1 {
		t.Fatalf("run.started events: %+v", first)
	}
	firstRun := strings.TrimPrefix(first[0].Detail, "run ")

	f.now = f.now.Add(DefaultBackoff + time.Second)
	f.pass(t)
	rs := f.resumeOf(t, 1)
	if rs == nil || rs.RunID != firstRun || rs.Status != "error" {
		t.Fatalf("the second run's resume: %+v, want run %s error", rs, firstRun)
	}
	ev := f.events(t, id, sched.EventRunResumed)
	if len(ev) != 1 || !strings.Contains(ev[0].Detail, "resumes run "+firstRun) || ev[0].Actor != "ops/rerun" {
		t.Fatalf("run.resumed: %+v", ev)
	}

	f.now = f.now.Add(DefaultBackoff + time.Second)
	f.pass(t)
	if f.resumeOf(t, 2) != nil || len(f.events(t, id, sched.EventRunResumed)) != 1 {
		t.Fatalf("a third run after max 1 resumes is fresh: %+v", f.w.got[2].Env)
	}
}

// An interrupted run (flotillad restarted while it ran) is resumed: it has no run.no_decision, only its
// run.started, and the status the store gave it.
func TestResumeAfterInterrupted(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	withResume(f, "interrupted")
	id := f.handOver(t, "")
	if err := f.st.StartRun(ctx, store.Run{ID: "run-gone", Routine: "ops/rerun", Status: store.StatusRunning, StartedAt: f.now.UTC().Format(store.TimeFormat)}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AddCaseEvent(ctx, id, sched.EventRunStarted, "ops/rerun", "run run-gone"); err != nil {
		t.Fatal(err)
	}
	if n, err := f.st.MarkInterrupted(ctx); err != nil || n != 1 {
		t.Fatalf("interrupted %d, %v", n, err)
	}
	f.pass(t)
	if rs := f.resumeOf(t, 0); rs == nil || rs.RunID != "run-gone" || rs.Status != store.StatusInterrupted {
		t.Fatalf("resume %+v", rs)
	}
}

// A status the routine does not list, a run that ended without a decision, and a routine with no resume
// declared are all fresh.
func TestResumeOnlyForListedStatuses(t *testing.T) {
	for name, c := range map[string]struct {
		on     []string
		status string
		plan   string
	}{
		"error not listed":    {[]string{"interrupted"}, "error", ""},
		"no resume declared":  {nil, "error", ""},
		"ok without decision": {[]string{"interrupted", "error"}, "ok", ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			if c.on != nil {
				withResume(f, c.on...)
			}
			f.w.status, f.w.plan = c.status, c.plan
			id := f.handOver(t, "")
			f.pass(t)
			f.now = f.now.Add(DefaultBackoff + time.Second)
			f.pass(t)
			if len(f.w.got) != 2 || f.resumeOf(t, 1) != nil || len(f.events(t, id, sched.EventRunResumed)) != 0 {
				t.Fatalf("%d runs, resume %+v", len(f.w.got), f.w.got[len(f.w.got)-1].Env)
			}
		})
	}
}

// An escalation and a reassign end the chain: the run after them is fresh even though the run before
// ended in error.
func TestResumeNotAcrossEscalationAndReassign(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	withResume(f, "interrupted", "error")
	op := api.Caller{Name: "operator", Role: api.RoleOperator}
	f.w.status = "error"
	id := f.handOver(t, "")
	f.pass(t)
	if err := f.st.MoveCase(ctx, id, cases.Escalated, cases.ByAct, "", "flotillad", "gave up"); err != nil {
		t.Fatal(err)
	}
	if err := f.d.API.ReassignCase(ctx, op, "test", id, "try again"); err != nil {
		t.Fatal(err)
	}
	f.pass(t)
	if len(f.w.got) != 2 || f.resumeOf(t, 1) != nil {
		t.Fatalf("%d runs; the run after a reassign: %+v", len(f.w.got), f.w.got[len(f.w.got)-1].Env)
	}
}

// The case's limit still bounds a case with a resumable run: one that has spent it is escalated, not resumed.
func TestResumeStaysInsideTheCaseLimit(t *testing.T) {
	f := setup(t)
	withResume(f, "interrupted", "error")
	f.d.API.Sched.Routines[0].Trigger.Case.Limit = &spec.CaseLimit{USD: 1, Minutes: 90}
	f.w.status, f.w.spend, f.w.secs = "error", 1.2, 60
	id := f.handOver(t, "")
	f.pass(t)
	f.now = f.now.Add(DefaultBackoff + time.Second)
	f.pass(t)
	if c := f.cs(t, id); len(f.w.got) != 1 || c.Status != cases.Escalated {
		t.Fatalf("%d runs, case %s: a case past its limit is escalated, not resumed", len(f.w.got), c.Status)
	}
	if len(f.events(t, id, sched.EventRunResumed)) != 0 {
		t.Fatal("a resume was recorded for a run that did not start")
	}
}

// A resumed run's time is the case's time left, as for any run.
func TestResumedRunTimeoutIsTheCasesTimeLeft(t *testing.T) {
	f := setup(t)
	withResume(f, "error")
	f.d.API.Sched.Routines[0].Limit = 3 * time.Hour
	f.d.API.Sched.Routines[0].Trigger.Case.Limit = &spec.CaseLimit{USD: 5, Minutes: 90}
	f.w.status, f.w.secs = "error", 60*60
	f.handOver(t, "")
	f.pass(t)
	f.now = f.now.Add(DefaultBackoff + time.Second)
	f.pass(t)
	if f.resumeOf(t, 1) == nil || f.w.got[1].TimeoutS != 30*60 {
		t.Fatalf("resume %+v, timeout %v s, want the 1800 s left", f.resumeOf(t, 1), f.w.got[1].TimeoutS)
	}
}
