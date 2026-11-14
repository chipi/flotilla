package ui

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chipi/flotilla/internal/cases"
	"github.com/chipi/flotilla/internal/store"
)

// G11: the brief is written once a day after brief_at from flotillad's record, kept as written, shown
// with its earlier briefs, and its items take a person's "more" or "less".
func TestMorningBrief(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	clock := time.Now().UTC().Truncate(24 * time.Hour).Add(6*time.Hour + 30*time.Minute)
	f.sch.Now = func() time.Time { return clock }
	// one case waiting for a person (escalated), one closed in the last day
	open := func(key, title string) string {
		k := store.Key{Kind: "target", Value: "failed_backup/host:mini" + key}
		id, _, err := f.st.OpenCase(ctx, store.NewCase{Kind: "failed_backup", Target: "host:mini" + key, Keys: []store.Key{k}, Canonical: k, Title: title}, "m", "")
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
