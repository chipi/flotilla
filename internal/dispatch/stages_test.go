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
	c := &spec.Routine{Fleet: "ops", Routine: "clarify", Description: "d", Enabled: true, Worker: "ops", Autonomy: "shadow",
		Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "failed_backup", Stage: "clarify"}}, Run: spec.Run{Command: "true"}, Limit: time.Minute}
	f.d.API.Sched.Routines = append(f.d.API.Sched.Routines, c)
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
	f.decide(`{"outcome": "hand_over", "stage": "", "detail": "answered: mini"}`)
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
			f.d.API.Sched.Routines[0].Trigger.Case.Limit = &spec.CaseLimit{USD: 1, Minutes: 90}
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
