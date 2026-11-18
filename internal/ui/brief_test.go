package ui

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/spec"
	"github.com/chipi/flotilla/internal/store"
)

// A handed-over case of a kind no routine acts on (the seo fleet's) waits for a person: it is in the
// brief's Needs you; one a routine acts on is not.
func TestHandedOverWithoutARoutineNeedsYou(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.sch.Routines = append(f.sch.Routines, &spec.Routine{Fleet: "ops", Routine: "rerun", Trigger: spec.Trigger{Case: &spec.CaseTrigger{Kind: "failed_backup"}}})
	open := func(kind, target, title string) string {
		k := store.Key{Kind: "target", Value: kind + "/" + target}
		id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: kind, Target: target, Keys: []store.Key{k}, Canonical: k, Title: title}, "m", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
		return id
	}
	seo := open("seo_live_regression", "site:robots/www.example.org/robots.txt", "robots.txt keeps Baiduspider out")
	open("failed_backup", "host:backup-01", "The backup is late")
	b, err := f.ui.composeBrief(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Sections) == 0 || b.Sections[0].Title != "Needs you" || len(b.Sections[0].Items) != 1 || b.Sections[0].Items[0].Case != seo {
		t.Fatalf("sections %+v", b.Sections)
	}
}

// A remote worker's link enrols like a host (RFC-0011) but is not a host agent: Watch does not say it is
// down, and the Hosts page calls it a worker link; a host agent that is down is still said so.
func TestWorkerLinkIsNotAHostAgent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, h := range []string{"w-bugfix-omarchy", "model-host"} {
		if _, err := f.st.AddEnrolment(ctx, h, "t-"+h, "user:ada", "test"); err != nil {
			t.Fatal(err)
		}
		if err := f.st.Enrol(ctx, h, "t-"+h, func() (string, time.Time, error) { return "c-" + h, time.Now().Add(720 * time.Hour), nil }); err != nil {
			t.Fatal(err)
		}
	}
	w, err := f.ui.wrongNow(ctx, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var down []string
	for _, x := range w.Items {
		if strings.Contains(x.What, "host agent not connected") {
			down = append(down, x.What)
		}
	}
	if len(down) != 1 || !strings.HasPrefix(down[0], "model-host ") {
		t.Fatalf("down: %v", down)
	}
	ck, _ := f.csrfOf(f.addOperator())
	if _, page := f.do("GET", "/hosts", ck, nil, nil); !strings.Contains(page, "worker link") {
		t.Fatal("the Hosts page does not call the link a worker link")
	}
	// the fleet page's host agents card counts model-host alone (it said "2 down" with a second machine's links, 2026-11-16)
	if _, page := f.do("GET", "/fleets/triage", ck, nil, nil); !strings.Contains(page, "1 down") || strings.Contains(page, "2 down") {
		t.Fatal("the fleet page counts a worker link as a host agent that is down")
	}
}

// A worker a routine names but the installation does not have (the ended candidate, 2026-11-16) is not
// "healthy" on the fleet page: the fixture configures no workers at all.
func TestUnconfiguredWorkerIsNotHealthy(t *testing.T) {
	f := setup(t)
	ck, _ := f.csrfOf(f.addOperator())
	_, page := f.do("GET", "/fleets/triage", ck, nil, nil)
	i := strings.Index(page, "worker triage")
	if i < 0 {
		t.Fatal("no worker block on the fleet page")
	}
	block := page[i:min(len(page), i+600)]
	if !strings.Contains(block, "not configured") || strings.Contains(block, ">healthy<") {
		t.Fatalf("worker block: %s", block)
	}
}

