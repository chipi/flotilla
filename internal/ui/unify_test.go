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

// One UI (docs/wip/ui-unify-2026-11-23.md): a fleet's "needs you" opens its cases, a pivot Cases understands.
func TestCasesPivotOnFleet(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, c := range []struct{ opener, target string }{{"jobs/backup-monitor", "host:a"}, {"seo/site-check", "site:b"}} {
		k := store.Key{Kind: "target", Value: c.target}
		if _, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: c.target, Keys: []store.Key{k}, Canonical: k}, c.opener, ""); err != nil {
			t.Fatal(err)
		}
	}
	ck := f.signin(f.admin)
	_, page := f.do("GET", "/cases?p=fleet%3Ajobs", ck, nil, nil)
	if !strings.Contains(page, "host:a") || strings.Contains(page, "site:b") {
		t.Error("the fleet pivot does not keep only that fleet's cases")
	}
	if !strings.Contains(page, `fleet = <strong>jobs</strong>`) {
		t.Error("the fleet pivot is not shown as a removable filter")
	}
}

// A column that is empty on every row is left out: Hosts has no Roles column, and no load column while no host reports one.
func TestHostsLeaveOutEmptyColumns(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.st.AddEnrolment(ctx, "backup-01", "tok-backup-01", "user:ada", "test"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Enrol(ctx, "backup-01", "tok-backup-01", func() (string, time.Time, error) {
		return "cert-sha-backup-01", time.Now().Add(27 * 24 * time.Hour), nil
	}); err != nil {
		t.Fatal(err)
	}
	ck := f.signin(f.admin)
	_, page := f.do("GET", "/hosts", ck, nil, nil)
	for _, gone := range []string{">Roles<", "CPU · mem · disk"} {
		if strings.Contains(page, gone) {
			t.Errorf("Hosts shows the empty column %q", gone)
		}
	}
}

// Instruments are one line unless a row is off its usual or past a limit.
func TestInstrumentsOpenOnlyOnWorry(t *testing.T) {
	calm := instruments{Lead: "All within limits", Rows: []instRow{{Label: "turns"}}}
	if calm.Worry() {
		t.Error("a calm card says it worries")
	}
	calm.Rows = append(calm.Rows, instRow{Label: "spend", Level: "alert"})
	if !calm.Worry() {
		t.Error("a card with an alert row does not open")
	}
}

// A staged case's last event reads cleanly when its actor is unknown.
func TestEventShortWithoutActor(t *testing.T) {
	if got := eventShort(store.CaseEvent{Kind: "case.staged", To: "decisions"}); got != "handed on to decisions" {
		t.Errorf("%q", got)
	}
	if got := eventShort(store.CaseEvent{Kind: "case.staged", Actor: "meetings/notes", To: "decisions"}); got != "meetings/notes: handed on to decisions" {
		t.Errorf("%q", got)
	}
}

// A fleet's board draws the host agents only when an action on its case kinds runs on one: backup.rerun (a host action)
// for failed_backup, not plumbing.create_tasks (a routine's) for meeting.
func TestHostAgentsOnlyOnFleetsThatUseThem(t *testing.T) {
	rerun := &spec.Routine{Fleet: "jobs", Routine: "rerun-backup", Description: "d", Enabled: true, Worker: "w", Autonomy: "record",
		Run: spec.Run{Command: "true"}, Limit: time.Minute, Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "failed_backup"}}}
	notes := &spec.Routine{Fleet: "meetings", Routine: "notes", Description: "d", Enabled: true, Worker: "w", Autonomy: "record",
		Run: spec.Run{Command: "true"}, Limit: time.Minute, Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "meeting"}}}
	f := setupWith(t, []*spec.Routine{rerun, notes})
	ck := f.signin(f.admin)
	if _, page := f.do("GET", "/fleets/jobs", ck, nil, nil); !strings.Contains(page, "host agents") {
		t.Error("jobs, whose backup.rerun runs on a host agent, does not draw the host agents")
	}
	if _, page := f.do("GET", "/fleets/meetings", ck, nil, nil); strings.Contains(page, "host agents") {
		t.Error("meetings, whose actions run by routine, draws the host agents")
	}
}

// Approvals opens the first waiting plan; the rest are one line each, folded, each opening to its full decision.
func TestApprovalsFoldAllButTheFirstPlan(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, h := range []string{"backup-01", "backup-02", "backup-03"} {
		k := store.Key{Kind: "target", Value: "failed_backup/host:" + h}
		id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:" + h, Keys: []store.Key{k}, Canonical: k, Title: "old backup on " + h}, "ops/backup-monitor", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
		_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun-backup", "")
		if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r-" + h, Action: "backup.rerun", Params: map[string]string{"host": h},
			PlanHash: "hash-" + h, Outcome: "propose", Autonomy: "propose", Reasons: []string{"ok"}}, cases.AwaitingApproval, "", "host:"+h, true); err != nil {
			t.Fatal(err)
		}
	}
	ck := f.signin(f.admin)
	_, page := f.do("GET", "/approvals", ck, nil, nil)
	if n := strings.Count(page, `<details class="queue__item"`); n != 2 {
		t.Errorf("%d folded plans, want 2 (all but the first)", n)
	}
	if !strings.Contains(page, "2 more plans wait") {
		t.Error("the fold does not say how many more wait")
	}
	if first := strings.Index(page, `<details class="queue__item"`); first < 0 || strings.Count(page[:first], "Approve this plan") != 1 {
		t.Error("the first plan is not open above the fold")
	}
}

// STOP is red only as an outline at rest, on the fleets list and a fleet's own page alike.
func TestStopIsOutlinedAtRest(t *testing.T) {
	f := setup(t)
	ck := f.signin(f.admin)
	_, page := f.do("GET", "/fleets", ck, nil, nil)
	if strings.Contains(page, `class="btn btn--stop"`) {
		t.Error("the fleets list fills STOP red at rest")
	}
}
