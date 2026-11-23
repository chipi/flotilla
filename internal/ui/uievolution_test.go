package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/api"
	"github.com/chipi/flotilla/internal/hook"
	"github.com/chipi/flotilla/internal/sched"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// A meeting case's strip: its first stage named by its kind's first routine, while its plan names a next stage that one as planned,
// the stage it was handed to as where it is now; closed, every stage is done.
func TestCaseStages(t *testing.T) {
	notes := &spec.Routine{Fleet: "meetings", Routine: "notes", Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "meeting"}}}
	sch := &sched.Scheduler{}
	sch.SetRoutines([]*spec.Routine{notes})
	u := &Server{API: &api.Server{Sched: sch}}
	str := func(ss []stageStep) string {
		var out []string
		for _, s := range ss {
			out = append(out, s.Name+":"+s.State)
		}
		return strings.Join(out, " ")
	}
	planned := []store.CaseEvent{{Kind: "case.detected"}, {Kind: "plan.then", To: "decisions"}}
	if got := str(u.caseStages(store.Case{Kind: "meeting", Status: "awaiting_approval"}, planned)); got != "notes:now decisions:next" {
		t.Fatalf("planned: %q", got)
	}
	handed := append(planned, store.CaseEvent{Kind: "case.staged", From: "first", To: "decisions"})
	if got := str(u.caseStages(store.Case{Kind: "meeting", Status: "awaiting_approval"}, handed)); got != "notes:done decisions:now" {
		t.Fatalf("handed on: %q", got)
	}
	if got := str(u.caseStages(store.Case{Kind: "meeting", Status: "closed"}, handed)); got != "notes:done decisions:done" {
		t.Fatalf("closed: %q", got)
	}
	if got := u.caseStages(store.Case{Kind: "failed_backup"}, []store.CaseEvent{{Kind: "case.detected"}}); got != nil {
		t.Fatalf("a case with no stages has no strip: %+v", got)
	}
}

// The Closed card says how the week's cases closed, most first.
func TestClosedCard(t *testing.T) {
	c := closedCard(make([]float64, 14), map[string]int{"verified": 1, "resolved_itself": 3})
	if c.Label != "Closed" || c.Delta != "7 days: resolved itself 3 · verified 1" {
		t.Fatalf("%+v", c)
	}
}

// A hook that only refused in the last hour is wrong right now; one that also accepted is not.
func TestAHookThatOnlyRefusesIsWrongRightNow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.ui.API.Hooks = map[string]*hook.Hook{"meeting": {Name: "meeting", Project: "meetings", Kind: "meeting"}, "smoke": {Name: "smoke"}}
	for _, e := range []struct{ hook, kind string }{{"meeting", "hook.refused"}, {"smoke", "hook.refused"}, {"smoke", "hook.accepted"}} {
		if err := f.st.Event(ctx, "hook:"+e.hook, "hook", e.kind, "hook:"+e.hook, "bad signature"); err != nil {
			t.Fatal(err)
		}
	}
	w, err := f.ui.wrongNow(ctx, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range w.Items {
		if strings.HasPrefix(x.What, "hook ") {
			got = append(got, x.What+" -> "+x.Href)
		}
	}
	if len(got) != 1 || got[0] != "hook meeting refused 1 call in the last hour -> /fleets/meetings" {
		t.Fatalf("%q", got)
	}
}
