package ui

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// recordedPlans gives backup.rerun n recorded plans from runs of routine, each judged right when judged is set; it returns
// their case ids.
func recordedPlans(t *testing.T, f *fixture, routine string, n int, judged bool, from int) []string {
	t.Helper()
	ctx := context.Background()
	var ids []string
	for i := from; i < from+n; i++ {
		run := fmt.Sprintf("run-%d", i)
		if err := f.st.StartRun(ctx, store.Run{ID: run, Routine: routine, SpecHash: "h", Trigger: "case", Actor: "flotillad", Autonomy: "record",
			StartedAt: "2026-11-23T08:00:00Z", Status: store.StatusRunning}); err != nil {
			t.Fatal(err)
		}
		k := store.Key{Kind: "target", Value: fmt.Sprintf("failed_backup/host:backup-01#%d", i)}
		id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01", Keys: []store.Key{k}, Canonical: k,
			Title: fmt.Sprintf("backup-01's backup is old (%d)", i)}, "ops/backup-monitor", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "ops/backup-monitor", "")
		_ = f.st.MoveCase(ctx, id, cases.Planned, cases.ByAct, "", routine, "")
		if _, err := f.st.ApplyPlan(ctx, store.PlanRecord{CaseID: id, RunID: run, Action: "backup.rerun", Params: map[string]string{"host": "backup-01"},
			PlanHash: "h", Outcome: "record", Autonomy: "record", Would: "propose", Reasons: []string{"ok"}}, cases.Closed, cases.PlanRecorded, "host:backup-01", false); err != nil {
			t.Fatal(err)
		}
		if judged {
			if err := f.st.SetVerdict(ctx, id, "ada", store.VerdictRight); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, id)
	}
	return ids
}

// The operator, 2026-11-23: make an earned level easy to see and raise. An action with five recorded plans judged right is
// said to be ready on Now and in the brief, comes first on Autonomy with its record written in as the reason, and the
// recorded plans nobody judged are asked about on Now, where one answer posts back to Now.
func TestEarnedTrustOnNowAutonomyAndTheBrief(t *testing.T) {
	rerun := &spec.Routine{Fleet: "ops", Routine: "rerun-backup", Description: "d", Enabled: true, Worker: "ops-act-host", Autonomy: "propose",
		Run: spec.Run{Command: "true"}, Limit: time.Minute}
	f := setupWith(t, []*spec.Routine{rerun})
	op := f.addOperator()
	ck, csrf := f.csrfOf(op)
	recordedPlans(t, f, "ops/rerun-backup", 5, true, 0)
	waiting := recordedPlans(t, f, "ops/rerun-backup", 1, false, 10)

	_, now := f.do("GET", "/", ck, nil, nil)
	now = html.UnescapeString(now)
	for _, want := range []string{"backup.rerun has earned propose: 5 recorded plans in a row judged right.", `href="/autonomy#action-backup.rerun"`,
		"Would these plans have been right?", "would have run backup.rerun", `action="/cases/` + waiting[0] + `/right"`, `name="back" value="/"`} {
		if !strings.Contains(now, want) {
			t.Errorf("Now lacks %q", want)
		}
	}

	_, page := f.do("GET", "/autonomy", ck, nil, nil)
	page = html.UnescapeString(page)
	first := strings.Index(page, `<tr id="action-`)
	if first < 0 || !strings.HasPrefix(page[first:], `<tr id="action-backup.rerun" class="row--ready">`) {
		t.Errorf("the ready action is not the first row, marked ready: %.120s", page[max(first, 0):])
	}
	if !strings.Contains(page, `value="5 recorded plans in a row judged right"`) {
		t.Error("the ready action's reason is not written in from its record")
	}

	b, err := f.ui.composeBrief(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var trust []string
	for _, s := range b.Sections {
		if s.Title == "Trust" {
			for _, it := range s.Items {
				trust = append(trust, it.Kind+" "+it.Text)
			}
		}
	}
	if len(trust) != 2 || !strings.HasPrefix(trust[0], "trust:ready backup.rerun has earned propose") ||
		!strings.HasPrefix(trust[1], "trust:judge 1 recorded plan waits for your Right or Wrong") {
		t.Errorf("the brief's Trust section: %q", trust)
	}

	resp, _ := f.do("POST", "/cases/"+waiting[0]+"/right", ck, url.Values{"csrf": {csrf}, "back": {"/"}}, nil)
	if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/?msg=") {
		t.Fatalf("a verdict from Now: %d to %q, want 303 back to / with its message", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, now = f.do("GET", "/", ck, nil, nil); strings.Contains(now, "Would these plans have been right?") {
		t.Error("Now still asks about a plan that was judged")
	}

	// a viewer sees what waits, without the answers
	vck := f.signin(f.vic)
	if _, v := f.do("GET", "/", vck, nil, nil); strings.Contains(v, `/right"`) {
		t.Error("a viewer is offered a verdict")
	}
}

// A ready action whose planning routine is capped below the next level says so wherever it is shown, and the page does not
// ask for that promotion; one whose routine only runs lower says to promote the routine too.
func TestEarnedTrustSaysWhatHoldsIt(t *testing.T) {
	for _, c := range []struct {
		autonomy, max, now, autonomyPage string
	}{
		{"record", "record", "Promoting it changes nothing until that cap is raised: ops/rerun-backup is capped at record by its spec",
			"Promoting it changes nothing yet: ops/rerun-backup is capped at record by its spec"},
		{"record", "", "ops/rerun-backup runs below propose, so promote it as well.", "ops/rerun-backup runs below propose: promote it as well"},
	} {
		rerun := &spec.Routine{Fleet: "ops", Routine: "rerun-backup", Description: "d", Enabled: true, Worker: "ops-act-host", Autonomy: c.autonomy,
			MaxAutonomy: c.max, Run: spec.Run{Command: "true"}, Limit: time.Minute}
		f := setupWith(t, []*spec.Routine{rerun})
		ck := f.signin(f.addOperator())
		recordedPlans(t, f, "ops/rerun-backup", 5, true, 0)

		_, now := f.do("GET", "/", ck, nil, nil)
		if !strings.Contains(html.UnescapeString(now), c.now) {
			t.Errorf("max %q: Now lacks %q", c.max, c.now)
		}
		_, page := f.do("GET", "/autonomy", ck, nil, nil)
		page = html.UnescapeString(page)
		if !strings.Contains(page, c.autonomyPage) {
			t.Errorf("max %q: Autonomy lacks %q", c.max, c.autonomyPage)
		}
		if strings.Contains(page, "row--ready") == (c.max != "") {
			t.Errorf("max %q: the row is marked ready %v", c.max, strings.Contains(page, "row--ready"))
		}
	}
}