// RFC-0012 §4: a monitor's summary lines go into the brief's section its spec names, in the brief
// written after its run only.
func TestBriefSummarySection(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.sch.Routines = append(f.sch.Routines, &spec.Routine{Fleet: "seo", Routine: "traffic", BriefSection: "Search", Detects: []string{"seo_traffic_drop"}})
	b, _ := json.Marshal(store.Summary{Run: "seo-traffic-1", Section: "Search", Lines: []string{"www.example.org: 10 visits (+5%)"}})
	if err := f.st.Event(ctx, "seo/traffic", "flotillad", store.SummaryKind, "seo/traffic", string(b)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour)
	br, err := f.ui.composeBrief(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	last := br.Sections[len(br.Sections)-1]
	if last.Title != "Search" || len(last.Items) != 1 || last.Items[0].Text != "www.example.org: 10 visits (+5%)" || last.Items[0].Href != "/runs/seo-traffic-1" {
		t.Fatalf("sections %+v", br.Sections)
	}
	if br, _ := f.ui.composeBrief(ctx, now.Add(48*time.Hour)); len(br.Sections) > 0 && br.Sections[len(br.Sections)-1].Title == "Search" {
		t.Fatal("a summary older than a day is in the brief again")
	}
}

// G11: the brief is written once a day after brief_at from flotillad's record, kept as written, shown
// with its earlier briefs, and its items take a person's "more" or "less".
func TestMorningBrief(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	clock := time.Now().UTC().Truncate(24 * time.Hour).Add(6*time.Hour + 30*time.Minute)
	f.sch.Now = func() time.Time { return clock }
	// one case waiting for a person (escalated), one closed in the last day
	open := func(key, title string) string {
		k := store.Key{Kind: "target", Value: "failed_backup/host:backup-01" + key}
		id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:backup-01" + key, Keys: []store.Key{k}, Canonical: k, Title: title}, "m", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.st.MoveCase(ctx, id, cases.HandedOver, cases.ByDetector, "", "m", "")
		return id
	}
	waitID := open("#w", "The backup is 30 hours old")
	if err := f.st.MoveCase(ctx, waitID, cases.Escalated, cases.ByAct, "", "a", "no plan"); err != nil {
		t.Fatal(err)
	}
	doneID := open("#d", "The backup was late")
	if err := f.st.MoveCase(ctx, doneID, cases.Closed, cases.ByAct, cases.ResolvedItself, "a", ""); err != nil {
		t.Fatal(err)
	}

	ck, csrf := f.csrfOf(f.addOperator())
	if _, page := f.do("GET", "/brief", ck, nil, nil); !strings.Contains(page, "The first brief is written at 07:00") {
		t.Error("before any brief, the page says when the first is written")
	}
	if err := f.ui.WriteBriefIfDue(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := f.st.BriefOn(ctx, clock.Format("2006-01-02")); b != nil {
		t.Fatal("a brief was written before brief_at")
	}
	clock = clock.Add(time.Hour) // 07:30
	if err := f.ui.WriteBriefIfDue(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := f.st.BriefOn(ctx, clock.Format("2006-01-02"))
	if b == nil {
		t.Fatal("no brief after brief_at")
	}
	if b.Headline != "1 thing needs you; 1 thing to watch." {
		t.Errorf("headline %q", b.Headline)
	}
	if !strings.Contains(b.Summary, "1 case was closed without you") {
		t.Errorf("summary %q", b.Summary)
	}
	// written once: a later case does not change today's brief
	open("#late", "Another one")
	_ = f.ui.WriteBriefIfDue(ctx)
	if again, _ := f.st.BriefOn(ctx, clock.Format("2006-01-02")); again.WrittenAt != b.WrittenAt {
		t.Error("the brief was written twice in a day")
	}

	_, page := f.do("GET", "/brief", ck, nil, nil)
	for _, want := range []string{"1 thing needs you; 1 thing to watch.", "triage/cycle is stale", "The backup is 30 hours old", `href="/cases/` + waitID + `"`, "Handled overnight",
		"The backup was late (resolved itself).", "More like this", "This is the first brief."} {
		if !strings.Contains(page, want) {
			t.Errorf("the brief page has no %q", want)
		}
	}
	resp, _ := f.do("POST", "/brief/"+b.Day+"/mark", ck, url.Values{"csrf": {csrf}, "item": {"0"}, "mark": {"less"}}, nil)
	if !strings.Contains(resp.Header.Get("Location"), "msg=") {
		t.Fatalf("a mark: %s", resp.Header.Get("Location"))
	}
	if _, page = f.do("GET", "/brief", ck, nil, nil); !strings.Contains(page, `value="less" aria-pressed="true"`) || !strings.Contains(page, "needs:failed_backup: 0 more, 1 less") {
		t.Error("the page does not show the mark, or this week's marks by kind")
	}
	if _, arch := f.do("GET", "/brief/archive", ck, nil, nil); !strings.Contains(arch, "/brief?day="+b.Day) {
		t.Error("the archive does not list the brief")
	}
}
