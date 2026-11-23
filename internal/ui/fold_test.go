package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// Nothing grows without bound on screen (the operator, 2026-11-23, on the live store): Now shows five cases that need
// you and folds the rest.
func TestNowFoldsNeedsAfterFive(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		k := store.Key{Kind: "target", Value: fmt.Sprintf("site:s%d", i)}
		id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "seo_live_regression", Target: k.Value, Keys: []store.Key{k}, Canonical: k, Title: fmt.Sprintf("problem %d", i)}, "seo/site-check", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "seo/site-check", "")
		_ = f.st.MoveCase(ctx, id, cases.Escalated, cases.ByAct, "", "seo/site-check", "")
	}
	ck := f.signin(f.admin)
	_, page := f.do("GET", "/", ck, nil, nil)
	fold := strings.Index(page, `<details class="fold">`)
	if fold < 0 || !strings.Contains(page[fold:], "Show all 7") {
		t.Fatal("Now does not fold the cases that need you after five")
	}
	if n := strings.Count(page[:fold], `class="casecard"`); n != 5 {
		t.Errorf("%d cases shown above the fold, want 5", n)
	}
}

// A case's runs that called no model and read nothing are one folded line, not a row each (138 on one live case).
func TestCaseFoldsCodeOnlyRuns(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01"}
	id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k, Title: "old backup"}, "ops/backup-monitor", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
	_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", "ops/rerun-backup", "")
	if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: "r1", Action: "backup.rerun", Params: map[string]string{"host": "backup-01"},
		PlanHash: "h1", Outcome: "propose", Autonomy: "propose", Reasons: []string{"ok"}}, cases.AwaitingApproval, "", "host:backup-01", true); err != nil {
		t.Fatal(err)
	}
	ck := f.signin(f.admin)
	_, page := f.do("GET", "/cases/"+id, ck, nil, nil)
	if !strings.Contains(page, "1 run code only: no model call, nothing read") {
		t.Fatal("a code-only run is not folded into one line")
	}
	if strings.Contains(page, "code only: no model call, so nothing was read") {
		t.Error("a code-only run still has a row of its own")
	}
}

// Runs leaves out a column that is empty on every row of the page: no case, no model call, no cost.
func TestRunsLeaveOutEmptyColumns(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_ = f.st.StartRun(ctx, store.Run{ID: "heartbeat-1", Routine: "triage/cycle", SpecHash: "h", Trigger: "schedule", Actor: "flotillad",
		Autonomy: "record", StartedAt: time.Now().UTC().Add(-time.Hour).Format(store.TimeFormat), Status: "running"})
	_ = f.st.FinishRun(ctx, "heartbeat-1", store.StatusOK, nil, 0, 1, "")
	ck := f.signin(f.admin)
	_, page := f.do("GET", "/runs", ck, nil, nil)
	for _, gone := range []string{`data-label="Cases"`, `data-label="Model"`, `data-label="Cost"`} {
		if strings.Contains(page, gone) {
			t.Errorf("Runs shows the empty column %s", gone)
		}
	}
	if !strings.Contains(page, "heartbeat-1") {
		t.Fatal("the run is not listed")
	}
}

// The case timer's escalation reads in words on Now, not as a Go duration and a timestamp.
func TestStuckCaseInWords(t *testing.T) {
	for in, want := range map[string]string{"24h0m0s": "24h", "1h30m0s": "1h30m", "30m0s": "30m", "30s": "30s", "90m0s": "1h30m", "x": "x"} {
		if got := durWords(in); got != want {
			t.Errorf("durWords(%q) = %q, want %q", in, got, want)
		}
	}
	c := caseRow{Case: store.Case{Status: cases.Escalated}}
	c.Last.Detail = "handed over since 2026-11-15T20:05:30Z, longer than 24h0m0s"
	if got := whyWaiting(c); got != "It stayed handed over longer than 24h, so it needs a person. Reassign it, or close it with a reason." {
		t.Errorf("%q", got)
	}
}

// On a phone a secondary column is left out of the row's card, and the counters sit two to a line.
func TestPhoneDropsMinorColumns(t *testing.T) {
	css, err := Static.ReadFile("static/flotilla.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"table.data td.minor { display: none !important; }", ".scoreline { display: grid; grid-template-columns: 1fr 1fr; }"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("the phone styles lack %q", want)
		}
	}
	f := setup(t)
	ck := f.signin(f.admin)
	if _, page := f.do("GET", "/autonomy", ck, nil, nil); !strings.Contains(page, `data-label="Spec default" class="minor"`) {
		t.Error("Autonomy's Spec default is not marked minor")
	}
}
